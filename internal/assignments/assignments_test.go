package assignments

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)

func active(slugs ...string) func(string) bool {
	set := map[string]bool{}
	for _, s := range slugs {
		set[s] = true
	}
	return func(s string) bool { return set[s] }
}

// harness applies changes in sequence, threading the Set through, so
// a test reads as a story.
type harness struct {
	t   *testing.T
	set *Set
	act func(string) bool
	now time.Time
}

func newHarness(t *testing.T, slugs ...string) *harness {
	return &harness{t: t, set: Build(nil), act: active(slugs...), now: t0}
}

func (h *harness) rules() Rules {
	h.now = h.now.Add(time.Minute)
	return Rules{Set: h.set, Active: h.act, Now: h.now}
}

func (h *harness) create(by string, in CreateInput) Change {
	h.t.Helper()
	ch, err := h.rules().Create(by, in)
	if err != nil {
		h.t.Fatalf("create by %s: %v", by, err)
	}
	h.set = ch.Set
	return ch
}

func (h *harness) update(id int, by string, in UpdateInput) Change {
	h.t.Helper()
	ch, err := h.rules().Update(id, by, in)
	if err != nil {
		h.t.Fatalf("update #%d by %s: %v", id, by, err)
	}
	h.set = ch.Set
	return ch
}

func (h *harness) close(id int, by string, res Resolution, outcome string) Change {
	h.t.Helper()
	ch, err := h.rules().Close(id, by, res, outcome)
	if err != nil {
		h.t.Fatalf("close #%d by %s: %v", id, by, err)
	}
	h.set = ch.Set
	return ch
}

func (h *harness) reopen(id int, by, note string) Change {
	h.t.Helper()
	ch, err := h.rules().Reopen(id, by, note)
	if err != nil {
		h.t.Fatalf("reopen #%d by %s: %v", id, by, err)
	}
	h.set = ch.Set
	return ch
}

func wantRefusal(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a refusal containing %q, got nil", contains)
	}
	if !IsRefusal(err) {
		t.Fatalf("expected a refusal, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("refusal %q does not contain %q", err.Error(), contains)
	}
}

func wakesTo(ws []Wake) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.To)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	h := newHarness(t, "cos", "alice")
	ch := h.create("cos", CreateInput{Title: "v3 release prep", Body: "Do the thing.\n\nThen the other thing.", Assignee: "alice", BlockedBy: nil})
	data, err := Marshal(ch.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("parse:\n%s\n%v", data, err)
	}
	again, err := Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Fatalf("round trip drifted:\n%s\n---\n%s", data, again)
	}
	if back.Body != ch.Assignment.Body {
		t.Fatalf("body drifted: %q vs %q", back.Body, ch.Assignment.Body)
	}
	// The log renders one flow-style line per entry so the file reads
	// as a table.
	if !strings.Contains(string(data), "- {seq: 1, ts: ") {
		t.Fatalf("log entries are not flow style:\n%s", data)
	}
	if !strings.Contains(string(data), "op: created, to: alice}") {
		t.Fatalf("created entry missing the assignee:\n%s", data)
	}
}

func TestParseRefusesUnknownKeysAndInvalidRecords(t *testing.T) {
	good := "---\nid: 1\ntitle: t\nstatus: open\nassignee: a\ncreator: a\ncreated: 2026-09-21T18:00:00Z\nupdated: 2026-09-21T18:00:00Z\nlog:\n  - {seq: 1, ts: 2026-09-21T18:00:00Z, by: a, op: created, to: a}\n---\n\nbody\n"
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("good record refused: %v", err)
	}
	cases := map[string]string{
		"unknown key":      strings.Replace(good, "creator: a\n", "creator: a\npriority: 1\n", 1),
		"closed no reason": strings.Replace(good, "status: open", "status: closed", 1),
		"bad status":       strings.Replace(good, "status: open", "status: blocked", 1),
		"self parent":      strings.Replace(good, "creator: a\n", "creator: a\nparent: 1\n", 1),
		"no log":           strings.Replace(good, "log:\n  - {seq: 1, ts: 2026-09-21T18:00:00Z, by: a, op: created, to: a}\n", "log: []\n", 1),
		"no front matter":  "just a body",
	}
	for name, data := range cases {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCreateWakesAssigneeNotSelf(t *testing.T) {
	h := newHarness(t, "cos", "alice")
	ch := h.create("cos", CreateInput{Title: "Own work", Body: "mine"})
	if ch.Assignment.Assignee != "cos" {
		t.Fatalf("assignee defaults to creator, got %s", ch.Assignment.Assignee)
	}
	if len(ch.Wakes) != 0 {
		t.Fatalf("self-assigned assignment wakes nobody, got %v", wakesTo(ch.Wakes))
	}
	ch = h.create("cos", CreateInput{Title: "Alice's work", Body: "spec", Assignee: "alice"})
	if got := wakesTo(ch.Wakes); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("wakes = %v", got)
	}
	w := ch.Wakes[0]
	if w.Title != "#2 assigned to you: Alice's work" {
		t.Errorf("title = %q", w.Title)
	}
	for _, want := range []string{"cos assigned you #2 \"Alice's work\".", "It is ready: nothing blocks it.", "spec"} {
		if !strings.Contains(w.Body, want) {
			t.Errorf("body missing %q:\n%s", want, w.Body)
		}
	}
}

