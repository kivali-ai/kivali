package assignments

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Rules applies a change to a Set under every rule in docs/developers/assignments.md
// §Rules, and says who to wake (§Wakes). Active reports whether a slug
// is an active agent; the CEO always counts. Now stamps the change.
type Rules struct {
	Set    *Set
	Active func(slug string) bool
	Now    time.Time
}

// CreateInput is what assignment_create takes.
type CreateInput struct {
	Title     string
	Body      string
	Assignee  string
	Parent    int
	BlockedBy []int
	// Acceptance names what this assignment expects from the assignments filed
	// under it; Satisfies names which of the parent's items this one
	// delivers, and needs a Parent.
	Acceptance []string
	Satisfies  []string
}

// UpdateInput is what assignment_update takes. A nil pointer leaves the
// field alone; Parent pointing at 0 clears the parent. Hold true puts
// the assignment on hold and false resumes it; a hold is a change on its
// own, refused alongside any other field, because it wakes everyone
// under the assignment and nothing else should ride on that.
type UpdateInput struct {
	Title           *string
	Body            *string
	Assignee        *string
	Parent          *int
	AddBlockedBy    []int
	RemoveBlockedBy []int
	Hold            *bool
	Note            string
	// The two item lists change the way blocked_by does: one name at
	// a time, added or removed.
	AddAcceptance    []string
	RemoveAcceptance []string
	AddSatisfies     []string
	RemoveSatisfies  []string
}

// other reports whether anything but Hold and Note is set.
func (in UpdateInput) other() bool {
	return in.Title != nil || in.Body != nil || in.Assignee != nil || in.Parent != nil ||
		len(in.AddBlockedBy) > 0 || len(in.RemoveBlockedBy) > 0 ||
		len(in.AddAcceptance) > 0 || len(in.RemoveAcceptance) > 0 ||
		len(in.AddSatisfies) > 0 || len(in.RemoveSatisfies) > 0
}

// editList applies removes then adds to a name list, refusing a
// remove of a name not there and an add of one already there.
func editList(iss *Assignment, field string, cur, remove, add []string) ([]string, error) {
	want := append([]string(nil), cur...)
	for _, n := range NormalizeItems(remove) {
		idx := indexOfItem(want, n)
		if idx < 0 {
			return nil, refuse("%s has no %q in %s; that list is %s", iss.Ref(), n, field, quoteList(want))
		}
		want = append(want[:idx], want[idx+1:]...)
	}
	for _, n := range NormalizeItems(add) {
		if indexOfItem(want, n) >= 0 {
			return nil, refuse("%s already has %q in %s", iss.Ref(), n, field)
		}
		want = append(want, n)
	}
	return want, nil
}

// Wake is one message the change owes one agent: who, the primary op
// it reports, a title for the inbox card, and the body the agent
// reads. The CEO is a valid recipient.
type Wake struct {
	To    string
	Op    Op
	Title string
	Body  string
	// Flips lists the other assignments this wake reports as newly ready,
	// folded into the body. A wake that carries one is news even to
	// an agent who never saw the assignment the change retired.
	Flips []int `json:"flips,omitempty"`
	// Quiet marks a wake that is told, not woken for: it folds into a
	// turn the agent is running and otherwise waits in their chat for
	// whatever wakes them next. A hold is quiet; an idle agent has
	// nothing to stop.
	Quiet bool `json:"quiet,omitempty"`
}

// Change is an applied action: the assignment as it now stands, the Set
// after it, the log entries appended, the wakes to route, and, when
// the spec was amended, the prior text to snapshot.
type Change struct {
	Assignment *Assignment
	Set        *Set
	Entries    []Entry
	Wakes      []Wake
	PriorText  string
	// Touched lists other assignments the change altered without a log
	// entry, to be written after Assignment: today the parent whose nudge
	// flag a first child raised.
	Touched []*Assignment
	// Unmet names the acceptance items a done close left unsatisfied.
	// The close succeeded; this is the warning the closer, the log and
	// every wake carry.
	Unmet []string
}

func (r Rules) isCEO(slug string) bool { return slug == CEO }

// active reports whether slug can be woken: the CEO always, otherwise
// what the caller's Active says.
func (r Rules) active(slug string) bool {
	return r.isCEO(slug) || (r.Active != nil && r.Active(slug))
}

// actor checks that slug may act at all: the CEO, or an active agent.
func (r Rules) actor(slug string) error {
	if !validSlug(slug) {
		return refuse("%q is not an agent slug", slug)
	}
	if r.active(slug) {
		return nil
	}
	return refuse("%q is not an active agent; get_org_chart lists who is", slug)
}

func (r Rules) canEditSpec(by string, iss *Assignment) bool {
	return by == iss.Creator || r.isCEO(by)
}

func (r Rules) canBlock(by string, iss *Assignment) bool {
	return by == iss.Creator || by == iss.Assignee || r.isCEO(by)
}

// canDrop is canBlock widened up the tree: a child is part of its
// parent's work, so whoever filed or holds an assignment it is part of may
// drop it. Without this the creator of an epic could cancel neither
// the epic (open children) nor the children its assignee filed.
func (r Rules) canDrop(by string, iss *Assignment) bool {
	if r.canBlock(by, iss) {
		return true
	}
	for _, a := range r.Set.Ancestors(iss.ID) {
		if p, ok := r.Set.Get(a); ok && (by == p.Creator || by == p.Assignee) {
			return true
		}
	}
	return false
}

