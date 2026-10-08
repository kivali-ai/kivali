package assignments

import (
	"strings"
	"testing"
)

// A release with five items and children claiming three: the counts,
// the item table, the wake line, and the warning a done close leaves.
func TestAcceptanceCountsAndCloseWarning(t *testing.T) {
	h := newHarness(t, "cos", "vp", "alice", "bob")
	rel := h.create("cos", CreateInput{Title: "v3 release", Assignee: "vp", Acceptance: []string{
		"login page reviewed", "export page reviewed", "settings page reviewed", "version tagged", "release notes",
	}}).Assignment
	a := h.create("vp", CreateInput{Title: "Review login page", Assignee: "alice", Parent: rel.ID, Satisfies: []string{"login page reviewed"}}).Assignment
	b := h.create("vp", CreateInput{Title: "Review export page", Assignee: "alice", Parent: rel.ID, Satisfies: []string{"export page reviewed"}}).Assignment
	fw := h.create("vp", CreateInput{Title: "Tag the version", Assignee: "bob", Parent: rel.ID, Satisfies: []string{"version tagged"}}).Assignment

	p, ok := h.set.Progress(rel.ID)
	if !ok || p.String() != "0 met / 3 claimed-open / 2 unclaimed" {
		t.Fatalf("progress = %v %v", p, ok)
	}
	h.close(a.ID, "alice", ResolutionDone, "done")
	h.close(b.ID, "alice", ResolutionDone, "done")
	h.close(fw.ID, "bob", ResolutionDone, "done")
	p, _ = h.set.Progress(rel.ID)
	if p.String() != "3 met / 0 claimed-open / 2 unclaimed" {
		t.Fatalf("progress after closes = %s", p)
	}
	items := h.set.Items(rel.ID)
	if len(items) != 5 || items[0].State() != ConditionMet || items[0].SatisfiedBy[0] != a.ID || items[2].State() != ConditionUnclaimed || items[4].State() != ConditionUnclaimed {
		t.Fatalf("items = %+v", items)
	}
	if got := h.set.Unmet(rel.ID); strings.Join(got, "|") != "settings page reviewed|release notes" {
		t.Fatalf("unmet = %v", got)
	}
	// The listing row and the wake note carry the counts, not the
	// child counts.
	r, _ := h.set.Get(rel.ID)
	if line := h.set.Line(r); !strings.Contains(line, "done when 3 met / 0 claimed-open / 2 unclaimed") || strings.Contains(line, "open)") {
		t.Errorf("line = %s", line)
	}
	if got := h.set.OpenAssignments("vp"); got != "Your open assignments:\n- #1 \"v3 release\" (ready; done when 3 met / 0 claimed-open / 2 unclaimed)" {
		t.Errorf("wake note section = %q", got)
	}
	// A leaf's row says what it counts toward.
	ca, _ := h.set.Get(a.ID)
	if line := h.set.Line(ca); !strings.Contains(line, `counts toward "login page reviewed"`) {
		t.Errorf("leaf line = %s", line)
	}

	// Closing as done with two unmet succeeds, logs the names, returns
	// them, and the creator's wake carries the warning.
	ch := h.close(rel.ID, "vp", ResolutionDone, "Shipped what we had.")
	if strings.Join(ch.Unmet, "|") != "settings page reviewed|release notes" {
		t.Fatalf("change unmet = %v", ch.Unmet)
	}
	last := ch.Assignment.Log[len(ch.Assignment.Log)-1]
	if last.Op != OpClosed || strings.Join(last.Unmet, "|") != "settings page reviewed|release notes" {
		t.Fatalf("log entry = %+v", last)
	}
	if len(ch.Wakes) != 1 || ch.Wakes[0].To != "cos" || !strings.Contains(ch.Wakes[0].Body, `Warning: closed as done with 2 Done-when conditions unmet: "settings page reviewed", "release notes".`) {
		t.Fatalf("wakes = %+v", ch.Wakes)
	}
	// The record round-trips with the new fields and the warning.
	data, err := Marshal(ch.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("parse:\n%s\n%v", data, err)
	}
	if len(back.Acceptance) != 5 || len(back.Log[len(back.Log)-1].Unmet) != 2 {
		t.Fatalf("round trip lost fields:\n%s", data)
	}
}

