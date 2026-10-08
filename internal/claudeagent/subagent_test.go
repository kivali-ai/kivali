package claudeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// Driver tests for RunSubagent against the fake CLI (TestMain's
// GO_FAKE_CLAUDE branch). The scenarios are played by fakeSubagentRun:
//
//	"subagent"       — one tool round and a closing answer, with the
//	                   CLI's usage snapshots, result-frame total, cost
//	                   and modelUsage gauge.
//	"subagent-error" — some text, then the CLI's close-out message
//	                   under the "<synthetic>" model carrying the API
//	                   error, an is_error result, and exit status 1.
//	"subagent-hang"  — one text block, then nothing until killed.
const (
	fakeSubagentAnswer = "Two test reminders bounced."
	fakeSubagentError  = `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
)

var (
	fakeSubagentCallA = provider.TokenUsage{InputTokens: 12, OutputTokens: 40, CacheReadTokens: 1000, CacheCreateTokens: 50}
	fakeSubagentCallB = provider.TokenUsage{InputTokens: 8, OutputTokens: 60, CacheReadTokens: 1100}
)

func fakeSubagentRun(emit func(map[string]any), scenario string) {
	assistant := func(id, model string, block map[string]any, snap provider.TokenUsage) {
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id": id, "role": "assistant", "model": model,
				"content": []map[string]any{block},
				"usage":   fakeUsageSnapshot(snap),
			},
		})
	}
	switch scenario {
	case "subagent":
		assistant("msg_s1", "claude-haiku-4-5", map[string]any{"type": "text", "text": "Reading the log."}, fakeSubagentCallA)
		assistant("msg_s1", "claude-haiku-4-5", map[string]any{
			"type": "tool_use", "id": "toolu_s1", "name": "mcp__kivali__file_view",
			"input": map[string]any{"path": "/files/project/log.md"},
		}, fakeSubagentCallA)
		emit(map[string]any{
			"type": "user",
			"message": map[string]any{"role": "user", "content": []map[string]any{{
				"type": "tool_result", "tool_use_id": "toolu_s1",
				"content": []map[string]any{{"type": "text", "text": "2 of 12 fail PD"}},
			}}},
		})
		assistant("msg_s2", "claude-haiku-4-5", map[string]any{"type": "text", "text": fakeSubagentAnswer}, fakeSubagentCallB)
		total := fakeSubagentCallA.Add(fakeSubagentCallB)
		emit(map[string]any{
			"type": "result", "subtype": "success", "result": fakeSubagentAnswer,
			"total_cost_usd": 0.0042,
			"usage":          fakeUsageBlock(total),
			"modelUsage":     map[string]any{"claude-haiku-4-5": fakeModelUsageEntry(total, 1)},
		})
	case "subagent-error":
		assistant("msg_e1", "claude-haiku-4-5", map[string]any{"type": "text", "text": "Checking the rig."}, fakeSubagentCallA)
		assistant("msg_e2", "<synthetic>", map[string]any{"type": "text", "text": fakeSubagentError}, provider.TokenUsage{CacheReadTokens: 500})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": true})
		os.Exit(1)
	case "subagent-hang":
		assistant("msg_h1", "claude-haiku-4-5", map[string]any{"type": "text", "text": "starting"}, fakeSubagentCallA)
		blockUntilSignal()
	}
}

// subagentTestRequest is the request a pod's runtime would build for a
// depth-1 subagent.
func subagentTestRequest(runDir string) provider.SubagentRequest {
	return provider.SubagentRequest{
		Model:  "claude-haiku-4-5",
		Effort: EffortLow,
		System: "You are a subagent.",
		Prompt: "Summarise the test log.",
		Tools: provider.ToolSet{
			Kivali:   []string{"file_view", "run_shell", "subagent"},
			Builtins: []string{provider.BuiltinWebFetch, provider.BuiltinWebSearch},
		},
		MCPServers: []provider.MCPServer{{
			Name:    provider.KivaliMCPServer,
			Command: "/usr/local/bin/kivali",
			Args:    []string{"mcp", "--toolkit", "subagent", "--depth", "1"},
		}},
		RunDir:  runDir,
		Purpose: "subagent",
		Agent:   "alice",
	}
}

// fakeSubagentResult is one run against the fake CLI: its events,
// final response, error, and the argv the fake saw.
type fakeSubagentResult struct {
	events []provider.StreamEvent
	final  *provider.CompleteResponse
	err    error
	args   []string
}

// runSubagentFake runs one subagent against the fake CLI scenario.
func runSubagentFake(t *testing.T, scenario string, req provider.SubagentRequest, recorder provider.UsageRecorder) fakeSubagentResult {
	t.Helper()
	withFakeCLIEnv(t, scenario)
	argsFile := filepath.Join(t.TempDir(), "args.jsonl")
	t.Setenv("GO_FAKE_CLAUDE_ARGS_FILE", argsFile)
	opts := helperOpts(t, scenario, nil)
	opts.Recorder = recorder
	c := New(opts)
	s, err := c.RunSubagent(context.Background(), req)
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	events, final := drainTurn(t, s)
	runErr := s.Err()
	_ = s.Close()
	spawns := readSpawnArgs(t, argsFile)
	if len(spawns) != 1 {
		t.Fatalf("fake CLI spawns = %d, want 1", len(spawns))
	}
	return fakeSubagentResult{events: events, final: final, err: runErr, args: spawns[0]}
}

// TestRunSubagentSpellsTheRequestTheCLIWay: the neutral request becomes
// the CLI's flags — kivali tools prefixed in --allowedTools, builtins
// as the CLI's names in --tools and --allowedTools, the MCP servers in
// the CLI's --mcp-config format under RunDir — and the run's events
// come back with bare tool names, its Final with the answer, usage,
// cost and the split the CLI reported.
func TestRunSubagentSpellsTheRequestTheCLIWay(t *testing.T) {
	runDir := t.TempDir()
	var rows []provider.UsageEvent
	res := runSubagentFake(t, "subagent", subagentTestRequest(runDir), func(ev provider.UsageEvent) error {
		rows = append(rows, ev)
		return nil
	})
	events, final, err, args := res.events, res.final, res.err, res.args
	if err != nil {
		t.Fatalf("Err = %v, want a clean run", err)
	}

	for flag, want := range map[string]string{
		"--tools":         "WebFetch,WebSearch",
		"--allowedTools":  "mcp__kivali__file_view,mcp__kivali__run_shell,mcp__kivali__subagent,WebFetch,WebSearch",
		"--model":         "claude-haiku-4-5",
		"--effort":        "low",
		"--system-prompt": "You are a subagent.",
		"--mcp-config":    filepath.Join(runDir, "mcp-config.json"),
		"--output-format": "stream-json",
		"--input-format":  "stream-json",
	} {
		if got := extractFlag(args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	if extractFlag(args, "--resume") != "" || !slices.Contains(args, "-p") {
		t.Errorf("args = %q, want -p and no --resume (a subagent run is one-shot)", args)
	}

	body, rerr := os.ReadFile(filepath.Join(runDir, "mcp-config.json"))
	if rerr != nil {
		t.Fatalf("read mcp-config: %v", rerr)
	}
	var cfg mcpServersConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("decode mcp-config: %v", err)
	}
	kivali, ok := cfg.McpServers["kivali"]
	if len(cfg.McpServers) != 1 || !ok {
		t.Fatalf("mcp-config servers = %v, want exactly kivali", cfg.McpServers)
	}
	if kivali.Command != "/usr/local/bin/kivali" || strings.Join(kivali.Args, " ") != "mcp --toolkit subagent --depth 1" {
		t.Errorf("kivali server = %+v, want the request's command and args", kivali)
	}

	var kinds []provider.StreamEventKind
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if strings.HasPrefix(ev.ToolName, "mcp__") {
			t.Errorf("event %s carries the CLI's tool name %q; the driver strips it", ev.Kind, ev.ToolName)
		}
	}
	wantKinds := []provider.StreamEventKind{
		provider.StreamDelta, provider.StreamToolUseStart, provider.StreamToolUseEnd, provider.StreamToolResult, provider.StreamDelta, provider.StreamEnd,
	}
	if !slices.Equal(kinds, wantKinds) {
		t.Errorf("event kinds = %v, want %v", kinds, wantKinds)
	}

	if final.StopReason != provider.StopEndTurn || final.Error != "" {
		t.Errorf("stop/error = %q/%q, want end_turn and no error", final.StopReason, final.Error)
	}
	if final.Model != "claude-haiku-4-5" || final.CostUSD != 0.0042 {
		t.Errorf("model/cost = %q/%v", final.Model, final.CostUSD)
	}
	if want := fakeSubagentCallA.Add(fakeSubagentCallB); final.Usage != want {
		t.Errorf("Usage = %+v, want each call once at the result frame's total %+v", final.Usage, want)
	}
	if final.ByModel != nil {
		t.Errorf("ByModel = %+v, want nil for a one-model run", final.ByModel)
	}
	var tail string
	for _, b := range final.Content {
		if b.Type == provider.ContentToolUse && b.ToolName != "file_view" {
			t.Errorf("content tool name = %q, want file_view", b.ToolName)
		}
		if b.Type == provider.ContentText {
			tail = b.Text
		}
	}
	if tail != fakeSubagentAnswer {
		t.Errorf("last text block = %q, want the answer", tail)
	}
	if len(rows) != 1 || rows[0].Purpose != "subagent" || rows[0].Agent != "alice" || rows[0].Model != "claude-haiku-4-5" {
		t.Errorf("usage rows = %+v, want one subagent row for alice", rows)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("caller's RunDir removed by the driver: %v", err)
	}
}

// TestRunSubagentReportsTheTypedError: the CLI's close-out message
// under a sentinel model becomes StreamError + Final{StopError, Error},
// never a delta, and its billed tokens stay on the model that answered.
func TestRunSubagentReportsTheTypedError(t *testing.T) {
	res := runSubagentFake(t, "subagent-error", subagentTestRequest(t.TempDir()), nil)
	events, final, err := res.events, res.final, res.err
	if !errors.Is(err, provider.ErrSubprocessExited) {
		t.Errorf("Err = %v, want the non-zero exit reported", err)
	}
	var errTexts, deltas []string
	for _, ev := range events {
		switch ev.Kind {
		case provider.StreamError:
			errTexts = append(errTexts, ev.Text)
		case provider.StreamDelta:
			deltas = append(deltas, ev.Text)
			if isSentinelModel(ev.Model) {
				t.Errorf("delta under sentinel model %q", ev.Model)
			}
		}
	}
	if !slices.Equal(errTexts, []string{fakeSubagentError}) {
		t.Errorf("StreamError texts = %q, want the API error once", errTexts)
	}
	if !slices.Equal(deltas, []string{"Checking the rig."}) {
		t.Errorf("deltas = %q, want only the model's own text", deltas)
	}
	if final.StopReason != provider.StopError || final.Error != fakeSubagentError {
		t.Errorf("Final stop/error = %q/%q, want error/%q", final.StopReason, final.Error, fakeSubagentError)
	}
	for _, b := range final.Content {
		if b.Text == fakeSubagentError {
			t.Error("error text landed in Content; it is not the agent speaking")
		}
	}
	if final.Model != "claude-haiku-4-5" {
		t.Errorf("Final.Model = %q, want the answering model, not the sentinel", final.Model)
	}
	for _, mu := range final.ByModel {
		if isSentinelModel(mu.Model) {
			t.Errorf("ByModel carries the sentinel bucket: %+v", final.ByModel)
		}
	}
	if final.Usage.CacheReadTokens != fakeSubagentCallA.CacheReadTokens+500 {
		t.Errorf("CacheReadTokens = %d, want the close-out call's tokens counted", final.Usage.CacheReadTokens)
	}
}

// TestRunSubagentCancel: cancelling the run's context ends it; the
// stream closes with an error and keeps what streamed before.
func TestRunSubagentCancel(t *testing.T) {
	withFakeCLIEnv(t, "subagent-hang")
	c := New(helperOpts(t, "subagent-hang", nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := c.RunSubagent(ctx, subagentTestRequest(t.TempDir()))
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	first := <-s.Events()
	if first.Kind != provider.StreamDelta || first.Text != "starting" {
		t.Fatalf("first event = %+v, want the run's first delta", first)
	}
	cancel()
	for range s.Events() {
	}
	if s.Err() == nil {
		t.Error("Err = nil after cancel; the caller cannot tell a cancelled run from a finished one")
	}
	if got := s.Final(); got == nil || got.Usage.InputTokens != fakeSubagentCallA.InputTokens {
		t.Errorf("Final = %+v, want the partial usage kept", got)
	}
	_ = s.Close()
}

// TestRunSubagentRefusesUnknownBuiltin: a neutral id this driver cannot
// spell is refused, not silently dropped.
func TestRunSubagentRefusesUnknownBuiltin(t *testing.T) {
	c := New(helperOpts(t, "subagent", nil))
	req := subagentTestRequest(t.TempDir())
	req.Tools.Builtins = []string{"code_execution"}
	if _, err := c.RunSubagent(context.Background(), req); err == nil || !strings.Contains(err.Error(), "code_execution") {
		t.Errorf("err = %v, want the unknown builtin named", err)
	}
}

// TestRunSubagentWithoutRunDirCleansUp: with no RunDir the driver keeps
// the run's config in a temp dir of its own and removes it afterwards.
func TestRunSubagentWithoutRunDirCleansUp(t *testing.T) {
	withFakeCLIEnv(t, "subagent")
	opts := helperOpts(t, "subagent", nil)
	c := New(opts)
	s, err := c.RunSubagent(context.Background(), subagentTestRequest(""))
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()
	left, _ := filepath.Glob(filepath.Join(opts.SessionDir, "subagent-*"))
	if len(left) != 0 {
		t.Errorf("driver-made run dirs left behind: %v", left)
	}
}
