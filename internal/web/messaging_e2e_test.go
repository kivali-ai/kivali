package web

// End-to-end functional tests for the messaging contract surfaced
// through the HTTP layer.
//
// Unit tests on each side of a wire can pass with fixtures that match
// only their own side. These walk the full send → receive → release /
// ack flow through the real HTTP handlers, so the JSON the server
// emits is what its readers consume.
//
// Conventions:
//   - Each Test_E2E_* exercises a full user-visible flow end to
//     end through Server.Handler() with no internal short-circuits.
//   - Setup uses the real Messenger + a no-op Claude client so
//     wakes don't actually fire model calls.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

/* ---------- helpers --------------------------------------------- */

// e2eServer is the standard fixture for these tests: a fresh server
// with Runtime + Messenger wired up, NotifyOrgState plumbed back to
// the runtime so liveness propagates the same way main.go wires it,
// and CEO + chief-of-staff agents seeded so the bootstrap shape
// matches a real install. Returns the server and a synchronous
// wait-for-snapshot helper for tests that need to observe a publish.
func e2eServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newTurnServer(t)
	srv.Claude = nil // no real model traffic; chat-loop spawns short-circuit
	if err := srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, ""); err != nil {
		// CEO may already exist from another path — only fail if the
		// error isn't "already exists".
		if !strings.Contains(err.Error(), "exists") {
			t.Fatalf("seed CEO: %v", err)
		}
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatalf("seed CoS: %v", err)
	}
	return srv
}

func multipartBody(t *testing.T, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func postMultipart(t *testing.T, srv *Server, path string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, fields)
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("content-type", ct)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

func readChat(t *testing.T, srv *Server, slug string) []store.ChatMessage {
	t.Helper()
	hist, err := srv.Store.ReadChatHistory(slug)
	if err != nil {
		t.Fatalf("read %s chat: %v", slug, err)
	}
	return hist
}

// publishedMessage is the full publish-side flow: WriteMessage on
// disk, then route through Messenger so the routing layer's split
// (CEO-bound vs agent-bound vs role) fires exactly the way an
// agent-published message would in production. Returns the relpath
// of the message file.
func publishedMessage(t *testing.T, srv *Server, msg store.Message) string {
	t.Helper()
	abs, err := srv.Store.WriteMessage(msg)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	msg.Path = abs
	if _, err := srv.Messenger.Route(context.Background(), msg); err != nil {
		t.Fatalf("Route: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	return rel
}

/* ---------- agent → CEO inbox flows ---------------------------- */

// TestE2E_AgentPublishesNotificationToCEO covers the most common CEO-bound
// flow: an agent emits a ceo_notification and it lands in the CEO's
// chat.jsonl as a Kind="ceo_inbox" entry. The pendingCEOInbox count
// should reflect the new item AND the org snapshot's
// inbox.unactioned should match.
func TestE2E_AgentPublishesNotificationToCEO(t *testing.T) {
	srv := e2eServer(t)
	if got := srv.pendingCEOInbox(); got != 0 {
		t.Fatalf("baseline pendingCEOInbox = %d, want 0", got)
	}

	rel := publishedMessage(t, srv, store.Message{
		Type:  store.MsgCEONotification,
		Title: "Q3 analysis ready",
		From:  "chief-of-staff",
		To:    store.Recipients{agent.CEOSlug},
		Date:  time.Now().UTC(),
		Body:  "report attached",
	})

	// The CEO's chat now has a ceo_inbox entry pointing at the
	// message file.
	hist := readChat(t, srv, agent.CEOSlug)
	var found *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "ceo_inbox" && hist[i].MessageRef == rel {
			found = &hist[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no ceo_inbox entry for %s in CEO chat: %+v", rel, hist)
	}
	if got := srv.pendingCEOInbox(); got != 1 {
		t.Errorf("pendingCEOInbox after publish = %d, want 1", got)
	}

	// Snapshot reflects the same count under the inbox.unactioned key
	// the JS reads.
	var snap orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatalf("snapshot decode: %v", err)
	}
	if snap.Inbox.Unactioned != 1 {
		t.Errorf("snapshot inbox.unactioned = %d, want 1", snap.Inbox.Unactioned)
	}
}

// TestE2E_CEOAcknowledgesNotificationDecrementsCount: acknowledging a
// notification drops the CEO inbox count. The full flow must:
//
//  1. Write the ceo_notification_ack response file with InReplyTo
//     pointing at the original notification.
//  2. Append a ceo_reply chat entry on the CEO's timeline so
//     buildCEOInbox can pair the request with the response.
//  3. Drop pendingCEOInbox by exactly one.
//  4. Push a fresh /org/stream snapshot so the sidebar's count
//     updates without requiring an unrelated event to wake the
//     publisher.
func TestE2E_CEOAcknowledgesNotificationDecrementsCount(t *testing.T) {
	srv := e2eServer(t)
	keep := publishedMessage(t, srv, store.Message{
		Type: store.MsgCEONotification, Title: "Notif A", From: "chief-of-staff",
		To: store.Recipients{agent.CEOSlug}, Date: time.Now().UTC(), Body: "stay",
	})
	ack := publishedMessage(t, srv, store.Message{
		Type: store.MsgCEONotification, Title: "Notif B", From: "chief-of-staff",
		To: store.Recipients{agent.CEOSlug}, Date: time.Now().UTC().Add(time.Millisecond), Body: "ack me",
	})
	if got := srv.pendingCEOInbox(); got != 2 {
		t.Fatalf("after publish: pendingCEOInbox = %d, want 2", got)
	}

	// Subscribe BEFORE the ack so we observe the post-ack snapshot
	// publish.
	_, snaps := srv.orgHub.subscribe()
	defer srv.orgHub.unsubscribe(snaps)

	rr := postMultipart(t, srv, "/api/v1/needs/ack", map[string]string{
		"path": ack, "message": "thanks",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("ack: code = %d body = %s", rr.Code, rr.Body.String())
	}

	if got := srv.pendingCEOInbox(); got != 1 {
		t.Fatalf("after ack: pendingCEOInbox = %d, want 1 (kept = %s)", got, keep)
	}

	// The ceo_reply chat entry exists and points at a notification_ack
	// whose in_reply_to is the original path.
	hist := readChat(t, srv, agent.CEOSlug)
	var ackEntry *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "ceo_reply" && hist[i].ReplyToMessageRef == ack {
			ackEntry = &hist[i]
			break
		}
	}
	if ackEntry == nil {
		t.Fatalf("no ceo_reply pointing at %s; hist = %+v", ack, hist)
	}
	resp, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), ackEntry.MessageRef))
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Type != store.MsgCEONotificationAck {
		t.Errorf("response type = %q, want ceo_notification_ack", resp.Type)
	}
	if resp.InReplyTo != ack {
		t.Errorf("response in_reply_to = %q, want %q", resp.InReplyTo, ack)
	}

	// Post-ack snapshot publish — the fix in handleCEOResponse calls
	// NotifyOrgState before the redirect, and the published snapshot
	// must reflect the decremented count.
	//
	// publishedMessage calls earlier in the test also fire NotifyOrgState
	// via the Store post-write hook, and those debounced snapshots may
	// arrive on the subscriber channel after subscribe() but before the
	// post-ack publish lands. Loop until we see Unactioned==1 — the
	// only state reachable via a post-ack rebuild — or timeout.
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case body := <-snaps:
			var snap orgSnapshot
			if err := json.Unmarshal(body, &snap); err != nil {
				t.Fatalf("snapshot decode: %v", err)
			}
			if snap.Inbox.Unactioned == 1 {
				return
			}
			// Pre-ack snapshot (Unactioned==2) — keep waiting for the
			// post-ack rebuild to land.
		case <-time.After(time.Until(deadline)):
			t.Fatal("ack did not trigger an org-state snapshot publish with Unactioned=1")
		}
	}
}