func (r Rules) canOpenUnder(by string, parent *Assignment) bool {
	return by == parent.Creator || by == parent.Assignee || r.isCEO(by)
}

// canEditSatisfies is canBlock widened to the parent's creator and
// assignee: what a child delivers is the parent's business too.
func (r Rules) canEditSatisfies(by string, iss, parent *Assignment) bool {
	if r.canBlock(by, iss) {
		return true
	}
	return parent != nil && (by == parent.Creator || by == parent.Assignee)
}

// conditionsFor validates an acceptance list as it would be stored.
func conditionsFor(iss *Assignment, items []string) ([]string, error) {
	want := NormalizeItems(items)
	if err := checkItems("acceptance", want); err != nil {
		return nil, refuse("%s: %v", iss.Ref(), err)
	}
	return want, nil
}

// countsTowardFor validates the items a child claims against its
// parent's list. The refusal carries the parent's items, so the
// caller can name one exactly or ask for a new one.
func countsTowardFor(child, parent *Assignment, names []string) ([]string, error) {
	want := NormalizeItems(names)
	if len(want) == 0 {
		return nil, nil
	}
	if parent == nil {
		return nil, refuse("%s is part of nothing, so there is nothing for it to count toward; open it as a part of the assignment whose Done-when condition it delivers, or leave satisfies empty", child.Ref())
	}
	if err := checkItems("satisfies", want); err != nil {
		return nil, refuse("%s: %v", child.Ref(), err)
	}
	if len(parent.Acceptance) == 0 {
		return nil, refuse("%s has no Done-when conditions to count toward; its creator (%s) or assignee (%s) can declare them with assignment_update add_acceptance", parent.Ref(), parent.Creator, parent.Assignee)
	}
	for _, n := range want {
		if indexOfItem(parent.Acceptance, n) < 0 {
			return nil, refuse("%s cannot count toward %q: %s's Done-when conditions are %s; name one of them exactly, or have %s's creator (%s) or assignee (%s) add it with assignment_update add_acceptance", child.Ref(), n, parent.Ref(), quoteList(parent.Acceptance), parent.Ref(), parent.Creator, parent.Assignee)
		}
	}
	return want, nil
}

// firstPartNudge is the parent to touch when an assignment is filed or
// moved under it: a parent with no children yet and no acceptance
// items has its Nudge raised, so its assignee's next wake note says
// so once. nil when there is nothing to raise.
func (r Rules) firstPartNudge(parent int) *Assignment {
	p, ok := r.Set.Get(parent)
	if !ok || len(r.Set.children[parent]) > 0 || len(p.Acceptance) > 0 || p.Nudge {
		return nil
	}
	pc := p.clone()
	pc.Nudge = true
	return pc
}

// partOfFor validates a parent id for an assignment by is filing or moving.
func (r Rules) partOfFor(by string, parent int, self int) (*Assignment, error) {
	if parent == self {
		return nil, refuse("%s cannot be part of itself", Ref(self))
	}
	p, ok := r.Set.Get(parent)
	if !ok {
		return nil, refuse("no assignment %s to open it under; assignment_list shows what exists", Ref(parent))
	}
	if !p.Open() {
		return nil, refuse("%s is closed, so no part can be opened under it; reopen it first (its creator %s or the owner) or open this as a goal", p.Ref(), p.Creator)
	}
	if !r.canOpenUnder(by, p) {
		return nil, refuse("only %s's creator (%s), its assignee (%s) or the owner can open parts under it, because an open part holds it up; open this as a goal, or as a part of one of your own assignments", p.Ref(), p.Creator, p.Assignee)
	}
	return p, nil
}

// hasCycle reports whether id reaches itself along the wait edges.
func hasCycle(s *Set, id int) bool {
	iss, ok := s.byID[id]
	if !ok {
		return false
	}
	for _, b := range iss.BlockedBy {
		if s.waitsOn(b, id) {
			return true
		}
	}
	for _, c := range s.children[id] {
		if s.waitsOn(c, id) {
			return true
		}
	}
	return false
}

