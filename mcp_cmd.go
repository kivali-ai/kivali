package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/controlclient"
	"github.com/kivali-ai/kivali/internal/mcp"
)

// runMCP is the `kivali mcp` subcommand. It launches a stdio MCP
// server exposing one of two named toolkits:
//
//   - --toolkit full-agent (default) — every Kivali agent's tool
//     surface: file_*, artifact_publish / artifact_unpublish,
//     agent_memory_*, publish_*, state lookups, run_shell, share_file,
//     search_past_chats, list_project_files, and `subagent` (the
//     parent-side spawn-a-subagent tool).
//
//   - --toolkit subagent — the narrow surface a subagent gets: file_*
//     scoped to the subagent's own /files/, artifact_publish /
//     artifact_unpublish (into its parent's published area),
//     list_skills, graph_query and graph_node, run_shell (cwd'd into a
//     per-subagent scratch dir inside the parent's agent pod), and
//     `subagent` only for a tier that may delegate. Explicitly
//     excluded: publish_*, agent_memory_*, share_file,
//     search_past_chats, list_project_files, get_org_chart, the
//     assignment_* tools, and read_agent_role.
//
// Toolkit-specific flags:
//
//   - full-agent: --agent <slug>  (the agent the MCP server is bound to)
//
//   - subagent:    --parent <slug> --subagent-id <id>  (the calling
//     parent's slug + the per-call subagent id, both used to scope
//     /files/ and the per-subagent cwd inside the parent's agent pod)
//
// Transport: run_shell and file_* go to the dev-shell sidecar
// (DEV_SHELL_SOCKET); every other dispatcher routes through
// agentpod.Client over the core UDS specified by --core-uds. There is
// no FSStore-in-process fallback — agents always run in their own pod
// with no store mount. Subagent + route-message backends still go
// through controlclient (same socket).
func runMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	toolkitName := fs.String("toolkit", "full-agent", "tool bundle to expose: full-agent | subagent")
	agentSlug := fs.String("agent", "", "[full-agent] agent slug that scopes every operation")
	parentSlug := fs.String("parent", "", "[subagent] slug of the parent agent that spawned this subagent")
	subagentID := fs.String("subagent-id", "", "[subagent] per-call subagent id (matches the on-disk path under data/agents/<parent>/subagents/)")
	coreUDS := fs.String("core-uds", "", "path to core's UDS socket; every dispatcher routes through agentpod.Client over UDS")
	subagentFilesRoot := fs.String("subagent-files-root", "", "[subagent] absolute path of the subagent's /files/ view, /files/subagents/<id>; file_* + run_shell both run against this directory")
	subagentDepth := fs.Int("depth", 1, "[subagent] tier below the durable agent (1 = dispatched by the agent itself). Decides whether this subagent may delegate.")
	_ = fs.Parse(args)

	logger := log.New(os.Stderr, "mcp: ", log.LstdFlags|log.Lmicroseconds)

	if *coreUDS == "" {
		logger.Fatal("--core-uds is required")
	}

	switch *toolkitName {
	case "full-agent", "":
		runFullAgentMCP(logger, *coreUDS, *agentSlug)
	case "subagent":
		runSubagentMCP(logger, *coreUDS, *parentSlug, *subagentID, *subagentFilesRoot, *subagentDepth)
	default:
		logger.Fatalf("unknown --toolkit %q (want full-agent or subagent)", *toolkitName)
	}
}

// runFullAgentMCP wires up the full agent toolkit. run_shell and the
// file_* tools dispatch to the dev-shell sidecar over the per-pod UDS
// at /run/kivali-dev-shell/sock (always present in production agent
// pods — see DevShellImageFor); every other dispatcher routes through
// agentpod.Client to core's UDS endpoints, and subagent +
// route-message use controlclient against the same core socket.
//
// CoS detection is by slug-equality (slug == "chief-of-staff") —
// sufficient because the slug IS the identity, and server-side
// gates re-check defensively.
func runFullAgentMCP(logger *log.Logger, socketPath, agentSlug string) {
	if agentSlug == "" {
		logger.Fatal("--agent is required for --toolkit full-agent")
	}
	if _, err := os.Stat(socketPath); err != nil {
		logger.Fatalf("--core-uds %q: %v", socketPath, err)
	}
	client := agentpod.NewClient(socketPath, agentSlug)
	isCoS := agentSlug == "chief-of-staff"

	// Subagent backend goes through controlclient against the same
	// socket. Publishes don't go through controlclient; the publish
	// dispatcher routes server-side.
	subagentBackend := controlSubagentBackend{client: controlclient.New(socketPath, agentSlug)}

	// run_shell and the file_* tools both execute in the dev-shell
	// sidecar, reached over its per-pod UDS. The sidecar carries the
	// Debian dev toolkit (python, jq, git, etc.) the slim Kivali
	// runtime image deliberately doesn't ship, and it is the only
	// container that mounts the agent's /files/: the one this process
	// runs in holds the Claude credentials and mounts nothing an agent
	// can write. The daemon builds the files Backend itself, from its
	// own mounts (docs/developers/files-and-publishing.md).
	shellExec, filesDisp, err := agentFileTools(os.Getenv("DEV_SHELL_SOCKET"), localFilesRequested(), agentSlug, agentFilesRoot())
	if err != nil {
		logger.Fatal(err)
	}

	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client:         client,
		IsChiefOfStaff: isCoS,
		Files:          filesDisp,
		ShellExec:      shellExec,
		Subagent:       subagentBackend,
		// The driver as Provider only: this process serves tools and
		// makes no model call, so nothing else of it is used.
		Provider: claudeagent.New(claudeagent.Options{}),
	})

	server := &mcp.Server{
		Tools:      tk.Tools,
		ServerName: "kivali",
		Version:    "0.1",
		Logger:     logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Printf("starting MCP server: toolkit=%s agent=%s core-uds=%s tools=%d cos=%v",
		tk.Name, agentSlug, socketPath, len(tk.Tools), isCoS)
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		logger.Fatalf("serve: %v", err)
	}
	logger.Print("shutdown (EOF)")
}

