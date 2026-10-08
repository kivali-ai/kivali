// Package assignments is the assignment tracker's pure core: the
// record, its validation, the state a set of records implies, every
// rule about who may change what, and the wakes a change produces. It
// touches neither the filesystem nor the store; internal/store reads
// and writes the files and internal/tracker applies a change end to
// end. See docs/developers/assignments.md, which this package is built to.
package assignments

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Caps on the free-text fields. The body cap matches the message body
// cap: an assignment is a spec, and a spec that needs more than this is an
// artifact the body points at.
const (
	MaxTitleBytes   = 200
	MaxBodyBytes    = 4096
	MaxOutcomeBytes = 2048
	MaxNoteBytes    = 500
	// MaxItemBytes caps one acceptance item: a named deliverable is a
	// line, like a title.
	MaxItemBytes = 200
)

// CEO is the slug of the human. An assignment may be assigned to the CEO
// (that is how a question reaches them) and the CEO may do anything.
const CEO = "ceo"

// Status is the one stored state. Blocked and ready are derived by
// Set, never written.
type Status string

const (
	StatusOpen   Status = "open"
	StatusClosed Status = "closed"
)

// Resolution is how a closed assignment ended.
type Resolution string

const (
	ResolutionDone    Resolution = "done"
	ResolutionDropped Resolution = "dropped"
)

// Op names one kind of log entry. The fields each op fills are noted
// beside it; everything else on the entry is empty. The values are
// written to every log on disk.
type Op string

const (
	OpCreated   Op = "created"   // To: the first assignee
	OpAssigned  Op = "assigned"  // From, To, Note
	OpAmended   Op = "amended"   // Fields, Note, Prior
	OpParent    Op = "parent"    // Ref: the new parent, 0 when cleared
	OpBlocked   Op = "blocked"   // Ref: the blocker added
	OpUnblocked Op = "unblocked" // Ref: the blocker removed
	OpClosed    Op = "closed"    // Resolution, Note: the outcome
	OpReopened  Op = "reopened"  // Note
	OpCreator   Op = "creator"   // From, To, Note: the creatorship moved (an offboard)
	OpHeld      Op = "held"      // Note: put on hold; everything under it stops
	OpResumed   Op = "resumed"   // Note: the hold lifted
	// OpAcceptance and OpSatisfies record the list as it stands after
	// the change (Items), so the log reads as a sequence of states
	// rather than a diff to replay.
	OpAcceptance Op = "acceptance" // Items, Note: the Done-when conditions this assignment expects from its parts
	OpSatisfies  Op = "satisfies"  // Items, Note: which Done-when conditions of the assignment it is part of this one counts toward
)

var validOps = map[Op]bool{
	OpCreated: true, OpAssigned: true, OpAmended: true, OpParent: true,
	OpBlocked: true, OpUnblocked: true, OpClosed: true, OpReopened: true,
	OpCreator: true, OpHeld: true, OpResumed: true,
	OpAcceptance: true, OpSatisfies: true,
}

// NormalizeText is the one shape every free-text field is stored in:
// LF line endings and no trailing newline. Browsers submit a textarea
// with CRLF, agents send LF, and a record must compare equal to what
// its own form posts back, or an untouched field reads as an edit.
func NormalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimRight(s, "\n")
}

// Entry is one line of an assignment's log: who did what, when, and the
// note that explains it. Seq is global across every assignment, so "what
// happened since N" is one comparison.
type Entry struct {
	Seq        int64      `yaml:"seq"`
	TS         time.Time  `yaml:"ts"`
	By         string     `yaml:"by"`
	Op         Op         `yaml:"op"`
	From       string     `yaml:"from,omitempty"`
	To         string     `yaml:"to,omitempty"`
	Ref        int        `yaml:"ref,omitempty"`
	Fields     string     `yaml:"fields,omitempty"`
	Resolution Resolution `yaml:"resolution,omitempty"`
	Note       string     `yaml:"note,omitempty"`
	Prior      string     `yaml:"prior,omitempty"`
	// Items is the acceptance or satisfies list after an OpAcceptance
	// or OpSatisfies change.
	Items []string `yaml:"items,omitempty,flow"`
	// Unmet names the acceptance items still unsatisfied when the
	// assignment was closed as done: the warning, kept where it happened.
	Unmet []string `yaml:"unmet,omitempty,flow"`
}

