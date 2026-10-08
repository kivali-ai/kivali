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
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// The subagent path on the driver seam: the runtime builds a
// provider.SubagentRequest from the spec, hands it to Claude.RunSubagent,
// and maps the stream it gets back with the code a parent turn uses.
// Each test drives handleSubagentTurn directly, so it returns only
// after the terminal event is posted — no polling.

// subagentRuntime is a Runtime wired for subagent runs against fc and
// cc, with its scratch under a temp dir.
func subagentRuntime(t *testing.T, fc *fakeCore, cc provider.Client) *Runtime {
	t.Helper()
	return &Runtime{
		Client:        NewClient(fc.addr, "alice"),
		Claude:        cc,
		Logger:        log.New(io.Discard, "", 0),
		KivaliBinary:  "/usr/local/bin/kivali",
		CoreUDS:       fc.addr,
		ScratchRoot:   t.TempDir(),
		SubagentModel: "claude-sonnet-5",
	}
}

// subagentTurnEvent is the chat-turn event core publishes for one
// subagent run of depth.
func subagentTurnEvent(t *testing.T, turnID string, depth int, spec SubagentSpec) Event {
	t.Helper()
	spec.Depth = depth
	body, err := json.Marshal(ChatTurnEvent{
		TurnID: turnID, Slug: "alice", Source: "subagent",
		Kind: ChatTurnKindSubagent, Subagent: &spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Event{Type: EventChatTurn, Data: body}
}

func lastPosted(t *testing.T, fc *fakeCore, turnID string) TurnEvent {
	t.Helper()
	got := fc.postedFor(turnID)
	if len(got) == 0 {
		t.Fatalf("turn %s: nothing posted", turnID)
	}
	return got[len(got)-1]
}

// TestSubagentTurnRunsThroughTheDriver: a normal run. The request the
// driver sees carries the spec's prompt, system prompt, neutral tool
// set and the one kivali MCP server with its launch arguments; the
// events come back as the same TurnEvents a parent turn posts; done
// carries the final release's text and the run's usage.
func TestSubagentTurnRunsThroughTheDriver(t *testing.T) {
	fc := startFakeCore(t)
	mc := &provider.MockClient{
		RunSubagentFn: func(_ context.Context, req provider.SubagentRequest) (provider.Stream, error) {
			return provider.NewMockStream(
				[]provider.StreamEvent{
					{Kind: provider.StreamDelta, Text: "Reading the log.", Model: "claude-haiku-4-5"},
					{Kind: provider.StreamToolUseStart, ToolUseID: "tu1", ToolName: "file_view"},
					{Kind: provider.StreamToolUseEnd, ToolUseID: "tu1", ToolName: "file_view", ToolInput: json.RawMessage(`{"path":"/files/project/log.md"}`)},
					{Kind: provider.StreamToolResult, ToolUseID: "tu1", ToolResultText: "2 reminders bounced"},
					{Kind: provider.StreamDelta, Text: "Two reminders bounced.", Model: "claude-haiku-4-5"},
					{Kind: provider.StreamEnd},
				},
				&provider.CompleteResponse{
					Model:      "claude-haiku-4-5",
					StopReason: provider.StopEndTurn,
					Content: []provider.ContentBlock{
						{Type: provider.ContentText, Text: "Reading the log."},
						{Type: provider.ContentToolUse, ToolUseID: "tu1", ToolName: "file_view"},
						{Type: provider.ContentText, Text: "Two reminders bounced."},
					},
					Usage:   provider.TokenUsage{InputTokens: 30, OutputTokens: 300, CacheReadTokens: 300},
					CostUSD: 0.5,
					ByModel: []provider.ModelUsage{
						{Model: "claude-opus-5", Usage: provider.TokenUsage{InputTokens: 10, OutputTokens: 100, CacheReadTokens: 100}},
						{Model: "claude-haiku-4-5", Usage: provider.TokenUsage{InputTokens: 20, OutputTokens: 200, CacheReadTokens: 200}},
					},
				},
			), nil
		},
	}
	rt := subagentRuntime(t, fc, mc)
	spec := SubagentSpec{
		SubagentID:   "sub1",
		Model:        "claude-haiku-4-5",
		Effort:       "low",
		SystemPrompt: "be brief",
		UserPrompt:   "summarise the log",
		Tools:        SubagentTools(2),
	}
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-sub", 2, spec)); err != nil {
		t.Fatal(err)
	}

	reqs := mc.SubagentRequests()
	if len(reqs) != 1 {
		t.Fatalf("RunSubagent calls = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Model != "claude-haiku-4-5" || req.Effort != "low" {
		t.Errorf("model/effort = %q/%q, want the spec's", req.Model, req.Effort)
	}
	if req.System != "be brief" || req.Prompt != "summarise the log" {
		t.Errorf("system/prompt = %q/%q, want the spec's", req.System, req.Prompt)
	}
	if !reflect.DeepEqual(req.Tools, SubagentTools(2)) {
		t.Errorf("Tools = %+v, want SubagentTools(2)", req.Tools)
	}
	if req.Purpose != "subagent" || req.Agent != "alice" {
		t.Errorf("usage metadata = %q/%q, want subagent/alice", req.Purpose, req.Agent)
	}
	if want := filepath.Join(rt.ScratchRoot, "subagents", "sub1"); req.RunDir != want {
		t.Errorf("RunDir = %q, want %q", req.RunDir, want)
	}
	if req.Stderr == nil {
		t.Error("Stderr = nil; the runtime keeps the run's stderr")
	}
	wantServer := provider.MCPServer{
		Name:    provider.KivaliMCPServer,
		Command: "/usr/local/bin/kivali",
		Args: []string{
			"mcp", "--toolkit", "subagent", "--parent", "alice", "--subagent-id", "sub1",
			"--core-uds", fc.addr, "--subagent-files-root", "/files/subagents/sub1", "--depth", "2",
		},
	}
	if len(req.MCPServers) != 1 || !reflect.DeepEqual(req.MCPServers[0], wantServer) {
		t.Errorf("MCPServers = %+v, want exactly %+v", req.MCPServers, wantServer)
	}
	// The run dir is the run's: removed once the run is over.
	if _, err := os.Stat(req.RunDir); !os.IsNotExist(err) {
		t.Errorf("run dir still present after the run (stat err %v)", err)
	}

	got := fc.postedFor("t-sub")
	wantKinds := []TurnEventKind{
		TurnEventDelta, TurnEventToolUseStart, TurnEventToolUseEnd, TurnEventToolResult, TurnEventDelta, TurnEventDone,
	}
	var kinds []TurnEventKind
	for _, ev := range got {
		kinds = append(kinds, ev.Kind)
	}
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("posted kinds = %v, want %v", kinds, wantKinds)
	}
	if got[1].ToolName != "file_view" {
		t.Errorf("tool name = %q, want the bare name the driver emits", got[1].ToolName)
	}
	done := got[len(got)-1]
	if done.Text != "Two reminders bounced." {
		t.Errorf("done Text = %q, want the final release's text only", done.Text)
	}
	if done.Model != "claude-haiku-4-5" || done.CostUSD != 0.5 || done.StopReason != provider.StopEndTurn {
		t.Errorf("done model/cost/stop = %q/%v/%q", done.Model, done.CostUSD, done.StopReason)
	}
	if done.InputTokens != 30 || done.OutputTokens != 300 || done.CacheReadTokens != 300 {
		t.Errorf("done tokens = %d/%d/%d, want 30/300/300", done.InputTokens, done.OutputTokens, done.CacheReadTokens)
	}
	if len(done.ByModel) != 2 || done.ByModel[0].Model != "claude-opus-5" || done.ByModel[1].OutputTokens != 200 {
		t.Errorf("done ByModel = %+v, want the driver's two-model split", done.ByModel)
	}
}

// TestSubagentTurnModelFallsBackToRequested: a run whose driver named
// no model still records one — the model it was asked to use, which is
// the pod's default when the spec left it empty.
func TestSubagentTurnModelFallsBackToRequested(t *testing.T) {
	fc := startFakeCore(t)
	mc := &provider.MockClient{
		RunSubagentFn: func(context.Context, provider.SubagentRequest) (provider.Stream, error) {
			return provider.NewMockStream(nil, &provider.CompleteResponse{
				Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "ok"}},
				Usage:   provider.TokenUsage{InputTokens: 5},
			}), nil
		},
	}
	rt := subagentRuntime(t, fc, mc)
	spec := SubagentSpec{SubagentID: "sub2", UserPrompt: "hi", Tools: SubagentTools(1)}
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-model", 1, spec)); err != nil {
		t.Fatal(err)
	}
	if got := mc.SubagentRequests()[0].Model; got != "claude-sonnet-5" {
		t.Errorf("requested model = %q, want the runtime's SubagentModel", got)
	}
	if done := lastPosted(t, fc, "t-model"); done.Kind != TurnEventDone || done.Model != "claude-sonnet-5" || done.Text != "ok" {
		t.Errorf("done = %+v, want done/claude-sonnet-5/ok", done)
	}
}