// TestE2E_CEOAckSurfaceChatAppendError: if the CEO chat append fails (e.g. CEO agent dir was deleted out from under
// us, or a future filesystem error), the handler MUST surface the
// error rather than swallow it — otherwise the response file lives
// on disk but pendingCEOInbox can't pair it, and the count stays
// wrong forever.
func TestE2E_CEOAckSurfaceChatAppendError(t *testing.T) {
	srv := e2eServer(t)
	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgCEONotification, Title: "X", From: "chief-of-staff",
		To: store.Recipients{agent.CEOSlug}, Date: time.Now().UTC(), Body: "x",
	})

	// Wipe the CEO's agent dir so AppendChatMessage returns ErrNotFound
	// when the ack handler tries to record the ceo_reply entry.
	if err := os.RemoveAll(filepath.Join(srv.Store.Root(), "agents", agent.CEOSlug)); err != nil {
		t.Fatalf("remove CEO dir: %v", err)
	}

	rr := postMultipart(t, srv, "/api/v1/needs/ack", map[string]string{
		"path": rel, "message": "x",
	})
	if rr.Code < 500 {
		t.Errorf("ack with broken CEO chat returned code = %d (want 5xx so the operator notices); body = %s",
			rr.Code, rr.Body.String())
	}
}

// TestE2E_CEOApprovesApprovalRequestDeliversToSender covers the
// approval-flow happy path: agent publishes a ceo_approval_request,
// CEO approves with a comment, and the approval response lands in
// the original sender's chat as an inbox_delivery.
func TestE2E_CEOApprovesApprovalRequestDeliversToSender(t *testing.T) {
	srv := e2eServer(t)
	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgCEOApprovalRequest, Title: "Run X?", From: "chief-of-staff",
		To: store.Recipients{agent.CEOSlug}, Date: time.Now().UTC(), Body: "rationale",
	})

	rr := postMultipart(t, srv, "/api/v1/needs/approve", map[string]string{
		"path": rel, "message": "go for it",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("approve: code = %d body = %s", rr.Code, rr.Body.String())
	}

	// Sender's chat picked up an inbox_delivery for the response.
	cosHist := readChat(t, srv, "chief-of-staff")
	var deliv *store.ChatMessage
	for i := range cosHist {
		if cosHist[i].Kind == "inbox_delivery" {
			ref := cosHist[i].MessageRef
			if ref == "" {
				continue
			}
			m, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), ref))
			if err == nil && m.Type == store.MsgCEOApprovalResponse && m.InReplyTo == rel {
				deliv = &cosHist[i]
				break
			}
		}
	}
	if deliv == nil {
		t.Fatalf("no approval-response inbox_delivery in CoS chat: %+v", cosHist)
	}
	if !strings.Contains(deliv.Content, "go for it") {
		t.Errorf("delivery content missing CEO message: %q", deliv.Content)
	}
}

// TestE2E_CEODenyPersistsApprovedFalse asserts the deny twin: same
// flow but the response carries Approved=false.
func TestE2E_CEODenyPersistsApprovedFalse(t *testing.T) {
	srv := e2eServer(t)
	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgCEOApprovalRequest, Title: "Run X?", From: "chief-of-staff",
		To: store.Recipients{agent.CEOSlug}, Date: time.Now().UTC(), Body: "rationale",
	})

	rr := postMultipart(t, srv, "/api/v1/needs/deny", map[string]string{
		"path": rel, "message": "not now",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("deny: code = %d body = %s", rr.Code, rr.Body.String())
	}

	list, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(list) != 1 {
		t.Fatalf("approval responses = %d, want 1", len(list))
	}
	resp, err := srv.Store.ReadMessage(list[0].Path)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Approved == nil || *resp.Approved {
		t.Errorf("response.Approved = %v, want pointer-to-false", resp.Approved)
	}
}

