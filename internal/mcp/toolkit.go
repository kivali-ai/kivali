package mcp

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
)

// Toolkit is a named bundle of MCP Tools. The MCP subprocess picks one
// at startup based on the --toolkit flag, then registers the bundle's
// Tools on its server.
//
// Two toolkits exist today:
//
//   - "full-agent" — what every Kivali agent gets: filesystem,
//     artifact_publish / artifact_unpublish, agent_memory, state
//     lookups, search, project file listing, publish_*, share_file,
//     run_shell, plus the `subagent` tool.
//   - "subagent" — the narrow surface a subagent gets: filesystem
//     (scoped to the subagent's own root), artifact_publish /
//     artifact_unpublish (into its parent's area), list_skills, the two
//     read-only graph tools (graph_query, graph_node), run_shell, and
//     `subagent` only for a tier that may delegate. Explicitly
//     excluded: publish_*, agent_memory_*, search_past_chats,
//     list_project_files, share_file, get_org_chart, the assignment tools,
//     read_agent_role.
//
// The split is enforced here rather than at the CLI's --allowedTools
// layer alone so a toolkit mismatch can never accidentally expose a
// publish_* call from a subagent process: defense in depth.
type Toolkit struct {
	Name  string
	Tools []Tool
}

// FullAgentDeps wires the dependencies a full agent's MCP toolkit
// needs. A single agentpod.Client backs every UDS-routed dispatcher;
// ShellExec is the dev-shell sidecar (LocalShell without one);
// Subagent reaches the Kivali web pod via controlclient (same control
// socket the Client uses).
//
// Files runs the file_* tools: in an agent pod the dev-shell sidecar
// (NewSidecarFilesDispatcher), so file_* and run_shell act on the same
// tree with the same privileges; without one, a local Backend
// (NewLocalFilesDispatcher).
type FullAgentDeps struct {
	Client         *agentpod.Client
	IsChiefOfStaff bool
	Files          FilesDispatcher
	ShellExec      agent.ShellExecutor
	Subagent       SubagentBackend
	// Provider supplies the subagent tool schema's models and efforts;
	// required when Subagent is set.
	Provider provider.Provider
}

// fullAgentToolkitDeps is the interface-bag the toolkit composer
// consumes. The public FullAgentDeps maps onto this; the indirection
// keeps the composer mockable from tests without exposing the
// dispatcher constructors as public API.
type fullAgentToolkitDeps struct {
	Slug           string
	IsChiefOfStaff bool

	AgentMemory  AgentMemoryDispatcher
	Files        FilesDispatcher
	State        StateDispatcher
	PastChats    PastChatsReader
	ProjectFiles ProjectFilesLister
	Publish      PublishDispatcher
	ShareFile    ShareFileDispatcher
	Artifact     ArtifactDispatcher

	ShellExec agent.ShellExecutor
	Subagent  SubagentBackend
	Provider  provider.Provider
}

// FullAgentToolkit returns the full-agent tool bundle wired entirely
// through UDS via an agentpod.Client. The in-pod MCP subprocess uses
// this; agents always run in their own pod and route every
// dispatcher through core's UDS.
//
// IsChiefOfStaff comes from the caller because the slug check
// (slug == "chief-of-staff") is sufficient — server-side gates
// re-check defensively for tools like read_agent_role.
func FullAgentToolkit(deps FullAgentDeps) Toolkit {
	requireFiles("FullAgentToolkit", deps.Files)
	c := deps.Client
	return composeFullAgentToolkit(fullAgentToolkitDeps{
		Slug:           c.Slug(),
		IsChiefOfStaff: deps.IsChiefOfStaff,
		AgentMemory:    NewAgentpodAgentMemoryDispatcher(c),
		Files:          deps.Files,
		State:          NewAgentpodStateDispatcher(c),
		PastChats:      NewAgentpodPastChatsReader(c),
		ProjectFiles:   NewAgentpodProjectFilesLister(c),
		Publish:        NewAgentpodPublishDispatcher(c),
		ShareFile:      NewAgentpodShareFileDispatcher(c),
		Artifact:       NewAgentpodArtifactDispatcher(c, ""),
		ShellExec:      deps.ShellExec,
		Subagent:       deps.Subagent,
		Provider:       deps.Provider,
	})
}

// requireFiles refuses a toolkit with no file_* dispatcher. The tools
// would register, and the first file_* call would then panic inside
// the MCP server; a wiring mistake should stop the server at startup,
// where the process log names it, not surface as a crashed tool call.
// A nil pointer in a non-nil interface counts as nil.
func requireFiles(constructor string, d FilesDispatcher) {
	if d == nil || isNilPointer(d) {
		panic("mcp." + constructor + ": Files is nil; wire NewSidecarFilesDispatcher (or NewLocalFilesDispatcher in tests)")
	}
}

// isNilPointer reports whether v holds a nil pointer.
func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

