package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Protocol capabilities the fake advertises on init. The runner only
// folds a message into a running turn when msg_lifecycle_v1 is present,
// and only asks for cancel_queued when interrupt_cancel_queued_v1 is.
var capabilities = []string{"interrupt_receipt_v1", "interrupt_cancel_queued_v1", "msg_lifecycle_v1"}

// inbound is one line the runner wrote on stdin: a user message
// (optionally uuid-stamped) or a control_request.
type inbound struct {
	Type      string          `json:"type"`
	UUID      string          `json:"uuid,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Request   struct {
		Subtype      string `json:"subtype"`
		CancelQueued bool   `json:"cancel_queued"`
	} `json:"request"`
}

// blocks returns the message's text blocks in order. The runner sends a
// fold as a plain string and a turn prompt as an array of content
// blocks: one per chat row delivered since the last turn, wake notes
// and markers included.
func (in inbound) blocks() []string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(in.Message, &m); err != nil {
		return nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []string{s}
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(m.Content, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// text is the whole message as one string.
func (in inbound) text() string { return strings.Join(in.blocks(), "\n") }

// newest is the latest thing a person said in the message: the last
// block that is not runtime plumbing (the wake note, a bracketed marker
// such as "[User pressed Stop.]"). A fresh CLI process is handed the
// chat since its last session, so older messages ride along in front.
// A message that is nothing but a scenario tag ("[fake:slow]") is the
// person's, not a marker.
func (in inbound) newest() string {
	b := in.blocks()
	for i := len(b) - 1; i >= 0; i-- {
		t := strings.TrimSpace(b[i])
		marker := strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") && !scenarioTag.MatchString(t)
		if strings.HasPrefix(t, "Update from the runtime at this wake") || marker {
			continue
		}
		return t
	}
	return ""
}

// usage is one API call's token counts, in the snake_case wire shape.
type usage struct {
	Input       int `json:"input_tokens"`
	Output      int `json:"output_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
}

func (u usage) add(v usage) usage {
	return usage{u.Input + v.Input, u.Output + v.Output, u.CacheRead + v.CacheRead, u.CacheCreate + v.CacheCreate}
}

// callUsage is the fixed cost of every scripted API call. Two calls per
// ordinary turn (before and after the tool round).
var callUsage = usage{Input: 1200, Output: 180, CacheRead: 24000, CacheCreate: 800}

// costPerCall is what the fake bills per call in total_cost_usd, which
// the CLI reports session-cumulative.
const costPerCall = 0.0125

// engine is one fake CLI process: a read loop on stdin feeding a
// channel, and the turn loop that plays scenarios against it.
type engine struct {
	cfg config

	outMu sync.Mutex
	out   *bufio.Writer

	in  <-chan inbound
	eof bool

	sessionID string
	turn      int
	calls     int // API calls made this process: drives ids and the cumulative gauge

	// cumulative is the session gauge the CLI reports on every result
	// frame (modelUsage + total_cost_usd).
	cumulative usage

	// queued holds uuid-stamped user messages that arrived while a
	// turn was running and have not been consumed yet. A tool seam
	// consumes them (the fold lands); if the turn ends first they run
	// as a continuation turn, exactly as the real CLI does.
	queued []inbound

	// lastText is the turn's latest text block. The CLI's result frame
	// carries the final assistant message as `result`, which is what a
	// subagent run hands back to its parent.
	lastText string

	// newTimer is the pause clock; a test seam.
	newTimer func(time.Duration) (<-chan time.Time, func())

	// trace, when set, gets one line per frame in each direction. The
	// runner copies the CLI's stderr into the agent runtime's log, so
	// this is what a failed e2e test's server.log shows of the wire.
	trace io.Writer
}

func newEngine(cfg config, stdin io.Reader, out *bufio.Writer, trace io.Writer) *engine {
	ch := make(chan inbound, 64)
	e := &engine{
		cfg:       cfg,
		out:       out,
		in:        ch,
		sessionID: sessionIDFor(cfg),
		trace:     trace,
		newTimer: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		},
	}
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
		for sc.Scan() {
			var in inbound
			if err := json.Unmarshal(sc.Bytes(), &in); err != nil {
				continue
			}
			e.tracef("<- %s uuid=%s %s", in.Type, in.UUID, in.Request.Subtype)
			ch <- in
		}
		e.tracef("<- EOF")
	}()
	return e
}

