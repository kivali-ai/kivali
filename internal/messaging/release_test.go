package messaging

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// seedQueued writes a task_request message and queues it on the named
// recipient's inbox. Returns the store-relative path so callers can
// reference it from comments / paths filters.
func seedQueued(t *testing.T, m *Messenger, from, to, body string) string {
	t.Helper()
	return seedQueuedAt(t, m, from, to, body, time.Now().UTC())
}

// seedQueuedAt is seedQueued with an explicit send time, so a test can
// queue messages in an order that disagrees with when their senders
// sent them.
func seedQueuedAt(t *testing.T, m *Messenger, from, to, body string, sent time.Time) string {
	t.Helper()
	msg := store.Message{
		Type:  store.MsgNotice,
		Title: "T",
		From:  from,
		To:    store.Recipients{to},
		Date:  sent,
		Body:  body,
	}
	abs, err := m.Store.WriteMessage(msg)
	if err != nil {
		t.Fatalf("write msg: %v", err)
	}
	rel, _ := filepath.Rel(m.Store.Root(), abs)
	ts, _ := m.Store.ReadMessageQueue()
	if ts.Agents == nil {
		ts.Agents = map[string]store.AgentQueue{}
	}
	rt := ts.Agents[to]
	rt.Inbox = append(rt.Inbox, rel)
	ts.Agents[to] = rt
	if err := m.Store.WriteMessageQueue(ts); err != nil {
		t.Fatalf("write queue: %v", err)
	}
	return rel
}

