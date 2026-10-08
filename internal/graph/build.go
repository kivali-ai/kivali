package graph

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// CEOSlug and ChiefOfStaffSlug are the two fixed agents. Declared here
// rather than imported so the package stays free of store and message
// dependencies; the values are the wire format and cannot drift.
const (
	CEOSlug          = "ceo"
	ChiefOfStaffSlug = "chief-of-staff"
)

// bindingKindMaxLines is the body length above which a requirement or
// decision draws a problem: substance belongs in a linked artifact,
// the node is the pointer.
const bindingKindMaxLines = 40

// AgentInput is one row of the org chart as Build wants it.
type AgentInput struct {
	Slug      string
	Role      string
	ReportsTo string
	Archived  bool
}

// FileInput is one public file as the store found it.
//
// Body is the file's bytes when Markdown is true; the store does not
// read anything else (a payload is hashed, never parsed). Facts, when
// set, stands in for Body: what ReadFacts made of the bytes, kept by a
// caller that does not hold every file in memory. Versions is
// the node's version list, newest last, already extended by the store
// for this pass. Project marks a CEO upload from the project-file
// store: no manifest, kind and summary supplied, source implied.
type FileInput struct {
	Owner    string
	Path     string
	Markdown bool
	Body     []byte
	Facts    *Facts
	Versions []Version

	// Name, when set, is the node name regardless of Path: for inputs
	// that carry no manifest but whose identity is not their path. A
	// project file's Path is the link name of its newest upload, which
	// gains a SHA prefix on re-upload; its Name comes from the first
	// upload so the node does not move.
	Name string

	Project bool
	Kind    Kind
	Summary string
	Source  string
}

// Input is everything one Build needs. Prev is the previous index
// (nil on the first pass) and is what ChangedSeq is computed against.
// SeqFloor is the highest sequence the store has evidence of outside
// the index (the version log records the sequence each version was cut
// at); when the index is lost, the sequence resumes above it rather
// than restarting at one under watermarks that still remember the old
// numbers.
type Input struct {
	Now      time.Time
	Prev     *Index
	SeqFloor int64
	Agents   []AgentInput
	Files    []FileInput
}

// fileState is Build's per-file working record.
type fileState struct {
	in        FileInput
	parsed    *Parsed // nil for bare files; may be shared, never written
	parseErr  string  // rejection reason from ParseManifest
	firstLine string  // the body's first line (Facts.FirstLine)
	lines     int     // the body's line count (Facts.Lines)
	name      string  // final node name (without owner)
	// pathNamed is true when name came from the path: no declared id
	// took effect and the store supplied no Name. Such an id does not
	// survive a rename, which matters once something points at it.
	pathNamed bool
	rejected  string // rejection reason, if any
	payload   string // resolved payload path, if any
	folded    bool   // this file is another node's payload
	// claimedBy is the manifest that folded this file as its payload;
	// if that manifest is later dropped, the payload is released and
	// becomes a node of its own again.
	claimedBy *fileState
	dropped   bool
}

// Build derives an Index from the org chart and the public files.
// Deterministic: inputs are sorted before any decision that could
// depend on order (id collisions, payload claims, cycle breaking).
func Build(in Input) *Index {
	ix := &Index{
		BuiltAt: in.Now,
		Nodes:   map[string]*Node{},
		Dropped: map[string][]string{},
	}
	buildAgents(ix, in.Agents)

	files := make([]*fileState, 0, len(in.Files))
	for i := range in.Files {
		files = append(files, &fileState{in: in.Files[i]})
	}
	sortFiles(files)
	parseAll(files)
	claimPayloads(files)
	assignNames(ix, files)
	for _, f := range files {
		if f.folded || f.dropped {
			continue
		}
		ix.Nodes[NodeID(f.in.Owner, f.name)] = buildArtifact(f)
	}

	resolveEdges(ix)
	breakCycles(ix)
	deriveStatus(ix)
	sortReverseEdges(ix)
	notePointedAtPathIds(ix, files)
	assignSeq(ix, in.Prev, in.SeqFloor)
	return ix
}