func TestCreateRefusals(t *testing.T) {
	h := newHarness(t, "cos", "alice")
	r := h.rules()
	_, err := r.Create("ghost", CreateInput{Title: "x"})
	wantRefusal(t, err, "not an active agent")
	_, err = r.Create("cos", CreateInput{Title: "x", Assignee: "ghost"})
	wantRefusal(t, err, "assignee")
	_, err = r.Create("cos", CreateInput{Title: "  "})
	wantRefusal(t, err, "title is empty")
	_, err = r.Create("cos", CreateInput{Title: "x", Parent: 9})
	wantRefusal(t, err, "no assignment #9")
	_, err = r.Create("cos", CreateInput{Title: "x", BlockedBy: []int{9}})
	wantRefusal(t, err, "no assignment #9")
	_, err = r.Create("cos", CreateInput{Title: "x", Body: strings.Repeat("a", MaxBodyBytes+1)})
	wantRefusal(t, err, "cap is")
	// The CEO is always an actor and always a valid assignee.
	if _, err := r.Create("ceo", CreateInput{Title: "x", Assignee: "ceo"}); err != nil {
		t.Fatalf("ceo create: %v", err)
	}
}

func TestChildrenBlockParentAndQuestionsAreChildren(t *testing.T) {
	h := newHarness(t, "cos", "alice", "bob")
	epic := h.create("cos", CreateInput{Title: "Epic", Body: "big", Assignee: "alice"})
	if !h.set.Ready(epic.Assignment.ID) {
		t.Fatal("a childless epic is ready")
	}
	// Alice, the assignee, decomposes: a child for bob.
	child := h.create("alice", CreateInput{Title: "Part 1", Body: "p1", Assignee: "bob", Parent: epic.Assignment.ID})
	if got := wakesTo(child.Wakes); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("child wakes = %v", got)
	}
	if h.set.Ready(epic.Assignment.ID) {
		t.Fatal("an open child blocks its parent")
	}
	if got := h.set.State(epic.Assignment.ID); got != "blocked by #2" {
		t.Fatalf("state = %q", got)
	}
	// Bob cannot file under the epic: not its creator or assignee.
	_, err := h.rules().Create("bob", CreateInput{Title: "Sneaky", Parent: epic.Assignment.ID})
	wantRefusal(t, err, "only #1's creator")
	// Bob can file a question under HIS assignment, assigned to alice.
	q := h.create("bob", CreateInput{Title: "Which gain?", Body: "1 or 2?", Assignee: "alice", Parent: child.Assignment.ID})
	if !h.set.Blocked(child.Assignment.ID) {
		t.Fatal("a question child blocks the asker's assignment")
	}
	// Alice answers: closing the question makes bob's assignment ready and
	// wakes him with the outcome.
	ans := h.close(q.Assignment.ID, "alice", ResolutionDone, "Gain 2.")
	if got := wakesTo(ans.Wakes); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("answer wakes = %v", got)
	}
	body := ans.Wakes[0].Body
	for _, want := range []string{"alice closed #3 \"Which gain?\" as done.", "Outcome: Gain 2.", "#2 \"Part 1\" is now ready"} {
		if !strings.Contains(body, want) {
			t.Errorf("answer body missing %q:\n%s", want, body)
		}
	}
	// Bob is both the creator of the question and the assignee of the
	// flipped assignment: one wake, not two.
	if ans.Wakes[0].Title != "#3 closed (done): Which gain?" {
		t.Errorf("title = %q", ans.Wakes[0].Title)
	}
	// Closing the epic while a child is open is refused.
	_, err = h.rules().Close(epic.Assignment.ID, "alice", ResolutionDone, "all good")
	wantRefusal(t, err, "open parts #2")
	// Bob finishes; the epic becomes ready and alice is woken to roll up.
	fin := h.close(child.Assignment.ID, "bob", ResolutionDone, "Part 1 shipped at /files/x.")
	tos := wakesTo(fin.Wakes)
	if len(tos) != 1 || tos[0] != "alice" {
		t.Fatalf("finish wakes = %v", tos)
	}
	if !strings.Contains(fin.Wakes[0].Body, "#1 \"Epic\" is now ready: bob closed #2 \"Part 1\" as done.") {
		t.Errorf("rollup line missing:\n%s", fin.Wakes[0].Body)
	}
	// The creator (cos) is not woken for the child's close: cos did not
	// create #2, alice did.
	roll := h.close(epic.Assignment.ID, "alice", ResolutionDone, "Epic done.")
	if got := wakesTo(roll.Wakes); len(got) != 1 || got[0] != "cos" {
		t.Fatalf("rollup close wakes = %v", got)
	}
}