// TestReleaseSelectedDrainsSubsetOnly is the core P06 fix: when a
// caller asks for two of three queued paths, the queue retains the
// untouched path and only the selected two land in their recipients'
// chat.jsonl. Same single lock, same broadcast wake.
func TestReleaseSelectedDrainsSubsetOnly(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	a1 := seedQueued(t, m, "chief-of-staff", "alice", "first to alice")
	a2 := seedQueued(t, m, "chief-of-staff", "alice", "second to alice")
	b1 := seedQueued(t, m, "chief-of-staff", "bob", "first to bob")

	delivered, notFound, warnings, err := m.ReleaseSelected(
		context.Background(),
		[]string{a1, b1},
		nil,
		rec.hooks(),
	)
	if err != nil {
		t.Fatalf("ReleaseSelected: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(notFound) != 0 {
		t.Errorf("notFound should be empty for queued paths; got %v", notFound)
	}
	sort.Strings(delivered)
	want := []string{a1, b1}
	sort.Strings(want)
	if strings.Join(delivered, "|") != strings.Join(want, "|") {
		t.Errorf("delivered = %v, want %v", delivered, want)
	}

	// a2 stays queued; a1, b1 are gone.
	ts, _ := m.Store.ReadMessageQueue()
	if got := ts.Agents["alice"].Inbox; len(got) != 1 || got[0] != a2 {
		t.Errorf("alice inbox = %v, want [%s]", got, a2)
	}
	if got := ts.Agents["bob"].Inbox; len(got) != 0 {
		t.Errorf("bob inbox = %v, want empty", got)
	}

	// alice's chat got one delivery (a1, not a2); bob's chat got one (b1).
	aliceHist, _ := m.Store.ReadChatHistory("alice")
	if len(aliceHist) != 1 || aliceHist[0].MessageRef != a1 {
		t.Errorf("alice chat = %v, want one entry referencing %s", aliceHist, a1)
	}
	bobHist, _ := m.Store.ReadChatHistory("bob")
	if len(bobHist) != 1 || bobHist[0].MessageRef != b1 {
		t.Errorf("bob chat = %v, want one entry referencing %s", bobHist, b1)
	}
}

// TestReleaseSelectedAppliesAnnotation is the data-loss case the CEO
// reported in P06: the typed `> CEO: ...` markup must land in the
// delivered message body — preserved across the selected-subset drain.
// Comments keyed on unselected paths are ignored.
func TestReleaseSelectedAppliesAnnotation(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	rec := &recordingHooks{store: m.Store}

	selected := seedQueued(t, m, "chief-of-staff", "alice", "draft Q3 plan")
	unselected := seedQueued(t, m, "chief-of-staff", "alice", "later")

	comments := map[string]string{
		selected:   "include APAC this time",
		unselected: "ignored — not in the paths filter",
	}
	delivered, _, _, err := m.ReleaseSelected(
		context.Background(),
		[]string{selected},
		comments,
		rec.hooks(),
	)
	if err != nil {
		t.Fatalf("ReleaseSelected: %v", err)
	}
	if len(delivered) != 1 || delivered[0] != selected {
		t.Fatalf("delivered = %v, want [%s]", delivered, selected)
	}

	// Annotation persisted to the message file (so any replay sees it).
	abs := filepath.Join(m.Store.Root(), selected)
	msg, err := m.Store.ReadMessage(abs)
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if !strings.Contains(msg.Body, "> CEO: include APAC this time") {
		t.Errorf("selected message body missing CEO annotation:\n%s", msg.Body)
	}

	// Unselected message stays unannotated even though comments[path]
	// was non-empty for it.
	unAbs := filepath.Join(m.Store.Root(), unselected)
	unMsg, err := m.Store.ReadMessage(unAbs)
	if err != nil {
		t.Fatalf("read unselected message: %v", err)
	}
	if strings.Contains(unMsg.Body, "> CEO:") {
		t.Errorf("unselected message should NOT have been annotated; body=%q", unMsg.Body)
	}

	// alice's chat entry carries the annotated body too — that's the
	// part the recipient actually sees.
	hist, _ := m.Store.ReadChatHistory("alice")
	if len(hist) != 1 {
		t.Fatalf("alice chat len = %d, want 1", len(hist))
	}
	if !strings.Contains(hist[0].Content, "> CEO: include APAC this time") {
		t.Errorf("delivered chat content missing CEO annotation: %q", hist[0].Content)
	}
}

// TestReleaseSelectedReportsNotFound covers the stale-card case the
// /release handler maps to 409: a requested path that isn't in any
// agent's inbox at lock time is surfaced via notFound. The rest of
// the batch still drains normally so a partially-stale selection
// doesn't fail the whole batch.
func TestReleaseSelectedReportsNotFound(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	rec := &recordingHooks{store: m.Store}

	queued := seedQueued(t, m, "chief-of-staff", "alice", "queued body")
	stale := "messages/2026-04-01/never-queued.md"

	delivered, notFound, _, err := m.ReleaseSelected(
		context.Background(),
		[]string{queued, stale},
		nil,
		rec.hooks(),
	)
	if err != nil {
		t.Fatalf("ReleaseSelected: %v", err)
	}
	if len(delivered) != 1 || delivered[0] != queued {
		t.Errorf("delivered = %v, want [%s]", delivered, queued)
	}
	if len(notFound) != 1 || notFound[0] != stale {
		t.Errorf("notFound = %v, want [%s]", notFound, stale)
	}
}

// TestReleaseAllDrainsEverything: the nil-filter path (Release-all
// with no selection) goes through the same releaseQueue body and
// drains everything.
func TestReleaseAllDrainsEverything(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	_ = seedQueued(t, m, "chief-of-staff", "alice", "a1")
	_ = seedQueued(t, m, "chief-of-staff", "alice", "a2")
	_ = seedQueued(t, m, "chief-of-staff", "bob", "b1")

	warnings, err := m.ReleaseAll(context.Background(), nil, rec.hooks())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	ts, _ := m.Store.ReadMessageQueue()
	if got := ts.Agents["alice"].Inbox; len(got) != 0 {
		t.Errorf("alice inbox should be empty; got %v", got)
	}
	if got := ts.Agents["bob"].Inbox; len(got) != 0 {
		t.Errorf("bob inbox should be empty; got %v", got)
	}
}

// TestReleaseSelectedEmptyIsNoop ensures an empty paths slice returns
// nil/nil/nil/nil without locking the queue. Useful when client-side
// race trims the selection down to zero between submit and dispatch.
func TestReleaseSelectedEmptyIsNoop(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	rec := &recordingHooks{store: m.Store}

	queued := seedQueued(t, m, "chief-of-staff", "alice", "still queued")

	delivered, notFound, warnings, err := m.ReleaseSelected(
		context.Background(),
		nil,
		nil,
		rec.hooks(),
	)
	if err != nil {
		t.Fatalf("ReleaseSelected(nil): %v", err)
	}
	if delivered != nil || notFound != nil || warnings != nil {
		t.Errorf("expected all-nil returns; got delivered=%v notFound=%v warnings=%v",
			delivered, notFound, warnings)
	}
	ts, _ := m.Store.ReadMessageQueue()
	if got := ts.Agents["alice"].Inbox; len(got) != 1 || got[0] != queued {
		t.Errorf("alice inbox = %v, want [%s] (no-op should not drain)", got, queued)
	}
}