/* ---------- agent → agent inbox flows -------------------------- */

// TestE2E_AgentToAgentMessageQueuesUntilRelease is THE end-to-end
// release-flow test: chief-of-staff publishes a task to alice,
// alice's chat stays empty until the CEO releases, then the message
// lands as inbox_delivery and alice is woken.
func TestE2E_AgentToAgentMessageQueuesUntilRelease(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgNotice, Title: "Draft Q3", From: "chief-of-staff", To: store.Recipients{"alice"},
		Date: time.Now().UTC(), Body: "do the thing",
	})

	// Pre-release: queued, not delivered.
	ts, _ := srv.Store.ReadMessageQueue()
	if got := ts.Agents["alice"].Inbox; len(got) != 1 || got[0] != rel {
		t.Fatalf("alice.Inbox = %v, want [%s]", got, rel)
	}
	if hist := readChat(t, srv, "alice"); len(hist) != 0 {
		t.Errorf("alice chat should be empty pre-release; got %+v", hist)
	}

	// Per-message Release.
	rr := queuePost(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{
		Path: rel, Note: "include APAC",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("release: code = %d body = %s", rr.Code, rr.Body.String())
	}

	// Post-release: queue empty for alice, chat has inbox_delivery
	// with the CEO comment annotated.
	ts, _ = srv.Store.ReadMessageQueue()
	for _, p := range ts.Agents["alice"].Inbox {
		if p == rel {
			t.Errorf("released message still queued: %s", p)
		}
	}
	hist := readChat(t, srv, "alice")
	var deliv *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "inbox_delivery" && hist[i].MessageRef == rel {
			deliv = &hist[i]
			break
		}
	}
	if deliv == nil {
		t.Fatalf("no inbox_delivery for %s in alice's chat", rel)
	}
	if !strings.Contains(deliv.Content, "do the thing") {
		t.Errorf("delivery missing original body: %q", deliv.Content)
	}
	if !strings.Contains(deliv.Content, "> CEO: include APAC") {
		t.Errorf("delivery missing CEO comment: %q", deliv.Content)
	}
}

// TestE2E_ReleaseAllDrainsEveryQueue covers the "Release all" button:
// 3 messages queued across 2 agents, one POST drains everyone.
// Locks in the fire-and-forget contract: 202 status, no per-agent
// stream, queue empty post-call, every recipient's chat got their
// deliveries.
func TestE2E_ReleaseAllDrainsEveryQueue(t *testing.T) {
	srv := e2eServer(t)
	for _, slug := range []string{"alice", "bob"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: "x", ReportsTo: "chief-of-staff"}, "k"); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}

	wantPaths := map[string][]string{}
	wantPaths["alice"] = []string{
		publishedMessage(t, srv, store.Message{
			Type: store.MsgNotice, Title: "A1", From: "chief-of-staff", To: store.Recipients{"alice"},
			Date: time.Now().UTC(), Body: "a1",
		}),
		publishedMessage(t, srv, store.Message{
			Type: store.MsgNotice, Title: "A2", From: "chief-of-staff", To: store.Recipients{"alice"},
			Date: time.Now().UTC().Add(time.Millisecond), Body: "a2",
		}),
	}
	wantPaths["bob"] = []string{
		publishedMessage(t, srv, store.Message{
			Type: store.MsgNotice, Title: "B1", From: "chief-of-staff", To: store.Recipients{"bob"},
			Date: time.Now().UTC().Add(2 * time.Millisecond), Body: "b1",
		}),
	}

	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("release-all: code = %d body = %s (want 200)", rr.Code, rr.Body.String())
	}

	ts, _ := srv.Store.ReadMessageQueue()
	for slug, want := range wantPaths {
		if len(ts.Agents[slug].Inbox) != 0 {
			t.Errorf("%s queue post-release-all = %v, want empty", slug, ts.Agents[slug].Inbox)
		}
		hist := readChat(t, srv, slug)
		got := map[string]bool{}
		for _, e := range hist {
			if e.Kind == "inbox_delivery" {
				got[e.MessageRef] = true
			}
		}
		for _, p := range want {
			if !got[p] {
				t.Errorf("%s missing inbox_delivery for %s; got %+v", slug, p, hist)
			}
		}
	}
}

