package claudeagent

import (
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/provider"
)

// streamJSONEvent is the wire shape of one line output by
// `claude -p --output-format stream-json`. The CLI emits one JSON
// object per line; we parse the discriminated union by Type first,
// then decode the message body based on the concrete message kind.
//
// The fields below reflect the current (as-shipped) Claude Code
// stream-json format. The CLI is the source of truth; if it changes,
// this struct is the single place to adjust.
type streamJSONEvent struct {
	Type    string          `json:"type"` // "system", "assistant", "user", "result", "control_response"
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	// Response carries the payload of a "control_response" event —
	// the CLI's reply to a control_request we wrote on stdin (see
	// runner.sendInterrupt). Decoded into controlResponse.
	Response json.RawMessage `json:"response,omitempty"`
	// Totals present on the terminal "result" event.
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	DurationMS   int     `json:"duration_ms,omitempty"`
	NumTurns     int     `json:"num_turns,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`
	Result       string  `json:"result,omitempty"`
	SessionID    string  `json:"session_id,omitempty"`

	// Usage is the result frame's own token total: the sum over every
	// API call the frame closes, with the FINAL output count. This is
	// the only place the stream reports output tokens correctly — the
	// usage on each assistant event is the snapshot taken when that
	// message started (see messageUsage). Nil when the frame carried
	// none, which test fakes and errored frames may do.
	Usage *messageUsage `json:"usage,omitempty"`

	// ModelUsage is the CLI's per-model gauge on the result frame,
	// keyed by model id. Unlike Usage it is cumulative over the whole
	// session, including turns run by earlier processes when the
	// session was resumed — see usageGauge for how a turn's own
	// share is recovered from it.
	ModelUsage map[string]resultModelUsage `json:"modelUsage,omitempty"`

	// UserMessageUUIDs is present on the terminal "result" event: the
	// client uuid of every user message the turn consumed, in
	// consumption order — the prompt that started it, then anything
	// folded into it between tool rounds. This is the ledger that
	// tells a fold which landed from one which missed, and it is why
	// the fold needs no watchdog timer: the result event is itself
	// the terminal answer.
	UserMessageUUIDs []string `json:"user_message_uuids,omitempty"`

	// Capabilities is present on the "system"/"init" frame and names
	// the protocol features this CLI build supports. It exists so
	// consumers feature-detect rather than version-sniff, which is
	// exactly what capabilityLifecycle gates on.
	Capabilities []string `json:"capabilities,omitempty"`

	// command_lifecycle fields. CommandUUID echoes the uuid we put on
	// a user message we wrote to stdin; State tracks its progress
	// through the CLI's command queue. The CLI emits NO lifecycle
	// events at all for a message sent without a uuid — which is why
	// writeUserMessageLine always stamps one.
	CommandUUID string `json:"command_uuid,omitempty"`
	State       string `json:"state,omitempty"`
}

// CLI protocol capabilities we care about, as advertised on the
// system/init frame's capabilities array (CLI 2.1.273 advertises
// both). Absence is not an error: it just means this build cannot
// fold, and the caller falls back to ending the turn.
const (
	// capabilityLifecycle — the CLI emits command_lifecycle frames
	// for uuid-stamped inbound messages. Without it we cannot observe
	// the fold seam, so we do not attempt a fold.
	capabilityLifecycle = "msg_lifecycle_v1"
	// capabilityCancelQueued — the interrupt control_request honours
	// cancel_queued:true, cancelling queued commands alongside the
	// abort and listing them on the response. Without it, an
	// interrupt that races a fold could leave the CLI holding a
	// message we also re-deliver ourselves.
	capabilityCancelQueued = "interrupt_cancel_queued_v1"
)

// command_lifecycle states we act on. The CLI defines more
// ("discarded", "refused"); we treat every non-started terminal state
// the same way — the fold did not land — so only these are named.
const (
	// lifecycleStarted means the command drained into a turn. When it
	// arrives BEFORE that turn's result event, the fold landed.
	lifecycleStarted = "started"
	// lifecycleQueued means the CLI accepted the message into its
	// queue. Useful for logging only: it says nothing about which
	// turn will consume it.
	lifecycleQueued = "queued"
)