func TestBlockedByCrossTreeAndCycles(t *testing.T) {
	h := newHarness(t, "cos", "alice", "bob")
	a := h.create("cos", CreateInput{Title: "A", Assignee: "alice"})
	b := h.create("cos", CreateInput{Title: "B", Assignee: "bob"})
	// Alice, as assignee, declares A waits on B.
	ch := h.update(a.Assignment.ID, "alice", UpdateInput{AddBlockedBy: []int{b.Assignment.ID}})
	if len(ch.Wakes) != 0 {
		t.Fatalf("adding a blocker wakes nobody, got %v", wakesTo(ch.Wakes))
	}
	if got := h.set.State(a.Assignment.ID); got != "blocked by #2" {
		t.Fatalf("state = %q", got)
	}
	if got := h.set.Blocks(b.Assignment.ID); len(got) != 1 || got[0] != a.Assignment.ID {
		t.Fatalf("B blocks = %v", got)
	}
	// A cycle through blocked_by is refused.
	_, err := h.rules().Update(b.Assignment.ID, "bob", UpdateInput{AddBlockedBy: []int{a.Assignment.ID}})
	wantRefusal(t, err, "cycle")
	// A cycle through parent + blocked_by is refused: A under B (so B
	// waits on its child A) while A waits on B.
	_, err = h.rules().Update(a.Assignment.ID, "cos", UpdateInput{Parent: intp(b.Assignment.ID)})
	wantRefusal(t, err, "cycle")
	// And the mirror at create time: a child that waits on its own
	// parent.
	_, err = h.rules().Create("cos", CreateInput{Title: "Loop", Parent: a.Assignment.ID, BlockedBy: []int{a.Assignment.ID}})
	wantRefusal(t, err, "cycle")
	// Bob (not creator/assignee of A) cannot touch A's blockers.
	_, err = h.rules().Update(a.Assignment.ID, "bob", UpdateInput{RemoveBlockedBy: []int{b.Assignment.ID}})
	wantRefusal(t, err, "only #1's creator")
	// Closing B as dropped still frees A, and the wake says dropped.
	ch = h.close(b.Assignment.ID, "bob", ResolutionDropped, "Not needed after all.")
	// cos created B and alice holds the freed A: two wakes.
	tos := wakesTo(ch.Wakes)
	if len(tos) != 2 || tos[0] != "alice" || tos[1] != "cos" {
		t.Fatalf("wakes = %v", tos)
	}
	if !strings.Contains(ch.Wakes[0].Body, "#1 \"A\" is now ready: bob dropped #2 \"B\".") || !strings.Contains(ch.Wakes[0].Body, "Reason of #2: Not needed after all.") {
		t.Errorf("alice's wake:\n%s", ch.Wakes[0].Body)
	}
	if ch.Wakes[0].Title != "#1 ready: A" {
		t.Errorf("alice's title = %q", ch.Wakes[0].Title)
	}
	if !strings.Contains(ch.Wakes[1].Body, "bob dropped #2 \"B\".") || strings.Contains(ch.Wakes[1].Body, "Stop work") {
		t.Errorf("cos's wake:\n%s", ch.Wakes[1].Body)
	}
	// Removing the last blocker by the creator wakes the assignee.
	c := h.create("cos", CreateInput{Title: "C", Assignee: "alice", BlockedBy: []int{a.Assignment.ID}})
	if h.set.Ready(c.Assignment.ID) {
		t.Fatal("C waits on A")
	}
	ch = h.update(c.Assignment.ID, "cos", UpdateInput{RemoveBlockedBy: []int{a.Assignment.ID}})
	if got := wakesTo(ch.Wakes); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("unblock wakes = %v", got)
	}
	if !strings.Contains(ch.Wakes[0].Body, "#3 \"C\" is now ready: cos changed what #3 waits on.") {
		t.Errorf("unblock body:\n%s", ch.Wakes[0].Body)
	}
}

