package agentpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/provider"
)

// Runtime is the in-pod agent runtime: it consumes events from core
// over the agentpod.Client's SSE stream and turns chat-turn events
// into model calls via a provider.Client (typically claudeagent.Client
// in production; a mock in tests). Stream events are translated to
// agentpod.TurnEvents and POSTed back so core can persist them and
// broadcast to the chat-hub SSE-to-browser path.
//
// One Runtime per agent pod. Bound to a slug, owned by the Kivali
// agent subcommand's main function. Uses two channels of comms:
//   - inbound (core → agentpod): the events SSE stream, opened via
//     Client.RunEvents, carries chat-turn / cache-invalidate events.
//   - outbound (agentpod → core): per-turn TurnEvent POSTs via
//     Client.PostTurnEvent.
//
// Concurrency:
//   - Parent chat-turns are serialized per pod (chat-hub already
//     enforces per-slug single-flight on the core side).
//   - Subagent runs (Kind=subagent) fan out concurrently — each
//     gets its own Claude.RunSubagent run + goroutine — so a parent
//     `subagent([t1, t2, t3])` batch runs in parallel.
//
// EventCancelTurn (sent on a Stop click) is the one event that needs
// to act on an in-flight turn. inflight tracks the per-turn cancel
// func so the cancel handler can short-circuit the stream — without
// it, the runtime would keep streaming until the model finished its
// current chunk and Stop would feel ignored. Same machinery is used
// by subagent runs so parent-side cancel cascades onto every in-flight
// subagent run for that parent.
//
// The runtime knows nothing about the provider behind Claude: which
// binary runs, its flags and file formats are the driver's (see the
// provider package doc). The runtime owns which tools a subagent gets
// (the spec's ToolSet) and the MCP server it loads.
type Runtime struct {
	Client *Client         // outbound UDS client
	Claude provider.Client // the driver in prod, a mock in tests
	// Provider is the same driver, asked for defaults (the subagent
	// model when neither the spec nor SubagentModel names one).
	Provider provider.Provider
	Logger   *log.Logger

	// Subagent configuration. Optional — required only when the
	// runtime is expected to drive Kind=subagent chat-turns. Tests
	// that exercise only chat-turn path leave them empty.
	KivaliBinary  string // absolute path to the Kivali binary (subagent's MCP server)
	CoreUDS       string // UDS socket path the subagent's MCP dials
	ScratchRoot   string // /scratch root; per-subagent dirs land at <ScratchRoot>/subagents/<id>/
	SubagentModel string // default model when SubagentSpec.Model is empty

	mu       sync.Mutex
	inflight map[string]context.CancelFunc // turnID → cancel

	// folders holds the fold-capable stream of each in-flight turn, so
	// EventFoldMessage can splice into a turn that is already running.
	// Only streams whose transport implements provider.Folder land here;
	// a turn driven by a transport that cannot fold simply has no
	// entry, and the fold is refused.
	folders map[string]provider.Folder // turnID → fold-capable stream

	// foldIDs maps the runner's own fold uuid back to core's delivery
	// id, so the StreamMessageFolded event coming off the stream can
	// be reported in terms core understands.
	foldIDs map[string]string // runner fold uuid → core delivery id
}

// sessionResetter is the optional capability the runtime needs from
// its provider.Client to honor EventSessionReset: tear down the warm
// `claude -p` runner for a slug so the next turn spawns a fresh CLI
// session. claudeagent.Client implements it; mocks need not — the
// reset is then a logged no-op (the warm-runner amnesia problem only
// exists for the persistent CLI runner).
type sessionResetter interface {
	ResetSession(slug string) error
}

// Run is the runtime's main loop. Blocks on the events stream until
// ctx is canceled. Reconnect-with-backoff is handled by the
// underlying Client.RunEvents; this loop's job is event dispatch.
//
// Returns the error RunEvents surfaced (typically ctx.Err on a
// clean shutdown). Callers should treat any non-nil non-ctx-err
// return as fatal-to-the-pod and exit.
func (r *Runtime) Run(ctx context.Context) error {
	if r.Client == nil {
		return errors.New("agentpod.Runtime: Client is nil")
	}
	if r.Logger == nil {
		r.Logger = log.New(log.Writer(), "agentpod: ", log.LstdFlags)
	}
	return r.Client.RunEvents(ctx, func(ev Event) error {
		return r.handleEvent(ctx, ev)
	})
}