// Create files a new assignment by `by`.
func (r Rules) Create(by string, in CreateInput) (Change, error) {
	if err := r.actor(by); err != nil {
		return Change{}, err
	}
	title := strings.TrimSpace(in.Title)
	if err := checkTitle(title); err != nil {
		return Change{}, refuse("%v", err)
	}
	body := NormalizeText(in.Body)
	if len(body) > MaxBodyBytes {
		return Change{}, refuse("description is %d bytes, cap is %d; put the substance in an artifact and point at it", len(body), MaxBodyBytes)
	}
	assignee := strings.TrimSpace(in.Assignee)
	if assignee == "" {
		assignee = by
	}
	if err := r.actor(assignee); err != nil {
		return Change{}, refuse("assignee %v", err)
	}
	id := r.Set.MaxID() + 1
	seq := r.Set.MaxSeq() + 1
	iss := &Assignment{
		ID: id, Title: title, Status: StatusOpen, Assignee: assignee, Creator: by,
		Created: r.Now, Updated: r.Now, Body: body,
	}
	var parent *Assignment
	if in.Parent != 0 {
		p, err := r.partOfFor(by, in.Parent, id)
		if err != nil {
			return Change{}, err
		}
		iss.Parent = in.Parent
		parent = p
	}
	blockers, err := r.blockersFor(iss, in.BlockedBy)
	if err != nil {
		return Change{}, err
	}
	iss.BlockedBy = blockers
	if iss.Acceptance, err = conditionsFor(iss, in.Acceptance); err != nil {
		return Change{}, err
	}
	if iss.Satisfies, err = countsTowardFor(iss, parent, in.Satisfies); err != nil {
		return Change{}, err
	}
	iss.Log = []Entry{{Seq: seq, TS: r.Now, By: by, Op: OpCreated, To: assignee}}
	after := r.Set.with(iss)
	if hasCycle(after, id) {
		return Change{}, refuse("opening %s under %s while it waits on %s would make a cycle: something it waits on already waits on the assignment it is part of", iss.Ref(), Ref(iss.Parent), refList(iss.BlockedBy))
	}
	var touched []*Assignment
	if iss.Parent != 0 {
		if pc := r.firstPartNudge(iss.Parent); pc != nil {
			after = after.with(pc)
			touched = append(touched, pc)
		}
	}
	ch := Change{Assignment: iss, Set: after, Entries: iss.Log, Touched: touched}
	var wakes []Wake
	if assignee != by {
		wakes = append(wakes, Wake{
			To: assignee, Op: OpCreated,
			Title: fmt.Sprintf("%s assigned to you: %s", iss.Ref(), iss.Title),
			Body:  fmt.Sprintf("%s assigned you %s %q.\n%s\n\n%s", by, iss.Ref(), iss.Title, describe(after, iss), spec(iss)),
		})
	}
	ch.Wakes = r.mergeFlips(wakes, r.Set, after, only(iss.ID), by, OpCreated, "", "")
	return ch, nil
}

// blockersFor validates a blocked_by list for iss.
func (r Rules) blockersFor(iss *Assignment, ids []int) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, b := range ids {
		if b == iss.ID {
			return nil, refuse("%s cannot block itself", iss.Ref())
		}
		if seen[b] {
			continue
		}
		seen[b] = true
		if _, ok := r.Set.Get(b); !ok {
			return nil, refuse("no assignment %s to wait on; assignment_list shows what exists", Ref(b))
		}
		out = append(out, b)
	}
	sort.Ints(out)
	return out, nil
}