// composeFullAgentToolkit is the shared toolkit builder.
func composeFullAgentToolkit(d fullAgentToolkitDeps) Toolkit {
	tools := FilesystemTools(d.Files)
	tools = append(tools, ArtifactTools(d.Artifact)...)
	tools = append(tools, AgentMemoryTools(d.AgentMemory)...)
	tools = append(tools, StateTools(d.State, d.IsChiefOfStaff)...)
	tools = append(tools, SearchPastChatsTool(d.PastChats))
	tools = append(tools, ListProjectFilesTool(d.ProjectFiles))
	tools = append(tools, PublishTools(PublishToolsConfig{
		Dispatcher:     d.Publish,
		From:           d.Slug,
		IsChiefOfStaff: d.IsChiefOfStaff,
	})...)
	if d.ShellExec != nil {
		if t := ShellTool(ShellToolConfig{
			Exec: d.ShellExec,
			From: d.Slug,
		}); t != nil {
			tools = append(tools, *t)
		}
	}
	if t := ShareFileTool(d.ShareFile); t != nil {
		tools = append(tools, *t)
	}
	if d.Subagent != nil {
		cfg := SubagentToolConfig{Backend: d.Subagent, Parent: d.Slug, Provider: d.Provider}
		// The three travel together: dispatch is fire-and-forget, so
		// an agent that can start background work must also be able to
		// see what is outstanding and call it off. Registering
		// `subagent` without them would leave an agent able to launch
		// work it has no way to inspect or stop.
		tools = append(tools,
			SubagentTool(cfg),
			SubagentStatusTool(cfg),
			SubagentCancelTool(cfg),
		)
	}
	return Toolkit{Name: "full-agent", Tools: tools}
}

// SubagentDeps wires the subagent-toolkit MCP server. Files runs
// file_* against the subagent's view of its parent's /files/
// (/files/subagents/<id>/, built by core), in the dev-shell sidecar
// like the parent's, so file_view and run_shell never disagree.
// list_skills routes through UDS (skills catalog is global).
type SubagentDeps struct {
	Client     *agentpod.Client
	SubagentID string
	Files      FilesDispatcher
	ShellExec  agent.ShellExecutor

	// NestedSubagent, when non-nil, gives this subagent the `subagent`
	// tool so it can act as a sub-lead. Nil for a leaf tier, and the
	// difference is enforced here rather than only at --allowedTools:
	// a leaf's MCP server does not implement the tool at all, so no
	// flag mistake can hand one out.
	NestedSubagent NestedSubagentBackend
	// Provider supplies the nested tool schema's models and efforts;
	// required when NestedSubagent is set.
	Provider provider.Provider
}

// SubagentToolkit returns the narrow tool bundle a subagent gets. Only
// what's necessary to do focused work and produce files for the parent.
// file_* and run_shell run in the dev-shell sidecar, rooted at the
// subagent's view; list_skills and the graph reads are UDS.
func SubagentToolkit(deps SubagentDeps) Toolkit {
	requireFiles("SubagentToolkit", deps.Files)
	tools := FilesystemTools(deps.Files)
	// Publishing: a subagent's files reach the org the way its
	// parent's do, into the parent's published area. Core executes
	// both tools and maps the source through the subagent's view.
	tools = append(tools, ArtifactTools(NewAgentpodArtifactDispatcher(deps.Client, deps.SubagentID))...)
	// list_skills is the only state-tool a subagent needs — skills
	// are under /files/skills/ in the parent's agent pod, so
	// list_skills tells the subagent what's runnable. The other
	// state tools (org chart, pending work, role reads) are
	// intentionally out: subagents don't need org context.
	stateDisp := NewAgentpodStateDispatcher(deps.Client)
	tools = append(tools, ListSkillsTool(stateDisp))
	// The two graph reads are the other exception: a subagent handed
	// an object needs to find what binds it, and both tools return ids
	// and one-liners, never anything the parent would not want a
	// subagent to see (every node is public by definition).
	tools = append(tools, GraphTools(stateDisp)...)
	if deps.NestedSubagent != nil {
		// A sub-lead gets dispatch and nothing else. No
		// subagent_status, no subagent_cancel: its dispatch blocks
		// until every worker is done, so there is never a moment when
		// it holds outstanding work it could ask about or call off.
		// Shipping those tools anyway would invite a poll loop against
		// a question whose answer is always "still waiting".
		tools = append(tools, NestedSubagentTool(NestedSubagentToolConfig{
			Backend:  deps.NestedSubagent,
			CallerID: deps.SubagentID,
			Provider: deps.Provider,
		}))
	}
	if deps.ShellExec != nil {
		// SubagentShellTool is a slim variant of ShellTool that rejects
		// `inputs[]` (those would re-introduce parent-store access we're
		// trying to avoid). The wrapped Exec is expected to be
		// slug-fixed to the parent already.
		tools = append(tools, SubagentShellTool(deps.ShellExec))
	}
	return Toolkit{Name: "subagent", Tools: tools}
}

// SubagentBackend is the abstraction the subagent MCP tool delegates to.
// In production the backend is a controlclient that POSTs to the
// parent web process's control socket — the actual subagent spawning,
// transcript watching, and live-event broadcasting all happen there,
// not in the MCP subprocess. The MCP handler is a pure RPC proxy.
//
// The interface is shaped so the MCP package never needs to know about
// the wire shape of inputs/outputs: it forwards the raw arguments
// JSON to the backend and returns the backend's response body as the
// tool_result content.
type SubagentBackend interface {
	RunSubagent(ctx context.Context, parent string, arguments json.RawMessage) (rendered string, err error)
	// SubagentStatus and SubagentCancel are the job-control half.
	// Both return text already rendered for the model by core — the
	// MCP side deliberately formats nothing, so a job's state is
	// described in exactly one place.
	SubagentStatus(ctx context.Context, parent string) (rendered string, err error)
	SubagentCancel(ctx context.Context, parent, id string) (rendered string, err error)
}
