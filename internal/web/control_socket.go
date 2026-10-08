package web

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ControlSocketName is the filename, under the data dir, of the Unix
// domain socket the web process listens on for inbound calls from its
// MCP subprocess fleet. Each spawned `kivali mcp` instance connects
// here to route messages back through the parent process.
//
// Why a separate socket vs. piggy-backing on the public HTTP listener:
//   - Auth boundary: the public listener is Google-OAuth-gated.
//     Internal subprocess plumbing must NOT be reachable through that
//     surface — a routing bug or a missing auth wrap on a new endpoint
//     must not expose internal state mutations to the open internet.
//   - Discovery: subprocesses derive the path from the shared --data
//     flag; no port/env-var dance for the common case.
//   - File-permission auth: 0o600 + the Kivali uid is the authn/authz.
//     No tokens to leak, no headers to forget.
const ControlSocketName = ".kivali-control.sock"

// ControlSocketPath returns the canonical control-socket path for a
// given data dir. Both the web server and `kivali mcp` subprocesses
// derive their endpoint from this — keeps the wiring symmetric and
// removes a class of "they disagreed on the path" failures.
func ControlSocketPath(dataDir string) string {
	return filepath.Join(dataDir, ControlSocketName)
}

// StartControlSocket binds the control socket and serves it on a
// dedicated mux. Returns a shutdown closure the caller defers; the
// closure stops the server gracefully and removes the socket file.
//
// The socket is intentionally NOT mounted on the public Server.mux:
// the public surface gets auth wrappers; this one is fenced off so
// no future endpoint added here can leak through the public auth
// boundary by mistake.
//
// Stale socket files (from a crashed prior run) are removed before
// bind. We never run more than one Kivali process per data dir
// (PVC + single-replica deployment), so removing a stale socket is
// safe; if a second writer somehow exists it would have failed to
// rename message_queue.json long before we got here.
func (s *Server) StartControlSocket(path string) (func(), error) {
	return s.startUDSListener(path)
}

// StartAgentpodSocket binds an additional Unix-domain socket on the
// shared hostPath UDS dir for the agent-pod runtime to dial. Same
// route surface as the control socket — splitting them physically
// reflects their topological reality:
//
//   - Control socket lives on the Kivali web pod's PVC. The in-pod
//     MCP fleet (kivali mcp subprocesses) reaches it via the shared
//     PVC mount; no other pod can.
//   - Agent-pod socket lives on a hostPath dir mounted into the
//     Kivali web pod AND every agent pod scheduled on the same node.
//     This is the only path agent runtimes can use to dial core.
//
// Both serve the same handlers because route-handler logic is
// identical regardless of which client opened the connection. The
// 0o600 mode + matching UID is the auth boundary for both.
//
// path empty → no-op (returns a noop closer). Lets main.go
// conditionally enable the agent-pod listener via env config without
// branching the call site.
func (s *Server) StartAgentpodSocket(path string) (func(), error) {
	return s.startUDSListener(path)
}

// startUDSListener binds path and serves the shared UDS-routes mux.
// Internal helper used by both StartControlSocket and
// StartAgentpodSocket so the route registration lives in exactly one
// place.
func (s *Server) startUDSListener(path string) (func(), error) {
	if path == "" {
		return func() {}, nil
	}
	// Remove any stale socket from a crashed prior run. ENOENT is
	// fine; anything else is a real error.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}

	mux := http.NewServeMux()
	s.registerUDSRoutes(mux)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("uds %s: %v", path, err)
		}
	}()

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.Remove(path)
	}, nil
}

