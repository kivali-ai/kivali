// Package graph is the knowledge graph's pure core: the front-matter
// schema agents write, the rules the index checks, and the build that
// turns a set of files plus the org chart into an Index with derived
// state (superseded, flagged, reverse edges, change sequence).
//
// Nothing here touches the filesystem or the store. The store walks
// the public artifact trees, snapshots versions, and feeds Build; the
// tools, the wake note and the CEO page read the Index it returns.
// Keeping the package pure is what makes the rules testable as a
// table and the build deterministic: same inputs, same Index, byte for
// byte.
//
// The design is docs/developers/knowledge-graph.md. Where this file and that
// document disagree, one of them is wrong and the fix lands in one
// commit.
package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// NodeType is one of the two node types. Everything authored or
// uploaded as a file is an artifact; a role, projected from the org
// chart, is an agent. See docs/developers/knowledge-graph.md §"Two node types"
// for why nothing else is a type.
type NodeType string

const (
	TypeArtifact NodeType = "artifact"
	TypeAgent    NodeType = "agent"
)

// Kind is the optional, strictly-listed label on an artifact. Each
// kind exists because a rule depends on it or a query filters on it;
// see Kinds. An artifact with no kind is authored primary material.
type Kind string

const (
	KindRequirement Kind = "requirement"
	KindDecision    Kind = "decision"
	KindCertificate Kind = "certificate"
	KindReference   Kind = "reference"
)

// Kinds is the allow list, in the order prompt surfaces enumerate it.
// A manifest naming any other kind is rejected. Adding a kind means
// adding a rule in checkKindRules and a line to the handbook.
var Kinds = []Kind{KindRequirement, KindDecision, KindCertificate, KindReference}

// KindDescriptions is what each kind means, one line each, for the
// tool descriptions and the handbook. Kept beside Kinds so a new
// kind cannot ship without its sentence.
var KindDescriptions = map[Kind]string{
	KindRequirement: "a constraint on an object, stated so an implementation can violate it; `about` required, optional `check`",
	KindDecision:    "a choice among alternatives about an object; `about` required",
	KindCertificate: "an attestation that a pinned version satisfies named requirements; `about` required, every `depends_on` pinned",
	KindReference:   "material captured from outside (a vendor document, a standard, a paper); `source` required",
}

// IsBindingKind reports whether nodes of this kind bind their object:
// the two kinds "what binds X" returns.
func (k Kind) IsBindingKind() bool {
	return k == KindRequirement || k == KindDecision
}

// ValidKind reports whether k is empty or on the allow list.
func ValidKind(k Kind) bool {
	if k == "" {
		return true
	}
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// Status is an artifact's lifecycle state. The owner declares one of
// the first four; the index derives the last, and Node.Flagged
// separately. An agent node's Status is "active" or "archived".
type Status string

const (
	StatusDraft       Status = "draft"       // not in force
	StatusProvisional Status = "provisional" // in force, conditional; Condition required
	StatusCurrent     Status = "current"     // in force, settled; the default
	StatusWithdrawn   Status = "withdrawn"
	StatusSuperseded  Status = "superseded" // derived: something supersedes this

	StatusActive   Status = "active"   // agents
	StatusArchived Status = "archived" // agents
)

// DeclaredStatuses is what an owner may write in front matter.
var DeclaredStatuses = []Status{StatusDraft, StatusProvisional, StatusCurrent, StatusWithdrawn}

// InForce reports whether an artifact with this effective status
// binds anything: provisional or current.
func (s Status) InForce() bool {
	return s == StatusProvisional || s == StatusCurrent
}

func validDeclaredStatus(s Status) bool {
	for _, known := range DeclaredStatuses {
		if s == known {
			return true
		}
	}
	return false
}

// Ref is a reference to a node: its id, optionally pinned to a
// version. Resolved is set by Build when the id names a node in the
// index (and, if pinned, when the pin names one of its versions).
type Ref struct {
	ID       string `json:"id"`
	Pin      string `json:"pin,omitempty"`
	Resolved bool   `json:"resolved"`
}

// ParseRef splits "owner/name@pin" into a Ref. The pin is everything
// after the last '@'; Build decides what it means (a version number,
// or a hex prefix of a version's SHA).
func ParseRef(s string) Ref {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "@"); i > 0 {
		return Ref{ID: s[:i], Pin: s[i+1:]}
	}
	return Ref{ID: s}
}

// String renders the reference as an agent would write it.
func (r Ref) String() string {
	if r.Pin == "" {
		return r.ID
	}
	return r.ID + "@" + r.Pin
}

// Version is one immutable snapshot of an artifact. N is 1-based and
// per node; SHA is the manifest's (or the bare file's) content hash;
// PayloadSHA is the `file:` payload's hash when the node has one.
// Pins name a version by N or by a prefix of either SHA.
type Version struct {
	N          int       `json:"n"`
	TS         time.Time `json:"ts"`
	SHA        string    `json:"sha"`
	PayloadSHA string    `json:"payload_sha,omitempty"`
}