// TestE2E_BounceDeliversStatusUpdateToSender: bouncing a queued task
// drops the queue pointer for the intended recipient AND delivers a
// CEO-authored notice straight into the original sender's
// chat. The sender doesn't have to wait for a release — bounces
// fire instantly so the agent can re-plan.
func TestE2E_BounceDeliversStatusUpdateToSender(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgNotice, Title: "T", From: "chief-of-staff", To: store.Recipients{"alice"},
		Date: time.Now().UTC(), Body: "do it",
	})

	rr := queuePost(t, srv, "/api/v1/queue/bounce", apitypes.QueueBounceRequest{
		Path: rel, Comment: "rescope first",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("bounce: code = %d body = %s", rr.Code, rr.Body.String())
	}

	ts, _ := srv.Store.ReadMessageQueue()
	for _, p := range ts.Agents["alice"].Inbox {
		if p == rel {
			t.Errorf("bounced message still in alice's queue: %s", p)
		}
	}

	hist := readChat(t, srv, "chief-of-staff")
	var found bool
	for _, e := range hist {
		if e.Kind != "inbox_delivery" || e.MessageRef == "" {
			continue
		}
		m, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), e.MessageRef))
		if err != nil {
			continue
		}
		// A bounce is a tell, not an answer: a parentless notice —
		// see ceoUndeliveredIsATell.
		if m.Type == store.MsgNotice && m.From == agent.CEOSlug {
			found = true
			if m.InReplyTo != "" {
				t.Errorf("bounce notice must be parentless; in_reply_to = %q", m.InReplyTo)
			}
			if !strings.Contains(m.Body, "rescope first") {
				t.Errorf("bounce body missing CEO comment: %q", m.Body)
			}
			break
		}
	}
	if !found {
		t.Fatalf("no CEO-authored bounce notice in CoS chat: %+v", hist)
	}
	// The bounce cancels the message: it must not be left in the live
	// ledger where a messages/ scan can still read it as in flight.
	if _, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), rel)); err == nil {
		t.Errorf("bounced message still readable at its live-ledger path %s", rel)
	}
}

/* ---------- wire-format contract -------------------------------- */

// TestE2E_OrgSnapshotKeysMatchJSContract is the cross-language check:
// the snapshot the server emits carries the exact JSON keys its
// readers reach into, so renaming a snapshot field fails this test
// before it can ship a broken inbox page.
func TestE2E_OrgSnapshotKeysMatchJSContract(t *testing.T) {
	srv := e2eServer(t)
	// One pending message so Inbox + Release fields aren't all zero.
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	publishedMessage(t, srv, store.Message{
		Type: store.MsgNotice, Title: "T", From: "chief-of-staff", To: store.Recipients{"alice"},
		Date: time.Now().UTC(), Body: "x",
	})

	body := srv.buildOrgSnapshot()
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("decode snapshot: %v body=%s", err, body)
	}

	// Top-level keys the JS reads.
	for _, k := range []string{"agents", "inbox", "release", "messages_total"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("snapshot missing top-level key %q (JS depends on it); body = %s", k, body)
		}
	}

	// release.{running,active_agents,pending_docs} — every JS file
	// reads at least one of these.
	rel, ok := generic["release"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot.release not an object: %v", generic["release"])
	}
	for _, k := range []string{"running", "active_agents", "pending_docs"} {
		if _, ok := rel[k]; !ok {
			t.Errorf("snapshot.release missing key %q (JS depends on it)", k)
		}
	}

	// inbox.{unactioned,pending_paths}.
	inbox, ok := generic["inbox"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot.inbox not an object: %v", generic["inbox"])
	}
	for _, k := range []string{"unactioned", "pending_paths"} {
		if _, ok := inbox[k]; !ok {
			t.Errorf("snapshot.inbox missing key %q (JS depends on it)", k)
		}
	}

	// agents[].slug always present.
	agents, ok := generic["agents"].([]any)
	if !ok {
		t.Fatalf("snapshot.agents not an array: %v", generic["agents"])
	}
	for i, a := range agents {
		ao, ok := a.(map[string]any)
		if !ok {
			t.Errorf("agents[%d] not object: %v", i, a)
			continue
		}
		if _, ok := ao["slug"]; !ok {
			t.Errorf("agents[%d] missing slug", i)
		}
	}
}

/* ---------- failure-path contracts -------- */

// TestE2E_CEOAckFailsCleanlyWhenRecipientUnreachable covers the
// archived-sender precheck + the no-fallback contract together:
//
//  1. The sender's existence is checked BEFORE we write any response
//     file or chat entry. This avoids the orphan-state class where
//     a response file lives on disk + the CEO's ceo_reply chat entry
//     resolves the inbox count, but the response was never delivered
//     to anyone.
//  2. There is NO fallback queue write — one would be both a
//     layering violation and a concurrency hazard (lost-write race
//     against in-flight Route calls).
//
// Result: a clean 4xx from the precheck. No response file, no
// ceo_reply chat entry, no queue mutation.
func TestE2E_CEOAckFailsCleanlyWhenRecipientUnreachable(t *testing.T) {
	srv := e2eServer(t)
	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgCEONotification, Title: "X", From: "chief-of-staff",
		To: store.Recipients{agent.CEOSlug}, Date: time.Now().UTC(), Body: "x",
	})

	// Snapshot the response-file count BEFORE the ack so we can assert
	// nothing got written by a half-completed handler.
	beforeAckResponses, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEONotificationAck})
	beforeCEOChat, _ := srv.Store.ReadChatHistory(agent.CEOSlug)
	beforeCEOReplyCount := 0
	for _, e := range beforeCEOChat {
		if e.Kind == "ceo_reply" {
			beforeCEOReplyCount++
		}
	}

	// Archive the sender between request and ack — simulates a
	// concurrent agent-removal in another tab.
	if err := os.RemoveAll(filepath.Join(srv.Store.Root(), "agents", "chief-of-staff")); err != nil {
		t.Fatalf("archive cos: %v", err)
	}

	rr := postMultipart(t, srv, "/api/v1/needs/ack", map[string]string{"path": rel, "message": "x"})
	if rr.Code != http.StatusConflict {
		t.Errorf("ack with archived sender = %d, want 409 Conflict from precheck; body = %s", rr.Code, rr.Body.String())
	}

	// Precheck must run BEFORE WriteMessage and AppendChatMessage:
	// no orphan response file, no ghost ceo_reply chat entry, no
	// phantom queue entry.
	afterAckResponses, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEONotificationAck})
	if len(afterAckResponses) != len(beforeAckResponses) {
		t.Errorf("ack precheck failure created an orphan response file: before=%d after=%d", len(beforeAckResponses), len(afterAckResponses))
	}
	afterCEOChat, _ := srv.Store.ReadChatHistory(agent.CEOSlug)
	afterCEOReplyCount := 0
	for _, e := range afterCEOChat {
		if e.Kind == "ceo_reply" {
			afterCEOReplyCount++
		}
	}
	if afterCEOReplyCount != beforeCEOReplyCount {
		t.Errorf("ack precheck failure appended a ghost ceo_reply: before=%d after=%d (would falsely resolve the inbox count)", beforeCEOReplyCount, afterCEOReplyCount)
	}
	ts, _ := srv.Store.ReadMessageQueue()
	if rt := ts.Agents["chief-of-staff"]; len(rt.Inbox) > 0 {
		t.Errorf("ack failure still queued response under archived agent: %v", rt.Inbox)
	}
}

