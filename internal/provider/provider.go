// Package provider is the seam between Kivali's core and a model
// driver. It holds only provider-neutral things: the agentic interface
// (Client, Stream, Folder) and its request, event and usage shapes; the
// Provider interface that answers what the core needs to know about the
// models a driver runs (catalog, labels, lineage, prices, effort,
// defaults) and its Credentials; and the test doubles in mock.go.
//
// A driver is one package that implements Client, Provider and
// Credentials on one value. The one driver today is internal/claudeagent;
// nothing under internal/ but that package imports it, and this package
// imports nothing from the module (provider_imports_test.go in
// internal/ holds both rules). The composition roots, main.go and
// agent_cmd.go, construct the driver and hand the one value to every
// consumer that needs a Client or a Provider.
//
// The request shape is independent of the driver:
//   - Required: Model, System, Messages, Tools (the conversational
//     payload). Must round-trip equivalently.
//   - Hint-only: cache_control markers on SystemBlock / ContentBlock /
//     Tool, CompleteRequest.Betas, and the fine-grained token counters
//     on TokenUsage. A driver that cannot honor or report these drops
//     them silently, never errors.
//
// What a driver owns: the provider's binary and flags, its tool-name
// spelling (it strips that spelling from every event, so callers see
// bare names), session ids, and how a failed model call is signalled
// (it reports StreamError + StopError instead). What callers own:
// which tools a run gets, which MCP servers it loads and with what
// arguments, and the transcript.
//
// Model ids are opaque strings to the core: stored as written and
// resolved through Provider.Resolve at read time.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Client is the agentic interface a driver implements: turns, one-shot
// completions and subagent runs.
//
// Model and effort arrive resolved: CompleteRequest.Effort and
// SubagentRequest.Effort are already valid for their Model (resolved
// by the core through Provider.Effort), never a raw stored value.
//
// File content (project docs, PDFs, images) is NOT uploaded to the
// model provider via this interface — it's served through the
// Kivali MCP server's resources capability (internal/mcp/resources.go)
// so the same code works on every transport.
type Client interface {
	Complete(ctx context.Context, req CompleteRequest) (*CompleteResponse, error)
	Stream(ctx context.Context, req CompleteRequest) (Stream, error)

	// FormatUsage renders a cumulative TokenUsage into a summary for
	// the UI: the token counters the driver reported, when any, and a
	// narrative saying how the calls are paid for. Callers render
	// whatever Fields they get — the set is intentionally open so a
	// driver can publish new dimensions without ripple-changes
	// through the UI.
	FormatUsage(u TokenUsage) UsageDisplay

	// HandlesToolLoop reports whether this client drives the full
	// tool loop inside a single Stream call (Stream emits events
	// for every assistant release until the model reaches a non-tool_use
	// final release) or emits events for only one assistant release at a
	// time (caller must iterate, dispatch tools itself, and feed
	// results back via CompleteRequest.Messages).
	//
	// The test double reports false — the caller dispatches tools
	// between iterations, which is what tests exercise.
	// The CLI driver (one `claude -p` subprocess drives the whole
	// conversation via MCP) reports true — Stream's event channel
	// will include StreamToolResult events emitted by the CLI's MCP
	// dispatch, and the caller should not loop or re-dispatch.
	HandlesToolLoop() bool

	// RunSubagent starts one subagent run: a fresh one-shot session
	// with its own tool set and MCP servers, fed req.Prompt once, with
	// no history and no resume. The returned Stream is the same shape
	// Stream returns — its events carry bare tool names, its Final
	// carries Model, ByModel, Usage, CostUSD and StopReason (StopError
	// plus Error when the run ended on a failed model call) — so a
	// caller maps a subagent's events with the code it uses for a
	// turn. Cancelling ctx ends the run; the stream then reports a
	// non-nil Err.
	RunSubagent(ctx context.Context, req SubagentRequest) (Stream, error)
}

