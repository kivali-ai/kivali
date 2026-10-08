package tracker

import (
	"context"
	"testing"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
)

// A crash between a wake's delivery and the deletion of its pending
// record makes boot route the record again. The queue path is
// idempotent (RouteProduced skips a path already queued), but the two
// instant paths — the CEO's own change to an agent, and any change
// addressed to the CEO — append to chat.jsonl with no such check, so
// the replay delivers the same wake twice.
func TestReconcileDoesNotRedeliverAnInstantWake(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// CEO → alice: delivered at once into alice's chat.
	ch, err := f.svc.Create(ctx, assignments.CEO, assignments.CreateInput{Title: "Draft the release notes", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	f.noPending()
	if n := countKind(f.chat("alice"), "inbox_delivery"); n != 1 {
		t.Fatalf("alice has %d deliveries after the create, want 1", n)
	}
	// Simulate the crash window: the wake landed, the record survived.
	if err := f.s.WritePendingWakes(store.PendingWakes{Seq: ch.Entries[0].Seq, Assignment: ch.Assignment.ID, By: assignments.CEO, At: ch.Entries[0].TS, Wakes: ch.Wakes}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countKind(f.chat("alice"), "inbox_delivery"); n != 1 {
		t.Errorf("alice has %d deliveries after the boot replay, want 1 (the same wake was delivered twice)", n)
	}

	// alice → ceo: a question lands in the CEO's inbox at once.
	ch, err = f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "Which runner?", Assignee: assignments.CEO, Parent: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.noPending()
	if n := countKind(f.chat(assignments.CEO), "ceo_inbox"); n != 1 {
		t.Fatalf("ceo has %d inbox entries after the create, want 1", n)
	}
	if err := f.s.WritePendingWakes(store.PendingWakes{Seq: ch.Entries[0].Seq, Assignment: ch.Assignment.ID, By: "alice", At: ch.Entries[0].TS, Wakes: ch.Wakes}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countKind(f.chat(assignments.CEO), "ceo_inbox"); n != 1 {
		t.Errorf("ceo has %d inbox entries after the boot replay, want 1 (the same question is now two cards)", n)
	}
}

func countKind(hist []store.ChatMessage, kind string) int {
	n := 0
	for _, m := range hist {
		if m.Kind == kind {
			n++
		}
	}
	return n
}
