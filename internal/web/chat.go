package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// handleAgentMessagePost appends a direct-chat message from the CEO to
// the target agent's chat.jsonl. The browser then opens an EventSource
// at /agents/<slug>/stream to receive the live response.
//
// Writes to chat.jsonl are persistent — if the browser refreshes before
// the stream starts, the user message is already saved.
func (s *Server) handleAgentMessagePost(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" || slug == agent.CEOSlug {
		http.Error(w, "cannot message this agent directly", http.StatusBadRequest)
		return
	}
	// Claude-nil is a configuration error that surfaces here, since
	// /stream spawns nothing on its own. Messaging
	// without a Claude client would silently enqueue the user's text
	// into chat.jsonl with nothing to respond — surface 503 instead so
	// the operator sees the misconfiguration.
	if s.Claude == nil {
		http.Error(w, "Claude not configured on this deployment", http.StatusServiceUnavailable)
		return
	}
	if _, err := s.Store.GetAgent(slug); err != nil {
		http.NotFound(w, r)
		return
	}
	// Support plain form-urlencoded (text only, fast path used when
	// the UI has no files to send), multipart/form-data (text + file
	// attachments), and application/json ({"text": ...}, no
	// attachments), which is what the web app sends without files.
	var text string
	var attachments []store.MessageAttachment
	contentType := r.Header.Get("content-type")
	if mt, _, _ := mime.ParseMediaType(contentType); mt == "application/json" {
		var body apitypes.MessagePostRequest
		if err := decodeJSON(r, &body); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "the message body is not valid: "+err.Error(), status)
			return
		}
		text = strings.TrimSpace(body.Text)
	} else if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			http.Error(w, "upload too large: "+err.Error(), http.StatusBadRequest)
			return
		}
		text = strings.TrimSpace(r.FormValue("text"))
		if r.MultipartForm != nil {
			for _, fh := range r.MultipartForm.File["attachment"] {
				att, err := s.ingestMessageAttachment(r, fh)
				if err != nil {
					http.Error(w, "attachment upload failed: "+fh.Filename+": "+err.Error(), http.StatusInternalServerError)
					return
				}
				attachments = append(attachments, store.MessageAttachment{SHA: att.SHA, Name: fh.Filename})
			}
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		text = strings.TrimSpace(r.FormValue("text"))
	}
	if text == "" && len(attachments) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	// Stamp the arrival time here (rather than letting deliverToAgent
	// default it) so we can echo it back in the response. The browser
	// stamps its optimistic bubble with this ts and dedups it against
	// the chat_message SSE event the spawn emits — otherwise the
	// submitting device would paint the bubble twice.
	ts := time.Now().UTC()
	// Route through deliverOrStage so this POST doesn't interleave
	// into chat.jsonl if the agent happens to be mid-response. When
	// the hub is idle this writes directly; when busy, the message is
	// staged (visible: the page shows it as pending) and offered to
	// the in-flight turn, and the reply carries its pending id.
	pendingID, err := s.deliverOrStage(slug, store.ChatMessage{
		Role:        store.RoleReceived,
		Content:     text,
		Kind:        "direct_chat",
		Attachments: attachments,
		TS:          ts,
	}, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// This request's attachments were linked under /files/attachments/
	// when its row landed (store.AppendChatMessage), or are linked
	// when it is folded into a running turn (foldText).

	// Kick off the response loop HERE, not on EventSource connect.
	// Rationale: opening the agent's page (GET /agents/.../stream)
	// should never itself cause a Claude inference — the CEO
	// clicking into an agent to read history shouldn't wake the
	// agent if nothing new happened. Spawning at POST time ties
	// the loop to an actual new user input. The browser's
	// subsequent EventSource open becomes a pure subscribe.
	//
	// When the message was buffered (agent was busy), this spawn is
	// a no-op — the active loop's defer will flush + spawn when it
	// finishes, so the buffered message still gets a response.
	s.spawnChatLoopIfIdle(slug, "chat")
	// Echo the assigned ts (and any attachments) so the browser can
	// stamp its optimistic bubble for dedup against the chat_message
	// SSE event. Both content-types return JSON now; the previous
	// text-only 204 carried no body for the client to key on.
	//
	// pending_id is present only when the message was staged behind a
	// running turn rather than written to chat.jsonl: the client then
	// paints a pending bubble keyed on it instead of a delivered one.
	w.Header().Set("content-type", "application/json")
	resp := apitypes.MessagePostResponse{TS: ts.UnixMilli(), PendingID: pendingID}
	for _, a := range attachments {
		resp.Attachments = append(resp.Attachments, apitypes.PendingAttachment{SHA: a.SHA, Name: a.Name})
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ingestMessageAttachment writes a direct-chat attachment into the
// shared blob store and returns the stored Attachment metadata. Bytes
// uploaded this way are not automatically added to the agent's file
// scope — they're scoped to this specific message.
func (s *Server) ingestMessageAttachment(r *http.Request, fh *multipart.FileHeader) (store.Attachment, error) {
	src, err := fh.Open()
	if err != nil {
		return store.Attachment{}, err
	}
	defer func() { _ = src.Close() }()
	return s.Store.AddAttachment(r.Context(), fh.Filename, src)
}

// handleAgentStream subscribes the browser to the per-agent event
// hub, if one exists. Returns 204 when nothing is in flight.
//
// Does NOT spawn a Claude inference on its own — page loads and
// refreshes on an agent page must never wake the agent. New work
// is kicked off by the POST paths that created it:
//
//   - handleAgentMessagePost (CEO direct chat) spawns via
//     spawnChatLoopIfIdle before the browser reconnects.
//   - handleCEOApprove / handleCEODeny / handleCEOAck spawn on the
//     recipient side after instant-delivering the response.
//   - The runtime runs agents itself inside /release.
//
// In every case, by the time the browser opens this EventSource,
// the hub is already populated (or there's legitimately no work
// and we should return 204 instead of silently launching an
// inference). Clicking into an agent to read history stays free of
// side effects.
func (s *Server) handleAgentStream(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" || slug == agent.CEOSlug {
		http.Error(w, "invalid target", http.StatusBadRequest)
		return
	}
	s.streamMu.Lock()
	hub := s.chatHubs[slug]
	s.streamMu.Unlock()
	// A completed-but-lingering hub still occupies chatHubs[slug] until
	// its eviction timer fires (chatHubLingerDuration). New connections
	// must NOT attach to it: its replay buffer holds the loop's terminal
	// events (e.g. done/rotation_done from a just-finished /new-chat),
	// and a client that reloads on rotation_done lands here again,
	// finds the same lingering hub, replays again, reloads again, in an
	// infinite ~50ms cycle until the linger expires.
	// Treat completed hubs as absent: existing in-flight subscribers
	// keep their hub pointer (this branch is moot for them), and
	// getOrCreateHub still finds the lingering slot to evict on a fresh
	// spawn (its eviction-race contract is unchanged).
	// The same holds from the moment the turn's `done` goes out, before
	// the hub completes (chatHub.turnEnded).
	if hub != nil && !hub.liveForPage() {
		hub = nil
	}
	if hub == nil {
		// No chat-hub for this slug — but the runtime might be
		// running this agent in parallel, in which case the client
		// should still show the turn running until the engine finishes.
		// Hold the connection open and watch the engine; emit a
		// terminal `done` when it releases the slug so the client
		// clears it. If the engine isn't running this
		// slug either, fall through to the 204 path so EventSource
		// closes cleanly.
		if s.turnInFlight(slug) {
			s.serveEngineWaitStream(w, r, slug)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	serveHubSSE(w, flusher, r.Context(), hub.hub)
}

// serveEngineWaitStream holds the per-agent SSE connection open while
// the runtime is running slug, then emits a `done` event and
// closes. Used when the agent is busy via the release-engine path (no
// chat hub exists), so the client's running state stays
// visible for the duration of the engine run instead of being yanked
// by an immediate 204→error.
//
// Event-driven: subscribes to orgHub for transitions. Every snapshot
// publish (engine markRunning, chat-hub start/stop, inbox writes) is
// our wake-up cue to re-check whether we should hand off to a chat
// hub or terminate. No polling.
func (s *Server) serveEngineWaitStream(w http.ResponseWriter, r *http.Request, slug string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")
	// Empty initial flush so the browser commits the connection
	// state — without this, some buffering proxies hold the
	// response-line until first event.
	_, _ = fmt.Fprint(w, ": engine-wait\n\n")
	flusher.Flush()

	_, snaps := s.orgHub.subscribe()
	defer s.orgHub.unsubscribe(snaps)
	hb := s.clk().NewTicker(orgStreamHeartbeat)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-hb.C():
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case _, ok := <-snaps:
			if !ok {
				// orgHub closed (only on server shutdown today).
				// Treat as done so the client can reconnect cleanly.
				writeRawSSE(w, flusher, "done", []byte(`{}`))
				return
			}
			// A snapshot published — something transitioned. The
			// snapshot itself is meant for /org/stream, not us;
			// we just use the wake-up to re-check our gating
			// conditions against authoritative server state.
			s.streamMu.Lock()
			hub := s.chatHubs[slug]
			s.streamMu.Unlock()
			// Skip lingering hubs — they're not the live work the
			// engine-wait stream is here to hand off to. Wait for an
			// actively-running hub or for the engine to release.
			if hub != nil && hub.liveForPage() {
				// A chat hub spawned mid-wait — usually
				// AfterTurnAgentComplete kicking off a follow-up
				// loop because a delivery landed during the engine
				// run. Hand the connection over to the hub: the
				// browser sees the live tool chips / deltas of the
				// follow-up loop on the SAME open EventSource, no
				// reconnect needed. Returns when the hub closes
				// or the client disconnects.
				serveHubSSE(w, flusher, r.Context(), hub.hub)
				return
			}
			if !s.turnInFlight(slug) {
				// Neither engine nor hub owns this slug → done.
				// The client ends the running state on `done`.
				writeRawSSE(w, flusher, "done", []byte(`{}`))
				return
			}
		}
	}
}

// spawnChatLoopIfIdle kicks off a chat tool loop for slug if one
// isn't already running. Idempotent: concurrent callers coalesce on
// the hub's originator ownership. Returns true when we actually
// published a chat-turn to the agent pod, false when the agent was
// already busy, the agent pod is unreachable, or there's nothing for
// them to respond to.
//
// Used by handlers that deliver a new "received" entry to an
// agent's chat.jsonl outside the release flow, so the agent reacts
// without waiting for anything else to spawn its loop.
//
// Order-invariant: by the time we build req.Messages here, chat.jsonl
// must already be in canonical order — UI, file, and model see the
// same sequence. Any reorder needed to achieve that (e.g. mid-flight
// deliveries moved after the prior loop's reply) is performed by
// spawnFollowupIfNewerReceived BEFORE calling us, by rewriting the
// file. We never quietly reorder in memory here.
//
// The actual chat tool loop runs in the per-agent agent pod. This
// function builds the CompleteRequest, marks the turn active on
// disk, and publishes an EventChatTurn over the agent-pod UDS hub.
// finalizeAgentpodTurn (agentpod_turn.go) handles the post-loop
// teardown when the pod POSTs back its done/failed event.
func (s *Server) spawnChatLoopIfIdle(slug, source string) bool {
	if slug == "" || slug == agent.CEOSlug {
		return false
	}
	if s.Claude == nil {
		return false
	}
	a, err := s.Store.GetAgent(slug)
	if err != nil {
		return false
	}
	// Cross-path coordination: if the runtime is mid-loop on this
	// agent, we must NOT spawn a second chat-turn on the same chat
	// history — they'd each fire an independent inference on
	// identical state and produce near-duplicate tool calls.
	if s.turnInFlight(slug) {
		return false
	}
	hub, isOriginator := s.getOrCreateHub(slug)
	if !isOriginator {
		// Something's already running for this slug — nothing to do.
		return false
	}
	hub.spawnSource = source
	// Rows appended under streamMu leave their files to be linked
	// once it is released; whatever is still unlinked is linked here,
	// before the turn reads a row that names it.
	s.Store.LinkChatAttachments(slug)
	hist, err := s.Store.ReadChatHistory(slug)
	if err != nil || len(hist) == 0 {
		return s.abandonSpawn(slug, hub, source)
	}
	// Spawn gate: walk chat history backward and decide. The state
	// machine in store.SpawnDecision honors the two interrupt
	// markers (KindUserInterruption from Stop, KindRuntimeDisruption
	// from boot-detected kills) on top of the basic "last entry is
	// an unanswered RoleReceived" rule:
	//
	//   - SpawnHoldForCEO: the CEO pressed Stop and hasn't yet
	//     redirected the agent. Don't spawn; the next CEO message
	//     will land AFTER the user-interruption entry and flip the
	//     verdict to SpawnNow on the next call.
	//   - SpawnNow: there's an unanswered RoleReceived OR a pending
	//     runtime-disruption to resume from.
	//   - SpawnIdle: nothing to do.
	//
	// Without this, release-all's broadcast wake would call Claude
	// for every active agent on every release, including those who
	// already replied. And without the interrupt-aware extension, a
	// Stop would be a no-op — chat.go would respawn within the same
	// second.
	verdict := store.SpawnDecision(hist)
	if verdict != store.SpawnNow {
		return s.abandonSpawn(slug, hub, source)
	}
	// Circuit breaker: if the agent has crashed-and-respawned three
	// times in a row with no productive activity in between, stop
	// auto-respawning. The CEO can re-engage via direct chat to
	// reset the count (any non-system, non-tool entry resets it
	// per store.ConsecutiveRuntimeDisruptions).
	if store.ConsecutiveRuntimeDisruptions(hist) >= chatHubQuarantineThreshold {
		log.Printf("chat %s: quarantined (%d consecutive runtime disruptions); not auto-respawning", slug, store.ConsecutiveRuntimeDisruptions(hist))
		return s.abandonSpawn(slug, hub, source)
	}
	if s.Runtime == nil {
		return s.abandonSpawn(slug, hub, source)
	}
	rt := s.Runtime
	// Nothing org-wide runs here. The /files/ farms are kept current
	// by the edges that change them (store.SyncAgentFilesystem lists
	// each), peers' published files are the pod's own mounts of the
	// published trees, and the graph index is the maintainer's, which
	// every publish has already updated: the wake note below describes
	// the index as it stands at this wake.
	ix := s.Store.Graph().Index()
	// The wake note: one RoleReceived entry of kind wake_update
	// appended behind the deliveries that woke the agent, so the model
	// reads what changed since its last turn and what it holds in the
	// same user message as the work it woke for. This is the one place
	// the runtime tells an agent such things: once per wake, never on a
	// delivery, so a message folded into a running turn brings no copy.
	// Appended here, before the spawn snapshot and the browser emit,
	// because it must be in chat.jsonl before the request is built and
	// because the spawn gate treats the kind as plumbing (it never
	// causes a wake, so appending it can never spin). See
	// docs/developers/knowledge-graph.md §"The wake note".
	var note *store.ChatMessage
	if update, gerr := agent.BuildWakeUpdate(s.Store, ix, slug); gerr != nil {
		log.Printf("chat %s: wake note: %v (continuing without it)", slug, gerr)
	} else {
		appended := true
		if update.Text != "" {
			entry := store.ChatMessage{Role: store.RoleReceived, Kind: store.KindWakeUpdate, Content: update.Text, TS: time.Now().UTC()}
			if aerr := s.Store.AppendChatMessage(slug, entry); aerr != nil {
				log.Printf("chat %s: append wake note: %v (will report again next wake)", slug, aerr)
				appended = false
			} else {
				hist = append(hist, entry)
				note = &entry
			}
		}
		// The watermark moves only once the text has landed, so a
		// failed append is reported again rather than lost.
		if appended {
			if cerr := agent.CommitWakeUpdate(s.Store, slug, update); cerr != nil {
				log.Printf("chat %s: graph watermark: %v", slug, cerr)
			}
		}
	}
	// Race-detection snapshot: the post-completion defer compares the
	// latest received TS against this. A strictly-newer TS means a
	// message arrived mid-loop and slipped past the buffer; the
	// defensive follow-up path handles that case.
	hub.spawnReceivedTS = lastRealReceivedTS(hist)
	// Broadcast the inbound message(s) that triggered this turn into the
	// hub's replay buffer BEFORE the pod starts emitting thinking/deltas,
	// so any browser that subscribes (a second device reconnecting via
	// the /org/stream reconciler, or the submitting device's own stream)
	// paints the user's bubble in order ahead of the response. The
	// wake note follows them, in file order.
	emitTriggeringChatMessages(hub, s.Provider, s.Store.Root(), hist)
	if note != nil {
		hub.Emit("wake_update", map[string]any{"content": note.Content, "ts": note.TS.UnixMilli()})
	}
	actx, err := rt.BuildChatContext(context.Background(), a, hist)
	if err != nil {
		return s.abandonSpawn(slug, hub, source)
	}
	// run_shell, share_file, and file_* tools are always exposed to
	// the chat agent — the agent pod has a LocalShell wired and
	// resolves filesystem ops via UDS to core.
	actx.IncludeShellTool = true
	actx.IncludeShareFileTool = true
	actx.IncludeFilesystemTools = true
	actx.FilesystemAvailable = true
	// The driver gets an effort valid for the model: the stored level when
	// the model offers it, else the model's default (the same value the
	// chat header and the bubble show).
	actx.Effort = s.effectiveEffort(actx.Model, actx.Effort)
	req := actx.BuildRequest()
	req.Purpose = "chat"
	if req.MaxTokens < 32768 {
		req.MaxTokens = 32768
	}
	// Mark the turn as active on disk before publishing the chat-turn
	// event. If Kivali crashes / OOMs / pod-restarts before the
	// agent-pod done handler clears this, the boot scan
	// (Server.RecoverInterruptedTurns) will see the orphan marker,
	// write a kind:runtime-disruption entry, and auto-spawn the
	// agent so they can resume. Best-effort: if the marker write
	// fails (disk full, etc.) we continue with the spawn anyway —
	// losing crash-recovery for one turn is better than refusing
	// to serve.
	if err := s.Store.MarkActiveTurn(slug, store.ActiveTurnMarker{
		StartedAt: time.Now().UTC(),
		Source:    source,
		Model:     req.Model,
	}); err != nil {
		log.Printf("chat %s: mark active turn: %v (continuing without crash-recovery marker)", slug, err)
	}
	// Publish the chat-turn event to the agent pod. Returns false
	// when no agent runtime is subscribed for slug — typically a
	// fresh hire whose pod hasn't booted yet, or a pod restart in
	// flight. We deliberately leave the active-turn marker on disk
	// so the next Kivali boot's RecoverInterruptedTurns scan writes
	// a kind:runtime-disruption row + auto-respawns once the pod
	// subscribes. The chat hub is still closed locally so the
	// browser doesn't sit on a stuck "thinking" indicator; the
	// snapshot's `disconnected` flag is the user-visible explanation
	// of why nothing came back.
	if !s.publishAgentpodChatTurn(slug, source, hub, &req) {
		log.Printf("chat %s: no agent-pod subscriber; dropping spawn (active-turn marker retained for recovery)", slug)
		return s.abandonSpawn(slug, hub, source)
	}
	return true
}

// finalizeSubprocessDisruption writes a kind:runtime-disruption chat
// entry when an agent pod reports the underlying CLI subprocess died
// mid-turn (TurnEventFailed with FailedReasonSubprocessDied). Mirrors
// the boot-recovery path's content shape (RecoverInterruptedTurns) so
// an agent inspecting their chat history sees identical "the runtime
// failed mid-task" prose regardless of whether the Kivali pod itself
// died or just the CLI.
//
// Also emits a chat_marker SSE event on hub so any browser holding the
// /stream socket open paints the disruption divider live, instead of
// requiring a page reload to see it. Pass nil hub from boot recovery
// (no open hub at startup; the user sees the divider on next load).
//
// Called from the agent-pod failed handler (onAgentpodFailed in
// agentpod_turn.go).
func (s *Server) finalizeSubprocessDisruption(slug string, hub *chatHub, cause error) {
	content := fmt.Sprintf(
		"The runtime failed mid-task: the underlying claude CLI subprocess died (%v). The Kivali server is still running; resume your prior work per the handbook.",
		cause,
	)
	msg := store.ChatMessage{
		Role:    store.RoleReceived,
		Kind:    store.KindRuntimeDisruption,
		Content: content,
		TS:      time.Now().UTC(),
	}
	if err := s.Store.AppendChatMessage(slug, msg); err != nil {
		log.Printf("chat %s: append subprocess disruption: %v", slug, err)
		return
	}
	emitChatMarker(hub, msg)
}

// finalizeTurnError writes a kind:turn-error chat entry when an agent's
// turn stopped on an error the runtime will not recover from on its
// own: the model call failed (a usage limit, an expired login, an API
// error — the CLI closes such a turn with is_error), the transport
// failed, or the agent pod went away mid-turn. It is the counterpart
// of finalizeSubprocessDisruption for everything that is NOT a crash:
// a crash is resumed automatically, an error is held until something
// new arrives, because a second attempt would hit the same wall.
//
// The row is what makes the failure durable — before it existed the
// only trace of an errored turn was a live-only red bubble that a
// reload erased, and a turn the CLI closed on an API error left a
// bubble of error text under a "<synthetic>" model pill that read as
// an ordinary reply. It drives three things: the marker row in the
// chat (live via chat_marker, on reload via the chat API's rows), the
// spawn gate's hold (store.SpawnHoldOnError), and the snapshot's
// errored state for the agent.
//
// Content is prose the model reads on its next wake through the same
// notice path as the other markers, so it says what happened and what
// to do, and carries the detail so the CEO can read the cause on the
// page without opening a log.
//
// Called from the agent-pod failed handler (onAgentpodFailed in
// agentpod_turn.go). nil hub is allowed and skips the live emit.
func (s *Server) finalizeTurnError(slug string, hub *chatHub, detail string) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		detail = "no detail was reported"
	}
	content := fmt.Sprintf(
		"The previous turn was stopped by an error and did not complete: %s. The Kivali server is still running; resume your prior work per the handbook.",
		detail,
	)
	msg := store.ChatMessage{
		Role:    store.RoleReceived,
		Kind:    store.KindTurnError,
		Content: content,
		TS:      time.Now().UTC(),
	}
	if err := s.Store.AppendChatMessage(slug, msg); err != nil {
		log.Printf("chat %s: append turn error: %v", slug, err)
		return
	}
	emitChatMarker(hub, msg)
}

