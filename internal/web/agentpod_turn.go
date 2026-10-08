package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// publishAgentpodChatTurn is spawnChatLoopIfIdle's handoff to the
// agent runtime: marshal req + install per-slug turn state, then
// publish EventChatTurn so the agent pod drives the model. Returns
// true when the event was published (or when a marshal failure was
// cleaned up — there is no fallback path), false only when no
// runtime is subscribed for slug.
//
// Failure modes:
//   - AgentpodHub unwired (test fixtures that don't install a fake) →
//     false. The caller (spawnChatLoopIfIdle) treats this as a hard
//     failure and tears the spawn down.
//   - SubscriberCount(slug) == 0 → false. Same outcome. The race
//     between Subscribe and the publish is small; the next CEO
//     interaction triggers a fresh spawn.
//   - JSON marshal of req fails → log, tear down state + hub, return
//     true (a retry would re-attempt the same marshal and fail again).
//     The hub gets closed so the user-visible state stays consistent;
//     the post-loop teardown for this branch is intentionally minimal
//     — no buffered deliveries to flush, no rotation to finalize.
func (s *Server) publishAgentpodChatTurn(slug, source string, hub *chatHub, req *provider.CompleteRequest) bool {
	if s.AgentpodHub == nil {
		return false
	}
	if s.AgentpodHub.SubscriberCount(slug) == 0 {
		return false
	}
	turnID := newAgentpodTurnID()
	raw, err := json.Marshal(req)
	if err != nil {
		log.Printf("chat %s: agentpod marshal request: %v", slug, err)
		// Best-effort cleanup: clear the active-turn marker we just
		// wrote and close the hub so the UI doesn't hang on a
		// "thinking" indicator that will never resolve.
		if cerr := s.Store.ClearActiveTurn(slug); cerr != nil {
			log.Printf("chat %s: clear active turn after marshal failure: %v", slug, cerr)
		}
		hub.markCompleted()
		hub.close()
		s.removeHub(slug)
		return true
	}
	s.InitAgentpodTurnState(slug, source, turnID, hub, req.Model, s.effectiveEffort(req.Model, req.Effort))
	payload := agentpod.ChatTurnEvent{
		TurnID:  turnID,
		Slug:    slug,
		Source:  source,
		Model:   req.Model,
		Request: raw,
	}
	if err := s.PublishAgentpodEvent(slug, agentpod.EventChatTurn, payload); err != nil {
		log.Printf("chat %s: agentpod publish chat-turn: %v", slug, err)
		// Mirror the marshal-failure cleanup so a stuck "thinking"
		// state can't develop.
		s.dropAgentpodTurn(slug)
		if cerr := s.Store.ClearActiveTurn(slug); cerr != nil {
			log.Printf("chat %s: clear active turn after publish failure: %v", slug, cerr)
		}
		hub.markCompleted()
		hub.close()
		s.removeHub(slug)
	}
	return true
}

