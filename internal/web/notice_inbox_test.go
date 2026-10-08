package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// seedNoticeQueue wires a server with a runtime + messenger, three
// agents, and one notice from market-analyst queued for all three.
// Returns the server and the notice's store-relative path.
func seedNoticeQueue(t *testing.T) (*Server, string) {
	t.Helper()
	srv := newTestServer(t)
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)
	for _, slug := range []string{"market-analyst", "alice", "bob", "carol"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: "R", ReportsTo: "ceo"}, "k"); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	abs, err := srv.Store.WriteMessage(store.Message{
		Type:  store.MsgNotice,
		Title: "Pricing page changed",
		From:  "market-analyst",
		To:    store.Recipients{"alice", "bob", "carol"},
		Date:  time.Now().UTC(),
		Body:  "List price moved to $49.",
	})
	if err != nil {
		t.Fatalf("write notice: %v", err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	q, _ := srv.Store.ReadMessageQueue()
	q.Agents = map[string]store.AgentQueue{
		"alice": {Inbox: []string{rel}},
		"bob":   {Inbox: []string{rel}},
		"carol": {Inbox: []string{rel}},
	}
	if err := srv.Store.WriteMessageQueue(q); err != nil {
		t.Fatalf("write queue: %v", err)
	}
	return srv, rel
}

func queuedFor(t *testing.T, srv *Server, slug string) []string {
	t.Helper()
	q, err := srv.Store.ReadMessageQueue()
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	return q.Agents[slug].Inbox
}

// TestSnapshotCountsMultiRecipientNoticeOnce: the snapshot's queued
// count and pending set count messages, not pointers, so they agree
// with Home's queue (one row per message; see
// TestAPIHomeQueueOneRowForThreeRecipients).
func TestSnapshotCountsMultiRecipientNoticeOnce(t *testing.T) {
	srv, rel := seedNoticeQueue(t)

	var snap orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(snap.Inbox.PendingPaths) != 1 || snap.Inbox.PendingPaths[0] != rel {
		t.Errorf("pending_paths = %v, want exactly [%s] — one entry per message, not per pointer",
			snap.Inbox.PendingPaths, rel)
	}
	// The live pill reads release.pending_docs, so the snapshot has to
	// agree with the inbox above.
	if snap.Release.PendingDocs != 1 {
		t.Errorf("release.pending_docs = %d, want 1 — one message, three pointers",
			snap.Release.PendingDocs)
	}
	if snap.Release.PendingDocs != len(snap.Inbox.PendingPaths) {
		t.Errorf("pending_docs (%d) and pending_paths (%d) must not disagree",
			snap.Release.PendingDocs, len(snap.Inbox.PendingPaths))
	}
}

// TestReleaseNoticeDeliversToEveryRecipient is the acceptance case:
// one Release on one card, and all three addressees get the full body.
func TestReleaseNoticeDeliversToEveryRecipient(t *testing.T) {
	srv, rel := seedNoticeQueue(t)

	rr := queuePost(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{Path: rel})
	if rr.Code != http.StatusOK {
		t.Fatalf("release code = %d: %s", rr.Code, rr.Body.String())
	}
	for _, slug := range []string{"alice", "bob", "carol"} {
		if q := queuedFor(t, srv, slug); len(q) != 0 {
			t.Errorf("%s's pointer should be drained; got %v", slug, q)
		}
		hist, _ := srv.Store.ReadChatHistory(slug)
		if len(hist) != 1 {
			t.Fatalf("%s should have exactly one delivery; got %d entries", slug, len(hist))
		}
		if !strings.Contains(hist[0].Content, "List price moved to $49.") {
			t.Errorf("%s should get the full body, not a digest; got %q", slug, hist[0].Content)
		}
	}
}

// TestReleaseNoticeAppliesOneCommentToEveryone: the CEO's note is
// written into the single message file, so every recipient sees it.
func TestReleaseNoticeAppliesOneCommentToEveryone(t *testing.T) {
	srv, rel := seedNoticeQueue(t)

	if rr := queuePost(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{Path: rel, Note: "confirm with finance before acting"}); rr.Code != http.StatusOK {
		t.Fatalf("release code = %d: %s", rr.Code, rr.Body.String())
	}
	for _, slug := range []string{"alice", "bob", "carol"} {
		hist, _ := srv.Store.ReadChatHistory(slug)
		if len(hist) != 1 || !strings.Contains(hist[0].Content, "> CEO: confirm with finance before acting") {
			t.Errorf("%s should have received the CEO annotation; got %+v", slug, hist)
		}
	}
}

// TestBounceNoticeCancelsForEveryone: one Bounce cancels the notice
// outright and returns a single message to the sender naming all of
// its recipients. Nobody receives it.
func TestBounceNoticeCancelsForEveryone(t *testing.T) {
	srv, rel := seedNoticeQueue(t)

	rr := queuePost(t, srv, "/api/v1/queue/bounce", apitypes.QueueBounceRequest{Path: rel, Comment: "pricing isn't theirs to track"})
	if rr.Code != http.StatusOK {
		t.Fatalf("bounce code = %d: %s", rr.Code, rr.Body.String())
	}
	for _, slug := range []string{"alice", "bob", "carol"} {
		if q := queuedFor(t, srv, slug); len(q) != 0 {
			t.Errorf("%s's pointer should be gone; got %v", slug, q)
		}
		if h, _ := srv.Store.ReadChatHistory(slug); len(h) != 0 {
			t.Errorf("%s must not receive a bounced notice; got %+v", slug, h)
		}
	}
	hist, _ := srv.Store.ReadChatHistory("market-analyst")
	if len(hist) != 1 {
		t.Fatalf("sender should get exactly one bounce, not one per recipient; got %d", len(hist))
	}
	if !strings.Contains(hist[0].Content, "alice, bob, carol") {
		t.Errorf("bounce should name every cancelled recipient; got %q", hist[0].Content)
	}
}

// TestReleaseAllDrainsNoticePointers: the batch endpoint takes message
// paths, and one path drains every pointer to it.
func TestReleaseAllDrainsNoticePointers(t *testing.T) {
	srv, rel := seedNoticeQueue(t)

	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{Paths: []string{rel}})
	if rr.Code != http.StatusOK {
		t.Fatalf("release-all code = %d: %s", rr.Code, rr.Body.String())
	}
	for _, slug := range []string{"alice", "bob", "carol"} {
		if q := queuedFor(t, srv, slug); len(q) != 0 {
			t.Errorf("%s should be drained; got %v", slug, q)
		}
	}
}

// TestNoticeMovesToHistoryOnRelease: the pending section and the
// history section have to agree — a queued notice belongs in pending,
// a released one in history, and never in both.
func TestNoticeMovesToHistoryOnRelease(t *testing.T) {
	srv, rel := seedNoticeQueue(t)

	rr := apiDo(t, srv, http.MethodGet, "/api/v1/home/history", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "Pricing page changed") {
		t.Error("a queued notice must not appear in history")
	}

	if rr := queuePost(t, srv, "/api/v1/queue/release", apitypes.QueueReleaseRequest{Path: rel}); rr.Code != http.StatusOK {
		t.Fatalf("release code = %d: %s", rr.Code, rr.Body.String())
	}
	rr = apiDo(t, srv, http.MethodGet, "/api/v1/home/history", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Pricing page changed") {
		t.Errorf("a released notice should appear in history; body: %s", rr.Body.String())
	}
}
