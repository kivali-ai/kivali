package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/message"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// handleAgentpodEvents serves the long-lived events SSE stream the
// agent runtime opens via GET /v1/agent/{slug}/events. Hands every
// event the agentpodHub publishes for slug down the wire as SSE
// frames.
//
// Wire framing per the SSE spec: each event is
//
//	event: <type>
//	data: <single-line JSON payload>
//	<blank line>
//
// Comment-line heartbeats (":\n") are sent every 15s so a dead UDS
// connection surfaces inside the HTTP layer rather than waiting on
// TCP keepalives. Important for prompt reconnect on crashes.
func (s *Server) handleAgentpodEvents(w http.ResponseWriter, r *http.Request) {
	if s.AgentpodHub == nil {
		http.Error(w, "agentpod hub not configured", http.StatusServiceUnavailable)
		return
	}
	slug, ok := slugFromAgentpodPath(r.URL.Path)
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/events", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sub := s.AgentpodHub.Subscribe(slug)
	// Wake the org-stream snapshot so the agent's disconnected
	// state flips off the moment the agent pod dials in. Mirrors the
	// defer below that fires when the pod drops.
	s.NotifyOrgState()
	defer func() {
		s.AgentpodHub.Unsubscribe(sub)
		s.handleAgentpodDisconnect(slug)
		s.NotifyOrgState()
	}()

	// Initial heartbeat so the client sees a healthy stream open
	// immediately (handy for tests, no-op otherwise).
	if _, err := w.Write([]byte(": ok\n\n")); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := s.clk().NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.closed:
			// Hub dropped us for being slow. Bail; the agent will
			// reconnect with backoff.
			return
		case ev := <-sub.ch:
			if err := writeSSEFrame(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C():
			if _, err := w.Write([]byte(": hb\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeSSEFrame writes a single Event as an SSE frame to w. JSON
// payloads are written single-line (we control the encoder) so the
// agent's reader doesn't need to handle multi-line data: continuation.
func writeSSEFrame(w http.ResponseWriter, ev agentpod.Event) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", ev.Type); err != nil {
		return err
	}
	if len(ev.Data) > 0 {
		if _, err := fmt.Fprintf(w, "data: %s\n", ev.Data); err != nil {
			return err
		}
	}
	_, err := w.Write([]byte("\n"))
	return err
}

// slugFromAgentpodPath extracts the slug from a path of the form
// /v1/agent/{slug}/events. Returns "", false on any other shape OR on
// a slug that doesn't pass message.ValidateSlug — the format check
// closes a defense-in-depth gap where a slug like ".." would resolve
// `agents/<slug>/x` to a path outside the agents tree after Clean.
func slugFromAgentpodPath(path string) (string, bool) {
	const (
		prefix = "/v1/agent/"
		suffix = "/events"
	)
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	slug := strings.TrimPrefix(path, prefix)
	slug = strings.TrimSuffix(slug, suffix)
	if message.ValidateSlug(slug) != nil {
		return "", false
	}
	return slug, true
}

// PublishAgentpodEvent is the typed publisher Server callers use to
// send a chat-turn (or other) event to the agent runtime for slug.
// Marshals the typed payload to JSON, builds the Event envelope,
// hands to the hub. Dropping subscribers is the hub's concern.
func (s *Server) PublishAgentpodEvent(slug string, kind agentpod.EventType, payload any) error {
	if s.AgentpodHub == nil {
		return fmt.Errorf("agentpod hub not configured")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", kind, err)
	}
	s.AgentpodHub.Publish(slug, agentpod.Event{Type: kind, Data: body})
	return nil
}

// handleAgentpodTurnEvent receives one in-turn event from an agent
// runtime via POST /v1/agent/{slug}/chat-turn/{turn-id}/event. The
// event types mirror provider.StreamEvent (delta, tool_use_*, tool_result)
// plus done/failed terminators. Stream events drive chat.jsonl
// persistence + chatHub broadcasts so the browser-facing UI stays in
// sync, split across event-per-POST instead of an in-process event
// channel.
//
// Per-slug active-turn state lives on Server.agentpodTurns;
// publishAgentpodChatTurn installs the entry via
// InitAgentpodTurnState before publishing the chat-turn event, and
// the done/failed handler tears it down.
//
// Stale events (no matching entry) are 204'd silently — typically a
// race where a prior turn's event lands after its done handler
// already ran. Erroring would force the agent pod into spurious
// retry without giving the chat any stuck recovery path.
func (s *Server) handleAgentpodTurnEvent(w http.ResponseWriter, r *http.Request) {
	slug, turnID, ok := slugAndTurnFromAgentpodTurnPath(r.URL.Path)
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/chat-turn/{turn-id}/event", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	var ev agentpod.TurnEvent
	if err := decodeJSONBounded(w, r, &ev); err != nil {
		http.Error(w, "decode TurnEvent: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch ev.Kind {
	case agentpod.TurnEventDelta,
		agentpod.TurnEventThinking,
		agentpod.TurnEventToolUseStart,
		agentpod.TurnEventToolInputDelta,
		agentpod.TurnEventToolUseEnd,
		agentpod.TurnEventToolResult,
		agentpod.TurnEventError:
		// Streamed in-flight events. Look up the matching turn-id —
		// subagent map first (multiple subagent turns can be in
		// flight per slug), then per-slug chat-turn map. A stale
		// event from a long-finished turn (no matching state) is
		// dropped.
		st := s.lookupAgentpodTurnByID(slug, turnID)
		if st == nil {
			break
		}
		switch ev.Kind {
		case agentpod.TurnEventDelta:
			s.onAgentpodDelta(st, ev)
		case agentpod.TurnEventThinking:
			s.onAgentpodThinking(st, ev)
		case agentpod.TurnEventToolUseStart:
			s.onAgentpodToolUseStart(st, ev)
		case agentpod.TurnEventToolInputDelta:
			s.onAgentpodToolInputDelta(st, ev)
		case agentpod.TurnEventToolUseEnd:
			s.onAgentpodToolUseEnd(st, ev)
		case agentpod.TurnEventToolResult:
			s.onAgentpodToolResult(st, ev)
		case agentpod.TurnEventError:
			s.onAgentpodError(st, ev)
		}
	case agentpod.TurnEventFailed:
		// LoopExitSubprocessDied carrier across UDS: write a
		// KindRuntimeDisruption row + run the post-loop teardown so
		// the spawn gate flips to SpawnNow on the next inbound. A
		// stale failed (no matching state) still writes the disruption
		// row for the parent's chat path — this is the resumption
		// signal even when the chat hub has already been torn down —
		// but skips the teardown block.
		st := s.lookupAgentpodTurnByID(slug, turnID)
		if st == nil {
			if ev.FailedReason == agentpod.FailedReasonSubprocessDied {
				detail := ev.FailedDetail
				if detail == "" {
					detail = "(no detail)"
				}
				// nil hub: by definition there is no live turn state
				// here, so no SSE subscriber to update. The disruption
				// row lands in chat.jsonl; the next GET of the chat
				// API carries it.
				s.finalizeSubprocessDisruption(slug, nil, fmt.Errorf("agent runtime reported subprocess-died: %s", detail))
				// Clear the active-turn marker too. Without this, a
				// Kivali crash before the next turn overwrites the
				// marker would cause RecoverInterruptedTurns to write
				// a SECOND disruption row on next boot — three
				// consecutive disruptions trips the quarantine.
				if cerr := s.Store.ClearActiveTurn(slug); cerr != nil {
					log.Printf("agentpod turn %s: clear active turn after stale subprocess-died: %v", slug, cerr)
				}
			}
			break
		}
		s.onAgentpodFailed(st, ev)
	case agentpod.TurnEventFolded:
		// The fate of a message we handed to a running turn. A stale
		// one (turn already gone) is dropped: flushAndComplete has
		// drained the buffer by then, so the message is in chat.jsonl
		// regardless of which way this would have gone.
		st := s.lookupAgentpodTurnByID(slug, turnID)
		if st == nil {
			break
		}
		s.onAgentpodFolded(st, ev)
	case agentpod.TurnEventDone:
		st := s.lookupAgentpodTurnByID(slug, turnID)
		if st == nil {
			break
		}
		s.onAgentpodDone(st, ev)
	default:
		http.Error(w, fmt.Sprintf("unknown TurnEvent kind: %q", ev.Kind), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentpodChatHistory serves GET /v1/agent/{slug}/chat/history.
// Returns the agent's chat.jsonl content as a JSON array of
// store.ChatMessage. The agent runtime's serializeHistory and the
// MCP search_past_chats tool both call this when they need the
// history, which the pod cannot read from the volume directly.
func (s *Server) handleAgentpodChatHistory(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/chat/history")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	hist, err := s.Store.ReadChatHistory(slug)
	if err != nil {
		http.Error(w, "read history: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if hist == nil {
		hist = []store.ChatMessage{} // never null over JSON
	}
	writeJSON(w, http.StatusOK, hist)
}

// handleAgentpodChatAppend serves POST /v1/agent/{slug}/chat/append.
// Body is one store.ChatMessage. Used by the agent runtime's
// chat-turn driver to land tool_use / tool_result / direct_chat
// rows on chat.jsonl. The single-writer (core) invariant holds via
// store.AppendChatMessage's per-agent mutex.
func (s *Server) handleAgentpodChatAppend(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/chat/append")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	var msg store.ChatMessage
	if err := decodeJSONBounded(w, r, &msg); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.AppendChatMessage(slug, msg); err != nil {
		http.Error(w, "append: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentpodRole serves GET / POST /v1/agent/{slug}/role —
// reads/writes the agent's role.md. role.md is set at hire time
// and rewritten on rotation; the agent runtime caches it via ETag
// (path-addressed cache class — see internal/agentpod/cache.go).
//
// GET emits an ETag header on every 200; clients send If-None-Match
// on subsequent reads and get 304 when the body hasn't changed.
func (s *Server) handleAgentpodRole(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/role")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		body, err := s.Store.ReadRole(slug)
		if err != nil {
			http.Error(w, "read role: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONWithETag(w, r, map[string]string{"body": body}, agentpod.ETag([]byte(body)))
	case http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		if err := decodeJSONBounded(w, r, &body); err != nil {
			http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Store.WriteRole(slug, body.Body); err != nil {
			http.Error(w, "write role: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAgentpodMemory serves GET / POST /v1/agent/{slug}/memory —
// reads/writes the agent's agent_memory.md. The memory is the
// curated summary the agent rewrites on rotation (and via the
// agent_memory_* tools mid-chat); the agent runtime's MCP tool
// handlers + system-prompt cache reach for it via this endpoint.
//
// POST uses an "append" flag so the agent_memory_append tool's
// semantics survive the round trip. Default (append=false) is a
// full overwrite.
func (s *Server) handleAgentpodMemory(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/memory")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		body, err := s.Store.ReadAgentMemory(slug)
		if err != nil {
			http.Error(w, "read memory: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONWithETag(w, r, map[string]string{"body": body}, agentpod.ETag([]byte(body)))
	case http.MethodPost:
		var body struct {
			Body   string `json:"body"`
			Append bool   `json:"append,omitempty"`
		}
		if err := decodeJSONBounded(w, r, &body); err != nil {
			http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
			return
		}
		var werr error
		if body.Append {
			werr = s.Store.AppendAgentMemory(slug, body.Body)
		} else {
			werr = s.Store.WriteAgentMemory(slug, body.Body)
		}
		if werr != nil {
			http.Error(w, "write memory: "+werr.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAgentpodMemoryDispatch serves
// POST /v1/agent/{slug}/memory/dispatch — the agent pod's MCP
// agent_memory_* tool calls route here instead of mutating
// agent_memory.md against a local FSStore. One endpoint covers both
// tools (append + str_replace),
// validation runs server-side under store.AppendAgentMemory /
// store.StrReplaceAgentMemory's locking, and the response carries
// the same (body, isError) shape agent.DispatchAgentMemoryTool
// produces in-process.
func (s *Server) handleAgentpodMemoryDispatch(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/memory/dispatch")
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/memory/dispatch", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.MemoryDispatchRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "decode MemoryDispatchRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Operating principles are rewritten only during the rotation turn.
	// This is the one chokepoint every principles write crosses (the
	// pod has no /data), so the gate lives here rather than in the tool
	// list: the MCP roster stays static — no respawn to add or remove a
	// tool per turn — and the refusal reads back to the model as an
	// ordinary tool error it can act on.
	if agent.IsAgentHabitsMutation(req.Tool) && !s.hasRotationPending(slug) {
		writeJSON(w, http.StatusOK, agentpod.MemoryDispatchResponse{
			Body:    habitsOutsideRotationMessage,
			IsError: true,
		})
		return
	}
	body, isErr := agent.DispatchAgentMemoryTool(s.Store, slug, req.Tool, req.Input)
	writeJSON(w, http.StatusOK, agentpod.MemoryDispatchResponse{Body: body, IsError: isErr})
}

// habitsOutsideRotationMessage is the tool_result an agent sees
// when it tries to write its habits outside a rotation
// turn. It says where the thought goes instead so the model does not
// retry — and it does NOT suggest a mid-chat memory write, which
// would cost a runtime restart and a full-conversation cache miss for
// something the rotation prompt will read out of the chat anyway.
const habitsOutsideRotationMessage = "habits are rewritten only during a chat rotation; this chat is not rotating. If this is a durable rule worth keeping, say so in your reply — the rotation prompt reads this chat, and that is when it moves to habits."

// handleAgentpodShareFileDispatch serves
// POST /v1/agent/{slug}/share-file/dispatch — the agent pod's MCP
// share_file tool routes here. Server resolves each path entry
// (agent.ResolveShareFilePath) and appends ONE chat row carrying
// every resolved attachment, so a multi-file share lands as a
// single bubble.
//
// Ack body shape (parsed back by agent.parseShareFileAck for the
// live UI emit):
//
//	Shared in this conversation:
//	- <name> (sha=<sha>)
//	- <name> (sha=<sha>)
//	[Caption: <text>]
func (s *Server) handleAgentpodShareFileDispatch(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/share-file/dispatch")
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/share-file/dispatch", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.ShareFileDispatchRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "decode ShareFileDispatchRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		writeJSON(w, http.StatusOK, agentpod.ShareFileDispatchResponse{Body: "share_file: files[] is required and must contain at least one entry", IsError: true})
		return
	}
	atts := make([]store.MessageAttachment, 0, len(req.Files))
	for i, entry := range req.Files {
		resolved, rerr := agent.ResolveShareFilePath(r.Context(), s.Store, slug, entry.Path)
		if rerr != nil {
			writeJSON(w, http.StatusOK, agentpod.ShareFileDispatchResponse{
				Body:    fmt.Sprintf("share_file: files[%d]: %s", i, rerr.Error()),
				IsError: true,
			})
			return
		}
		display := strings.TrimSpace(entry.Name)
		if display == "" {
			display = resolved.Name
		}
		atts = append(atts, store.MessageAttachment{SHA: resolved.SHA, Name: display})
	}
	caption := strings.TrimSpace(req.Caption)
	bubble := caption
	if bubble == "" {
		if len(atts) == 1 {
			bubble = "Shared file: " + atts[0].Name
		} else {
			bubble = fmt.Sprintf("Shared %d files", len(atts))
		}
	}
	if err := s.Store.AppendChatMessage(slug, store.ChatMessage{
		Role:        store.RoleSent,
		Kind:        "file_shared",
		Content:     bubble,
		Attachments: atts,
		ToolName:    agent.ShareFileToolName,
		TS:          time.Now().UTC(),
	}); err != nil {
		writeJSON(w, http.StatusOK, agentpod.ShareFileDispatchResponse{Body: "share_file: append chat: " + err.Error(), IsError: true})
		return
	}
	var ack strings.Builder
	ack.WriteString("Shared in this conversation:\n")
	for _, a := range atts {
		fmt.Fprintf(&ack, "- %s (sha=%s)\n", a.Name, a.SHA)
	}
	if caption != "" {
		fmt.Fprintf(&ack, "Caption: %s\n", caption)
	}
	writeJSON(w, http.StatusOK, agentpod.ShareFileDispatchResponse{Body: strings.TrimRight(ack.String(), "\n")})
}

// handleAgentpodArtifactDispatch serves
// POST /v1/agent/{slug}/artifact/dispatch — artifact_publish and
// artifact_unpublish, from the agent or one of its subagents. Core is
// the only writer of the published trees: the store copies from the
// caller's workspace (store.Publish) or removes (store.Unpublish),
// runs the index, and the reply says what the graph made of it.
func (s *Server) handleAgentpodArtifactDispatch(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/artifact/dispatch")
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/artifact/dispatch", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.ArtifactDispatchRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "decode ArtifactDispatchRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch req.Tool {
	case agent.ArtifactPublishToolName:
		rep, err := s.Store.Publish(store.PublishRequest{
			Owner:      slug,
			SubagentID: strings.TrimSpace(req.SubagentID),
			Source:     req.Source,
			Dest:       req.Dest,
		})
		if err != nil {
			body := req.Tool + ": " + err.Error()
			if len(rep.Files) > 0 {
				body += "\n" + agent.RenderPublishReport(rep)
			}
			writeJSON(w, http.StatusOK, agentpod.ArtifactDispatchResponse{Body: body, IsError: true})
			return
		}
		if rep.IndexErr != nil {
			log.Printf("artifact_publish %s: graph index: %v", slug, rep.IndexErr)
		}
		writeJSON(w, http.StatusOK, agentpod.ArtifactDispatchResponse{Body: agent.RenderPublishReport(rep)})
	case agent.ArtifactUnpublishToolName:
		rep, err := s.Store.Unpublish(slug, req.Path)
		if err != nil {
			writeJSON(w, http.StatusOK, agentpod.ArtifactDispatchResponse{Body: req.Tool + ": " + err.Error(), IsError: true})
			return
		}
		if rep.IndexErr != nil {
			log.Printf("artifact_unpublish %s: graph index: %v", slug, rep.IndexErr)
		}
		writeJSON(w, http.StatusOK, agentpod.ArtifactDispatchResponse{Body: agent.RenderUnpublishReport(rep)})
	default:
		writeJSON(w, http.StatusOK, agentpod.ArtifactDispatchResponse{Body: fmt.Sprintf("unknown artifact tool %q", req.Tool), IsError: true})
	}
}

// handleAgentpodPastChatsList serves
// GET /v1/agent/{slug}/past-chats — returns the list of archived
// chat generations (Slug + Timestamp) for the slug. Mirrors
// store.FSStore.ListArchivedChats.
func (s *Server) handleAgentpodPastChatsList(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/past-chats")
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/past-chats", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	out, err := s.Store.ListArchivedChats(slug)
	if err != nil {
		http.Error(w, "list past chats: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []store.ArchivedChat{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentpodPastChatsRead serves
// GET /v1/agent/{slug}/past-chats/{ts} — returns the full
// chat.jsonl messages for one archived generation. Mirrors
// store.FSStore.ReadArchivedChat. Returns 404 if no archive at
// that timestamp.
func (s *Server) handleAgentpodPastChatsRead(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	ts := r.PathValue("ts")
	if slug == "" || ts == "" {
		http.Error(w, "bad path: expected /v1/agent/{slug}/past-chats/{ts}", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	hist, err := s.Store.ReadArchivedChat(slug, ts)
	if err != nil {
		// Distinguish not-found from real I/O errors so the client
		// can surface "no such archive" without 5xx noise.
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no archived chat at "+ts, http.StatusNotFound)
			return
		}
		http.Error(w, "read past chat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if hist == nil {
		hist = []store.ChatMessage{}
	}
	writeJSON(w, http.StatusOK, hist)
}

// handleAgentpodStateDispatch serves
// POST /v1/agent/{slug}/state/dispatch — the agent pod's MCP
// state-tool family (get_org_chart, list_skills, graph_query,
// graph_node, the assignment tools, read_agent_role) routes here.
// Server-side flow reuses mcp.DispatchStateToolInProcess
// against this server's FSStore so rendering stays in lock-step
// with the in-process MCP fleet.
//
// IsChiefOfStaff is derived from the URL slug, which checkSlugHeader
// has already verified equals the request's X-Kivali-Agent header.
// The header is set by the in-pod MCP subprocess at boot from its
// `--agent <slug>` flag; the slug is the bound caller's identity.
// Read-agent-role's CoS-only gate is therefore enforced against the
// caller, not against a wire-controlled hint.
func (s *Server) handleAgentpodStateDispatch(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/state/dispatch")
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/state/dispatch", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.StateDispatchRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "decode StateDispatchRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	body, isErr := mcp.DispatchStateToolInProcess(mcp.StateDispatchDeps{
		Store:          s.Store,
		Slug:           slug,
		IsChiefOfStaff: slug == "chief-of-staff",
		Tracker:        s.tracker(),
	}, req.Tool, req.Input)
	writeJSON(w, http.StatusOK, agentpod.StateDispatchResponse{Body: body, IsError: isErr})
}

// newStorePublisherFor constructs a two-phase StorePublisher bound
// to this server's Store + Messenger for the given slug. The
// ApplyRoute closure mutates the unlocked MessageQueue in place via
// the Messenger's RouteProduced; CommitPublish holds the queue lock
// for the duration so concurrent commits + Release batches serialize
// correctly.
func (s *Server) newStorePublisherFor(slug string) *mcp.StorePublisher {
	dispatcher := files.Dispatcher{DataDir: s.Store.Root()}
	return mcp.NewStorePublisher(mcp.StorePublishDeps{
		Store:             s.Store,
		FilesystemBackend: dispatcher.BackendFor(slug),
		From:              slug,
		ApplyRoute: func(ctx context.Context, q *store.MessageQueue, m store.Message) error {
			out := &messaging.RouteOutcome{}
			rerr := s.Messenger.RouteProduced(ctx, q, []store.Message{m}, out)
			if rerr != nil {
				return rerr
			}
			if len(out.Warnings) > 0 {
				// Surface route warnings (e.g. recipient archived →
				// dead-letter) as a single error string so commit's
				// rendered body carries the same "[warning] route:"
				// suffix the in-process path produces.
				return errors.New(strings.Join(out.Warnings, "; "))
			}
			return nil
		},
	})
}

// handleAgentpodPublishStage serves
// POST /v1/agent/{slug}/publish/stage — phase 1 of the two-phase
// publish flow. Runs parse → role-conflict check → attachment resolve
// against this server's authoritative Store; persists a staged-publish
// record and returns the StageID. No message file is written and no
// queue mutation happens here; durable side effects are deferred to
// /publish/commit.
//
// Returns 200 with PublishStageResponse on parse / conflict /
// attachment failures (IsError=true, no StageID); the bridge surfaces
// the rendered body directly to the model without retry. 4xx / 5xx
// indicates transport-class failures the bridge MAY retry.
func (s *Server) handleAgentpodPublishStage(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/publish/stage")
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/publish/stage", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil || s.Messenger == nil {
		http.Error(w, "publish dispatcher not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.PublishStageRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "decode PublishStageRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	publisher := s.newStorePublisherFor(slug)
	stageID, body, isErr, derr := publisher.StagePublish(r.Context(), req.Tool, req.Input)
	if derr != nil {
		http.Error(w, "stage: "+derr.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, agentpod.PublishStageResponse{
		StageID: stageID,
		Body:    body,
		IsError: isErr,
	})
}

// handleAgentpodPublishCommit serves
// POST /v1/agent/{slug}/publish/commit — phase 2 of the two-phase
// publish flow. Writes the message file at the deterministic path
// from the staged record, atomically appends the recipient inbox
// entry AND records StageID in the queue's CommittedStageIDs set
// under one queue-lock acquisition.
//
// Idempotent on StageID: a retry for an already-committed id returns
// the same 200 response shape (with a "duplicate commit suppressed"
// body). The bridge therefore can safely retry commit on any
// transport failure.
//
// A missing staged record (expired by janitor, stale bridge against
// fresh core) returns 404 — the bridge MUST treat this as
// "definitely not committed" and re-stage if it still wants to
// publish.
func (s *Server) handleAgentpodPublishCommit(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/publish/commit")
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/publish/commit", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil || s.Messenger == nil {
		http.Error(w, "publish dispatcher not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.PublishCommitRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "decode PublishCommitRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.StageID == "" {
		http.Error(w, "commit: empty stage_id", http.StatusBadRequest)
		return
	}
	publisher := s.newStorePublisherFor(slug)
	body, isErr, derr := publisher.CommitPublish(r.Context(), req.StageID)
	if derr != nil {
		if errors.Is(derr, store.ErrStagedPublishNotFound) {
			http.Error(w, "commit: stage_id not found", http.StatusNotFound)
			return
		}
		http.Error(w, "commit: "+derr.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, agentpod.PublishCommitResponse{Body: body, IsError: isErr})
}

// slugFromAgentpodPath2 is the generic helper for paths of the form
// /v1/agent/{slug}<suffix>. Returns "", false on any other shape OR
// on a slug that doesn't pass message.ValidateSlug.
func slugFromAgentpodPath2(path, suffix string) (string, bool) {
	const prefix = "/v1/agent/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	slug := strings.TrimPrefix(path, prefix)
	slug = strings.TrimSuffix(slug, suffix)
	if message.ValidateSlug(slug) != nil {
		return "", false
	}
	return slug, true
}

// checkSlugHeader compares the request's X-Kivali-Agent header to the
// agentpodMaxJSONBody caps every JSON body decoded off the UDS
// surface. The agent-pod socket authenticates by filesystem
// permission only — the calling pod could be a compromised or buggy
// agent runtime. Without an upper bound, json.NewDecoder buffers the
// entire body into memory before unmarshal, so a multi-GB payload
// would OOM the Kivali web pod. Realistic payloads are bounded by
// the memory-edit cap (~64KB) for memory tools, attachment metadata
// (~few KB) for state tools, and the largest legitimate JSON shape
// is the turn-event delta stream (typically <1MB; bursty for huge
// tool_inputs). 16MB covers all realistic shapes with comfortable
// headroom and rejects DoS shapes outright.
const agentpodMaxJSONBody = 16 << 20

// decodeJSONBounded wraps r.Body in http.MaxBytesReader and decodes
// into v. Required for every JSON-body decode on the UDS surfaces;
// see agentpodMaxJSONBody for rationale.
func decodeJSONBounded(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, agentpodMaxJSONBody)
	return json.NewDecoder(r.Body).Decode(v)
}

// path-derived slug and writes a 403 on mismatch or when the header
// is missing. Returns true when the request may proceed.
//
// Empty-header rejection is load-bearing: every agent pod mounts the
// same hostPath UDS and runs as the same UID, so any pod's process
// can dial the socket directly. Without this gate, an attacker pod
// can simply omit the header and address requests to any other
// agent's slug — turning the slug header from a binding check into
// a courtesy.
func checkSlugHeader(w http.ResponseWriter, r *http.Request, slug string) bool {
	hdr := agentpod.SlugFromHeader(r.Header)
	if hdr == "" || hdr != slug {
		http.Error(w, fmt.Sprintf("slug mismatch: header=%q path=%q", hdr, slug), http.StatusForbidden)
		return false
	}
	return true
}

// handleAgentpodAttachmentMeta serves GET /v1/attachments/{sha}.
// Returns the Attachment struct (sha + name + ext + MIME +
// canonical name + size + created_at). Does not return bytes —
// see /blob and /canonical for those.
//
// Content-addressed reads are not slug-scoped at the wire layer;
// auth is the UDS file mode + same-UID. Tool-layer access checks
// (AttachmentSHAsForAgent) live higher up.
func (s *Server) handleAgentpodAttachmentMeta(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	att, err := s.Store.GetAttachment(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, att)
}

// handleAgentpodAttachmentBlob serves GET /v1/attachments/{sha}/blob.
// Returns the original bytes as application/octet-stream. Caller
// inspects the Content-Type the metadata endpoint returns to know
// the actual MIME type.
func (s *Server) handleAgentpodAttachmentBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	rc, err := s.Store.OpenAttachmentOriginal(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// handleAgentpodAttachmentCanonical serves GET
// /v1/attachments/{sha}/canonical. Returns the canonical text form
// (PDF text-extracted; office text-extracted; markdown as-is) or
// empty body when no canonical exists (image / unknown binary).
// Cap-driven by store.MaxInlinedFileBytes.
func (s *Server) handleAgentpodAttachmentCanonical(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	text, err := s.Store.ReadAttachmentText(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}

// handleAgentpodAttachmentAdd serves POST /v1/agent/{slug}/attachments.
// Body shape: {"name": "<original-name>", "data": "<base64-bytes>"}.
// Returns {"sha": "<hex>", ...} as JSON Attachment metadata. Slug-
// scoped (vs. the GET endpoints) because we record which agent
// originated the upload — preserves the in-process AddAttachment
// access-control bookkeeping.
func (s *Server) handleAgentpodAttachmentAdd(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodPath2(r.URL.Path, "/attachments")
	if !ok {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Name string `json:"name"`
		Data []byte `json:"data"` // JSON base64-decoded automatically
	}
	if err := decodeJSONBounded(w, r, &body); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	att, err := s.Store.AddAttachment(r.Context(), body.Name, bytes.NewReader(body.Data))
	if err != nil {
		http.Error(w, "AddAttachment: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, att)
}

// handleAgentpodProjectFiles serves GET /v1/project-files. Returns a
// JSON array of store.ProjectFile (no slug scoping — project files
// are global to the Kivali install).
func (s *Server) handleAgentpodProjectFiles(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	pfs, err := s.Store.ListProjectFiles()
	if err != nil {
		http.Error(w, "list: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if pfs == nil {
		pfs = []store.ProjectFile{}
	}
	writeJSON(w, http.StatusOK, pfs)
}

// handleAgentpodProjectFileMeta + Blob + Canonical mirror the
// attachment shape: per-sha metadata, original bytes, canonical text.
func (s *Server) handleAgentpodProjectFileMeta(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" || s.Store == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pf, err := s.Store.GetProjectFile(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, pf)
}

func (s *Server) handleAgentpodProjectFileBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" || s.Store == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rc, err := s.Store.OpenOriginal(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleAgentpodProjectFileCanonical(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" || s.Store == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rc, err := s.Store.OpenCanonical(sha)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// handleAgentpodRouteMessage routes a published message on behalf
// of the slug bound to the calling MCP subprocess. Body shape: a
// JSON-encoded store.Message.
//
// SECURITY: msg.From is overridden with the slug from the URL/header
// — the body's From field is NOT trusted. Without this override, an
// authenticated agent could publish messages claiming to be from the
// CEO or any other agent. The MCP-side parser stamps From correctly
// in normal flow, but a runtime that crafts raw HTTP to this endpoint
// bypasses the parser.
func (s *Server) handleAgentpodRouteMessage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Runtime == nil {
		http.Error(w, "engine not configured", http.StatusServiceUnavailable)
		return
	}
	var msg store.Message
	if err := decodeJSONBounded(w, r, &msg); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	msg.From = slug
	if _, err := s.Messenger.Route(r.Context(), msg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentpodRunSubagent runs a subagent batch on behalf of the
// slug bound to the calling MCP subprocess. Body shape: {"parent":
// <slug>, "arguments": <subagent tool input>}.
//
// SECURITY: parent is overridden with the URL/header slug — the
// body's parent field is NOT trusted. Same threat as above:
// preventing a malicious runtime from claiming a different parent.
//
// Stop-press gate: when the slug's chatHub has interruptRequested set
// (Stop click landed but the runner-kill hasn't taken effect yet —
// at most a 2s window, see gracefulInterruptDeadline), refuse the
// batch with 409. The parent CLI sees the gated tool_result and the
// runner's routeEvent triggers gracefulKill on it — so the gate
// both prevents wasted subagent work and accelerates the kill.
func (s *Server) handleAgentpodRunSubagent(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	// Stop-press gate fires before any service-availability checks so
	// a click that lands during a misconfigured deploy still gates
	// (and the gate is the cheaper test, so it's the right early-out
	// regardless).
	s.streamMu.Lock()
	hub := s.chatHubs[slug]
	s.streamMu.Unlock()
	if hub != nil && hub.peekInterruptRequested() {
		http.Error(w, "[stopped by user]", http.StatusConflict)
		return
	}
	if s.SubagentService == nil {
		http.Error(w, "subagent service not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Parent           string          `json:"parent"`
		CallerSubagentID string          `json:"caller_subagent_id"`
		Arguments        json.RawMessage `json:"arguments"`
	}
	if err := decodeJSONBounded(w, r, &body); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Parent = slug

	// One endpoint, two contracts, told apart by who is calling. A
	// durable agent sends no caller id and gets the async receipt; a
	// sub-lead names its own job and blocks for the answers.
	//
	// The caller id is a claim, not a credential — but it is only ever
	// used to LOOK UP a job that must already belong to this verified
	// slug and must still be running, so the worst a wrong one does is
	// fail. Note what is absent: no field here lets a caller state its
	// own depth. That is derived from the job core registered.
	if id := strings.TrimSpace(body.CallerSubagentID); id != "" {
		rendered, err := s.SubagentService.RunNestedBatch(r.Context(), body.Parent, id, body.Arguments)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(rendered))
		return
	}

	rendered, err := s.SubagentService.StartBatch(r.Context(), body.Parent, body.Arguments)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(rendered))
}

// handleAgentpodSubagentStatus serves the caller's background-job
// list, already rendered as the text the model will see. Rendering
// server-side keeps one description of a job's state rather than one
// here and another in the MCP subprocess.
//
// No Stop-press gate, unlike run-subagent: reading what is running
// starts no work and costs nothing, and an agent mid-stop asking what
// it had outstanding is a reasonable question.
func (s *Server) handleAgentpodSubagentStatus(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.SubagentService == nil {
		http.Error(w, "subagent service not configured", http.StatusServiceUnavailable)
		return
	}
	// Body is ignored beyond decoding: parent comes from the bound
	// slug, never from what the caller claims.
	var body struct {
		Parent string `json:"parent"`
	}
	_ = decodeJSONBounded(w, r, &body)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.SubagentService.SubagentStatus(slug)))
}

// handleAgentpodSubagentCancel stops one of the caller's background
// jobs. Ownership is enforced inside CancelSubagent against the bound
// slug, so an agent cannot cancel another's work by guessing an id.
func (s *Server) handleAgentpodSubagentCancel(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.SubagentService == nil {
		http.Error(w, "subagent service not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Parent string `json:"parent"`
		ID     string `json:"id"`
	}
	if err := decodeJSONBounded(w, r, &body); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s.SubagentService.SubagentCancel(slug, body.ID)))
}

// handleAgentpodSkillsList serves GET /v1/skills. Returns the
// catalog of skills (name, description, when-to-use, file tree).
// Globally scoped — skills are app-wide, not per-agent.
func (s *Server) handleAgentpodSkillsList(w http.ResponseWriter, _ *http.Request) {
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	skills, err := s.Store.ListEnabledSkills()
	if err != nil {
		http.Error(w, "list skills: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if skills == nil {
		skills = []store.Skill{}
	}
	writeJSON(w, http.StatusOK, skills)
}

// handleAgentpodSkill serves GET /v1/skills/{name}. Returns one
// skill's metadata + sorted file tree. 404 when not found.
func (s *Server) handleAgentpodSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || s.Store == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sk, err := s.Store.ReadSkill(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

// handleAgentpodSkillManifest serves GET
// /v1/skills/{name}/manifest. Returns the raw SKILL.md body as
// text/markdown. Emits an ETag and respects If-None-Match so the
// agent-pod cache can short-circuit on unchanged manifests (skills
// are path-addressed in the cache classification).
func (s *Server) handleAgentpodSkillManifest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || s.Store == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body, err := s.Store.ReadSkillManifest(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	bytesBody := []byte(body)
	etag := agentpod.ETag(bytesBody)
	w.Header().Set("ETag", etag)
	if matchETag(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytesBody)
}

// handleAgentpodSkillFile serves GET
// /v1/skills/{name}/files/{rest...}. Returns the raw bytes of one
// file inside a skill directory. ETag/If-None-Match support so the
// agent-pod cache materializes once and re-validates cheaply
// thereafter.
//
// Path is parsed manually because Go's ServeMux doesn't bind
// trailing-slash wildcards to typed PathValue calls — we strip the
// known prefix and use the remainder as the rel path.
func (s *Server) handleAgentpodSkillFile(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/skills/"
	const filesSep = "/files/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	idx := strings.Index(rest, filesSep)
	if idx <= 0 {
		http.Error(w, "bad path: expected /v1/skills/{name}/files/{rel}", http.StatusBadRequest)
		return
	}
	name := rest[:idx]
	rel := rest[idx+len(filesSep):]
	if name == "" || rel == "" || s.Store == nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body, err := s.Store.ReadSkillFile(name, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	etag := agentpod.ETag(body)
	w.Header().Set("ETag", etag)
	if matchETag(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeJSON is a tiny helper for the agentpod handlers —
// JSON-encodes v with the given status code. On encode failure
// falls back to a 500 with the marshaling error so the agent
// runtime gets a useful message instead of a hung connection.
func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// writeJSONWithETag is writeJSON's cache-aware sibling: emits ETag
// on every 200, and short-circuits to 304 when the request's
// If-None-Match matches. The etag should be the canonical
// agentpod.ETag(body) value where body is the *content* the caller
// is exposing (not the JSON envelope) — e.g. the role.md text, not
// `{"body": ...}`. Keeping etag scoped to the underlying content
// keeps it stable across envelope changes.
func writeJSONWithETag(w http.ResponseWriter, r *http.Request, v any, etag string) {
	w.Header().Set("ETag", etag)
	if matchETag(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// matchETag reports whether the request's If-None-Match header
// names etag. Honors the multi-value list form ("a", "b") and the
// "*" wildcard. Used by every cache-validating handler so the
// equality rule lives in one place.
func matchETag(r *http.Request, etag string) bool {
	header := r.Header.Get("If-None-Match")
	if header == "" || etag == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// handleAgentpodSessionID serves the per-agent claude CLI session-id
// endpoints used by the agentpod runtime's UDS-backed SessionStore
// (claudeagent.SessionStore). Routes by HTTP method:
//
//	GET    → return persisted id (200 with body, "" when unset)
//	POST   → persist id from JSON body {"session_id":"..."}
//	DELETE → clear (idempotent; missing file is not an error)
//
// Today's claudeagent.Client uses *store.FSStore directly for these
// reads/writes; in the new architecture the claudeagent.Client lives
// inside the agent pod and goes through agentpod.Client → core →
// store, preserving the "core is authoritative" invariant.
func (s *Server) handleAgentpodSessionID(w http.ResponseWriter, r *http.Request) {
	slug, ok := slugFromAgentpodSessionPath(r.URL.Path)
	if !ok {
		http.Error(w, "bad path: expected /v1/agent/{slug}/session-id", http.StatusBadRequest)
		return
	}
	if hdr := agentpod.SlugFromHeader(r.Header); hdr != "" && hdr != slug {
		http.Error(w, fmt.Sprintf("slug mismatch: header=%q path=%q", hdr, slug), http.StatusForbidden)
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		id, err := s.Store.ReadClaudeSessionID(slug)
		if err != nil {
			http.Error(w, "read session id: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": id})
	case http.MethodPost:
		var body struct {
			SessionID string `json:"session_id"`
		}
		if err := decodeJSONBounded(w, r, &body); err != nil {
			http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.Store.WriteClaudeSessionID(slug, body.SessionID); err != nil {
			http.Error(w, "write session id: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := s.Store.ClearClaudeSessionID(slug); err != nil {
			http.Error(w, "clear session id: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// slugFromAgentpodSessionPath extracts slug from a path of the form
// /v1/agent/{slug}/session-id.
func slugFromAgentpodSessionPath(path string) (string, bool) {
	const (
		prefix = "/v1/agent/"
		suffix = "/session-id"
	)
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	slug := strings.TrimPrefix(path, prefix)
	slug = strings.TrimSuffix(slug, suffix)
	if slug == "" || strings.Contains(slug, "/") {
		return "", false
	}
	return slug, true
}

// slugAndTurnFromAgentpodTurnPath extracts slug + turn-id from a
// path of the form /v1/agent/{slug}/chat-turn/{turn-id}/event.
// Returns "", "", false on any other shape.
func slugAndTurnFromAgentpodTurnPath(path string) (slug, turnID string, ok bool) {
	const (
		prefix = "/v1/agent/"
		mid    = "/chat-turn/"
		suffix = "/event"
	)
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimSuffix(rest, suffix)
	// rest is now "{slug}/chat-turn/{turn-id}".
	idx := strings.Index(rest, mid)
	if idx <= 0 {
		return "", "", false
	}
	slug = rest[:idx]
	turnID = rest[idx+len(mid):]
	if slug == "" || turnID == "" || strings.Contains(slug, "/") || strings.Contains(turnID, "/") {
		return "", "", false
	}
	return slug, turnID, true
}
