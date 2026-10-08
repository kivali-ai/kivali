package agent

import (
	"encoding/json"
	"strings"

	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/provider"
)

// Tool names for the knowledge-graph read family. Two tools, both
// read-only, both given to subagents as well: a filtered listing and a
// single-node lookup. They return ids and one-line summaries; the
// agent opens a node with file_view at the path the tool prints. The
// one body they serve is a pinned version's (graph_node owner/name@N),
// which is the text file_view cannot reach: it reads the current
// file, and a pin names what the file was. See docs/developers/knowledge-graph.md
// §Reading.
const (
	GraphQueryToolName = "graph_query"
	GraphNodeToolName  = "graph_node"
)

// GraphTools returns the native-API definitions of the two graph
// tools. The MCP side (internal/mcp/graph_tools.go) builds its
// definitions from the same constants, so the model reads one text
// whichever transport it runs under.
func GraphTools() []provider.Tool {
	return []provider.Tool{
		{
			Name:        GraphQueryToolName,
			Description: GraphQueryDescription,
			InputSchema: json.RawMessage(GraphQueryInputSchema),
		},
		{
			Name:        GraphNodeToolName,
			Description: GraphNodeDescription,
			InputSchema: json.RawMessage(GraphNodeInputSchema),
		},
	}
}

// GraphQueryInputSchema is graph_query's JSON schema. Filters are
// ANDed; every one is optional; no filters lists the whole graph.
const GraphQueryInputSchema = `{
  "type": "object",
  "properties": {
    "type": {"type": "string", "enum": ["artifact", "agent"], "description": "node type"},
    "owner": {"type": "string", "description": "agent slug; the nodes this agent owns (\"what has O published\")"},
    "about": {"type": "string", "description": "node id; artifacts whose subject is this node (\"what is about X\")"},
    "kind": {"type": "string", "enum": ["requirement", "decision", "certificate", "reference"], "description": "artifact kind"},
    "binding": {"type": "boolean", "description": "true: only requirements and decisions (\"what binds X\", with about)"},
    "status": {"type": "string", "enum": ["draft", "provisional", "current", "withdrawn", "superseded", "in_force", "active", "archived"], "description": "effective status; in_force means provisional or current; active and archived are agent statuses"},
    "limit": {"type": "integer", "description": "cap on rows returned (default 50)"}
  }
}`

// GraphNodeInputSchema is graph_node's JSON schema.
const GraphNodeInputSchema = `{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "node id: an agent slug, or owner/name as printed by graph_query; pinned as owner/name@N (or @<sha prefix>) it also returns the file as it was at that version"}
  },
  "required": ["id"]
}`

// GraphQueryDescription is graph_query's model-facing text.
var GraphQueryDescription = strings.TrimSpace(`
Search the knowledge graph: every public artifact in the org (yours under /files/artifacts/public/, peers' under /files/artifacts/shared/<owner>/, the owner's project files) and every agent, as nodes. Returns one line per node — id, kind, status, version, owner, subject, summary — never bodies. Filters AND together: owner (what has O published), about (what is about X), binding=true with about (what binds X: its requirements and decisions), kind=requirement with about (which of those are checks), status=in_force (provisional or current), status=current with about (what is of record for X, at which version). Open a node with file_view at the path graph_node prints, or graph_node id@N for the text of an older version. Answers from the current index, which every artifact_publish and artifact_unpublish updates before it returns. Kinds: ` + kindSentence() + `.`)

// GraphNodeDescription is graph_node's model-facing text.
var GraphNodeDescription = strings.TrimSpace(`
Look up one knowledge-graph node by id (an agent slug, or owner/name as printed by graph_query). Returns its front matter as indexed — kind, effective status and any flags, condition, check, source, payload — its edges both ways (about, depends_on, supersedes; and what is about it, what rests on it, what supersedes it), its version list (pin one as id@N), any index problems, and the file_view path to read the current body. With a pinned id (owner/name@N, as a certificate or a pinned depends_on writes it) it also returns the file as it was at that version, front matter and body, so you can read the exact text a pin names even after the file has changed; file_view only ever reads the current file. Use it to see what a node rests on before you rest on it, to check whether a version you hold is still current, and to read what a pinned version said.`)

func kindSentence() string {
	parts := make([]string, 0, len(graph.Kinds))
	for _, k := range graph.Kinds {
		parts = append(parts, string(k)+" ("+graph.KindDescriptions[k]+")")
	}
	return strings.Join(parts, "; ")
}