func intp(i int) *int { return &i }

func strp(s string) *string { return &s }

func TestAmendAndReassignRules(t *testing.T) {
	h := newHarness(t, "cos", "alice", "bob")
	a := h.create("cos", CreateInput{Title: "A", Body: "v1", Assignee: "alice"})
	// The assignee cannot amend the spec.
	_, err := h.rules().Update(a.Assignment.ID, "alice", UpdateInput{Body: strp("v2")})
	wantRefusal(t, err, "only #1's creator")
	// The creator must say why when the assignee is someone else.
	_, err = h.rules().Update(a.Assignment.ID, "cos", UpdateInput{Body: strp("v2")})
	wantRefusal(t, err, "note is required")
	ch := h.update(a.Assignment.ID, "cos", UpdateInput{Body: strp("v2"), Note: "gain changed"})
	if ch.PriorText != "A\n\nv1" {
		t.Fatalf("prior text = %q", ch.PriorText)
	}
	if e := ch.Entries[0]; e.Op != OpAmended || e.Fields != "description" || e.Prior == "" {
		t.Fatalf("entry = %+v", e)
	}
	if got := wakesTo(ch.Wakes); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("amend wakes = %v", got)
	}
	if !strings.Contains(ch.Wakes[0].Body, "cos amended #1 \"A\" (description): gain changed") || !strings.Contains(ch.Wakes[0].Body, "\nv2") {
		t.Errorf("amend body:\n%s", ch.Wakes[0].Body)
	}
	// No-op edits are refused rather than logged.
	_, err = h.rules().Update(a.Assignment.ID, "cos", UpdateInput{Body: strp("v2"), Note: "same"})
	wantRefusal(t, err, "nothing to change")
	// Self-assigned amend needs no note.
	mine := h.create("cos", CreateInput{Title: "Mine", Body: "x"})
	h.update(mine.Assignment.ID, "cos", UpdateInput{Title: strp("Mine, renamed")})
	// The assignee cannot reassign.
	_, err = h.rules().Update(a.Assignment.ID, "alice", UpdateInput{Assignee: strp("bob"), Note: "not me"})
	wantRefusal(t, err, "does not hand an assignment on")
	// Reassign needs a note and wakes both.
	_, err = h.rules().Update(a.Assignment.ID, "cos", UpdateInput{Assignee: strp("bob")})
	wantRefusal(t, err, "note is required")
	ch = h.update(a.Assignment.ID, "cos", UpdateInput{Assignee: strp("bob"), Note: "bob owns v3 now"})
	tos := wakesTo(ch.Wakes)
	if len(tos) != 2 || tos[0] != "alice" || tos[1] != "bob" {
		t.Fatalf("reassign wakes = %v", tos)
	}
	if !strings.Contains(ch.Wakes[0].Body, "from you to bob: bob owns v3 now\nStop work on it.") {
		t.Errorf("old assignee body:\n%s", ch.Wakes[0].Body)
	}
	if !strings.Contains(ch.Wakes[1].Body, "to you (from alice): bob owns v3 now") || !strings.Contains(ch.Wakes[1].Body, "\nv2") {
		t.Errorf("new assignee body:\n%s", ch.Wakes[1].Body)
	}
	// Reassigning to the current assignee is a refusal.
	_, err = h.rules().Update(a.Assignment.ID, "cos", UpdateInput{Assignee: strp("bob"), Note: "again"})
	wantRefusal(t, err, "already assigned")
}