// KivaliMCPServer is the name the core's own MCP server is registered
// under in a run's MCPServers. ToolSet.Kivali names that server's
// tools; a driver whose provider spells tool names per server derives
// the spelling from this name.
const KivaliMCPServer = "kivali"

// ToolSet names the tools a run may use, in provider-neutral terms.
type ToolSet struct {
	// Kivali are the kivali MCP server's tools, by bare name (file_view, subagent, ...).
	Kivali []string `json:"kivali"`
	// Builtins are provider-side tools by neutral id (BuiltinWebFetch, BuiltinWebSearch).
	Builtins []string `json:"builtins"`
}

// Neutral ids for provider-side tools a run may be given. A driver maps
// each to its provider's name and refuses an id it does not know.
const (
	BuiltinWebFetch  = "web_fetch"
	BuiltinWebSearch = "web_search"
)

// MCPServer is an MCP server a run loads, as the core launches it.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
}

// SubagentRequest is one subagent run: a fresh one-shot session with
// its own tool set and its own MCP servers; no history, no resume.
type SubagentRequest struct {
	Model, Effort  string // empty: the driver's defaults
	System, Prompt string
	Tools          ToolSet
	MCPServers     []MCPServer
	RunDir         string    // the driver may keep its own files for this run here (config, logs)
	Stderr         io.Writer // the subprocess's stderr, when the driver has one; nil discards
	Purpose, Agent string    // usage metadata, as on CompleteRequest
}

// Stop reasons a driver reports on CompleteResponse.StopReason (and a
// done event carries across the agent-pod wire). The values are the
// Anthropic API's own spellings plus StopError, which no API call
// returns: it marks a run the driver closed because a model call
// failed (a usage limit, an expired login, a 429, an output-token
// maximum), with the provider's text in CompleteResponse.Error.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopError     = "error"
)

// UsageDisplay is what the driver wants the UI to show as the
// post-message "what did that cost" footer. Fields is a flat list
// of (label, value) pairs. Narrative is optional free text shown
// below the fields (e.g. "billed against the signed-in Claude
// account").
type UsageDisplay struct {
	Fields    []UsageField
	Narrative string
}

// UsageField is one labeled value in a UsageDisplay.
type UsageField struct {
	Label string
	Value string
}

// CompleteRequest is the Kivali-shaped input to a model call.
type CompleteRequest struct {
	Model       string
	System      []SystemBlock
	Messages    []Message
	Tools       []Tool
	MaxTokens   int
	Temperature float64

	// Effort is the reasoning level for the turn. Callers resolve it
	// before the request reaches a Client: it is already valid for
	// Model (an id Provider.Effort accepts for it, or that model's
	// default), or empty for a model with no reasoning control. A
	// driver never has to second-guess a stored value; it may still
	// guard its own wire against an id it cannot send.
	Effort string

	// Betas is a list of additional anthropic-beta flags a request
	// would need (e.g. "context-management-2025-06-27" for the memory
	// tool). A hint: the CLI driver cannot set beta headers and drops
	// them silently.
	Betas []string

	// Metadata captured on the UsageEvent emitted by this call.
	Purpose string // "release" | "chat" | "summary" | "seed" | ...
	Agent   string
}

// SystemBlock is one piece of the layered system prompt. Marking Cache=true
// attaches cache_control: ephemeral so the block is served from prompt
// cache on subsequent calls within the TTL window.
type SystemBlock struct {
	Text  string
	Cache bool
}

// Message is one release in the conversation.
type Message struct {
	Role    Role
	Content []ContentBlock
}

// Role is the speaker of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ContentType enumerates the kinds of content blocks Kivali uses.
type ContentType string

const (
	ContentText       ContentType = "text"
	ContentToolUse    ContentType = "tool_use"
	ContentToolResult ContentType = "tool_result"
	// ContentImage is a raw image attached to a user-role message.
	// Used for tasks where the model must describe or analyze the
	// image (e.g. project-files summarization for uploaded
	// screenshots, diagrams, scans). Anthropic accepts image/png,
	// image/jpeg, image/gif, image/webp; other MIME types must be
	// converted before sending.
	ContentImage ContentType = "image"
)