// newAgentpodTurnID returns a 16-hex-char turn correlator. Source of
// uniqueness is crypto/rand — overkill for the cardinality we
// generate but avoids any per-process counter coordination, and
// makes the turnID self-contained for log greps.
func newAgentpodTurnID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failures on Linux are essentially impossible;
		// fall back to a timestamp so the turn at least has a value.
		return fmt.Sprintf("t%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// agentpodTurnState holds the per-slug in-flight state for one
// agent-pod chat turn (assembled assistant text, pendingTools map,
// assistantStartTS) so the handler-per-event flow can pick up where
// the previous event left off — events arrive on independent
// goroutines, so the per-turn state lives on Server, not in a closure.
//
// Lifecycle: InitAgentpodTurnState installs an entry when the chat
// spawn decides to publish a chat-turn event to the agent pod. The
// entry stays around for the duration of the turn — every TurnEvent
// the agent pod POSTs back finds it via lookupTurn. done/failed
// handlers tear it down via dropAgentpodTurnIfMatches.
//
// Concurrency: handleAgentpodTurnEvent runs each TurnEvent on its own
// goroutine (the http.Server handler model). All mutation of an entry
// is gated by mu; the per-slug map slot is gated by Server.streamMu
// (same lock as chatHubs).
//
// Kind discriminates the turn type: chat (parent's own chat-turn,
// keyed by slug) vs subagent (a `claude -p` driven by SubagentService,
// keyed by turn-id). Subagent state has subagentID populated and
// done set so SubagentService.runJob can block on completion;
// hub is left nil since subagent transcripts don't drive the parent's
// chat hub directly (parent's chat hub renders the subagent via the
// EmitToParent fan-out from SubagentService).
type agentpodTurnState struct {
	// Slug, hub, source, and turnID are immutable for the life of
	// the turn — captured at Init time. hub is the chatHub the
	// browser-facing SSE feeds off of; agentpod events drive
	// hub.Emit calls so the UI stays in sync.
	slug   string
	hub    *chatHub
	source string
	turnID string

	// kind is "" / "chat" for the parent's own chat turn,
	// "subagent" for a subagent run. Routes appendChat between
	// AppendChatMessage and AppendSubagentMessage and gates the
	// subagent-only finalize path.
	kind agentpod.ChatTurnKind

	// subagentID is populated when kind=subagent. Names the subagent
	// transcript the TurnEvent handlers append to.
	subagentID string

	// done is populated when kind=subagent. Buffered (size 1) so
	// finalizeSubagentTurn can send without blocking even if
	// SubagentService.runJob's select races on its ctx-cancel arm.
	done chan subagentTurnResult

	// assembled buffers the assistant text streaming in via delta
	// events. Flushed to chat.jsonl as a kind:"direct_chat" row on
	// tool_use_start and on done — keeping each chat row a single
	// kind, with tool rows interleaved between text rows.
	//
	// assistantStartTS stamps the bubble's UI timestamp on the first
	// delta of a fresh assembled run. Reset to 0 by flushAssistant.
	assembled        strings.Builder
	assistantStartTS int64

	// preemptRequested records that a delivery has already asked this
	// turn to wind up early (see preemptForDelivery). Releasing five
	// messages at once must produce ONE interrupt, not five: the
	// first ends the turn, and the rest ride the same buffer flush.
	// Guarded by streamMu, like the map that holds this state.
	preemptRequested bool

	// pausedForDelivery records that the CEO pressed Send now on a
	// pending message: the turn was asked to end early so the waiting
	// messages reach the agent now. finalizeAgentpodTurn reads it to
	// keep the partial reply and write the paused-to-deliver marker in
	// front of the flushed messages. Set alongside preemptRequested,
	// which it never replaces (that latch still stops folds and
	// duplicate cancels). Guarded by streamMu.
	pausedForDelivery bool

	// pendingTools maps tool_use_id → (name, input) for tools whose
	// tool_use_end has been received but whose tool_result is still
	// pending. Used on tool_result to synthesize the
	// doc_published / file_shared follow-up emit when the underlying
	// tool was publish_* / share_file.
	pendingTools map[string]agentpodPendingTool

	// model is the Claude model id this turn ASKED for, captured at
	// publish time from the CompleteRequest. It is the fallback label
	// and pricing basis for the turn, used when the pod reported no
	// model of its own — which is all a failed turn that died before
	// its first assistant message can offer.
	//
	// Not the same thing as what answered: see respModel.
	model string

	// respModel is the model that produced the text currently buffered
	// in `assembled` — read off the CLI's assistant message and carried
	// on each delta, so it is what ACTUALLY answered rather than what
	// the request asked for. Empty until the first delta of a turn
	// arrives, which is why modelForBubble falls back to st.model.
	//
	// The two can differ: a request can be served by a different model
	// than it named, and a turn's calls need not all be served by the
	// same one. Labelling a bubble from the request would state
	// something we never verified.
	respModel string

	// errorDetail collects the text of the turn's TurnEventError
	// events. The driver reports a turn it could not complete — a
	// usage limit, an expired login, a 429, an output-token maximum —
	// as one or more of those, then a done with StopReason
	// provider.StopError. That text is the cause of the failure, not the
	// agent speaking, so it never enters `assembled` and never becomes
	// a direct_chat row; it becomes the detail of the kind:turn-error
	// marker the done handler writes.
	errorDetail strings.Builder

	// effort is the resolved (normalized) reasoning level this turn ran
	// at, captured at publish time from the CompleteRequest — the agent
	// pod never echoes it back over the wire, so we remember the value we
	// passed via --effort. Stamped onto each direct_chat row in
	// flushAssistant so the bubble can show an effort pill next to the
	// model pill.
	effort string

	// thinkingSteps counts the model round-trips that included extended
	// reasoning this turn (one per agent-pod TurnEventThinking). Surfaced
	// in the thinking bubble as a progress signal so the user can tell a
	// long turn is advancing (step count climbing) vs stalled. Touched
	// only from the single-threaded agent-pod event loop, so no lock.
	thinkingSteps int
}

// subagentTurnResult is the value sent on agentpodTurnState.done
// when a subagent run finishes. SubagentService.runJob consumes it
// to render the parent's tool_result body.
type subagentTurnResult struct {
	FinalText string
	Err       error // nil on clean completion; populated on cancel/failed
}

// agentpodPendingTool is the subset of tool_use_end fields we keep so
// we can reconstruct doc_published / file_shared synthetic emits when
// the matching tool_result arrives later.
type agentpodPendingTool struct {
	toolName  string
	toolInput json.RawMessage
}

// InitAgentpodTurnState records that an agent-pod chat turn is
// in-flight for slug. Returns the freshly-installed state so the
// caller (publishAgentpodChatTurn) can stamp the turnID on its
// outgoing event. Replaces any prior entry for slug — a fresh
// chat-turn event implies the previous turn already completed (or
// its done/failed never landed, in which case we'd rather start
// clean than block).
//
// hub may be nil in tests that exercise the per-event handlers
// directly without a chat hub; the Emit calls null-check.
func (s *Server) InitAgentpodTurnState(slug, source, turnID string, hub *chatHub, model, effort string) *agentpodTurnState {
	st := &agentpodTurnState{
		slug:         slug,
		hub:          hub,
		source:       source,
		turnID:       turnID,
		model:        model,
		effort:       effort,
		pendingTools: map[string]agentpodPendingTool{},
	}
	s.streamMu.Lock()
	if s.agentpodTurns == nil {
		s.agentpodTurns = map[string]*agentpodTurnState{}
	}
	s.agentpodTurns[slug] = st
	s.streamMu.Unlock()
	return st
}

// lookupAgentpodTurn returns the in-flight state for slug. Returns nil
// when no turn is registered — the per-event handler 204s the request
// in that case (a stale event from a prior turn that the runtime is
// still flushing should not error the agent pod).
func (s *Server) lookupAgentpodTurn(slug string) *agentpodTurnState {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	return s.agentpodTurns[slug]
}

// lookupAgentpodTurnByID returns the in-flight state for a turn-id:
// a subagent turn from the per-turn-id map, or the slug's chat turn
// (keyed by slug) when its turn-id matches.
func (s *Server) lookupAgentpodTurnByID(slug, turnID string) *agentpodTurnState {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if st, ok := s.agentpodSubagentTurns[turnID]; ok {
		return st
	}
	if st, ok := s.agentpodTurns[slug]; ok && st.turnID == turnID {
		return st
	}
	return nil
}

// installAgentpodSubagentTurn registers a subagent turn keyed by
// turn-id. Caller (SubagentService.runJob) keeps a reference to
// the returned state so it can block on `done`. parent is the
// parent agent's slug — used by appendChat and the cancel cascade
// to address the parent's UDS hub. model is the subagent's resolved
// model id (per-call spec.Model override, or the parent's default
// when empty) — recorded on the usage.jsonl row written when the
// run finishes.
func (s *Server) installAgentpodSubagentTurn(parent, turnID, subagentID, model string) *agentpodTurnState {
	st := &agentpodTurnState{
		slug:         parent,
		turnID:       turnID,
		kind:         agentpod.ChatTurnKindSubagent,
		source:       "subagent",
		model:        model,
		subagentID:   subagentID,
		done:         make(chan subagentTurnResult, 1),
		pendingTools: map[string]agentpodPendingTool{},
	}
	s.streamMu.Lock()
	if s.agentpodSubagentTurns == nil {
		s.agentpodSubagentTurns = map[string]*agentpodTurnState{}
	}
	s.agentpodSubagentTurns[turnID] = st
	s.streamMu.Unlock()
	return st
}

// dropAgentpodSubagentTurn evicts a subagent turn entry. Safe to
// call when no entry exists.
func (s *Server) dropAgentpodSubagentTurn(turnID string) {
	s.streamMu.Lock()
	delete(s.agentpodSubagentTurns, turnID)
	s.streamMu.Unlock()
}

// collectInflightSubagentTurns returns slug's in-flight subagent turn
// states, paired with the turn-id they are registered under.
//
// Separate from collectInflightTurnIDs (which returns bare ids for the
// cancel cascade) because the disconnect sweep needs the STATE: it
// synthesizes a terminal event against it, and it has to confirm the
// same state is still registered after the grace window rather than
// trusting an id that may have been reused.
func (s *Server) collectInflightSubagentTurns(slug string) map[string]*agentpodTurnState {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	out := map[string]*agentpodTurnState{}
	for id, st := range s.agentpodSubagentTurns {
		if st != nil && st.slug == slug {
			out[id] = st
		}
	}
	return out
}

// appendChat routes the chat-row write to the right transcript:
// AppendChatMessage for the parent's chat, AppendSubagentMessage for
// a subagent run. Centralized so every per-event handler
// (delta/tool_use/tool_result/done) takes the same fork in one place.
func (st *agentpodTurnState) appendChat(s *Server, msg store.ChatMessage) error {
	if st.kind == agentpod.ChatTurnKindSubagent {
		return s.Store.AppendSubagentMessage(st.slug, st.subagentID, msg)
	}
	return s.Store.AppendChatMessage(st.slug, msg)
}

// emit forwards an event to the chatHub if one is wired. Centralized
// nil-check — handlers below stay tight.
func (st *agentpodTurnState) emit(kind string, payload any) {
	if st.hub == nil {
		return
	}
	st.hub.Emit(kind, payload)
}

// emitTerminal sends the turn's `done` or `error`. Unless a rotation
// follows, the page is finished with the hub from here on.
func (st *agentpodTurnState) emitTerminal(kind string, payload map[string]any) {
	if st.hub == nil {
		return
	}
	st.hub.Emit(kind, payload)
	if payload["rotation"] != true {
		st.hub.markTurnEnded()
	}
}

// modelForBubble returns the model id to stamp on a direct_chat row:
// the CLI-reported model that actually produced the text when a delta
// carried one, else the request's resolved model.
func (st *agentpodTurnState) modelForBubble() string {
	if st.respModel != "" {
		return st.respModel
	}
	return st.model
}

// flushAssistant writes the buffered assistant text to chat.jsonl as
// a kind:"direct_chat" row, trims deltas from the hub's replay buffer,
// and resets the buffer + timestamp.
func (st *agentpodTurnState) flushAssistant(s *Server) {
	if st.assembled.Len() == 0 {
		return
	}
	text := st.assembled.String()
	if err := st.appendChat(s, store.ChatMessage{
		Role: store.RoleSent, Content: text, Kind: "direct_chat", TS: time.Now().UTC(),
		// Stamp the model that produced this text so the bubble can be
		// labeled with its friendly name. Prefer the id the CLI reported
		// for the calls that wrote this text over the one we asked for —
		// see agentpodTurnState.respModel. Falls back to st.model (the
		// turn's resolved request model, always a concrete ID because
		// modelFor() never yields "") when no delta carried one, so
		// every direct_chat row still gets a pill.
		Model: st.modelForBubble(),
		// Stamp the resolved effort the same way so the bubble can show an
		// effort pill alongside the model. st.effort is normalized at
		// publish time (NormalizeEffort never yields ""), so every
		// direct_chat row carries a concrete level.
		Effort: st.effort,
	}); err != nil {
		log.Printf("agentpod turn %s: append assistant message: %v", st.slug, err)
	}
	if st.hub != nil {
		st.hub.TrimDeltas()
	}
	st.assembled.Reset()
	st.assistantStartTS = 0
}

// onAgentpodError handles a TurnEventError: the driver's account of
// why it could not complete the turn, not the agent's reply. Filed as
// a reply, it read — on the page and to the spawn gate alike — as the
// agent having answered. It is held as the error detail instead; the
// done event that follows carries StopReason provider.StopError and
// turns it into the kind:turn-error marker. See
// agentpodTurnState.errorDetail.
func (s *Server) onAgentpodError(st *agentpodTurnState, ev agentpod.TurnEvent) {
	if st.errorDetail.Len() > 0 {
		st.errorDetail.WriteString("\n")
	}
	st.errorDetail.WriteString(ev.Text)
}

// onAgentpodDelta handles a TurnEventDelta: stamp the bubble timestamp
// on first delta, append text to the assembled buffer, emit the
// browser-facing delta event.
func (s *Server) onAgentpodDelta(st *agentpodTurnState, ev agentpod.TurnEvent) {
	// A turn is a loop of N calls and the answering model is per-call.
	// Text already buffered came from the previous one, so close it as
	// its own bubble before mixing in text from a different model —
	// otherwise a single bubble carries two models' words under one
	// pill, and the pill is wrong for at least half of them. Flushing
	// here is also what keeps each persisted row single-model, which is
	// what makes the stamped Model meaningful at all.
	if ev.Model != "" && st.respModel != "" && ev.Model != st.respModel {
		st.flushAssistant(s)
	}
	if ev.Model != "" {
		st.respModel = ev.Model
	}
	if st.assistantStartTS == 0 {
		st.assistantStartTS = time.Now().UnixMilli()
		// First chunk of a fresh text block = a message back from the
		// model, which ends the current reasoning interval. Zero the
		// per-interval thinking-step counter so the next emitted step
		// number restarts at 1 (the UI shows reasoning progress BETWEEN
		// messages, not a turn total). assistantStartTS is reset to 0 by
		// flushAssistant, so each new text block re-triggers this.
		st.thinkingSteps = 0
	}
	st.assembled.WriteString(ev.Text)
	// Carry the model's friendly name on the delta so the live streaming
	// bubble can show the same model the chat API's row carries. Sending
	// the formatted string — not the raw ID — keeps the kebab→friendly
	// mapping in one place (Go) instead of duplicating it in the client.
	st.emit("delta", map[string]any{
		"text":     ev.Text,
		"start_ts": st.assistantStartTS,
		"model":    s.modelLabel(st.modelForBubble()),
		// Carry the effort the same way so the live bubble renders the same
		// effort the chat API's row carries.
		"effort": st.effort,
	})
}

// onAgentpodThinking records one extended-reasoning step and forwards a
// progress update to the live "thinking…" bubble. The step counter is
// PER-INTERVAL, not per-turn: it's reset to 0 each time a message goes
// back to the model (onAgentpodDelta / onAgentpodToolUseStart), because
// reasoning happens between messages and the UI shows the effort for the
// current inter-message gap. The client pairs the step number with a
// per-interval elapsed clock so the user can see the gap advancing
// rather than stalled.
//
// ev.Text carries the reasoning text only on models that expose it
// (Sonnet/Haiku); Opus encrypts it, so Text is usually empty and we
// surface step + elapsed alone. When text IS present we also send a
// one-line summary. Deliberately does NOT touch the assembled assistant
// buffer — thinking is ephemeral, never persisted or replayed.
func (s *Server) onAgentpodThinking(st *agentpodTurnState, ev agentpod.TurnEvent) {
	st.thinkingSteps++
	payload := map[string]any{"step": st.thinkingSteps}
	if summary := thinkingSummary(ev.Text); summary != "" {
		payload["summary"] = summary
	}
	st.emit("thinking", payload)
}

// thinkingMaxSummaryLen bounds the thinking-bubble label. Long enough
// to convey the gist of a reasoning step, short enough to stay on one
// line in the bubble.
const thinkingMaxSummaryLen = 140

// thinkingSummary condenses a raw extended-reasoning chunk into a
// single-line gist for the thinking bubble. The model's thinking
// usually opens with a topic sentence, so the first sentence/line is a
// good proxy for "what it's working on"; we collapse whitespace, take
// that lead, and truncate on a word boundary with an ellipsis. Markdown
// emphasis markers are stripped so the label reads as plain prose.
func thinkingSummary(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Take the first line — thinking blocks are often multi-paragraph
	// and the opening line is the most summary-like.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// Prefer the first sentence if it ends cleanly within the line.
	if i := strings.IndexAny(s, ".!?"); i >= 0 && i+1 < len(s) {
		s = s[:i+1]
	}
	// Collapse internal whitespace runs and drop markdown emphasis.
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer("**", "", "*", "", "`", "", "#", "").Replace(s)
	s = strings.TrimSpace(s)
	if len(s) <= thinkingMaxSummaryLen {
		return s
	}
	// Truncate on a word boundary at or before the cap.
	cut := s[:thinkingMaxSummaryLen]
	if sp := strings.LastIndexByte(cut, ' '); sp > thinkingMaxSummaryLen/2 {
		cut = cut[:sp]
	}
	return strings.TrimSpace(cut) + "…"
}

// onAgentpodToolUseStart flushes any buffered assistant text (so the
// resulting chat.jsonl shows ..., direct_chat, tool_use, tool_result,
// ...) then forwards the tool_use_start event for the live UI.
func (s *Server) onAgentpodToolUseStart(st *agentpodTurnState, ev agentpod.TurnEvent) {
	st.flushAssistant(s)
	// A tool call is a message back from the model — end the current
	// reasoning interval so the next step number restarts at 1 (matches
	// the client's per-interval reset; see onAgentpodThinking).
	st.thinkingSteps = 0
	st.emit("tool_use_start", map[string]any{
		"tool_use_id": ev.ToolUseID,
		"tool_name":   ev.ToolName,
		"start_ts":    time.Now().UnixMilli(),
	})
}

// onAgentpodToolInputDelta forwards the partial JSON chunk for the
// live UI's input-streaming animation. No persistence; the full input
// arrives on tool_use_end.
func (s *Server) onAgentpodToolInputDelta(st *agentpodTurnState, ev agentpod.TurnEvent) {
	st.emit("tool_input_delta", map[string]any{
		"tool_use_id": ev.ToolUseID,
		"text":        ev.Text,
	})
}

// onAgentpodToolUseEnd persists the tool_use chat row, records the
// pending tool so a later tool_result can synthesize the
// doc_published / file_shared follow-up emit, and forwards the
// tool_use event to the live UI.
func (s *Server) onAgentpodToolUseEnd(st *agentpodTurnState, ev agentpod.TurnEvent) {
	st.pendingTools[ev.ToolUseID] = agentpodPendingTool{
		toolName:  ev.ToolName,
		toolInput: ev.ToolInput,
	}
	if err := st.appendChat(s, store.ChatMessage{
		Role: store.RoleSent, Kind: "tool_use",
		Content:   fmt.Sprintf("%s %s", ev.ToolName, string(ev.ToolInput)),
		ToolUseID: ev.ToolUseID, ToolName: ev.ToolName, ToolInput: string(ev.ToolInput),
		TS: time.Now().UTC(),
	}); err != nil {
		log.Printf("agentpod turn %s: append tool_use: %v", st.slug, err)
	}
	st.emit("tool_use", map[string]any{
		"tool_use_id": ev.ToolUseID,
		"tool_name":   ev.ToolName,
		"tool_input":  string(ev.ToolInput),
	})
}

// onAgentpodToolResult persists the tool_result chat row, forwards the
// tool_result event to the live UI, and — when the matching pending
// tool was a publish_* or share_file — emits the synthetic
// doc_published / file_shared event so the UI paints the friendly
// chip alongside the raw tool_result.
func (s *Server) onAgentpodToolResult(st *agentpodTurnState, ev agentpod.TurnEvent) {
	if err := st.appendChat(s, store.ChatMessage{
		Role: store.RoleReceived, Kind: "tool_result",
		Content:   ev.ToolResultText,
		ToolUseID: ev.ToolUseID, IsError: ev.ToolResultIsError,
		TS: time.Now().UTC(),
	}); err != nil {
		log.Printf("agentpod turn %s: append tool_result: %v", st.slug, err)
	}
	st.emit("tool_result", map[string]any{
		"tool_use_id": ev.ToolUseID,
		"is_error":    ev.ToolResultIsError,
		"stdout":      ev.ToolResultText,
	})
	pt, ok := st.pendingTools[ev.ToolUseID]
	if !ok {
		return
	}
	delete(st.pendingTools, ev.ToolUseID)
	if ev.ToolResultIsError {
		return
	}
	tu := provider.StreamEvent{
		Kind:      provider.StreamToolUseEnd,
		ToolUseID: ev.ToolUseID,
		ToolName:  pt.toolName,
		ToolInput: pt.toolInput,
	}
	if strings.HasPrefix(pt.toolName, "publish_") {
		if s.Runtime != nil && st.hub != nil {
			s.Runtime.EmitDocPublishedFromMCPAck(st.slug, st.hub, tu, ev.ToolResultText, s.clk().Now())
		}
		return
	}
	if pt.toolName == agent.ShareFileToolName {
		if st.hub != nil {
			agent.EmitFileSharedFromMCPAck(st.hub, tu, ev.ToolResultText, s.clk().Now())
		}
	}
}

// onAgentpodDone handles a TurnEventDone: flushes any trailing
// assistant text, emits the "done" UI event with usage + stop info,
// then hands off to finalizeAgentpodTurn for the post-loop teardown
// (rotation finalize, interrupt marker, flushAndComplete,
// ClearActiveTurn, follow-up spawn).
//
// Subagent runs short-circuit the chat-turn finalize and signal the
// blocking SubagentService.runJob caller via st.done — there's no
// rotation, no follow-up spawn, no chat hub interaction.
func (s *Server) onAgentpodDone(st *agentpodTurnState, ev agentpod.TurnEvent) {
	st.flushAssistant(s)
	// Record usage on EVERY done event regardless of kind. This is the
	// single chokepoint where every successful model call — parent
	// chat turns, subagent runs — lands a usage.jsonl row. The agent
	// pod's claudeagent.Client carries no Recorder; the TurnEvent wire
	// is the recording boundary instead. New kinds added later inherit
	// usage recording for free as long as they emit TurnEventDone with
	// token counts populated.
	s.recordAgentpodUsage(st, ev)
	if st.kind == agentpod.ChatTurnKindSubagent {
		s.finalizeSubagentTurn(st, subagentTurnResult{FinalText: ev.Text})
		return
	}
	// StopReason provider.StopError is the driver reporting that a failed
	// model call (usage limit, expired login, 429, output-token
	// maximum) ended the turn rather than the model. The stream reached
	// its end, so the pod reports done — but nothing about the turn is
	// done, and treating it as such is what made a limit hit look like
	// a reply. Usage above is still recorded (the calls before the
	// failure were billed); the window occupancy is kept when the pod
	// measured one. Everything else takes the failure path, with the
	// text the TurnEventError events carried as the marker's detail.
	if ev.StopReason == provider.StopError {
		if ev.ContextTokens > 0 {
			if err := s.Store.WriteContextWindow(st.slug, ev.ContextTokens); err != nil {
				log.Printf("agentpod turn %s: write context-window stat: %v", st.slug, err)
			}
		}
		s.finishFailedChatTurn(st, agentpod.FailedReasonTurnError, st.errorDetail.String())
		return
	}
	if st.errorDetail.Len() > 0 {
		// An error event on a turn that then completed normally is not
		// a shape any driver produces; keep the text in the log so a
		// new shape is noticed rather than silently dropped.
		log.Printf("agentpod turn %s: error text on a completed turn, dropped: %q", st.slug, st.errorDetail.String())
	}
	usage := provider.TokenUsage{
		InputTokens:       ev.InputTokens,
		OutputTokens:      ev.OutputTokens,
		CacheReadTokens:   ev.CacheReadTokens,
		CacheCreateTokens: ev.CacheCreateTokens,
	}
	rotation := s.hasRotationPending(st.slug)
	payload := map[string]any{
		"usage":      usage,
		"stop":       ev.StopReason,
		"iterations": 1,
		"rotation":   rotation,
		// waiting_tasks is what stops "done" from reading as "idle".
		// An agent that dispatched background subagents ends its turn
		// on purpose — it said what it was doing and stopped — but the
		// work it owns is still running. done closes the EventSource,
		// so this count is the client's last word from the turn: it
		// keeps the thinking bubble up in waiting mode instead of
		// tearing it down, and /org/stream drives it from there.
		//
		// Read at emit time rather than when the batch was dispatched,
		// so a job that finished mid-turn is already discounted.
		"waiting_tasks": s.SubagentService.OutstandingSubagents(st.slug),
	}
	if s.Claude != nil {
		payload["usage_display"] = s.Claude.FormatUsage(usage)
	}
	// Persist this turn's REAL single-call window occupancy before the
	// snap so the ring reflects actual context fill (system prompt +
	// role + tool/skill/MCP schemas + memory + files), not just the
	// chat transcript — and so a later page reload shows the same value.
	// Subagent turns never reach here (they short-circuit above), so
	// this only ever records the parent chat's occupancy.
	if err := s.Store.WriteContextWindow(st.slug, ev.ContextTokens); err != nil {
		log.Printf("agentpod turn %s: write context-window stat: %v", st.slug, err)
	}
	// Snap the context-window ring / long-chat banner / token-stat
	// before the terminal event lands — the JS done handler closes
	// the EventSource synchronously, so any later emit goes nowhere.
	s.emitChatFillSnap(st)
	st.emitTerminal("done", payload)
	s.finalizeAgentpodTurn(st)
}

// recordAgentpodUsage persists the usage.jsonl row(s) for a completed
// agent-pod turn. Single recording site for every model call that
// terminates on the agent-pod boundary: parent chat turns AND
// subagent runs both flow here.
//
// Writes ONE ROW PER ANSWERING MODEL. A turn is a loop of N
// /v1/messages calls whose token counts accumulate, and the model that
// serves a call is per-call — so a turn can be genuinely mixed. Pricing
// the accumulated total at a single id bills every token at that id's
// rate, which is wrong by the ratio between the models involved
// (Fable and Opus differ by 2x). When the pod sends a split, each
// bucket becomes its own row and prices at its own rate; the rows sum
// to the same tokens the flat counts carry.
//
// Field resolution:
//   - Model: prefer the CLI-reported value off TurnEvent (authoritative
//     when SubagentSpec.Model overrode the parent's model); fall back
//     to st.model (set at publish time from req.Model).
//   - CostUSD: prefer the CLI's total_cost_usd off TurnEvent (more
//     accurate for batched / discounted billing); fall back to
//     local pricing-table compute so we never emit cost=0 on a
//     non-zero-token row. The CLI reports one total for the whole turn
//     and cannot attribute it per model, so on a split it lands on the
//     first row only — repeating it per row would multiply the turn's
//     cost for anything that sums the column.
//   - Purpose: st.source — "chat"/"release"/"rotation" for parent
//     turns, "subagent" for subagent runs.
//   - Agent: st.slug — for subagents this is the PARENT slug, which
//     is the correct billing rollup (subagents don't have their own
//     billing identity).
//
// Zero-token done events still get a row written; matches the SDK
// transport's behavior where the recorder fires on every "result"
// regardless of token counts.
func (s *Server) recordAgentpodUsage(st *agentpodTurnState, ev agentpod.TurnEvent) {
	model := ev.Model
	if model == "" {
		// The pod never saw an ID: fall back to the model we asked for
		// at publish time. A driver never reports a provider's
		// placeholder id here — it attributes those tokens to the model
		// that answered (see claudeagent.attributeModel).
		model = st.model
	}
	usage := provider.TokenUsage{
		InputTokens:       ev.InputTokens,
		OutputTokens:      ev.OutputTokens,
		CacheReadTokens:   ev.CacheReadTokens,
		CacheCreateTokens: ev.CacheCreateTokens,
	}
	// Buckets to record. Single-model turns (the common case, and every
	// turn from a pod predating the split) yield exactly one, identical
	// to what this wrote before ByModel existed.
	buckets := []agentpod.TurnModelUsage{{
		Model:             model,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CacheReadTokens:   usage.CacheReadTokens,
		CacheCreateTokens: usage.CacheCreateTokens,
	}}
	if len(ev.ByModel) > 0 {
		buckets = ev.ByModel
	}
	for i, b := range buckets {
		bModel := b.Model
		if bModel == "" {
			bModel = model
		}
		bUsage := provider.TokenUsage{
			InputTokens:       b.InputTokens,
			OutputTokens:      b.OutputTokens,
			CacheReadTokens:   b.CacheReadTokens,
			CacheCreateTokens: b.CacheCreateTokens,
		}
		// The CLI's turn total rides the first row only; see the doc
		// comment. Rows after it fall through to local pricing, which is
		// correct per-bucket because the tokens are already split.
		var evCost float64
		if i == 0 {
			evCost = ev.CostUSD
		}
		s.appendUsageRow(st, bModel, bUsage, evCost)
	}
}

// appendUsageRow writes one usage.jsonl row, resolving cost from the
// CLI-reported value when present and the local pricing table
// otherwise.
func (s *Server) appendUsageRow(st *agentpodTurnState, model string, usage provider.TokenUsage, reported float64) {
	cost := reported
	if cost == 0 {
		cost = s.Provider.Price(model, usage)
		// Provider.Price returns 0 for any model id the provider does not
		// know. Combined with cost==0 above that yields silent
		// $0-cost rows whenever a misconfigured (typo) or
		// newly-rolled-out model id slips through — easy to miss in
		// usage.jsonl because everything else looks normal. Log once
		// per model id so the first hit is loud; subsequent ones stay
		// quiet to avoid log spam on a busy day.
		if cost == 0 && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
			if _, already := s.unknownModelLogged.LoadOrStore(model, struct{}{}); !already {
				log.Printf("usage: model %q unknown to the %s provider; cost recorded as $0 (in=%d out=%d cache_r=%d cache_w=%d)",
					model, s.Provider.Name(), usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens, usage.CacheCreateTokens)
			}
		}
	}
	if err := s.Store.AppendUsage(store.UsageRecord{
		TS:                time.Now().UTC(),
		Agent:             st.slug,
		Purpose:           st.source,
		Model:             model,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CacheReadTokens:   usage.CacheReadTokens,
		CacheCreateTokens: usage.CacheCreateTokens,
		CostUSD:           cost,
	}); err != nil {
		log.Printf("agentpod turn %s: append usage: %v", st.slug, err)
	}
}

