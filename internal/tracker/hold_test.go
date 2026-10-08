package tracker

import (
	"context"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
)

// A hold rides the same pipe as every other wake, minus the wake: the
// CEO's lands in every transcript under the assignment at once, quiet, and
// an agent's queues for release and lands quiet then. Nobody is woken
// to be told to stop; a running turn has it folded in, an idle agent
// reads it with the next wake. A resume wakes.
func TestHoldReachesEveryoneUnderTheAssignmentWithoutAWake(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var woke []string
	f.svc.Hooks.WakeAgent = func(slug string) bool { woke = append(woke, slug); return true }
	if _, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Epic", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "Part", Assignee: "bob", Parent: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ReleaseAll(ctx, nil, f.svc.Hooks); err != nil {
		t.Fatal(err)
	}
	// Both have answered their assignments; nothing is owed.
	for _, slug := range []string{"alice", "bob"} {
		if err := f.s.AppendChatMessage(slug, store.ChatMessage{Role: store.RoleSent, Content: "On it."}); err != nil {
			t.Fatal(err)
		}
	}
	woke = nil

	on, off := true, false
	if _, err := f.svc.Update(ctx, "ceo", 1, assignments.UpdateInput{Hold: &on, Note: "Pause."}); err != nil {
		t.Fatal(err)
	}
	if len(woke) != 0 {
		t.Fatalf("the CEO's hold woke %v", woke)
	}
	if q := f.queued("bob"); len(q) != 0 {
		t.Fatalf("the CEO's hold was queued: %v", q)
	}
	hist := f.chat("bob")
	last := hist[len(hist)-1]
	if !last.Quiet || !strings.Contains(last.Content, `ceo put #1 "Epic" on hold: Pause.`) || !strings.Contains(last.Content, `Stop work on #2 "Part" (part of it) until it is resumed`) {
		t.Fatalf("bob's hold delivery = %+v", last)
	}
	// The hold state itself is the wake note's to show
	// (assignments.Set.OpenAssignments); the delivery carries the event alone.
	if strings.Contains(last.Content, "open assignments") {
		t.Fatalf("bob's hold delivery repeats what he holds = %q", last.Content)
	}
	set, err := f.svc.Set()
	if err != nil {
		t.Fatal(err)
	}
	if line := set.OpenAssignments("bob"); line != "Your open assignments:\n- #2 \"Part\" (on hold under #1)" {
		t.Fatalf("bob's wake note section = %q", line)
	}
	if store.SpawnDecision(hist) != store.SpawnIdle {
		t.Fatal("a hold left bob owing a turn")
	}
	hist = f.chat("alice")
	if last = hist[len(hist)-1]; !last.Quiet || !strings.Contains(last.Content, `Stop work on #1 "Epic" until it is resumed`) {
		t.Fatalf("alice's hold delivery = %+v", last)
	}
	f.noPending()

	if _, err := f.svc.Update(ctx, "ceo", 1, assignments.UpdateInput{Hold: &off, Note: "Go."}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(woke, ","); got != "alice,bob" {
		t.Fatalf("the CEO's resume woke %q", got)
	}
	hist = f.chat("bob")
	if last = hist[len(hist)-1]; last.Quiet || store.SpawnDecision(hist) != store.SpawnNow {
		t.Fatalf("bob's resume delivery = %+v", last)
	}
	for _, slug := range []string{"alice", "bob"} {
		if err := f.s.AppendChatMessage(slug, store.ChatMessage{Role: store.RoleSent, Content: "Carrying on."}); err != nil {
			t.Fatal(err)
		}
	}
	f.noPending()

	// An agent's hold queues for release; released, it lands quiet.
	if _, err := f.svc.Update(ctx, "cos", 1, assignments.UpdateInput{Hold: &on, Note: "Pause again."}); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"alice", "bob"} {
		q := f.queued(slug)
		if len(q) != 1 {
			t.Fatalf("%s queue after an agent's hold = %v", slug, q)
		}
		if m := f.message(q[0]); m.Assignment.Op != "held" || !strings.Contains(m.Body, `cos put #1 "Epic" on hold: Pause again.`) {
			t.Fatalf("%s queued hold = %+v", slug, m)
		}
	}
	if _, err := f.m.ReleaseAll(ctx, nil, f.svc.Hooks); err != nil {
		t.Fatal(err)
	}
	hist = f.chat("bob")
	if last = hist[len(hist)-1]; !last.Quiet || last.MessageRef == "" {
		t.Fatalf("bob's released hold = %+v", last)
	}
	if store.SpawnDecision(hist) != store.SpawnIdle {
		t.Fatal("a released hold left bob owing a turn")
	}
	f.noPending()
}