// Update amends an open assignment.
func (r Rules) Update(id int, by string, in UpdateInput) (Change, error) {
	if err := r.actor(by); err != nil {
		return Change{}, err
	}
	cur, ok := r.Set.Get(id)
	if !ok {
		return Change{}, refuse("no assignment %s; assignment_list shows what exists", Ref(id))
	}
	if !cur.Open() {
		return Change{}, refuse("%s is closed (%s); a closed assignment takes no edit but assignment_reopen, which its creator %s or the owner can call", cur.Ref(), cur.Resolution, cur.Creator)
	}
	note := strings.TrimSpace(NormalizeText(in.Note))
	if len(note) > MaxNoteBytes {
		return Change{}, refuse("note is %d bytes, cap is %d", len(note), MaxNoteBytes)
	}
	iss := cur.clone()
	iss.Updated = r.Now
	seq := r.Set.MaxSeq()
	next := func() int64 { seq++; return seq }
	var entries []Entry
	var fields []string
	prior := ""

	if in.Hold != nil {
		if in.other() {
			return Change{}, refuse("a hold is a change on its own: put %s on hold, or resume it, in an assignment_update that changes nothing else", cur.Ref())
		}
		if !r.canEditSpec(by, cur) {
			return Change{}, refuse("only %s's creator (%s) or the owner can put it on hold or resume it; if you hold it and cannot continue, open a question under it or say what it waits on with add_blocked_by", cur.Ref(), cur.Creator)
		}
		if *in.Hold == cur.Held {
			if cur.Held {
				return Change{}, refuse("%s is already on hold", cur.Ref())
			}
			return Change{}, refuse("%s is not on hold", cur.Ref())
		}
		if note == "" {
			if *in.Hold {
				return Change{}, refuse("note is required: putting %s on hold wakes its assignee and everyone working under it, who need to know why", cur.Ref())
			}
			return Change{}, refuse("note is required: resuming %s wakes its assignee and everyone working under it, who need to know what changed", cur.Ref())
		}
		iss.Held = *in.Hold
		op := OpResumed
		if iss.Held {
			op = OpHeld
		}
		entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: op, Note: note})
		iss.Log = append(iss.Log, entries...)
		after := r.Set.with(iss)
		ch := Change{Assignment: iss, Set: after, Entries: entries}
		wakes, covered := r.holdWakes(r.Set, after, iss, by, op, note)
		ch.Wakes = r.mergeFlips(wakes, r.Set, after, covered, by, op, "", "")
		return ch, nil
	}

	if in.Title != nil || in.Body != nil {
		if !r.canEditSpec(by, cur) {
			return Change{}, refuse("only %s's creator (%s) or the owner can amend it; if you hold it and the spec is wrong, open a question under it for %s, or close it as dropped with the reason", cur.Ref(), cur.Creator, cur.Creator)
		}
		if in.Title != nil {
			t := strings.TrimSpace(*in.Title)
			if err := checkTitle(t); err != nil {
				return Change{}, refuse("%v", err)
			}
			if t != cur.Title {
				iss.Title = t
				fields = append(fields, "title")
			}
		}
		if in.Body != nil {
			b := NormalizeText(*in.Body)
			if len(b) > MaxBodyBytes {
				return Change{}, refuse("description is %d bytes, cap is %d; put the substance in an artifact and point at it", len(b), MaxBodyBytes)
			}
			if b != cur.Body {
				iss.Body = b
				fields = append(fields, "description")
			}
		}
		if len(fields) > 0 {
			if cur.Assignee != by && note == "" {
				return Change{}, refuse("note is required: amending %s wakes its assignee %s, who needs to know what changed and why", cur.Ref(), cur.Assignee)
			}
			prior = cur.Title + "\n\n" + cur.Body
			entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: OpAmended, Fields: strings.Join(fields, ","), Note: note, Prior: hashText(prior)})
		}
	}

	oldAssignee := ""
	if in.Assignee != nil {
		to := strings.TrimSpace(*in.Assignee)
		if !r.canEditSpec(by, cur) {
			return Change{}, refuse("only %s's creator (%s) or the owner can reassign it; an assignee does not hand an assignment on. Break it into parts for whoever should do them, or close it as dropped with the reason", cur.Ref(), cur.Creator)
		}
		if to == cur.Assignee {
			return Change{}, refuse("%s is already assigned to %s", cur.Ref(), to)
		}
		if err := r.actor(to); err != nil {
			return Change{}, refuse("assignee %v", err)
		}
		if note == "" {
			return Change{}, refuse("note is required: reassigning %s wakes both %s and %s, who need to know why", cur.Ref(), cur.Assignee, to)
		}
		oldAssignee = cur.Assignee
		iss.Assignee = to
		entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: OpAssigned, From: oldAssignee, To: to, Note: note})
	}

	movedTo := 0
	if in.Parent != nil && *in.Parent != cur.Parent {
		if !r.canEditSpec(by, cur) {
			return Change{}, refuse("only %s's creator (%s) or the owner can make it part of another assignment", cur.Ref(), cur.Creator)
		}
		if *in.Parent != 0 {
			if _, err := r.partOfFor(by, *in.Parent, id); err != nil {
				return Change{}, err
			}
		}
		iss.Parent = *in.Parent
		movedTo = *in.Parent
		entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: OpParent, Ref: *in.Parent, Note: note})
	}

	if len(in.AddBlockedBy) > 0 || len(in.RemoveBlockedBy) > 0 {
		if !r.canBlock(by, cur) {
			return Change{}, refuse("only %s's creator (%s), its assignee (%s) or the owner can change what it waits on", cur.Ref(), cur.Creator, cur.Assignee)
		}
	}
	for _, b := range in.RemoveBlockedBy {
		idx := indexOf(iss.BlockedBy, b)
		if idx < 0 {
			return Change{}, refuse("%s does not wait on %s", cur.Ref(), Ref(b))
		}
		iss.BlockedBy = append(iss.BlockedBy[:idx], iss.BlockedBy[idx+1:]...)
		entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: OpUnblocked, Ref: b, Note: note})
	}
	for _, b := range in.AddBlockedBy {
		if b == id {
			return Change{}, refuse("%s cannot block itself", cur.Ref())
		}
		if indexOf(iss.BlockedBy, b) >= 0 {
			return Change{}, refuse("%s already waits on %s", cur.Ref(), Ref(b))
		}
		if _, ok := r.Set.Get(b); !ok {
			return Change{}, refuse("no assignment %s to wait on; assignment_list shows what exists", Ref(b))
		}
		iss.BlockedBy = append(iss.BlockedBy, b)
		sort.Ints(iss.BlockedBy)
		entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: OpBlocked, Ref: b, Note: note})
	}

	if len(in.AddAcceptance) > 0 || len(in.RemoveAcceptance) > 0 {
		if !r.canBlock(by, cur) {
			return Change{}, refuse("only %s's creator (%s), its assignee (%s) or the owner can change its Done-when conditions", cur.Ref(), cur.Creator, cur.Assignee)
		}
		want, err := editList(cur, "acceptance", cur.Acceptance, in.RemoveAcceptance, in.AddAcceptance)
		if err != nil {
			return Change{}, err
		}
		if err := checkItems("acceptance", want); err != nil {
			return Change{}, refuse("%s: %v", cur.Ref(), err)
		}
		for _, c := range r.Set.OpenParts(id) {
			child, _ := r.Set.Get(c)
			for _, n := range child.Satisfies {
				if indexOfItem(cur.Acceptance, n) >= 0 && indexOfItem(want, n) < 0 {
					return Change{}, refuse("%q is claimed by open %s %q; change what %s counts toward first, or close it", n, child.Ref(), child.Title, child.Ref())
				}
			}
		}
		if !sameItems(want, cur.Acceptance) {
			iss.Acceptance = want
			if len(want) > 0 {
				iss.Nudge = false
			}
			entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: OpAcceptance, Items: want, Note: note})
		}
	}

	if satChanged := len(in.AddSatisfies) > 0 || len(in.RemoveSatisfies) > 0; satChanged || (movedTo != cur.Parent && len(cur.Satisfies) > 0) {
		// The list must name the parent's items, whichever parent the
		// assignment is under once this change lands: a move takes the old
		// parent's names with it, so a move away from them removes
		// them in the same call.
		var parent *Assignment
		if iss.Parent != 0 {
			parent, _ = r.Set.Get(iss.Parent)
		}
		if satChanged && !r.canEditSatisfies(by, cur, parent) {
			return Change{}, refuse("only %s's creator (%s), its assignee (%s), the creator or assignee of the assignment it is part of, or the owner can change what it counts toward", cur.Ref(), cur.Creator, cur.Assignee)
		}
		want, err := editList(cur, "satisfies", cur.Satisfies, in.RemoveSatisfies, in.AddSatisfies)
		if err != nil {
			return Change{}, err
		}
		if !satChanged && len(want) > 0 {
			if parent == nil {
				return Change{}, refuse("%s counts toward %s of %s; remove them with remove_satisfies in the same call to make it a goal", cur.Ref(), quoteList(want), Ref(cur.Parent))
			}
			for _, n := range want {
				if indexOfItem(parent.Acceptance, n) < 0 {
					return Change{}, refuse("%s counts toward %q, which %s does not declare (its Done-when conditions: %s); remove it with remove_satisfies in the same call, or add one of %s's conditions", cur.Ref(), n, parent.Ref(), quoteList(parent.Acceptance), parent.Ref())
				}
			}
		}
		if want, err = countsTowardFor(cur, parent, want); err != nil {
			return Change{}, err
		}
		if !sameItems(want, cur.Satisfies) {
			iss.Satisfies = want
			entries = append(entries, Entry{Seq: next(), TS: r.Now, By: by, Op: OpSatisfies, Items: want, Note: note})
		}
	}

	if len(entries) == 0 {
		return Change{}, refuse("nothing to change on %s", cur.Ref())
	}
	iss.Log = append(iss.Log, entries...)
	after := r.Set.with(iss)
	if hasCycle(after, id) {
		return Change{}, refuse("that would make a cycle: %s would wait, directly or through its parts and blockers, on something that waits on it", cur.Ref())
	}
	var touched []*Assignment
	if movedTo != 0 {
		if pc := r.firstPartNudge(movedTo); pc != nil {
			after = after.with(pc)
			touched = append(touched, pc)
		}
	}
	ch := Change{Assignment: iss, Set: after, Entries: entries, PriorText: prior, Touched: touched}

	var wakes []Wake
	primary := OpAmended
	switch {
	case oldAssignee != "":
		primary = OpAssigned
		if iss.Assignee != by {
			wakes = append(wakes, Wake{
				To: iss.Assignee, Op: OpAssigned,
				Title: fmt.Sprintf("%s assigned to you: %s", iss.Ref(), iss.Title),
				Body: fmt.Sprintf("%s reassigned %s %q to you (from %s): %s\n%s\n\n%s",
					by, iss.Ref(), iss.Title, oldAssignee, note, describe(after, iss), spec(iss)),
			})
		}
		if oldAssignee != by {
			wakes = append(wakes, Wake{
				To: oldAssignee, Op: OpAssigned,
				Title: fmt.Sprintf("%s reassigned to %s: %s", iss.Ref(), iss.Assignee, iss.Title),
				Body: fmt.Sprintf("%s reassigned %s %q from you to %s: %s\nStop work on it.",
					by, iss.Ref(), iss.Title, iss.Assignee, note),
			})
		}
	case len(fields) > 0:
		if iss.Assignee != by {
			wakes = append(wakes, Wake{
				To: iss.Assignee, Op: OpAmended,
				Title: fmt.Sprintf("%s amended: %s", iss.Ref(), iss.Title),
				Body: fmt.Sprintf("%s amended %s %q (%s): %s\nThe spec as it now stands follows; act on this version.\n%s\n\n%s",
					by, iss.Ref(), iss.Title, strings.Join(fields, " and "), note, describe(after, iss), spec(iss)),
			})
		}
	default:
		primary = entries[len(entries)-1].Op
	}
	cause := ""
	switch primary {
	case OpUnblocked, OpBlocked, OpParent:
		cause = fmt.Sprintf("%s changed what %s waits on", by, iss.Ref())
	}
	ch.Wakes = r.mergeFlips(wakes, r.Set, after, only(iss.ID), by, primary, cause, "")
	return ch, nil
}