// handleEvent dispatches a single inbound event. Errors returned
// here propagate up through RunEvents to terminate the loop, so
// most failures (a single chat-turn driving error) are absorbed
// here and surfaced as a "failed" TurnEvent rather than killing
// the pod.
//
// Chat-turn drives its model stream on a fresh goroutine so the
// events loop stays responsive to EventCancelTurn (which would
// otherwise queue behind a long-running turn and arrive too late
// to short-circuit the stream). Per-slug serialization is preserved
// by the chat hub on the core side; this runtime owns one slug, so
// only one chat-turn goroutine is ever in flight per pod in
// practice.
func (r *Runtime) handleEvent(ctx context.Context, ev Event) error {
	switch ev.Type {
	case EventChatTurn:
		// Peek the kind without committing to a path. A subagent run
		// fans out on its own goroutine so multiple `claude -p`
		// subprocesses can drive in parallel from one parent batch;
		// the parent's chat-turn also runs on a goroutine so the events
		// loop stays responsive to EventCancelTurn mid-stream.
		var peek struct {
			Kind ChatTurnKind `json:"kind"`
		}
		_ = json.Unmarshal(ev.Data, &peek)
		if peek.Kind == ChatTurnKindSubagent {
			go func() {
				if err := r.handleSubagentTurn(ctx, ev); err != nil {
					r.Logger.Printf("subagent-turn: %v", err)
				}
			}()
			return nil
		}
		go func() {
			if err := r.handleChatTurn(ctx, ev); err != nil {
				r.Logger.Printf("chat-turn: %v", err)
			}
		}()
	case EventCancelTurn:
		r.handleCancelTurn(ev)
	case EventFoldMessage:
		r.handleFoldMessage(ctx, ev)
	case EventCacheInvalidate:
		var ci CacheInvalidateEvent
		if err := json.Unmarshal(ev.Data, &ci); err != nil {
			r.Logger.Printf("cache-invalidate: malformed payload: %v", err)
			return nil
		}
		cache := r.Client.Cache()
		if cache == nil {
			// No cache wired (dev / --skip-claude).
			// Invalidation is a no-op; nothing to drop.
			return nil
		}
		for _, p := range ci.Paths {
			cache.InvalidatePath(p)
		}
		for _, sha := range ci.SHAs {
			cache.InvalidateSHA(sha)
		}
		r.Logger.Printf("cache-invalidate: dropped paths=%d shas=%d", len(ci.Paths), len(ci.SHAs))
	case EventSessionReset:
		var sr SessionResetEvent
		if err := json.Unmarshal(ev.Data, &sr); err != nil {
			r.Logger.Printf("session-reset: malformed payload: %v", err)
			return nil
		}
		resetter, ok := r.Claude.(sessionResetter)
		if !ok {
			// No reset capability (mock client). The
			// warm-runner amnesia problem is specific to the persistent
			// CLI runner, so a client that doesn't expose ResetSession
			// has nothing to tear down — log and move on.
			r.Logger.Printf("session-reset %s: claude client does not support reset; rotation context will not clear", sr.Slug)
			return nil
		}
		if err := resetter.ResetSession(sr.Slug); err != nil {
			r.Logger.Printf("session-reset %s: %v", sr.Slug, err)
			return nil
		}
		r.Logger.Printf("session-reset: tore down warm runner for %s (next turn starts fresh)", sr.Slug)
	case EventPing:
		// Heartbeat — SSE keepalives carry liveness; no action needed.
	default:
		r.Logger.Printf("unknown event type: %q", ev.Type)
	}
	return nil
}

// handleCancelTurn looks up the in-flight cancel func for the
// targeted turn and fires it. Stale cancels (no matching turn —
// either the turn already finished or core's TurnID drifted) are
// no-ops; the runtime can't error on stop-after-done without
// forcing an unhealthy retry loop.
func (r *Runtime) handleCancelTurn(ev Event) {
	var ct CancelTurnEvent
	if err := json.Unmarshal(ev.Data, &ct); err != nil {
		r.Logger.Printf("cancel-turn: malformed payload: %v", err)
		return
	}
	if ct.TurnID == "" {
		return
	}
	r.mu.Lock()
	cancel := r.inflight[ct.TurnID]
	r.mu.Unlock()
	if cancel == nil {
		r.Logger.Printf("cancel-turn: no in-flight turn for %s (already done?)", ct.TurnID)
		return
	}
	r.Logger.Printf("cancel-turn: cancelling turn %s", ct.TurnID)
	cancel()
}