// TestSubagentTurnModelFallsBackToTheProviderDefault: with neither the
// spec nor the runtime naming a model, the run asks for the provider's
// subagent default — the pod holds the same driver as core, so the
// fallback is never a literal of its own.
func TestSubagentTurnModelFallsBackToTheProviderDefault(t *testing.T) {
	fc := startFakeCore(t)
	mc := &provider.MockClient{
		RunSubagentFn: func(context.Context, provider.SubagentRequest) (provider.Stream, error) {
			return provider.NewMockStream(nil, &provider.CompleteResponse{
				Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "ok"}},
			}), nil
		},
	}
	rt := subagentRuntime(t, fc, mc)
	rt.SubagentModel = ""
	rt.Provider = provider.MockProvider{}
	spec := SubagentSpec{SubagentID: "sub3", UserPrompt: "hi", Tools: SubagentTools(1)}
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-default", 1, spec)); err != nil {
		t.Fatal(err)
	}
	if got, want := mc.SubagentRequests()[0].Model, (provider.MockProvider{}).Defaults().Subagent; got != want {
		t.Errorf("requested model = %q, want the provider's subagent default %q", got, want)
	}
}

// TestSubagentTurnFailedReasons maps each way a run can end badly onto
// the failed event core reads, and checks the run's partial text and
// usage ride on it either way.
func TestSubagentTurnFailedReasons(t *testing.T) {
	partial := &provider.CompleteResponse{
		Model:   "claude-haiku-4-5",
		Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "half an answer"}},
		Usage:   provider.TokenUsage{InputTokens: 7, CacheReadTokens: 900},
	}
	typedError := *partial
	typedError.StopReason = provider.StopError
	typedError.Error = "API Error: 529 overloaded"

	cases := []struct {
		name       string
		stream     func() provider.Stream
		wantReason string
		wantDetail string
	}{
		{
			name: "typed model error",
			stream: func() provider.Stream {
				return provider.NewMockStream([]provider.StreamEvent{{Kind: provider.StreamError, Text: typedError.Error}}, &typedError)
			},
			wantReason: FailedReasonTurnError,
			wantDetail: "API Error: 529 overloaded",
		},
		{
			// The provider's process exiting non-zero over the error it
			// already reported does not turn a model error into a crash.
			name: "typed model error then exit",
			stream: func() provider.Stream {
				s := provider.NewMockStreamErr(nil, fmt.Errorf("claudeagent: %w (cause: exit status 1)", provider.ErrSubprocessExited))
				return &finalOverride{MockStream: s, final: &typedError}
			},
			wantReason: FailedReasonTurnError,
			wantDetail: "API Error: 529 overloaded",
		},
		{
			name: "process died",
			stream: func() provider.Stream {
				s := provider.NewMockStreamErr(nil, fmt.Errorf("claudeagent: %w (cause: signal: killed)", provider.ErrSubprocessExited))
				return &finalOverride{MockStream: s, final: partial}
			},
			wantReason: FailedReasonSubprocessDied,
			wantDetail: "subagent: claudeagent: provider: subprocess exited mid-stream (cause: signal: killed)",
		},
		{
			name: "other transport error",
			stream: func() provider.Stream {
				s := provider.NewMockStreamErr(nil, errors.New("parse stream-json: invalid"))
				return &finalOverride{MockStream: s, final: partial}
			},
			wantReason: FailedReasonOtherError,
			wantDetail: "subagent: parse stream-json: invalid",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := startFakeCore(t)
			mc := &provider.MockClient{
				RunSubagentFn: func(context.Context, provider.SubagentRequest) (provider.Stream, error) { return tc.stream(), nil },
			}
			rt := subagentRuntime(t, fc, mc)
			turnID := fmt.Sprintf("t-fail-%d", i)
			spec := SubagentSpec{SubagentID: "sub3", UserPrompt: "go", Tools: SubagentTools(1)}
			if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, turnID, 1, spec)); err != nil {
				t.Fatal(err)
			}
			ev := lastPosted(t, fc, turnID)
			if ev.Kind != TurnEventFailed || ev.FailedReason != tc.wantReason {
				t.Fatalf("terminal = %s/%s, want failed/%s", ev.Kind, ev.FailedReason, tc.wantReason)
			}
			if ev.FailedDetail != tc.wantDetail {
				t.Errorf("FailedDetail = %q, want %q", ev.FailedDetail, tc.wantDetail)
			}
			if ev.Text != "half an answer" {
				t.Errorf("Text = %q, want the partial answer", ev.Text)
			}
			if ev.InputTokens != 7 || ev.CacheReadTokens != 900 || ev.Model != "claude-haiku-4-5" {
				t.Errorf("partial usage = %d/%d %q, want 7/900 claude-haiku-4-5", ev.InputTokens, ev.CacheReadTokens, ev.Model)
			}
		})
	}
}