// holdWakes is what a hold or a resume of iss owes: one wake to the
// assignee of every open assignment whose derived hold changed, that is,
// iss and everything under it not already covered by a hold of its
// own or of a nearer ancestor. Each agent gets one wake listing the
// assignments of theirs it concerns. Returns the wakes and the ids they
// cover, so mergeFlips does not report the same assignments again.
func (r Rules) holdWakes(before, after *Set, iss *Assignment, by string, op Op, note string) ([]Wake, map[int]bool) {
	covered := map[int]bool{}
	byTo := map[string][]*Assignment{}
	var order []string
	for _, id := range append([]int{iss.ID}, after.OpenDescendants(iss.ID)...) {
		if before.Held(id) == after.Held(id) {
			continue
		}
		covered[id] = true
		t, _ := after.Get(id)
		if t.Assignee == by {
			continue
		}
		if _, seen := byTo[t.Assignee]; !seen {
			order = append(order, t.Assignee)
		}
		byTo[t.Assignee] = append(byTo[t.Assignee], t)
	}
	sort.Strings(order)
	var wakes []Wake
	for _, to := range order {
		var parts []string
		for _, t := range byTo[to] {
			part := fmt.Sprintf("%s %q", t.Ref(), t.Title)
			var notes []string
			if t.ID != iss.ID {
				notes = append(notes, "part of it")
			}
			if op == OpResumed {
				notes = append(notes, after.State(t.ID))
			}
			if len(notes) > 0 {
				part += " (" + strings.Join(notes, ", ") + ")"
			}
			parts = append(parts, part)
		}
		list := strings.Join(parts, ", ")
		var title, body string
		if op == OpHeld {
			title = fmt.Sprintf("%s on hold: %s", iss.Ref(), iss.Title)
			body = fmt.Sprintf("%s put %s %q on hold: %s\nStop work on %s until it is resumed; you are woken when it is.", by, iss.Ref(), iss.Title, note, list)
		} else {
			title = fmt.Sprintf("%s resumed: %s", iss.Ref(), iss.Title)
			body = fmt.Sprintf("%s resumed %s %q: %s\nCarry on with %s.", by, iss.Ref(), iss.Title, note, list)
		}
		wakes = append(wakes, Wake{To: to, Op: op, Title: title, Body: body, Quiet: op == OpHeld})
	}
	return wakes, covered
}