// emitChatMarker pushes a chat_marker SSE event for a user-interruption,
// runtime-disruption or turn-error row to the open hub. Without it, those rows
// land in chat.jsonl but never surface in the running browser session
// until the next full reload — meaning the CEO can press Stop, see no
// visible change, and have no idea the marker is sitting in the
// transcript. nil-hub no-op so callers without a live hub (boot
// recovery) can share the same write path without branching.
func emitChatMarker(hub *chatHub, msg store.ChatMessage) {
	if hub == nil {
		return
	}
	hub.Emit("chat_marker", chatMarkerOf(msg))
}

// chatMarkerOf is the chat_marker payload for a marker row.
func chatMarkerOf(msg store.ChatMessage) apitypes.ChatMarker {
	return apitypes.ChatMarker{
		Kind:    msg.Kind,
		Content: msg.Content,
		TS:      msg.TS.UnixMilli(),
	}
}

// emitTriggeringChatMessages pushes a `chat_message` SSE event for
// each trailing unanswered direct-chat message the agent is about to
// respond to. Without it, a browser open on ANOTHER device — one that
// never saw the POST that created the message — flips to "thinking"
// (via the /org/stream reconciler reopening the agent stream) but
// never paints the user's bubble, so the agent appears to be replying
// to nothing until a manual reload renders it from chat.jsonl.
//
// The web app paints a sent message only from this event or from the
// chat API's rows, deduplicating on the row's ts, so a device that
// already has the row does not paint it twice.
//
// Scoped to direct_chat and subagent_result: other received kinds
// (inbox_delivery, ceo_queue) carry structure a plain chat_message does
// not, and reach the client through the chat API's rows. The trailing
// run is bounded by the last RoleSent entry — anything before that
// belongs to an already-answered turn.
//
// A subagent_result carries its task_result row, read under storeRoot,
// so the page paints the row the chat API serves for it.
//
// A rotation_prompt in that run is the exception among the structured
// kinds, because nothing else tells an open page that a new chat was
// asked for: the page that asked does not refetch, and any other page
// on the agent only follows the stream. It goes out as a chat_marker
// of kind rotation carrying the prompt's ts, so the page paints the
// same row the chat API serves for it, and once. The prompt's text is
// for the model and stays off the wire.
func emitTriggeringChatMessages(hub *chatHub, p provider.Provider, storeRoot string, hist []store.ChatMessage) {
	if hub == nil {
		return
	}
	// Collect newest-first, stopping at the first entry authored by the
	// agent (RoleSent text or tool_use) — that closes the prior turn.
	var triggering []store.ChatMessage
	for i := len(hist) - 1; i >= 0; i-- {
		m := hist[i]
		if m.Role == store.RoleSent {
			break
		}
		// direct_chat and subagent_result are the two kinds that both
		// TRIGGER a spawn and paint from this event. Emitting them is
		// what stops the browser showing a reply to nothing: the agent
		// starts answering a message the page never painted.
		//
		// A finished background task is the case where this bites
		// hardest, because the triggering message is never something
		// this browser typed — there is no optimistic local bubble to
		// fall back on, so without the emit the result appears only on
		// refresh.
		//
		// Deliberately NOT every received kind: inbox_delivery reaches
		// the client as a structured row of the chat API, and
		// tool_result is plumbing inside a turn, not a trigger.
		if m.Role == store.RoleReceived &&
			(m.Kind == "direct_chat" || m.Kind == store.KindSubagentResult || m.Kind == "rotation_prompt") {
			triggering = append(triggering, m)
		}
	}
	// Emit oldest-first so multi-message runs land in file order.
	for i := len(triggering) - 1; i >= 0; i-- {
		m := triggering[i]
		if m.Kind == "rotation_prompt" {
			hub.Emit("chat_marker", apitypes.ChatMarker{
				Kind: string(apitypes.MarkerKindRotation),
				TS:   m.TS.UnixMilli(),
			})
			continue
		}
		atts := make([]apitypes.PendingAttachment, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			atts = append(atts, apitypes.PendingAttachment{SHA: a.SHA, Name: a.Name})
		}
		ev := apitypes.ChatMessageEvent{
			Role:        apitypes.MessageRoleReceived,
			Kind:        m.Kind,
			Content:     m.Content,
			TS:          m.TS.UnixMilli(),
			Attachments: atts,
		}
		if m.Kind == store.KindSubagentResult {
			ev.Row = ptr(taskResultRow(p, storeRoot, hub.slug, m))
		}
		hub.Emit("chat_message", ev)
	}
}

