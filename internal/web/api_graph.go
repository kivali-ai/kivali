package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Graph's JSON: the knowledge graph as a compact list, grouped by
// owner (GET /api/v1/graph), and one node with its text
// (GET /api/v1/graph/nodes/{id}). Owners come in buildGraphPage's
// order. Every request answers from the graph maintainer's current
// index; none runs a pass.

// graphDefaultLimit is how many nodes each owner section shows until
// "Show all".
const graphDefaultLimit = 5

// wireAPIGraphRoutes registers Graph's routes on the API mux.
func (s *Server) wireAPIGraphRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/graph", s.handleAPIGraph)
	// Node ids are owner/name, so the id is the rest of the path.
	mux.HandleFunc("GET /api/v1/graph/nodes/{id...}", s.handleAPIGraphNode)
}

// apiGraphIndex returns the graph maintainer's current index, which
// every publish, org change and project-file change has already
// updated, so no load runs a pass: the page's open, every keystroke of
// a search, every "Show all" and every row opened read the same
// pointer.
func (s *Server) apiGraphIndex(w http.ResponseWriter) (*graph.Index, bool) {
	ix := s.Store.Graph().Index()
	if ix == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "the knowledge graph has not been indexed yet", whoServer)
		return nil, false
	}
	return ix, true
}

// graphListQuery is what GET /api/v1/graph filters and pages by.
type graphListQuery struct {
	q        string // lowercased; matches id or title
	flagged  bool
	problems bool
	// owner, when set, selects one owner's section ("" is Unowned)
	// and makes offset apply.
	owner  *string
	offset int
	limit  int
}

func parseGraphListQuery(r *http.Request) (graphListQuery, error) {
	v := r.URL.Query()
	gq := graphListQuery{
		q:        strings.ToLower(strings.TrimSpace(v.Get("q"))),
		flagged:  truthy(v.Get("flagged")),
		problems: truthy(v.Get("problems")),
		limit:    graphDefaultLimit,
	}
	if v.Has("owner") {
		o := strings.TrimSpace(v.Get("owner"))
		gq.owner = &o
	}
	if txt := v.Get("limit"); txt != "" {
		n, err := strconv.Atoi(txt)
		if err != nil || n < 1 {
			return gq, fmt.Errorf("limit must be a positive number")
		}
		gq.limit = n
	}
	if txt := v.Get("offset"); txt != "" {
		n, err := strconv.Atoi(txt)
		if err != nil || n < 0 {
			return gq, fmt.Errorf("offset must be zero or more")
		}
		gq.offset = n
	}
	return gq, nil
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// nodeProblem is a node's findings in words: its front matter
// rejection, then its problems. Empty when it has none.
func nodeProblem(n *graph.Node) string {
	var parts []string
	if n.Rejected != "" {
		parts = append(parts, "front matter rejected: "+n.Rejected)
	}
	parts = append(parts, n.Problems...)
	return strings.Join(parts, "; ")
}

// nodeTitle is what a row calls a node: its summary, or its path.
func nodeTitle(n *graph.Node) string {
	if n.Summary != "" {
		return n.Summary
	}
	return n.Path
}

func nodeKind(n *graph.Node) string {
	if n.Kind != "" {
		return string(n.Kind)
	}
	return string(n.Type)
}

func nodeRow(n *graph.Node) apitypes.NodeRow {
	row := apitypes.NodeRow{
		ID: n.ID, Kind: nodeKind(n), Type: string(n.Type), Title: nodeTitle(n),
		Status: string(n.Status), Flagged: n.Flagged,
	}
	if p := nodeProblem(n); p != "" {
		row.Problem = &p
	}
	if v := n.CurrentVersion(); v.N > 0 {
		at := v.TS
		row.Updated, row.Version = &at, v.N
	}
	return row
}

func (gq graphListQuery) matches(n *graph.Node) bool {
	if gq.flagged && !n.Flagged {
		return false
	}
	if gq.problems && nodeProblem(n) == "" {
		return false
	}
	if gq.q != "" && !strings.Contains(strings.ToLower(n.ID), gq.q) && !strings.Contains(strings.ToLower(nodeTitle(n)), gq.q) {
		return false
	}
	return true
}

// buildGraphList filters and pages the graph. Readouts count every
// artifact whatever the filters. An owner with no matching node is
// left out. Each owner shows its first limit matches; with an owner
// selected only that owner is returned, from offset.
func buildGraphList(ix *graph.Index, gq graphListQuery, person personFunc) apitypes.Graph {
	page := buildGraphPage(ix)
	out := apitypes.Graph{Owners: []apitypes.GraphOwner{}}
	var unowned *apitypes.GraphOwner
	for _, o := range page.Owners {
		for _, n := range o.Artifacts {
			out.Readouts.Nodes++
			if n.Flagged {
				out.Readouts.Flagged++
			}
			if nodeProblem(n) != "" {
				out.Readouts.Problems++
			}
		}
		slug := o.Node.ID
		if gq.owner != nil && *gq.owner != slug {
			continue
		}
		var matched []*graph.Node
		for _, n := range o.Artifacts {
			if gq.matches(n) {
				matched = append(matched, n)
			}
		}
		if len(matched) == 0 {
			continue
		}
		start := 0
		if gq.owner != nil {
			start = min(gq.offset, len(matched))
		}
		// Compared as a remainder, not start+limit, which overflows for
		// a limit near the int maximum and would slice out of range.
		end := len(matched)
		if gq.limit < end-start {
			end = start + gq.limit
		}
		section := apitypes.GraphOwner{
			Slug: slug, Name: graphOwnerName(o, person), Count: len(matched),
			Nodes: make([]apitypes.NodeRow, 0, end-start), HasMore: end < len(matched),
		}
		for _, n := range matched[start:end] {
			section.Nodes = append(section.Nodes, nodeRow(n))
		}
		if slug == "" {
			unowned = &section
			continue
		}
		out.Owners = append(out.Owners, section)
	}
	if unowned != nil {
		out.Owners = append(out.Owners, *unowned)
	}
	return out
}

// graphOwnerName names an owner section: "Unowned" for artifacts with
// no owner, the agent's display name (its role on the graph when it
// has left the team), or the bare slug for an owner not on the chart.
func graphOwnerName(o graphPageOwner, person personFunc) string {
	if o.Node.ID == "" {
		return "Unowned"
	}
	p := person(o.Node.ID)
	if p.Name == p.Slug && o.Node.Type == graph.TypeAgent && o.Node.Status != "unknown" && o.Node.Summary != "" {
		return o.Node.Summary
	}
	return p.Name
}

func (s *Server) handleAPIGraph(w http.ResponseWriter, r *http.Request) {
	gq, err := parseGraphListQuery(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), whoDevelopers)
		return
	}
	ix, ok := s.apiGraphIndex(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, buildGraphList(ix, gq, s.newHomeView().person))
}