// onAgentpodFailed handles a TurnEventFailed: records partial spend,
// then hands a parent chat turn to finishFailedChatTurn (marker row,
// error UI event, post-loop teardown) and a subagent turn to its
// blocked caller.
func (s *Server) onAgentpodFailed(st *agentpodTurnState, ev agentpod.TurnEvent) {
	// Record partial spend before any short-circuit. The agent pod's
	// failure paths (parent chat-turn + subagent) ship whatever usage
	// the stream captured before the failure on the failed event —
	// Anthropic billed for every assistant message that landed
	// pre-crash, so a Stop / subprocess-died with non-zero tokens
	// must still land a usage.jsonl row. Zero-token failed events
	// (e.g. synthesized SSE-disconnect, stream-open errors) get no
	// row — there's nothing to bill for.
	if ev.InputTokens > 0 || ev.OutputTokens > 0 ||
		ev.CacheReadTokens > 0 || ev.CacheCreateTokens > 0 {
		s.recordAgentpodUsage(st, ev)
	}
	if st.kind == agentpod.ChatTurnKindSubagent {
		// Subagent path: no chat-hub error emit, no disruption row
		// (subagent failure is part of the parent's tool_result —
		// not a runtime crash). Surface the failure to the blocked
		// SubagentService.runJob caller via the done channel.
		var err error
		switch ev.FailedReason {
		case agentpod.FailedReasonCancelled:
			err = errSubagentCancelled
		default:
			detail := ev.FailedDetail
			if detail == "" {
				detail = "(no detail)"
			}
			err = fmt.Errorf("subagent failed: %s", detail)
		}
		s.finalizeSubagentTurn(st, subagentTurnResult{FinalText: ev.Text, Err: err})
		return
	}
	s.finishFailedChatTurn(st, ev.FailedReason, ev.FailedDetail)
}

