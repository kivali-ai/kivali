package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/controlclient"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/structured"
)

// runMCP is the `kivali mcp` subcommand. It launches a stdio MCP
// server exposing one of two named toolkits:
//
//   - --toolkit full-agent (default) — every Kivali agent's tool
//     surface: file_*, artifact_publish / artifact_unpublish,
//     agent_memory_*, publish_*, state lookups, run_shell, share_file,
//     search_past_chats, list_project_files, and `subagent` (the
//     parent-side spawn-a-subagent tool), and the actions of the
//     integrations the agent has access to, listed by core at start.
//
//   - --toolkit subagent — the narrow surface a subagent gets: file_*
//     scoped to the subagent's own /files/, artifact_publish /
//     artifact_unpublish (into its parent's published area),
//     list_skills, graph_query and graph_node, run_shell (cwd'd into a
//     per-subagent scratch dir inside the parent's agent pod),
//     `subagent` only for a tier that may delegate, and `submit_result`
//     only for a task with a schema (--result-schema). Explicitly
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
// Transport: run_shell and file_* go to the agent-shell sidecar
// (AGENT_SHELL_SOCKET); every other dispatcher routes through
// agentpod.Client over the core UDS specified by --core-uds. There is
// no FSStore-in-process fallback — agents always run in their own pod
// with no store mount. Subagent + route-message backends still go
// through controlclient (same socket).
func runMCP(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	toolkitName := fs.String("toolkit", "full-agent", "tool bundle to expose: full-agent | subagent | web")
	agentSlug := fs.String("agent", "", "[full-agent] agent slug that scopes every operation")
	parentSlug := fs.String("parent", "", "[subagent] slug of the parent agent that spawned this subagent")
	subagentID := fs.String("subagent-id", "", "[subagent] per-call subagent id (matches the on-disk path under data/agents/<parent>/subagents/)")
	coreUDS := fs.String("core-uds", "", "path to core's UDS socket; every dispatcher routes through agentpod.Client over UDS")
	subagentFilesRoot := fs.String("subagent-files-root", "", "[subagent] absolute path of the subagent's /files/ view, /files/subagents/<id>; file_* + run_shell both run against this directory")
	subagentDepth := fs.Int("depth", 1, "[subagent] tier below the durable agent (1 = dispatched by the agent itself). Decides whether this subagent may delegate.")
	resultSchemaPath := fs.String("result-schema", "", "[subagent, web] path of the task's result schema; when set, the toolkit carries submit_result built from it")
	webSession := fs.String("session", "", "[web] the web session the task runs in (open, or a site)")
	webSite := fs.String("site", "", "[web] the site the task is held to: its session's, or the site its uploads were approved for; empty for none")
	_ = fs.Parse(args)

	logger := log.New(os.Stderr, "mcp: ", log.LstdFlags|log.Lmicroseconds)

	if *coreUDS == "" {
		logger.Fatal("--core-uds is required")
	}

	switch *toolkitName {
	case "full-agent", "":
		runFullAgentMCP(logger, *coreUDS, *agentSlug)
	case "subagent":
		runSubagentMCP(logger, *coreUDS, *parentSlug, *subagentID, *subagentFilesRoot, *subagentDepth, *resultSchemaPath)
	case "web":
		runWebSubagentMCP(logger, *coreUDS, *parentSlug, *subagentID, *webSession, *webSite, *subagentDepth, *resultSchemaPath)
	default:
		logger.Fatalf("unknown --toolkit %q (want full-agent, subagent or web)", *toolkitName)
	}
}