func (e *engine) tracef(format string, args ...any) {
	if e.trace == nil {
		return
	}
	e.outMu.Lock()
	defer e.outMu.Unlock()
	_, _ = fmt.Fprintf(e.trace, "fake-claude[%s]: "+format+"\n", append([]any{e.sessionID[:8]}, args...)...)
}

// sessionIDFor is stable per process identity: the resumed id when
// there is one, otherwise a hash of what the runner configured.
func sessionIDFor(cfg config) string {
	if cfg.resumeID != "" {
		return cfg.resumeID
	}
	sum := sha256.Sum256([]byte(cfg.mcpConfig + "\x00" + cfg.systemPrompt + "\x00" + cfg.model))
	h := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func (e *engine) emit(v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	e.tracef("-> %v %v %v", v["type"], v["subtype"], v["state"])
	e.outMu.Lock()
	defer e.outMu.Unlock()
	_, _ = e.out.Write(b)
	_ = e.out.WriteByte('\n')
	_ = e.out.Flush()
}

// run is the process lifetime: one turn per user message until stdin
// closes and nothing is left queued.
func (e *engine) run() {
	for {
		msg, ok := e.nextPrompt()
		if !ok {
			return
		}
		e.runTurn(msg)
	}
}

// nextPrompt returns the message that starts the next turn: a message
// still queued from the last turn first (the continuation), else the
// next user line. A control_request while idle is acknowledged with an
// empty receipt, as the CLI does when there is nothing to interrupt.
func (e *engine) nextPrompt() (inbound, bool) {
	if len(e.queued) > 0 {
		msg := e.queued[0]
		e.queued = e.queued[1:]
		return msg, true
	}
	for {
		if e.eof {
			return inbound{}, false
		}
		in, ok := <-e.in
		if !ok {
			e.eof = true
			continue
		}
		switch in.Type {
		case "user":
			return in, true
		case "control_request":
			e.ack(in, nil, nil)
		}
	}
}

// ack answers a control_request with a success receipt.
func (e *engine) ack(req inbound, cancelled, stillQueued []string) {
	if cancelled == nil {
		cancelled = []string{}
	}
	if stillQueued == nil {
		stillQueued = []string{}
	}
	e.emit(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": req.RequestID,
			"response":   map[string]any{"cancelled": cancelled, "still_queued": stillQueued},
		},
	})
}