// finishFailedChatTurn is the one exit for a parent chat turn that did
// not complete, whether the pod said so (TurnEventFailed) or core
// inferred it (done with StopReason "error"). What it writes depends
// on the reason, and the three cases are the three things that can
// stop an agent:
//
//   - cancelled: the CEO pressed Stop. Nothing to write — handleAgentStop
//     wrote the user-interruption marker at click time — and no error
//     event, since a cancel is not an error from the CEO's view.
//   - subprocess-died: the CLI process crashed. A kind:runtime-disruption
//     row; the spawn gate resumes the agent from it (and quarantines a
//     crash loop).
//   - everything else (other-error, turn-error, and any reason a newer
//     pod may add): the turn stopped on something a retry would hit
//     again. A kind:turn-error row; the spawn gate holds the agent until
//     something new arrives, and the org snapshot marks it errored.
//
// The marker is written and its chat_marker emitted BEFORE the error
// event: the browser's error handler closes the EventSource, so a
// marker emitted after it reaches only the replay buffer and the live
// page never paints it. Then the same post-loop teardown as
// onAgentpodDone, so chat-hub and active-turn bookkeeping stay
// symmetric across exit paths.
func (s *Server) finishFailedChatTurn(st *agentpodTurnState, reason, detail string) {
	// Snap the ring / banner / footer-stat for every chat-turn
	// failure — including cancel, where the partial assistant text
	// flushed on stop still grew the chat. Emit BEFORE the error
	// emit so the JS error handler (which closes the EventSource)
	// can't drop our update.
	s.emitChatFillSnap(st)
	emitDetail := detail
	if emitDetail == "" {
		emitDetail = "(no detail)"
	}
	// A rotation is finalized whichever way its turn ends, so the error
	// event says so exactly as done does: the page holds the stream open
	// for the rotation_done that follows instead of closing on the error
	// and settling on a chat that is about to be archived.
	failed := map[string]any{"error": emitDetail, "rotation": s.hasRotationPending(st.slug)}
	switch reason {
	case agentpod.FailedReasonCancelled:
		// no error emit; handleAgentStop already wrote the
		// user-interruption marker synchronously at click time.
	case agentpod.FailedReasonSubprocessDied:
		// Text the agent produced before the failure is real work;
		// land it as its own row, above the marker, so the transcript
		// reads in the order things happened.
		st.flushAssistant(s)
		s.finalizeSubprocessDisruption(st.slug, st.hub, fmt.Errorf("agent runtime reported subprocess-died: %s", emitDetail))
		st.emitTerminal("error", failed)
	default:
		st.flushAssistant(s)
		// The raw detail: finalizeTurnError words the empty case itself.
		s.finalizeTurnError(st.slug, st.hub, detail)
		st.emitTerminal("error", failed)
	}
	s.finalizeAgentpodTurn(st)
}