// A leaf's done close meets its parent's item; the parent's clean
// close meets the grandparent's; a drop meets nothing.
func TestSatisfactionPropagatesThreeLevels(t *testing.T) {
	h := newHarness(t, "cos", "vp", "alice")
	program := h.create("cos", CreateInput{Title: "Program", Assignee: "vp", Acceptance: []string{"v3 shipped", "v4 shipped"}}).Assignment
	leg := h.create("vp", CreateInput{Title: "v3 leg", Assignee: "vp", Parent: program.ID, Satisfies: []string{"v3 shipped"}, Acceptance: []string{"build", "tests"}}).Assignment
	place := h.create("vp", CreateInput{Title: "Build", Assignee: "alice", Parent: leg.ID, Satisfies: []string{"build"}}).Assignment
	route := h.create("vp", CreateInput{Title: "Test", Assignee: "alice", Parent: leg.ID, Satisfies: []string{"tests"}}).Assignment

	if p, _ := h.set.Progress(program.ID); p.String() != "0 met / 1 claimed-open / 1 unclaimed" {
		t.Fatalf("program = %s", p)
	}
	h.close(place.ID, "alice", ResolutionDone, "placed")
	if p, _ := h.set.Progress(leg.ID); p.String() != "1 met / 1 claimed-open / 0 unclaimed" {
		t.Fatalf("leg after place = %s", p)
	}
	// Dropping the routing leaf satisfies nothing: the item is back
	// to unclaimed and the leg cannot close cleanly.
	h.close(route.ID, "vp", ResolutionDropped, "not this rev")
	if p, _ := h.set.Progress(leg.ID); p.String() != "1 met / 0 claimed-open / 1 unclaimed" {
		t.Fatalf("leg after drop = %s", p)
	}
	if got := h.set.Unmet(leg.ID); len(got) != 1 || got[0] != "tests" {
		t.Fatalf("leg unmet = %v", got)
	}
	// Someone files the routing work again; done meets it.
	route2 := h.create("vp", CreateInput{Title: "Route again", Assignee: "alice", Parent: leg.ID, Satisfies: []string{"tests"}}).Assignment
	h.close(route2.ID, "alice", ResolutionDone, "routed")
	if got := h.set.Unmet(leg.ID); len(got) != 0 {
		t.Fatalf("leg unmet after route2 = %v", got)
	}
	// The leg closes without a warning, and that meets the program's
	// item; the other item stays unclaimed.
	ch := h.close(leg.ID, "vp", ResolutionDone, "leg done")
	if len(ch.Unmet) != 0 || len(ch.Assignment.Log[len(ch.Assignment.Log)-1].Unmet) != 0 {
		t.Fatalf("clean close carried a warning: %v", ch.Unmet)
	}
	for _, w := range ch.Wakes {
		if strings.Contains(w.Body, "Warning") {
			t.Fatalf("clean close warned in a wake: %s", w.Body)
		}
	}
	if p, _ := h.set.Progress(program.ID); p.String() != "1 met / 0 claimed-open / 1 unclaimed" {
		t.Fatalf("program after leg = %s", p)
	}
	// Reopening the leg un-satisfies the program's item.
	h.reopen(leg.ID, "vp", "routing was wrong")
	if p, _ := h.set.Progress(program.ID); p.String() != "0 met / 1 claimed-open / 1 unclaimed" {
		t.Fatalf("program after reopen = %s", p)
	}
}

// Two children naming one item: the first done close satisfies it,
// the item counts once, and it stays satisfied while the other is
// open.
func TestDuplicateClaimCountsOnce(t *testing.T) {
	h := newHarness(t, "cos", "alice", "bob")
	rel := h.create("cos", CreateInput{Title: "Release", Assignee: "cos", Acceptance: []string{"certified"}}).Assignment
	one := h.create("cos", CreateInput{Title: "One", Assignee: "alice", Parent: rel.ID, Satisfies: []string{"certified"}}).Assignment
	two := h.create("cos", CreateInput{Title: "Two", Assignee: "bob", Parent: rel.ID, Satisfies: []string{"certified"}}).Assignment
	if p, _ := h.set.Progress(rel.ID); p.Total() != 1 || p.Claimed != 1 {
		t.Fatalf("progress = %s", p)
	}
	h.close(one.ID, "alice", ResolutionDone, "ok")
	items := h.set.Items(rel.ID)
	if len(items) != 1 || items[0].State() != ConditionMet || len(items[0].SatisfiedBy) != 1 || items[0].SatisfiedBy[0] != one.ID || len(items[0].ClaimedBy) != 1 || items[0].ClaimedBy[0] != two.ID {
		t.Fatalf("items = %+v", items)
	}
	if p, _ := h.set.Progress(rel.ID); p.String() != "1 met / 0 claimed-open / 0 unclaimed" {
		t.Fatalf("progress = %s", p)
	}
	// Reopening the first leaves the item claimed by both; closing the
	// second then satisfies it.
	h.reopen(one.ID, "cos", "redo")
	if p, _ := h.set.Progress(rel.ID); p.String() != "0 met / 1 claimed-open / 0 unclaimed" {
		t.Fatalf("after reopen = %s", p)
	}
	h.close(two.ID, "bob", ResolutionDone, "ok")
	if p, _ := h.set.Progress(rel.ID); p.Satisfied != 1 {
		t.Fatalf("after second close = %s", p)
	}
}

