package files

// Tool names and the beta header live here rather than in the agent
// package so internal/store (which triggers per-agent filesystem
// syncs) and the dispatcher can reference them without pulling agent
// back into store's import graph. internal/agent owns the provider.Tool
// slice that Claude sees; this package owns the string identity.
//
// The names are file_* (rather than the Anthropic-default memory_*) so
// agents don't conflate this filesystem tool family with the
// agent_memory_* tools that edit the curated agent_memory.md summary.

const (
	ToolView       = "file_view"
	ToolCreate     = "file_create"
	ToolStrReplace = "file_str_replace"
	ToolInsert     = "file_insert"
	ToolDelete     = "file_delete"
	ToolRename     = "file_rename"
	ToolCopy       = "file_copy"
)

// Beta is the anthropic-beta flag required for the context-management
// virtual-filesystem tool family. The file_* tools are registered as
// MCP tools that follow the operation semantics of Anthropic's memory_*
// family, so they need this beta even though their names differ.
const Beta = "context-management-2025-06-27"

// IsToolName reports whether a tool-call name belongs to the file_*
// virtual-filesystem family.
func IsToolName(name string) bool {
	switch name {
	case ToolView, ToolCreate, ToolStrReplace, ToolInsert, ToolDelete, ToolRename, ToolCopy:
		return true
	}
	return false
}