// deliverToAgent is the one way anything external reaches an agent's
// chat.jsonl — the CEO typing, a released inbox message, a rotation
// prompt, a peer's instant-route, a finished background subagent.
//
// Three paths, and the name deliberately advertises only the first,
// because staging is a mechanism rather than the point:
//
//   - Agent idle: append directly. Done.
//   - Agent MID-TURN: stage the message in a per-agent buffer AND
//     offer it to the running turn as a fold (foldForDelivery); a fold
//     that cannot land asks the turn to wind up at its next clean step
//     boundary instead (fallBackToPreempt). finalizeAgentpodTurn
//     flushes whatever is still buffered and spawns the follow-up
//     loop, which reads it.
//   - Agent MID-ROTATION (a turn in flight AND a pending_rotation
//     marker on disk): stage the message and nothing else. That turn
//     is the memory reconcile that precedes the archive; a fold would
//     have the model answer inside a transcript about to be archived,
//     and the fallback preempt would cut the reconcile short. The turn
//     runs to its end, finalizeAgentpodTurn archives FIRST and flushes
//     SECOND, so the message opens the fresh chat and the flush spawns
//     the follow-up that answers it.
//
// The staging is not a delay policy. It exists because the turn is
// actively writing assistant text and tool rows into chat.jsonl, and
// splicing a received message into the middle of that would break
// append ordering and can break the tool-pair invariant. The message
// waits only as long as it takes the turn to reach a safe seam —
// seconds, typically — rather than the minutes the turn could take
// to finish on its own.
//
// It DOES still wait for a natural turn end in two cases: a rotation
// (above), and when there is no agentpod turn state to fold into or
// preempt (in-process runtime, or state missing). Nothing is lost, it
// just arrives later.
//
// The marker alone is NOT a reason to stage. An idle agent with a
// marker writes through: the rotation is either about to start, and
// its turn reads the message from disk, or was interrupted by a
// restart, and the recovered turn does. With no turn in flight nothing
// would flush the buffer promptly, and a message held in memory
// against a pod that may never come back is a message lost on the next
// restart.
//
// Guarantees:
//   - chat.jsonl is strictly append-ordered at all times. No
//     rewrites, no interleaves.
//   - UI order (file order) matches model order (req.Messages order).
//   - Multiple deliveries arriving during one response are batched;
//     they all flush together when the response ends, preserving
//     arrival order relative to each other.
//
// Concurrency: streamMu is held across the busy-check AND the
// direct AppendChatMessage, so a hub claim can't sneak in between
// our check and write. AppendChatMessage's internal per-agent
// mutex is independent of streamMu, so no deadlock.
//
// Callers should NOT use this for the agent's OWN output
// (tool_use, tool_result, direct_chat produced by the loop) —
// those go through AppendChatMessage directly since they're part
// of the in-flight response stream, not external deliveries.
// bufferedDelivery is one buffered external message plus the id core
// uses to track it. The id exists because a fold is answered
// asynchronously: the pod reports "this delivery landed" some time
// after we hand it over, and we need to know WHICH of possibly several
// buffered messages it means. store.ChatMessage has no stable
// identity of its own and is a persisted wire type, so the id lives
// out here rather than on it.
//
// The id is also the pending message's public identity: the CEO's
// direct chat that waits here is shown on the agent page as a pending
// bubble (visible), which the pending_* stream events and the
// /api/v1/agents/{slug}/pending/{id} actions all name by this id.
type bufferedDelivery struct {
	id  string
	msg store.ChatMessage
	// queuedAt is when the message arrived. Equal to msg.TS, which the
	// flush preserves, so the delivered row keys on the same instant
	// the pending bubble showed.
	queuedAt time.Time
	// visible marks a CEO direct_chat message: the only kind the page
	// shows while it waits. Inbox releases, subagent results, handoff
	// prompts and every other system delivery stay invisible until
	// they are in chat.jsonl, as they always were.
	visible bool
	// offered is set when the message is handed to the running turn as
	// a fold. From then on it sits in the CLI's input queue, which can
	// give back all of its contents (an interrupt) but not one message,
	// so a pending message can be deleted only while this is false.
	// Guarded by streamMu with the buffer.
	offered bool
}