// handleFoldMessage splices core's message into the turn that is
// already running, instead of ending that turn to deliver it.
//
// Every refusal answers with Landed=false rather than staying silent:
// core is holding the message in its pending-deliveries buffer, and a
// silent refusal would leave it buffered until the turn happened to
// end. Answering immediately lets core fall back at once.
func (r *Runtime) handleFoldMessage(ctx context.Context, ev Event) {
	var fm FoldMessageEvent
	if err := json.Unmarshal(ev.Data, &fm); err != nil {
		r.Logger.Printf("fold-message: malformed payload: %v", err)
		return
	}
	if fm.TurnID == "" || fm.DeliveryID == "" {
		r.Logger.Printf("fold-message: missing turn id or delivery id; ignoring")
		return
	}

	r.mu.Lock()
	folder := r.folders[fm.TurnID]
	r.mu.Unlock()
	if folder == nil {
		r.Logger.Printf("fold-message %s: turn %s is not fold-capable (already done, or a transport that cannot fold)", fm.DeliveryID, fm.TurnID)
		r.postFolded(ctx, fm.TurnID, fm.DeliveryID, false)
		return
	}

	foldUUID, err := folder.SendFold(fm.Text)
	if err != nil {
		r.Logger.Printf("fold-message %s: %v", fm.DeliveryID, err)
		r.postFolded(ctx, fm.TurnID, fm.DeliveryID, false)
		return
	}

	// Record the correlation BEFORE the outcome can arrive: the
	// stream's StreamMessageFolded event names the runner's uuid, and
	// core only understands its own delivery id.
	r.mu.Lock()
	if r.foldIDs == nil {
		r.foldIDs = map[string]string{}
	}
	r.foldIDs[foldUUID] = fm.DeliveryID
	r.mu.Unlock()
	r.Logger.Printf("fold-message %s: handed to turn %s as %s", fm.DeliveryID, fm.TurnID, foldUUID)
}

// postFolded reports one fold outcome back to core.
func (r *Runtime) postFolded(ctx context.Context, turnID, deliveryID string, landed bool) {
	if err := r.Client.PostTurnEvent(ctx, turnID, TurnEvent{
		Kind:       TurnEventFolded,
		DeliveryID: deliveryID,
		Landed:     landed,
	}); err != nil {
		r.Logger.Printf("fold-message %s: post folded: %v", deliveryID, err)
	}
}

// resolveFoldDelivery translates a runner fold uuid back to core's
// delivery id, consuming the mapping. An unknown uuid returns "",
// which the caller treats as "not ours to report".
func (r *Runtime) resolveFoldDelivery(foldUUID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.foldIDs[foldUUID]
	delete(r.foldIDs, foldUUID)
	return id
}

// trackFolder registers a turn's stream for folding, when its
// transport supports it. A stream that does not implement
// provider.Folder is simply not registered.
func (r *Runtime) trackFolder(turnID string, stream provider.Stream) {
	folder, ok := stream.(provider.Folder)
	if !ok {
		return
	}
	r.mu.Lock()
	if r.folders == nil {
		r.folders = map[string]provider.Folder{}
	}
	r.folders[turnID] = folder
	r.mu.Unlock()
}

// untrackFolder removes a turn's fold registration once its stream is
// done. Safe to call when no entry exists.
func (r *Runtime) untrackFolder(turnID string) {
	r.mu.Lock()
	delete(r.folders, turnID)
	r.mu.Unlock()
}

// trackInflight installs the per-turn cancel func so handleCancelTurn
// can fire it. Call site is the chat-turn driver, which derives its
// stream context off this cancel.
func (r *Runtime) trackInflight(turnID string, cancel context.CancelFunc) {
	r.mu.Lock()
	if r.inflight == nil {
		r.inflight = map[string]context.CancelFunc{}
	}
	r.inflight[turnID] = cancel
	r.mu.Unlock()
}

// untrackInflight removes the per-turn cancel func once the turn
// has cleaned up its stream. Safe to call when no entry exists.
func (r *Runtime) untrackInflight(turnID string) {
	r.mu.Lock()
	delete(r.inflight, turnID)
	r.mu.Unlock()
}