// emitChatFillSnap re-reads the freshly-flushed chat history and
// emits a "chat_fill" event with the context-fill numbers the chat
// API reports, so the web app's context badge follows a running chat
// instead of staying at the values it loaded with. Called from every
// chat-turn
// teardown path (done, error, cancel).
//
// Best-effort: a history read failure just skips the emit, leaving
// the previous (stale) ring value in place. That's better than
// emitting wrong numbers.
func (s *Server) emitChatFillSnap(st *agentpodTurnState) {
	hist, err := s.Store.ReadChatHistory(st.slug)
	if err != nil {
		return
	}
	var agentModel string
	if a, gerr := s.Store.GetAgent(st.slug); gerr == nil {
		agentModel = a.Model
	}
	// onAgentpodDone wrote the fresh occupancy just before calling this
	// on the done path; on the error/cancel path the prior turn's value
	// (or 0 → chars/4 fallback) is what's available.
	cw, _ := s.Store.ReadContextWindow(st.slug)
	stats := s.computeChatFillStats(agentModel, s.AgentModel, hist, cw.ContextTokens)
	st.emit("chat_fill", map[string]any{
		"chat_fill_pct":       stats.FillPct,
		"chat_token_est":      stats.TokenEst,
		"chat_context_limit":  stats.ContextLimit,
		"chat_long_thresh":    stats.LongThresh,
		"chat_fill_bucket":    stats.FillBucket,
		"chat_resolved_model": stats.ResolvedModel,
	})
}