// The store's rules on the two fields.
func TestAcceptanceAndSatisfiesRules(t *testing.T) {
	h := newHarness(t, "cos", "vp", "alice", "bob")
	// Duplicate names within one assignment.
	_, err := h.rules().Create("cos", CreateInput{Title: "Dup", Acceptance: []string{"a", " a "}})
	wantRefusal(t, err, `acceptance names "a" twice`)
	// Satisfies without a parent.
	_, err = h.rules().Create("cos", CreateInput{Title: "Orphan", Satisfies: []string{"a"}})
	wantRefusal(t, err, "is part of nothing, so there is nothing for it to count toward")
	rel := h.create("cos", CreateInput{Title: "Release", Assignee: "vp", Acceptance: []string{"step A", "step B"}}).Assignment
	// Satisfies naming an item the parent does not declare: the
	// refusal carries the parent's list.
	_, err = h.rules().Create("vp", CreateInput{Title: "Review billing page", Assignee: "alice", Parent: rel.ID, Satisfies: []string{"step D"}})
	wantRefusal(t, err, `#1's Done-when conditions are "step A", "step B"`)
	// A parent with no items has nothing to satisfy.
	bare := h.create("cos", CreateInput{Title: "Bare", Assignee: "vp"}).Assignment
	_, err = h.rules().Create("vp", CreateInput{Title: "Under bare", Parent: bare.ID, Satisfies: []string{"x"}})
	wantRefusal(t, err, "has no Done-when conditions to count toward")

	child := h.create("vp", CreateInput{Title: "Review login page", Assignee: "alice", Parent: rel.ID, Satisfies: []string{"step A"}}).Assignment
	// Only the child's creator, assignee, the parent's creator or
	// assignee, or the CEO change what it satisfies.
	_, err = h.rules().Update(child.ID, "bob", UpdateInput{AddSatisfies: []string{"step B"}})
	wantRefusal(t, err, "can change what it counts toward")
	ch := h.update(child.ID, "cos", UpdateInput{AddSatisfies: []string{"step B"}, Note: "covers both"})
	if e := ch.Entries[0]; e.Op != OpSatisfies || strings.Join(e.Items, "|") != "step A|step B" || e.Note != "covers both" {
		t.Fatalf("satisfies entry = %+v", e)
	}
	if len(ch.Wakes) != 0 {
		t.Fatalf("a satisfies change woke %v", wakesTo(ch.Wakes))
	}
	_, err = h.rules().Update(child.ID, "alice", UpdateInput{AddSatisfies: []string{"step B"}})
	wantRefusal(t, err, `already has "step B" in satisfies`)
	_, err = h.rules().Update(child.ID, "alice", UpdateInput{RemoveSatisfies: []string{"step Z"}})
	wantRefusal(t, err, `has no "step Z" in satisfies`)
	_, err = h.rules().Update(child.ID, "alice", UpdateInput{AddSatisfies: []string{"step D"}})
	wantRefusal(t, err, `#1's Done-when conditions are "step A", "step B"`)
	// Acceptance: creator, assignee or CEO; not a bystander.
	_, err = h.rules().Update(rel.ID, "bob", UpdateInput{AddAcceptance: []string{"step C"}})
	wantRefusal(t, err, "can change its Done-when conditions")
	// Add and remove by name.
	ch = h.update(rel.ID, "vp", UpdateInput{AddAcceptance: []string{"step C"}})
	if strings.Join(ch.Assignment.Acceptance, "|") != "step A|step B|step C" || ch.Entries[0].Op != OpAcceptance {
		t.Fatalf("after add = %v / %+v", ch.Assignment.Acceptance, ch.Entries)
	}
	_, err = h.rules().Update(rel.ID, "vp", UpdateInput{AddAcceptance: []string{"step C"}})
	wantRefusal(t, err, `already has "step C" in acceptance`)
	_, err = h.rules().Update(rel.ID, "vp", UpdateInput{RemoveAcceptance: []string{"step Z"}})
	wantRefusal(t, err, `has no "step Z" in acceptance`)
	// An item an open child claims cannot go until the child lets go.
	_, err = h.rules().Update(rel.ID, "vp", UpdateInput{RemoveAcceptance: []string{"step B"}})
	wantRefusal(t, err, `"step B" is claimed by open #3`)
	h.update(child.ID, "alice", UpdateInput{RemoveSatisfies: []string{"step B"}})
	ch = h.update(rel.ID, "vp", UpdateInput{RemoveAcceptance: []string{"step B"}})
	if strings.Join(ch.Assignment.Acceptance, "|") != "step A|step C" {
		t.Fatalf("after remove = %v", ch.Assignment.Acceptance)
	}
	// Removing the last name clears the list; the log records the
	// empty list.
	ch = h.update(child.ID, "alice", UpdateInput{RemoveSatisfies: []string{"step A"}})
	if len(ch.Assignment.Satisfies) != 0 || ch.Entries[0].Op != OpSatisfies || len(ch.Entries[0].Items) != 0 {
		t.Fatalf("clear = %+v", ch.Entries)
	}
	ch = h.update(rel.ID, "vp", UpdateInput{RemoveAcceptance: []string{"step A", "step C"}})
	if len(ch.Assignment.Acceptance) != 0 || len(ch.Entries[0].Items) != 0 {
		t.Fatalf("clear = %v", ch.Assignment.Acceptance)
	}
	// Nothing to remove is nothing to change.
	_, err = h.rules().Update(rel.ID, "vp", UpdateInput{RemoveAcceptance: []string{}})
	wantRefusal(t, err, "nothing to change")

	// Moving a child that satisfies something: the names must fit the
	// new parent, or come off in the same call.
	other := h.create("cos", CreateInput{Title: "Other release", Assignee: "vp", Acceptance: []string{"step A"}}).Assignment
	h.update(rel.ID, "vp", UpdateInput{AddAcceptance: []string{"step A"}})
	h.update(child.ID, "alice", UpdateInput{AddSatisfies: []string{"step A"}})
	top := 0
	_, err = h.rules().Update(child.ID, "vp", UpdateInput{Parent: &top})
	wantRefusal(t, err, "remove them with remove_satisfies in the same call")
	third := h.create("cos", CreateInput{Title: "Third", Assignee: "vp", Acceptance: []string{"something else"}}).Assignment
	_, err = h.rules().Update(child.ID, "vp", UpdateInput{Parent: &third.ID})
	wantRefusal(t, err, `which #5 does not declare (its Done-when conditions: "something else")`)
	// Swapping the claim in the same call as the move is checked
	// against the new parent.
	_, err = h.rules().Update(child.ID, "vp", UpdateInput{Parent: &third.ID, RemoveSatisfies: []string{"step A"}, AddSatisfies: []string{"nope"}})
	wantRefusal(t, err, `#5's Done-when conditions are "something else"`)
	// Same name under the new parent: it moves and keeps the claim.
	h.update(child.ID, "vp", UpdateInput{Parent: &other.ID})
	if p, _ := h.set.Progress(other.ID); p.Claimed != 1 {
		t.Fatalf("other after move = %s", p)
	}
	// Moving to top level with the name removed in the same call.
	h.update(child.ID, "vp", UpdateInput{Parent: &top, RemoveSatisfies: []string{"step A"}})
	c, _ := h.set.Get(child.ID)
	if c.Parent != 0 || len(c.Satisfies) != 0 {
		t.Fatalf("child after move = %+v", c)
	}
}

