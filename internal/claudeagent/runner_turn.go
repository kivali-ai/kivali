package claudeagent

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
)

// runnerTurn implements provider.Stream against one turn of a persistent
// runner. The runner's dispatcher feeds events here via handleEvent;
// when the CLI emits "result", we emit StreamEnd and close events.
//
// runnerTurn does NOT own the subprocess. Close() unhooks this turn's
// state but leaves the runner alive for the next turn.
type runnerTurn struct {
	clk    clock.Clock
	events chan provider.StreamEvent
	done   chan struct{}
	once   sync.Once

	// finalized closes when finalize() runs, regardless of whether
	// the turn ended naturally (CLI emitted "result") or was failed
	// by the supervisor (subprocess died, user-cancel kill). Used by
	// the runner's per-turn interrupt watcher to distinguish "natural
	// completion, exit watcher" from "external cancel, fire kill".
	finalized chan struct{}

	errMu sync.Mutex
	err   error

	fmu        sync.Mutex
	id         string
	model      string
	stopReason string
	// errText is the CLI's account of the failed call that ended the
	// turn (Final.Error); pendingErr is sentinel-model text held until
	// the turn's outcome says what it was. See settleResult.
	errText    string
	pendingErr string
	blocks     []provider.ContentBlock
	// usage is the turn's token accounting — each API call counted
	// once, superseded by each result frame's final totals, bucketed by
	// answering model. See turnUsage and subprocStream.usage. The
	// per-model share of a frame comes from the runner's gauge, which
	// outlives the turn because the CLI's reading is cumulative across
	// the process; see runner.usageGauge.
	usage turnUsage
	// lastReqContext is the window occupancy (input + cache_read +
	// cache_create) of the most recent assistant message — latest-wins,
	// NOT accumulated like `usage`. Surfaces on
	// CompleteResponse.ContextTokens. See subprocStream for the rationale.
	lastReqContext int
	costUSD        float64

	req      provider.CompleteRequest
	recorder provider.UsageRecorder

	sessions SessionStore
	slug     string
	resumeID string

	// closed flips true after we've emitted StreamEnd + closed the
	// events chan. Guarded by fmu.
	closed bool

	// owner is the runner driving this turn. Set by RunTurn before
	// the turn goes live; nil in unit tests that build a turn
	// directly, which is why SendFold checks it. Immutable once set.
	owner *runner
}

func newRunnerTurn(req provider.CompleteRequest, recorder provider.UsageRecorder, sessions SessionStore, slug, resumeID string, clk clock.Clock) *runnerTurn {
	if clk == nil {
		clk = clock.New()
	}
	return &runnerTurn{
		clk:       clk,
		events:    make(chan provider.StreamEvent, 16),
		done:      make(chan struct{}),
		finalized: make(chan struct{}),
		req:       req,
		recorder:  recorder,
		sessions:  sessions,
		slug:      slug,
		resumeID:  resumeID,
	}
}

func (t *runnerTurn) Events() <-chan provider.StreamEvent { return t.events }
func (t *runnerTurn) Err() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.err
}

func (t *runnerTurn) setErr(err error) {
	t.errMu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.errMu.Unlock()
}

// fail closes the turn with err. Called by the runner when the
// subprocess dies mid-turn.
func (t *runnerTurn) fail(err error) {
	t.setErr(err)
	// Sentinel text no result frame resolved: the failure makes it the
	// turn's error detail. Recorded on Final only — fail can run off
	// the dispatcher goroutine, so it emits nothing but the end.
	t.fmu.Lock()
	if !t.closed {
		out := settleExit(t.stopReason, t.errText, t.pendingErr, err, "runner["+t.slug+"]")
		t.stopReason, t.errText, t.pendingErr = out.stopReason, out.detail, ""
	}
	t.fmu.Unlock()
	t.finalize()
}

// Close unhooks this turn. The subprocess lives on for subsequent
// turns. Idempotent.
func (t *runnerTurn) Close() error {
	t.once.Do(func() {
		close(t.done)
	})
	return nil
}

func (t *runnerTurn) Final() *provider.CompleteResponse {
	t.fmu.Lock()
	defer t.fmu.Unlock()
	content := make([]provider.ContentBlock, 0, len(t.blocks))
	for _, b := range t.blocks {
		if b.Type == provider.ContentText && b.Text == "" {
			continue
		}
		content = append(content, b)
	}
	return &provider.CompleteResponse{
		ID:            t.id,
		Model:         t.model,
		StopReason:    t.stopReason,
		Error:         t.errText,
		Content:       content,
		Usage:         t.usage.Total(),
		ByModel:       t.usage.Split(),
		CostUSD:       t.costUSD,
		ContextTokens: t.lastReqContext,
	}
}