// errSubagentCancelled marks a subagent run that exited because the
// parent's chat-turn (or the parent's HTTP context) was cancelled.
// Surfaces in the parent's tool_result body.
//
// It UNWRAPS TO context.Canceled, and that is load-bearing rather than
// tidiness. Cancellation reaches a job by two different routes:
//
//   - the job's own context being cancelled (subagent_cancel, Stop's
//     CancelAllSubagents) — which arrives as context.Canceled, and
//   - the POD reporting FailedReasonCancelled for a turn it cancelled
//     — which arrives as this sentinel.
//
// finishJob classifies on errors.Is(err, context.Canceled). Were the
// sentinel not to unwrap, the second route would land in "failed" and
// sail past the guard that stops a cancelled job from reporting back:
// the parent would get "Background task … FAILED" for work it asked to
// stop, and that delivery would wake the agent the CEO just stopped.
//
// Unwrapping here rather than special-casing the comparison site means
// any context.Canceled check classifies it correctly.
type subagentCancelledError struct{}

func (subagentCancelledError) Error() string { return "subagent: cancelled" }
func (subagentCancelledError) Unwrap() error { return context.Canceled }

var errSubagentCancelled error = subagentCancelledError{}

// agentpodSubagentCancelGrace is how long DriveSubagent waits after
// firing EventCancelTurn for the runtime to actually POST done/failed
// before giving up on the wait. Sized to cover SIGTERM →
// graceful-exit → POST round trip.
var agentpodSubagentCancelGrace = 10 * time.Second

// DriveSubagent implements SubagentDriver — runs one subagent
// end-to-end: installs the per-turn-id state, publishes the
// ChatTurnKindSubagent event at the parent's agent pod, and blocks
// until the runtime POSTs done/failed.
//
// Context cancel (parent CLI died → MCP disconnected → /run-subagent
// HTTP ctx cancelled) fires EventCancelTurn at the agent pod, which
// cancels the subagent's run (its driver ends it) and POSTs
// failed/cancelled.
// DriveSubagent returns the partial finalText + ctx.Err.
func (s *Server) DriveSubagent(ctx context.Context, parent, turnID, subagentID string, spec agentpod.SubagentSpec) (string, error) {
	if s.AgentpodHub == nil {
		return "", fmt.Errorf("agentpod hub not configured")
	}
	if s.AgentpodHub.SubscriberCount(parent) == 0 {
		return "", fmt.Errorf("agent pod not connected for %s", parent)
	}
	st := s.installAgentpodSubagentTurn(parent, turnID, subagentID, spec.Model)

	specCopy := spec
	ev := agentpod.ChatTurnEvent{
		TurnID:   turnID,
		Slug:     parent,
		Source:   "subagent",
		Model:    spec.Model,
		Kind:     agentpod.ChatTurnKindSubagent,
		Subagent: &specCopy,
	}
	if err := s.PublishAgentpodEvent(parent, agentpod.EventChatTurn, ev); err != nil {
		s.dropAgentpodSubagentTurn(turnID)
		return "", fmt.Errorf("agentpod publish subagent chat-turn: %w", err)
	}

	select {
	case res := <-st.done:
		return res.FinalText, res.Err
	case <-ctx.Done():
		// Parent disconnected. Fire cancel-turn and bound the wait
		// for the runtime's done/failed so a dead pod can't hang
		// the request.
		if err := s.PublishAgentpodEvent(parent, agentpod.EventCancelTurn, agentpod.CancelTurnEvent{
			TurnID: turnID,
			Slug:   parent,
		}); err != nil {
			log.Printf("subagent %s/%s: publish cancel: %v", parent, turnID, err)
		}
		grace := s.clk().NewTimer(agentpodSubagentCancelGrace)
		defer grace.Stop()
		select {
		case res := <-st.done:
			return res.FinalText, res.Err
		case <-grace.C():
			s.dropAgentpodSubagentTurn(turnID)
			return "", ctx.Err()
		}
	}
}

// finalizeSubagentTurn signals the blocking runJob caller and
// evicts the per-turn-id entry. Idempotent on the channel send (it's
// buffered size 1; double-finalize would just drop the second value).
func (s *Server) finalizeSubagentTurn(st *agentpodTurnState, result subagentTurnResult) {
	select {
	case st.done <- result:
	default:
		// Already signaled (double-finalize race; benign).
	}
	s.dropAgentpodSubagentTurn(st.turnID)
}

// finalizeAgentpodTurn is the post-loop teardown for an agent-pod
// chat turn. Order matters:
//
//  1. Rotation finalize (archives chat, clears CLI session id,
//     refreshes filesystem).
//  2. flushAndComplete: under streamMu, drains buffered deliveries
//     and flips the hub's completed flag in one acquisition so a
//     concurrent deliverToAgent can't sneak in mid-drain. After a
//     Send now the partial reply is flushed first and a
//     paused-to-deliver marker precedes the drained messages. After
//     a rotation, a "rotation_done" emit follows: it tells the
//     browser holding the stream past "done" that the fresh chat is
//     settled and can be fetched.
//  3. hub.close so existing subscribers exit.
//  4. Store.ClearActiveTurn so the next boot scan doesn't mistake
//     this completion for a kill.
//  5. time.AfterFunc to evict the lingering hub from chatHubs after
//     chatHubLingerDuration.
//  6. Follow-up spawn rule: if any deliveries flushed, the agent
//     has new work — spawn a chat loop. Otherwise, the defensive
//     spawnFollowupIfNewerReceived catches anything that slipped
//     past the buffer (shouldn't happen with correct wiring; kept
//     as a safety net).
//
// Stop handling: handleAgentStop writes the user-interruption marker
// to chat.jsonl synchronously at click time and publishes EventCancelTurn
// at the agent runtime (see publishCancelTurn). The runtime cancels
// the in-pod stream and POSTs back a failed event with
// FailedReasonCancelled, which lands here through the standard
// finalize path. No marker write happens here — it has already landed.
func (s *Server) finalizeAgentpodTurn(st *agentpodTurnState) {
	// Identity-check the eviction: a follow-up spawn (driven by the
	// flushed-buffer branch below) calls publishAgentpodChatTurn,
	// which installs a fresh state under the same slug. Unconditional
	// delete on return would clobber that fresh state and drop every
	// inbound TurnEvent for the new turn.
	defer s.dropAgentpodTurnIfMatches(st)
	hub := st.hub
	rotated := s.consumeRotationIfReady(st.slug)
	if rotated {
		s.finalizeRotation(st.slug)
	}
	// No graph pass: whatever this turn published was versioned by
	// artifact_publish before the tool call returned.
	var flushed int
	if hub != nil {
		// Send now ended this turn early. Unlike Stop, what the agent
		// had written so far is kept — the CEO paused it, they did not
		// reject it — so it lands first, then the marker, then the
		// messages, and the follow-up turn reads them in that order.
		// The cancel path does not flush on its own (finishFailedChatTurn),
		// and on the done path this flush is a no-op.
		//
		// The latch is read inside the drain's streamMu hold (the hook
		// runs there; see flushAndCompleteMarked), so a Send now that
		// found these messages pending — and answered 202 — always gets
		// its marker in front of them, even when it lands while this
		// finalize is already under way.
		flushed = s.flushAndCompleteMarked(st.slug, hub, func() *store.ChatMessage {
			if !st.pausedForDelivery {
				return nil
			}
			st.flushAssistant(s)
			m := pausedToDeliverMarker(s.clk().Now())
			return &m
		})
		// rotation_done goes out last, once the hub is completed and the
		// held messages are in the fresh chat. The page answers it by
		// fetching the chat at once, and that answer has to be the
		// settled one: fetched any earlier it says the agent is still
		// running, the page reopens the stream, and this hub replays the
		// turn that was just archived onto the screen that was just
		// cleared.
		if rotated {
			hub.Emit("rotation_done", map[string]any{})
		}
		hub.close()
	}
	if err := s.Store.ClearActiveTurn(st.slug); err != nil {
		log.Printf("agentpod turn %s: clear active turn: %v", st.slug, err)
	}
	if hub != nil {
		time.AfterFunc(chatHubLingerDuration, func() {
			s.evictHubIfMatches(st.slug, hub)
			s.NotifyOrgState()
		})
	}
	source := "chat"
	spawnReceivedTS := time.Time{}
	if hub != nil {
		source = hub.spawnSource
		spawnReceivedTS = hub.spawnReceivedTS
	}
	if flushed > 0 {
		s.spawnChatLoopIfIdle(st.slug, source)
	} else if !rotated {
		s.spawnFollowupIfNewerReceived(st.slug, spawnReceivedTS, source)
	}
}