// notePointedAtPathIds attaches a problem to every node whose id is
// derived from its path and that some other node points at. The
// handbook asks owners to declare an id for anything a peer will
// point at; the owner of a bare release zip cannot know a peer pinned
// it, and the day they replace the file under a new name the pin
// dangles and the pinner is flagged for a retraction that never
// happened. The note lands on the target, whose owner is the one who
// can fix it, and only when the node is not already rejected: a
// rejection is a louder finding about the same file. Runs after
// sortReverseEdges so the pointer list is stable.
func notePointedAtPathIds(ix *Index, files []*fileState) {
	for _, f := range files {
		if f.folded || f.dropped || !f.pathNamed {
			continue
		}
		n := ix.Nodes[NodeID(f.in.Owner, f.name)]
		if n == nil || n.Rejected != "" {
			continue
		}
		var by []string
		by = append(by, n.AboutMe...)
		by = append(by, n.Dependents...)
		by = append(by, n.SupersededBy...)
		if len(by) == 0 {
			continue
		}
		sort.Strings(by)
		by = dedupeSorted(by)
		n.Problems = append(n.Problems, fmt.Sprintf("pointed at by %s; its id comes from its path, so renaming or replacing the file orphans that edge. Declare an id in front matter", strings.Join(by, ", ")))
	}
}

func dedupeSorted(list []string) []string {
	out := list[:0]
	for i, s := range list {
		if i == 0 || s != list[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// NameForPath is the node name a file gets when it declares no id:
// DeriveName for markdown (the extension dropped), the extension kept
// for anything else so `report.zip` and `report.md` beside each other
// stay distinct. Decided by extension, not by whether the file was
// read as markdown, so a markdown file too large to parse keeps the id
// it will have once it shrinks. The store uses the same function to
// key a bare file's versions before Build runs.
func NameForPath(relPath string) string {
	if strings.EqualFold(path.Ext(relPath), ".md") {
		return DeriveName(relPath)
	}
	return deriveNameKeepExt(relPath)
}

// sortFiles orders files by owner then path: the order every
// order-dependent decision in Plan and Build is made in.
func sortFiles(files []*fileState) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].in.Owner != files[j].in.Owner {
			return files[i].in.Owner < files[j].in.Owner
		}
		return files[i].in.Path < files[j].in.Path
	})
}

// ownerSummary is the ceo node's summary: the person, in the words the
// tools use for them (internal/owner).
const ownerSummary = "the owner: the person this team works for"

func buildAgents(ix *Index, agents []AgentInput) {
	seenCEO := false
	for _, a := range agents {
		slug := strings.TrimSpace(a.Slug)
		if slug == "" {
			continue
		}
		if slug == CEOSlug {
			seenCEO = true
		}
		n := &Node{
			ID:        slug,
			Type:      TypeAgent,
			Owner:     ChiefOfStaffSlug,
			Status:    StatusActive,
			Summary:   a.Role,
			ReportsTo: a.ReportsTo,
		}
		if slug == CEOSlug {
			n.Owner = CEOSlug
			n.Summary = ownerSummary
		}
		if a.Archived {
			n.Status = StatusArchived
		}
		ix.Nodes[slug] = n
	}
	if !seenCEO {
		ix.Nodes[CEOSlug] = &Node{ID: CEOSlug, Type: TypeAgent, Owner: CEOSlug, Status: StatusActive, Summary: ownerSummary}
	}
}

// parseAll reads front matter off every markdown file that has one.
// A file without a fence is a bare artifact; a fence that does not
// decode is a rejection.
func parseAll(files []*fileState) {
	for _, f := range files {
		if !f.in.Markdown || f.in.Project {
			continue
		}
		facts := f.in.Facts
		if facts == nil {
			read := ReadFacts(f.in.Body)
			facts = &read
		}
		f.parsed = facts.Parsed
		f.parseErr = facts.ParseErr
		f.firstLine = facts.FirstLine
		f.lines = facts.Lines
	}
}

