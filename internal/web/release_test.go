package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

func newTurnServer(t *testing.T) (*Server, *provider.MockClient) {
	t.Helper()
	srv := newTestServer(t)
	mc := &provider.MockClient{}
	srv.Claude = mc
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0", MaxTokens: 1024})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)
	return srv, mc
}

// seedPendingRelease writes a notice from chief-of-staff to
// alice and queues it in alice's release-state inbox. Returns the
// relative path of the message file. Helper used by every release
// test so each one starts from the exact state the UI is showing
// when the CEO clicks Release.
func seedPendingRelease(t *testing.T, srv *Server, body string) string {
	t.Helper()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k"); err != nil && !strings.Contains(err.Error(), "exists") && !strings.Contains(err.Error(), "already active") {
		t.Fatalf("seed cos: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil && !strings.Contains(err.Error(), "exists") && !strings.Contains(err.Error(), "already active") {
		t.Fatalf("seed alice: %v", err)
	}
	m := store.Message{
		Type:  store.MsgNotice,
		Title: "Draft Q3",
		From:  "chief-of-staff",
		To:    store.Recipients{"alice"},
		Date:  time.Now().UTC(),
		Body:  body,
	}
	path, err := srv.Store.WriteMessage(m)
	if err != nil {
		t.Fatalf("write msg: %v", err)
	}
	relPath, _ := filepath.Rel(srv.Store.Root(), path)
	ts, _ := srv.Store.ReadMessageQueue()
	if ts.Agents == nil {
		ts.Agents = map[string]store.AgentQueue{}
	}
	rt := ts.Agents["alice"]
	rt.Inbox = append(rt.Inbox, relPath)
	ts.Agents["alice"] = rt
	if err := srv.Store.WriteMessageQueue(ts); err != nil {
		t.Fatalf("write ts: %v", err)
	}
	return relPath
}

// queuePost sends a JSON body to one of the /api/v1/queue/* endpoints
// the way the web app does: same-origin, application/json, with a
// session. Returns the ResponseRecorder so the caller can assert
// status + body.
func queuePost(t *testing.T, srv *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

// postRelease releases one queued message with an optional note, as
// Home's Release button does. Returns the ResponseRecorder so the
// caller can assert status + body.
func postRelease(t *testing.T, srv *Server, relPath, comment string) *httptest.ResponseRecorder {
	t.Helper()
	return queuePost(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{Path: relPath, Note: comment})
}

// TestReleaseDispatchesWithComment is THE release test: it asserts
// what the user actually sees when they click Release on a pending
// card with text in the comment box. "Release" must mean:
//
//  1. The message leaves the recipient's release-state inbox (the card
//     goes away on /inbox reload).
//  2. The message lands in the recipient's chat.jsonl as an
//     inbox_delivery entry (the recipient has actually received it).
//  3. The CEO's comment is appended to the delivered body as a
//     `> CEO: …` annotation AND persisted back to the message file
//     on disk (so reopening the raw message shows the annotation and
//     any later replay sees the same text the recipient saw).
//
// Asserting only the response and the rewritten file body would pass
// for a handler that never dispatched, so keep the chat.jsonl check.
func TestReleaseDispatchesWithComment(t *testing.T) {
	srv, _ := newTurnServer(t)
	// Disable the Claude follow-up spawn so the test is
	// deterministic — we're asserting delivery, not the response
	// loop. spawnChatLoopIfIdle short-circuits when Claude is nil.
	srv.Claude = nil

	relPath := seedPendingRelease(t, srv, "please draft the Q3 plan")

	rr := postRelease(t, srv, relPath, "include APAC this time")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/queue/release code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// (1) alice's inbox pointer is gone.
	ts, _ := srv.Store.ReadMessageQueue()
	for _, p := range ts.Agents["alice"].Inbox {
		if p == relPath {
			t.Errorf("released message still pending in alice's inbox: %s", p)
		}
	}

	// (2) alice's chat.jsonl has the inbox_delivery with the
	// annotated body. This is the assertion that would have caught
	// the original bug — without it, the handler could skip delivery
	// entirely and the test would still pass.
	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("read alice chat: %v", err)
	}
	var delivery *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "inbox_delivery" && hist[i].MessageRef == relPath {
			delivery = &hist[i]
			break
		}
	}
	if delivery == nil {
		t.Fatalf("no inbox_delivery for %q in alice's chat; got %+v", relPath, hist)
	}
	if delivery.Role != store.RoleReceived {
		t.Errorf("delivery role = %q, want received", delivery.Role)
	}
	if !strings.Contains(delivery.Content, "please draft the Q3 plan") {
		t.Errorf("delivery missing original body; content = %q", delivery.Content)
	}
	if !strings.Contains(delivery.Content, "> CEO: include APAC this time") {
		t.Errorf("delivery missing CEO annotation; content = %q", delivery.Content)
	}

	// (3) the on-disk message file was rewritten with the CEO block
	// so the annotation survives restart / replay / sender inspection.
	got, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), relPath))
	if err != nil {
		t.Fatalf("read message file: %v", err)
	}
	if !strings.Contains(got.Body, "please draft the Q3 plan") {
		t.Errorf("message body lost original text; body = %q", got.Body)
	}
	if !strings.Contains(got.Body, "> CEO: include APAC this time") {
		t.Errorf("message body missing CEO annotation; body = %q", got.Body)
	}
}