// only is the covered set for a change that describes one assignment.
func only(id int) map[int]bool { return map[int]bool{id: true} }

// Close ends an open assignment with an outcome.
func (r Rules) Close(id int, by string, res Resolution, outcome string) (Change, error) {
	if err := r.actor(by); err != nil {
		return Change{}, err
	}
	cur, ok := r.Set.Get(id)
	if !ok {
		return Change{}, refuse("no assignment %s; assignment_list shows what exists", Ref(id))
	}
	if !cur.Open() {
		return Change{}, refuse("%s is already closed (%s) by %s", cur.Ref(), cur.Resolution, cur.Log[len(cur.Log)-1].By)
	}
	outcome = strings.TrimSpace(NormalizeText(outcome))
	switch res {
	case ResolutionDone:
		if by != cur.Assignee && !r.isCEO(by) {
			return Change{}, refuse("only %s's assignee (%s) or the owner can close it as done. If the work is no longer wanted, its creator can close it as dropped with the reason", cur.Ref(), cur.Assignee)
		}
		if err := checkText("outcome", outcome, MaxOutcomeBytes); err != nil {
			return Change{}, refuse("%v: say what was done and where the result is", err)
		}
	case ResolutionDropped:
		if !r.canDrop(by, cur) {
			return Change{}, refuse("only %s's creator (%s), its assignee (%s), the creator or assignee of an assignment it is part of, or the owner can drop it", cur.Ref(), cur.Creator, cur.Assignee)
		}
		if err := checkText("outcome", outcome, MaxOutcomeBytes); err != nil {
			return Change{}, refuse("%v: say why it is being dropped; whoever is woken reads that first", err)
		}
	default:
		return Change{}, refuse("resolution %q is not done or dropped", res)
	}
	if open := r.Set.OpenParts(id); len(open) > 0 {
		return Change{}, refuse("%s has open parts %s; close or drop them first, each with its own outcome", cur.Ref(), refList(open))
	}
	// A done close with acceptance items still unmet succeeds and is
	// recorded as such: on the log entry, in the closer's result, and
	// in every wake the close sends. Never a refusal, which would only
	// teach vaguer items. Dropping meets nothing and warns of nothing.
	var unmet []string
	if res == ResolutionDone {
		unmet = r.Set.Unmet(id)
	}
	iss := cur.clone()
	now := r.Now
	iss.Status = StatusClosed
	iss.Closed = &now
	iss.Resolution = res
	iss.Outcome = outcome
	iss.Held = false
	iss.Nudge = false
	iss.Updated = now
	e := Entry{Seq: r.Set.MaxSeq() + 1, TS: now, By: by, Op: OpClosed, Resolution: res, Note: outcome, Unmet: unmet}
	iss.Log = append(iss.Log, e)
	after := r.Set.with(iss)
	ch := Change{Assignment: iss, Set: after, Entries: []Entry{e}, Unmet: unmet}

	verb := "closed"
	label := "Outcome"
	if res == ResolutionDropped {
		verb = "dropped"
		label = "Reason"
	}
	warning := UnmetWarning(unmet)
	var wakes []Wake
	for _, to := range uniqueSlugs(cur.Creator, cur.Assignee) {
		if to == by {
			continue
		}
		body := fmt.Sprintf("%s %s %s %q", by, verb, iss.Ref(), iss.Title)
		if res == ResolutionDone {
			body += " as done"
		}
		body += "."
		if to == cur.Assignee {
			body += " Stop work on it."
		}
		body += fmt.Sprintf("\n\n%s: %s", label, outcome)
		if warning != "" {
			body += "\n\n" + warning
		}
		title := fmt.Sprintf("%s %s: %s", iss.Ref(), verb, iss.Title)
		if res == ResolutionDone {
			title = fmt.Sprintf("%s closed (done): %s", iss.Ref(), iss.Title)
		}
		wakes = append(wakes, Wake{To: to, Op: OpClosed, Title: title, Body: body})
	}
	cause := fmt.Sprintf("%s %s %s %q", by, verb, iss.Ref(), iss.Title)
	if res == ResolutionDone {
		cause += " as done"
	}
	detail := fmt.Sprintf("%s of %s: %s", label, iss.Ref(), iss.Outcome)
	if warning != "" {
		detail += "\n\n" + warning
	}
	ch.Wakes = r.mergeFlips(wakes, r.Set, after, only(iss.ID), by, OpClosed, cause, detail)
	return ch, nil
}

