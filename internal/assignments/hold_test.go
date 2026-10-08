package assignments

import (
	"strings"
	"testing"
)

func wakeTo(t *testing.T, ws []Wake, to string) Wake {
	t.Helper()
	for _, w := range ws {
		if w.To == to {
			return w
		}
	}
	t.Fatalf("no wake to %s in %v", to, wakesTo(ws))
	return Wake{}
}

// A hold on an epic pauses the epic and everything under it: every
// assignee under it is woken once to stop, nothing under it is ready,
// and a resume wakes the same agents to carry on. An assignment in another
// tree that waits on the epic was waiting already and hears nothing.
func TestHoldPausesTheSubtreeAndResumeWakesIt(t *testing.T) {
	h := newHarness(t, "cos", "vp", "eng1", "eng2")
	h.create("cos", CreateInput{Title: "epic", Assignee: "vp"})                             // #1
	h.create("vp", CreateInput{Title: "part a", Assignee: "eng1", Parent: 1})               // #2
	h.create("vp", CreateInput{Title: "part b", Assignee: "eng2", Parent: 1})               // #3
	h.create("eng2", CreateInput{Title: "sub b", Assignee: "eng2", Parent: 3})              // #4
	h.create("cos", CreateInput{Title: "elsewhere", Assignee: "eng1", BlockedBy: []int{1}}) // #5

	hold := true
	ch := h.update(1, "cos", UpdateInput{Hold: &hold, Note: "rethinking scope"})
	if got := strings.Join(wakesTo(ch.Wakes), ","); got != "eng1,eng2,vp" {
		t.Fatalf("hold woke %s", got)
	}
	if e := ch.Entries; len(e) != 1 || e[0].Op != OpHeld || e[0].Note != "rethinking scope" {
		t.Fatalf("hold entries = %+v", e)
	}
	vp := wakeTo(t, ch.Wakes, "vp")
	if vp.Op != OpHeld || vp.Title != `#1 on hold: epic` || !strings.Contains(vp.Body, `cos put #1 "epic" on hold: rethinking scope`) || !strings.Contains(vp.Body, `Stop work on #1 "epic" until it is resumed`) {
		t.Fatalf("vp wake = %+v", vp)
	}
	eng2 := wakeTo(t, ch.Wakes, "eng2")
	if !strings.Contains(eng2.Body, `Stop work on #3 "part b" (part of it), #4 "sub b" (part of it) until it is resumed`) {
		t.Fatalf("eng2 wake = %+v", eng2)
	}
	eng1 := wakeTo(t, ch.Wakes, "eng1")
	if !strings.Contains(eng1.Body, `Stop work on #2 "part a" (part of it) until`) || strings.Contains(eng1.Body, "#5") {
		t.Fatalf("eng1 wake = %+v", eng1)
	}
	for id, want := range map[int]string{1: "on hold", 2: "on hold under #1", 3: "on hold under #1", 4: "on hold under #1", 5: "blocked by #1"} {
		if got := h.set.State(id); got != want {
			t.Fatalf("state #%d = %q, want %q", id, got, want)
		}
	}
	if h.set.Ready(2) || h.set.Ready(4) {
		t.Fatal("assignments under a hold are ready")
	}
	if line := h.set.OpenAssignments("eng1"); !strings.Contains(line, `#2 "part a" (on hold under #1)`) || !strings.Contains(line, `#5 "elsewhere" (blocked by #1)`) {
		t.Fatalf("eng1 open assignments = %q", line)
	}
	if h.set.Query(Filter{Ready: ptr(true)}) != nil {
		t.Fatal("ready filter found something under a hold")
	}

	hold = false
	ch = h.update(1, "cos", UpdateInput{Hold: &hold, Note: "scope settled"})
	if got := strings.Join(wakesTo(ch.Wakes), ","); got != "eng1,eng2,vp" {
		t.Fatalf("resume woke %s", got)
	}
	eng1 = wakeTo(t, ch.Wakes, "eng1")
	if eng1.Op != OpResumed || eng1.Title != `#1 resumed: epic` || !strings.Contains(eng1.Body, `cos resumed #1 "epic": scope settled`) || !strings.Contains(eng1.Body, `Carry on with #2 "part a" (part of it, ready).`) {
		t.Fatalf("eng1 resume wake = %+v", eng1)
	}
	if strings.Contains(eng1.Body, "is now ready") || len(eng1.Flips) != 0 {
		t.Fatalf("resume wake repeats the assignment as a flip: %+v", eng1)
	}
	if vp = wakeTo(t, ch.Wakes, "vp"); !strings.Contains(vp.Body, `Carry on with #1 "epic" (blocked by #2, #3).`) {
		t.Fatalf("vp resume wake = %+v", vp)
	}
	if got := h.set.State(2); got != "ready" {
		t.Fatalf("state #2 after resume = %q", got)
	}
}

func ptr[T any](v T) *T { return &v }