// TestReleaseDispatchesWithoutComment covers the "empty comment"
// branch of Release — the CEO releases the message as-is without
// typing anything. Same delivery contract as the with-comment path,
// minus the CEO annotation.
func TestReleaseDispatchesWithoutComment(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.Claude = nil

	relPath := seedPendingRelease(t, srv, "please draft the Q3 plan")

	rr := postRelease(t, srv, relPath, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/queue/release code = %d, body = %s", rr.Code, rr.Body.String())
	}

	ts, _ := srv.Store.ReadMessageQueue()
	for _, p := range ts.Agents["alice"].Inbox {
		if p == relPath {
			t.Errorf("released message still pending in alice's inbox: %s", p)
		}
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	var delivery *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "inbox_delivery" && hist[i].MessageRef == relPath {
			delivery = &hist[i]
			break
		}
	}
	if delivery == nil {
		t.Fatalf("no inbox_delivery for %q in alice's chat; got %+v", relPath, hist)
	}
	if !strings.Contains(delivery.Content, "please draft the Q3 plan") {
		t.Errorf("delivery missing body; content = %q", delivery.Content)
	}
	// No CEO annotation should appear.
	if strings.Contains(delivery.Content, "> CEO:") {
		t.Errorf("empty-comment release should not annotate; content = %q", delivery.Content)
	}
	got, _ := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), relPath))
	if strings.Contains(got.Body, "> CEO:") {
		t.Errorf("empty-comment release should not annotate file; body = %q", got.Body)
	}
}

// TestReleaseRejectsBadPath guards the path-traversal / wrong-prefix
// rejection at the boundary.
func TestReleaseRejectsBadPath(t *testing.T) {
	srv, _ := newTurnServer(t)
	for _, bad := range []string{"../secret", "not-in-messages/x.md"} {
		rr := postRelease(t, srv, bad, "x")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("bad=%q code = %d, want 400", bad, rr.Code)
		}
	}
}