// publishCancelTurn fires EventCancelTurn at the agent runtime for
// slug's in-flight turn (if any). The runtime cancels the in-pod
// stream and POSTs back a failed event with FailedReasonCancelled
// — which finalizeAgentpodTurn handles like any other terminal
// event. Skips silently when no hub is wired or there's no in-flight
// turn; Stop fires on hub presence (handleAgentStop's gate), not on
// agentpod turn presence, so a no-op cancel is fine.
//
// Cancels the parent's chat turn AND every in-flight subagent turn
// owned by this parent — stopping the parent should cascade to
// every spawned `claude -p` so a Stop click clears the whole tree.
func (s *Server) publishCancelTurn(slug string) {
	if s.AgentpodHub == nil {
		return
	}
	cancelIDs := s.collectInflightTurnIDs(slug)
	for _, id := range cancelIDs {
		if err := s.PublishAgentpodEvent(slug, agentpod.EventCancelTurn, agentpod.CancelTurnEvent{
			TurnID: id,
			Slug:   slug,
		}); err != nil {
			log.Printf("chat %s: agentpod publish cancel-turn %s: %v", slug, id, err)
		}
	}
}

// preemptForDelivery asks slug's in-flight PARENT chat turn to wind up
// early, because a message arrived for it. Returns true when an
// interrupt was published.
//
// This is what makes a released message reach a busy agent promptly
// instead of waiting out the turn. Everything after the turn ends is
// machinery that already existed: finalizeAgentpodTurn flushes the
// buffered deliveries and, because something flushed, spawns the
// follow-up loop — which reads the new message as part of its history.
//
// Three deliberate narrowings:
//
//   - PARENT TURN ONLY. publishCancelTurn cancels the parent plus
//     every subagent it spawned, which is right for a Stop click and
//     wrong here: a subagent batch is expensive in-flight work that a
//     delivery has no business destroying. If the parent is blocked on
//     one, the runner's boundary rule defers the interrupt until the
//     batch returns anyway.
//   - ONCE PER TURN, via preemptRequested. A release-all of twenty
//     messages should not publish twenty cancels at the same turn.
//   - NO-OP WITH NO TURN STATE. If the agent reads busy but core holds
//     no agentpod turn for it, there is nothing to interrupt and the
//     message simply stays buffered.
//
// The turn surfaces as FailedReasonCancelled, which onAgentpodFailed
// already treats as "end quietly": partial usage recorded, no error
// emit, and no user-interruption marker (that is written by
// handleAgentStop at click time, a path this does not touch).
func (s *Server) preemptForDelivery(slug, turnID string) bool {
	if s.AgentpodHub == nil || turnID == "" {
		return false
	}
	if err := s.PublishAgentpodEvent(slug, agentpod.EventCancelTurn, agentpod.CancelTurnEvent{
		TurnID: turnID,
		Slug:   slug,
	}); err != nil {
		log.Printf("delivery preempt %s: publish cancel-turn %s: %v", slug, turnID, err)
		return false
	}
	log.Printf("delivery preempt %s: asked turn %s to wind up for an inbound message", slug, turnID)
	return true
}

// foldForDelivery hands one buffered message to a turn that is
// already running, asking the pod to splice it into that turn rather
// than end it.
//
// Fire-and-forget by design: the outcome arrives asynchronously as a
// TurnEventFolded. A publish failure is the one case we resolve here
// and now, by falling straight back — the pod never heard us, so no
// answer is coming.
func (s *Server) foldForDelivery(slug, turnID, deliveryID, text string) {
	if s.AgentpodHub == nil || turnID == "" {
		s.fallBackToPreempt(slug, turnID, deliveryID, "no agentpod hub")
		return
	}
	if err := s.PublishAgentpodEvent(slug, agentpod.EventFoldMessage, agentpod.FoldMessageEvent{
		TurnID:     turnID,
		Slug:       slug,
		DeliveryID: deliveryID,
		Text:       text,
	}); err != nil {
		log.Printf("delivery fold %s: publish fold-message %s: %v", slug, turnID, err)
		s.fallBackToPreempt(slug, turnID, deliveryID, "publish failed")
		return
	}
	log.Printf("delivery fold %s: offered message %s to in-flight turn %s", slug, deliveryID, turnID)
}

// onAgentpodFolded handles a TurnEventFolded: the pod reporting what
// became of one message we handed to a running turn.
//
// Landed means that turn consumed it and will answer it, so it belongs
// in chat.jsonl now, at this point in the stream — that is the whole
// benefit, the message is answered inside the turn already in flight.
//
// Not landed is not a failure. The pod refused (an older CLI, a turn
// already winding up) or the turn ended before a fold seam came up. In
// both cases the message is still buffered and still ours to deliver,
// so we fall back to preempting the running turn.
func (s *Server) onAgentpodFolded(st *agentpodTurnState, ev agentpod.TurnEvent) {
	if ev.DeliveryID == "" {
		return
	}
	if ev.Landed {
		// Close the bubble the agent was mid-way through first. Text
		// already assembled was written BEFORE this message arrived,
		// and chat.jsonl is read in file order — leaving it buffered
		// would file the CEO's message above the reply that preceded
		// it. Matters most when the fold landed as a continuation
		// (the turn had just finished its final text, so nothing else
		// was going to flush it).
		st.flushAssistant(s)
		s.flushFoldedDelivery(st.slug, ev.DeliveryID)
		return
	}
	log.Printf("delivery fold %s: message %s did not make turn %s; falling back", st.slug, ev.DeliveryID, st.turnID)
	s.fallBackToPreempt(st.slug, st.turnID, ev.DeliveryID, "fold missed")
}

// fallBackToPreempt asks the turn to wind up so the buffered message
// is delivered the way it was before folds: flush + follow-up spawn in
// finalizeAgentpodTurn.
//
// Latched on preemptRequested so N buffered messages whose folds all
// miss produce ONE cancel, not N.
func (s *Server) fallBackToPreempt(slug, turnID, deliveryID, why string) {
	if turnID == "" {
		return
	}
	s.streamMu.Lock()
	st := s.agentpodTurns[slug]
	if st == nil || st.turnID != turnID || st.preemptRequested {
		s.streamMu.Unlock()
		return
	}
	st.preemptRequested = true
	s.streamMu.Unlock()
	log.Printf("delivery fold %s: preempting turn %s for message %s (%s)", slug, turnID, deliveryID, why)
	s.preemptForDelivery(slug, turnID)
}