// controlResponse is the payload of a "control_response" stream-json
// event. The CLI answers every control_request we write on stdin with
// one of these, correlated by RequestID.
//
// Measured shape (CLI 2.1.273) for an accepted interrupt:
//
//	{"type":"control_response","response":{"subtype":"success",
//	 "request_id":"req_1","response":{"still_queued":[]}}}
//
// Subtype is "success" on acceptance; anything else means the CLI
// declined or could not parse the request, and the caller escalates
// to the hard kill rather than waiting.
type controlResponse struct {
	Subtype   string `json:"subtype,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Error     string `json:"error,omitempty"`

	// Response is the interrupt receipt. StillQueued names messages
	// the CLI is still holding after the abort; Cancelled names the
	// ones it dropped because we asked (cancel_queued). Together they
	// are how we reconcile an interrupt that raced a fold: a message
	// listed as cancelled is ours to re-deliver, and one still queued
	// is not.
	Response struct {
		StillQueued []string `json:"still_queued,omitempty"`
		Cancelled   []string `json:"cancelled,omitempty"`
	} `json:"response,omitempty"`
}

// message mirrors the "message" object Claude Code wraps around its
// assistant/user content. Only the fields we care about are decoded.
type message struct {
	ID      string           `json:"id,omitempty"`
	Role    string           `json:"role,omitempty"`
	Model   string           `json:"model,omitempty"`
	Content []messageContent `json:"content,omitempty"`
	Usage   messageUsage     `json:"usage,omitempty"`
}

// messageContent is the discriminated union used inside
// message.content[]. We look at Type first, then peel off the
// relevant fields.
type messageContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Thinking carries the reasoning text of a "thinking" content block
	// (extended thinking). Surfaced live to the UI as a progress cue;
	// not persisted.
	Thinking string `json:"thinking,omitempty"`
	// tool_use fields
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result fields
	ToolUseID string `json:"tool_use_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// Content of a tool_result is usually an array of text blocks;
	// decode lazily when needed.
	ContentRaw json.RawMessage `json:"content,omitempty"`
}

// messageUsage is the usage block Claude Code puts on each assistant
// event and on the result frame. On an assistant event it is the
// snapshot the API took when the message STARTED: input and cache
// counts are final, output_tokens is the few tokens written so far
// (measured 1–8 against CLI 2.1.281 for messages that finished at
// 40–130). Every content block of the same message repeats the same
// snapshot. On the result frame it is the turn's real total. On a
// signed-in account some fields may be zero — that's expected; we
// report what's there and leave the rest zero.
type messageUsage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

func (u messageUsage) tokens() provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CacheReadTokens:   u.CacheReadInputTokens,
		CacheCreateTokens: u.CacheCreationInputTokens,
	}
}

// resultModelUsage is one model's entry in the result frame's
// modelUsage map. camelCase, unlike the snake_case usage blocks; the
// map also carries costUSD, contextWindow and similar fields we
// ignore.
type resultModelUsage struct {
	InputTokens              int `json:"inputTokens"`
	OutputTokens             int `json:"outputTokens"`
	CacheReadInputTokens     int `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int `json:"cacheCreationInputTokens"`
}

// resultTotal returns the result frame's own token total, and whether
// the frame carried one worth trusting. A frame with no usage block,
// or an all-zero one, says nothing about the calls before it — the
// reducer keeps their snapshots rather than superseding them with
// zeros.
func (ev *streamJSONEvent) resultTotal() (provider.TokenUsage, bool) {
	if ev.Usage == nil {
		return provider.TokenUsage{}, false
	}
	total := ev.Usage.tokens()
	return total, !total.IsZero()
}

// gaugeReading converts the result frame's modelUsage map into the
// shape usageGauge.Delta takes. Nil when the frame carried no
// map, so the gauge is left untouched.
func (ev *streamJSONEvent) gaugeReading() map[string]provider.TokenUsage {
	if ev.ModelUsage == nil {
		return nil
	}
	out := make(map[string]provider.TokenUsage, len(ev.ModelUsage))
	for model, u := range ev.ModelUsage {
		out[model] = provider.TokenUsage{
			InputTokens:       u.InputTokens,
			OutputTokens:      u.OutputTokens,
			CacheReadTokens:   u.CacheReadInputTokens,
			CacheCreateTokens: u.CacheCreationInputTokens,
		}
	}
	return out
}

// mcpServersConfig is the JSON shape Claude Code expects from a
// --mcp-config file. Only the subset we use is typed — unknown
// fields are ignored by the consumer.
type mcpServersConfig struct {
	McpServers map[string]mcpServerEntry `json:"mcpServers"`
}

type mcpServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}