func (s *Server) deliverToAgent(slug string, msg store.ChatMessage) error {
	_, err := s.deliverOrStage(slug, msg, false)
	return err
}

// deliverOrStage is deliverToAgent that says what it did: pendingID is
// empty when the message went straight into chat.jsonl, and the
// buffer id when it was staged behind a running turn. visible marks a
// CEO direct chat that the page shows as pending while it waits (see
// bufferedDelivery.visible); only handleAgentMessagePost passes true.
func (s *Server) deliverOrStage(slug string, msg store.ChatMessage, visible bool) (pendingID string, err error) {
	if msg.TS.IsZero() {
		// Stamp at arrival time. If we buffer, flush preserves this
		// stamp — chat.jsonl may show a received TS earlier than the
		// preceding sent TS, which reflects the truth: the delivery
		// ARRIVED during the reply, the reply finished later. UI
		// displays file order so this doesn't create visible
		// inversions, and debugging is easier with truthful stamps.
		msg.TS = time.Now().UTC()
	}
	s.streamMu.Lock()
	if !s.midTurnLocked(slug) {
		s.streamMu.Unlock()
		return "", s.Store.AppendChatMessage(slug, msg)
	}
	bd := bufferedDelivery{id: newAgentpodTurnID(), msg: msg, queuedAt: msg.TS, visible: visible}
	bd, foldTurnID := s.stageLocked(slug, bd, nil)
	hub := s.chatHubs[slug]
	s.streamMu.Unlock()
	hub.flushOrdered()
	s.offerFold(slug, foldTurnID, bd)
	return bd.id, nil
}

