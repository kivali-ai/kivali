package tracker

import (
	"context"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/assignments"
)

// The first child raises the nudge on the parent's file with no log
// entry. Telling it is the wake note's job (agent.BuildWakeUpdate) and
// clearing it once told is the store's (Store.ClearAssignmentNudges), again
// with no log entry; the note's own test covers "once".
func TestFirstChildRaisesTheNudgeOnTheFile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Epic", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "Part", Assignee: "bob", Parent: 1}); err != nil {
		t.Fatal(err)
	}
	epic, err := f.s.ReadAssignment(1)
	if err != nil {
		t.Fatal(err)
	}
	if !epic.Nudge {
		t.Fatal("the first child did not raise the parent's nudge on disk")
	}
	if epic.LastSeq() != 1 {
		t.Fatalf("raising the nudge added a log entry: %+v", epic.Log)
	}
	set, err := f.svc.Set()
	if err != nil {
		t.Fatal(err)
	}
	if got := set.Nudges("alice"); len(got) != 1 || got[0] != 1 {
		t.Fatalf("alice's nudges = %v", got)
	}
	// Bob holds the child, not the parent: no nudge for him.
	if got := set.Nudges("bob"); len(got) != 0 {
		t.Fatalf("bob nudged: %v", got)
	}
	// An id that is not nudged, or does not exist, is skipped.
	if err := f.s.ClearAssignmentNudges([]int{1, 2, 99}); err != nil {
		t.Fatal(err)
	}
	epic, _ = f.s.ReadAssignment(1)
	if epic.Nudge {
		t.Fatal("the store did not clear the nudge")
	}
	if epic.LastSeq() != 1 {
		t.Fatalf("clearing the nudge added a log entry: %+v", epic.Log)
	}
}

// A done close with unmet items succeeds; the warning reaches the
// creator's chat through the same wake the close already sends.
func TestCloseWarningTravelsInTheWake(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, "ceo", assignments.CreateInput{Title: "Release", Assignee: "alice", Acceptance: []string{"step A", "step B"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "Review login page", Assignee: "bob", Parent: 1, Satisfies: []string{"step A"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Close(ctx, "bob", 2, assignments.ResolutionDone, "placed"); err != nil {
		t.Fatal(err)
	}
	ch, err := f.svc.Close(ctx, "alice", 1, assignments.ResolutionDone, "shipped A only")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Unmet) != 1 || ch.Unmet[0] != "step B" {
		t.Fatalf("unmet = %v", ch.Unmet)
	}
	// The CEO filed it, so the close lands in the CEO's inbox now; the
	// card in their chat points at the message that carries the body.
	hist := f.chat("ceo")
	if len(hist) == 0 || !strings.Contains(hist[len(hist)-1].MessageRef, "-assignment_event-alice--to--ceo.md") {
		t.Fatalf("ceo chat = %+v", hist)
	}
	m := f.message(hist[len(hist)-1].MessageRef)
	if !strings.Contains(m.Body, `Warning: closed as done with 1 Done-when condition unmet: "step B".`) {
		t.Fatalf("close wake body:\n%s", m.Body)
	}
	rel, _ := f.s.ReadAssignment(1)
	if last := rel.Log[len(rel.Log)-1]; len(last.Unmet) != 1 || last.Unmet[0] != "step B" {
		t.Fatalf("log entry = %+v", last)
	}
}