// The nudge: raised on the first child under an assignment with no items,
// carried in the wake note's assignments section, never raised again, and
// cleared by the arrival of items.
func TestNudgeOnFirstChildOnly(t *testing.T) {
	h := newHarness(t, "cos", "vp", "alice")
	epic := h.create("cos", CreateInput{Title: "Epic", Assignee: "vp"}).Assignment
	ch := h.create("vp", CreateInput{Title: "Part 1", Assignee: "alice", Parent: epic.ID})
	if len(ch.Touched) != 1 || ch.Touched[0].ID != epic.ID || !ch.Touched[0].Nudge {
		t.Fatalf("first child touched %+v", ch.Touched)
	}
	if got := h.set.Nudges("vp"); len(got) != 1 || got[0] != epic.ID {
		t.Fatalf("nudges = %v", got)
	}
	if got := h.set.OpenAssignments("vp"); !strings.HasSuffix(got, "\n\n#1 now has parts and no Done-when conditions.") {
		t.Fatalf("wake note section = %q", got)
	}
	// The nudge is the assignee's, not the creator's.
	if got := h.set.Nudges("cos"); len(got) != 0 {
		t.Fatalf("creator nudged: %v", got)
	}
	// A second child raises nothing: the parent already has children.
	ch = h.create("vp", CreateInput{Title: "Part 2", Assignee: "alice", Parent: epic.ID})
	if len(ch.Touched) != 0 {
		t.Fatalf("second child touched %+v", ch.Touched)
	}
	// Once told, never again: the wake note's commit clears the flag
	// (store.ClearAssignmentNudges); here the set is rebuilt with it cleared.
	e, _ := h.set.Get(epic.ID)
	cleared := e.clone()
	cleared.Nudge = false
	h.set = h.set.with(cleared)
	if got := h.set.OpenAssignments("vp"); strings.Contains(got, "no acceptance items") {
		t.Fatalf("nudge repeated: %q", got)
	}
	// A parent filed with items never nudges; one that gains items
	// after the flag was raised has it cleared by the change.
	rel := h.create("cos", CreateInput{Title: "Release", Assignee: "vp", Acceptance: []string{"a"}}).Assignment
	ch = h.create("vp", CreateInput{Title: "A", Assignee: "alice", Parent: rel.ID, Satisfies: []string{"a"}})
	if len(ch.Touched) != 0 {
		t.Fatalf("parent with items touched %+v", ch.Touched)
	}
	late := h.create("cos", CreateInput{Title: "Late", Assignee: "vp"}).Assignment
	h.create("vp", CreateInput{Title: "Under late", Assignee: "alice", Parent: late.ID})
	if got := h.set.Nudges("vp"); len(got) != 1 || got[0] != late.ID {
		t.Fatalf("nudges = %v", got)
	}
	ch = h.update(late.ID, "vp", UpdateInput{AddAcceptance: []string{"x"}})
	if ch.Assignment.Nudge || len(h.set.Nudges("vp")) != 0 {
		t.Fatalf("items did not clear the nudge: %+v", ch.Assignment)
	}
	// Moving an existing assignment under a childless, itemless parent is
	// its first child too.
	solo := h.create("cos", CreateInput{Title: "Solo", Assignee: "vp"}).Assignment
	loose := h.create("cos", CreateInput{Title: "Loose", Assignee: "alice"}).Assignment
	ch = h.update(loose.ID, "cos", UpdateInput{Parent: &solo.ID})
	if len(ch.Touched) != 1 || ch.Touched[0].ID != solo.ID {
		t.Fatalf("move touched %+v", ch.Touched)
	}
}

// Assignments with neither field behave exactly as before: child counts on
// the row, no warning on close, no table.
func TestAssignmentsWithoutItemsAreCountedByChildren(t *testing.T) {
	h := newHarness(t, "cos", "alice")
	epic := h.create("cos", CreateInput{Title: "Epic", Assignee: "cos"}).Assignment
	part := h.create("cos", CreateInput{Title: "Part", Assignee: "alice", Parent: epic.ID}).Assignment
	e, _ := h.set.Get(epic.ID)
	if line := h.set.Line(e); !strings.Contains(line, "1 part (1 open)") || strings.Contains(line, "acceptance") {
		t.Errorf("line = %s", line)
	}
	if items := h.set.Items(epic.ID); items != nil {
		t.Errorf("items = %v", items)
	}
	if _, ok := h.set.Progress(epic.ID); ok {
		t.Error("progress reported without items")
	}
	h.close(part.ID, "alice", ResolutionDone, "ok")
	ch := h.close(epic.ID, "cos", ResolutionDone, "ok")
	if len(ch.Unmet) != 0 || len(ch.Assignment.Log[len(ch.Assignment.Log)-1].Unmet) != 0 {
		t.Fatalf("warning without items: %v", ch.Unmet)
	}
}