// midTurnLocked is deliverOrStage's busy check: whether a delivery for
// slug must be staged rather than written straight in. The caller
// holds streamMu.
func (s *Server) midTurnLocked(slug string) bool {
	// A lingering hub (loop done, awaiting eviction) does NOT
	// constitute "busy" — nothing is consuming the buffer. New
	// deliveries land directly on disk so a follow-up spawn (which
	// will displace the lingering hub via getOrCreateHub) reads
	// them in canonical file order.
	hub := s.chatHubs[slug]
	hubStreaming := hub != nil && !hub.isCompleted()
	// Deliberately the NARROW question: is a turn mid-stream right
	// now? Not "does this agent have work outstanding" — an agent
	// waiting on background tasks is displayed as working (see
	// agents.go) but is perfectly able to receive a message, and
	// gating on the broad predicate here would silently stop
	// deliveries from reaching it.
	return hubStreaming || s.turnInFlight(slug)
}

// stageLocked puts bd into slug's pending buffer and decides whether
// it is offered to the running turn as a fold. The caller holds
// streamMu, and after unlocking passes the returned turn id to
// offerFold; the returned delivery carries the offered flag as set.
//
// followers places the entry: it goes in front of the first of those
// ids still buffered, or at the end when none is (or followers is
// nil, as for every fresh arrival). A restored message uses it to go
// back where it was relative to what is still waiting.
//
// A visible entry's pending_message (and pending_offered) is queued on
// the hub here, under the same lock as the buffer change, so the
// stream's order of pending_* events is the buffer's order of changes:
// a flush racing this call cannot announce the delivery before the
// staging. The caller runs hub.flushOrdered after unlocking.
func (s *Server) stageLocked(slug string, bd bufferedDelivery, followers []string) (bufferedDelivery, string) {
	if s.pendingDeliveries == nil {
		s.pendingDeliveries = map[string][]bufferedDelivery{}
	}
	deliveryID := bd.id

	// A rotation turn is left alone: no fold offer, no preempt. The
	// wait is correct only because finalizeAgentpodTurn archives before
	// it flushes — the message lands in the fresh chat, not the one
	// being archived.
	//
	// Read under streamMu on purpose. handleNewChat writes the marker
	// before spawnChatLoopIfIdle claims the hub, and a claim takes this
	// lock, so a delivery that finds the hub streaming also finds the
	// marker: there is no window in which the reconcile turn is running
	// and a fold slips through to it.
	rotating := s.hasRotationPending(slug)
	if rotating {
		log.Printf("delivery hold %s: message %s waits for the rotation in progress", slug, deliveryID)
	}

	// The agent is mid-turn, so buffering alone would sit on this
	// message until the turn happens to finish. Hand it to the turn
	// instead, to be folded in at its next tool round — the turn keeps
	// running and answers this message inside it.
	//
	// We do NOT decide here whether the fold is possible; the pod
	// answers that, and answers it promptly even when it refuses. A
	// refusal (or a fold that misses the turn) comes back as
	// TurnEventFolded with Landed=false, and onAgentpodFolded falls
	// back to the wind-up-the-turn path this replaced. That keeps the
	// fallback on one code path instead of duplicating the capability
	// check on both sides of the socket.
	//
	// preemptRequested is the fallback's latch, not the fold's: once
	// something has asked this turn to wind up, folding into it would
	// race the abort, so we stop offering.
	//
	// The decision is made under the same lock that holds the state,
	// but the publish is NOT: PublishAgentpodEvent writes to the pod's
	// SSE transport, and holding streamMu across that would block
	// every other hub operation on a network write.
	var foldTurnID string
	if st := s.agentpodTurns[slug]; !rotating && st != nil && !st.preemptRequested {
		foldTurnID = st.turnID
		bd.offered = true
	}

	buf := s.pendingDeliveries[slug]
	pos := len(buf)
	if len(followers) > 0 {
		after := make(map[string]bool, len(followers))
		for _, id := range followers {
			after[id] = true
		}
		for i, pd := range buf {
			if after[pd.id] {
				pos = i
				break
			}
		}
	}
	buf = append(buf, bufferedDelivery{})
	copy(buf[pos+1:], buf[pos:])
	buf[pos] = bd
	s.pendingDeliveries[slug] = buf

	hub := s.chatHubs[slug]
	queuePendingEvent(hub, bd, "pending_message", pendingMessageOf(bd))
	if bd.offered {
		queuePendingEvent(hub, bd, "pending_offered", apitypes.PendingRef{ID: bd.id})
	}
	return bd, foldTurnID
}