// TestE2E_ReleaseAllBuffersForAlreadyRunningAgent: ReleaseAll's drain
// goes through DeliverToAgent, so a release to an agent with an active
// chat hub lands in pendingDeliveries rather than racing the chat
// loop's output (tool_use, tool_result, assistant_text) in
// chat.jsonl.
func TestE2E_ReleaseAllBuffersForAlreadyRunningAgent(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgNotice, Title: "T", From: "chief-of-staff", To: store.Recipients{"alice"},
		Date: time.Now().UTC(), Body: "x",
	})

	// Pretend alice is already mid-loop: chatHubs[alice] is non-nil
	// the way it would be during a direct CEO chat. No real Claude
	// loop runs (Claude is nil) but deliverToAgent's busy check is
	// based on the map.
	srv.streamMu.Lock()
	srv.chatHubs = map[string]*chatHub{
		"alice": {hub: newHub(), slug: "alice", spawnSource: "chat"},
	}
	srv.streamMu.Unlock()

	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("release-all: code = %d body = %s", rr.Code, rr.Body.String())
	}

	// Alice's chat.jsonl must NOT yet have the inbox_delivery — it's
	// buffered until her current loop ends.
	hist := readChat(t, srv, "alice")
	for _, e := range hist {
		if e.Kind == "inbox_delivery" && e.MessageRef == rel {
			t.Errorf("released message landed directly on chat.jsonl while hub was active — bypass regressed: %+v", e)
		}
	}

	// And it MUST be sitting in the buffer waiting to flush.
	srv.streamMu.Lock()
	pending := srv.pendingDeliveries["alice"]
	srv.streamMu.Unlock()
	if len(pending) != 1 {
		t.Fatalf("pendingDeliveries[alice] = %d, want 1 (buffered while hub active)", len(pending))
	}
	if pending[0].msg.MessageRef != rel {
		t.Errorf("buffered MessageRef = %q, want %q", pending[0].msg.MessageRef, rel)
	}

	// The message-queue side of release-all still ran: alice's queue
	// is empty (drain succeeded, just held in the buffer instead of
	// chat.jsonl).
	ts, _ := srv.Store.ReadMessageQueue()
	if len(ts.Agents["alice"].Inbox) != 0 {
		t.Errorf("alice queue post-release-all = %v, want empty", ts.Agents["alice"].Inbox)
	}
}

// TestE2E_FlushAndCompletePreservesArrivalOrder asserts the basic
// contract of the phase-1 teardown helper: buffered entries land on
// chat.jsonl in the same order they were buffered, the buffer clears,
// and the hub is marked completed (but stays in chatHubs until the
// linger window expires so getOrCreateHub can identity-evict it on
// a fresh spawn).
func TestE2E_FlushAndCompletePreservesArrivalOrder(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	hub := &chatHub{hub: newHub(), slug: "alice", spawnSource: "chat"}
	srv.streamMu.Lock()
	srv.chatHubs = map[string]*chatHub{"alice": hub}
	srv.pendingDeliveries = map[string][]bufferedDelivery{
		"alice": {
			{id: "d1", msg: store.ChatMessage{Role: store.RoleReceived, Content: "first", Kind: "inbox_delivery", MessageRef: "messages/2026-04-18/a.md"}},
			{id: "d2", msg: store.ChatMessage{Role: store.RoleReceived, Content: "second", Kind: "inbox_delivery", MessageRef: "messages/2026-04-18/b.md"}},
			{id: "d3", msg: store.ChatMessage{Role: store.RoleReceived, Content: "third", Kind: "inbox_delivery", MessageRef: "messages/2026-04-18/c.md"}},
		},
	}
	srv.streamMu.Unlock()

	flushed := srv.flushAndComplete("alice", hub)
	if flushed != 3 {
		t.Fatalf("flushed = %d, want 3", flushed)
	}

	// Buffer cleared, hub marked completed but still in chatHubs
	// until the linger eviction timer fires (so a fresh spawn can
	// identity-evict it via getOrCreateHub).
	srv.streamMu.Lock()
	hubStillThere := srv.chatHubs["alice"] == hub
	bufGone := len(srv.pendingDeliveries["alice"]) == 0
	srv.streamMu.Unlock()
	if !hubStillThere {
		t.Error("chatHubs[alice] should still hold the lingering hub")
	}
	if !hub.isCompleted() {
		t.Error("hub.completed not set after flushAndComplete")
	}
	if bufGone == false {
		t.Error("pendingDeliveries[alice] not cleared")
	}

	// Disk order matches insertion order.
	hist := readChat(t, srv, "alice")
	var contents []string
	for _, e := range hist {
		if e.Kind == "inbox_delivery" {
			contents = append(contents, e.Content)
		}
	}
	want := []string{"first", "second", "third"}
	if len(contents) != 3 || contents[0] != want[0] || contents[1] != want[1] || contents[2] != want[2] {
		t.Errorf("chat.jsonl order = %v, want %v", contents, want)
	}
}