// handleChatTurn drives one chat turn end-to-end. Decodes the
// event payload, opens a claude stream against the carried request,
// translates StreamEvents → TurnEvents, POSTs each back, and
// reports completion (done) or failure (failed) at the end.
//
// Subprocess-death is the resumption-bug carrier: it surfaces as
// FailedReasonSubprocessDied which core's handler turns into a
// KindRuntimeDisruption row.
func (r *Runtime) handleChatTurn(ctx context.Context, ev Event) error {
	var ct ChatTurnEvent
	if err := json.Unmarshal(ev.Data, &ct); err != nil {
		r.Logger.Printf("chat-turn: malformed payload: %v (raw=%q)", err, string(ev.Data))
		return nil // drop and continue; can't post-back without a TurnID
	}
	if ct.TurnID == "" {
		r.Logger.Printf("chat-turn: missing TurnID (raw=%q)", string(ev.Data))
		return nil
	}
	r.Logger.Printf("chat-turn: turn_id=%s source=%s model=%s", ct.TurnID, ct.Source, ct.Model)

	if r.Claude == nil {
		// Pod started without a provider.Client wired (dev/test).
		// Report failed so the core-side hub doesn't hang.
		_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError, "agentpod runtime: claude client not configured")
		return nil
	}

	var req provider.CompleteRequest
	if len(ct.Request) > 0 {
		if err := json.Unmarshal(ct.Request, &req); err != nil {
			_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError, "decode request: "+err.Error())
			return nil
		}
	}

	// Per-turn cancel func so EventCancelTurn can short-circuit the
	// stream mid-flight. The parent context (from RunEvents) is the
	// pod's lifetime ctx; cancelling streamCtx unblocks the stream
	// without bringing down the events loop.
	streamCtx, cancel := context.WithCancel(ctx)
	r.trackInflight(ct.TurnID, cancel)
	defer func() {
		cancel()
		r.untrackInflight(ct.TurnID)
		r.untrackFolder(ct.TurnID)
	}()

	stream, err := r.Claude.Stream(streamCtx, req)
	if err != nil {
		if cancelled := streamCtx.Err(); cancelled != nil {
			// Cancel raced ahead of stream open. Treat as cancelled.
			_ = r.postFailed(ctx, ct.TurnID, FailedReasonCancelled, "stream open cancelled: "+err.Error())
			return nil
		}
		_ = r.postFailed(ctx, ct.TurnID, classifyErr(err), "stream open: "+err.Error())
		return nil
	}

	// Now that the stream exists, expose it for folding. Registered
	// after the open (there is nothing to fold into before it) and
	// torn down by the defer above.
	r.trackFolder(ct.TurnID, stream)

	out := r.driveStream(ctx, ct.TurnID, stream, func(err error) bool {
		// A deliberate cancel (Stop click) is told apart from a
		// transport failure by two carriers:
		//   - provider.ErrUserCancelled: the persistent runner did a
		//     graceful kill in response to the per-turn ctx cancel.
		//   - context.Canceled on our streamCtx (with the pod's ctx
		//     healthy): the stream saw the cancel before finalizing.
		// Anything else is a failure even mid-Stop: a runner that
		// died for another reason must post subprocess-died, so core
		// writes the runtime-disruption row and the spawn gate
		// resumes the agent.
		return errors.Is(err, provider.ErrUserCancelled) ||
			(errors.Is(err, context.Canceled) && streamCtx.Err() != nil && ctx.Err() == nil)
	})
	if out.err != nil {
		// Whatever the stream captured before the failure rides back so
		// partial usage reaches core — Anthropic billed for every
		// assistant message that landed before the crash / Stop.
		if out.cancelled {
			_ = r.postTurnEvent(ctx, ct.TurnID, failedEvent(FailedReasonCancelled, "cancelled by Stop", out.final))
			return nil
		}
		_ = r.postTurnEvent(ctx, ct.TurnID, failedEvent(classifyErr(out.err), "stream: "+out.err.Error(), out.final))
		return nil
	}

	// A done whose StopReason is provider.StopError is still a done here:
	// core derives the turn-error marker from it, with the
	// TurnEventError texts already posted as the detail.
	doneEvt := usageEvent(TurnEventDone, out.final)
	if out.final != nil {
		doneEvt.StopReason = out.final.StopReason
		// Single-call window occupancy (NOT the accumulated billing sum)
		// — drives the context-fill ring. See TurnEvent.ContextTokens.
		doneEvt.ContextTokens = out.final.ContextTokens
	}
	if err := r.Client.PostTurnEvent(ctx, ct.TurnID, doneEvt); err != nil {
		r.Logger.Printf("chat-turn %s: post done: %v", ct.TurnID, err)
	}
	return nil
}