// offerFold hands a just-staged delivery to the running turn identified
// by turnID, or does nothing when turnID is empty (no turn to fold
// into, a rotation in progress, or a wind-up already requested).
// Called after streamMu is released: the publish is a network write.
func (s *Server) offerFold(slug, turnID string, bd bufferedDelivery) {
	if turnID == "" {
		return
	}
	text, ok := s.foldText(slug, bd.msg)
	if !ok {
		s.fallBackToPreempt(slug, turnID, bd.id, "attachments not materialised")
		return
	}
	s.foldForDelivery(slug, turnID, bd.id, text)
}

// foldText is what a buffered message says when it is spliced into a
// running turn: its content and, when it carries files, where they
// are.
//
// The prompt renders the "--- attached files ---" block from
// chat.jsonl at the top of a turn, after the filesystem sync that
// creates the links it names. A fold has neither. The message is not
// on disk yet, and the running turn was synced before it existed, so
// folding the bare content told the model nothing about the files —
// and a path it inferred from the handbook did not resolve until
// the next turn: not for its own file_view, and not for a subagent
// handed that path, which reported the file absent as a finding
// rather than as a failed read. So the links are made here, before
// the model can hear the path, and the fold quotes the names they
// were made under.
//
// ok=false means the links could not be made. The caller then winds
// the turn up instead, and the message reaches the model the way it
// did before folds existed: flushed, synced, rendered from disk.
func (s *Server) foldText(slug string, msg store.ChatMessage) (string, bool) {
	if len(msg.Attachments) == 0 {
		return msg.Content, true
	}
	links, err := s.Store.SyncAgentAttachments(slug, msg.Attachments)
	if err != nil {
		log.Printf("delivery fold %s: attachments: %v", slug, err)
		return "", false
	}
	lines := agent.AttachedFilesLines(msg.Attachments, links, s.Store.AttachmentHasTextSidecar)
	if strings.TrimSpace(msg.Content) == "" {
		return lines, true
	}
	return msg.Content + "\n\n" + lines, true
}

// abandonSpawn gives up a hub that spawnChatLoopIfIdle claimed without
// a turn ever starting: the history was unreadable, nothing was owed,
// the agent is quarantined, the request could not be built, or no pod
// was listening. While the claim was held, deliverToAgent buffered
// anything that arrived against a turn that will now not run. Those
// deliveries are flushed into chat.jsonl in arrival order before the
// hub goes, exactly as finalizeAgentpodTurn would have, and if there
// were any the spawn is tried again so the agent answers them rather
// than leaving them to land behind whatever arrives next. Returns what
// the caller should: whether a turn ended up starting.
//
// The window is short (the history read, the wake note's write), but
// it is real, and a delivery stranded in the buffer for one turn
// is exactly the class of loss the buffering was built to prevent.
func (s *Server) abandonSpawn(slug string, hub *chatHub, source string) bool {
	flushed := s.flushAndComplete(slug, hub)
	hub.close()
	s.removeHub(slug)
	if flushed > 0 {
		return s.spawnChatLoopIfIdle(slug, source)
	}
	return false
}

// flushAndComplete is the chat-loop defer's atomic phase-1 teardown:
// under one streamMu acquisition, drains the pending-deliveries
// buffer, appends every buffered entry to chat.jsonl, and marks the
// hub completed. The hub stays in chatHubs — phase 2 (eviction) runs
// from a time.AfterFunc after the linger window so late SSE
// subscribers can still replay buffered events.
//
// Why one acquisition: deliverToAgent also holds streamMu across
// its direct AppendChatMessage call. Holding it across the drain +
// flag flip means concurrent deliveries either queue behind us
// (still see the hub as not-completed → buffer) or, once we
// release, see the completed flag and fall through to direct-append
// — strictly AFTER our batch on chatLock(slug). Without the
// atomicity, a delivery arriving between the drain and the flag
// flip would land on disk BEFORE the older buffered entries,
// scrambling arrival order.
//
// Returns the count of flushed entries so the caller can decide
// whether to spawn a follow-up loop. NotifyOrgState fires after the
// unlock so the snapshot's running state clears as soon as
// the loop is done, even though the hub itself lingers for replay.
//
// Disk I/O is held under streamMu, which is the same pattern
// deliverToAgent uses for its direct-append branch; if streamMu
// becomes a hot path under load, the right fix is per-slug
// striping in the chatHubs map, not relaxing this lock discipline.
func (s *Server) flushAndComplete(slug string, hub *chatHub) int {
	return s.flushAndCompleteMarked(slug, hub, nil)
}

// flushAndCompleteMarked is flushAndComplete with a hook, markerLocked,
// that runs under the same streamMu hold as the drain, right after it,
// and may return a marker row to write in front of the flushed
// deliveries. It serves the Send now path (kind paused-to-deliver):
// the hook reads the turn's pausedForDelivery latch, which
// sendPendingNow sets under streamMu after finding the message
// pending. Reading it under the drain's own hold — rather than in a
// separate acquisition before the drain — is what makes "answered 202,
// so the marker precedes the delivered messages" an invariant instead
// of a race: the latch cannot land between a check and the drain.
//
// The marker is written only when something is flushed: a marker that
// introduces no messages would describe a delivery that did not
// happen. The hook itself runs either way, so what it does besides
// choosing the marker (keeping the partial reply) does not depend on
// anything having been waiting.
//
// Stream order, on the dying turn's hub: the marker's chat_marker,
// then one pending_delivered per visible row. The chat_message that
// paints each row comes later, from the follow-up spawn on its own
// hub, so pending_delivered always precedes it.
func (s *Server) flushAndCompleteMarked(slug string, hub *chatHub, markerLocked func() *store.ChatMessage) int {
	s.streamMu.Lock()
	pending := s.pendingDeliveries[slug]
	delete(s.pendingDeliveries, slug)
	var marker *store.ChatMessage
	if markerLocked != nil {
		marker = markerLocked()
	}
	// Rows land under streamMu; their files are linked after it is
	// released (LinkChatAttachments below, and again at the spawn
	// that reads them), so no stream event waits on a sync.
	if marker != nil && len(pending) > 0 {
		if err := s.Store.AppendChatMessageLinkLater(slug, *marker); err != nil {
			log.Printf("flush pending delivery %s: append %s marker: %v", slug, marker.Kind, err)
		} else {
			hub.queueOrdered("chat_marker", chatMarkerOf(*marker))
		}
	}
	for _, pd := range pending {
		if err := s.Store.AppendChatMessageLinkLater(slug, pd.msg); err != nil {
			log.Printf("flush pending delivery %s: %v", slug, err)
			// The page would otherwise show it waiting forever.
			queuePendingEvent(hub, pd, "pending_deleted", apitypes.PendingRef{ID: pd.id})
			continue
		}
		queuePendingEvent(hub, pd, "pending_delivered", apitypes.PendingDelivered{ID: pd.id, TS: pd.msg.TS.UnixMilli()})
	}
	hub.markCompleted()
	s.streamMu.Unlock()
	s.Store.LinkChatAttachments(slug)
	// Before the caller closes the hub, and before the follow-up spawn
	// emits the chat_message for these rows on its own hub.
	hub.flushOrdered()
	s.NotifyOrgState()
	return len(pending)
}