// MarshalYAML writes each entry as one flow-style line so a log reads
// as a table under `cat`.
func (e Entry) MarshalYAML() (any, error) {
	type raw Entry
	var n yaml.Node
	if err := n.Encode(raw(e)); err != nil {
		return nil, err
	}
	n.Style = yaml.FlowStyle
	return &n, nil
}

// Assignment is the record. Front matter is every field but Body; Body
// is the markdown after the front matter, the description. The
// front-matter keys (parent, blocked_by, acceptance, satisfies and the
// rest) are what every file on disk carries.
type Assignment struct {
	ID        int    `yaml:"id"`
	Title     string `yaml:"title"`
	Status    Status `yaml:"status"`
	Assignee  string `yaml:"assignee"`
	Creator   string `yaml:"creator"`
	Parent    int    `yaml:"parent,omitempty"`
	BlockedBy []int  `yaml:"blocked_by,omitempty,flow"`
	// Held is the one stored pause: the creator or the CEO put this
	// assignment on hold, and it and everything under it wait until it is
	// resumed. Whether an assignment is on hold is derived (Set.Held),
	// because a hold on a parent covers its subtree.
	Held bool `yaml:"held,omitempty"`
	// Acceptance names what this assignment expects the assignments filed under
	// it to deliver, written independently of what has been filed.
	// Points down. Satisfies names which of the immediate parent's
	// acceptance items this assignment delivers; closing it as done is
	// what meets them. Points up. Both optional; an assignment with neither
	// is counted by its children, as before either existed. Whether
	// an item is satisfied, claimed or unclaimed is derived (Set.Items),
	// never stored.
	Acceptance []string `yaml:"acceptance,omitempty"`
	Satisfies  []string `yaml:"satisfies,omitempty,flow"`
	// Nudge is set when the assignment gains its first child while it has
	// no acceptance items, and cleared when the assignee's wake note
	// has said so once, or when acceptance items arrive. The nudge is
	// never repeated and is never a wake of its own.
	Nudge      bool       `yaml:"nudge,omitempty"`
	Created    time.Time  `yaml:"created"`
	Updated    time.Time  `yaml:"updated"`
	Closed     *time.Time `yaml:"closed,omitempty"`
	Resolution Resolution `yaml:"resolution,omitempty"`
	Outcome    string     `yaml:"outcome,omitempty"`
	Log        []Entry    `yaml:"log"`
	Body       string     `yaml:"-"`
}

// Ref renders an id the way every surface prints it.
func Ref(id int) string { return fmt.Sprintf("#%d", id) }

// Ref is the assignment's id as printed.
func (i *Assignment) Ref() string { return Ref(i.ID) }

// Open reports whether the assignment is open.
func (i *Assignment) Open() bool { return i.Status == StatusOpen }

// LastSeq is the sequence number of the newest log entry.
func (i *Assignment) LastSeq() int64 {
	if len(i.Log) == 0 {
		return 0
	}
	return i.Log[len(i.Log)-1].Seq
}

// clone copies the assignment deeply enough that mutating the copy leaves
// the original alone.
func (i *Assignment) clone() *Assignment {
	c := *i
	c.BlockedBy = append([]int(nil), i.BlockedBy...)
	c.Acceptance = append([]string(nil), i.Acceptance...)
	c.Satisfies = append([]string(nil), i.Satisfies...)
	c.Log = append([]Entry(nil), i.Log...)
	if i.Closed != nil {
		t := *i.Closed
		c.Closed = &t
	}
	return &c
}