// TestE2E_FlushAndCompleteBeatsConcurrentDelivery is the
// concurrency-stress version: a delivery racing flushAndComplete
// must always end up AFTER the buffered batch on disk, never
// interleaved or before. Runs the race many times to expose any
// remaining window.
//
// Without the streamMu-spanning hold, this test scrambled order
// roughly 1 in N iterations — the failing schedule is when the
// racing delivery acquires chatLock between two of the flushed
// AppendChatMessage calls. With the hold, the race is closed
// because deliverToAgent can't grab streamMu until the flush
// completes AND the completed flag is flipped, so its chatLock
// acquisition is strictly after.
func TestE2E_FlushAndCompleteBeatsConcurrentDelivery(t *testing.T) {
	const iterations = 50
	for i := 0; i < iterations; i++ {
		srv := e2eServer(t)
		if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		hub := &chatHub{hub: newHub(), slug: "alice", spawnSource: "chat"}
		srv.streamMu.Lock()
		srv.chatHubs = map[string]*chatHub{"alice": hub}
		srv.pendingDeliveries = map[string][]bufferedDelivery{
			"alice": {
				{id: "d1", msg: store.ChatMessage{Role: store.RoleReceived, Content: "buf-1", Kind: "inbox_delivery", MessageRef: "messages/2026-04-18/a.md"}},
				{id: "d2", msg: store.ChatMessage{Role: store.RoleReceived, Content: "buf-2", Kind: "inbox_delivery", MessageRef: "messages/2026-04-18/b.md"}},
			},
		}
		srv.streamMu.Unlock()

		done := make(chan struct{})
		racingMsg := store.ChatMessage{Role: store.RoleReceived, Content: "race", Kind: "inbox_delivery", MessageRef: "messages/2026-04-18/r.md"}
		go func() {
			// Best-effort race: try to land mid-flush. The streamMu
			// hold means we'll always end up after.
			_ = srv.deliverToAgent("alice", racingMsg)
			close(done)
		}()
		srv.flushAndComplete("alice", hub)
		<-done

		hist := readChat(t, srv, "alice")
		var seq []string
		for _, e := range hist {
			if e.Kind == "inbox_delivery" {
				seq = append(seq, e.Content)
			}
		}
		// Either the racing delivery slipped in BEFORE we acquired the
		// stream lock (so it was buffered and flushed with the batch,
		// landing as one of the entries) — fine; or it hit AFTER the
		// flush released and went direct — also fine, but must be
		// last. The forbidden case is "race" appearing between
		// "buf-1" and "buf-2".
		raceIdx := -1
		buf1Idx, buf2Idx := -1, -1
		for j, c := range seq {
			switch c {
			case "race":
				raceIdx = j
			case "buf-1":
				buf1Idx = j
			case "buf-2":
				buf2Idx = j
			}
		}
		if buf1Idx < 0 || buf2Idx < 0 {
			t.Fatalf("iter %d: buffered entries missing from disk: %v", i, seq)
		}
		if buf1Idx > buf2Idx {
			t.Fatalf("iter %d: buffer flushed out of order: %v", i, seq)
		}
		if raceIdx >= 0 && raceIdx > buf1Idx && raceIdx < buf2Idx {
			t.Fatalf("iter %d: racing delivery interleaved between buffered entries: %v (race condition)", i, seq)
		}
	}
}

// TestE2E_WakeSuppressedWhenAgentAlreadyReplied: a broadcast wake
// (release-all and the like) spawns only agents that owe a response
// (store.HasUnansweredReceived). An agent that already answered its
// last received and has no new mail would otherwise run on stale
// context, often inventing follow-up work. This test asserts:
//
//  1. An agent whose last real entry is RoleSent does NOT spawn.
//  2. The chat history is unchanged (no spurious assistant_text).
//  3. Releasing actual mail to that same agent unblocks the spawn.
func TestE2E_WakeSuppressedWhenAgentAlreadyReplied(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv.Claude = &provider.MockClient{}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0", MaxTokens: 1024})
	// fake records every chat-turn the spawn gate publishes. With
	// the gate working, no chat-turn is published for the
	// already-answered case.
	var streamCalls atomic.Int32
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		streamCalls.Add(1)
		return []agentpod.TurnEvent{doneEvent()}
	})

	// Seed alice's chat as "received then replied" — she owes nothing.
	now := time.Now().UTC()
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "earlier task", MessageRef: "messages/2026-04-18/old.md", TS: now.Add(-2 * time.Second)},
		{Role: store.RoleSent, Content: "done", TS: now.Add(-1 * time.Second)},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("seed alice chat: %v", err)
		}
	}
	chatBefore := readChat(t, srv, "alice")

	// (1) Broadcast wake: simulate the release-all WakeAgent call.
	if spawned := srv.spawnChatLoopIfIdle("alice", "release"); spawned {
		t.Errorf("spawnChatLoopIfIdle returned true on already-answered agent — gate regressed")
	}
	if streamCalls.Load() != 0 {
		t.Errorf("Claude.Stream called %d times for already-answered wake — gate didn't suppress", streamCalls.Load())
	}

	// (2) chat.jsonl is byte-identical: no spurious assistant_text
	// got persisted by a confused-model reply.
	chatAfter := readChat(t, srv, "alice")
	if len(chatAfter) != len(chatBefore) {
		t.Errorf("chat history grew during suppressed wake: before=%d after=%d", len(chatBefore), len(chatAfter))
	}

	// (3) Now publish a real new message and release-all. The agent
	// has unanswered work → spawn fires.
	publishedMessage(t, srv, store.Message{
		Type: store.MsgNotice, Title: "Q3", From: "chief-of-staff", To: store.Recipients{"alice"},
		Date: time.Now().UTC(), Body: "do it",
	})
	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("release-all: code = %d", rr.Code)
	}
	// release-all is fire-and-forget: the wake spawn publishes the
	// chat-turn inline, the fake answers it on its own goroutine, and
	// core finalizes the turn (chat, usage and graph writes) when the
	// done event lands. Wait for the turn, then for its finalize, so
	// the assertion is deterministic and nothing is still writing when
	// the TempDir goes.
	fake.AwaitTurn(t, 2*time.Second)
	fake.AwaitFinished(t, 5*time.Second)
	if streamCalls.Load() == 0 {
		t.Error("the fake pod never received a chat-turn after a real release — gate is over-suppressing")
	}
}