// streamOutcome is how one model stream ended, as driveStream saw it.
type streamOutcome struct {
	// final is the stream's Final, possibly partial; may be nil.
	final *provider.CompleteResponse
	// err is the stream's Err.
	err error
	// cancelled reports that err is the turn's own cancel (a Stop
	// click / EventCancelTurn) rather than a transport failure.
	cancelled bool
}

// driveStream posts every event of stream to core as a TurnEvent and
// returns how the stream ended; isCancel decides whether the stream's
// error is the turn's own cancel (the caller's rule — a parent turn and
// a subagent run read Stop differently). It is the one mapping from the driver's
// StreamEvents to the pod's wire, shared by a parent chat turn and a
// subagent run so both report deltas, tool calls, results and the typed
// error identically.
//
// Posts use the pod's ctx, not streamCtx: a cancel on the per-turn
// streamCtx means the turn is being torn down, but an in-flight delta
// or tool_result should still land if it can.
func (r *Runtime) driveStream(ctx context.Context, turnID string, stream provider.Stream, isCancel func(error) bool) streamOutcome {
	for sev := range stream.Events() {
		// Fold outcomes carry the runner's uuid; core speaks its own
		// delivery id, so translate before posting. Handled here
		// rather than in streamToTurn because the mapping is per-turn
		// runtime state, not a property of the event.
		if sev.Kind == provider.StreamMessageFolded {
			if deliveryID := r.resolveFoldDelivery(sev.FoldUUID); deliveryID != "" {
				r.postFolded(ctx, turnID, deliveryID, sev.FoldLanded)
			}
			continue
		}
		te := streamToTurn(sev)
		if te == nil {
			continue
		}
		if err := r.Client.PostTurnEvent(ctx, turnID, *te); err != nil {
			r.Logger.Printf("turn %s: post %s: %v", turnID, te.Kind, err)
			// Don't bail: a transient post failure shouldn't kill
			// the turn. The model has already produced the bytes;
			// missing a delta is recoverable, missing the final
			// "done" leaves the chat-hub waiting until something
			// else (next turn or rotation) closes it.
		}
	}
	out := streamOutcome{err: stream.Err(), final: stream.Final()}
	_ = stream.Close()
	if out.err != nil {
		out.cancelled = isCancel(out.err)
	}
	return out
}

// postTurnEvent posts ev, logging a failure.
func (r *Runtime) postTurnEvent(ctx context.Context, turnID string, ev TurnEvent) error {
	err := r.Client.PostTurnEvent(ctx, turnID, ev)
	if err != nil {
		r.Logger.Printf("turn %s: post %s: %v", turnID, ev.Kind, err)
	}
	return err
}

// usageEvent is a TurnEvent of kind carrying final's model, cost and
// token counts — the fields core's usage recording reads off a done or
// a failed event alike. A nil final yields the bare kind.
func usageEvent(kind TurnEventKind, final *provider.CompleteResponse) TurnEvent {
	ev := TurnEvent{Kind: kind}
	if final == nil {
		return ev
	}
	// Model + cost (when the driver reported one) ride back so core's
	// usage.jsonl row carries the driver-resolved values. Core falls
	// back to its own pricing table when CostUSD is zero, and to the
	// publish-time Model when this field is empty.
	ev.Model = final.Model
	ev.CostUSD = final.CostUSD
	ev.InputTokens = final.Usage.InputTokens
	ev.OutputTokens = final.Usage.OutputTokens
	ev.CacheReadTokens = final.Usage.CacheReadTokens
	ev.CacheCreateTokens = final.Usage.CacheCreateTokens
	// Per-model split, present only when the turn was actually served
	// by more than one model. Core writes one usage row per entry so
	// each model's tokens price at its own rate.
	ev.ByModel = turnModelUsage(final.ByModel)
	return ev
}

