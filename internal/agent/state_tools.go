package agent

import (
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/provider"
)

// Tool names for the live-state lookup family. Exposed to every
// agent on every call so current org state is retrievable on demand
// instead of pre-inlined into the system prompt (where models tend
// to read mutable blocks as invariant background and miss changes
// that happened mid-chat).
const (
	GetOrgChartToolName      = "get_org_chart"
	SearchPastChatsToolName  = "search_past_chats"
	ListProjectFilesToolName = "list_project_files"
	ListSkillsToolName       = "list_skills"
	ReadAgentRoleToolName    = "read_agent_role"
	ReadHandbookToolName     = "read_handbook"
)

// ReadHandbookInputSchema is read_handbook's input: an optional section
// heading. Shared with the MCP definition so both transports offer the
// same schema.
const ReadHandbookInputSchema = `{
  "type": "object",
  "properties": {
    "section": {"type": "string", "description": "a section heading without its leading #s (e.g. 'How the org works'); returns that section and its subsections. Omit for the whole handbook."}
  }
}`

// ReadHandbookDescription is read_handbook's model-facing text, shared
// with the MCP definition.
const ReadHandbookDescription = `Read the org's handbook as it is saved right now, prefixed with a snapshot timestamp. Your system prompt carries the handbook as it stood when this turn began; this returns the current text, which differs when a change was approved during the turn. Pass section to read one section and its subsections instead of the whole document; a heading that matches no section returns the list of headings. A draft for propose_handbook_update starts from this text.`

// StateTools returns the provider.Tool definitions for the live-state
// lookup family. The actual handlers live in the MCP server
// (internal/mcp/state_tools.go); BuildRequest lists them in req.Tools
// so the CLI driver passes them through `--allowedTools`.
//
// Kept as a minimal slice — tools with no input parameters still
// need a valid JSON-schema object to pass Claude's validation.
//
// isCoS controls visibility of CoS-only tools (currently:
// read_agent_role); kept on the signature so the caller (BuildRequest)
// can pass through its own IsChiefOfStaff flag in one place.
func StateTools(isCoS bool) []provider.Tool {
	tools := []provider.Tool{
		{
			Name:        GetOrgChartToolName,
			Description: getOrgChartDescription,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {}
}`),
		},
		{
			Name:        SearchPastChatsToolName,
			Description: searchPastChatsDescription,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "literal substring to search for; case-insensitive"},
    "max_results": {"type": "integer", "description": "cap on total matches returned across all chats (default 30)"}
  },
  "required": ["query"]
}`),
		},
		{
			Name:        ListProjectFilesToolName,
			Description: listProjectFilesDescription,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {}
}`),
		},
		{
			Name:        ListSkillsToolName,
			Description: listSkillsDescription,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {}
}`),
		},
		{
			Name:        ReadHandbookToolName,
			Description: ReadHandbookDescription,
			InputSchema: json.RawMessage(ReadHandbookInputSchema),
		},
	}
	if isCoS {
		tools = append(tools, provider.Tool{
			Name:        ReadAgentRoleToolName,
			Description: readAgentRoleDescription,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "slug": {"type": "string", "description": "slug of the agent whose role.md you want to read (active or archived)"}
  },
  "required": ["slug"]
}`),
		})
	}
	return tools
}

// Descriptions kept verbatim aligned with mcp/state_tools.go,
// mcp/search_tools.go, and mcp/project_files_tool.go so model-facing
// text is identical across transports. Triggers and cross-tool
// workflows live in the handbook / cos_role.md — these descriptions
// stick to "what the tool does and what it returns".

const getOrgChartDescription = `Returns the current org chart — every active agent and who they report to — as a timestamped snapshot. No input parameters. Not pre-inlined in your system prompt; the "as of <timestamp>" line lets you tell whether a lookup in chat history is still current.`

const listProjectFilesDescription = `List the project files available to you, with a one-sentence summary per file. No input parameters. Use to triage the catalog without loading everything; open a specific file with file_view /files/project/<filename>. Output is grouped: text and image files first (with summaries), then opaque binaries (not viewable via file_view, but reachable from run_shell at /files/project/<name>).`

const readAgentRoleDescription = `Read the current ` + "`role.md`" + ` of an existing agent (active or archived under ` + "`agents/_archived/<slug>/`" + `). Chief of Staff only. Returns the verbatim contents prefixed with a snapshot timestamp. Used as the first step of a propose_role_update — see cos_role.md §Updating an existing agent's role.`

const listSkillsDescription = `List the org-wide skills available to you (name, description, when_to_use, file count). No input parameters. Skills are shared procedures rooted at /files/skills/<name>/; read the chosen one with file_view /files/skills/<name>/SKILL.md. Run their scripts directly from run_shell, e.g. bash /files/skills/<name>/scripts/foo.sh.`

const searchPastChatsDescription = `Grep-style search across your OWN archived past chats (case-insensitive substring on Content fields only — not tool_name or tool_input). Returns matching snippets grouped by chat with the path you can then open via file_view. Newest-first. See handbook §/files/past-chats/ for when to reach for this. Inputs: query (required, keep it specific — common words flood), max_results (optional, default 30).`
