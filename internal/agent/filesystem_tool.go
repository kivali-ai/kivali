package agent

import (
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
)

// File-tool identity is owned by internal/files (so internal/store
// can reference it without pulling agent into store's import graph).
// This package exposes thin re-exports + the provider.Tool slice the
// model sees.

// IsFilesystemTool reports whether a tool-call name belongs to the file_*
// (virtual-filesystem) family. Thin re-export of files.IsToolName so
// callers that already import agent don't also have to
// import files.
func IsFilesystemTool(name string) bool { return files.IsToolName(name) }

// FilesystemBeta is the anthropic-beta flag required for the file_* tools.
var FilesystemBeta = files.Beta

// FilesystemTools is the file_* virtual-filesystem tool family. All paths
// are rooted at "/files/" from the model's perspective; the
// internal/files Backend maps paths to a per-agent on-disk directory
// with traversal protection. The tools execute in the dev-shell
// container, beside run_shell, with the same mounts and privileges, so
// file_view paths and shell paths point at the same physical bytes
// (see docs/developers/files-and-publishing.md).
//
// Description text leans on the model to search narrowly — the whole
// point of moving project files out of the system prompt is that most
// conversations need zero files, and cost blows up if the agent
// defensively reads everything on each release.
func FilesystemTools() []provider.Tool {
	return []provider.Tool{
		{
			Name: files.ToolView,
			Description: `View a file or directory under /files/. For directories, returns a sorted listing. For files, returns the contents prefixed with a one-line header: [file: N lines, B bytes — showing X-Y].

Narrow-read params (USE THESE on large files to keep chat-history weight down — every full read replays on every subsequent API call until rotation):
  - offset / limit: 1-indexed line offset and max lines to return (Claude Code Read-tool semantics).
  - grep: Go regex; returns only matching lines, prefixed with line numbers (like grep -n). Pair with grep_context for surrounding lines.
  - view_range: [start, end] 1-indexed inclusive slice; offset/limit does the same and is preferred.

With no narrow-read params the whole file is returned. Prefer exact paths over browsing — the project/ subtree can be large, and listing + reading everything is how this tool costs more than it saves.`,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":         {"type": "string", "description": "absolute path under /files/, e.g. /files/project/biz-plan.md or /files/"},
    "offset":       {"type": "integer", "minimum": 0, "description": "optional 1-indexed line to start reading from (0 = from line 1). Pair with limit for narrow reads on large files."},
    "limit":        {"type": "integer", "minimum": 0, "description": "optional max lines to return starting at offset (0 = to end of file)."},
    "grep":         {"type": "string", "description": "optional Go regex; only lines matching this pattern are returned, prefixed with their line number. Cheaper than reading the whole file when you only need to find a section."},
    "grep_context": {"type": "integer", "minimum": 0, "description": "optional number of context lines emitted before/after each grep match (default 0)."},
    "view_range":   {"type": "array", "items": {"type": "integer"}, "minItems": 2, "maxItems": 2, "description": "optional [start_line, end_line] (1-indexed, inclusive) — prefer offset/limit."}
  },
  "required": ["path"]
}`),
		},
		{
			Name:        files.ToolCreate,
			Description: `Create or overwrite a file under /files/. Use /files/artifacts/private/ for your own working files and /files/background/ (your background workspace) for what your subagents must read or write; the project/, skills/, attachments/, past-chats/, episodes/, artifacts/public/ and artifacts/shared/ subtrees are read-only to this tool and to run_shell alike. Nothing you write here is visible to other agents until you publish it with artifact_publish; a markdown file with YAML front matter that you publish is a knowledge-graph node (see graph_query), and the publish reply says what the index made of it.`,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":      {"type": "string", "description": "absolute path under /files/ where the file should be written"},
    "file_text": {"type": "string", "description": "full file contents"}
  },
  "required": ["path", "file_text"]
}`),
		},
		{
			Name:        files.ToolStrReplace,
			Description: `Replace a unique substring inside a file under /files/. Fails if old_str occurs zero times or more than once.`,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":    {"type": "string"},
    "old_str": {"type": "string"},
    "new_str": {"type": "string"}
  },
  "required": ["path", "old_str", "new_str"]
}`),
		},
		{
			Name:        files.ToolInsert,
			Description: `Insert text after a specific 1-indexed line number in a file under /files/. Use 0 to prepend.`,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":       {"type": "string"},
    "insert_line":{"type": "integer", "minimum": 0},
    "insert_text":{"type": "string"}
  },
  "required": ["path", "insert_line", "insert_text"]
}`),
		},
		{
			Name:        files.ToolDelete,
			Description: `Delete a file in your workspace (anywhere under /files/ outside the read-only subtrees, e.g. /files/artifacts/private/ or /files/background/). Directories cannot be removed with this tool (run_shell can). A published file is removed with artifact_unpublish.`,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string"}
  },
  "required": ["path"]
}`),
		},
		{
			Name:        files.ToolRename,
			Description: `Rename or move a file inside your workspace (/files/artifacts/private/, /files/background/ and the rest of /files/ outside the read-only subtrees). A directory moved into or out of /files/artifacts/ needs run_shell's mv. A subagent cannot move a file between its /files/artifacts/private/ and /files/background/; file_copy it, or use mv. To move a published file, publish it at the new path and unpublish the old.`,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "old_path": {"type": "string"},
    "new_path": {"type": "string"}
  },
  "required": ["old_path", "new_path"]
}`),
		},
		{
			Name:        files.ToolCopy,
			Description: `Copy a file. Source can be any readable path under /files/ (including read-only subtrees like /files/project/, /files/attachments/, /files/skills/, /files/artifacts/shared/); destination must sit in your workspace (e.g. /files/artifacts/private/ or /files/background/). Existing destinations are overwritten. Use this to pull a project file or attachment into your writable workspace; artifact_publish is how a file gets into /files/artifacts/public/.`,
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "src_path":  {"type": "string"},
    "dest_path": {"type": "string"}
  },
  "required": ["src_path", "dest_path"]
}`),
		},
	}
}