func (e *engine) lifecycle(uuid, state string) {
	if uuid == "" {
		return
	}
	e.emit(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": state})
}

// turnState is what one running turn has accumulated.
type turnState struct {
	ledger      []string // user_message_uuids, in consumption order
	calls       int      // API calls this turn
	interrupted bool
}

// accept takes one inbound line that arrived mid-turn. It reports
// whether it was an interrupt, which the caller must honour by ending
// the turn.
func (e *engine) accept(ts *turnState, in inbound) bool {
	switch in.Type {
	case "user":
		// Queued, not consumed: it lands at the next tool seam, or
		// runs as its own continuation turn if this one ends first.
		e.queued = append(e.queued, in)
		e.lifecycle(in.UUID, "queued")
	case "control_request":
		if in.Request.Subtype != "interrupt" {
			e.ack(in, nil, nil)
			return false
		}
		var cancelled, still []string
		for _, q := range e.queued {
			if in.Request.CancelQueued {
				cancelled = append(cancelled, q.UUID)
			} else {
				still = append(still, q.UUID)
			}
		}
		if in.Request.CancelQueued {
			// Handed back: the runner re-delivers these itself.
			e.queued = nil
		}
		e.ack(in, cancelled, still)
		ts.interrupted = true
		return true
	}
	return false
}

// drain takes whatever is already waiting on stdin without blocking.
// Reports whether an interrupt was among it.
func (e *engine) drain(ts *turnState) bool {
	for {
		select {
		case in, ok := <-e.in:
			if !ok {
				e.eof = true
				e.in = nil
				return false
			}
			if e.accept(ts, in) {
				return true
			}
		default:
			return false
		}
	}
}

// pause waits d, or less if stdin brings an interrupt (reported true)
// or, when untilMessage, a user message (which is queued).
func (e *engine) pause(ts *turnState, d time.Duration, untilMessage bool) bool {
	if d <= 0 {
		return e.drain(ts)
	}
	c, stop := e.newTimer(d)
	defer stop()
	for {
		select {
		case <-c:
			return false
		case in, ok := <-e.in:
			if !ok {
				e.eof = true
				e.in = nil // a nil channel blocks: wait out the timer
				continue
			}
			if e.accept(ts, in) {
				return true
			}
			if untilMessage && in.Type == "user" {
				return false
			}
		}
	}
}

// seam is a tool round's boundary: every queued message is consumed
// into the running turn here, which is what a fold landing means.
func (e *engine) seam(ts *turnState) []inbound {
	if e.drain(ts) {
		return nil
	}
	folded := e.queued
	e.queued = nil
	for _, m := range folded {
		e.lifecycle(m.UUID, "started")
		if m.UUID != "" {
			ts.ledger = append(ts.ledger, m.UUID)
		}
	}
	return folded
}

// ---- frames ----

func (e *engine) initFrame() {
	e.emit(map[string]any{
		"type":                "system",
		"subtype":             "init",
		"session_id":          e.sessionID,
		"model":               e.cfg.model,
		"message":             map[string]any{"model": e.cfg.model},
		"cwd":                 "/scratch",
		"tools":               []string{"WebFetch", "WebSearch"},
		"permissionMode":      "default",
		"capabilities":        capabilities,
		"claude_code_version": "2.1.281",
	})
}

// newCall starts an API call and returns its message id. Every content
// block of one call repeats the same start-of-message usage snapshot.
func (e *engine) newCall(ts *turnState) string {
	e.calls++
	ts.calls++
	return fmt.Sprintf("msg_fake_%03d_%02d", e.turn, ts.calls)
}

func (e *engine) assistant(id, model string, block map[string]any) {
	snap := callUsage
	snap.Output = 1
	e.emit(map[string]any{
		"type":       "assistant",
		"session_id": e.sessionID,
		"message": map[string]any{
			"id":      id,
			"type":    "message",
			"role":    "assistant",
			"model":   model,
			"content": []map[string]any{block},
			"usage":   snap,
		},
		"parent_tool_use_id": nil,
	})
}

func (e *engine) thinking(id, text string) {
	e.assistant(id, e.cfg.model, map[string]any{"type": "thinking", "thinking": text, "signature": "fake"})
}

func (e *engine) text(id, text string) {
	e.lastText = text
	e.assistant(id, e.cfg.model, map[string]any{"type": "text", "text": text})
}

func (e *engine) toolUse(id, toolID, name string, input map[string]any) {
	e.assistant(id, e.cfg.model, map[string]any{"type": "tool_use", "id": toolID, "name": name, "input": input})
}

func (e *engine) toolResult(toolID, text string, isError bool) {
	e.emit(map[string]any{
		"type":       "user",
		"session_id": e.sessionID,
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": toolID,
				"content":     []map[string]any{{"type": "text", "text": text}},
				"is_error":    isError,
			}},
		},
		"parent_tool_use_id": nil,
	})
}

// result closes the turn. errored marks a failed model call: is_error
// with no result text, which the runner reads as stop reason "error".
func (e *engine) result(ts *turnState, errored bool) {
	turnUsage := usage{}
	for i := 0; i < ts.calls; i++ {
		turnUsage = turnUsage.add(callUsage)
	}
	e.cumulative = e.cumulative.add(turnUsage)
	frame := map[string]any{
		"type":           "result",
		"subtype":        "success",
		"session_id":     e.sessionID,
		"duration_ms":    1000 * ts.calls,
		"num_turns":      ts.calls,
		"total_cost_usd": costPerCall * float64(e.calls),
		"usage":          turnUsage,
		"modelUsage": map[string]any{
			e.cfg.model: map[string]any{
				"inputTokens":              e.cumulative.Input,
				"outputTokens":             e.cumulative.Output,
				"cacheReadInputTokens":     e.cumulative.CacheRead,
				"cacheCreationInputTokens": e.cumulative.CacheCreate,
				"costUSD":                  costPerCall * float64(e.calls),
			},
		},
		"user_message_uuids": nonNil(ts.ledger),
	}
	switch {
	case ts.interrupted:
		frame["subtype"] = "error_during_execution"
		frame["is_error"] = true
	case errored:
		frame["is_error"] = true
	default:
		frame["result"] = e.lastText
		if e.lastText == "" {
			frame["result"] = "ok"
		}
	}
	e.emit(frame)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return append([]string(nil), s...)
}
