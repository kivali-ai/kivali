package claudeagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kivali-ai/kivali/internal/claudeauth"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
)

// TestMain doubles the test binary as a fake `claude` CLI when
// GO_FAKE_CLAUDE=1. The runner's exec.LookPath happily resolves an
// absolute path to a regular file, so tests set Options.ClaudeBinary
// to os.Args[0] and the subprocess re-enters this TestMain, takes the
// fake-CLI branch, and never runs real tests.
//
// The fake CLI's behavior is driven by GO_FAKE_CLAUDE_SCENARIO.
func TestMain(m *testing.M) {
	if os.Getenv("GO_FAKE_CLAUDE") == "1" {
		runFakeCLI(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runFakeCLI imitates `claude -p --input-format stream-json
// --output-format stream-json` for a few scripted scenarios. It
// reads NDJSON user releases off stdin and emits NDJSON events on
// stdout in response. Stays alive until stdin EOF, then exits 0.
//
// Scenarios (env GO_FAKE_CLAUDE_SCENARIO):
//
//	"echo"            — for each turn, emit assistant{text} + result
//	"tooluse"         — for each turn, emit assistant{tool_use} + user{tool_result} + result
//	"crash-after-1"   — emit response to first turn, then exit 1
//	"hang"            — read one turn but never emit response
//	"ignore-stdin"    — emit init, sleep forever, never read stdin
//	"crash-on-resume" — exit 1 immediately if --resume was passed
//	                    (simulates a corrupt session log); behaves
//	                    like "echo" otherwise
//	"fold-landed"     — advertises msg_lifecycle_v1, reaches a
//	                    tool_result seam, then folds the next stdin
//	                    user message into the SAME turn and names it
//	                    in the result's user_message_uuids
//	"fold-missed"     — same up to the seam, but ends the turn without
//	                    the folded message and then does what the real
//	                    CLI does when a turn has no further tool round:
//	                    reports "started" behind the result and answers
//	                    the message in a continuation turn of its own
//	"fold-orphaned"   — the same miss, but the continuation NEVER
//	                    comes: the CLI silently swallows the queued
//	                    message. Drives the absorb deadline.
func runFakeCLI(args []string) {
	// `auth status --json`, which the runner registry's sign-in check
	// runs before a turn: a fixed subscription sign-in, outside every
	// scenario and every capture file.
	if strings.Join(args, " ") == strings.Join(claudeauth.Args(), " ") {
		fmt.Println(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"fake@example.com"}`)
		os.Exit(0)
	}
	scenario := os.Getenv("GO_FAKE_CLAUDE_SCENARIO")
	if scenario == "" {
		scenario = "echo"
	}

	// Record this spawn's full arg vector (one JSON array per line) when
	// a capture file is configured. Lets tests assert exactly what flags
	// each (re)spawn carried — e.g. that a respawn passed the edited
	// --system-prompt and a --resume. JSON-encoded so arg values
	// containing newlines (the system prompt joins blocks on "\n\n")
	// survive the line-delimited file round-trip.
	if af := os.Getenv("GO_FAKE_CLAUDE_ARGS_FILE"); af != "" {
		if f, err := os.OpenFile(af, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			line, _ := json.Marshal(args)
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
	// Record this spawn's environment, one NAME=value per line, when a
	// capture file is configured. Lets tests assert what the CLI
	// inherits — the deployment's credential reaches it this way.
	if ef := os.Getenv("GO_FAKE_CLAUDE_ENV_FILE"); ef != "" {
		_ = os.WriteFile(ef, []byte(strings.Join(os.Environ(), "\n")+"\n"), 0o644)
	}

	resumeID := extractFlag(args, "--resume")
	sessionID := resumeID
	if sessionID == "" {
		sessionID = "fake-session-" + scenario
	}

	// crash-on-resume: if --resume was passed, exit 1 fast.
	// Otherwise behave like echo. Used to test the fast-death
	// session-id-clear recovery path.
	if scenario == "crash-on-resume" && resumeID != "" {
		os.Exit(1)
	}

	// ignore-stdin: emit nothing, never read stdin. Used to test the
	// stdin write timeout — the kernel pipe buffer fills, then our
	// write blocks until the runner times out and closes. Block on a
	// signal we never raise; signal.Notify keeps Go's runtime busy
	// so the deadlock detector stays quiet, and the test SIGKILLs us
	// via runner.Close.
	if scenario == "ignore-stdin" {
		blockUntilSignal()
		os.Exit(0)
	}

	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()

	emit := func(payload map[string]any) {
		body, _ := json.Marshal(payload)
		_, _ = out.Write(body)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}

	// Initial system event carries the session_id. Real CLI does
	// this once at startup.
	initEvent := map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
		"message":    map[string]any{"model": "claude-fake-1"},
	}
	// Only the fold scenarios advertise the capability, so the other
	// scenarios double as the "CLI too old to fold" case that
	// sendFold must refuse rather than guess at.
	if strings.HasPrefix(scenario, "fold") {
		initEvent["capabilities"] = []string{"msg_lifecycle_v1", "interrupt_cancel_queued_v1"}
	}
	emit(initEvent)

	turn := 0
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	if scenario == "interrupt-honouring" {
		fakeInterruptHonouringLoop(scanner, emit)
		return
	}

	if strings.HasPrefix(scenario, "fold") {
		fakeFoldLoop(scanner, emit, scenario)
		return
	}

	for scanner.Scan() {
		turn++
		// Loosely validate input is a user release, but don't error.
		_ = scanner.Bytes()

		switch scenario {
		case "echo", "crash-on-resume":
			emit(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role":  "assistant",
					"model": "claude-fake-1",
					"content": []map[string]any{
						{"type": "text", "text": fmt.Sprintf("turn %d reply", turn)},
					},
				},
			})
			emit(map[string]any{"type": "result", "result": "ok", "total_cost_usd": 0.0})

		case "tooluse", "tooluse-stuck":
			emit(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role":  "assistant",
					"model": "claude-fake-1",
					"content": []map[string]any{
						{
							"type":  "tool_use",
							"id":    fmt.Sprintf("toolu_t%d", turn),
							"name":  "mcp__kivali__file_view",
							"input": map[string]any{"path": "/files/test.md", "turn": turn},
						},
					},
				},
			})
			emit(map[string]any{
				"type": "user",
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{
						{
							"type":        "tool_result",
							"tool_use_id": fmt.Sprintf("toolu_t%d", turn),
							"content":     []map[string]any{{"type": "text", "text": "view ok"}},
							"is_error":    false,
						},
					},
				},
			})
			if scenario == "tooluse-stuck" {
				// Mid-tool-loop simulation: do NOT emit "result" — the
				// real CLI would re-prompt the model and emit more
				// tool_use events. The fake just blocks here, so the
				// only way the turn ends is via SIGTERM.
				blockUntilSignal()
			}
			emit(map[string]any{"type": "result", "result": "ok", "total_cost_usd": 0.0})

		case "usage-gauge":
			fakeUsageGaugeTurn(emit, turn, resumeID != "")

		case "crash-after-1":
			emit(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role":  "assistant",
					"model": "claude-fake-1",
					"content": []map[string]any{
						{"type": "text", "text": "first reply"},
					},
				},
			})
			emit(map[string]any{"type": "result", "result": "ok"})
			os.Exit(1)

		case "hang":
			// Read one turn, never respond. Caller should close stdin
			// after grace timeout and (eventually) SIGKILL.
			blockUntilSignal()

		case "subagent", "subagent-error", "subagent-hang":
			fakeSubagentRun(emit, scenario)
		}
	}
}

// fakeInterruptHonouringLoop models the real CLI's behaviour around
// the interrupt control channel, as measured against CLI 2.1.273:
//
//   - A user message opens a turn: the fake announces one tool_use,
//     returns its tool_result, and then leaves the turn OPEN (no
//     terminal result), the way the real CLI does while it is still
//     looping over tool calls.
//   - A control_request{subtype:interrupt} is answered with a
//     control_response{subtype:success} and then a terminal result of
//     subtype "error_during_execution" — and the process STAYS ALIVE,
//     ready to serve the next turn. That last part is the whole
//     behaviour under test.
//
// Unlike the generic scenario loop this dispatches on the parsed line
// type rather than treating every line as a new turn, because the
// control channel deliberately shares stdin with the turn channel.
// fakeFoldLoop scripts the two fold outcomes deterministically, with
// no sleeps: it blocks on stdin at the tool_result seam, so the test
// decides when the fold happens by calling SendFold from inside its
// event drain.
// fakeResultCostUSD is what the fold fake prices one result frame at,
// so a test can tell one billed CLI invocation from two.
const fakeResultCostUSD = 0.25

// fakeFoldFrame1Usage / fakeFoldFrame2Usage are the two result frames'
// own token totals in the fold-missed scenario. Unlike total_cost_usd,
// a frame's usage is NOT cumulative (measured against CLI 2.1.281), so
// the absorbed turn's usage is their sum.
var (
	fakeFoldFrame1Usage = provider.TokenUsage{InputTokens: 10, OutputTokens: 100, CacheReadTokens: 1000, CacheCreateTokens: 5}
	fakeFoldFrame2Usage = provider.TokenUsage{InputTokens: 20, OutputTokens: 200, CacheReadTokens: 2000, CacheCreateTokens: 6}
)

// The "usage-gauge" scenario bills two calls per turn, one per model:
// fakeUsageModelOne answers a tool_use call, fakeUsageModelTwo the
// closing text. Both are streamed the way the real CLI streams them —
// one event per content block, each repeating the call's
// start-of-message snapshot (prompt counts final, output_tokens 1) —
// and the result frame carries the turn's own total plus the
// session-cumulative modelUsage gauge. A --resume'd fake starts that
// gauge fakeGaugeHistoryTurns turns in, as the real CLI reloads the
// session's history into it.
var (
	fakeUsageModelOne = provider.TokenUsage{InputTokens: 10, OutputTokens: 100, CacheReadTokens: 100, CacheCreateTokens: 5}
	fakeUsageModelTwo = provider.TokenUsage{InputTokens: 20, OutputTokens: 200, CacheReadTokens: 200, CacheCreateTokens: 6}
)

const fakeGaugeHistoryTurns = 5

func fakeUsageSnapshot(u provider.TokenUsage) map[string]any {
	return map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               1,
		"cache_read_input_tokens":     u.CacheReadTokens,
		"cache_creation_input_tokens": u.CacheCreateTokens,
	}
}

func fakeUsageBlock(u provider.TokenUsage) map[string]any {
	return map[string]any{
		"input_tokens":                u.InputTokens,
		"output_tokens":               u.OutputTokens,
		"cache_read_input_tokens":     u.CacheReadTokens,
		"cache_creation_input_tokens": u.CacheCreateTokens,
	}
}

func fakeModelUsageEntry(u provider.TokenUsage, times int) map[string]any {
	return map[string]any{
		"inputTokens":              u.InputTokens * times,
		"outputTokens":             u.OutputTokens * times,
		"cacheReadInputTokens":     u.CacheReadTokens * times,
		"cacheCreationInputTokens": u.CacheCreateTokens * times,
		"costUSD":                  0.1 * float64(times),
	}
}

func fakeUsageGaugeTurn(emit func(map[string]any), turn int, resumed bool) {
	callA := fmt.Sprintf("msg_t%d_a", turn)
	callB := fmt.Sprintf("msg_t%d_b", turn)
	toolID := fmt.Sprintf("toolu_g%d", turn)
	assistant := func(id, model string, block map[string]any, snap provider.TokenUsage) {
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id":      id,
				"role":    "assistant",
				"model":   model,
				"content": []map[string]any{block},
				"usage":   fakeUsageSnapshot(snap),
			},
		})
	}
	assistant(callA, "claude-fake-1", map[string]any{"type": "thinking", "thinking": ""}, fakeUsageModelOne)
	assistant(callA, "claude-fake-1", map[string]any{
		"type": "tool_use", "id": toolID, "name": "mcp__kivali__file_view",
		"input": map[string]any{"path": "/files/test.md"},
	}, fakeUsageModelOne)
	emit(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type": "tool_result", "tool_use_id": toolID,
				"content": []map[string]any{{"type": "text", "text": "view ok"}},
			}},
		},
	})
	assistant(callB, "claude-fake-2", map[string]any{"type": "thinking", "thinking": ""}, fakeUsageModelTwo)
	assistant(callB, "claude-fake-2", map[string]any{"type": "text", "text": fmt.Sprintf("turn %d reply", turn)}, fakeUsageModelTwo)

	cumulative := turn
	if resumed {
		cumulative += fakeGaugeHistoryTurns
	}
	emit(map[string]any{
		"type":           "result",
		"result":         "ok",
		"total_cost_usd": 0.2 * float64(cumulative),
		"usage":          fakeUsageBlock(fakeUsageModelOne.Add(fakeUsageModelTwo)),
		"modelUsage": map[string]any{
			"claude-fake-1": fakeModelUsageEntry(fakeUsageModelOne, cumulative),
			"claude-fake-2": fakeModelUsageEntry(fakeUsageModelTwo, cumulative),
		},
	})
}

func fakeFoldLoop(scanner *bufio.Scanner, emit func(map[string]any), scenario string) {
	uuidOf := func(line []byte) string {
		var in struct {
			UUID string `json:"uuid"`
		}
		_ = json.Unmarshal(line, &in)
		return in.UUID
	}

	// Turn 1's release.
	if !scanner.Scan() {
		return
	}
	turnUUID := uuidOf(scanner.Bytes())

	// A tool round, which is both the fold seam the real CLI needs and
	// the event the test arms on.
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":  "assistant",
			"model": "claude-fake-1",
			"content": []map[string]any{{
				"type":  "tool_use",
				"id":    "toolu_fold1",
				"name":  "mcp__kivali__file_view",
				"input": map[string]any{"path": "/files/test.md"},
			}},
		},
	})
	emit(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": "toolu_fold1",
				"content":     []map[string]any{{"type": "text", "text": "view ok"}},
				"is_error":    false,
			}},
		},
	})

	// Block until the fold arrives. Blocking is what makes this
	// deterministic — no timing assumption about when SendFold runs.
	if !scanner.Scan() {
		emit(map[string]any{"type": "result", "result": "ok", "user_message_uuids": []string{turnUUID}})
		return
	}
	foldUUID := uuidOf(scanner.Bytes())

	lifecycle := func(state string) {
		emit(map[string]any{"type": "command_lifecycle", "command_uuid": foldUUID, "state": state})
	}

	switch scenario {
	case "fold-landed":
		lifecycle("queued")
		lifecycle("started")
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"model":   "claude-fake-1",
				"content": []map[string]any{{"type": "text", "text": "folded in"}},
			},
		})
		emit(map[string]any{
			"type":               "result",
			"result":             "ok",
			"user_message_uuids": []string{turnUUID, foldUUID},
		})
	case "fold-missed":
		lifecycle("queued")
		// The turn ends WITHOUT the folded message — the real shape
		// when no further tool round happens. Its "started" then
		// arrives just behind the result, and the CLI answers the
		// message in a continuation turn of its own. Frame order and
		// content are copied from a trace of a turn with no tool
		// round against CLI 2.1.273.
		emit(map[string]any{
			"type":               "result",
			"result":             "ok",
			"total_cost_usd":     fakeResultCostUSD,
			"usage":              fakeUsageBlock(fakeFoldFrame1Usage),
			"modelUsage":         map[string]any{"claude-fake-1": fakeModelUsageEntry(fakeFoldFrame1Usage, 1)},
			"user_message_uuids": []string{turnUUID},
		})
		lifecycle("started")
		emit(map[string]any{
			"type":    "system",
			"subtype": "init",
			"capabilities": []string{
				"interrupt_receipt_v1", "interrupt_cancel_queued_v1", "msg_lifecycle_v1",
			},
		})
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"model":   "claude-fake-1",
				"content": []map[string]any{{"type": "text", "text": "answered in my own turn"}},
			},
		})
		// total_cost_usd is the CLI's session-cumulative gauge: the
		// second frame carries the first invocation's spend inside it
		// (measured against 2.1.278). So is modelUsage; usage is not
		// — it is this frame's own total (measured against 2.1.281).
		emit(map[string]any{
			"type":           "result",
			"result":         "ok",
			"total_cost_usd": 2 * fakeResultCostUSD,
			"usage":          fakeUsageBlock(fakeFoldFrame2Usage),
			"modelUsage": map[string]any{
				"claude-fake-1": fakeModelUsageEntry(fakeFoldFrame1Usage.Add(fakeFoldFrame2Usage), 1),
			},
			"user_message_uuids": []string{foldUUID},
		})
	case "fold-orphaned":
		lifecycle("queued")
		// The miss, with no continuation behind it ever. Not measured
		// behaviour — the guard against it.
		emit(map[string]any{
			"type":               "result",
			"result":             "ok",
			"user_message_uuids": []string{turnUUID},
		})
	}

	// Stay alive after the scripted turn, exactly as the real CLI
	// does: folding must not end the subprocess, and a test that
	// asserts that needs the process to still be here. Exits on stdin
	// EOF, i.e. when the runner closes.
	for scanner.Scan() {
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"model":   "claude-fake-1",
				"content": []map[string]any{{"type": "text", "text": "later turn"}},
			},
		})
		emit(map[string]any{"type": "result", "result": "ok"})
	}
}

func fakeInterruptHonouringLoop(scanner *bufio.Scanner, emit func(map[string]any)) {
	turn := 0
	for scanner.Scan() {
		var in struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &in); err != nil {
			continue
		}
		switch in.Type {
		case "user":
			turn++
			emit(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"role":  "assistant",
					"model": "claude-fake-1",
					"content": []map[string]any{{
						"type":  "tool_use",
						"id":    fmt.Sprintf("toolu_i%d", turn),
						"name":  "mcp__kivali__file_view",
						"input": map[string]any{"path": "/files/test.md"},
					}},
				},
			})
			emit(map[string]any{
				"type": "user",
				"message": map[string]any{
					"role": "user",
					"content": []map[string]any{{
						"type":        "tool_result",
						"tool_use_id": fmt.Sprintf("toolu_i%d", turn),
						"content":     []map[string]any{{"type": "text", "text": "view ok"}},
						"is_error":    false,
					}},
				},
			})
			// Turn intentionally left open.

		case "control_request":
			if in.Request.Subtype != "interrupt" {
				continue
			}
			emit(map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "success",
					"request_id": in.RequestID,
					"response":   map[string]any{"still_queued": []any{}},
				},
			})
			emit(map[string]any{
				"type":    "result",
				"subtype": "error_during_execution",
			})
		}
	}
}

// blockUntilSignal parks the calling goroutine forever waiting on a
// signal that will never be raised. signal.Notify wires up the Go
// runtime's signal handler, so the deadlock detector doesn't trip
// even when this is the only goroutine doing anything. The test
// will SIGKILL the fake CLI subprocess via runner.Close.
//
// We deliberately don't use time.Sleep here — sleeps in test
// infrastructure encode "wait long enough that this almost certainly
// happens" which is precisely the kind of debt we're avoiding.
func blockUntilSignal() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR2)
	<-c
}

// extractFlag returns the value of --flag from args, or "" if absent.
func extractFlag(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}

// helperBinary returns the absolute path of the test binary, used as
// the fake claude CLI in tests.
func helperBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	return abs
}

// helperOpts builds Options that route through the fake CLI for the
// given scenario.
func helperOpts(t *testing.T, scenario string, sessions SessionStore) Options {
	t.Helper()
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	return Options{
		ClaudeBinary: helperBinary(t),
		KivaliBinary: helperBinary(t), // any executable works; CLI just spawns it
		DataDir:      t.TempDir(),
		SessionDir:   sessionDir,
		Sessions:     sessions,
	}
}

// withFakeCLIEnv exports the scenario env var so the test subprocess
// becomes the fake CLI. Restores prior value when the test exits.
func withFakeCLIEnv(t *testing.T, scenario string) {
	t.Helper()
	t.Setenv("GO_FAKE_CLAUDE", "1")
	t.Setenv("GO_FAKE_CLAUDE_SCENARIO", scenario)
}

// drainTurn pulls every event off s until events closes. Returns the
// collected events and the final response.
func drainTurn(t *testing.T, s provider.Stream) ([]provider.StreamEvent, *provider.CompleteResponse) {
	t.Helper()
	var events []provider.StreamEvent
	for ev := range s.Events() {
		events = append(events, ev)
	}
	return events, s.Final()
}

// ─── Tests ─────────────────────────────────────────────────────────

// TestRunnerMultiTurnPersistsSubprocess verifies the core promise of
// persistent mode: one CLI subprocess handles two consecutive turns.
// If this fails, the persistence model is broken and we're still in
// spawn-per-call land.
func TestRunnerMultiTurnPersistsSubprocess(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	ctx := context.Background()
	req := provider.CompleteRequest{
		Agent: "test-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "first"}}},
		},
	}

	// Turn 1
	s1, err := c.Stream(ctx, req)
	if err != nil {
		t.Fatalf("turn 1: stream: %v", err)
	}
	_, final1 := drainTurn(t, s1)
	if err := s1.Err(); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if got := finalText(final1); got != "turn 1 reply" {
		t.Errorf("turn 1: final text = %q, want %q", got, "turn 1 reply")
	}
	_ = s1.Close()

	// Capture the runner — same one should service turn 2.
	c.registry.mu.Lock()
	r1 := c.registry.runners["test-agent"]
	c.registry.mu.Unlock()
	if r1 == nil {
		t.Fatalf("no runner registered for test-agent after turn 1")
	}
	if r1.isDead() {
		t.Fatalf("runner died unexpectedly between turns")
	}
	pid1 := r1.cmd.Process.Pid

	// Turn 2 — same agent, same subprocess.
	req.Messages[0].Content[0].Text = "second"
	s2, err := c.Stream(ctx, req)
	if err != nil {
		t.Fatalf("turn 2: stream: %v", err)
	}
	_, final2 := drainTurn(t, s2)
	if err := s2.Err(); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if got := finalText(final2); got != "turn 2 reply" {
		t.Errorf("turn 2: final text = %q, want %q", got, "turn 2 reply")
	}
	_ = s2.Close()

	c.registry.mu.Lock()
	r2 := c.registry.runners["test-agent"]
	c.registry.mu.Unlock()
	if r2 == nil || r2.cmd.Process.Pid != pid1 {
		t.Errorf("turn 2 used a different subprocess (want pid %d, got %v)", pid1, r2)
	}
}

// TestRunnerToolUseEmitsFullInput: Claude Code's stream-json never
// emits tool_input_delta — full
// tool inputs arrive at tool_use_end. Persistent mode must preserve
// this; downstream chip rendering relies on Input being populated
// at the END event.
func TestRunnerToolUseEmitsFullInput(t *testing.T) {
	withFakeCLIEnv(t, "tooluse")
	store := &memSessionStore{}
	opts := helperOpts(t, "tooluse", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "tool-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "use a tool"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	var sawStart, sawEnd, sawResult bool
	var endInput json.RawMessage
	for ev := range s.Events() {
		switch ev.Kind {
		case provider.StreamToolUseStart:
			sawStart = true
		case provider.StreamToolUseEnd:
			sawEnd = true
			endInput = ev.ToolInput
		case provider.StreamToolResult:
			sawResult = true
		case provider.StreamToolInputDelta:
			t.Errorf("unexpected StreamToolInputDelta — Claude Code stream-json should not emit per-token tool input deltas")
		}
	}
	if !sawStart || !sawEnd || !sawResult {
		t.Fatalf("missing events: start=%v end=%v result=%v", sawStart, sawEnd, sawResult)
	}
	if len(endInput) == 0 {
		t.Fatal("StreamToolUseEnd carried empty ToolInput — chip rendering would have nothing to display")
	}
	// Decode and assert the input round-tripped intact (it was
	// {"path":"/files/test.md","turn":1} on the wire).
	var decoded map[string]any
	if err := json.Unmarshal(endInput, &decoded); err != nil {
		t.Fatalf("decode ToolInput: %v\nraw=%s", err, endInput)
	}
	if decoded["path"] != "/files/test.md" {
		t.Errorf("ToolInput.path = %v, want /files/test.md", decoded["path"])
	}
}

// TestRunnerCrashRecoveryRespawns verifies that when a runner's
// subprocess exits unexpectedly mid-life, the next Acquire detects
// the corpse, evicts it, and spawns a fresh runner.
func TestRunnerCrashRecoveryRespawns(t *testing.T) {
	withFakeCLIEnv(t, "crash-after-1")
	store := &memSessionStore{}
	opts := helperOpts(t, "crash-after-1", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "crash-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	s1, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// Capture the runner BEFORE draining; we need its dead chan to
	// synchronize on the supervisor firing.
	c.registry.mu.Lock()
	r := c.registry.runners["crash-agent"]
	c.registry.mu.Unlock()
	if r == nil {
		t.Fatal("no runner registered for crash-agent")
	}
	_, _ = drainTurn(t, s1)
	_ = s1.Close()

	// Block on the runner's dead channel — closes after the
	// supervisor's cmd.Wait returns. Deterministic; no polling.
	<-r.dead

	// A second turn should detect the corpse, evict, and spawn
	// fresh. To verify, switch the scenario so the new subprocess
	// behaves differently.
	t.Setenv("GO_FAKE_CLAUDE_SCENARIO", "echo")

	s2, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 2 (post-crash): %v", err)
	}
	_, final2 := drainTurn(t, s2)
	if err := s2.Err(); err != nil {
		t.Fatalf("turn 2 err: %v", err)
	}
	if got := finalText(final2); got != "turn 1 reply" {
		// New subprocess starts at turn 1 again, hence "turn 1 reply".
		t.Errorf("post-crash turn: final text = %q, want %q (fresh subprocess restarts turn counter)", got, "turn 1 reply")
	}
	_ = s2.Close()
}

// TestRegistryGrowsUnbounded verifies that adding more agent slugs
// just adds more warm runners — there is no cap. If a future change
// reintroduces an LRU pool, this test is the canary.
func TestRegistryGrowsUnbounded(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	runOneTurn := func(slug string) {
		req := provider.CompleteRequest{
			Agent: slug,
			Messages: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
			},
		}
		s, err := c.Stream(context.Background(), req)
		if err != nil {
			t.Fatalf("stream %s: %v", slug, err)
		}
		drainTurn(t, s)
		_ = s.Close()
	}

	for _, slug := range []string{"a", "b", "c", "d", "e"} {
		runOneTurn(slug)
	}

	c.registry.mu.Lock()
	got := len(c.registry.runners)
	c.registry.mu.Unlock()
	if got != 5 {
		t.Errorf("after 5 unique slugs, want 5 warm runners, got %d (a cap snuck in?)", got)
	}
}

// TestRegistryRespawnsOnConfigDrift verifies the immediate-effect path
// for the per-chat Model/Effort selectors: a turn whose effort differs
// from the live runner's spawn effort evicts + respawns (so the new
// flag takes effect now, not on the next chat), while a same-config
// turn reuses the warm runner. The respawn carries --resume so the
// conversation isn't lost.
func TestRegistryRespawnsOnConfigDrift(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	runTurn := func(effort string) *runner {
		req := provider.CompleteRequest{
			Agent:  "drifter",
			Model:  "claude-opus-4-8",
			Effort: effort,
			Messages: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
			},
		}
		s, err := c.Stream(context.Background(), req)
		if err != nil {
			t.Fatalf("stream (effort=%q): %v", effort, err)
		}
		drainTurn(t, s)
		_ = s.Close()
		c.registry.mu.Lock()
		r := c.registry.runners["drifter"]
		c.registry.mu.Unlock()
		return r
	}

	// Two turns at the same effort reuse the same runner.
	r1 := runTurn("high")
	r2 := runTurn("high")
	if r1 == nil || r2 == nil {
		t.Fatal("no runner registered")
	}
	if r1 != r2 {
		t.Errorf("same-config turn respawned the runner (r1=%p r2=%p); want reuse", r1, r2)
	}

	// Changing effort evicts + respawns: a different live runner, and
	// the old one is closed (dead).
	r3 := runTurn("max")
	if r3 == r1 {
		t.Errorf("config drift did NOT respawn — still the original runner %p", r1)
	}
	if r3 == nil {
		t.Fatal("no runner after drift respawn")
	}
	if !r1.isDead() {
		t.Error("old runner was not closed after config-drift eviction")
	}
	if r3.spawnEffort != "max" {
		t.Errorf("respawned runner effort = %q, want max", r3.spawnEffort)
	}
}

// TestConfigMatchesSystemPrompt unit-tests the drift predicate for the
// system prompt alone (handbook / role / agent_memory all flow into
// it). Same model+effort, edited system prompt → drift; identical
// content rebuilt fresh each turn → no drift (so we don't respawn on
// every turn just because core re-assembles the prompt).
func TestConfigMatchesSystemPrompt(t *testing.T) {
	base := provider.CompleteRequest{
		Model:  "claude-opus-4-8",
		Effort: "high",
		System: []provider.SystemBlock{{Text: "handbook v1"}, {Text: "role v1"}},
	}
	r := &runner{
		spawnModel:      base.Model,
		spawnEffort:     normalizeEffort(base.Effort),
		spawnSystemHash: systemPromptHash(base),
	}

	if !r.configMatches(base) {
		t.Fatal("identical request should match")
	}

	// Same model/effort, but the role block was edited → drift.
	edited := provider.CompleteRequest{
		Model:  "claude-opus-4-8",
		Effort: "high",
		System: []provider.SystemBlock{{Text: "handbook v1"}, {Text: "role v2 EDITED"}},
	}
	if r.configMatches(edited) {
		t.Error("edited system prompt should be detected as config drift")
	}

	// Identical content, freshly rebuilt blocks (new slice/strings) →
	// must still match, or every turn would spuriously respawn.
	rebuilt := provider.CompleteRequest{
		Model:  "claude-opus-4-8",
		Effort: "high",
		System: []provider.SystemBlock{{Text: "handbook v1"}, {Text: "role v1"}},
	}
	if !r.configMatches(rebuilt) {
		t.Error("identical content rebuilt fresh should not look like drift")
	}
}

// readSpawnArgs parses the fake CLI's args-capture file into one
// []string arg-vector per spawn, in spawn order.
func readSpawnArgs(t *testing.T, path string) [][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spawn-args file: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("decode spawn-args line %q: %v", line, err)
		}
		out = append(out, argv)
	}
	return out
}

// TestSpawnInheritsEnv locks in that the spawn inherits this process's
// environment unchanged: HOME (where the CLI keeps its sign-in)
// and anything else the CLI reads arrive as the server has them.
// Stripping it would leave agents on a different sign-in than the
// server's `claude auth status` reports.
func TestSpawnInheritsEnv(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	envFile := filepath.Join(t.TempDir(), "spawn-env.log")
	t.Setenv("GO_FAKE_CLAUDE_ENV_FILE", envFile)
	t.Setenv("KIVALI_SPAWN_ENV_PROBE", "inherited")

	c := New(helperOpts(t, "echo", &memSessionStore{}))
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()
	req := provider.CompleteRequest{
		Agent:  "keyed",
		Model:  "claude-opus-4-8",
		Effort: "high",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()

	assertProbeInherited := func(path string) {
		t.Helper()
		raw, err := os.ReadFile(envFile)
		if err != nil {
			t.Fatalf("%s: the fake CLI did not record its environment: %v", path, err)
		}
		if !strings.Contains("\n"+string(raw), "\nKIVALI_SPAWN_ENV_PROBE=inherited\n") {
			t.Errorf("%s: spawned CLI did not inherit KIVALI_SPAWN_ENV_PROBE; env was:\n%s", path, raw)
		}
	}
	assertProbeInherited("warm runner")

	// The per-call path (no agent slug: summaries and one-offs) spawns
	// separately and must inherit the same environment.
	if err := os.Remove(envFile); err != nil {
		t.Fatalf("reset env capture: %v", err)
	}
	req.Agent = ""
	s, err = c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("per-call stream: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()
	assertProbeInherited("per-call")
}

// TestRegistryRespawnsOnSystemPromptDrift is the end-to-end guarantee
// for live handbook/role/memory edits: editing the system prompt
// mid-conversation respawns the runner so the CLI actually sees the new
// prompt, while --resume preserves the conversation. Same prompt across
// turns must reuse the runner (no churn).
func TestRegistryRespawnsOnSystemPromptDrift(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	argsFile := filepath.Join(t.TempDir(), "spawn-args.log")
	t.Setenv("GO_FAKE_CLAUDE_ARGS_FILE", argsFile)

	// Pre-seed a session id + its on-disk log so resolveResumeSessionID's
	// probe passes and the respawn keeps the session (passes --resume),
	// matching production where the conversation is preserved across the
	// drift respawn.
	store := &memSessionStore{}
	_ = store.WriteClaudeSessionID("drifter", "drift-sess-id")
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "drift-sess-id.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	opts := helperOpts(t, "echo", store)
	opts.HomeDir = home

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	runTurn := func(sys string) *runner {
		req := provider.CompleteRequest{
			Agent:  "drifter",
			Model:  "claude-opus-4-8",
			Effort: "high",
			System: []provider.SystemBlock{{Text: sys}},
			Messages: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
			},
		}
		s, err := c.Stream(context.Background(), req)
		if err != nil {
			t.Fatalf("stream (sys=%q): %v", sys, err)
		}
		drainTurn(t, s)
		_ = s.Close()
		c.registry.mu.Lock()
		r := c.registry.runners["drifter"]
		c.registry.mu.Unlock()
		return r
	}

	// Same system prompt across two turns reuses the runner.
	r1 := runTurn("handbook v1\n\nrole v1")
	r2 := runTurn("handbook v1\n\nrole v1")
	if r1 == nil || r2 == nil {
		t.Fatal("no runner registered")
	}
	if r1 != r2 {
		t.Errorf("same system prompt respawned the runner (r1=%p r2=%p); want reuse", r1, r2)
	}

	// Editing the system prompt evicts + respawns; the old runner dies.
	r3 := runTurn("handbook v1\n\nrole v2 EDITED")
	if r3 == r1 {
		t.Error("system-prompt drift did NOT respawn the runner")
	}
	if r3 == nil {
		t.Fatal("no runner after system-prompt drift respawn")
	}
	if !r1.isDead() {
		t.Error("old runner was not closed after system-prompt drift eviction")
	}

	// End-to-end: the respawn must carry the EDITED --system-prompt to
	// the CLI, and pass --resume so the conversation is preserved.
	spawns := readSpawnArgs(t, argsFile)
	if len(spawns) < 2 {
		t.Fatalf("expected >=2 CLI spawns, got %d", len(spawns))
	}
	first, last := spawns[0], spawns[len(spawns)-1]
	if got, want := extractFlag(first, "--system-prompt"), "handbook v1\n\nrole v1"; got != want {
		t.Errorf("first spawn --system-prompt = %q, want %q", got, want)
	}
	if got, want := extractFlag(last, "--system-prompt"), "handbook v1\n\nrole v2 EDITED"; got != want {
		t.Errorf("respawn --system-prompt = %q, want %q", got, want)
	}
	if extractFlag(last, "--resume") == "" {
		t.Error("respawn did not pass --resume; conversation context would be lost")
	}
}

// TestRegistryCloseDrainsAll verifies graceful shutdown: every live
// runner gets Close()'d when the registry is closed.
func TestRegistryCloseDrainsAll(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	for _, slug := range []string{"x", "y", "z"} {
		req := provider.CompleteRequest{
			Agent: slug,
			Messages: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
			},
		}
		s, err := c.Stream(context.Background(), req)
		if err != nil {
			t.Fatalf("stream %s: %v", slug, err)
		}
		drainTurn(t, s)
		_ = s.Close()
	}

	c.registry.mu.Lock()
	runners := make([]*runner, 0, len(c.registry.runners))
	for _, r := range c.registry.runners {
		runners = append(runners, r)
	}
	c.registry.mu.Unlock()
	if len(runners) != 3 {
		t.Fatalf("want 3 live runners, got %d", len(runners))
	}

	if err := c.registry.Close(); err != nil {
		t.Fatalf("registry close: %v", err)
	}

	// Every runner should be dead post-Close.
	for _, r := range runners {
		if !r.isDead() {
			t.Errorf("runner[%s] still alive after registry.Close()", r.slug)
		}
	}
}

// TestRegistryShutdownCtxCloses verifies the registry drains when
// the caller-provided ShutdownCtx is canceled (the SIGTERM path).
func TestRegistryShutdownCtxCloses(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)
	ctx, cancel := context.WithCancel(context.Background())
	opts.ShutdownCtx = ctx

	c := New(opts)
	req := provider.CompleteRequest{
		Agent: "ctx-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()

	c.registry.mu.Lock()
	r := c.registry.runners["ctx-agent"]
	c.registry.mu.Unlock()
	if r == nil {
		t.Fatal("no runner")
	}

	cancel()

	// Registry's ctx-watching goroutine triggers Close, which waits
	// for cmd.Wait to return and closes r.dead. Block on the chan
	// directly — no polling, no real-time deadline.
	<-r.dead
}

// TestEmptySlugTakesPerCallPath: callers without an agent slug
// (project-files summarization, anonymous one-offs) bypass the
// runner registry and use the spawn-per-call path. The registry is
// slug-keyed; an empty-slug request has nowhere to go.
//
// We assert by reaching the per-call path's binary-resolution step,
// which fails with a recognizable "not found on PATH" error when the
// binary doesn't exist — and verify the registry was NOT constructed
// (since no slug-bearing call has come in yet).
func TestEmptySlugTakesPerCallPath(t *testing.T) {
	c := New(Options{
		ClaudeBinary: "nonexistent-claude-bin-for-test",
	})
	_, err := c.Stream(context.Background(), provider.CompleteRequest{
		Agent: "", // empty — must take per-call path
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
		},
	})
	if err == nil {
		t.Fatal("expected error from missing binary, got nil")
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("error %q doesn't look like the per-call binary-resolve error", err)
	}
	if c.registry != nil {
		t.Error("registry was constructed for an empty-slug request; should be lazy on the first slug-bearing call")
	}
}

// finalText returns the concatenated text of all text blocks in resp.
// Helper for asserting against echo-scenario replies.
func finalText(resp *provider.CompleteResponse) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range resp.Content {
		if c.Type == provider.ContentText {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// TestRunnerResumesAfterCrash exercises the full crash-recovery
// path: a stored session id with a matching on-disk log file, the
// runner crashes, the next Acquire spawns fresh AND passes --resume
// with the persisted id (because the log still exists).
func TestRunnerResumesAfterCrash(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	_ = store.WriteClaudeSessionID("resume-agent", "stored-sess-id")

	// Pre-create the on-disk log so resolveResumeSessionID's probe
	// passes and we keep the stored session id.
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(projDir, "stored-sess-id.jsonl")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	opts := helperOpts(t, "echo", store)
	opts.HomeDir = home

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	// Turn 1: spawn should pass --resume stored-sess-id.
	req := provider.CompleteRequest{
		Agent: "resume-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()

	c.registry.mu.Lock()
	r := c.registry.runners["resume-agent"]
	c.registry.mu.Unlock()
	if r == nil {
		t.Fatal("no runner registered")
	}
	if r.resumeID != "stored-sess-id" {
		t.Errorf("runner.resumeID = %q, want %q (--resume not honored)", r.resumeID, "stored-sess-id")
	}
	// Confirm cmd.Args includes "--resume stored-sess-id".
	args := strings.Join(r.cmd.Args, " ")
	if !strings.Contains(args, "--resume stored-sess-id") {
		t.Errorf("cmd.Args = %q, want it to contain --resume stored-sess-id", args)
	}
}

// TestRunnerFastDeathClearsSessionAfterResume verifies the M4 fix:
// when a runner spawned with --resume dies inside fastDeathWindow,
// the supervisor clears the stored session id so the next Acquire
// doesn't pass --resume again (otherwise the agent would crash-loop).
func TestRunnerFastDeathClearsSessionAfterResume(t *testing.T) {
	withFakeCLIEnv(t, "crash-on-resume")
	store := &memSessionStore{}
	_ = store.WriteClaudeSessionID("fast-death-agent", "doomed-sess-id")

	// Pre-create the log file so resolveResumeSessionID returns
	// the stored id (and we hit the --resume path).
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(projDir, "doomed-sess-id.jsonl")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	opts := helperOpts(t, "crash-on-resume", store)
	opts.HomeDir = home

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	// Turn 1: spawn passes --resume, fake CLI exits 1 immediately.
	// We expect Acquire to succeed (newRunner returns the runner
	// before the supervisor fires), then RunTurn to fail because
	// the runner is dead by the time we try to write.
	req := provider.CompleteRequest{
		Agent: "fast-death-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	s, streamErr := c.Stream(context.Background(), req)
	// Acquire registered the runner before c.Stream returned (in
	// either a success or error path), so it's in the map now.
	c.registry.mu.Lock()
	r := c.registry.runners["fast-death-agent"]
	c.registry.mu.Unlock()
	if r == nil {
		t.Fatalf("no runner registered after Stream call (streamErr=%v)", streamErr)
	}

	// Supervisor closes r.dead AFTER clearing the session id.
	// Block on the chan — deterministic, no polling.
	<-r.dead

	if s != nil {
		_, _ = drainTurn(t, s)
		_ = s.Close()
	}

	if stored, _ := store.ReadClaudeSessionID("fast-death-agent"); stored != "" {
		t.Fatalf("session id should have been cleared after fast-death; still %q", stored)
	}

	// Turn 2 should now spawn fresh (no --resume) — switch the
	// scenario so we don't crash again.
	t.Setenv("GO_FAKE_CLAUDE_SCENARIO", "echo")

	s2, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 2 (post-clear): %v", err)
	}
	drainTurn(t, s2)
	if err := s2.Err(); err != nil {
		t.Fatalf("turn 2 err: %v", err)
	}
	_ = s2.Close()

	c.registry.mu.Lock()
	r2 := c.registry.runners["fast-death-agent"]
	c.registry.mu.Unlock()
	if r2 == nil {
		t.Fatal("no runner after recovery turn")
	}
	if r2.resumeID != "" {
		t.Errorf("runner.resumeID = %q, want empty (session id should have been cleared and respawn should start fresh)", r2.resumeID)
	}
}

// TestRunnerWriteTimeout verifies the H2 fix: when the CLI is alive
// but doesn't read stdin, the runner-protective timeout fires and
// tears the runner down.
//
// Driven by a clock.Fake — no real time elapses. The test starts the
// Stream call on a goroutine, waits for the runner to register its
// timer with the clock, advances the clock past writeTimeout, and
// asserts the call returns with a timeout error.
func TestRunnerWriteTimeout(t *testing.T) {
	withFakeCLIEnv(t, "ignore-stdin")

	store := &memSessionStore{}
	opts := helperOpts(t, "ignore-stdin", store)
	clk := clock.NewFake()
	opts.clock = clk

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	// Send a request body large enough that the JSON encoding can't
	// fit in the kernel pipe buffer (typical 16-64 KiB on macOS /
	// Linux). With "ignore-stdin" never reading from stdin, the
	// write goroutine blocks on the pipe — exactly the production
	// scenario the timeout exists for.
	bigText := strings.Repeat("A", 256*1024) // 256 KiB
	req := provider.CompleteRequest{
		Agent: "hung-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: bigText}}},
		},
	}

	type result struct {
		stream provider.Stream
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		s, err := c.Stream(context.Background(), req)
		resultCh <- result{s, err}
	}()

	// First: wait for the writeTimeout timer to be registered, then
	// fire it. This makes RunTurn take its timeout branch.
	clk.BlockUntil(1)
	clk.Advance(writeTimeout)

	res := <-resultCh
	if res.err == nil {
		t.Fatal("expected write timeout error, got nil")
	}
	if !strings.Contains(res.err.Error(), "timed out") {
		t.Errorf("error %q does not look like write-timeout error", res.err)
	}

	// Then: RunTurn's timeout branch kicked off `go r.Close()`.
	// That async Close registers a closeGraceTimeout timer with the
	// fake clock. Wait for it, then fire it so Close SIGKILLs the
	// hung subprocess. Without this the deferred registry.Close()
	// blocks indefinitely waiting on the runner's closeOnce.
	clk.BlockUntil(1)
	clk.Advance(closeGraceTimeout)
}

// TestRunnerPerRunnerDirIsolation verifies the H3 fix: two runners
// for different slugs get distinct working directories, so neither
// can stomp on the other's mcp-config or stderr files.
func TestRunnerPerRunnerDirIsolation(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	runOneTurn := func(slug string) {
		req := provider.CompleteRequest{
			Agent: slug,
			Messages: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
			},
		}
		s, err := c.Stream(context.Background(), req)
		if err != nil {
			t.Fatalf("stream %s: %v", slug, err)
		}
		drainTurn(t, s)
		_ = s.Close()
	}

	runOneTurn("alpha")
	runOneTurn("beta")

	c.registry.mu.Lock()
	a := c.registry.runners["alpha"]
	b := c.registry.runners["beta"]
	c.registry.mu.Unlock()
	if a == nil || b == nil {
		t.Fatal("both runners should be live")
	}
	if a.dir == b.dir || a.dir == "" || b.dir == "" {
		t.Errorf("runners share or lack a dir; alpha=%q beta=%q", a.dir, b.dir)
	}
	// Each dir should contain its own mcp-config + stderr + stdin-mirror.
	for _, r := range []*runner{a, b} {
		for _, name := range []string{"mcp-config.json", "stderr.log", "stdin-mirror.jsonl"} {
			path := filepath.Join(r.dir, name)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("runner[%s] missing %s: %v", r.slug, name, err)
			}
		}
	}

	// Verify Close removes the dir.
	dirA := a.dir
	if err := a.Close(); err != nil {
		t.Errorf("close alpha: %v", err)
	}
	if _, err := os.Stat(dirA); !os.IsNotExist(err) {
		t.Errorf("alpha dir %s still exists after Close", dirA)
	}
}

// TestRunnerStdinMirrorCapturesWrites verifies the L2 fix: stdin
// writes are mirrored to the per-runner stdin-mirror.jsonl file for
// post-mortem diagnosis.
func TestRunnerStdinMirrorCapturesWrites(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "mirror-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "MIRRORME"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()

	c.registry.mu.Lock()
	r := c.registry.runners["mirror-agent"]
	c.registry.mu.Unlock()
	if r == nil {
		t.Fatal("no runner")
	}

	mirrorPath := filepath.Join(r.dir, "stdin-mirror.jsonl")
	body, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if !strings.Contains(string(body), "MIRRORME") {
		t.Errorf("stdin mirror missing the user content; got %q", body)
	}
}

// TestRunnerInterruptDeadlineKillsRunner verifies the fallback path
// of the graceful-kill design: when no in-flight tool returns a
// tool_result within gracefulInterruptDeadline (here, the fake CLI
// is hung after stdin), the runner SIGTERMs the subprocess. The
// supervisor's deadErr is wrapped with provider.ErrUserCancelled so
// the chat-loop classifies the failure as cancelled, not as a
// runtime crash.
//
// Driven by a clock.Fake — no real time elapses.
func TestRunnerInterruptDeadlineKillsRunner(t *testing.T) {
	withFakeCLIEnv(t, "hang")
	store := &memSessionStore{}
	opts := helperOpts(t, "hang", store)
	clk := clock.NewFake()
	opts.clock = clk

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	turnCtx, cancel := context.WithCancel(context.Background())
	req := provider.CompleteRequest{
		Agent: "stuck-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	s, err := c.Stream(turnCtx, req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	// Stop click: cancel the per-turn ctx. The watcher arms
	// interrupt and registers the deadline timer.
	cancel()
	clk.BlockUntil(1)
	clk.Advance(gracefulInterruptDeadline)

	// gracefulKill SIGTERMs the fake (default handler kills it),
	// supervisor wakes, fails the active turn with ErrUserCancelled,
	// events channel closes.
	for range s.Events() {
		// drain — we don't care about content, just exit
	}
	if !errors.Is(s.Err(), provider.ErrUserCancelled) {
		t.Errorf("stream.Err() = %v, want wrapping provider.ErrUserCancelled", s.Err())
	}

	// forceKillGrace timer registered by gracefulKill still ticks;
	// fire it so the deferred registry.Close doesn't block on a
	// stuck timer goroutine. (The subprocess is already dead via
	// SIGTERM, so this is just goroutine cleanup.)
	clk.Advance(forceKillGrace)
}

// TestRunnerToolResultKillsRunnerWhenArmed exercises routeEvent's
// post-tool-result kill path directly: with interruptArmed set, a
// "user" event (which carries tool_result blocks in stream-json from
// `claude -p`) triggers gracefulKill. Avoids the timing-sensitive
// orchestration of cancelling between a real CLI's tool_use and
// tool_result emits.
//
// The arming has to happen BEFORE the turn is dispatched, which is why
// this goes through Acquire + RunTurn rather than the one-line
// client.Stream. Acquire spawns the runner and stops; the fake CLI is
// parked on its stdin scanner and cannot have emitted anything yet, so
// arming here is ordered against the tool_result by construction.
//
// Arming after Stream() instead — Stream both spawns AND dispatches —
// is a race the test loses whenever the pipe roundtrip beats the next
// few statements on this goroutine. It is a rare loss on a fast, idle
// machine and a much less rare one on a loaded CI runner, and losing it
// hangs rather than fails: the tool_result goes by unarmed, the fake
// blocks forever with no terminating "result", and the deadline timer
// that would rescue a real runner is on a fake clock this test cannot
// advance until after the drain it is stuck in. That is a ten-minute
// test timeout.
func TestRunnerToolResultKillsRunnerWhenArmed(t *testing.T) {
	withFakeCLIEnv(t, "tooluse-stuck")
	store := &memSessionStore{}
	opts := helperOpts(t, "tooluse-stuck", store)
	clk := clock.NewFake()
	opts.clock = clk

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	// Use a non-cancelled ctx; we'll arm interrupt directly so the
	// tool_result-triggered kill is what fires (not the deadline).
	req := provider.CompleteRequest{
		Agent: "armed-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	reg, err := c.ensureRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	r, err := reg.Acquire(context.Background(), req.Agent, "", req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Arm interrupt directly — simulates "Stop click landed before
	// the tool_use → tool_result roundtrip completed". The
	// "tooluse-stuck" fake emits assistant{tool_use} then
	// user{tool_result} and then BLOCKS (no terminating result).
	// routeEvent's tool_result branch sees interruptArmed → SIGTERM.
	r.armInterrupt()

	// Now dispatch. The fake wakes on this line, emits the tool_use and
	// the tool_result, and by then the flag it is read against has been
	// set for good — armInterrupt latches and nothing clears it.
	s, err := r.RunTurn(context.Background(), req)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}

	// The fake ignores the control_request entirely (it is parked in
	// blockUntilSignal and never reads stdin again), which is exactly
	// the wedged-CLI case the escalation exists for. Advance the fake
	// clock from a second goroutine so the escalation fires while the
	// main goroutine is inside the drain below.
	//
	// BlockUntil rather than a sleep: it waits for the timer to be
	// registered, so the Advance cannot land before the thing it is
	// meant to trigger exists.
	go func() {
		clk.BlockUntil(1)
		clk.Advance(interruptAckDeadline + gracefulInterruptDeadline + forceKillGrace)
	}()

	// Drain events. Escalation SIGTERMs the fake; subprocess exits;
	// supervisor fails the active turn with ErrUserCancelled.
	for range s.Events() {
	}
	if !errors.Is(s.Err(), provider.ErrUserCancelled) {
		t.Errorf("stream.Err() = %v, want wrapping provider.ErrUserCancelled", s.Err())
	}

	// The gentle path must have been tried first: the control_request
	// should be on the wire ahead of any kill.
	mirror, rerr := os.ReadFile(filepath.Join(r.dir, "stdin-mirror.jsonl"))
	if rerr != nil {
		t.Fatalf("read stdin mirror: %v", rerr)
	}
	if !strings.Contains(string(mirror), `"subtype":"interrupt"`) {
		t.Errorf("stdin mirror has no interrupt control_request; gentle path was skipped.\n%s", mirror)
	}
}

// TestRunnerGentleInterruptKeepsSubprocessWarm is the headline test for
// the control_request path: a CLI that honours the interrupt ends the
// turn WITHOUT the process dying, and the same subprocess goes on to
// serve the next turn: no SIGTERM, no cold respawn with --resume.
func TestRunnerGentleInterruptKeepsSubprocessWarm(t *testing.T) {
	withFakeCLIEnv(t, "interrupt-honouring")
	store := &memSessionStore{}
	opts := helperOpts(t, "interrupt-honouring", store)
	clk := clock.NewFake()
	opts.clock = clk

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "warm-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	reg, err := c.ensureRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	r, err := reg.Acquire(context.Background(), req.Agent, "", req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	s, err := r.RunTurn(context.Background(), req)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}

	// Arm on the tool_result event rather than before dispatch. That
	// is the ordering under test — the boundary has been reached, so
	// armInterrupt takes the "no tool in flight" branch and writes the
	// control_request straight away. Doing it from inside the drain
	// keeps it deterministic: no sleep, no clock advance, no race on
	// when the fake gets around to emitting.
	for ev := range s.Events() {
		if ev.Kind == provider.StreamToolResult {
			r.armInterrupt()
		}
	}
	if !errors.Is(s.Err(), provider.ErrUserCancelled) {
		t.Fatalf("stream.Err() = %v, want wrapping provider.ErrUserCancelled", s.Err())
	}

	// The whole point: no SIGTERM, so the subprocess is still there.
	if r.isDead() {
		t.Fatalf("runner died on a gentle interrupt; the control_request path should leave it warm")
	}

	// Per-turn interrupt state must have been cleared, or the next
	// turn inherits the cancel and dies on arrival.
	r.mu.Lock()
	armed, sent, inFlight := r.interruptArmed, r.interruptSent, r.toolsInFlight
	r.mu.Unlock()
	if armed || sent || inFlight != 0 {
		t.Errorf("interrupt state leaked into next turn: armed=%v sent=%v toolsInFlight=%d", armed, sent, inFlight)
	}

	// And the real proof: the SAME subprocess serves another turn.
	s2, err := r.RunTurn(context.Background(), req)
	if err != nil {
		t.Fatalf("second turn on the warm runner: %v", err)
	}
	var sawToolResult bool
	for ev := range s2.Events() {
		if ev.Kind == provider.StreamToolResult {
			sawToolResult = true
			r.armInterrupt()
		}
	}
	if !sawToolResult {
		t.Errorf("second turn produced no tool_result; subprocess did not really survive")
	}
}

// TestArmInterruptDefersUntilToolBoundary is the "don't interrupt a
// command in flight" rule, tested on the state machine directly rather
// than through subprocess timing.
//
// Parallel tool calls are the case that makes counting (rather than a
// bool) load-bearing: with two tools announced, the FIRST tool_result
// must not trigger the interrupt, because the second tool is still
// running and cutting it off is the exact thing we are avoiding.
func TestArmInterruptDefersUntilToolBoundary(t *testing.T) {
	withFakeCLIEnv(t, "hang")
	store := &memSessionStore{}
	opts := helperOpts(t, "hang", store)
	clk := clock.NewFake()
	opts.clock = clk

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "defer-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	reg, err := c.ensureRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	r, err := reg.Acquire(context.Background(), req.Agent, "", req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Dispatch a turn so routeEvent has an active turn to route
	// against — its boundary branch sits behind that check. The
	// "hang" fake reads the prompt and parks without emitting, so the
	// only events this turn ever sees are the synthetic ones below.
	if _, err := r.RunTurn(context.Background(), req); err != nil {
		t.Fatalf("run turn: %v", err)
	}

	mirrorPath := filepath.Join(r.dir, "stdin-mirror.jsonl")
	interruptOnWire := func() bool {
		b, _ := os.ReadFile(mirrorPath)
		return strings.Contains(string(b), `"subtype":"interrupt"`)
	}

	// Two tools announced in one assistant message (parallel calls).
	r.trackToolsInFlight(&streamJSONEvent{
		Type: "assistant",
		Message: json.RawMessage(`{"role":"assistant","content":[
			{"type":"tool_use","id":"a","name":"mcp__kivali__run_shell","input":{}},
			{"type":"tool_use","id":"b","name":"mcp__kivali__run_shell","input":{}}]}`),
	})

	r.armInterrupt()
	if interruptOnWire() {
		t.Fatalf("interrupt sent while two tools were still in flight")
	}

	// First result back — one tool still running, so still no interrupt.
	r.routeEvent(&streamJSONEvent{
		Type:    "user",
		Message: json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":[]}]}`),
	})
	if interruptOnWire() {
		t.Fatalf("interrupt sent after the first of two parallel tool_results; the second tool was still running")
	}

	// Second result — boundary genuinely reached, interrupt goes out.
	r.routeEvent(&streamJSONEvent{
		Type:    "user",
		Message: json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":[]}]}`),
	})
	if !interruptOnWire() {
		b, _ := os.ReadFile(mirrorPath)
		t.Fatalf("no interrupt after the last tool_result; boundary was missed.\n%s", b)
	}

	// Drain the escalation goroutines before Close.
	clk.BlockUntil(1)
	clk.Advance(interruptAckDeadline + gracefulInterruptDeadline + forceKillGrace)
}

// TestRunnerUserCancelPreservesSessionAfterResume: the supervisor
// clears the stored session_id when a --resume-spawned subprocess
// dies inside fastDeathWindow (a corrupt session log), but never for a
// user-initiated Stop (a graceful kill on a healthy log): clearing it
// then would orphan the CLI session log, and the next turn would start
// with a blank conversation history.
//
// This test pre-seeds a session id, simulates a Stop click inside
// fastDeathWindow, and verifies the id is preserved so the next turn
// can --resume from the same conversation.
func TestRunnerUserCancelPreservesSessionAfterResume(t *testing.T) {
	withFakeCLIEnv(t, "hang")
	store := &memSessionStore{}
	const keptID = "kept-sess-id"
	_ = store.WriteClaudeSessionID("stop-agent", keptID)

	// Pre-create the CLI's on-disk session log so
	// resolveResumeSessionID returns the stored id (the existence
	// probe gates whether we pass --resume at all).
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(projDir, keptID+".jsonl")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	opts := helperOpts(t, "hang", store)
	opts.HomeDir = home
	clk := clock.NewFake()
	opts.clock = clk

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	turnCtx, cancel := context.WithCancel(context.Background())
	req := provider.CompleteRequest{
		Agent: "stop-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	s, err := c.Stream(turnCtx, req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	c.registry.mu.Lock()
	r := c.registry.runners["stop-agent"]
	c.registry.mu.Unlock()
	if r == nil {
		t.Fatal("no runner registered")
	}
	// Sanity-check the precondition this test is guarding: the
	// runner WAS spawned with --resume against the stored id.
	if r.resumeID != keptID {
		t.Fatalf("runner.resumeID = %q, want %q (test setup didn't reach the --resume path)", r.resumeID, keptID)
	}

	// Stop click: cancel the per-turn ctx, then advance past
	// gracefulInterruptDeadline so gracefulKill SIGTERMs the fake.
	cancel()
	clk.BlockUntil(1)
	clk.Advance(gracefulInterruptDeadline)

	// Drain events until the subprocess exits and the stream closes.
	for range s.Events() {
	}
	if !errors.Is(s.Err(), provider.ErrUserCancelled) {
		t.Fatalf("stream.Err() = %v, want wrapping provider.ErrUserCancelled", s.Err())
	}

	// Block on r.dead so the supervisor has run its fast-death
	// check before we inspect the session store.
	<-r.dead

	// elapsed at the supervisor was at most gracefulInterruptDeadline (2s)
	// of fake-clock time — well inside fastDeathWindow (10s) — so without
	// the userCancelled guard the supervisor would have cleared the id.
	// With the guard in place, the id must still be there.
	if stored, _ := store.ReadClaudeSessionID("stop-agent"); stored != keptID {
		t.Fatalf("session id after Stop click = %q, want %q preserved (the supervisor's fastDeathWindow path cleared it — Stop must not orphan the CLI session log)", stored, keptID)
	}

	// Cleanup: forceKillGrace timer from gracefulKill is still pending.
	clk.Advance(forceKillGrace)
}

// TestRegistryEvictTearsDownWarmRunner is the unit-level guard for the
// rotation seam: Evict drops the warm runner from the map and kills its
// subprocess, so the next Acquire spawns a fresh one.
func TestRegistryEvictTearsDownWarmRunner(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "evict-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()

	c.registry.mu.Lock()
	r1 := c.registry.runners["evict-agent"]
	c.registry.mu.Unlock()
	if r1 == nil {
		t.Fatal("no runner after turn 1")
	}

	c.registry.Evict("evict-agent")

	// The runner is removed from the map synchronously...
	c.registry.mu.Lock()
	_, stillThere := c.registry.runners["evict-agent"]
	c.registry.mu.Unlock()
	if stillThere {
		t.Error("runner still in registry map immediately after Evict")
	}
	// ...and its subprocess is torn down asynchronously. Block on dead.
	<-r1.dead

	// Evict on an unknown slug is a no-op, not a panic.
	c.registry.Evict("never-existed")
}

// TestRotationEvictForcesFreshSession: rotation resets the CLI session.
//
// In agent-pod mode every agent turn is served by a long-lived
// `claude -p` runner that keeps the entire conversation in the
// subprocess's memory across turns. Chat rotation ("New chat") archives
// the transcript and clears the stored CLI session id, intending the
// next turn to start a clean session. But clearing the id is INERT
// against a warm runner: Acquire returns the live runner without
// re-reading the id (its in-memory session is the source of truth past
// spawn). Without an evict the agent would keep the full pre-rotation
// context: the transcript rotates, the context window does not.
//
// This test shows that clearing the id leaves the SAME live subprocess
// in place, and that evicting the runner forces the next turn to spawn
// fresh with NO --resume.
func TestRotationEvictForcesFreshSession(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	_ = store.WriteClaudeSessionID("rot-agent", "old-sess")

	// On-disk log so resolveResumeSessionID keeps the stored id and the
	// first runner genuinely spawns with --resume old-sess.
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "old-sess.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	opts := helperOpts(t, "echo", store)
	opts.HomeDir = home

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "rot-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "first"}}},
		},
	}

	// Turn 1: warm runner spawns with --resume old-sess.
	s1, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	drainTurn(t, s1)
	_ = s1.Close()

	c.registry.mu.Lock()
	r1 := c.registry.runners["rot-agent"]
	c.registry.mu.Unlock()
	if r1 == nil {
		t.Fatal("no runner after turn 1")
	}
	if r1.resumeID != "old-sess" {
		t.Fatalf("turn 1 runner.resumeID = %q, want old-sess (setup didn't reach the --resume path)", r1.resumeID)
	}
	pid1 := r1.cmd.Process.Pid

	// Rotation finalize, step 1 — clear the stored session id. On its
	// own this is INERT against the warm runner: prove it by acquiring
	// again and observing the SAME live subprocess.
	if err := store.ClearClaudeSessionID("rot-agent"); err != nil {
		t.Fatalf("clear session: %v", err)
	}
	rSame, err := c.registry.Acquire(context.Background(), "rot-agent", c.resolveResumeSessionID("rot-agent"), req)
	if err != nil {
		t.Fatalf("acquire after clear: %v", err)
	}
	if rSame != r1 || rSame.cmd.Process.Pid != pid1 {
		t.Fatalf("clearing the session id changed the runner; the warm runner is supposed to outlive a bare id-clear (got pid %d, want %d)", rSame.cmd.Process.Pid, pid1)
	}
	if rSame.resumeID != "old-sess" {
		t.Errorf("warm runner.resumeID = %q after clear; expected the live runner unchanged (confirms clear-id alone is inert)", rSame.resumeID)
	}

	// Rotation finalize, step 2 — evict: it tears the warm runner down
	// so the next Acquire spawns fresh.
	c.registry.Evict("rot-agent")
	<-r1.dead // Evict closes async; block until the old subprocess is gone.

	// Turn 2: fresh runner, new PID, NO --resume (cleared id → a clean
	// session). This is the rotation actually taking effect on the model.
	s2, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 2 (post-rotation): %v", err)
	}
	drainTurn(t, s2)
	if err := s2.Err(); err != nil {
		t.Fatalf("turn 2 err: %v", err)
	}
	_ = s2.Close()

	c.registry.mu.Lock()
	r2 := c.registry.runners["rot-agent"]
	c.registry.mu.Unlock()
	if r2 == nil {
		t.Fatal("no runner after rotation turn")
	}
	if r2 == r1 || r2.cmd.Process.Pid == pid1 {
		t.Errorf("rotation turn reused the pre-rotation subprocess (pid %d); the agent would still see the full prior conversation", pid1)
	}
	if r2.resumeID != "" {
		t.Errorf("post-rotation runner.resumeID = %q, want empty — a fresh session, not a --resume of the archived chat", r2.resumeID)
	}
	if args := strings.Join(r2.cmd.Args, " "); strings.Contains(args, "--resume") {
		t.Errorf("post-rotation runner spawned with --resume (args=%q); rotation must start a clean session", args)
	}
}