// matchesPin reports whether pin names this version: its number, or a
// hex prefix of at least 7 characters of either SHA.
func (v Version) matchesPin(pin string) bool {
	if pin == fmt.Sprint(v.N) {
		return true
	}
	if len(pin) < 7 {
		return false
	}
	return strings.HasPrefix(v.SHA, pin) || (v.PayloadSHA != "" && strings.HasPrefix(v.PayloadSHA, pin))
}

// Node is one vertex of the graph as the index holds it.
//
// For an artifact: Path is relative to the owner's public directory
// (or, for a project file, the link name). Manifested is true when a
// front-matter manifest was present AND accepted; Rejected carries the
// reason when one was present and refused, in which case the node is
// a bare artifact with its kind and edges dropped. Status is the
// effective status (declared, or superseded when derived); Flagged
// and Flags are the retraction signal, carried down depends_on.
//
// For an agent: Path is empty, Owner is the Chief of Staff (or the
// CEO for itself), Status is active or archived, ReportsTo is the
// manager slug.
type Node struct {
	ID    string   `json:"id"`
	Type  NodeType `json:"type"`
	Owner string   `json:"owner"`
	Path  string   `json:"path,omitempty"`

	Manifested bool   `json:"manifested,omitempty"`
	Rejected   string `json:"rejected,omitempty"`

	Kind           Kind   `json:"kind,omitempty"`
	DeclaredStatus Status `json:"declared_status,omitempty"`
	Status         Status `json:"status"`
	Condition      string `json:"condition,omitempty"`
	Check          string `json:"check,omitempty"`
	Source         string `json:"source,omitempty"`
	Summary        string `json:"summary,omitempty"`
	Payload        string `json:"payload,omitempty"`
	// Lines is the manifest body's line count; the binding-kind
	// length problem is computed from it.
	Lines int `json:"lines,omitempty"`

	About      *Ref     `json:"about,omitempty"`
	DependsOn  []Ref    `json:"depends_on,omitempty"`
	Supersedes []Ref    `json:"supersedes,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`

	Flagged  bool     `json:"flagged,omitempty"`
	Flags    []string `json:"flags,omitempty"`
	Problems []string `json:"problems,omitempty"`

	Versions []Version `json:"versions,omitempty"`

	// ChangedSeq is the index sequence at which this node last
	// changed in any way an agent could care about: a new version, a
	// different effective status, flag, problem or edge. The wake
	// trailer lists nodes with ChangedSeq above the agent's watermark.
	ChangedSeq int64 `json:"changed_seq"`

	// Reverse edges, derived. Ids of the nodes that point here.
	AboutMe      []string `json:"about_me,omitempty"`
	Dependents   []string `json:"dependents,omitempty"`
	SupersededBy []string `json:"superseded_by,omitempty"`

	// Agent-only.
	ReportsTo string `json:"reports_to,omitempty"`

	fingerprint string
}

// CurrentVersion returns the newest version, or zero when the node
// has none (agents; a file the store has not versioned yet).
func (n *Node) CurrentVersion() Version {
	if len(n.Versions) == 0 {
		return Version{}
	}
	return n.Versions[len(n.Versions)-1]
}

// Version returns the version a pin names — its number, or a hex
// prefix of at least 7 characters of its manifest or payload SHA —
// and false when no version of this node matches. Build uses it to
// resolve pinned edges; graph_node uses it to serve a version's text.
func (n *Node) Version(pin string) (Version, bool) {
	for _, v := range n.Versions {
		if v.matchesPin(pin) {
			return v, true
		}
	}
	return Version{}, false
}

// Line is the node as one listing row: id, then kind or type, status
// with any flag, version, owner, subject, summary. The same row serves
// graph_query, the wake note and the CEO page, so an agent reads
// one shape everywhere.
func (n *Node) Line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  ", n.ID)
	if n.Type == TypeAgent {
		fmt.Fprintf(&b, "agent %s", n.Status)
		if n.ReportsTo != "" {
			fmt.Fprintf(&b, ", reports to %s", n.ReportsTo)
		}
		if n.Summary != "" {
			fmt.Fprintf(&b, " — %s", n.Summary)
		}
		return b.String()
	}
	if n.Kind != "" {
		b.WriteString(string(n.Kind))
	} else {
		b.WriteString("artifact")
	}
	fmt.Fprintf(&b, " %s", n.Status)
	if n.Flagged {
		b.WriteString(", flagged")
	}
	if n.Rejected != "" {
		b.WriteString(", front matter rejected")
	}
	if v := n.CurrentVersion(); v.N > 0 {
		fmt.Fprintf(&b, " v%d", v.N)
	}
	fmt.Fprintf(&b, " · owner %s", n.Owner)
	if n.About != nil {
		fmt.Fprintf(&b, " · about %s", n.About.String())
	}
	if n.Summary != "" {
		fmt.Fprintf(&b, " — %s", n.Summary)
	} else {
		fmt.Fprintf(&b, " — %s", n.Path)
	}
	return b.String()
}