// ContentBlock is one piece of message or system content. Only fields
// relevant to Type are populated.
//
// Binary content (PDFs, images, etc.) is not expressed as content
// blocks — it reaches the model through the Kivali MCP server's
// resources capability instead.
type ContentBlock struct {
	Type ContentType
	Text string

	// tool_use
	ToolUseID string
	ToolName  string
	ToolInput json.RawMessage

	// tool_result
	ToolResultID      string
	ToolResultContent string
	ToolResultIsError bool

	// image — the raw bytes live in ImageData, pre-base64-encoded on
	// the wire. MediaType must be one of image/png, image/jpeg,
	// image/gif, image/webp (Anthropic's accepted set). Unused for
	// non-image blocks.
	ImageMediaType string
	ImageData      []byte

	// Cache, when true, emits cache_control: ephemeral on this block's
	// wire form. Set it on the LAST block the caller wants treated as
	// a cacheable prefix boundary (up to ~4 breakpoints per request).
	Cache bool
}

// Tool describes a tool available to the model, with a JSON Schema input.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema for input

	// Cache, when true, emits cache_control: ephemeral on this tool's
	// wire form. Set it on the LAST tool the caller wants cached so
	// the entire tool-definition block becomes a cacheable prefix.
	Cache bool
}

// CompleteResponse is the decoded model output.
//
// CostUSD is the per-call cost the CLI reported on its "result" event
// (total_cost_usd), read by the persistent and per-call runners. A
// driver that reports none leaves it zero — callers compute cost
// themselves via the pricing table. Zero means "no driver-reported
// cost"; treat as a hint to
// fall back to local pricing.
type CompleteResponse struct {
	ID         string
	Model      string
	StopReason string
	Content    []ContentBlock
	Usage      TokenUsage
	CostUSD    float64

	// Error is why the run stopped, set together with StopReason ==
	// StopError and empty otherwise: the provider's text when it gave
	// one (the same text the stream's StreamError events carried),
	// else what the driver knows of the cause (an error result's kind,
	// an exit status). Never the agent speaking: it is not in Content.
	Error string

	// ByModel splits Usage by the model that actually served each call,
	// in first-seen order. Summing it reproduces Usage exactly.
	//
	// Model (above) is the LAST model to answer, kept for callers that
	// want a single label. It is not a safe basis for pricing a mixed
	// turn — Usage accumulates across every call, so pricing the total
	// at one id bills the whole turn at that id's rate. Price from
	// ByModel when it is populated.
	//
	// Empty on transports that don't surface per-call model ids; callers
	// fall back to {Model, Usage} as a single bucket.
	ByModel []ModelUsage

	// ContextTokens is the window occupancy of the SINGLE most recent
	// API call in this turn: input_tokens + cache_read_input_tokens +
	// cache_creation_input_tokens off the last assistant message.
	//
	// This is deliberately distinct from Usage, which ACCUMULATES across
	// every /v1/messages call a multi-step tool loop makes (Anthropic
	// bills the sum). That accumulated figure is a billing total, not a
	// window measurement — a 2-call turn doubles it, a 5-call turn
	// quintuples it, even though the model never saw more than one
	// prompt's worth of context at once. ContextTokens answers the
	// different question "how full was the window on the last call?" and
	// is the correct numerator for a context-fill gauge.
	//
	// Zero when the driver did not surface per-call usage, in which
	// case the gauge falls back to a chars/4 estimate.
	ContextTokens int
}

// TokenUsage accounts tokens for one API call.
type TokenUsage struct {
	InputTokens       int
	OutputTokens      int
	CacheReadTokens   int
	CacheCreateTokens int
}