// TestResetSessionEvictsRunner verifies the client-level entry point
// the agent runtime calls on EventSessionReset: ResetSession(slug)
// evicts the warm runner so the next turn spawns fresh.
func TestResetSessionEvictsRunner(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	store := &memSessionStore{}
	opts := helperOpts(t, "echo", store)

	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	req := provider.CompleteRequest{
		Agent: "reset-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	drainTurn(t, s)
	_ = s.Close()

	c.registry.mu.Lock()
	r1 := c.registry.runners["reset-agent"]
	c.registry.mu.Unlock()
	if r1 == nil {
		t.Fatal("no runner after turn 1")
	}

	if err := c.ResetSession("reset-agent"); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	<-r1.dead

	c.registry.mu.Lock()
	_, stillThere := c.registry.runners["reset-agent"]
	c.registry.mu.Unlock()
	if stillThere {
		t.Error("runner still registered after ResetSession")
	}

	// Empty slug is rejected; unknown slug is a no-op.
	if err := c.ResetSession(""); err == nil {
		t.Error("ResetSession(\"\") should error")
	}
	if err := c.ResetSession("never-existed"); err != nil {
		t.Errorf("ResetSession on unknown slug should be a no-op, got %v", err)
	}
}

// (memSessionStore is defined in stream_test.go.)

// TestPersistentRunnerArgsEffort asserts the persistent-runner spawn
// always carries a validated --effort: the request's level when valid,
// and the fleet default (high) when empty or bogus — so the CLI never
// sees a bad flag and every runner pins a known reasoning depth.
// A model id may carry the provider's "claude:" prefix (AGENT_MODEL, a
// stored pin); the CLI is given the bare id, which is all it knows.
func TestPersistentRunnerArgsModelDropsProviderPrefix(t *testing.T) {
	args := persistentRunnerArgs("/tmp/mcp.json", "", provider.CompleteRequest{Agent: "alice", Model: "claude:claude-opus-5-5"})
	if joined := strings.Join(args, " "); !strings.Contains(joined, "--model claude-opus-5-5") || strings.Contains(joined, "claude:") {
		t.Errorf("args = %q, want --model claude-opus-5-5", joined)
	}
}

func TestPersistentRunnerArgsEffort(t *testing.T) {
	cases := []struct {
		name   string
		effort string
		want   string
	}{
		{"empty defaults to high", "", "high"},
		{"explicit max honored", "max", "max"},
		{"bogus falls back to high", "turbo", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := persistentRunnerArgs("/tmp/mcp.json", "", provider.CompleteRequest{
				Agent:  "alice",
				Model:  "claude-opus-4-8",
				Effort: tc.effort,
			})
			joined := strings.Join(args, " ")
			if want := "--effort " + tc.want; !strings.Contains(joined, want) {
				t.Errorf("args = %q, want it to contain %q", joined, want)
			}
		})
	}
}
