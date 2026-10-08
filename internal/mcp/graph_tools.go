package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/store"
)

// Graph tool names, shared with the native-API definitions in
// internal/agent/graph_tools.go.
const (
	GraphQueryToolName = agent.GraphQueryToolName
	GraphNodeToolName  = agent.GraphNodeToolName
)

// graphQueryDefaultLimit caps a listing nobody narrowed. A whole-org
// graph is hundreds of rows; the agent can filter.
const graphQueryDefaultLimit = 50

// GraphTools returns the two read-only graph tools dispatched through
// d. Both the full-agent and the subagent toolkits include them: a
// subagent doing focused work needs to find what binds the object it
// was handed as much as its parent does.
func GraphTools(d StateDispatcher) []Tool {
	if d == nil {
		return nil
	}
	return []Tool{
		readOnlyStateTool(d, GraphQueryToolName, agent.GraphQueryDescription, agent.GraphQueryInputSchema),
		readOnlyStateTool(d, GraphNodeToolName, agent.GraphNodeDescription, agent.GraphNodeInputSchema),
	}
}

// graphIndexFor is the graph maintainer's current index; nil when
// there is none at all. It runs no pass: every publish and unpublish
// updates the index before it returns, so a caller's own publishes are
// already in it.
func graphIndexFor(s *store.FSStore) *graph.Index {
	return s.Graph().Index()
}

// lookupNode finds a node by id, forgiving what an agent types from a
// directory listing: ids are lowercase and drop a markdown extension,
// the file names agents see are neither.
func lookupNode(ix *graph.Index, id string) (*graph.Node, bool) {
	if n, ok := ix.Get(id); ok {
		return n, true
	}
	lower := strings.ToLower(id)
	if n, ok := ix.Get(lower); ok {
		return n, true
	}
	if trimmed, cut := strings.CutSuffix(lower, ".md"); cut {
		if n, ok := ix.Get(trimmed); ok {
			return n, true
		}
	}
	return nil, false
}

type graphQueryInput struct {
	Type    string `json:"type"`
	Owner   string `json:"owner"`
	About   string `json:"about"`
	Kind    string `json:"kind"`
	Binding bool   `json:"binding"`
	Status  string `json:"status"`
	Limit   int    `json:"limit"`
}

func renderGraphQuery(s *store.FSStore, caller string, raw json.RawMessage) (string, bool) {
	var in graphQueryInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "graph_query: invalid input: " + err.Error(), true
		}
	}
	f := graph.Filter{
		Type:  graph.NodeType(strings.TrimSpace(in.Type)),
		Owner: strings.TrimSpace(in.Owner),
		// Agents paste ids as graph_node prints them, pinned; the
		// subject filter is about the node, whatever version.
		About: graph.ParseRef(in.About).ID,
		Kind:  graph.Kind(strings.ToLower(strings.TrimSpace(in.Kind))),
	}
	if f.Type != "" && f.Type != graph.TypeArtifact && f.Type != graph.TypeAgent {
		return fmt.Sprintf("graph_query: type %q is not artifact or agent", in.Type), true
	}
	if !graph.ValidKind(f.Kind) {
		return fmt.Sprintf("graph_query: kind %q is not one of %s", in.Kind, kindNames()), true
	}
	if in.Binding {
		f.KindSet = []graph.Kind{graph.KindRequirement, graph.KindDecision}
	}
	switch st := strings.ToLower(strings.TrimSpace(in.Status)); st {
	case "":
	case "in_force", "in-force", "inforce":
		f.InForce = true
	case string(graph.StatusDraft), string(graph.StatusProvisional), string(graph.StatusCurrent),
		string(graph.StatusWithdrawn), string(graph.StatusSuperseded), string(graph.StatusActive), string(graph.StatusArchived):
		f.Status = graph.Status(st)
	default:
		return fmt.Sprintf("graph_query: status %q is not one of draft, provisional, current, withdrawn, superseded, in_force (artifacts) or active, archived (agents)", in.Status), true
	}
	ix := graphIndexFor(s)
	if ix == nil {
		return "graph_query: the graph has not been indexed yet", true
	}
	limit := in.Limit
	if limit <= 0 {
		limit = graphQueryDefaultLimit
	}
	nodes := ix.Query(f)
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	var b strings.Builder
	fmt.Fprintf(&b, "Graph snapshot as of %s (index seq %d): %d node%s match%s", now, ix.Seq, len(nodes), plural(len(nodes)), filterSummary(in))
	if len(nodes) == 0 {
		b.WriteString(".\n")
		return b.String(), false
	}
	b.WriteString(".\n\n")
	for i, n := range nodes {
		if i >= limit {
			fmt.Fprintf(&b, "… %d more; narrow the filters or raise limit.\n", len(nodes)-limit)
			break
		}
		b.WriteString("- ")
		b.WriteString(n.Line())
		b.WriteByte('\n')
	}
	b.WriteString("\nOpen a node with graph_node (edges, versions, the file_view path) or read it directly with file_view.\n")
	return b.String(), false
}