func TestCloseAndReopenRules(t *testing.T) {
	h := newHarness(t, "cos", "alice", "bob")
	a := h.create("cos", CreateInput{Title: "A", Body: "spec", Assignee: "alice"})
	// Only the assignee (or CEO) closes as done; the creator can drop.
	_, err := h.rules().Close(a.Assignment.ID, "cos", ResolutionDone, "I did it for her")
	wantRefusal(t, err, "only #1's assignee")
	_, err = h.rules().Close(a.Assignment.ID, "bob", ResolutionDropped, "meh")
	wantRefusal(t, err, "only #1's creator")
	_, err = h.rules().Close(a.Assignment.ID, "alice", ResolutionDone, "  ")
	wantRefusal(t, err, "outcome is empty")
	_, err = h.rules().Close(a.Assignment.ID, "alice", Resolution("wontfix"), "x")
	wantRefusal(t, err, "not done or dropped")
	// Creator drops: the assignee is told to stop, with the reason.
	ch := h.close(a.Assignment.ID, "cos", ResolutionDropped, "Overtaken by events.")
	if got := wakesTo(ch.Wakes); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("drop wakes = %v", got)
	}
	if !strings.Contains(ch.Wakes[0].Body, "cos dropped #1 \"A\". Stop work on it.\n\nReason: Overtaken by events.") {
		t.Errorf("drop body:\n%s", ch.Wakes[0].Body)
	}
	if ch.Wakes[0].Title != "#1 dropped: A" {
		t.Errorf("drop title = %q", ch.Wakes[0].Title)
	}
	// Closed takes no edit but reopen.
	_, err = h.rules().Update(a.Assignment.ID, "cos", UpdateInput{Body: strp("v2"), Note: "n"})
	wantRefusal(t, err, "closed (dropped)")
	_, err = h.rules().Close(a.Assignment.ID, "alice", ResolutionDone, "x")
	wantRefusal(t, err, "already closed")
	// Only the creator or CEO reopens, with a note.
	_, err = h.rules().Reopen(a.Assignment.ID, "alice", "please")
	wantRefusal(t, err, "only #1's creator")
	_, err = h.rules().Reopen(a.Assignment.ID, "cos", "")
	wantRefusal(t, err, "note is empty")
	ch = h.reopen(a.Assignment.ID, "ceo", "Events un-overtook.")
	if got := wakesTo(ch.Wakes); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("reopen wakes = %v", got)
	}
	if !strings.Contains(ch.Wakes[0].Body, "ceo reopened #1 \"A\": Events un-overtook.\nIts earlier outcome was: Overtaken by events.") {
		t.Errorf("reopen body:\n%s", ch.Wakes[0].Body)
	}
	got, _ := h.set.Get(a.Assignment.ID)
	if !got.Open() || got.Outcome != "" || got.Closed != nil || got.Resolution != "" {
		t.Fatalf("reopened record still carries a closing: %+v", got)
	}
	// The closing is still on the record, in the log.
	var sawOutcome bool
	for _, e := range got.Log {
		if e.Op == OpClosed && e.Note == "Overtaken by events." && e.Resolution == ResolutionDropped {
			sawOutcome = true
		}
	}
	if !sawOutcome {
		t.Fatal("closed entry lost its outcome")
	}
	if err := Validate(got); err != nil {
		t.Fatal(err)
	}
	// A child under a closed parent cannot be reopened first.
	p := h.create("cos", CreateInput{Title: "P", Assignee: "alice"})
	c := h.create("cos", CreateInput{Title: "C", Assignee: "alice", Parent: p.Assignment.ID})
	h.close(c.Assignment.ID, "alice", ResolutionDone, "done c")
	h.close(p.Assignment.ID, "alice", ResolutionDone, "done p")
	_, err = h.rules().Reopen(c.Assignment.ID, "cos", "again")
	wantRefusal(t, err, "reopen #2 first")
}