// TestE2E_DeadLetterBouncesToSender: a message to a recipient that
// doesn't exist (typo, archived agent, never-created hire) is not
// dropped silently. The router queues a CEO-authored notice in the
// SENDER's inbox,
// linked back to the original via InReplyTo. The sender sees it on
// the next release, can correct the recipient, and the audit trail
// preserves the original message.
func TestE2E_DeadLetterBouncesToSender(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// Alice publishes to "ghost" — recipient doesn't exist.
	rel := publishedMessage(t, srv, store.Message{
		Type: store.MsgNotice, Title: "Hello ghost", From: "alice", To: store.Recipients{"ghost"},
		Date: time.Now().UTC(), Body: "anyone there?",
	})

	// Original message is preserved on disk for the forensic record.
	if _, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), rel)); err != nil {
		t.Errorf("original dead-lettered message should still exist on disk: %v", err)
	}

	// "ghost" still doesn't exist (no recipient to find the message).
	if _, err := srv.Store.GetAgent("ghost"); err == nil {
		t.Error("router should not have created the missing recipient")
	}

	// The dead-letter must be queued in alice's inbox as a notice:
	// telling a sender their message went nowhere is a tell — see
	// ceoUndeliveredIsATell. The ask stops reading as awaiting a
	// response because the recipient is archived, which
	// CollectPendingOutbound treats as unanswerable.
	ts, _ := srv.Store.ReadMessageQueue()
	rt := ts.Agents["alice"]
	if len(rt.Inbox) == 0 {
		t.Fatalf("alice has no queued dead-letter; queue = %+v", ts.Agents)
	}
	var dl *store.Message
	for _, p := range rt.Inbox {
		m, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), p))
		if err != nil {
			continue
		}
		if m.Type == store.MsgNotice && m.From == agent.CEOSlug && m.To.Primary() == "alice" {
			dl = &m
			break
		}
	}
	if dl == nil {
		t.Fatalf("no CEO-authored dead-letter notice in alice's queue; queued = %+v", rt.Inbox)
	}
	if dl.InReplyTo != "" {
		t.Errorf("dead-letter notice must be parentless; in_reply_to = %q", dl.InReplyTo)
	}
	if !strings.Contains(dl.Body, rel) {
		t.Errorf("dead-letter should cite the preserved original %s: %q", rel, dl.Body)
	}
	if !strings.Contains(dl.Body, "ghost") {
		t.Errorf("dead-letter body should name the missing recipient: %q", dl.Body)
	}
}

// TestE2E_RouteConcurrentPublishersUnderContention is the stress
// test for the message queue's mutex discipline: 20 senders publish
// 50 messages each, all going through Messenger.Route concurrently.
// At the end every published message must appear in exactly one
// recipient's inbox — no losses, no duplicates, no scrambled queue
// state.
//
// Without Messenger.mu serializing the read-modify-write cycle on
// the message queue, the goroutines would race on the JSON file
// and lose entries to last-writer-wins. The test runs N=1000 events
// to give the race plenty of opportunity to manifest if the lock
// ever weakens.
func TestE2E_RouteConcurrentPublishersUnderContention(t *testing.T) {
	srv := e2eServer(t)
	const senders = 20
	const perSender = 50
	const total = senders * perSender

	// Seed senders + a single recipient.
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	for i := 0; i < senders; i++ {
		slug := fmt.Sprintf("sender-%02d", i)
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: "x", ReportsTo: "chief-of-staff"}, "k"); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}

	type pub struct {
		from, title string
		path        string
	}
	wantPaths := make(chan pub, total)

	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			from := fmt.Sprintf("sender-%02d", i)
			for j := 0; j < perSender; j++ {
				title := fmt.Sprintf("%s-msg-%03d", from, j)
				msg := store.Message{
					Type: store.MsgNotice, Title: title,
					From: from, To: store.Recipients{"alice"},
					// Stagger nanoseconds to avoid filename collisions.
					Date: time.Now().UTC().Add(time.Duration(i*1000+j) * time.Nanosecond),
					Body: "load test",
				}
				abs, err := srv.Store.WriteMessage(msg)
				if err != nil {
					t.Errorf("WriteMessage %s: %v", title, err)
					return
				}
				msg.Path = abs
				if _, err := srv.Messenger.Route(context.Background(), msg); err != nil {
					t.Errorf("Route %s: %v", title, err)
					return
				}
				rel, _ := filepath.Rel(srv.Store.Root(), abs)
				wantPaths <- pub{from: from, title: title, path: rel}
			}
		}()
	}
	wg.Wait()
	close(wantPaths)

	// Every published message should be queued for alice. Build the
	// expected set, then compare against what's on disk.
	wantSet := map[string]string{} // path → title
	for p := range wantPaths {
		wantSet[p.path] = p.title
	}
	if len(wantSet) != total {
		t.Fatalf("test bug: wantSet=%d, expected %d (channel didn't drain)", len(wantSet), total)
	}

	ts, err := srv.Store.ReadMessageQueue()
	if err != nil {
		t.Fatalf("ReadMessageQueue: %v", err)
	}
	got := ts.Agents["alice"].Inbox
	if len(got) != total {
		t.Errorf("alice queue length = %d, want %d (lost messages under concurrent Route)", len(got), total)
	}
	gotSet := map[string]bool{}
	for _, p := range got {
		if gotSet[p] {
			t.Errorf("duplicate inbox entry: %s (concurrent Route allowed double-write)", p)
		}
		gotSet[p] = true
	}
	for p := range wantSet {
		if !gotSet[p] {
			t.Errorf("missing inbox entry for %s (lost during concurrent Route)", p)
		}
	}
}