func filterSummary(in graphQueryInput) string {
	var parts []string
	if in.Type != "" {
		parts = append(parts, "type="+in.Type)
	}
	if in.Owner != "" {
		parts = append(parts, "owner="+in.Owner)
	}
	if in.About != "" {
		parts = append(parts, "about="+in.About)
	}
	if in.Kind != "" {
		parts = append(parts, "kind="+in.Kind)
	}
	if in.Binding {
		parts = append(parts, "binding")
	}
	if in.Status != "" {
		parts = append(parts, "status="+in.Status)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func renderGraphNode(s *store.FSStore, caller string, raw json.RawMessage) (string, bool) {
	var in struct {
		ID string `json:"id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return "graph_node: invalid input: " + err.Error(), true
		}
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return "graph_node: `id` is required (an agent slug, or owner/name as printed by graph_query)", true
	}
	ix := graphIndexFor(s)
	if ix == nil {
		return "graph_node: the graph has not been indexed yet", true
	}
	ref := graph.ParseRef(id)
	n, ok := lookupNode(ix, ref.ID)
	if !ok {
		return fmt.Sprintf("graph_node: no node %q. graph_query lists ids; an owner's nodes are graph_query owner=<slug>.", ref.ID), true
	}
	// A pinned id asks for the node as it was at that version: the
	// header below is the current index state, and the version's own
	// text follows it. Resolve the pin first so a bad one is an error
	// with the version list, not a header with nothing under it.
	var pinned graph.Version
	if ref.Pin != "" {
		if n.Type == graph.TypeAgent {
			return fmt.Sprintf("graph_node: %s is an agent; agents have no versions to pin", n.ID), true
		}
		v, ok := n.Version(ref.Pin)
		if !ok {
			return fmt.Sprintf("graph_node: %s has no version %q; its versions are %s (pin as %s@N)", n.ID, ref.Pin, versionList(n), n.ID), true
		}
		pinned = v
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	var b strings.Builder
	if n.Type == graph.TypeAgent {
		fmt.Fprintf(&b, "Node %s (agent) — snapshot as of %s\n", n.ID, now)
		fmt.Fprintf(&b, "  role: %s\n", orDash(n.Summary))
		fmt.Fprintf(&b, "  status: %s\n", n.Status)
		if n.ReportsTo != "" {
			fmt.Fprintf(&b, "  reports to: %s\n", n.ReportsTo)
		}
		owned := ix.Query(graph.Filter{Owner: n.ID, Type: graph.TypeArtifact})
		fmt.Fprintf(&b, "  owns: %d artifact%s (graph_query owner=%s)\n", len(owned), plural(len(owned)), n.ID)
		writeIDList(&b, "about this agent", n.AboutMe)
		return b.String(), false
	}
	label := "artifact"
	if n.Kind != "" {
		label = string(n.Kind)
	}
	fmt.Fprintf(&b, "Node %s (%s) — snapshot as of %s\n", n.ID, label, now)
	fmt.Fprintf(&b, "  owner: %s\n", n.Owner)
	fmt.Fprintf(&b, "  path: %s\n", n.Path)
	fmt.Fprintf(&b, "  read: file_view %s\n", graphViewPath(n, caller))
	if n.Payload != "" {
		fmt.Fprintf(&b, "  payload: %s\n", n.Payload)
	}
	status := string(n.Status)
	if n.DeclaredStatus != "" && n.DeclaredStatus != n.Status {
		status += fmt.Sprintf(" (declared %s)", n.DeclaredStatus)
	}
	if n.Condition != "" {
		status += fmt.Sprintf("; condition: %s", n.Condition)
	}
	fmt.Fprintf(&b, "  status: %s\n", status)
	if n.Flagged {
		fmt.Fprintf(&b, "  flagged: %s\n", strings.Join(n.Flags, "; "))
	}
	if n.Rejected != "" {
		fmt.Fprintf(&b, "  front matter rejected: %s (indexed as a bare artifact)\n", n.Rejected)
	}
	if n.Summary != "" {
		fmt.Fprintf(&b, "  summary: %s\n", n.Summary)
	}
	if n.Check != "" {
		fmt.Fprintf(&b, "  check: %s\n", n.Check)
	}
	if n.Source != "" {
		fmt.Fprintf(&b, "  source: %s\n", n.Source)
	}
	if n.About != nil {
		fmt.Fprintf(&b, "  about: %s\n", refLine(*n.About))
	}
	writeRefList(&b, "depends on", n.DependsOn)
	writeRefList(&b, "supersedes", n.Supersedes)
	if len(n.Evidence) > 0 {
		fmt.Fprintf(&b, "  evidence: %s\n", strings.Join(n.Evidence, ", "))
	}
	if len(n.Versions) > 0 {
		fmt.Fprintf(&b, "  versions: %s (pin as %s@%d; graph_node %s@N returns the file at version N)\n", versionList(n), n.ID, n.CurrentVersion().N, n.ID)
	}
	writeIDList(&b, "about this", n.AboutMe)
	writeIDList(&b, "rests on this", n.Dependents)
	writeIDList(&b, "superseded by", n.SupersededBy)
	if len(n.Problems) > 0 {
		fmt.Fprintf(&b, "  problems: %s\n", strings.Join(n.Problems, "; "))
	}
	if pinned.N > 0 {
		writeGraphVersionText(&b, s, n, pinned)
	}
	return b.String(), false
}

// versionList renders a node's versions oldest first: number, date,
// short manifest SHA and, when the node has one, short payload SHA.
func versionList(n *graph.Node) string {
	parts := make([]string, 0, len(n.Versions))
	for _, v := range n.Versions {
		part := fmt.Sprintf("v%d %s %s", v.N, v.TS.UTC().Format("2006-01-02"), shortSHA(v.SHA))
		if v.PayloadSHA != "" {
			part += "+" + shortSHA(v.PayloadSHA)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " · ")
}

// writeGraphVersionText appends the file as it was at version v: the
// whole file, front matter and body, between begin/end lines that
// name the pin, so the reader can tell it from the current header
// above it. This is the one place the graph tools return a body:
// file_view reads the current file, and a certificate or a pinned
// dependency names a version that file may no longer be.
func writeGraphVersionText(b *strings.Builder, s *store.FSStore, n *graph.Node, v graph.Version) {
	pin := fmt.Sprintf("%s@%d", n.ID, v.N)
	current := n.CurrentVersion()
	standing := "the current version; file_view reads the same text"
	if v.N != current.N {
		newer := 0
		for _, other := range n.Versions {
			if other.N > v.N {
				newer++
			}
		}
		standing = fmt.Sprintf("%d newer version%s; v%d is current and is what file_view reads", newer, plural(newer), current.N)
	}
	fmt.Fprintf(b, "\n%s — the file as versioned %s (sha %s); %s.\n", pin, v.TS.UTC().Format("2006-01-02 15:04 UTC"), shortSHA(v.SHA), standing)
	if v.PayloadSHA != "" {
		fmt.Fprintf(b, "%s payload at this version: sha %s (a file; not shown).\n", pin, shortSHA(v.PayloadSHA))
	}
	snap, err := s.ReadGraphSnapshot(n, v)
	switch {
	case errors.Is(err, store.ErrGraphSnapshotGone):
		fmt.Fprintf(b, "%s: the bytes of this version are no longer in the store (a deleted upload, or a snapshot lost to a restore); the version stays on record so pins to it still resolve.\n", pin)
		return
	case err != nil:
		log.Printf("graph_node: read %s: %v", pin, err)
		fmt.Fprintf(b, "%s: this version could not be read: %v\n", pin, err)
		return
	case snap.Binary:
		fmt.Fprintf(b, "%s: %d bytes, not text; not shown.\n", pin, snap.Size)
		return
	}
	fmt.Fprintf(b, "----- begin %s -----\n", pin)
	b.WriteString(snap.Text)
	if !strings.HasSuffix(snap.Text, "\n") {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "----- end %s -----\n", pin)
}

// graphViewPath is where the caller can file_view a node's file: its
// own public directory for its own nodes, the shared tree for a
// peer's, the project directory for the CEO's.
func graphViewPath(n *graph.Node, caller string) string {
	switch n.Owner {
	case graph.CEOSlug:
		return "/files/project/" + n.Path
	case caller:
		return "/files/artifacts/public/" + n.Path
	default:
		return "/files/artifacts/shared/" + n.Owner + "/" + n.Path
	}
}

func refLine(r graph.Ref) string {
	if r.Resolved {
		return r.String()
	}
	return r.String() + " (unresolved)"
}

func writeRefList(b *strings.Builder, label string, refs []graph.Ref) {
	if len(refs) == 0 {
		return
	}
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, refLine(r))
	}
	fmt.Fprintf(b, "  %s: %s\n", label, strings.Join(parts, ", "))
}

func writeIDList(b *strings.Builder, label string, ids []string) {
	if len(ids) == 0 {
		return
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	fmt.Fprintf(b, "  %s (%d): %s\n", label, len(sorted), strings.Join(sorted, ", "))
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func kindNames() string {
	parts := make([]string, len(graph.Kinds))
	for i, k := range graph.Kinds {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}