// failedEvent is a failed TurnEvent carrying whatever usage the stream
// accumulated before it failed (mid-turn crash, Stop click after
// assistant messages already landed). Core's onAgentpodFailed records a
// usage.jsonl row when tokens are non-zero, so partial spend isn't
// silently dropped on the failure path.
func failedEvent(reason, detail string, partial *provider.CompleteResponse) TurnEvent {
	ev := usageEvent(TurnEventFailed, partial)
	ev.FailedReason = reason
	ev.FailedDetail = detail
	return ev
}

// postFailed is a small helper for the failure paths where no
// partial usage is available (stream open failed, payload malformed,
// pre-stream config error). For mid-stream failures that captured
// some usage, post failedEvent so the partial spend lands in
// usage.jsonl rather than vanishing.
func (r *Runtime) postFailed(ctx context.Context, turnID, reason, detail string) error {
	return r.Client.PostTurnEvent(ctx, turnID, TurnEvent{
		Kind:         TurnEventFailed,
		FailedReason: reason,
		FailedDetail: detail,
	})
}

// classifyErr distinguishes subprocess death from other transport
// errors using the shared provider.ErrSubprocessExited sentinel.
// Subprocess-died is the carrier core's onAgentpodFailed maps to a
// KindRuntimeDisruption row.
func classifyErr(err error) string {
	if errors.Is(err, provider.ErrSubprocessExited) {
		return FailedReasonSubprocessDied
	}
	return FailedReasonOtherError
}

// streamToTurn translates one provider.StreamEvent into the matching
// agentpod.TurnEvent. Returns nil when the event has no TurnEvent
// equivalent (StreamEnd is folded into the post-loop "done" event
// instead of emitted standalone).
func streamToTurn(ev provider.StreamEvent) *TurnEvent {
	switch ev.Kind {
	case provider.StreamDelta:
		return &TurnEvent{Kind: TurnEventDelta, Text: ev.Text, Model: ev.Model}
	case provider.StreamThinking:
		return &TurnEvent{Kind: TurnEventThinking, Text: ev.Text}
	case provider.StreamToolUseStart:
		return &TurnEvent{Kind: TurnEventToolUseStart, ToolUseID: ev.ToolUseID, ToolName: ev.ToolName}
	case provider.StreamToolInputDelta:
		return &TurnEvent{Kind: TurnEventToolInputDelta, ToolUseID: ev.ToolUseID, Text: ev.Text}
	case provider.StreamToolUseEnd:
		return &TurnEvent{
			Kind:      TurnEventToolUseEnd,
			ToolUseID: ev.ToolUseID,
			ToolName:  ev.ToolName,
			ToolInput: ev.ToolInput,
		}
	case provider.StreamToolResult:
		return &TurnEvent{
			Kind:              TurnEventToolResult,
			ToolUseID:         ev.ToolUseID,
			ToolResultText:    ev.ToolResultText,
			ToolResultIsError: ev.ToolResultIsError,
		}
	case provider.StreamError:
		return &TurnEvent{Kind: TurnEventError, Text: ev.Text}
	case provider.StreamEnd:
		return nil
	default:
		return nil
	}
}

// streamToTurnFmt is a debug helper kept around for test failure
// messages.
func streamToTurnFmt(te *TurnEvent) string {
	if te == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s(tu=%s name=%s)", te.Kind, te.ToolUseID, te.ToolName)
}