// claimPayloads resolves every manifest's `file:` against the owner's
// file set. The payload is folded into its manifest's node and stops
// being a node of its own. First claim (by sorted manifest path) wins;
// a second manifest naming the same payload is rejected.
func claimPayloads(files []*fileState) {
	byPath := map[string]*fileState{} // owner+"\x00"+path
	for _, f := range files {
		byPath[f.in.Owner+"\x00"+f.in.Path] = f
	}
	claimed := map[string]string{} // owner+"\x00"+payload → manifest path
	for _, f := range files {
		if f.parsed == nil || f.parsed.Manifest.File == "" {
			continue
		}
		rel, ok := ResolvePayload(f.in.Path, f.parsed.Manifest.File)
		if !ok {
			f.rejected = fmt.Sprintf("file %q escapes the public directory", f.parsed.Manifest.File)
			continue
		}
		key := f.in.Owner + "\x00" + rel
		target, exists := byPath[key]
		switch {
		case !exists:
			f.rejected = fmt.Sprintf("file %q does not exist beside this manifest", f.parsed.Manifest.File)
		case target == f:
			f.rejected = "file names the manifest itself"
		case target.parsed != nil || target.parseErr != "":
			f.rejected = fmt.Sprintf("file %q is itself a node manifest", f.parsed.Manifest.File)
		case claimed[key] != "":
			f.rejected = fmt.Sprintf("file %q is already the payload of %s", f.parsed.Manifest.File, claimed[key])
		default:
			claimed[key] = f.in.Path
			target.folded = true
			target.claimedBy = f
			f.payload = rel
		}
	}
}

// assignNames gives every unfolded file its node name. A valid
// declared id wins and survives renames; otherwise the name is derived
// from the path. Collisions within one owner resolve in sorted path
// order: the first keeps the name, a later declared id falls back to
// its derived name with a rejection, and a file whose derived name is
// also taken is dropped with an owner-level finding.
func assignNames(ix *Index, files []*fileState) {
	taken := map[string]*fileState{} // owner+"\x00"+name
	for _, f := range files {
		if !f.folded {
			assignName(ix, taken, f)
		}
	}
	// A payload whose manifest was just dropped is a file again, not
	// anybody's payload; give it the name it would have had. Second
	// pass, in the same sorted order, so the outcome does not depend
	// on whether the payload sorted before or after its manifest.
	for _, f := range files {
		if f.folded && f.claimedBy != nil && f.claimedBy.dropped {
			f.claimedBy.payload = ""
			f.claimedBy = nil
			f.folded = false
			assignName(ix, taken, f)
		}
	}
}

// assignName decides one file's node name against the names taken so
// far, or drops it.
func assignName(ix *Index, taken map[string]*fileState, f *fileState) {
	name := NameForPath(f.in.Path)
	pathNamed := true
	if f.in.Name != "" {
		name = f.in.Name
		pathNamed = false
	}
	if f.parsed != nil && f.parsed.Manifest.ID != "" {
		declared := f.parsed.Manifest.ID
		switch {
		case !ValidName(declared):
			f.rejected = firstNonEmpty(f.rejected, fmt.Sprintf("id %q is not a slug path (lowercase letters, digits, . _ - and /)", declared))
		case taken[f.in.Owner+"\x00"+declared] != nil:
			f.rejected = firstNonEmpty(f.rejected, fmt.Sprintf("id %q is also declared by %s", declared, taken[f.in.Owner+"\x00"+declared].in.Path))
		default:
			name = declared
			pathNamed = false
		}
	}
	key := f.in.Owner + "\x00" + name
	if other := taken[key]; other != nil {
		f.dropped = true
		ix.Dropped[f.in.Owner] = append(ix.Dropped[f.in.Owner],
			fmt.Sprintf("%s: not indexed, its id %q is taken by %s", f.in.Path, name, other.in.Path))
		return
	}
	taken[key] = f
	f.name = name
	f.pathNamed = pathNamed
}