// registerUDSRoutes mounts every UDS-served route on mux. Shared by
// the control socket (PVC, in-pod MCP fleet) and the agent-pod
// socket (hostPath, per-agent runtime) so adding a new endpoint
// only needs one entry here.
func (s *Server) registerUDSRoutes(mux *http.ServeMux) {
	// Agentpod events stream — agent runtimes connect here for
	// chat-turn wake-ups and cache invalidations. See
	// internal/agentpod and docs/developers/architecture.md.
	mux.HandleFunc("GET /v1/agent/{slug}/events", s.handleAgentpodEvents)
	// Agentpod turn-event POST — agent runtimes report stream events
	// (delta, tool_use_*, tool_result, done) and terminal failures
	// (chat-turn-failed → KindRuntimeDisruption) for in-flight turns.
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turn_id}/event", s.handleAgentpodTurnEvent)
	// Agentpod session-id sync — claude CLI session_id read/write/clear
	// for --resume continuity across long-lived agent runtimes. Today's
	// claudeagent.Client takes a *store.FSStore directly; in the new
	// architecture the agent runtime's claudeagent.Client uses an
	// agentpod.Client UDS-backed store so reads/writes round-trip to
	// core (the only authoritative writer of the session-id file).
	mux.HandleFunc("/v1/agent/{slug}/session-id", s.handleAgentpodSessionID)
	// Agentpod chat history GET + chat append POST. Single-writer
	// (core) for chat.jsonl preserved via store.AppendChatMessage.
	mux.HandleFunc("GET /v1/agent/{slug}/chat/history", s.handleAgentpodChatHistory)
	mux.HandleFunc("POST /v1/agent/{slug}/chat/append", s.handleAgentpodChatAppend)
	// Agentpod role + memory GET/POST. Read for system-prompt cache
	// fill on the agent pod side; write for the agent_memory_*
	// tools + role rewrite paths.
	mux.HandleFunc("/v1/agent/{slug}/role", s.handleAgentpodRole)
	mux.HandleFunc("/v1/agent/{slug}/memory", s.handleAgentpodMemory)
	// Agentpod agent_memory_* tool dispatch — one endpoint covers
	// both tools (append + str_replace) so the in-pod MCP handler
	// forwards (tool, raw) and core applies the change under its
	// single-writer lock.
	mux.HandleFunc("POST /v1/agent/{slug}/memory/dispatch", s.handleAgentpodMemoryDispatch)
	// Agentpod share_file dispatch — runs attachment resolution +
	// chat-row append server-side so neither needs an FSStore on
	// the agent pod.
	mux.HandleFunc("POST /v1/agent/{slug}/share-file/dispatch", s.handleAgentpodShareFileDispatch)
	// artifact_publish / artifact_unpublish: core is the only writer
	// of the published trees, so both tools execute here.
	mux.HandleFunc("POST /v1/agent/{slug}/artifact/dispatch", s.handleAgentpodArtifactDispatch)
	// Agentpod publish_* two-phase commit. Stage runs the
	// deterministic prep (parse → conflict-check → attachment
	// resolve) and persists a staged-publish record. Commit writes
	// the message file + appends the recipient inbox entry +
	// records the StageID in CommittedStageIDs under one queue-lock
	// acquisition, making it idempotent on StageID so the bridge
	// can retry on transport failure without double-routing.
	mux.HandleFunc("POST /v1/agent/{slug}/publish/stage", s.handleAgentpodPublishStage)
	mux.HandleFunc("POST /v1/agent/{slug}/publish/commit", s.handleAgentpodPublishCommit)
	// Agentpod state-tool dispatch — one endpoint covers
	// get_org_chart, list_skills, read_agent_role, the graph reads and
	// the assignment tools. Server-side rendering keeps the in-pod handler
	// trivial.
	mux.HandleFunc("POST /v1/agent/{slug}/state/dispatch", s.handleAgentpodStateDispatch)
	// Agentpod past-chats reads — list + per-archive read so the
	// in-pod search_past_chats tool can grep without bringing
	// chat.jsonl serialization into the agent pod.
	mux.HandleFunc("GET /v1/agent/{slug}/past-chats", s.handleAgentpodPastChatsList)
	mux.HandleFunc("GET /v1/agent/{slug}/past-chats/{ts}", s.handleAgentpodPastChatsRead)
	// Content-addressed reads (no slug scoping — auth is the
	// UDS file mode + same-UID; tool-layer access checks live
	// higher up in the agent runtime).
	mux.HandleFunc("GET /v1/attachments/{sha}", s.handleAgentpodAttachmentMeta)
	mux.HandleFunc("GET /v1/attachments/{sha}/blob", s.handleAgentpodAttachmentBlob)
	mux.HandleFunc("GET /v1/attachments/{sha}/canonical", s.handleAgentpodAttachmentCanonical)
	mux.HandleFunc("POST /v1/agent/{slug}/attachments", s.handleAgentpodAttachmentAdd)
	mux.HandleFunc("GET /v1/project-files", s.handleAgentpodProjectFiles)
	mux.HandleFunc("GET /v1/project-files/{sha}", s.handleAgentpodProjectFileMeta)
	mux.HandleFunc("GET /v1/project-files/{sha}/blob", s.handleAgentpodProjectFileBlob)
	mux.HandleFunc("GET /v1/project-files/{sha}/canonical", s.handleAgentpodProjectFileCanonical)
	// Skills (app-wide, no slug).
	mux.HandleFunc("GET /v1/skills", s.handleAgentpodSkillsList)
	mux.HandleFunc("GET /v1/skills/{name}", s.handleAgentpodSkill)
	mux.HandleFunc("GET /v1/skills/{name}/manifest", s.handleAgentpodSkillManifest)
	// /v1/skills/{name}/files/... uses a generic prefix match and
	// parses the rel path from the URL because trailing-slash
	// wildcards don't pair with typed path values cleanly. The
	// trailing slash here is what registers the prefix dispatch.
	mux.HandleFunc("GET /v1/skills/", s.handleAgentpodSkillFile)
	// Slug-scoped routing + subagent endpoints. These are the ONLY
	// route/subagent paths: a path without a slug binding would let any
	// caller spoof msg.From / parent. controlclient
	// (used by both the in-process MCP fleet and the agent-pod
	// MCP) targets the slug-scoped URLs and stamps X-Kivali-Agent
	// from its bound slug; the server overrides body From/parent
	// with that slug.
	mux.HandleFunc("POST /v1/agent/{slug}/route-message", s.handleAgentpodRouteMessage)
	mux.HandleFunc("POST /v1/agent/{slug}/run-subagent", s.handleAgentpodRunSubagent)
	// Job control for the background subagents run-subagent dispatches.
	// Same slug binding as everything above: the bound slug is both the
	// owner whose jobs get listed and the owner a cancel is checked
	// against.
	mux.HandleFunc("POST /v1/agent/{slug}/subagent-status", s.handleAgentpodSubagentStatus)
	mux.HandleFunc("POST /v1/agent/{slug}/subagent-cancel", s.handleAgentpodSubagentCancel)
}