// handleSubagentTurn drives one subagent run: builds a
// provider.SubagentRequest from the SubagentSpec carried inline on the
// chat-turn event, runs it through the driver (Claude.RunSubagent),
// posts its events with the same mapping a parent turn uses, and
// reports done/failed when the run ends.
//
// Cancel cascade: EventCancelTurn for this turn-id (carried as
// ChatTurnEvent.TurnID) cancels the streamCtx the run was started
// with; the driver ends the run, the drain returns, and we POST
// FailedReasonCancelled.
//
// Final text rides on the post-loop "done" TurnEvent's Text field —
// see SubagentSpec doc for the wire compat note.
func (r *Runtime) handleSubagentTurn(ctx context.Context, ev Event) error {
	var ct ChatTurnEvent
	if err := json.Unmarshal(ev.Data, &ct); err != nil {
		r.Logger.Printf("subagent-turn: malformed payload: %v", err)
		return nil
	}
	if ct.TurnID == "" || ct.Subagent == nil {
		r.Logger.Printf("subagent-turn: missing TurnID or Subagent payload")
		return nil
	}
	spec := *ct.Subagent
	if spec.SubagentID == "" {
		_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError, "subagent: missing subagent_id")
		return nil
	}

	r.Logger.Printf("subagent-turn: turn_id=%s subagent=%s parent=%s", ct.TurnID, spec.SubagentID, ct.Slug)

	if r.Claude == nil {
		_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError, "subagent: claude client not configured")
		return nil
	}
	if r.KivaliBinary == "" || r.CoreUDS == "" {
		_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError,
			"subagent: runtime not configured (KivaliBinary/CoreUDS empty)")
		return nil
	}
	if r.ScratchRoot == "" {
		// Scratch holds the run's driver files (its MCP config) and
		// stderr.log: the agent container's own half of the per-agent
		// PVC, which the dev-shell cannot reach. The subagent's /files/
		// tree is the dev-shell's, under the parent's
		// /files/subagents/<id>/.
		_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError,
			"subagent: runtime not configured (ScratchRoot empty)")
		return nil
	}

	streamCtx, cancel := context.WithCancel(ctx)
	r.trackInflight(ct.TurnID, cancel)
	defer func() {
		cancel()
		r.untrackInflight(ct.TurnID)
	}()

	// Subagent-private run dir: the driver keeps the run's config here
	// and the runtime its stderr.log. The subagent's /files/ view lives
	// on the unified mount at /files/subagents/<id>/ — the symlink
	// overlay is built core-side by SubagentService.runOne before this
	// turn fires.
	runDir := filepath.Join(r.ScratchRoot, "subagents", spec.SubagentID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError, "subagent run dir: "+err.Error())
		return nil
	}
	defer func() {
		if err := os.RemoveAll(runDir); err != nil {
			r.Logger.Printf("subagent-turn %s: cleanup %s: %v", ct.TurnID, runDir, err)
		}
	}()
	var stderr io.Writer = os.Stderr
	if f, ferr := os.OpenFile(filepath.Join(runDir, "stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); ferr == nil {
		stderr = io.MultiWriter(os.Stderr, f)
		defer func() { _ = f.Close() }()
	}

	model := spec.Model
	if strings.TrimSpace(model) == "" {
		model = r.SubagentModel
	}
	if strings.TrimSpace(model) == "" && r.Provider != nil {
		model = r.Provider.Defaults().Subagent
	}

	stream, err := r.Claude.RunSubagent(streamCtx, provider.SubagentRequest{
		Model:  model,
		Effort: spec.Effort,
		System: spec.SystemPrompt,
		Prompt: spec.UserPrompt,
		Tools:  spec.Tools,
		MCPServers: []provider.MCPServer{
			subagentMCPServer(r.KivaliBinary, r.CoreUDS, ct.Slug, spec.SubagentID, spec.EffectiveDepth()),
		},
		RunDir:  runDir,
		Stderr:  stderr,
		Purpose: "subagent",
		Agent:   ct.Slug,
	})
	if err != nil {
		if streamCtx.Err() != nil && ctx.Err() == nil {
			_ = r.postFailed(ctx, ct.TurnID, FailedReasonCancelled, "cancelled by Stop")
			return nil
		}
		_ = r.postFailed(ctx, ct.TurnID, FailedReasonOtherError, "subagent start: "+err.Error())
		return nil
	}

	// A subagent's Stop kills its process, which then exits with an
	// error of its own; any failure once the run's context is cancelled
	// (with the pod's healthy) is the cancel.
	out := r.driveStream(ctx, ct.TurnID, stream, func(error) bool {
		return streamCtx.Err() != nil && ctx.Err() == nil
	})
	_ = r.postTurnEvent(ctx, ct.TurnID, subagentTerminalEvent(out, model))
	return nil
}