// TestReleaseRefusesWhenNotPending covers the "stale card"
// case: the CEO clicks Release on a message that was already
// dispatched (or was bounced) in another tab. We answer 404 (no longer queued) so the
// client can alert instead of silently succeeding on a no-op.
func TestReleaseRefusesWhenNotPending(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.Claude = nil
	m := store.Message{
		Type: store.MsgNotice, Title: "t", From: "chief-of-staff", To: store.Recipients{"alice"},
		Date: time.Now().UTC(), Body: "body",
	}
	path, _ := srv.Store.WriteMessage(m)
	relPath, _ := filepath.Rel(srv.Store.Root(), path)
	// Note: NOT added to any agent's inbox.

	rr := postRelease(t, srv, relPath, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("release on non-pending message code = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
}

// TestTurnBounceRemovesFromInboxAndDeliversToSender locks in the
// bounce flow: the bounced message's release-inbox pointer is dropped
// from its intended recipient, and a CEO-authored notice is
// delivered IMMEDIATELY into the original sender's chat.jsonl —
// not queued for next release. The bounce body must NOT include the
// original message text (the recipient already has it via
// in_reply_to; quoting it just inflates every future prompt).
func TestTurnBounceRemovesFromInboxAndDeliversToSender(t *testing.T) {
	srv, _ := newTurnServer(t)
	// Disable Claude so spawnChatLoopIfIdle is a no-op — we only
	// want to assert what landed in chat.jsonl.
	srv.Claude = nil
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed cos: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	const originalBody = "please do the thing in detail"
	message := store.Message{
		Type:  store.MsgNotice,
		Title: "Do the thing",
		From:  "chief-of-staff",
		To:    store.Recipients{"alice"},
		Date:  time.Now().UTC(),
		Body:  originalBody,
	}
	path, err := srv.Store.WriteMessage(message)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	relPath, _ := filepath.Rel(srv.Store.Root(), path)

	ts, _ := srv.Store.ReadMessageQueue()
	if ts.Agents == nil {
		ts.Agents = map[string]store.AgentQueue{}
	}
	rt := ts.Agents["alice"]
	rt.Inbox = append(rt.Inbox, relPath)
	ts.Agents["alice"] = rt
	if err := srv.Store.WriteMessageQueue(ts); err != nil {
		t.Fatalf("write ts: %v", err)
	}

	rr := queuePost(t, srv, "/api/v1/queue/bounce", apitypes.QueueBounceRequest{Path: relPath, Comment: "rescope first"})
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// The bounced message should no longer be in alice's inbox.
	ts, _ = srv.Store.ReadMessageQueue()
	for _, p := range ts.Agents["alice"].Inbox {
		if p == relPath {
			t.Errorf("bounced message still in alice's inbox: %s", p)
		}
	}
	// And the bounce must NOT have been queued onto chief-of-staff's
	// release inbox — bounces deliver immediately, not via the queue.
	for _, p := range ts.Agents["chief-of-staff"].Inbox {
		t.Errorf("bounce should be delivered immediately, not queued; found %s in chief-of-staff inbox", p)
	}

	// The bounce should appear as an inbox_delivery in chief-of-staff's
	// chat.jsonl, with MessageRef pointing to a parentless notice — a
	// bounce is a tell, not an answer (see ceoUndeliveredIsATell).
	hist, err := srv.Store.ReadChatHistory("chief-of-staff")
	if err != nil {
		t.Fatalf("read chat history: %v", err)
	}
	var bounceMsg store.Message
	var found bool
	for _, cm := range hist {
		if cm.Kind != "inbox_delivery" || cm.MessageRef == "" {
			continue
		}
		m, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), cm.MessageRef))
		if err != nil {
			continue
		}
		if m.Type == store.MsgNotice && m.From == "ceo" {
			bounceMsg = m
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no CEO-authored bounce inbox_delivery in chief-of-staff's chat.jsonl; got %+v", hist)
	}
	if bounceMsg.InReplyTo != "" {
		t.Errorf("bounce notice must be parentless; in_reply_to = %q", bounceMsg.InReplyTo)
	}
	if !strings.Contains(bounceMsg.Body, "rescope first") {
		t.Errorf("bounce body missing CEO comment: %q", bounceMsg.Body)
	}
	// The original message body must NOT be quoted into the bounce —
	// the sender wrote it and still has it in their own chat history.
	if strings.Contains(bounceMsg.Body, originalBody) {
		t.Errorf("bounce body should not include the original message text; got %q", bounceMsg.Body)
	}
}

// queuePendingForAlice writes one more notice for alice and
// appends it to her pending inbox. Used when a test needs more than
// one pending message and seedPendingRelease's idempotent re-seed
// path isn't a perfect fit (it returns "agent already active" on the
// second call against the same alice).
func queuePendingForAlice(t *testing.T, srv *Server, body string) string {
	t.Helper()
	path, err := srv.Store.WriteMessage(store.Message{
		Type:  store.MsgNotice,
		Title: "Draft Q3",
		From:  "chief-of-staff",
		To:    store.Recipients{"alice"},
		Date:  time.Now().UTC(),
		Body:  body,
	})
	if err != nil {
		t.Fatalf("write msg: %v", err)
	}
	relPath, _ := filepath.Rel(srv.Store.Root(), path)
	ts, _ := srv.Store.ReadMessageQueue()
	if ts.Agents == nil {
		ts.Agents = map[string]store.AgentQueue{}
	}
	rt := ts.Agents["alice"]
	rt.Inbox = append(rt.Inbox, relPath)
	ts.Agents["alice"] = rt
	if err := srv.Store.WriteMessageQueue(ts); err != nil {
		t.Fatalf("write ts: %v", err)
	}
	return relPath
}