// handleEvent processes one parsed stream-json event from the runner's
// dispatcher. The event-handling logic mirrors subprocStream.handleEvent
// (per-call path) — kept as a parallel implementation so the per-call
// path is not touched by this slice.
func (t *runnerTurn) handleEvent(ev *streamJSONEvent) {
	switch ev.Type {
	case "system":
		var sys struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(ev.Message, &sys)
		if sys.Model != "" {
			t.fmu.Lock()
			t.model = sys.Model
			t.fmu.Unlock()
		}
		// session_id capture is handled at the runner level (one
		// persistent process = one session id for its lifetime). We
		// don't re-capture per turn.
	case "assistant":
		t.handleAssistantMessage(ev.Message)
	case "user":
		t.handleUserMessage(ev.Message)
	case "result":
		// Reached only by callers with no runner behind them (unit
		// tests driving a turn directly): the runner routes result
		// frames itself, because the per-model share of a frame
		// comes from its process-scoped gauge. With no gauge the
		// reducer rebuilds the split from the calls it saw.
		t.recordResult(ev, nil)
		// Persistent mode: turn is done, runner stays alive. Drain
		// the per-turn state and close events so the caller's
		// for-range exits.
		t.finalize()
	}
}

// recordResult takes the turn-level totals off one result frame
// WITHOUT ending the turn.
//
// Split out of handleEvent because a turn can legitimately span more
// than one result frame: when a folded message misses, the CLI answers
// it in a continuation the runner absorbs into the same turn (see
// runner.absorbMissedFolds).
//
// total_cost_usd is the CLI's session-cumulative gauge, not the price
// of one frame: measured against CLI 2.1.278, the second frame of a
// session carries the first frame's spend inside its value. Latest
// wins, therefore — the last frame already contains every invocation
// before it, and adding the frames would bill the first one twice.
// settings.go prices the fleet from per-call tokens for the same
// reason and keeps this column as a diagnostic.
//
// The frame's `usage` is different: it is the frame's OWN total (the
// second frame of a session does not contain the first), so frames
// add, via recordUsage.
func (t *runnerTurn) recordResult(ev *streamJSONEvent, byModel []provider.ModelUsage) {
	t.recordUsage(ev, byModel)
	t.fmu.Lock()
	if ev.TotalCostUSD > 0 {
		t.costUSD = ev.TotalCostUSD
	}
	out := settleResult(t.stopReason, t.errText, t.pendingErr, ev, "runner["+t.slug+"]")
	t.stopReason, t.errText, t.pendingErr = out.stopReason, out.detail, ""
	t.fmu.Unlock()
	// Called on the dispatcher goroutine, before finalize closes events.
	if out.emit != "" {
		t.emit(provider.StreamEvent{Kind: provider.StreamError, Text: out.emit})
	}
}

// recordUsage folds one result frame's token total into the turn: the
// final numbers for every call streamed since the previous frame,
// replacing their start-of-message snapshots. byModel is the frame's
// per-model share as the runner's gauge recovered it, or nil when the
// gauge had no baseline. A frame with no usable total leaves the
// snapshots in place. Kept apart from recordResult so an interrupted
// turn can bank its tokens without adopting the frame's stop reason.
func (t *runnerTurn) recordUsage(ev *streamJSONEvent, byModel []provider.ModelUsage) {
	total, ok := ev.resultTotal()
	if !ok {
		return
	}
	t.fmu.Lock()
	t.usage.Result(total, byModel)
	t.fmu.Unlock()
}

// finalize records usage, emits StreamEnd, closes events. Idempotent.
func (t *runnerTurn) finalize() {
	t.fmu.Lock()
	if t.closed {
		t.fmu.Unlock()
		return
	}
	t.closed = true
	close(t.finalized)
	if t.pendingErr != "" {
		// An interrupted turn banks usage without a result's outcome.
		log.Printf("claudeagent: runner[%s]: sentinel-model text on a turn that ended without an outcome, dropped: %q", t.slug, t.pendingErr)
		t.pendingErr = ""
	}
	// One row per answering model on a mixed turn, one row total
	// otherwise. See usageEvents.
	rows := usageEvents(provider.UsageEvent{
		TS:      t.clk.Now().UTC(),
		Model:   t.model,
		Purpose: t.req.Purpose,
		Agent:   t.req.Agent,
	}, t.usage.Total(), t.usage.Split(), t.costUSD)
	t.fmu.Unlock()
	if t.recorder != nil {
		for _, row := range rows {
			_ = t.recorder(row)
		}
	}
	// Emit StreamEnd, then close the events chan so caller for-range
	// exits. The timer guards against a buggy caller that abandoned
	// the stream without draining or closing — in persistent mode
	// this runs in the runner's dispatcher goroutine, so blocking
	// here would wedge ALL future turns on this runner. If the
	// caller is gone we drop the StreamEnd and proceed.
	timer := t.clk.NewTimer(finalizeEmitTimeout)
	defer timer.Stop()
	select {
	case t.events <- provider.StreamEvent{Kind: provider.StreamEnd}:
	case <-t.done:
	case <-timer.C():
	}
	close(t.events)
}

// finalizeEmitTimeout is how long finalize will wait to deliver
// StreamEnd to a possibly-abandoned caller before giving up. Generous
// because real callers almost always drain promptly; this only kicks
// in for buggy/leaked streams. Tests inject a fakeClock if they need
// to verify the timeout fires.
const finalizeEmitTimeout = 5 * time.Second

