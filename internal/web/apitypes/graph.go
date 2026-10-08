package apitypes

import "time"

// ---- /api/v1/graph ----

// Graph is GET /api/v1/graph?q=&flagged=1&problems=1&owner=&offset=&limit=:
// the knowledge graph as a compact list, artifacts grouped by owner.
type Graph struct {
	Readouts GraphReadouts `json:"readouts"`
	// Owners are the owners with at least one matching node: you
	// first, then active agents by slug, then archived agents, then
	// owners no longer on the team, then Unowned (slug "").
	Owners []GraphOwner `json:"owners"`
	// Stale is always false: the graph maintainer's index is current
	// when it is served, and a load runs no pass that could fail. The
	// page still reads it.
	Stale bool `json:"stale"`
}

// GraphReadouts count the whole graph, whatever the filters.
type GraphReadouts struct {
	// Nodes is the number of artifacts (agents are the owners, not
	// nodes on the list).
	Nodes int `json:"nodes"`
	// Flagged is how many artifacts rest on something withdrawn,
	// superseded, rejected, missing or itself flagged.
	Flagged int `json:"flagged"`
	// Problems is how many artifacts carry an index finding: front
	// matter that was rejected, or a problem with one of its edges or
	// its length.
	Problems int `json:"problems"`
}

// GraphOwner is one owner's section.
type GraphOwner struct {
	// Slug is "" for Unowned.
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Count is how many of the owner's nodes match.
	Count int `json:"count"`
	// Nodes is one page of them, by id.
	Nodes   []NodeRow `json:"nodes"`
	HasMore bool      `json:"has_more"`
}

// NodeRow is one node as a list row.
type NodeRow struct {
	ID string `json:"id"`
	// Kind is requirement, decision, certificate or reference, or
	// "artifact" for authored material with no kind.
	Kind string `json:"kind"`
	Type string `json:"type"`
	// Title is the node's summary, or its path when it has none.
	Title   string `json:"title"`
	Status  string `json:"status"`
	Flagged bool   `json:"flagged"`
	// Problem is the node's findings in words, when it has any.
	Problem *string `json:"problem,omitempty"`
	// Updated is when its current version was recorded; absent when
	// it has none yet.
	Updated *time.Time `json:"updated,omitempty"`
	// Version is its current version number, 0 when it has none.
	Version int `json:"version"`
}

// ---- /api/v1/graph/nodes/{id} ----

// GraphNode is GET /api/v1/graph/nodes/{id}: every field the node
// carries, and its text. The id may pin a version, owner/name@N (or
// @<sha prefix>); the text is then that version's.
type GraphNode struct {
	Fields GraphNodeFields `json:"fields"`
	// Version is the version whose text BodyMD is: the pinned one, or
	// the current one. Absent for an agent or a node with no versions.
	Version *GraphVersion `json:"version,omitempty"`
	// BodyMD is the file as it was at Version, front matter and all.
	// Absent when there is no text to show; BodyNote then says why.
	BodyMD   *string `json:"body_md,omitempty"`
	BodyNote *string `json:"body_note,omitempty"`
}

// GraphNodeFields is a node as the index holds it.
type GraphNodeFields struct {
	ID    string    `json:"id"`
	Type  string    `json:"type"`
	Kind  string    `json:"kind"`
	Owner PersonRef `json:"owner"`
	// Path is the file under the owner's public directory, or the
	// project file's name; empty for an agent.
	Path   string `json:"path"`
	Status string `json:"status"`
	// DeclaredStatus is what the owner wrote, when it differs from
	// the effective status (a superseded node).
	DeclaredStatus string `json:"declared_status"`
	Condition      string `json:"condition"`
	Check          string `json:"check"`
	Source         string `json:"source"`
	Summary        string `json:"summary"`
	Payload        string `json:"payload"`
	Manifested     bool   `json:"manifested"`
	// Rejected is why the front matter was refused, when it was.
	Rejected     string         `json:"rejected"`
	About        *GraphRef      `json:"about,omitempty"`
	DependsOn    []GraphRef     `json:"depends_on"`
	Supersedes   []GraphRef     `json:"supersedes"`
	Evidence     []string       `json:"evidence"`
	Flagged      bool           `json:"flagged"`
	Flags        []string       `json:"flags"`
	Problems     []string       `json:"problems"`
	Versions     []GraphVersion `json:"versions"`
	AboutMe      []string       `json:"about_me"`
	Dependents   []string       `json:"dependents"`
	SupersededBy []string       `json:"superseded_by"`
	// ReportsTo is an agent node's manager.
	ReportsTo string `json:"reports_to"`
}

// GraphRef is an edge to another node, optionally pinned to one of
// its versions. Resolved is false when the target or pin does not
// exist.
type GraphRef struct {
	ID       string `json:"id"`
	Pin      string `json:"pin"`
	Resolved bool   `json:"resolved"`
}

// GraphVersion is one recorded version of an artifact. URL opens its
// stored bytes; absent for your project files, which are opened from
// the files page.
type GraphVersion struct {
	N          int       `json:"n"`
	At         time.Time `json:"at"`
	SHA        string    `json:"sha"`
	PayloadSHA string    `json:"payload_sha"`
	URL        *string   `json:"url,omitempty"`
}