// spawnFollowupIfNewerReceived catches any received entry that is
// newer than the turn that just ended, and spawns a follow-up if the
// spawn gate agrees the agent still owes an answer.
//
// Two ways to get here, and only one of them is a bug. A fold that
// LANDED wrote its message into chat.jsonl mid-turn on purpose, so a
// newer received entry is expected and SpawnDecision will read it as
// already answered. The other way is an entry that reached chat.jsonl
// without going through deliverToAgent at all, which should not
// happen — hence the check stays. "Real" excludes the
// tool-plumbing kinds (tool_use, tool_result, doc_published) that are
// part of an agent's own in-flight thinking, not a new user release.
//
// The timestamp gate is load-bearing: it prevents spin when the prior
// loop legitimately processed a received entry but produced no
// assistant text (e.g. mocked streams in tests, or real errors mid-
// flight). Only a strictly-newer received entry re-triggers.
func (s *Server) spawnFollowupIfNewerReceived(slug string, spawnTS time.Time, source string) {
	hist, err := s.Store.ReadChatHistory(slug)
	if err != nil || len(hist) == 0 {
		return
	}
	latestTS := lastRealReceivedTS(hist)
	if latestTS.IsZero() {
		return
	}
	if !latestTS.After(spawnTS) {
		return
	}
	if s.spawnChatLoopIfIdle(slug, source) {
		log.Printf("chat %s: followup spawned for a received entry newer than the turn that just ended", slug)
	}
}

// lastRealReceivedTS is a thin alias over the shared helper so
// every layer agrees on what counts as a "real" received entry.
// See store.LastRealReceivedTS for the canonical rule.
func lastRealReceivedTS(hist []store.ChatMessage) time.Time {
	return store.LastRealReceivedTS(hist)
}

// turnInFlight reports whether the runtime is currently inside a tool
// loop for slug — that is, whether a TURN is streaming. Used by
// handlers + spawnChatLoopIfIdle to avoid firing a parallel Claude
// inference on the same agent (two starts from the same chat history
// produce near-duplicate tool calls), and by deliverToAgent to decide
// whether a delivery must be staged rather than written straight in.
//
// Named for the turn on purpose. It was runtimeRunningAgent, and
// "running" invites the reading "has work outstanding" — which will be
// true of an agent merely waiting on background tasks. That agent is
// displayed as working but is NOT mid-turn, and conflating the two
// would both block its deliveries and suppress its interrupt. The
// display-side predicate lives in agents.go.
//
// Returns false when the runtime isn't configured (tests / API-only
// deployments).
func (s *Server) turnInFlight(slug string) bool {
	if s.Runtime == nil {
		return false
	}
	for _, running := range s.Runtime.RunningAgents() {
		if running == slug {
			return true
		}
	}
	return false
}

// ChatHubActive reports whether there's an active chat-hub entry
// for slug. Used by the runtime (wired via main.go as the
// Config.SkipAgent hook) to avoid spawning a parallel Claude
// stream when the web chat path is already running one. Exported
// so it's callable across packages.
//
// A "lingering" hub (loop completed, kept in chatHubs for late SSE
// subscribers) reports false: nothing is actually running, and a
// new spawn through getOrCreateHub will displace the lingering hub.
func (s *Server) ChatHubActive(slug string) bool {
	s.streamMu.Lock()
	hub, ok := s.chatHubs[slug]
	s.streamMu.Unlock()
	return ok && !hub.isCompleted()
}

// RecoverInterruptedTurns is called once at Kivali boot to detect
// agents whose chat-loop turn was killed mid-flight by infrastructure
// (OOM, segfault, pod restart, kubelet eviction). For each agent with
// an orphan active_turn.json marker, this writes a
// kind:runtime-disruption system entry to chat.jsonl with diagnostic
// content (when the turn started, what source triggered it, what
// model) and removes the marker. The handbook tells the agent
// that this means "the runtime failed mid-task; resume your prior
// work."
//
// Auto-spawn is deferred: this method returns the slugs that need to
// be re-woken so main.go can run them through SpawnRecovered AFTER
// bulk agent-pod provisioning. Calling spawnChatLoopIfIdle here
// directly would race those async startup tasks: a recovered
// agent's first chat-turn event would publish to a hub with no
// subscribed runtime and silently drop.
//
// Best-effort: a failure on any one agent is logged and the scan
// continues for the rest.
func (s *Server) RecoverInterruptedTurns() []string {
	orphans, err := s.Store.ListOrphanActiveTurns()
	if err != nil {
		log.Printf("recover-interrupted-turns: list: %v", err)
		return nil
	}
	recovered := make([]string, 0, len(orphans))
	for _, slug := range orphans {
		marker, err := s.Store.ReadActiveTurn(slug)
		if err != nil {
			log.Printf("recover-interrupted-turns %s: read marker: %v", slug, err)
			continue
		}
		// Diagnostic content the agent will see in chat history.
		// Plain prose, not JSON, since this is read by the model
		// via BuildRequest like any other ChatMessage.Content.
		content := fmt.Sprintf(
			"The runtime failed mid-task at an unknown point after %s (source: %s). Kivali has restarted; resume your prior work per the handbook.",
			marker.StartedAt.UTC().Format(time.RFC3339),
			marker.Source,
		)
		if err := s.Store.AppendChatMessage(slug, store.ChatMessage{
			Role:    store.RoleReceived,
			Kind:    store.KindRuntimeDisruption,
			Content: content,
		}); err != nil {
			log.Printf("recover-interrupted-turns %s: append disruption: %v", slug, err)
			continue
		}
		if err := s.Store.ClearActiveTurn(slug); err != nil {
			log.Printf("recover-interrupted-turns %s: clear marker: %v", slug, err)
			// continue — the marker will get overwritten on next
			// successful turn anyway.
		}
		recovered = append(recovered, slug)
	}
	return recovered
}

// SpawnRecovered fires spawnChatLoopIfIdle for each slug in the list
// (typically the slugs returned by RecoverInterruptedTurns), subject
// to the spawn gate's circuit breaker. Safe to call any time after
// RecoverInterruptedTurns; main.go calls this AFTER bulk agent-pod
// provisioning so the resumed agents' chat-turn events land on
// subscribed runtimes.
//
// Quarantined agents (chatHubQuarantineThreshold consecutive
// runtime-disruption entries with no productive activity in between)
// are held off; an operator observing the chat sees the latest
// disruption entries and knows to investigate.
//
// Each spawn is scheduled on its own goroutine because the agent pod
// it targets may not have subscribed to the events hub yet — main.go
// fires Provision (a k8s create call) and then SpawnRecovered serially,
// but pod startup and SSE connect take seconds. Without waiting, every
// spawn races ahead of subscription and silently drops as
// publishAgentpodChatTurn returns false (no subscribers). The wait is
// bounded by spawnRecoveredSubscribeTimeout so a permanently-missing
// pod doesn't leak a goroutine forever.
func (s *Server) SpawnRecovered(slugs []string) {
	for _, slug := range slugs {
		go s.spawnRecoveredOnce(slug)
	}
}