// TestReleaseAllAppliesPerPathComments: a CEO can type notes into N pending cards, click
// Release-all once, and each message lands annotated. The batch
// semantics (single queue lock, atomic drain, broadcast wake) must
// stay intact — fan-out per /release would serialize delivery.
func TestReleaseAllAppliesPerPathComments(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.Claude = nil

	annotated := seedPendingRelease(t, srv, "first message body")
	plain := queuePendingForAlice(t, srv, "second message body")

	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{
		Notes: map[string]string{annotated: "include APAC this time", plain: "   "},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/queue/release-all code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Queue fully drained.
	ts, _ := srv.Store.ReadMessageQueue()
	if got := len(ts.Agents["alice"].Inbox); got != 0 {
		t.Errorf("alice inbox post-release-all = %d, want 0", got)
	}

	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("read alice chat: %v", err)
	}
	byPath := make(map[string]store.ChatMessage)
	for _, cm := range hist {
		if cm.Kind == "inbox_delivery" && cm.MessageRef != "" {
			byPath[cm.MessageRef] = cm
		}
	}
	a, ok := byPath[annotated]
	if !ok {
		t.Fatalf("no inbox_delivery for %q in alice's chat; got %+v", annotated, hist)
	}
	if !strings.Contains(a.Content, "> CEO: include APAC this time") {
		t.Errorf("annotated delivery missing CEO block; content = %q", a.Content)
	}
	p, ok := byPath[plain]
	if !ok {
		t.Fatalf("no inbox_delivery for %q in alice's chat", plain)
	}
	// Whitespace-only comment must be treated as "no annotation" —
	// otherwise we'd write "> CEO:    " into the chat body.
	if strings.Contains(p.Content, "> CEO:") {
		t.Errorf("whitespace-only comment leaked a CEO block; content = %q", p.Content)
	}

	// On-disk message body for the annotated path carries the block
	// (so replay / sender inspection / forensic reads see the same
	// text the recipient saw). The plain path's body is untouched.
	got, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), annotated))
	if err != nil {
		t.Fatalf("read annotated message: %v", err)
	}
	if !strings.Contains(got.Body, "> CEO: include APAC this time") {
		t.Errorf("annotated message file missing CEO block on disk; body = %q", got.Body)
	}
	got, err = srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), plain))
	if err != nil {
		t.Fatalf("read plain message: %v", err)
	}
	if strings.Contains(got.Body, "> CEO:") {
		t.Errorf("plain message file picked up an annotation; body = %q", got.Body)
	}
}

// TestReleaseAllWithoutComments: a POST with no body still drains every
// queue (the no-annotation client path).
func TestReleaseAllWithoutComments(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.Claude = nil
	relPath := seedPendingRelease(t, srv, "body")

	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/queue/release-all code = %d, body = %s", rr.Code, rr.Body.String())
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	delivered := false
	for _, cm := range hist {
		if cm.Kind == "inbox_delivery" && cm.MessageRef == relPath {
			delivered = true
			if strings.Contains(cm.Content, "> CEO:") {
				t.Errorf("nil-body release-all should not annotate; content = %q", cm.Content)
			}
		}
	}
	if !delivered {
		t.Fatal("release-all (nil body) did not deliver the pending message")
	}
}

// TestReleaseAllRejectsBadPathInComments protects the notes map from
// path-traversal. Its keys must be store-relative `messages/...`
// paths; a typo or malicious payload must not trick the server into
// annotating an arbitrary file with `> CEO: ...`. The API refuses the
// whole request rather than dropping the bad key: a client that named
// what to annotate must never get something else released.
func TestReleaseAllRejectsBadPathInComments(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.Claude = nil
	relPath := seedPendingRelease(t, srv, "legit body")

	// Two suspicious entries that the handler must NOT honor:
	// a bare relative path (no `messages/` prefix) and a `..` escape.
	for _, bad := range []string{"../etc/passwd", "not-messages/x.md"} {
		rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{
			Notes: map[string]string{bad: "x", relPath: "keep this one"},
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("notes key %q: code = %d, want 400; body = %s", bad, rr.Code, rr.Body.String())
		}
	}

	got, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), relPath))
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if strings.Contains(got.Body, "> CEO:") {
		t.Errorf("a refused release-all annotated the message; body = %q", got.Body)
	}
	ts, _ := srv.Store.ReadMessageQueue()
	if len(ts.Agents["alice"].Inbox) != 1 {
		t.Errorf("a refused release-all released something; alice inbox = %v", ts.Agents["alice"].Inbox)
	}
}

func TestTurnExecuteWithoutEngine(t *testing.T) {
	srv := newTestServer(t) // no engine
	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d", rr.Code)
	}
}
