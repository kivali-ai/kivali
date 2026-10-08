package messaging

import (
	"context"
	"strings"
	"testing"
	"time"
)

// deliveredRefs returns the MessageRefs of an agent's inbox_delivery
// chat entries, in the order they were appended.
func deliveredRefs(t *testing.T, m *Messenger, slug string) []string {
	t.Helper()
	hist, err := m.Store.ReadChatHistory(slug)
	if err != nil {
		t.Fatalf("read chat history for %s: %v", slug, err)
	}
	var refs []string
	for _, h := range hist {
		if h.Kind == "inbox_delivery" {
			refs = append(refs, h.MessageRef)
		}
	}
	return refs
}

// TestReleaseAllOrdersDeliveriesBySendTime is the ordering contract:
// when several messages land in one recipient's chat in a single
// release, they read oldest-first by when their senders sent them.
//
// Queue position can't be trusted for this. A message is routed onto
// the inbox at the end of its sender's turn, so a long turn queues a
// message that was composed early behind one a shorter turn sent
// later. The seeds below reproduce exactly that: carol spoke first but
// her pointer was appended last.
func TestReleaseAllOrdersDeliveriesBySendTime(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	rec := &recordingHooks{store: m.Store}

	base := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	second := seedQueuedAt(t, m, "bob", "alice", "second", base.Add(2*time.Minute))
	third := seedQueuedAt(t, m, "dave", "alice", "third", base.Add(5*time.Minute))
	first := seedQueuedAt(t, m, "carol", "alice", "first", base)

	warnings, err := m.ReleaseAll(context.Background(), nil, rec.hooks())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	got := deliveredRefs(t, m, "alice")
	want := []string{first, second, third}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("delivery order =\n  %v\nwant (oldest first)\n  %v", got, want)
	}
}

// TestReleaseSelectedOrdersDeliveriesBySendTime covers the filtered
// shape: the selection order the CEO clicked in must not leak into the
// recipient's chat, and the messages left behind keep their queue
// order for the next release.
func TestReleaseSelectedOrdersDeliveriesBySendTime(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	rec := &recordingHooks{store: m.Store}

	base := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	newest := seedQueuedAt(t, m, "bob", "alice", "newest", base.Add(9*time.Minute))
	held := seedQueuedAt(t, m, "carol", "alice", "held back", base.Add(3*time.Minute))
	oldest := seedQueuedAt(t, m, "dave", "alice", "oldest", base)

	// Selected newest-first — the opposite of the order they should be
	// delivered in.
	delivered, notFound, warnings, err := m.ReleaseSelected(
		context.Background(),
		[]string{newest, oldest},
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
		t.Errorf("notFound = %v, want empty", notFound)
	}
	if len(delivered) != 2 {
		t.Fatalf("delivered = %v, want 2 paths", delivered)
	}

	got := deliveredRefs(t, m, "alice")
	want := []string{oldest, newest}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("delivery order =\n  %v\nwant (oldest first)\n  %v", got, want)
	}

	ts, err := m.Store.ReadMessageQueue()
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	if q := ts.Agents["alice"].Inbox; len(q) != 1 || q[0] != held {
		t.Errorf("alice inbox = %v, want [%s] still queued", q, held)
	}
}

// TestReleaseOrderingIsPerRecipient: sorting reorders each recipient's
// own batch and nothing else. Two agents whose mail interleaves in
// send time each see their own two messages oldest-first.
func TestReleaseOrderingIsPerRecipient(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	mustCreateAgent(t, m.Store, "bob")
	rec := &recordingHooks{store: m.Store}

	base := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	aLate := seedQueuedAt(t, m, "carol", "alice", "alice late", base.Add(6*time.Minute))
	bLate := seedQueuedAt(t, m, "carol", "bob", "bob late", base.Add(4*time.Minute))
	aEarly := seedQueuedAt(t, m, "dave", "alice", "alice early", base.Add(1*time.Minute))
	bEarly := seedQueuedAt(t, m, "dave", "bob", "bob early", base)

	if _, err := m.ReleaseAll(context.Background(), nil, rec.hooks()); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	for _, tc := range []struct {
		slug string
		want []string
	}{
		{"alice", []string{aEarly, aLate}},
		{"bob", []string{bEarly, bLate}},
	} {
		got := deliveredRefs(t, m, tc.slug)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s delivery order =\n  %v\nwant\n  %v", tc.slug, got, tc.want)
		}
	}
}

// TestReleaseOrderingIsStableForEqualSendTimes: two messages stamped
// with the same instant keep their queue order rather than swapping
// between releases. sort.SliceStable is load-bearing here — an
// unstable sort would make the delivered order non-deterministic for
// the messages a single turn published in one batch, which all carry
// the turn's timestamp.
func TestReleaseOrderingIsStableForEqualSendTimes(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	rec := &recordingHooks{store: m.Store}

	same := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	firstQueued := seedQueuedAt(t, m, "bob", "alice", "queued first", same)
	secondQueued := seedQueuedAt(t, m, "carol", "alice", "queued second", same)
	older := seedQueuedAt(t, m, "dave", "alice", "older", same.Add(-time.Hour))

	if _, err := m.ReleaseAll(context.Background(), nil, rec.hooks()); err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	got := deliveredRefs(t, m, "alice")
	want := []string{older, firstQueued, secondQueued}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("delivery order =\n  %v\nwant\n  %v", got, want)
	}
}

// TestReleaseOrderingSurvivesUnreadableMessage: a pointer whose file
// can't be read drops out of the batch with a warning, and the
// messages around it still deliver in send order. Regression guard for
// the read/sort/deliver split — the read failure happens in the first
// pass, before anything is ordered or appended.
func TestReleaseOrderingSurvivesUnreadableMessage(t *testing.T) {
	m := newTestMessenger(t)
	mustCreateAgent(t, m.Store, "alice")
	rec := &recordingHooks{store: m.Store}

	base := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	newer := seedQueuedAt(t, m, "bob", "alice", "newer", base.Add(3*time.Minute))
	older := seedQueuedAt(t, m, "carol", "alice", "older", base)

	// A pointer to a file that was never written.
	missing := "messages/2026-05-04/000000-task_request-ghost--to--alice-T.md"
	ts, _ := m.Store.ReadMessageQueue()
	rt := ts.Agents["alice"]
	rt.Inbox = append([]string{missing}, rt.Inbox...)
	ts.Agents["alice"] = rt
	if err := m.Store.WriteMessageQueue(ts); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	warnings, err := m.ReleaseAll(context.Background(), nil, rec.hooks())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], missing) {
		t.Errorf("warnings = %v, want one naming %s", warnings, missing)
	}

	got := deliveredRefs(t, m, "alice")
	want := []string{older, newer}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("delivery order =\n  %v\nwant\n  %v", got, want)
	}

	// The unreadable pointer is dropped, not retried forever.
	after, _ := m.Store.ReadMessageQueue()
	if q := after.Agents["alice"].Inbox; len(q) != 0 {
		t.Errorf("alice inbox = %v, want empty after release", q)
	}
}