// TestSubagentTurnStartFailure: the driver refusing to start the run
// (an unknown builtin, no CLI on PATH) is a failed run, not a hang.
func TestSubagentTurnStartFailure(t *testing.T) {
	fc := startFakeCore(t)
	mc := &provider.MockClient{
		RunSubagentFn: func(context.Context, provider.SubagentRequest) (provider.Stream, error) {
			return nil, errors.New(`claudeagent: unknown builtin tool "x"`)
		},
	}
	rt := subagentRuntime(t, fc, mc)
	spec := SubagentSpec{SubagentID: "sub4", UserPrompt: "go"}
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-start", 1, spec)); err != nil {
		t.Fatal(err)
	}
	ev := lastPosted(t, fc, "t-start")
	if ev.Kind != TurnEventFailed || ev.FailedReason != FailedReasonOtherError || !strings.Contains(ev.FailedDetail, "unknown builtin") {
		t.Errorf("terminal = %+v, want failed/other-error naming the driver's refusal", ev)
	}
}

// TestSubagentTurnCancelled: EventCancelTurn for the run's turn cancels
// the context the run was started with; the run ends, and the runtime
// posts failed/cancelled with what the run had produced.
func TestSubagentTurnCancelled(t *testing.T) {
	fc := startFakeCore(t)
	started := make(chan struct{})
	mc := &provider.MockClient{
		RunSubagentFn: func(ctx context.Context, _ provider.SubagentRequest) (provider.Stream, error) {
			s := &cancellableStream{events: make(chan provider.StreamEvent, 4), ctx: ctx}
			go func() {
				s.events <- provider.StreamEvent{Kind: provider.StreamDelta, Text: "starting"}
				close(started)
				<-ctx.Done()
				s.errMu.Lock()
				s.err = fmt.Errorf("claudeagent: %w (cause: signal: killed)", provider.ErrSubprocessExited)
				s.errMu.Unlock()
				close(s.events)
			}()
			return s, nil
		},
	}
	rt := subagentRuntime(t, fc, mc)
	spec := SubagentSpec{SubagentID: "sub5", UserPrompt: "go", Tools: SubagentTools(1)}
	finished := make(chan error, 1)
	go func() {
		finished <- rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-cancel-sub", 1, spec))
	}()
	<-started
	body, _ := json.Marshal(CancelTurnEvent{TurnID: "t-cancel-sub", Slug: "alice"})
	rt.handleCancelTurn(Event{Type: EventCancelTurn, Data: body})
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	ev := lastPosted(t, fc, "t-cancel-sub")
	if ev.Kind != TurnEventFailed || ev.FailedReason != FailedReasonCancelled {
		t.Errorf("terminal = %s/%s, want failed/cancelled (a killed run on Stop is a cancel, not a crash)", ev.Kind, ev.FailedReason)
	}
}