// runSubagentMCP wires up the subagent toolkit. file_* and run_shell
// BOTH execute in the dev-shell sidecar against the parent's /files/
// narrowed to the subagent's overlay subdir (/files/subagents/<id>/).
// Same physical tree, no dual-view confusion; files the subagent
// writes under artifacts/private/ persist directly on the PVC where
// the parent file_views them.
//
// list_skills routes through the agentpod.Client over UDS (skills
// catalog is global, not per-subagent).
func runSubagentMCP(logger *log.Logger, socketPath, parent, id, filesRoot string, depth int) {
	if parent == "" || id == "" {
		logger.Fatal("--parent and --subagent-id are required for --toolkit subagent")
	}
	if filesRoot == "" {
		logger.Fatal("--subagent-files-root is required for --toolkit subagent")
	}
	if _, err := os.Stat(socketPath); err != nil {
		logger.Fatalf("--core-uds %q: %v", socketPath, err)
	}
	client := agentpod.NewClient(socketPath, parent)
	// Shell cwd and the file_* root are the subagent's narrowed
	// /files/ view. In the agent pod both execute in the dev-shell
	// sidecar (DEV_SHELL_SOCKET is set on `kivali agent`), named by
	// the view's path under the daemon's root; the daemon builds the
	// Backend, with the parent's tree as a WriteRoot since the view's
	// links resolve into it.
	shellExec, filesDisp, err := subagentFileTools(os.Getenv("DEV_SHELL_SOCKET"), localFilesRequested(), parent, filesRoot)
	if err != nil {
		logger.Fatal(err)
	}

	deps := mcp.SubagentDeps{
		Client:     client,
		SubagentID: id,
		Files:      filesDisp,
		ShellExec:  shellExec,
		Provider:   claudeagent.New(claudeagent.Options{}),
	}
	// A sub-lead's MCP server implements `subagent`; a leaf's does not
	// implement it at all. The tier is decided by the runtime that
	// wrote this process's arguments, so the model running above this
	// server has no way to reach for a tool its tier was not given.
	if agentpod.CanDelegate(depth) {
		deps.NestedSubagent = nestedSubagentBackend{client: controlclient.New(socketPath, parent)}
	}
	tk := mcp.SubagentToolkit(deps)

	server := &mcp.Server{
		Tools:      tk.Tools,
		ServerName: "kivali-subagent",
		Version:    "0.1",
		Logger:     logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Printf("starting MCP server (subagent): toolkit=%s parent=%s subagent=%s depth=%d can-delegate=%v core-uds=%s files-root=%s tools=%d",
		tk.Name, parent, id, depth, agentpod.CanDelegate(depth), socketPath, filesRoot, len(tk.Tools))
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		logger.Fatalf("serve: %v", err)
	}
	logger.Print("shutdown (EOF)")
}

// controlSubagentBackend adapts a controlclient.Client into the
// mcp.SubagentBackend interface.
type controlSubagentBackend struct {
	client *controlclient.Client
}

func (b controlSubagentBackend) RunSubagent(ctx context.Context, parent string, arguments json.RawMessage) (string, error) {
	return b.client.RunSubagent(ctx, parent, arguments)
}

// nestedSubagentBackend adapts a controlclient.Client into the
// mcp.NestedSubagentBackend interface. Separate from
// controlSubagentBackend because the two have opposite contracts:
// this one blocks and returns answers rather than a receipt.
type nestedSubagentBackend struct {
	client *controlclient.Client
}

func (b nestedSubagentBackend) RunNestedSubagent(ctx context.Context, callerID string, arguments json.RawMessage) (string, error) {
	return b.client.RunNestedSubagent(ctx, callerID, arguments)
}

func (b controlSubagentBackend) SubagentStatus(ctx context.Context, parent string) (string, error) {
	return b.client.SubagentStatus(ctx, parent)
}

func (b controlSubagentBackend) SubagentCancel(ctx context.Context, parent, id string) (string, error) {
	return b.client.SubagentCancel(ctx, parent, id)
}