// subagentTerminalEvent is the done or failed event that closes a
// subagent run. Every case carries the run's text so far (the parent
// renders it, partial or not) and its usage, so a failed or cancelled
// run's spend still lands in usage.jsonl. requested is the model the
// run was asked to use, the label when the driver reported none.
//
// Order matters: a cancel is a cancel whatever else happened, and a
// run the driver closed on a failed model call is a turn error even
// when the provider's process then exited non-zero over it.
func subagentTerminalEvent(out streamOutcome, requested string) TurnEvent {
	var ev TurnEvent
	switch {
	case out.cancelled:
		ev = failedEvent(FailedReasonCancelled, "cancelled by Stop", out.final)
	case out.final != nil && out.final.StopReason == provider.StopError:
		detail := out.final.Error
		if detail == "" && out.err != nil {
			// The driver named no cause; the stream's own error (an
			// exit status, a parse failure) is the next best thing.
			detail = out.err.Error()
		}
		if detail == "" {
			detail = "the model call failed with no detail"
		}
		ev = failedEvent(FailedReasonTurnError, detail, out.final)
	case out.err != nil:
		ev = failedEvent(classifyErr(out.err), "subagent: "+out.err.Error(), out.final)
	default:
		ev = usageEvent(TurnEventDone, out.final)
		if out.final != nil {
			ev.StopReason = out.final.StopReason
		}
	}
	ev.Text = subagentFinalText(out.final)
	if ev.Model == "" {
		// Usage rows are never missing a model for a subagent run: the
		// one we asked for stands in when the driver named none.
		ev.Model = requested
	}
	return ev
}

// subagentFinalText is a subagent's answer: the text of its final
// release, which the parent receives verbatim. That is the text after
// the run's last tool call; a run that ended inside a tool round (cut
// short) has none, and then everything it said is the best partial
// answer there is.
func subagentFinalText(final *provider.CompleteResponse) string {
	if final == nil {
		return ""
	}
	last := -1
	for i, b := range final.Content {
		if b.Type == provider.ContentToolUse {
			last = i
		}
	}
	var tail, all strings.Builder
	for i, b := range final.Content {
		if b.Type != provider.ContentText {
			continue
		}
		all.WriteString(b.Text)
		if i > last {
			tail.WriteString(b.Text)
		}
	}
	if tail.Len() > 0 {
		return tail.String()
	}
	return all.String()
}

// subagentMCPServer is the one MCP server a subagent run loads: this
// binary's `kivali mcp --toolkit subagent`, scoped to the parent and
// the subagent's id, routing list_skills and the other core tools
// through UDS, and run_shell and every file_* call to the dev-shell
// sidecar's daemon (this container mounts no /files), both scoped to
// the same /files/subagents/<id>/ root — file_view and run_shell never
// disagree.
//
// The launch arguments are the trust boundary: the driver writes them
// into a config in the agent container's own half of /scratch, which
// the dev-shell does not mount, so nothing the shell can write may be
// read as the command a provider executes.
func subagentMCPServer(kivaliBin, coreUDS, parent, id string, depth int) provider.MCPServer {
	return provider.MCPServer{
		Name:    provider.KivaliMCPServer,
		Command: kivaliBin,
		Args: []string{
			"mcp",
			"--toolkit", "subagent",
			"--parent", parent,
			"--subagent-id", id,
			"--core-uds", coreUDS,
			// The subagent's /files/ root in the dev-shell container. The
			// mount (manifest.go subPath agents/<parent>/<storage>)
			// exposes the parent's whole tree at /files/; the
			// SubagentService overlay places the subagent-scoped subdir
			// at /files/subagents/<id>/.
			"--subagent-files-root", "/files/subagents/" + id,
			// Depth decides whether this subagent's MCP child exposes
			// `subagent` at all. It arrives as an argument the RUNTIME
			// sets, so it is as trustworthy as the slug: the model
			// inside the subagent can call tools, but it cannot reach
			// the arguments its own MCP subprocess was started with.
			"--depth", strconv.Itoa(depth),
		},
	}
}

// turnModelUsage converts the transport's per-model split into the wire
// shape. Returns nil for a single-model turn, where the flat Model +
// token fields on TurnEvent already carry the whole story.
func turnModelUsage(split []provider.ModelUsage) []TurnModelUsage {
	if len(split) == 0 {
		return nil
	}
	out := make([]TurnModelUsage, 0, len(split))
	for _, mu := range split {
		out = append(out, TurnModelUsage{
			Model:             mu.Model,
			InputTokens:       mu.Usage.InputTokens,
			OutputTokens:      mu.Usage.OutputTokens,
			CacheReadTokens:   mu.Usage.CacheReadTokens,
			CacheCreateTokens: mu.Usage.CacheCreateTokens,
		})
	}
	return out
}