// Add returns the element-wise sum of two TokenUsage values. Used to
// fold per-call usage into a per-model bucket.
func (u TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:       u.InputTokens + o.InputTokens,
		OutputTokens:      u.OutputTokens + o.OutputTokens,
		CacheReadTokens:   u.CacheReadTokens + o.CacheReadTokens,
		CacheCreateTokens: u.CacheCreateTokens + o.CacheCreateTokens,
	}
}

// IsZero reports whether every counter is zero.
func (u TokenUsage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheCreateTokens == 0
}

// ModelUsage is one model's share of a turn's token totals.
//
// A turn is a tool loop of N /v1/messages calls, and the model that
// answers is per-call, not per-turn: a request can be served by a
// different model than the one asked for. Attributing the whole turn to
// a single id would price every token at that id's rate,
// which is wrong by the ratio between the two whenever a turn is mixed
// — Fable and Opus differ by 2x, in whichever direction the last call
// happened to land.
//
// Summing Usage across a turn's ModelUsage slice reproduces
// CompleteResponse.Usage exactly; the split is the same tokens bucketed,
// never a different total.
type ModelUsage struct {
	Model string
	Usage TokenUsage
}

// Stream is a live streaming model response.
//
// Usage:
//
//	stream, err := client.Stream(ctx, req)
//	if err != nil { ... }
//	defer stream.Close()
//	for ev := range stream.Events() {
//	    if ev.Kind == StreamDelta { w.Write([]byte(ev.Text)) }
//	}
//	if err := stream.Err(); err != nil { ... }
//	final := stream.Final() // may be non-nil even on error (partial)
type Stream interface {
	Events() <-chan StreamEvent
	Err() error
	Close() error
	Final() *CompleteResponse
}

// Folder is implemented by a Stream whose transport can splice a new
// user message into the turn that is ALREADY RUNNING, rather than
// ending that turn and starting another one with the message
// prepended.
//
// The CLI transport can do this: its stream-json stdin accepts a user
// message at any time, and the CLI folds it into the in-flight turn at
// the next tool round — in the very request that was going to carry
// the tool results anyway. Nothing is aborted, no generated output is
// discarded, and the prompt-cache prefix stays intact, which is what
// makes it cheaper than stop-and-reprompt rather than merely faster.
//
// SendFold returns the delivery's uuid. It reports only whether the
// message reached the CLI's queue; the OUTCOME arrives later as a
// StreamMessageFolded event carrying that uuid. A fold that misses the
// turn is not an error — see StreamMessageFolded.
//
// Transports that cannot fold simply do not implement this, and
// callers fall back to ending the turn.
type Folder interface {
	SendFold(text string) (string, error)
}

// StreamEventKind is the kind of a streaming event.
type StreamEventKind string

const (
	StreamDelta          StreamEventKind = "delta"
	StreamEnd            StreamEventKind = "end"
	StreamToolUseStart   StreamEventKind = "tool_use_start"
	StreamToolUseEnd     StreamEventKind = "tool_use_end"
	StreamToolInputDelta StreamEventKind = "tool_input_delta"
	// StreamThinking carries a chunk of the model's extended-reasoning
	// ("thinking") output in Text. Emitted only by transports that
	// surface thinking blocks (currently the SDK/CLI transport). Purely
	// a live progress signal — thinking is NOT persisted to chat.jsonl
	// and not replayed back into the next request (the CLI owns its own
	// session context); the UI uses it to show what the model is
	// currently reasoning about while a turn is in flight.
	StreamThinking StreamEventKind = "thinking"
	// StreamToolResult is emitted only by a client whose
	// HandlesToolLoop() returns true — the CLI driver receives
	// user-release tool_result blocks from the CLI's own MCP dispatch
	// and surfaces them so the UI can persist + render them without
	// the caller having to dispatch the tool itself.
	StreamToolResult StreamEventKind = "tool_result"
	// StreamMessageFolded reports the fate of a message the caller
	// spliced into a turn that was ALREADY RUNNING (see
	// Folder.SendFold). Emitted only by transports that can do that
	// — currently the SDK/CLI transport, whose CLI folds a queued
	// user message into the in-flight turn between tool rounds.
	//
	// FoldLanded distinguishes the two outcomes, and the caller needs
	// both: landed means the running turn consumed the message and
	// will answer it, so the caller appends it to the transcript at
	// this point in the stream; missed means the turn ended without
	// it (typically because no further tool round happened) and the
	// message will run as its own turn instead. Missed is NOT an
	// error — nothing is lost either way.
	StreamMessageFolded StreamEventKind = "message_folded"
	// StreamError carries, in Text, the provider's account of a model
	// call that failed and ended the run: a usage limit, an expired
	// login, a 429, an output-token maximum. It is the cause of the
	// failure, not the model's reply — consumers never file it as the
	// agent speaking. The stream's Final then reports StopReason
	// StopError with the same text in Error.
	StreamError StreamEventKind = "error"
)