// UnmetWarning is the sentence a done close with unmet items carries
// everywhere it is reported. Empty when nothing was unmet.
func UnmetWarning(unmet []string) string {
	if len(unmet) == 0 {
		return ""
	}
	return fmt.Sprintf("Warning: closed as done with %d Done-when condition%s unmet: %s.", len(unmet), plural(len(unmet)), quoteList(unmet))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Reopen returns a closed assignment to open.
func (r Rules) Reopen(id int, by string, note string) (Change, error) {
	if err := r.actor(by); err != nil {
		return Change{}, err
	}
	cur, ok := r.Set.Get(id)
	if !ok {
		return Change{}, refuse("no assignment %s; assignment_list shows what exists", Ref(id))
	}
	if cur.Open() {
		return Change{}, refuse("%s is open", cur.Ref())
	}
	if !r.canEditSpec(by, cur) {
		return Change{}, refuse("only %s's creator (%s) or the owner can reopen it; if you need the work redone, open a new assignment that says what is still missing", cur.Ref(), cur.Creator)
	}
	note = strings.TrimSpace(NormalizeText(note))
	if err := checkText("note", note, MaxNoteBytes); err != nil {
		return Change{}, refuse("%v: say what is wrong with the outcome; the assignee reads that first", err)
	}
	if cur.Parent != 0 {
		if p, ok := r.Set.Get(cur.Parent); ok && !p.Open() {
			return Change{}, refuse("%s is under %s, which is closed; reopen %s first", cur.Ref(), p.Ref(), p.Ref())
		}
	}
	iss := cur.clone()
	iss.Status = StatusOpen
	iss.Closed = nil
	iss.Resolution = ""
	iss.Outcome = ""
	iss.Updated = r.Now
	seq := r.Set.MaxSeq()
	entries := []Entry{{Seq: seq + 1, TS: r.Now, By: by, Op: OpReopened, Note: note}}
	if !r.active(cur.Assignee) {
		// The assignee was archived after the close. Reopening onto
		// them would wake nobody and leave an assignment nobody can act on,
		// so the reopener holds it and hands it on with assignment_update
		// (they are its creator or the CEO, so they may).
		iss.Assignee = by
		entries = append(entries, Entry{Seq: seq + 2, TS: r.Now, By: by, Op: OpAssigned, From: cur.Assignee, To: by, Note: cur.Assignee + " is no longer an active agent"})
	}
	iss.Log = append(iss.Log, entries...)
	after := r.Set.with(iss)
	ch := Change{Assignment: iss, Set: after, Entries: entries}
	var wakes []Wake
	if iss.Assignee != by {
		wakes = append(wakes, Wake{
			To: iss.Assignee, Op: OpReopened,
			Title: fmt.Sprintf("%s reopened: %s", iss.Ref(), iss.Title),
			Body: fmt.Sprintf("%s reopened %s %q: %s\nIts earlier outcome was: %s\n%s\n\n%s",
				by, iss.Ref(), iss.Title, note, cur.Outcome, describe(after, iss), spec(iss)),
		})
	}
	ch.Wakes = r.mergeFlips(wakes, r.Set, after, only(iss.ID), by, OpReopened, "", "")
	return ch, nil
}

// TransferCreator moves an open assignment's creatorship, which is what an
// offboard does with the assignments the departing agent filed: the creator
// is who is told when an assignment closes and who may amend, reassign or
// reopen it, and an archived agent can do none of that. CEO only.
func (r Rules) TransferCreator(id int, by, to, note string) (Change, error) {
	if !r.isCEO(by) {
		return Change{}, refuse("only the owner can move an assignment's creator")
	}
	cur, ok := r.Set.Get(id)
	if !ok {
		return Change{}, refuse("no assignment %s; assignment_list shows what exists", Ref(id))
	}
	if !cur.Open() {
		return Change{}, refuse("%s is closed; a closed assignment keeps its creator, and the owner can reopen it", cur.Ref())
	}
	to = strings.TrimSpace(to)
	if to == cur.Creator {
		return Change{}, refuse("%s was already opened by %s", cur.Ref(), to)
	}
	if err := r.actor(to); err != nil {
		return Change{}, refuse("creator %v", err)
	}
	note = strings.TrimSpace(NormalizeText(note))
	if len(note) > MaxNoteBytes {
		return Change{}, refuse("note is %d bytes, cap is %d", len(note), MaxNoteBytes)
	}
	iss := cur.clone()
	iss.Creator = to
	iss.Updated = r.Now
	e := Entry{Seq: r.Set.MaxSeq() + 1, TS: r.Now, By: by, Op: OpCreator, From: cur.Creator, To: to, Note: note}
	iss.Log = append(iss.Log, e)
	after := r.Set.with(iss)
	ch := Change{Assignment: iss, Set: after, Entries: []Entry{e}}
	var wakes []Wake
	if to != by {
		wakes = append(wakes, Wake{
			To: to, Op: OpCreator,
			Title: fmt.Sprintf("%s is now yours to follow: %s", iss.Ref(), iss.Title),
			Body: fmt.Sprintf("%s made you the creator of %s %q (opened by %s): %s\nYou are told when it closes, and only you or the owner can amend, reassign or reopen it.\n%s\n\n%s",
				by, iss.Ref(), iss.Title, cur.Creator, note, describe(after, iss), spec(iss)),
		})
	}
	ch.Wakes = r.mergeFlips(wakes, r.Set, after, only(iss.ID), by, OpCreator, "", "")
	return ch, nil
}

// mergeFlips adds a line for every assignment that became ready because of
// this change, to its assignee, folding it into that agent's primary
// wake when there is one. An assignment in covered is skipped when its
// assignee's primary wake already describes its state. detail, when
// set, is appended to a wake that carries nothing but flips (the
// outcome of a close, so the freed assignee reads why).
func (r Rules) mergeFlips(primary []Wake, before, after *Set, covered map[int]bool, by string, op Op, cause, detail string) []Wake {
	byTo := map[string]*Wake{}
	var order []string
	for i := range primary {
		w := &primary[i]
		byTo[w.To] = w
		order = append(order, w.To)
	}
	var flipped []*Assignment
	for _, id := range after.IDs() {
		if !after.Ready(id) || before.Ready(id) {
			continue
		}
		iss, _ := after.Get(id)
		if iss.Assignee == by {
			continue
		}
		if covered[id] && byTo[iss.Assignee] != nil {
			continue
		}
		flipped = append(flipped, iss)
	}
	for _, f := range flipped {
		line := fmt.Sprintf("%s %q is now ready", f.Ref(), f.Title)
		if cause != "" {
			line += ": " + cause
		}
		line += "."
		if w, ok := byTo[f.To()]; ok {
			w.Body += "\n\n" + line
			w.Flips = append(w.Flips, f.ID)
			continue
		}
		body := line
		if detail != "" {
			body += "\n\n" + detail
		}
		w := &Wake{To: f.Assignee, Op: op, Title: fmt.Sprintf("%s ready: %s", f.Ref(), f.Title), Body: body, Flips: []int{f.ID}}
		byTo[f.Assignee] = w
		order = append(order, f.Assignee)
	}
	out := make([]Wake, 0, len(order))
	for _, to := range order {
		out = append(out, *byTo[to])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].To < out[j].To })
	return out
}