// Index is one built graph. Nodes is keyed by id. Seq increases by
// one on every build that changed anything, so a per-agent watermark
// is just the Seq the agent last saw. Dropped lists, per owner, the
// public files that could not be indexed at all (an id collision that
// the derived name could not resolve) — a rare, owner-facing finding.
type Index struct {
	Seq     int64               `json:"seq"`
	BuiltAt time.Time           `json:"built_at"`
	Nodes   map[string]*Node    `json:"nodes"`
	Dropped map[string][]string `json:"dropped,omitempty"`
}

// Get returns the node with this id.
func (ix *Index) Get(id string) (*Node, bool) {
	if ix == nil || ix.Nodes == nil {
		return nil, false
	}
	n, ok := ix.Nodes[id]
	return n, ok
}

// Filter narrows Query. Zero values match everything. Status matches
// the effective status; InForce restricts to provisional or current
// (and wins over Status when both are set).
type Filter struct {
	Type    NodeType
	Owner   string
	About   string
	Kind    Kind
	Status  Status
	InForce bool
	// KindSet, when non-empty, matches any of the listed kinds and
	// wins over Kind. "What binds X" is KindSet{requirement, decision}.
	KindSet []Kind
}

// Query returns the nodes matching f, sorted by id.
func (ix *Index) Query(f Filter) []*Node {
	if ix == nil {
		return nil
	}
	var out []*Node
	for _, n := range ix.Nodes {
		if f.Type != "" && n.Type != f.Type {
			continue
		}
		if f.Owner != "" && n.Owner != f.Owner {
			continue
		}
		if f.About != "" && (n.About == nil || n.About.ID != f.About) {
			continue
		}
		if len(f.KindSet) > 0 {
			hit := false
			for _, k := range f.KindSet {
				if n.Kind == k {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		} else if f.Kind != "" && n.Kind != f.Kind {
			continue
		}
		if f.InForce {
			if n.Type != TypeArtifact || !n.Status.InForce() {
				continue
			}
		} else if f.Status != "" && n.Status != f.Status {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Sorted returns every node sorted by id.
func (ix *Index) Sorted() []*Node {
	return ix.Query(Filter{})
}

// ChangedSince returns the nodes whose ChangedSeq is above seq,
// sorted by id.
func (ix *Index) ChangedSince(seq int64) []*Node {
	if ix == nil {
		return nil
	}
	var out []*Node
	for _, n := range ix.Nodes {
		if n.ChangedSeq > seq {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RelevantTo reports whether a change to n is one slug should hear
// about on wake: n is slug itself or something slug owns, n is about
// something slug owns (or about slug), or one of slug's own artifacts
// depends on n. Everything else is a count in the trailer.
func (ix *Index) RelevantTo(slug string, n *Node) bool {
	if n.ID == slug || n.Owner == slug {
		return true
	}
	if n.About != nil {
		if n.About.ID == slug {
			return true
		}
		if target, ok := ix.Get(n.About.ID); ok && target.Owner == slug {
			return true
		}
	}
	for _, dep := range n.Dependents {
		if d, ok := ix.Get(dep); ok && d.Owner == slug {
			return true
		}
	}
	return false
}

// OwnerFindings returns everything the index wants slug to fix:
// rejections and problems on nodes slug owns, plus files of slug's
// that were dropped. One line each, sorted, stable.
func (ix *Index) OwnerFindings(slug string) []string {
	if ix == nil {
		return nil
	}
	var out []string
	for _, n := range ix.Nodes {
		if n.Owner != slug || n.Type != TypeArtifact {
			continue
		}
		if n.Rejected != "" {
			out = append(out, fmt.Sprintf("%s: front matter rejected: %s", n.Path, n.Rejected))
		}
		for _, p := range n.Problems {
			out = append(out, fmt.Sprintf("%s: %s", n.Path, p))
		}
	}
	out = append(out, ix.Dropped[slug]...)
	sort.Strings(out)
	return out
}

// fingerprintOf hashes everything about a node that a reader could
// notice changing. Two builds over unchanged inputs produce equal
// fingerprints, which is what keeps ChangedSeq still.
func fingerprintOf(n *Node) string {
	h := sha256.New()
	w := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	w(n.ID, string(n.Type), n.Owner, n.Path, string(n.Kind), string(n.DeclaredStatus), string(n.Status),
		n.Condition, n.Check, n.Source, n.Summary, n.Payload, n.Rejected, n.ReportsTo,
		fmt.Sprint(n.Manifested), fmt.Sprint(n.Flagged))
	if n.About != nil {
		w("about", n.About.String(), fmt.Sprint(n.About.Resolved))
	}
	for _, r := range n.DependsOn {
		w("dep", r.String(), fmt.Sprint(r.Resolved))
	}
	for _, r := range n.Supersedes {
		w("sup", r.String(), fmt.Sprint(r.Resolved))
	}
	// Group markers keep Evidence=[s],Flags=[] distinct from
	// Evidence=[],Flags=[s].
	w("evidence")
	w(n.Evidence...)
	w("flags")
	w(n.Flags...)
	w("problems")
	w(n.Problems...)
	if v := n.CurrentVersion(); v.N > 0 {
		w("v", fmt.Sprint(v.N), v.SHA, v.PayloadSHA)
	}
	return hex.EncodeToString(h.Sum(nil))
}