// TestSubagentFinalText pins which text a parent receives: the final
// release, i.e. what follows the last tool call; everything when the
// run never got past one.
func TestSubagentFinalText(t *testing.T) {
	text := func(s string) provider.ContentBlock {
		return provider.ContentBlock{Type: provider.ContentText, Text: s}
	}
	tool := provider.ContentBlock{Type: provider.ContentToolUse, ToolUseID: "tu"}
	cases := []struct {
		name    string
		content []provider.ContentBlock
		want    string
	}{
		{"no tools", []provider.ContentBlock{text("a"), text("b")}, "ab"},
		{"after the last tool", []provider.ContentBlock{text("narration"), tool, text("x"), tool, text("answer")}, "answer"},
		{"ended in a tool round", []provider.ContentBlock{text("partial "), tool}, "partial "},
		{"nothing", nil, ""},
	}
	for _, tc := range cases {
		if got := subagentFinalText(&provider.CompleteResponse{Content: tc.content}); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := subagentFinalText(nil); got != "" {
		t.Errorf("nil final: got %q", got)
	}
}

// finalOverride is a MockStream whose Final is a partial response,
// the way a real stream's is after it fails mid-run.
type finalOverride struct {
	*provider.MockStream
	final *provider.CompleteResponse
}

func (f *finalOverride) Final() *provider.CompleteResponse { return f.final }

// TestChatTurnRunnerDiedDuringStopIsADisruption: on a parent turn, a
// Stop that coincides with the runner dying for another reason is not
// a cancel. Only provider.ErrUserCancelled or context.Canceled count; a
// subprocess exit posts subprocess-died, so core writes the
// runtime-disruption row and the spawn gate resumes the agent.
func TestChatTurnRunnerDiedDuringStopIsADisruption(t *testing.T) {
	fc := startFakeCore(t)
	started := make(chan struct{})
	cc := &scriptedClaudeClient{
		streams: func(ctx context.Context, _ provider.CompleteRequest) (provider.Stream, error) {
			s := &cancellableStream{events: make(chan provider.StreamEvent, 4), ctx: ctx}
			go func() {
				close(started)
				<-ctx.Done()
				s.errMu.Lock()
				s.err = fmt.Errorf("claudeagent: %w (cause: signal: segmentation fault)", provider.ErrSubprocessExited)
				s.errMu.Unlock()
				close(s.events)
			}()
			return s, nil
		},
	}
	rt := &Runtime{Client: NewClient(fc.addr, "alice"), Claude: cc, Logger: log.New(io.Discard, "", 0)}
	body, _ := json.Marshal(ChatTurnEvent{TurnID: "t-died", Slug: "alice"})
	finished := make(chan error, 1)
	go func() { finished <- rt.handleChatTurn(context.Background(), Event{Type: EventChatTurn, Data: body}) }()
	<-started
	cancelBody, _ := json.Marshal(CancelTurnEvent{TurnID: "t-died", Slug: "alice"})
	rt.handleCancelTurn(Event{Type: EventCancelTurn, Data: cancelBody})
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if ev := lastPosted(t, fc, "t-died"); ev.Kind != TurnEventFailed || ev.FailedReason != FailedReasonSubprocessDied {
		t.Errorf("terminal = %s/%s, want failed/subprocess-died", ev.Kind, ev.FailedReason)
	}
}

// TestSubagentTurnErrorDetailFallsBackToStreamError: a StopError the
// driver gave no text for reports the stream's own error, not "no
// detail".
func TestSubagentTurnErrorDetailFallsBackToStreamError(t *testing.T) {
	out := streamOutcome{
		err:   fmt.Errorf("claudeagent: %w (cause: exit status 1)", provider.ErrSubprocessExited),
		final: &provider.CompleteResponse{StopReason: provider.StopError},
	}
	ev := subagentTerminalEvent(out, "claude-sonnet-5")
	if ev.FailedReason != FailedReasonTurnError || !strings.Contains(ev.FailedDetail, "exit status 1") {
		t.Errorf("terminal = %s %q, want turn-error carrying the exit status", ev.FailedReason, ev.FailedDetail)
	}
}