// spawnRecoveredSubscribeTimeout is how long SpawnRecovered will wait
// for an agent pod to subscribe before giving up on a recovered turn.
// Long enough to cover image pull + k8s scheduling + SSE connect on a
// cold cluster; short enough that a misconfigured cluster's recovered
// turns don't accumulate goroutines forever. Var so tests can shrink it.
var spawnRecoveredSubscribeTimeout = 2 * time.Minute

// spawnRecoveredSubscribePollInterval governs the polling cadence for
// SubscriberCount. 100ms is fast enough that the recovered turn fires
// promptly once the pod connects, slow enough not to busy-loop.
var spawnRecoveredSubscribePollInterval = 100 * time.Millisecond

func (s *Server) spawnRecoveredOnce(slug string) {
	s.spawnWhenSubscribed(slug, "release")
}

// spawnWhenSubscribed starts slug's chat loop (for reason) once its
// agent pod has subscribed, waiting up to spawnRecoveredSubscribeTimeout:
// a recovered turn after a restart, or the Chief of Staff's first turn
// right after setup created its pod.
func (s *Server) spawnWhenSubscribed(slug, reason string) {
	if s.AgentpodHub == nil {
		// No hub wired (test harness, or main.go misconfig). Fire
		// once anyway — spawnChatLoopIfIdle's other gates still apply.
		if !s.spawnChatLoopIfIdle(slug, reason) {
			log.Printf("recover-interrupted-turns %s: spawn gate held off (likely quarantined: %d consecutive disruptions)", slug, store.ConsecutiveRuntimeDisruptions(s.readChatHistorySafe(slug)))
		}
		return
	}
	deadline := time.Now().Add(spawnRecoveredSubscribeTimeout)
	for s.AgentpodHub.SubscriberCount(slug) == 0 {
		if time.Now().After(deadline) {
			log.Printf("recover-interrupted-turns %s: agent pod never subscribed within %s; giving up (next inbound chat-turn will pick up the disruption row)", slug, spawnRecoveredSubscribeTimeout)
			return
		}
		time.Sleep(spawnRecoveredSubscribePollInterval)
	}
	if !s.spawnChatLoopIfIdle(slug, reason) {
		log.Printf("recover-interrupted-turns %s: spawn gate held off (likely quarantined: %d consecutive disruptions)", slug, store.ConsecutiveRuntimeDisruptions(s.readChatHistorySafe(slug)))
	}
}

// readChatHistorySafe reads chat history but never panics or returns
// an error — used in log lines where we don't care about partial
// failure. Empty slice on any error.
func (s *Server) readChatHistorySafe(slug string) []store.ChatMessage {
	hist, err := s.Store.ReadChatHistory(slug)
	if err != nil {
		return nil
	}
	return hist
}

// handleAgentStop cancels the in-flight chat-turn for slug, if any.
// Called by the Stop button in the UI while a response is streaming.
//
// Three actions, in order, every click:
//
//  1. (First click only) Append a kind:user-interruption row to
//     chat.jsonl synchronously. Doing this BEFORE any side-effect
//     dispatch keeps the audit trail correct even when downstream
//     cancellation fails — the marker is durable the moment the user
//     presses Stop, not whenever the runtime gets around to posting
//     a terminator. SpawnDecision and chatHistoryToMessages tolerate
//     the marker landing between a tool_use and its tool_result
//     (later events from the dying turn append after it) by walking
//     past the marker rather than requiring it at the tail.
//  2. publishCancelTurn fires EventCancelTurn at the agent runtime
//     for every currently-in-flight turn owned by slug — parent's
//     own chat turn plus any subagents. Re-resolved at click time
//     (not cached from the prior click), so a second Stop press
//     after the parent's CLI has already retried into a fresh
//     subagent cancels the new subagent, not the dead one.
//  3. Returns 202 unless the hub is already completed (204).
//
// 204 when there's nothing to stop (no hub, or hub completed before
// the click reached us).
func (s *Server) handleAgentStop(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" || slug == agent.CEOSlug {
		http.Error(w, "invalid target", http.StatusBadRequest)
		return
	}
	s.streamMu.Lock()
	hub := s.chatHubs[slug]
	s.streamMu.Unlock()

	// acted tracks whether Stop found anything to stop, and is what
	// separates 202 from 204. It is NOT a gate on the cancellation
	// below — see the comment there.
	acted := false

	// The MARKER is gated on a live hub, and only on a live hub.
	// Writing a user-interruption row against a finished hub silently
	// quarantines the agent until the CEO engages it manually, because
	// no in-flight loop produced the interruption the row claims.
	if hub != nil {
		first, alreadyCompleted := hub.requestInterrupt()
		if !alreadyCompleted {
			acted = true
			if first {
				msg := store.ChatMessage{
					Role:    store.RoleReceived,
					Kind:    store.KindUserInterruption,
					Content: "User pressed Stop.",
					TS:      time.Now().UTC(),
				}
				if err := s.Store.AppendChatMessage(slug, msg); err != nil {
					log.Printf("stop %s: append interruption entry: %v", slug, err)
				} else {
					emitChatMarker(hub, msg)
				}
			}
		}
	}

	// CANCELLATION is deliberately NOT gated on the hub, and this is
	// the whole point of the handler.
	//
	// Background subagents outlive the turn that dispatched them: the
	// agent dispatches, says what it is doing, and ends its turn, so
	// its hub is completed or already evicted while the batch keeps
	// running. That is exactly when someone reaches for Stop, so a
	// nil-or-completed hub is no reason to return early.
	//
	// Two halves, because a job can be in either state:
	//
	//   - CancelAllSubagents cancels each job's context. That covers
	//     QUEUED jobs, which have no turn at the pod yet and would
	//     otherwise start after the CEO said stop, and it unwinds
	//     DriveSubagent for running ones (which publishes its own
	//     cancel to the pod and waits out the grace).
	//   - publishCancelTurn reaches the pod directly for the parent
	//     turn and every subagent turn registered under this slug.
	//
	// Both are no-ops when there is nothing in that state.
	if n := s.SubagentService.CancelAllSubagents(slug); n > 0 {
		acted = true
		log.Printf("stop %s: cancelled %d outstanding background task(s)", slug, n)
		// Each cancelled job fires NotifyWorkingChanged as it unwinds,
		// which is what clears the snapshot's waiting state and
		// waiting_tasks count. Cancelled jobs deliver no result, so nothing
		// else would have pushed a snapshot.
	}
	// Always re-arm cancellation. collectInflightTurnIDs picks up
	// whatever is in flight RIGHT NOW; a no-op cancel (no in-flight
	// turn) is fine. Repeat clicks reach whatever the parent has
	// since spawned.
	s.publishCancelTurn(slug)

	if !acted {
		// Genuinely idle: no live turn, no background work. Same 204
		// the caller has always got for a no-op Stop.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