// ---- GET /api/v1/graph/nodes/{id} ----

func graphRef(r graph.Ref) apitypes.GraphRef {
	return apitypes.GraphRef{ID: r.ID, Pin: r.Pin, Resolved: r.Resolved}
}

func graphRefs(rs []graph.Ref) []apitypes.GraphRef {
	out := make([]apitypes.GraphRef, 0, len(rs))
	for _, r := range rs {
		out = append(out, graphRef(r))
	}
	return out
}

func strs(list []string) []string {
	return append([]string{}, list...)
}

func graphVersion(n *graph.Node, v graph.Version) apitypes.GraphVersion {
	out := apitypes.GraphVersion{N: v.N, At: v.TS, SHA: v.SHA, PayloadSHA: v.PayloadSHA}
	// An agent's versions are snapshotted into the attachment store; a
	// project file's are the uploads, opened from the files page.
	if n.Owner != graph.CEOSlug {
		u := "/attachments/" + v.SHA
		out.URL = &u
	}
	return out
}

// graphNodeFields is every field the index holds for n.
func graphNodeFields(n *graph.Node, person personFunc) apitypes.GraphNodeFields {
	f := apitypes.GraphNodeFields{
		ID: n.ID, Type: string(n.Type), Kind: nodeKind(n), Owner: person(n.Owner), Path: n.Path,
		Status: string(n.Status), Condition: n.Condition, Check: n.Check, Source: n.Source,
		Summary: n.Summary, Payload: n.Payload, Manifested: n.Manifested, Rejected: n.Rejected,
		DependsOn: graphRefs(n.DependsOn), Supersedes: graphRefs(n.Supersedes),
		Evidence: strs(n.Evidence), Flagged: n.Flagged, Flags: strs(n.Flags), Problems: strs(n.Problems),
		Versions: make([]apitypes.GraphVersion, 0, len(n.Versions)),
		AboutMe:  strs(n.AboutMe), Dependents: strs(n.Dependents), SupersededBy: strs(n.SupersededBy),
		ReportsTo: n.ReportsTo,
	}
	if n.DeclaredStatus != "" && n.DeclaredStatus != n.Status {
		f.DeclaredStatus = string(n.DeclaredStatus)
	}
	if n.About != nil {
		r := graphRef(*n.About)
		f.About = &r
	}
	for _, v := range n.Versions {
		f.Versions = append(f.Versions, graphVersion(n, v))
	}
	return f
}

// graphNodeBody is a stored node file without its front matter: the
// fields already carry it, and rendered as markdown its closing `---`
// turns the manifest into a heading. A file whose fence is not a
// manifest (prose between two rules) or whose manifest was rejected is
// shown whole, so the owner sees what they wrote.
func graphNodeBody(text string) string {
	p, err := graph.ParseManifest([]byte(text))
	if err != nil {
		return text
	}
	return strings.TrimLeft(p.Body, "\r\n")
}

func (s *Server) handleAPIGraphNode(w http.ResponseWriter, r *http.Request) {
	ref := graph.ParseRef(r.PathValue("id"))
	ix, ok := s.apiGraphIndex(w)
	if !ok {
		return
	}
	n, found := ix.Get(ref.ID)
	if !found {
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("there is no node %s in the graph", ref.ID), whoNoOne)
		return
	}
	out := apitypes.GraphNode{Fields: graphNodeFields(n, s.newHomeView().person)}
	var v graph.Version
	switch {
	case ref.Pin != "":
		pinned, ok := n.Version(ref.Pin)
		if !ok {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("%s has no version %s", n.ID, ref.Pin), whoNoOne)
			return
		}
		v = pinned
	case len(n.Versions) > 0:
		v = n.CurrentVersion()
	default:
		writeJSON(w, http.StatusOK, out)
		return
	}
	gv := graphVersion(n, v)
	out.Version = &gv
	snap, err := s.Store.ReadGraphSnapshot(n, v)
	var note string
	switch {
	case errors.Is(err, store.ErrGraphSnapshotGone):
		note = "The text of this version is no longer stored. The version stays on record."
	case err != nil:
		note = "The text of this version could not be read."
	case snap.Binary:
		note = fmt.Sprintf("This version is a %d-byte file that is not text.", snap.Size)
	default:
		text := graphNodeBody(snap.Text)
		out.BodyMD = &text
	}
	if note != "" {
		out.BodyNote = &note
	}
	writeJSON(w, http.StatusOK, out)
}