// runWebSubagentMCP wires up the web toolkit: the web_* tools and
// submit_result, every one a relay to core over the agent pod's UDS.
// No files root and no depth: a web subagent has no files, no shell
// and cannot delegate.
func runWebSubagentMCP(logger *log.Logger, socketPath, parent, id, session, site string, depth int, resultSchemaPath string) {
	if parent == "" || id == "" {
		logger.Fatal("--parent and --subagent-id are required for --toolkit web")
	}
	if session == "" {
		logger.Fatal("--session is required for --toolkit web")
	}
	if resultSchemaPath == "" {
		logger.Fatal("--result-schema is required for --toolkit web: a web task always has a schema")
	}
	if _, err := os.Stat(socketPath); err != nil {
		logger.Fatalf("--core-uds %q: %v", socketPath, err)
	}
	raw, err := os.ReadFile(resultSchemaPath)
	if err != nil {
		logger.Fatalf("--result-schema %q: %v", resultSchemaPath, err)
	}
	schema, err := structured.Parse(raw)
	if err != nil {
		logger.Fatalf("--result-schema %q: %v", resultSchemaPath, err)
	}
	tk := mcp.WebSubagentToolkit(mcp.WebSubagentDeps{
		Client:     coreClient(socketPath, parent),
		SubagentID: id,
		Session:    session,
		Site:       site,
		// A task the agent dispatched itself, in a site session, may
		// pause for a person to sign in; a deeper one may not.
		CanWait:      session != agentpod.OpenWebSession && depth == 1,
		ResultSchema: schema,
	})
	server := &mcp.Server{
		Tools:      tk.Tools,
		ServerName: "kivali-web",
		Version:    "0.1",
		Logger:     logger,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger.Printf("starting MCP server (web): toolkit=%s parent=%s subagent=%s session=%s core-uds=%s tools=%d",
		tk.Name, parent, id, session, socketPath, len(tk.Tools))
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		logger.Fatalf("serve: %v", err)
	}
	logger.Print("shutdown (EOF)")
}

// runFullAgentMCP wires up the full agent toolkit. run_shell and the
// file_* tools dispatch to the agent-shell sidecar over the per-pod UDS
// at /run/kivali-agent-shell/sock (always present in production agent
// pods — see AgentShellImageFor); every other dispatcher routes through
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
	client := coreClient(socketPath, agentSlug)
	isCoS := agentSlug == "chief-of-staff"

	// Subagent backend goes through controlclient against the same
	// socket. Publishes don't go through controlclient; the publish
	// dispatcher routes server-side.
	subagentBackend := controlSubagentBackend{client: controlClient(socketPath, agentSlug)}

	// run_shell and the file_* tools both execute in the agent-shell
	// sidecar, reached over its per-pod UDS. The sidecar carries the
	// Debian dev toolkit (python, jq, git, etc.) the slim Kivali
	// runtime image deliberately doesn't ship, and it is the only
	// container that mounts the agent's /files/: the one this process
	// runs in holds the Claude credentials and mounts nothing an agent
	// can write. The daemon builds the files Backend itself, from its
	// own mounts (docs/developers/files-and-publishing.md).
	shellExec, filesDisp, err := agentFileTools(os.Getenv("AGENT_SHELL_SOCKET"), localFilesRequested(), agentSlug, agentFilesRoot())
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
	integrationTools := fullAgentIntegrationTools(logger, client)
	tk.Tools = append(tk.Tools, mcp.IntegrationTools(client, integrationTools, tk.Tools)...)

	server := &mcp.Server{
		Tools:      tk.Tools,
		ServerName: "kivali",
		Version:    "0.1",
		Logger:     logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Printf("starting MCP server: toolkit=%s agent=%s core-uds=%s tools=%d integration-actions=%d cos=%v",
		tk.Name, agentSlug, socketPath, len(tk.Tools), len(integrationTools), isCoS)
	if err := server.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		logger.Fatalf("serve: %v", err)
	}
	logger.Print("shutdown (EOF)")
}

// integrationListTimeout bounds the start's wait for the agent's
// integration actions: the CLI waits on this server's start, and core
// may ask an integration for its list first.
const integrationListTimeout = 10 * time.Second