func TestCEOAsAssigneeAndActor(t *testing.T) {
	h := newHarness(t, "cos", "alice")
	// A question to the CEO: filed under alice's assignment, assigned to ceo.
	a := h.create("cos", CreateInput{Title: "A", Assignee: "alice"})
	q := h.create("alice", CreateInput{Title: "Budget?", Body: "How much?", Assignee: "ceo", Parent: a.Assignment.ID})
	if got := wakesTo(q.Wakes); len(got) != 1 || got[0] != "ceo" {
		t.Fatalf("question wakes = %v", got)
	}
	// The CEO answers by closing it; alice is woken with the answer and
	// the news that #1 is ready again.
	ch := h.close(q.Assignment.ID, "ceo", ResolutionDone, "$40k.")
	if got := wakesTo(ch.Wakes); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("answer wakes = %v", got)
	}
	if !strings.Contains(ch.Wakes[0].Body, "Outcome: $40k.") || !strings.Contains(ch.Wakes[0].Body, "#1 \"A\" is now ready") {
		t.Errorf("answer body:\n%s", ch.Wakes[0].Body)
	}
	// The CEO can do everything: amend, reassign, close as done.
	h.update(a.Assignment.ID, "ceo", UpdateInput{Title: strp("A, sharpened"), Note: "clearer"})
	h.close(a.Assignment.ID, "ceo", ResolutionDone, "Fine.")
}

func TestQueryLineAndOpenAssignments(t *testing.T) {
	h := newHarness(t, "cos", "alice", "bob")
	epic := h.create("cos", CreateInput{Title: "Epic", Assignee: "alice"})
	h.create("alice", CreateInput{Title: "Part", Assignee: "bob", Parent: epic.Assignment.ID})
	h.create("cos", CreateInput{Title: "Solo", Assignee: "alice"})
	done := h.create("cos", CreateInput{Title: "Done", Assignee: "alice"})
	h.close(done.Assignment.ID, "alice", ResolutionDone, "ok")

	if got := h.set.Query(Filter{Assignee: "alice"}); len(got) != 2 {
		t.Fatalf("alice open = %d", len(got))
	}
	if got := h.set.Query(Filter{Assignee: "alice", Status: "all"}); len(got) != 3 {
		t.Fatalf("alice all = %d", len(got))
	}
	if got := h.set.Query(Filter{Parent: epic.Assignment.ID}); len(got) != 1 || got[0].Title != "Part" {
		t.Fatalf("children = %v", got)
	}
	if got := h.set.Query(Filter{Parent: -1}); len(got) != 2 {
		t.Fatalf("top level open = %d", len(got))
	}
	ready := true
	if got := h.set.Query(Filter{Ready: &ready}); len(got) != 2 {
		t.Fatalf("ready = %d", len(got))
	}
	e, _ := h.set.Get(epic.Assignment.ID)
	if line := h.set.Line(e); line != `#1 blocked by #2 "Epic" — assignee alice, creator cos, 1 part (1 open)` {
		t.Errorf("line = %s", line)
	}
	p, _ := h.set.Get(2)
	if line := h.set.Line(p); line != `#2 ready "Part" — assignee bob, creator alice, part of #1; blocks #1` {
		t.Errorf("line = %s", line)
	}
	// The epic gained a child and declares no acceptance items, so
	// alice's section ends with the one-time nudge.
	if got := h.set.OpenAssignments("alice"); got != "Your open assignments:\n- #1 \"Epic\" (blocked by #2)\n- #3 \"Solo\" (ready)\n\n#1 now has parts and no Done-when conditions." {
		t.Errorf("open assignments = %q", got)
	}
	if got := h.set.OpenAssignments("nobody"); got != "" {
		t.Errorf("empty section = %q, want nothing: the section rides every wake note and an agent holding nothing gets none", got)
	}
}