// To is the assignee: the agent a change to this assignment wakes.
func (i *Assignment) To() string { return i.Assignee }

// describe is the state block of a wake: whether it is on hold, what
// the assignment waits on, what it is part of, what it holds up.
func describe(s *Set, iss *Assignment) string {
	var lines []string
	switch h := s.HeldBy(iss.ID); {
	case h == iss.ID:
		lines = append(lines, "It is on hold: do nothing on it until it is resumed, and you are woken when it is.")
	case h != 0:
		lines = append(lines, fmt.Sprintf("It is on hold under %s: do nothing on it until %s is resumed, and you are woken when it is.", Ref(h), Ref(h)))
	}
	if blockers := s.Blockers(iss.ID); len(blockers) > 0 {
		parts := make([]string, 0, len(blockers))
		for _, b := range blockers {
			t, _ := s.Get(b)
			rel := ""
			if t.Parent == iss.ID {
				rel = ", a part of it"
			}
			parts = append(parts, fmt.Sprintf("%s %q (assignee %s%s)", t.Ref(), t.Title, t.Assignee, rel))
		}
		lines = append(lines, "It is blocked by "+strings.Join(parts, ", ")+"; it becomes ready when they close.")
	} else if iss.Open() {
		lines = append(lines, "It is ready: nothing blocks it.")
	}
	if iss.Parent != 0 {
		if p, ok := s.Get(iss.Parent); ok {
			line := fmt.Sprintf("It is part of %s %q (assignee %s)", p.Ref(), p.Title, p.Assignee)
			if len(iss.Satisfies) > 0 {
				line += " and counts toward its Done-when condition" + plural(len(iss.Satisfies)) + " " + quoteList(iss.Satisfies)
			}
			lines = append(lines, line+".")
		}
	}
	if p, ok := s.Progress(iss.ID); ok {
		line := "Its Done-when conditions stand at " + p.String()
		if unclaimed := s.unclaimed(iss.ID); len(unclaimed) > 0 {
			line += "; unclaimed: " + quoteList(unclaimed)
		}
		lines = append(lines, line+".")
	}
	if blocks := s.Blocks(iss.ID); len(blocks) > 0 {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			t, _ := s.Get(b)
			parts = append(parts, fmt.Sprintf("%s %q (assignee %s)", t.Ref(), t.Title, t.Assignee))
		}
		lines = append(lines, "It holds up "+strings.Join(parts, ", ")+".")
	}
	return strings.Join(lines, "\n")
}

// spec is the description as quoted in a wake.
func spec(iss *Assignment) string {
	if strings.TrimSpace(iss.Body) == "" {
		return "(no description)"
	}
	return iss.Body
}

func indexOf(ids []int, id int) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return -1
}

func uniqueSlugs(slugs ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range slugs {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