// Refusal is a rule saying no. It is the model-facing text: it names
// the rule and, where there is one, the move to make instead. The
// service and the tools tell a Refusal from an internal error so the
// former is a tool-level error and the latter is a transport one.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) error {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a rule refusal.
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// frontMatterRE matches a YAML block fenced by `---` lines at the top
// of the file. CRLF tolerant, same shape as the graph's.
var frontMatterRE = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?(.*)\z`)

// Parse reads one assignment file. Unknown front-matter keys are an error:
// the files are written only by core, so a stray key is corruption,
// not an agent's typo to report gently.
func Parse(data []byte) (*Assignment, error) {
	m := frontMatterRE.FindSubmatch(data)
	if m == nil {
		return nil, errors.New("no front matter")
	}
	var iss Assignment
	dec := yaml.NewDecoder(strings.NewReader(string(m[1])))
	dec.KnownFields(true)
	if err := dec.Decode(&iss); err != nil {
		return nil, fmt.Errorf("front matter: %w", err)
	}
	body := string(m[2])
	// Marshal writes one blank line between the fence and the body and
	// one newline after it; strip exactly what it wrote. Bodies are
	// held without a trailing newline (the rules trim one), so a round
	// trip is stable in memory as well as on disk.
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimSuffix(body, "\n")
	iss.Body = body
	if err := Validate(&iss); err != nil {
		return nil, err
	}
	return &iss, nil
}

// Marshal writes the assignment as front matter plus body. The record is
// validated first: an invalid assignment never reaches disk.
func Marshal(iss *Assignment) ([]byte, error) {
	if err := Validate(iss); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(iss); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	b.WriteString("---\n")
	if iss.Body != "" {
		b.WriteString("\n")
		b.WriteString(iss.Body)
		if !strings.HasSuffix(iss.Body, "\n") {
			b.WriteString("\n")
		}
	}
	return []byte(b.String()), nil
}

// validSlug is the agent slug rule (lowercase, digits, hyphens) or the
// CEO. Kept local so this package stays a leaf.
func validSlug(s string) bool {
	if s == "" || strings.HasPrefix(s, "_") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// Validate checks one record in isolation: every field within its
// rule and the log consistent with the status. Relationships between
// assignments (parents that exist, cycles) are the Set's business.
func Validate(i *Assignment) error {
	if i.ID < 1 {
		return fmt.Errorf("assignment: id %d is not positive", i.ID)
	}
	if err := checkTitle(i.Title); err != nil {
		return fmt.Errorf("assignment %s: %w", i.Ref(), err)
	}
	if len(i.Body) > MaxBodyBytes {
		return fmt.Errorf("assignment %s: body is %d bytes, cap is %d", i.Ref(), len(i.Body), MaxBodyBytes)
	}
	if i.Status != StatusOpen && i.Status != StatusClosed {
		return fmt.Errorf("assignment %s: status %q is not open or closed", i.Ref(), i.Status)
	}
	if !validSlug(i.Assignee) {
		return fmt.Errorf("assignment %s: assignee %q is not a slug", i.Ref(), i.Assignee)
	}
	if !validSlug(i.Creator) {
		return fmt.Errorf("assignment %s: creator %q is not a slug", i.Ref(), i.Creator)
	}
	if i.Parent < 0 || i.Parent == i.ID {
		return fmt.Errorf("assignment %s: parent %d is not another assignment", i.Ref(), i.Parent)
	}
	seen := map[int]bool{}
	for _, b := range i.BlockedBy {
		if b < 1 || b == i.ID || seen[b] {
			return fmt.Errorf("assignment %s: blocked_by %d is not another distinct assignment", i.Ref(), b)
		}
		seen[b] = true
	}
	if err := checkItems("acceptance", i.Acceptance); err != nil {
		return fmt.Errorf("assignment %s: %w", i.Ref(), err)
	}
	if err := checkItems("satisfies", i.Satisfies); err != nil {
		return fmt.Errorf("assignment %s: %w", i.Ref(), err)
	}
	if len(i.Satisfies) > 0 && i.Parent == 0 {
		return fmt.Errorf("assignment %s: satisfies names Done-when conditions but it is part of nothing", i.Ref())
	}
	if i.Created.IsZero() {
		return fmt.Errorf("assignment %s: created is unset", i.Ref())
	}
	if i.Updated.Before(i.Created) {
		return fmt.Errorf("assignment %s: updated precedes created", i.Ref())
	}
	switch i.Status {
	case StatusClosed:
		if i.Closed == nil {
			return fmt.Errorf("assignment %s: closed without a closed time", i.Ref())
		}
		if i.Resolution != ResolutionDone && i.Resolution != ResolutionDropped {
			return fmt.Errorf("assignment %s: resolution %q is not done or dropped", i.Ref(), i.Resolution)
		}
		if err := checkText("outcome", i.Outcome, MaxOutcomeBytes); err != nil {
			return fmt.Errorf("assignment %s: %w", i.Ref(), err)
		}
	case StatusOpen:
		if i.Closed != nil || i.Resolution != "" || i.Outcome != "" {
			return fmt.Errorf("assignment %s: open but carries a closing", i.Ref())
		}
	}
	if i.Held && i.Status != StatusOpen {
		return fmt.Errorf("assignment %s: closed but on hold", i.Ref())
	}
	if len(i.Log) == 0 {
		return fmt.Errorf("assignment %s: empty log", i.Ref())
	}
	if i.Log[0].Op != OpCreated {
		return fmt.Errorf("assignment %s: log does not start with created", i.Ref())
	}
	var prev int64
	for n, e := range i.Log {
		if e.Seq <= prev {
			return fmt.Errorf("assignment %s: log entry %d seq %d is not after %d", i.Ref(), n, e.Seq, prev)
		}
		prev = e.Seq
		if e.TS.IsZero() {
			return fmt.Errorf("assignment %s: log entry %d has no timestamp", i.Ref(), n)
		}
		if !validSlug(e.By) {
			return fmt.Errorf("assignment %s: log entry %d by %q is not a slug", i.Ref(), n, e.By)
		}
		if !validOps[e.Op] {
			return fmt.Errorf("assignment %s: log entry %d op %q is unknown", i.Ref(), n, e.Op)
		}
		if len(e.Note) > MaxNoteBytes && e.Op != OpClosed {
			return fmt.Errorf("assignment %s: log entry %d note is %d bytes, cap is %d", i.Ref(), n, len(e.Note), MaxNoteBytes)
		}
	}
	last := i.Log[len(i.Log)-1].Op
	if (i.Status == StatusClosed) != (last == OpClosed) {
		return fmt.Errorf("assignment %s: status %s but the last log entry is %s", i.Ref(), i.Status, last)
	}
	return nil
}

func checkTitle(t string) error {
	if strings.TrimSpace(t) == "" {
		return errors.New("title is empty")
	}
	if strings.ContainsAny(t, "\r\n") {
		return errors.New("title spans more than one line")
	}
	if len(t) > MaxTitleBytes {
		return fmt.Errorf("title is %d bytes, cap is %d", len(t), MaxTitleBytes)
	}
	return nil
}

func checkText(name, s string, capBytes int) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if len(s) > capBytes {
		return fmt.Errorf("%s is %d bytes, cap is %d", name, len(s), capBytes)
	}
	return nil
}

// checkItems validates a stored item list: every name one trimmed
// non-empty line within the cap, and no name twice. Names are exact
// strings; "Step A" and "step A" are two items.
func checkItems(field string, items []string) error {
	seen := map[string]bool{}
	for n, it := range items {
		if it != strings.TrimSpace(it) || it == "" {
			return fmt.Errorf("%s item %d is empty or not trimmed", field, n)
		}
		if strings.ContainsAny(it, "\r\n") {
			return fmt.Errorf("%s item %q spans more than one line", field, it)
		}
		if len(it) > MaxItemBytes {
			return fmt.Errorf("%s item %q is %d bytes, cap is %d", field, it, len(it), MaxItemBytes)
		}
		if seen[it] {
			return fmt.Errorf("%s names %q twice", field, it)
		}
		seen[it] = true
	}
	return nil
}

// NormalizeItems is the shape an item list is stored in: each name
// trimmed, blank entries dropped, order kept. It does not check the
// caps or for duplicates; the rules do, with a refusal.
func NormalizeItems(items []string) []string {
	var out []string
	for _, it := range items {
		it = strings.TrimSpace(NormalizeText(it))
		if it == "" {
			continue
		}
		out = append(out, it)
	}
	return out
}

// sameItems reports whether two lists hold the same names in the
// same order.
func sameItems(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func indexOfItem(items []string, name string) int {
	for i, it := range items {
		if it == name {
			return i
		}
	}
	return -1
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Set is every assignment at once, with the state that only the whole
// collection can say: children, dependents, blocked and ready. It is
// immutable; a change builds a new one.
type Set struct {
	byID       map[int]*Assignment
	ids        []int
	children   map[int][]int
	dependents map[int][]int
}

// Build indexes a list of assignments.
func Build(list []*Assignment) *Set {
	s := &Set{byID: map[int]*Assignment{}, children: map[int][]int{}, dependents: map[int][]int{}}
	for _, iss := range list {
		s.byID[iss.ID] = iss
		s.ids = append(s.ids, iss.ID)
	}
	sort.Ints(s.ids)
	for _, id := range s.ids {
		iss := s.byID[id]
		if iss.Parent != 0 {
			s.children[iss.Parent] = append(s.children[iss.Parent], id)
		}
		for _, b := range iss.BlockedBy {
			s.dependents[b] = append(s.dependents[b], id)
		}
	}
	return s
}

// with returns a Set with iss added or replaced.
func (s *Set) with(iss *Assignment) *Set {
	list := make([]*Assignment, 0, len(s.ids)+1)
	replaced := false
	for _, id := range s.ids {
		if id == iss.ID {
			list = append(list, iss)
			replaced = true
			continue
		}
		list = append(list, s.byID[id])
	}
	if !replaced {
		list = append(list, iss)
	}
	return Build(list)
}

// Get returns one assignment.
func (s *Set) Get(id int) (*Assignment, bool) {
	iss, ok := s.byID[id]
	return iss, ok
}

// IDs lists every id, ascending.
func (s *Set) IDs() []int { return append([]int(nil), s.ids...) }

// Len is the number of assignments.
func (s *Set) Len() int { return len(s.ids) }

// Parts lists the assignments that are part of id, ascending.
func (s *Set) Parts(id int) []int { return append([]int(nil), s.children[id]...) }

// OpenParts lists the open assignments that are part of id.
func (s *Set) OpenParts(id int) []int {
	var out []int
	for _, c := range s.children[id] {
		if s.byID[c].Open() {
			out = append(out, c)
		}
	}
	return out
}

// Blockers lists what an open assignment waits on right now: its open
// blocked_by targets and its open children, ascending. An unknown
// blocked_by target counts as satisfied; the file it named is gone.
func (s *Set) Blockers(id int) []int {
	iss, ok := s.byID[id]
	if !ok {
		return nil
	}
	set := map[int]bool{}
	for _, b := range iss.BlockedBy {
		if t, ok := s.byID[b]; ok && t.Open() {
			set[b] = true
		}
	}
	for _, c := range s.OpenParts(id) {
		set[c] = true
	}
	out := make([]int, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	sort.Ints(out)
	return out
}

// Blocked reports whether an open assignment waits on anything.
func (s *Set) Blocked(id int) bool { return len(s.Blockers(id)) > 0 }

// HeldBy is the assignment whose hold covers id: id itself when it is held,
// else the nearest held ancestor, else 0. A closed assignment is never on
// hold. A hold travels down the parent edge only: a child is part of
// its parent's work, so pausing the parent pauses the part; an assignment
// in another tree that waits on a held assignment was waiting already.
func (s *Set) HeldBy(id int) int {
	iss, ok := s.byID[id]
	if !ok || !iss.Open() {
		return 0
	}
	if iss.Held {
		return id
	}
	for _, a := range s.Ancestors(id) {
		if p := s.byID[a]; p.Held && p.Open() {
			return a
		}
	}
	return 0
}

// Held reports whether an open assignment is on hold, itself or through an
// ancestor.
func (s *Set) Held(id int) bool { return s.HeldBy(id) != 0 }

// Ready reports whether an assignment is open, on hold nowhere, and waits
// on nothing.
func (s *Set) Ready(id int) bool {
	iss, ok := s.byID[id]
	return ok && iss.Open() && !s.Held(id) && !s.Blocked(id)
}

// OpenDescendants lists every open assignment under id at any depth,
// ascending. A closed assignment has no open descendants (a close is
// refused while a child is open, a reopen while the parent is
// closed), so the walk follows open children only.
func (s *Set) OpenDescendants(id int) []int {
	var out []int
	stack := s.OpenParts(id)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		out = append(out, n)
		stack = append(stack, s.OpenParts(n)...)
	}
	sort.Ints(out)
	return out
}

// ConditionState is where one Done-when condition stands.
type ConditionState string

const (
	ConditionMet       ConditionState = "met"       // a part naming it closed as done
	ConditionClaimed   ConditionState = "claimed"   // an open part names it
	ConditionUnclaimed ConditionState = "unclaimed" // nothing opened names it: the work nobody has noticed
)

// ConditionStatus is one Done-when condition of an assignment with the state its
// children imply. Nothing here is stored: a dropped child satisfies
// nothing, reopening a done child un-satisfies what it met, and when
// several children name one item the first done close satisfies it.
type ConditionStatus struct {
	Name        string
	SatisfiedBy []int // children closed as done that name it, ascending
	ClaimedBy   []int // open children that name it, ascending
}

// State is the one word the tables print: satisfied outranks claimed,
// which outranks unclaimed.
func (it ConditionStatus) State() ConditionState {
	switch {
	case len(it.SatisfiedBy) > 0:
		return ConditionMet
	case len(it.ClaimedBy) > 0:
		return ConditionClaimed
	}
	return ConditionUnclaimed
}

// Items lists id's acceptance items in declared order, each with the
// children that satisfy or claim it. Nil when the assignment declares none.
// A child's satisfies may only name its parent's items, so a name a
// child carries that the parent no longer declares matches nothing.
func (s *Set) Items(id int) []ConditionStatus {
	iss, ok := s.byID[id]
	if !ok || len(iss.Acceptance) == 0 {
		return nil
	}
	out := make([]ConditionStatus, len(iss.Acceptance))
	for i, name := range iss.Acceptance {
		out[i].Name = name
	}
	for _, c := range s.children[id] {
		child := s.byID[c]
		for _, name := range child.Satisfies {
			i := indexOfItem(iss.Acceptance, name)
			if i < 0 {
				continue
			}
			switch {
			case child.Open():
				out[i].ClaimedBy = append(out[i].ClaimedBy, c)
			case child.Resolution == ResolutionDone:
				out[i].SatisfiedBy = append(out[i].SatisfiedBy, c)
			}
		}
	}
	return out
}

// Progress is how a parent with acceptance items is summarised
// wherever the tracker summarises a parent: items met, items an open
// child has claimed, items nobody has claimed. It replaces the child
// counts, which said only how much of the filed work was done.
type Progress struct {
	Satisfied, Claimed, Unclaimed int
}

// Total is the number of items.
func (p Progress) Total() int { return p.Satisfied + p.Claimed + p.Unclaimed }

// String is the three counts as every surface prints them.
func (p Progress) String() string {
	return fmt.Sprintf("%d met / %d claimed-open / %d unclaimed", p.Satisfied, p.Claimed, p.Unclaimed)
}

// Progress counts id's items by state. ok is false when the assignment
// declares no items, in which case the caller falls back to counting
// children.
func (s *Set) Progress(id int) (p Progress, ok bool) {
	items := s.Items(id)
	if len(items) == 0 {
		return Progress{}, false
	}
	for _, it := range items {
		switch it.State() {
		case ConditionMet:
			p.Satisfied++
		case ConditionClaimed:
			p.Claimed++
		default:
			p.Unclaimed++
		}
	}
	return p, true
}

// Unmet lists id's acceptance items not yet satisfied, in declared
// order: what closing it as done would leave behind.
func (s *Set) Unmet(id int) []string {
	var out []string
	for _, it := range s.Items(id) {
		if it.State() != ConditionMet {
			out = append(out, it.Name)
		}
	}
	return out
}

// unclaimed lists id's acceptance items nothing filed names, in
// declared order.
func (s *Set) unclaimed(id int) []string {
	var out []string
	for _, it := range s.Items(id) {
		if it.State() == ConditionUnclaimed {
			out = append(out, it.Name)
		}
	}
	return out
}

// Nudges lists the open assignments assigned to slug whose assignee has
// yet to be told that they have children and no acceptance items,
// ascending. The tracker clears the flag once the line has said so.
func (s *Set) Nudges(slug string) []int {
	var out []int
	for _, id := range s.ids {
		iss := s.byID[id]
		if iss.Nudge && iss.Open() && iss.Assignee == slug && len(iss.Acceptance) == 0 && len(s.children[id]) > 0 {
			out = append(out, id)
		}
	}
	return out
}

// NudgeLine is the one sentence the wake note's assignments section
// carries, once, for an assignment that gained parts and has no
// Done-when conditions.
func NudgeLine(id int) string {
	return fmt.Sprintf("%s now has parts and no Done-when conditions.", Ref(id))
}

// Blocks lists the open assignments that wait on id: the open assignments naming
// it in blocked_by, and its parent when id is open. Ascending.
func (s *Set) Blocks(id int) []int {
	iss, ok := s.byID[id]
	if !ok {
		return nil
	}
	set := map[int]bool{}
	for _, d := range s.dependents[id] {
		if s.byID[d].Open() {
			set[d] = true
		}
	}
	if iss.Open() && iss.Parent != 0 {
		if p, ok := s.byID[iss.Parent]; ok && p.Open() {
			set[iss.Parent] = true
		}
	}
	out := make([]int, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

// Ancestors lists the parent chain from the nearest parent up.
func (s *Set) Ancestors(id int) []int {
	var out []int
	seen := map[int]bool{id: true}
	for {
		iss, ok := s.byID[id]
		if !ok || iss.Parent == 0 || seen[iss.Parent] {
			return out
		}
		out = append(out, iss.Parent)
		seen[iss.Parent] = true
		id = iss.Parent
	}
}

// waitsOn reports whether b is reachable from a along the edges an
// assignment waits on: its blocked_by targets and its children, closed or
// not. Closed assignments count because a reopen must not create a cycle.
func (s *Set) waitsOn(a, b int) bool {
	seen := map[int]bool{}
	stack := []int{a}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == b {
			return true
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		if iss, ok := s.byID[n]; ok {
			stack = append(stack, iss.BlockedBy...)
		}
		stack = append(stack, s.children[n]...)
	}
	return false
}

// MaxID is the highest id in the set, 0 when empty.
func (s *Set) MaxID() int {
	if len(s.ids) == 0 {
		return 0
	}
	return s.ids[len(s.ids)-1]
}

// MaxSeq is the highest log sequence number in the set.
func (s *Set) MaxSeq() int64 {
	var max int64
	for _, id := range s.ids {
		for _, e := range s.byID[id].Log {
			if e.Seq > max {
				max = e.Seq
			}
		}
	}
	return max
}

// Filter narrows a listing. Empty fields match everything; Status
// "all" matches both open and closed. Parent -1 means top-level only.
type Filter struct {
	Assignee string
	Creator  string
	Parent   int
	Status   string
	Ready    *bool
}

// Query lists the assignments matching f, ascending by id.
func (s *Set) Query(f Filter) []*Assignment {
	var out []*Assignment
	for _, id := range s.ids {
		iss := s.byID[id]
		if f.Assignee != "" && iss.Assignee != f.Assignee {
			continue
		}
		if f.Creator != "" && iss.Creator != f.Creator {
			continue
		}
		if f.Parent > 0 && iss.Parent != f.Parent {
			continue
		}
		if f.Parent < 0 && iss.Parent != 0 {
			continue
		}
		switch f.Status {
		case "", string(StatusOpen):
			if !iss.Open() {
				continue
			}
		case string(StatusClosed):
			if iss.Open() {
				continue
			}
		}
		if f.Ready != nil && s.Ready(id) != *f.Ready {
			continue
		}
		out = append(out, iss)
	}
	return out
}

// Line is the one-line form every listing prints: id, derived state,
// title, who holds it, and what it waits on.
func (s *Set) Line(iss *Assignment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", iss.Ref(), s.State(iss.ID))
	fmt.Fprintf(&b, " %q", iss.Title)
	fmt.Fprintf(&b, " — assignee %s, creator %s", iss.Assignee, iss.Creator)
	if iss.Parent != 0 {
		fmt.Fprintf(&b, ", part of %s", Ref(iss.Parent))
	}
	if len(iss.Satisfies) > 0 {
		fmt.Fprintf(&b, ", counts toward %s", quoteList(iss.Satisfies))
	}
	if p, ok := s.Progress(iss.ID); ok {
		fmt.Fprintf(&b, ", done when %s", p)
	} else if n := len(s.children[iss.ID]); n > 0 {
		open := len(s.OpenParts(iss.ID))
		fmt.Fprintf(&b, ", %d part%s (%d open)", n, pluralParts(n), open)
	}
	if blocks := s.Blocks(iss.ID); len(blocks) > 0 {
		fmt.Fprintf(&b, "; blocks %s", refList(blocks))
	}
	return b.String()
}

// quoteList renders item names the way every surface prints them.
func quoteList(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = fmt.Sprintf("%q", it)
	}
	return strings.Join(parts, ", ")
}

// State is the derived state word for an assignment: ready, "on hold" (or
// "on hold under #n" when the hold is an ancestor's), "blocked by #a,
// #b", or closed (done|dropped). A hold outranks a block: nothing is
// to be done on the assignment either way, and the hold is the reason.
func (s *Set) State(id int) string {
	iss, ok := s.byID[id]
	if !ok {
		return "unknown"
	}
	if !iss.Open() {
		return fmt.Sprintf("closed (%s)", iss.Resolution)
	}
	switch h := s.HeldBy(id); {
	case h == id:
		return "on hold"
	case h != 0:
		return "on hold under " + Ref(h)
	}
	if blockers := s.Blockers(id); len(blockers) > 0 {
		return "blocked by " + refList(blockers)
	}
	return "ready"
}

func refList(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = Ref(id)
	}
	return strings.Join(parts, ", ")
}

func pluralParts(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// OpenAssignments is the wake note's section on what the agent holds right
// now: each open assignment by id and title, one per line, marked ready, on
// hold, or blocked by which ids, and, on a parent with acceptance
// items, the three counts. It ends with the nudge for any assignment of
// theirs that gained children and has no items (Nudges); the caller
// clears those flags once the note has landed. Empty when the agent
// holds nothing, so the note carries no section; an assignment event
// that closed or moved the agent's last assignment is itself the news.
func (s *Set) OpenAssignments(slug string) string {
	mine := s.Query(Filter{Assignee: slug})
	if len(mine) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Your open assignments:")
	for _, iss := range mine {
		state := s.State(iss.ID)
		if p, ok := s.Progress(iss.ID); ok {
			state += "; done when " + p.String()
		}
		fmt.Fprintf(&b, "\n- %s %q (%s)", iss.Ref(), iss.Title, state)
	}
	// Its own paragraph, so a markdown reader does not fold it into
	// the last item.
	for i, id := range s.Nudges(slug) {
		if i == 0 {
			b.WriteString("\n\n")
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(NudgeLine(id))
	}
	return b.String()
}