func (t *runnerTurn) handleAssistantMessage(raw json.RawMessage) {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		t.setErr(fmt.Errorf("claudeagent: assistant message: %w", err))
		return
	}
	// A sentinel ID must not win this latest-wins assignment — see
	// isSentinelModel. t.model is the turn's answering model on
	// CompleteResponse and the fallback every usage row prices against;
	// letting "<synthetic>" land here books the turn at $0.
	if m.Model != "" && !isSentinelModel(m.Model) {
		t.fmu.Lock()
		t.model = m.Model
		t.fmu.Unlock()
	}
	// One API call per message id, one start-of-message snapshot per
	// content block: the reducer counts the call once and lets the
	// result frame's final totals replace the snapshot. See
	// subprocStream.handleAssistantMessage.
	if snapshot := m.Usage.tokens(); !snapshot.IsZero() {
		t.fmu.Lock()
		// Bucket this call's tokens under the model that served IT —
		// see subprocStream.handleAssistantMessage. attributeModel
		// keeps a synthetic close-out message's real, billed tokens on
		// the model that actually answered.
		t.usage.Call(m.ID, attributeModel(m.Model, t.model, t.req.Model), snapshot)
		// Window occupancy is per-call (latest-wins), not cumulative —
		// see subprocStream.handleAssistantMessage.
		t.lastReqContext = snapshot.InputTokens + snapshot.CacheReadTokens + snapshot.CacheCreateTokens
		t.fmu.Unlock()
	}
	// A sentinel-model message is the CLI's close-out text, not the
	// model speaking: hold it until the result frame says whether the
	// turn failed. See settleResult.
	if isSentinelModel(m.Model) {
		t.fmu.Lock()
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				t.pendingErr = joinText(t.pendingErr, c.Text)
			}
		}
		t.fmu.Unlock()
		return
	}
	thinkingEmitted := false
	for _, c := range m.Content {
		switch c.Type {
		case "thinking":
			// One thinking step-marker per assistant message (live-only,
			// not persisted). Emitted even when empty — Opus encrypts its
			// reasoning, so the marker is the only progress signal. See
			// subprocStream.handleAssistantMessage.
			if thinkingEmitted {
				continue
			}
			thinkingEmitted = true
			t.emit(provider.StreamEvent{Kind: provider.StreamThinking, Text: c.Thinking})
		case "text":
			if c.Text == "" {
				continue
			}
			t.fmu.Lock()
			t.blocks = append(t.blocks, provider.ContentBlock{Type: provider.ContentText, Text: c.Text})
			t.fmu.Unlock()
			// Tag the delta with the model that produced THIS message.
			// Consumers label the bubble from it, and a turn can switch
			// models between calls, so the per-call id is the only one
			// that labels the text correctly.
			t.emit(provider.StreamEvent{Kind: provider.StreamDelta, Text: c.Text, Model: m.Model})
		case "tool_use":
			id := c.ID
			name := bareToolName(c.Name)
			t.fmu.Lock()
			t.blocks = append(t.blocks, provider.ContentBlock{
				Type:      provider.ContentToolUse,
				ToolUseID: id,
				ToolName:  name,
				ToolInput: c.Input,
			})
			t.fmu.Unlock()
			t.emit(provider.StreamEvent{Kind: provider.StreamToolUseStart, ToolUseID: id, ToolName: name})
			// Tool inputs arrive WHOLE
			// at tool_use_end (Claude Code's stream-json never emits
			// tool_input_delta). The full Input
			// JSON travels with this single event; downstream chip
			// rendering depends on it being non-empty here.
			t.emit(provider.StreamEvent{Kind: provider.StreamToolUseEnd, ToolUseID: id, ToolName: name, ToolInput: c.Input})
		}
	}
}

func (t *runnerTurn) handleUserMessage(raw json.RawMessage) {
	var m struct {
		Content []messageContent `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.setErr(fmt.Errorf("claudeagent: user message: %w", err))
		return
	}
	for _, c := range m.Content {
		if c.Type != "tool_result" {
			continue
		}
		text := flattenToolResultContent(c.ContentRaw)
		t.emit(provider.StreamEvent{
			Kind:              provider.StreamToolResult,
			ToolUseID:         c.ToolUseID,
			ToolResultText:    text,
			ToolResultIsError: c.IsError,
		})
	}
}

// SendFold implements provider.Folder: it splices text into this
// already-running turn instead of ending it. See runner.sendFold for
// the mechanism and the refusal cases.
func (t *runnerTurn) SendFold(text string) (string, error) {
	if t.owner == nil {
		return "", errFoldNoTurn
	}
	return t.owner.sendFold(t, text)
}

// emitFold reports the fate of one folded message to the caller.
// Called only from the runner's dispatcher goroutine, and always
// before finalize() closes the events channel.
func (t *runnerTurn) emitFold(id string, landed bool) {
	t.emit(provider.StreamEvent{
		Kind:       provider.StreamMessageFolded,
		FoldUUID:   id,
		FoldLanded: landed,
	})
}

func (t *runnerTurn) emit(ev provider.StreamEvent) {
	select {
	case t.events <- ev:
	case <-t.done:
	}
}