// fullAgentIntegrationTools is the agent's integration actions as core
// lists them at this server's start: the set the turn that spawned the
// CLI carried, which a grant given or taken away changes by respawning
// the CLI, and so this server. A failure is logged and leaves none
// registered: the model is shown none of them until the CLI next
// respawns, and a call of one of their names has no tool to answer it.
func fullAgentIntegrationTools(logger *log.Logger, client *agentpod.Client) []agentpod.IntegrationTool {
	ctx, cancel := context.WithTimeout(context.Background(), integrationListTimeout)
	defer cancel()
	resp, err := client.IntegrationTools(ctx)
	if err != nil {
		logger.Printf("integration actions: %v (starting without them)", err)
		return nil
	}
	return resp.Tools
}

// runSubagentMCP wires up the subagent toolkit. file_* and run_shell
// BOTH execute in the agent-shell sidecar against the parent's /files/
// narrowed to the subagent's overlay subdir (/files/subagents/<id>/).
// Same physical tree, no dual-view confusion; files the subagent
// writes under artifacts/private/ persist directly on the PVC where
// the parent file_views them.
//
// list_skills routes through the agentpod.Client over UDS (skills
// catalog is global, not per-subagent).
//
// resultSchemaPath names the task's schema when the caller asked for a
// structured answer; the toolkit then carries submit_result.
func runSubagentMCP(logger *log.Logger, socketPath, parent, id, filesRoot string, depth int, resultSchemaPath string) {
	if parent == "" || id == "" {
		logger.Fatal("--parent and --subagent-id are required for --toolkit subagent")
	}
	if filesRoot == "" {
		logger.Fatal("--subagent-files-root is required for --toolkit subagent")
	}
	if _, err := os.Stat(socketPath); err != nil {
		logger.Fatalf("--core-uds %q: %v", socketPath, err)
	}
	client := coreClient(socketPath, parent)
	// Shell cwd and the file_* root are the subagent's narrowed
	// /files/ view. In the agent pod both execute in the agent-shell
	// sidecar (AGENT_SHELL_SOCKET is set on `kivali agent`), named by
	// the view's path under the daemon's root; the daemon builds the
	// Backend, with the parent's tree as a WriteRoot since the view's
	// links resolve into it.
	shellExec, filesDisp, err := subagentFileTools(os.Getenv("AGENT_SHELL_SOCKET"), localFilesRequested(), parent, filesRoot)
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
		deps.NestedSubagent = nestedSubagentBackend{client: controlClient(socketPath, parent)}
	}
	// The schema file was written by the agent runtime from the spec
	// core sent, and core accepted the schema at dispatch; a file that
	// does not parse here is a wiring fault, not a model's mistake.
	if resultSchemaPath != "" {
		raw, err := os.ReadFile(resultSchemaPath)
		if err != nil {
			logger.Fatalf("--result-schema %q: %v", resultSchemaPath, err)
		}
		schema, err := structured.Parse(raw)
		if err != nil {
			logger.Fatalf("--result-schema %q: %v", resultSchemaPath, err)
		}
		deps.ResultSchema = schema
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

func (b controlSubagentBackend) WebSessions(ctx context.Context, parent string) (string, error) {
	return b.client.WebSessions(ctx, parent)
}

func (b controlSubagentBackend) WebSessionReset(ctx context.Context, parent, session string) (string, error) {
	return b.client.WebSessionReset(ctx, parent, session)
}

// An MCP server started by an agent pod's CLI inherits the runtime's
// credentialed proxy URL in its environment. Every request it sends
// over core.sock, through either client below, has that credential
// scrubbed from its body and query: tool arguments the model wrote,
// subagent prompts among them. Its requests to the agent-shell daemon
// are scrubbed the same way (shellScrubber).

// coreClient is the agentpod.Client an MCP server talks to core with.
func coreClient(socketPath, slug string) *agentpod.Client {
	return agentpod.NewClient(socketPath, slug).WithScrubber(egresscred.ScrubberFromEnv(os.Getenv))
}

// controlClient is the controlclient.Client an MCP server dispatches
// subagents and routes messages with.
func controlClient(socketPath, slug string) *controlclient.Client {
	return controlclient.New(socketPath, slug).WithScrubber(egresscred.ScrubberFromEnv(os.Getenv))
}