// Every free-text field is stored with LF endings and no trailing
// newline, whatever the transport sent: a browser posts CRLF, and a
// record that kept it would differ from its own form on the next edit.
func TestTextIsNormalisedOnEveryWrite(t *testing.T) {
	h := newHarness(t, "cos", "alice")
	ch := h.create("cos", CreateInput{Title: "Epic", Body: "line one\r\nline two\r\n", Assignee: "alice"})
	if ch.Assignment.Body != "line one\nline two" {
		t.Fatalf("created body = %q", ch.Assignment.Body)
	}
	body := "line one\r\nline two"
	if _, err := h.rules().Update(ch.Assignment.ID, "cos", UpdateInput{Body: &body}); !IsRefusal(err) || !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("an update posting the same text with CRLF endings should be no change, got %v", err)
	}
	closed := h.close(ch.Assignment.ID, "alice", ResolutionDone, "done\r\nsee /files/artifacts/shared/alice/out.md\r\n")
	if closed.Assignment.Outcome != "done\nsee /files/artifacts/shared/alice/out.md" {
		t.Fatalf("outcome = %q", closed.Assignment.Outcome)
	}
	re := h.reopen(ch.Assignment.ID, "cos", "not\r\nquite")
	if re.Entries[0].Note != "not\nquite" {
		t.Fatalf("reopen note = %q", re.Entries[0].Note)
	}
}

// An offboard hands the departing agent's open asks to their manager
// as creator; a closed assignment keeps its creator, and only the CEO does
// this at all.
func TestTransferCreator(t *testing.T) {
	h := newHarness(t, "cos", "alice", "bob")
	open := h.create("alice", CreateInput{Title: "Ask", Assignee: "bob"})
	done := h.create("alice", CreateInput{Title: "Answered", Assignee: "bob"})
	h.close(done.Assignment.ID, "bob", ResolutionDone, "ok")

	if _, err := h.rules().TransferCreator(open.Assignment.ID, "cos", "cos", "alice left"); !IsRefusal(err) {
		t.Fatalf("a manager cannot move creatorship: %v", err)
	}
	if _, err := h.rules().TransferCreator(done.Assignment.ID, CEO, "cos", "alice left"); !IsRefusal(err) {
		t.Fatalf("a closed assignment keeps its creator: %v", err)
	}
	ch, err := h.rules().TransferCreator(open.Assignment.ID, CEO, "cos", "alice left")
	if err != nil {
		t.Fatal(err)
	}
	h.set = ch.Set
	if ch.Assignment.Creator != "cos" {
		t.Fatalf("creator = %s", ch.Assignment.Creator)
	}
	if e := ch.Entries[0]; e.Op != OpCreator || e.From != "alice" || e.To != "cos" || e.Note != "alice left" {
		t.Fatalf("entry = %+v", e)
	}
	if len(ch.Wakes) != 1 || ch.Wakes[0].To != "cos" || ch.Wakes[0].Op != OpCreator || !strings.Contains(ch.Wakes[0].Body, "made you the creator of #1 \"Ask\" (opened by alice): alice left") {
		t.Fatalf("wakes = %+v", ch.Wakes)
	}
	// The new creator holds the creator's rights: amend with a note.
	title := "Ask, clarified"
	if _, err := h.rules().Update(open.Assignment.ID, "cos", UpdateInput{Title: &title, Note: "clearer"}); err != nil {
		t.Fatalf("new creator cannot amend: %v", err)
	}
	// And the record round-trips with the new op.
	b, err := Marshal(ch.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b); err != nil {
		t.Fatalf("parse after creator transfer: %v", err)
	}
}

func TestSeqAndIDsAreGlobalAndMonotonic(t *testing.T) {
	h := newHarness(t, "cos", "alice")
	a := h.create("cos", CreateInput{Title: "A", Assignee: "alice"})
	b := h.create("cos", CreateInput{Title: "B", Assignee: "alice"})
	if a.Assignment.ID != 1 || b.Assignment.ID != 2 {
		t.Fatalf("ids = %d, %d", a.Assignment.ID, b.Assignment.ID)
	}
	if a.Entries[0].Seq != 1 || b.Entries[0].Seq != 2 {
		t.Fatalf("seqs = %d, %d", a.Entries[0].Seq, b.Entries[0].Seq)
	}
	ch := h.update(a.Assignment.ID, "cos", UpdateInput{AddBlockedBy: []int{b.Assignment.ID}, Title: strp("A2"), Note: "n"})
	if len(ch.Entries) != 2 || ch.Entries[0].Seq != 3 || ch.Entries[1].Seq != 4 {
		t.Fatalf("entries = %+v", ch.Entries)
	}
	if h.set.MaxSeq() != 4 {
		t.Fatalf("max seq = %d", h.set.MaxSeq())
	}
}