// TestE2E_InboxHandlesDanglingInReplyTo covers the case where a
// reply's InReplyTo points at a message file that no longer exists
// (or never did). buildHistoryThreads's rootOf walks the chain via
// byPath lookups; a missing parent must break the walk gracefully
// rather than panicking, and the /inbox page must still render the
// orphan reply (anchoring at the orphan's own path as the thread
// root). Without this guarantee the inbox could start rendering
// 500s the moment a single message file gets manually deleted or a
// future rotation reorganizes paths.
func TestE2E_InboxHandlesDanglingInReplyTo(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// Reply with InReplyTo pointing at a path that doesn't exist on disk.
	reply := store.Message{
		Type: store.MsgNotice, Title: "Reply to ghost",
		From: "alice", To: store.Recipients{"chief-of-staff"},
		Date: time.Now().UTC(), Body: "answering the missing message",
		// Dangling: this path was never written.
		InReplyTo: "messages/2026-04-18/never-existed.md",
	}
	if _, err := srv.Store.WriteMessage(reply); err != nil {
		t.Fatalf("write reply: %v", err)
	}

	// /inbox must render the page without panic and without 5xx.
	rr := apiDo(t, srv, http.MethodGet, "/api/v1/home/history", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("home history with dangling InReplyTo = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Reply to ghost") {
		t.Errorf("orphan reply not rendered in inbox history: body has no 'Reply to ghost'")
	}
}

// TestE2E_InboxHandlesInReplyToCycle defends against the (unlikely
// but possible) case of a cycle in InReplyTo pointers — file A
// in_reply_to: B, file B in_reply_to: A. The thread builder uses a
// visited-set to break the walk; without it the rootOf loop runs
// forever and /inbox hangs. This test pins the defense.
func TestE2E_InboxHandlesInReplyToCycle(t *testing.T) {
	srv := e2eServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// Write A with no InReplyTo first to claim a real path.
	a := store.Message{
		Type: store.MsgNotice, Title: "A", From: "alice", To: store.Recipients{"chief-of-staff"},
		Date: time.Now().UTC(), Body: "first",
	}
	aAbs, err := srv.Store.WriteMessage(a)
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	aRel, _ := filepath.Rel(srv.Store.Root(), aAbs)

	// Write B pointing back at A.
	b := store.Message{
		Type: store.MsgNotice, Title: "B", From: "alice", To: store.Recipients{"chief-of-staff"},
		Date: time.Now().UTC().Add(time.Millisecond), Body: "second",
		InReplyTo: aRel,
	}
	bAbs, err := srv.Store.WriteMessage(b)
	if err != nil {
		t.Fatalf("write B: %v", err)
	}
	bRel, _ := filepath.Rel(srv.Store.Root(), bAbs)

	// Now rewrite A so its InReplyTo points at B — completes the cycle.
	a.InReplyTo = bRel
	a.Path = aAbs
	if _, err := srv.Store.WriteMessage(a); err != nil {
		t.Fatalf("rewrite A with cycle: %v", err)
	}

	rr := apiDo(t, srv, http.MethodGet, "/api/v1/home/history", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("home history with cycle = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
}

// TestE2E_DeadLetterWhenSenderAlsoArchivedDoesNotPanic covers the
// edge case where BOTH sender and recipient are gone (e.g., the
// sender was archived between publishing and the next route pass).
// Should warn-and-drop rather than recurse into another dead-letter
// (which would itself fail).
func TestE2E_DeadLetterWhenSenderAlsoArchivedDoesNotPanic(t *testing.T) {
	srv := e2eServer(t)
	// "alice" is never created. Publishing from her to "ghost" should
	// route, find no recipient, attempt to bounce back to alice, find
	// alice is also missing, and gracefully drop.
	abs, err := srv.Store.WriteMessage(store.Message{
		Type: store.MsgNotice, Title: "T", From: "alice", To: store.Recipients{"ghost"},
		Date: time.Now().UTC(), Body: "x",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	msg := store.Message{
		Type: store.MsgNotice, Title: "T", From: "alice", To: store.Recipients{"ghost"},
		Date: time.Now().UTC(), Body: "x", Path: abs,
	}
	out, err := srv.Messenger.Route(context.Background(), msg)
	if err != nil {
		t.Fatalf("Route should not error on double-archived dead-letter: %v", err)
	}
	if len(out.Warnings) == 0 {
		t.Error("expected a warning about the double-archived dead-letter being dropped")
	}
	// Neither agent exists, so neither inbox should have an entry.
	ts, _ := srv.Store.ReadMessageQueue()
	for slug, rt := range ts.Agents {
		if len(rt.Inbox) > 0 {
			t.Errorf("queue mutated for %s on double-archived dead-letter: %v", slug, rt.Inbox)
		}
	}
}