// flushFoldedDelivery appends one folded message to chat.jsonl at the
// moment the running turn consumed it, and drops it from the pending
// buffer so the end-of-turn flush does not write it twice.
//
// Append order stays correct without any sorting: the CLI folds
// messages in the order it queued them, so their landed reports arrive
// in the order they were buffered.
//
// streamMu is held across the append for the same reason
// deliverToAgent holds it across its direct-append branch — a
// concurrent delivery must not interleave between the lookup and the
// write.
func (s *Server) flushFoldedDelivery(slug, deliveryID string) {
	s.streamMu.Lock()
	pending := s.pendingDeliveries[slug]
	idx := -1
	for i, pd := range pending {
		if pd.id == deliveryID {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.streamMu.Unlock()
		// Already flushed (a duplicate report, or the turn ended and
		// flushAndComplete drained the buffer first). Nothing to do —
		// the message is in chat.jsonl either way.
		return
	}
	pd := pending[idx]
	msg := pd.msg
	s.pendingDeliveries[slug] = append(pending[:idx:idx], pending[idx+1:]...)
	if len(s.pendingDeliveries[slug]) == 0 {
		delete(s.pendingDeliveries, slug)
	}
	// The fold linked this message's files before the model heard of
	// them (foldText); the link after Unlock only confirms it.
	err := s.Store.AppendChatMessageLinkLater(slug, msg)
	hub := s.chatHubs[slug]
	if err != nil {
		queuePendingEvent(hub, pd, "pending_deleted", apitypes.PendingRef{ID: pd.id})
		s.streamMu.Unlock()
		hub.flushOrdered()
		log.Printf("flush folded delivery %s/%s: %v", slug, deliveryID, err)
		return
	}
	// Retire the pending bubble BEFORE the chat_message below paints
	// the row: the client re-keys the pending bubble to this ts and
	// moves it to the seam, so the chat_message that follows dedups
	// instead of painting a second bubble. Queued under the lock with
	// the buffer change, like every pending_* event; flushOrdered has
	// broadcast it by the time it returns.
	queuePendingEvent(hub, pd, "pending_delivered", apitypes.PendingDelivered{ID: pd.id, TS: msg.TS.UnixMilli()})
	s.streamMu.Unlock()
	s.Store.LinkChatAttachments(slug)
	hub.flushOrdered()
	log.Printf("delivery fold %s: message %s landed in the running turn", slug, deliveryID)

	// Paint the bubble now. A folded message is answered inside a turn
	// that is still streaming, so without this it stays invisible
	// until something re-renders from disk — the agent would appear to
	// answer something the user cannot see.
	//
	// Reuses emitTriggeringChatMessages rather than emitting by hand:
	// it already carries the narrowing to the kinds that paint from the
	// event (other received kinds reach the client through the chat
	// API's rows) and the payload shape. A one-entry history emits
	// exactly this message, or nothing when the kind is not paintable.
	// The client dedups on the row's ts, so it never paints a second
	// bubble.
	emitTriggeringChatMessages(hub, s.Provider, s.Store.Root(), []store.ChatMessage{msg})

	s.NotifyOrgState()
}

// collectInflightTurnIDs returns the turn-ids of every in-flight
// agentpod turn owned by slug — the parent's own chat turn plus
// any subagent runs the parent spawned.
func (s *Server) collectInflightTurnIDs(slug string) []string {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	var ids []string
	if st, ok := s.agentpodTurns[slug]; ok && st != nil {
		ids = append(ids, st.turnID)
	}
	for id, st := range s.agentpodSubagentTurns {
		if st != nil && st.slug == slug {
			ids = append(ids, id)
		}
	}
	return ids
}

// dropAgentpodTurn removes slug's in-flight turn state. Used by the
// chat-spawn fallback paths (e.g. publishAgentpodChatTurn cleanup
// after a marshal/publish failure). Safe to call when no entry
// exists.
func (s *Server) dropAgentpodTurn(slug string) {
	s.streamMu.Lock()
	delete(s.agentpodTurns, slug)
	s.streamMu.Unlock()
}

// dropAgentpodTurnIfMatches deletes slug's in-flight state only when
// it still belongs to st (turnID match). Used by the finalize defer
// so a follow-up spawn that landed mid-finalize — installing a fresh
// state under the same slug — survives the cleanup.
func (s *Server) dropAgentpodTurnIfMatches(st *agentpodTurnState) {
	s.streamMu.Lock()
	if cur, ok := s.agentpodTurns[st.slug]; ok && cur == st {
		delete(s.agentpodTurns, st.slug)
	}
	s.streamMu.Unlock()
}

// agentpodDisconnectGrace is how long we wait after the last events
// SSE drops for slug before declaring the runtime presumed-dead and
// synthesizing a failed event for any in-flight turn. Sized to cover
// a normal pod restart (image pull on a warm cache + reschedule + SSE
// reconnect) without lingering after a truly-dead pod. Var so tests
// can shrink it.
var agentpodDisconnectGrace = 30 * time.Second

// handleAgentpodDisconnect is fired by the events SSE handler's
// deferred cleanup when its client connection ends. If this was the
// last subscriber for slug AND there's an in-flight turn, we schedule
// a grace-period cleanup: if no new subscriber appears, synthesize a
// `failed` event so finalizeAgentpodTurn runs and drains the per-slug
// state (latestRotations, pendingDeliveries, the
// chat hub). Without this, a runtime that crashes without sending
// done/failed leaves stale rotation state that the next agent's chat
// turn would consume incorrectly.
func (s *Server) handleAgentpodDisconnect(slug string) {
	if s.AgentpodHub == nil {
		return
	}
	if s.AgentpodHub.SubscriberCount(slug) > 0 {
		// Another subscriber is still attached — nothing to do.
		return
	}
	// A pod can die with background subagent turns in flight and NO
	// parent turn: the agent dispatches a batch, ends its turn, and
	// the fan-out is what kills the pod — N CLIs against one memory
	// limit is the OOM shape. Those turns are registered separately
	// from the parent's, so bailing on a nil parent turn (as this used
	// to) left every one of them blocked in DriveSubagent forever,
	// with the parent showing "waiting on N tasks" for work whose
	// process no longer exists.
	//
	// Snapshot both here, at disconnect time, and re-confirm each
	// after the grace.
	st := s.lookupAgentpodTurn(slug)
	subagentTurns := s.collectInflightSubagentTurns(slug)
	if st == nil && len(subagentTurns) == 0 {
		return
	}
	turnID := ""
	if st != nil {
		turnID = st.turnID
	}
	time.AfterFunc(agentpodDisconnectGrace, func() {
		// Re-check on the timer's goroutine: a reconnecting runtime
		// inside the grace window cancels this synthesis.
		if s.AgentpodHub.SubscriberCount(slug) > 0 {
			return
		}
		// Fail the orphaned subagent turns first, so the parent's own
		// finalize (below) sees a settled batch rather than racing it.
		//
		// Identity is checked by POINTER, not by turn-id: ids are
		// generated per run, but a state that was replaced rather than
		// finished must not be failed out from under its new owner.
		// A turn that completed normally in the window is already gone
		// from the map and is skipped.
		for id, want := range subagentTurns {
			s.streamMu.Lock()
			cur, stillThere := s.agentpodSubagentTurns[id]
			s.streamMu.Unlock()
			if !stillThere || cur != want {
				continue
			}
			log.Printf("agentpod subagent turn %s/%s: subscriber gone for %s without done/failed; synthesizing failed (pod presumed dead)",
				slug, id, agentpodDisconnectGrace)
			// Routes to finalizeSubagentTurn, which hands the error to
			// the blocked DriveSubagent. That unwinds runJob →
			// finishJob, which is what finally moves the job out of
			// "running" and releases the parent's waiting indicator.
			s.onAgentpodFailed(cur, agentpod.TurnEvent{
				Kind:         agentpod.TurnEventFailed,
				FailedReason: agentpod.FailedReasonOtherError,
				FailedDetail: "agent runtime SSE disconnected without terminator event (pod presumed dead)",
			})
		}
		if st == nil {
			return
		}
		// Confirm the same turn is still in flight. If a new turn
		// installed via InitAgentpodTurnState since we scheduled,
		// don't synthesize a failure for it — it has its own
		// subscriber lifecycle.
		cur := s.lookupAgentpodTurn(slug)
		if cur == nil || cur.turnID != turnID {
			return
		}
		// If the user clicked Stop while the runtime was already
		// dropped, treat the synthesized event as a cancellation
		// rather than an error — Stop is the user's intent and
		// surfacing "SSE disconnected" as an error in the UI is
		// misleading. The user-interruption marker was already
		// written synchronously by handleAgentStop at click time.
		reason := agentpod.FailedReasonOtherError
		detail := "agent runtime SSE disconnected without terminator event"
		if cur.hub != nil && cur.hub.peekInterruptRequested() {
			reason = agentpod.FailedReasonCancelled
			detail = "agent runtime disconnected; user had pressed Stop"
		}
		log.Printf("agentpod turn %s/%s: subscriber gone for %s without done/failed; synthesizing %s", slug, turnID, agentpodDisconnectGrace, reason)
		s.onAgentpodFailed(cur, agentpod.TurnEvent{
			Kind:         agentpod.TurnEventFailed,
			FailedReason: reason,
			FailedDetail: detail,
		})
	})
}