// Who may hold, what a hold needs, and how a hold of an assignment's own
// interacts with one it inherits.
func TestHoldRules(t *testing.T) {
	h := newHarness(t, "cos", "vp", "eng1")
	h.create("cos", CreateInput{Title: "epic", Assignee: "vp"})               // #1
	h.create("vp", CreateInput{Title: "part a", Assignee: "eng1", Parent: 1}) // #2
	on, off := true, false

	_, err := h.rules().Update(1, "vp", UpdateInput{Hold: &on, Note: "x"})
	wantRefusal(t, err, "only #1's creator (cos) or the owner can put it on hold or resume it")
	_, err = h.rules().Update(1, "cos", UpdateInput{Hold: &on})
	wantRefusal(t, err, "note is required: putting #1 on hold wakes its assignee and everyone working under it")
	title := "renamed"
	_, err = h.rules().Update(1, "cos", UpdateInput{Hold: &on, Title: &title, Note: "x"})
	wantRefusal(t, err, "a hold is a change on its own")
	_, err = h.rules().Update(1, "cos", UpdateInput{Hold: &off, Note: "x"})
	wantRefusal(t, err, "#1 is not on hold")

	h.update(1, "cos", UpdateInput{Hold: &on, Note: "pause"})
	_, err = h.rules().Update(1, "cos", UpdateInput{Hold: &on, Note: "x"})
	wantRefusal(t, err, "#1 is already on hold")

	// A hold of its own on a child already covered changes no state
	// and wakes nobody; it outlives the parent's resume.
	ch := h.update(2, "vp", UpdateInput{Hold: &on, Note: "keep this part paused"})
	if len(ch.Wakes) != 0 {
		t.Fatalf("holding an already-held child woke %v", wakesTo(ch.Wakes))
	}
	ch = h.update(1, "cos", UpdateInput{Hold: &off, Note: "go"})
	if got := strings.Join(wakesTo(ch.Wakes), ","); got != "vp" {
		t.Fatalf("resuming the epic woke %s; #2 has a hold of its own", got)
	}
	if got := h.set.State(2); got != "on hold" {
		t.Fatalf("state #2 = %q", got)
	}
	ch = h.update(2, "vp", UpdateInput{Hold: &off, Note: "go"})
	if got := strings.Join(wakesTo(ch.Wakes), ","); got != "eng1" || h.set.State(2) != "ready" {
		t.Fatalf("resuming #2 woke %s, state %q", got, h.set.State(2))
	}

	// The CEO can hold anything; a child filed under a held parent is
	// told so; closing lifts an assignment's own hold.
	h.update(1, CEO, UpdateInput{Hold: &on, Note: "ceo pauses"})
	ch = h.create("cos", CreateInput{Title: "part c", Assignee: "eng1", Parent: 1})
	if w := wakeTo(t, ch.Wakes, "eng1"); !strings.Contains(w.Body, "It is on hold under #1: do nothing on it until #1 is resumed") {
		t.Fatalf("create under a held parent = %+v", w)
	}
	h.update(2, "vp", UpdateInput{Hold: &on, Note: "pause"})
	ch = h.close(2, "eng1", ResolutionDone, "done anyway")
	if ch.Assignment.Held {
		t.Fatal("a closed assignment kept its hold")
	}
	if _, err := Marshal(ch.Assignment); err != nil {
		t.Fatalf("closed assignment does not validate: %v", err)
	}
}

// The held flag round-trips through the file, and a closed record
// carrying one is refused.
func TestHeldRoundTripsAndNeverCloses(t *testing.T) {
	h := newHarness(t, "cos", "vp")
	h.create("cos", CreateInput{Title: "epic", Assignee: "vp"})
	on := true
	ch := h.update(1, "cos", UpdateInput{Hold: &on, Note: "pause"})
	data, err := Marshal(ch.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "held: true\n") {
		t.Fatalf("held not written:\n%s", data)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Held || back.Log[len(back.Log)-1].Op != OpHeld {
		t.Fatalf("held lost on parse: %+v", back)
	}
	closed := ch.Assignment.clone()
	now := h.now
	closed.Status, closed.Closed, closed.Resolution, closed.Outcome = StatusClosed, &now, ResolutionDone, "x"
	closed.Log = append(closed.Log, Entry{Seq: 99, TS: now, By: "vp", Op: OpClosed, Resolution: ResolutionDone, Note: "x"})
	if err := Validate(closed); err == nil || !strings.Contains(err.Error(), "closed but on hold") {
		t.Fatalf("closed+held validated: %v", err)
	}
}

// Whoever filed or holds an assignment may drop anything under it, which
// is how the creator of an epic cancels the parts its assignee filed.
// Done stays the assignee's.
func TestAncestorCreatorMayDrop(t *testing.T) {
	h := newHarness(t, "cos", "vp", "eng1", "eng2", "bystander")
	h.create("cos", CreateInput{Title: "epic", Assignee: "vp"})                // #1
	h.create("vp", CreateInput{Title: "part a", Assignee: "eng1", Parent: 1})  // #2
	h.create("eng1", CreateInput{Title: "sub a", Assignee: "eng2", Parent: 2}) // #3

	_, err := h.rules().Close(3, "bystander", ResolutionDropped, "no")
	wantRefusal(t, err, "only #3's creator (eng1), its assignee (eng2), the creator or assignee of an assignment it is part of, or the owner can drop it")
	_, err = h.rules().Close(3, "cos", ResolutionDone, "no")
	wantRefusal(t, err, "only #3's assignee (eng2) or the owner can close it as done")
	_, err = h.rules().Close(1, "cos", ResolutionDropped, "cancelled")
	wantRefusal(t, err, "#1 has open parts #2")

	ch := h.close(3, "cos", ResolutionDropped, "epic cancelled")
	if got := strings.Join(wakesTo(ch.Wakes), ","); got != "eng1,eng2" {
		t.Fatalf("dropping a grandchild woke %s", got)
	}
	if w := wakeTo(t, ch.Wakes, "eng2"); !strings.Contains(w.Body, "Stop work on it.") || !strings.Contains(w.Body, "Reason: epic cancelled") {
		t.Fatalf("eng2 wake = %+v", w)
	}
	h.close(2, "cos", ResolutionDropped, "epic cancelled")
	ch = h.close(1, "cos", ResolutionDropped, "epic cancelled")
	if got := strings.Join(wakesTo(ch.Wakes), ","); got != "vp" {
		t.Fatalf("dropping the epic woke %s", got)
	}
}