// deriveNameKeepExt is DeriveName for non-markdown files: the
// extension stays part of the name.
func deriveNameKeepExt(relPath string) string {
	return deriveName(relPath, false)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// buildArtifact turns one file into its node, applying the manifest
// when it was accepted and recording the rejection when it was not.
func buildArtifact(f *fileState) *Node {
	n := &Node{
		ID:       NodeID(f.in.Owner, f.name),
		Type:     TypeArtifact,
		Owner:    f.in.Owner,
		Path:     f.in.Path,
		Status:   StatusCurrent,
		Versions: f.in.Versions,
	}
	if f.in.Project {
		n.Kind = f.in.Kind
		n.Summary = f.in.Summary
		n.Source = f.in.Source
		if !ValidKind(n.Kind) {
			n.Problems = append(n.Problems, fmt.Sprintf("kind %q is not one of %s", n.Kind, kindList()))
			n.Kind = ""
		}
		n.DeclaredStatus = StatusCurrent
		return n
	}
	if f.parseErr != "" {
		n.Rejected = f.parseErr
		return n
	}
	if f.parsed == nil {
		return n // bare file
	}
	p := f.parsed
	n.Summary = firstNonEmpty(p.Manifest.Summary, f.firstLine)
	n.Lines = f.lines
	// A rejected manifest keeps its payload: the file has a face, the
	// face is broken, and hiding the bytes would make the rejection
	// harder to see, not easier to fix.
	n.Payload = f.payload
	if f.rejected == "" {
		f.rejected = checkManifest(p.Manifest)
	}
	if f.rejected != "" {
		n.Rejected = f.rejected
		return n
	}
	m := p.Manifest
	n.Manifested = true
	n.Kind = m.Kind
	n.DeclaredStatus = m.Status
	if n.DeclaredStatus == "" {
		n.DeclaredStatus = StatusCurrent
	}
	n.Status = n.DeclaredStatus
	n.Condition = m.Condition
	n.Check = m.Check
	n.Source = m.Source
	// Copied: the manifest may be a caller's cached Facts, shared by
	// every build, and a node must not alias it.
	n.Evidence = append([]string(nil), m.Evidence...)
	if m.About != "" {
		r := ParseRef(m.About)
		n.About = &r
	}
	for _, s := range m.DependsOn {
		n.DependsOn = append(n.DependsOn, ParseRef(s))
	}
	for _, s := range m.Supersedes {
		n.Supersedes = append(n.Supersedes, ParseRef(s))
	}
	for _, k := range p.UnknownFields {
		n.Problems = append(n.Problems, fmt.Sprintf("front matter field %q is not part of the schema and was ignored", k))
	}
	if n.Kind.IsBindingKind() && n.Lines > bindingKindMaxLines {
		n.Problems = append(n.Problems, fmt.Sprintf("a %s should be a few lines; this one is %d. Put the substance in a linked artifact", n.Kind, n.Lines))
	}
	return n
}

// checkManifest applies the base rules and the kind rules. It returns
// the rejection reason, or "" when the manifest is acceptable.
func checkManifest(m Manifest) string {
	if !ValidKind(m.Kind) {
		return fmt.Sprintf("kind %q is not one of %s", m.Kind, kindList())
	}
	if m.Status != "" && !validDeclaredStatus(m.Status) {
		return fmt.Sprintf("status %q is not one of %s", m.Status, statusList())
	}
	if m.Status == StatusProvisional && m.Condition == "" {
		return "status provisional requires a condition"
	}
	switch m.Kind {
	case KindRequirement, KindDecision:
		if m.About == "" {
			return fmt.Sprintf("a %s must say what it is about", m.Kind)
		}
	case KindCertificate:
		if m.About == "" {
			return "a certificate must say what it is about"
		}
		if len(m.DependsOn) == 0 {
			return "a certificate must depend on at least one pinned node"
		}
		for _, s := range m.DependsOn {
			if ParseRef(s).Pin == "" {
				return fmt.Sprintf("a certificate must pin every dependency to a version; %q is not pinned", s)
			}
		}
	case KindReference:
		if m.Source == "" {
			return "a reference must name its source"
		}
	}
	return ""
}

func kindList() string {
	parts := make([]string, len(Kinds))
	for i, k := range Kinds {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}

func statusList() string {
	parts := make([]string, len(DeclaredStatuses))
	for i, s := range DeclaredStatuses {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// resolveEdges marks every reference that names a node (and, when
// pinned, one of its versions) and adds the reverse edges. An
// unresolved about or supersedes is a problem; an unresolved
// depends_on is handled by deriveStatus as a flag, because a missing
// premise is a retraction, not a typo.
func resolveEdges(ix *Index) {
	for _, n := range ix.Sorted() {
		if n.Type != TypeArtifact || !n.Manifested {
			continue
		}
		if n.About != nil {
			// The subject is the node, whatever version: a bad pin is a
			// problem on this node, but "what is about X" and X's
			// about-me list must still agree that this node is about X.
			if target, ok := ix.Nodes[n.About.ID]; ok {
				target.AboutMe = append(target.AboutMe, n.ID)
				if pinOK(target, n.About.Pin) {
					n.About.Resolved = true
				} else {
					n.Problems = append(n.Problems, fmt.Sprintf("about %s: no such version", n.About.String()))
				}
			} else {
				n.Problems = append(n.Problems, fmt.Sprintf("about %s: no such node", n.About.String()))
			}
		}
		for i := range n.DependsOn {
			r := &n.DependsOn[i]
			target, ok := ix.Nodes[r.ID]
			if !ok {
				continue
			}
			if !pinOK(target, r.Pin) {
				n.Problems = append(n.Problems, fmt.Sprintf("depends_on %s: no such version", r.String()))
				continue
			}
			if target.Type != TypeArtifact {
				n.Problems = append(n.Problems, fmt.Sprintf("depends_on %s: an agent is not something to rest on; use about", r.ID))
				continue
			}
			r.Resolved = true
			target.Dependents = append(target.Dependents, n.ID)
		}
		for i := range n.Supersedes {
			r := &n.Supersedes[i]
			target, ok := ix.Nodes[r.ID]
			switch {
			case !ok:
				n.Problems = append(n.Problems, fmt.Sprintf("supersedes %s: no such node", r.ID))
			case target.Type != TypeArtifact:
				n.Problems = append(n.Problems, fmt.Sprintf("supersedes %s: an agent cannot be superseded", r.ID))
			case target.ID == n.ID:
				n.Problems = append(n.Problems, "supersedes itself")
			case !pinOK(target, r.Pin):
				n.Problems = append(n.Problems, fmt.Sprintf("supersedes %s: no such version", r.String()))
			default:
				r.Resolved = true
				if n.DeclaredStatus.InForce() {
					target.SupersededBy = append(target.SupersededBy, n.ID)
				}
			}
		}
	}
}

func pinOK(target *Node, pin string) bool {
	if pin == "" {
		return true
	}
	_, ok := target.Version(pin)
	return ok
}

// breakCycles removes dependency cycles among resolved depends_on
// edges. Nodes are visited in id order and edges in target order, so
// the same graph always loses the same edge: the one that closes the
// cycle last in that order. The edge is unresolved (not removed) and
// its source gets a problem.
func breakCycles(ix *Index) {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	ids := make([]string, 0, len(ix.Nodes))
	for id, n := range ix.Nodes {
		if n.Type == TypeArtifact {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var visit func(id string)
	visit = func(id string) {
		color[id] = grey
		n := ix.Nodes[id]
		order := make([]int, 0, len(n.DependsOn))
		for i := range n.DependsOn {
			if n.DependsOn[i].Resolved {
				order = append(order, i)
			}
		}
		sort.Slice(order, func(a, b int) bool { return n.DependsOn[order[a]].ID < n.DependsOn[order[b]].ID })
		for _, i := range order {
			r := &n.DependsOn[i]
			switch color[r.ID] {
			case grey:
				r.Resolved = false
				n.Problems = append(n.Problems, fmt.Sprintf("depends_on %s closes a dependency cycle and was ignored", r.ID))
				if t := ix.Nodes[r.ID]; t != nil {
					t.Dependents = removeString(t.Dependents, n.ID)
				}
			case white:
				visit(r.ID)
			}
		}
		color[id] = black
	}
	for _, id := range ids {
		if color[id] == white {
			visit(id)
		}
	}
}

func removeString(list []string, s string) []string {
	out := list[:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// deriveStatus sets the effective status and the flags. Superseded
// overrides whatever the owner declared. A node is flagged when a
// premise is missing, withdrawn, superseded, had its manifest
// rejected, or is itself flagged: a flag travels down depends_on
// through any number of hops, so a retraction reaches every node that
// rests on it however indirectly, and each flag names the premise one
// hop up. Only a resolved edge carries a flag onward; one breakCycles
// dropped is already a problem on its source. Flags only grow, so the
// pass ends.
func deriveStatus(ix *Index) {
	for _, n := range ix.Nodes {
		if n.Type == TypeArtifact && len(n.SupersededBy) > 0 {
			n.Status = StatusSuperseded
		}
	}
	for changed := true; changed; {
		changed = false
		for _, n := range ix.Sorted() {
			if n.Type != TypeArtifact || !n.Manifested {
				continue
			}
			for _, r := range n.DependsOn {
				flag := premiseFlag(ix, r)
				if flag == "" || hasString(n.Flags, flag) {
					continue
				}
				n.Flags = append(n.Flags, flag)
				n.Flagged = true
				changed = true
			}
		}
	}
}

// premiseFlag is the flag a depends_on edge puts on its source, or ""
// when the premise stands. A bad pin on a node that exists is a
// problem, not a flag, and does not carry the target's flag either.
func premiseFlag(ix *Index, r Ref) string {
	target, ok := ix.Nodes[r.ID]
	switch {
	case !ok:
		return fmt.Sprintf("rests on %s, which does not exist", r.ID)
	case target.Type != TypeArtifact:
		return ""
	case target.Rejected != "":
		return fmt.Sprintf("rests on %s, whose front matter was rejected", r.ID)
	case target.Status == StatusWithdrawn, target.Status == StatusSuperseded:
		return fmt.Sprintf("rests on %s, which is %s", r.ID, target.Status)
	case r.Resolved && target.Flagged:
		return fmt.Sprintf("rests on %s, which is flagged", r.ID)
	}
	return ""
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortReverseEdges(ix *Index) {
	for _, n := range ix.Nodes {
		sort.Strings(n.AboutMe)
		sort.Strings(n.Dependents)
		sort.Strings(n.SupersededBy)
		sort.Strings(n.Flags)
		sort.Strings(n.Problems)
	}
	for owner := range ix.Dropped {
		sort.Strings(ix.Dropped[owner])
	}
}

// assignSeq compares every node's fingerprint against the previous
// index and stamps ChangedSeq. The index sequence advances only when
// something changed, so an unchanged store re-indexed a hundred times
// stays at the same Seq and no trailer has anything to say.
// oldFingerprint is the fingerprint Build recorded on a previous
// index's node, or one computed now for a node read back from JSON.
// Nothing writes a node after Build returns it, so the recorded one
// is still its fingerprint.
func oldFingerprint(n *Node) string {
	if n.fingerprint != "" {
		return n.fingerprint
	}
	return fingerprintOf(n)
}

func assignSeq(ix *Index, prev *Index, floor int64) {
	prevSeq := floor
	if prev != nil && prev.Seq > prevSeq {
		prevSeq = prev.Seq
	}
	changed := false
	for id, n := range ix.Nodes {
		n.fingerprint = fingerprintOf(n)
		var old *Node
		if prev != nil {
			old = prev.Nodes[id]
		}
		// A previous index read back from JSON has no fingerprint and
		// gets one computed; one this package built kept its own.
		if old == nil || oldFingerprint(old) != n.fingerprint {
			changed = true
			n.ChangedSeq = prevSeq + 1
			continue
		}
		n.ChangedSeq = old.ChangedSeq
	}
	if prev != nil {
		for id := range prev.Nodes {
			if _, still := ix.Nodes[id]; !still {
				changed = true
				break
			}
		}
		// A file dropped for an id collision is not a node, but the
		// owner's finding about it is part of the index, and "nothing
		// changed" must not be reported when a new one appeared.
		if !droppedEqual(prev.Dropped, ix.Dropped) {
			changed = true
		}
	}
	ix.Seq = prevSeq
	if changed {
		ix.Seq = prevSeq + 1
	}
}

// droppedEqual compares two owner→findings maps. Both are sorted per
// owner by the time assignSeq runs (sortReverseEdges before, and the
// previous index was sorted when written).
func droppedEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for owner, lines := range a {
		other, ok := b[owner]
		if !ok || len(other) != len(lines) {
			return false
		}
		for i := range lines {
			if lines[i] != other[i] {
				return false
			}
		}
	}
	return true
}