// StreamEvent is a single event delivered on the stream's Events channel.
// Kind determines which fields are populated:
//
//	StreamDelta          → Text
//	StreamThinking       → Text (extended-reasoning chunk)
//	StreamToolUseStart   → ToolUseID, ToolName
//	StreamToolInputDelta → ToolUseID, Text (partial JSON chunk)
//	StreamToolUseEnd     → ToolUseID, ToolName, ToolInput (full JSON)
//	StreamToolResult     → ToolUseID, ToolResultText, ToolResultIsError
//	StreamMessageFolded  → FoldUUID, FoldLanded
//	StreamError          → Text (the failure's detail)
//	StreamEnd            → (no fields)
type StreamEvent struct {
	Kind      StreamEventKind
	Text      string
	ToolUseID string
	ToolName  string
	ToolInput json.RawMessage

	// Model is the model id that produced this event, when the
	// transport knows it per-call (the SDK/CLI transport reads it off
	// each stream-json assistant message). Empty on transports that
	// don't surface it, and on events that aren't tied to one call.
	//
	// Carried on the event rather than only on the final response
	// because the consumer needs it WHILE the turn runs: a chat bubble
	// is labeled with the model that wrote it, and a turn can switch
	// models between calls, so a value that only arrives at the end
	// can label the earlier text wrong.
	Model string

	// tool_result payload (StreamToolResult events only).
	ToolResultText    string
	ToolResultIsError bool

	// StreamMessageFolded payload. FoldUUID is the id SendFold
	// returned for this delivery; FoldLanded says whether the
	// running turn consumed it.
	FoldUUID   string
	FoldLanded bool
}

// UsageEvent is emitted to the UsageRecorder after each API call.
type UsageEvent struct {
	TS                time.Time
	Model             string
	Purpose           string
	Agent             string
	InputTokens       int
	OutputTokens      int
	CacheReadTokens   int
	CacheCreateTokens int
	CostUSD           float64
}

// UsageRecorder is called after every API call to persist token/cost
// telemetry. Errors are logged but do not fail the API call.
type UsageRecorder func(UsageEvent) error

// ErrSubprocessExited is returned (wrapped) by Stream.Err when the
// driver's subprocess died mid-stream — the provider's process was killed
// (OOM, segfault, kubelet eviction). Distinct from "normal" stream
// errors (parse failures, spawn setup) so the chat-loop can write a
// runtime-disruption marker for the agent to recover from.
//
// Callers identify with errors.Is(err, provider.ErrSubprocessExited).
var ErrSubprocessExited = errors.New("provider: subprocess exited mid-stream")

// ErrUserCancelled is returned (wrapped) by Stream.Err when the
// transport killed its subprocess in response to a user-driven Stop
// click — distinct from ErrSubprocessExited (a crash) so the chat-loop
// surfaces the failure as a cancelled turn (FailedReasonCancelled, no
// runtime-disruption row), not a runtime crash.
var ErrUserCancelled = errors.New("provider: turn cancelled by user")
