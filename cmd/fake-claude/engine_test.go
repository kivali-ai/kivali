package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/provider"
)

// harness runs one engine over pipes. Pauses never elapse on their own
// (the timer seam returns a channel nothing sends on), so every wait in
// a scenario ends only on what the test writes: no sleeps, no races
// with a wall clock.
type harness struct {
	t      *testing.T
	stdin  *io.PipeWriter
	frames chan map[string]any
	done   chan struct{}
}

func startEngine(t *testing.T, cfg config) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	e := newEngine(cfg, inR, bufio.NewWriter(outW), nil)
	e.newTimer = func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	h := &harness{t: t, stdin: inW, frames: make(chan map[string]any, 256), done: make(chan struct{})}
	go func() {
		e.run()
		_ = outW.Close()
		close(h.done)
	}()
	go func() {
		defer close(h.frames)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var f map[string]any
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				t.Errorf("fake wrote a line that is not JSON: %q", sc.Text())
				continue
			}
			h.frames <- f
		}
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

func (h *harness) write(v map[string]any) {
	h.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := h.stdin.Write(append(b, '\n')); err != nil {
		h.t.Fatalf("write stdin: %v", err)
	}
}

func (h *harness) user(uuid, text string) {
	h.write(map[string]any{
		"type": "user", "uuid": uuid,
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}},
	})
}

func (h *harness) interrupt(id string, cancelQueued bool) {
	h.write(map[string]any{
		"type": "control_request", "request_id": id,
		"request": map[string]any{"subtype": "interrupt", "cancel_queued": cancelQueued},
	})
}

func (h *harness) next() map[string]any {
	h.t.Helper()
	f, ok := <-h.frames
	if !ok {
		h.t.Fatal("fake closed stdout early")
	}
	return f
}

// until reads frames up to and including the first one match accepts,
// returning all of them.
func (h *harness) until(match func(map[string]any) bool) []map[string]any {
	h.t.Helper()
	var got []map[string]any
	for {
		f := h.next()
		got = append(got, f)
		if match(f) {
			return got
		}
	}
}

func isType(typ string) func(map[string]any) bool {
	return func(f map[string]any) bool { return f["type"] == typ }
}

func isText(text string) func(map[string]any) bool {
	return func(f map[string]any) bool { return blockOf(f)["text"] == text }
}

// blockOf returns the single content block of an assistant or user frame.
func blockOf(f map[string]any) map[string]any {
	m, _ := f["message"].(map[string]any)
	c, _ := m["content"].([]any)
	if len(c) != 1 {
		return nil
	}
	b, _ := c[0].(map[string]any)
	return b
}

func ledger(f map[string]any) []string {
	var out []string
	for _, v := range f["user_message_uuids"].([]any) {
		out = append(out, v.(string))
	}
	return out
}

func TestReplyFraming(t *testing.T) {
	h := startEngine(t, config{scenario: scenarioReply, model: "claude-opus-5-5"})
	h.user("u1", "How is the mail switch going?")
	frames := h.until(isType("result"))
	_ = h.stdin.Close()
	<-h.done

	var kinds []string
	var text strings.Builder
	for _, f := range frames {
		k, _ := f["type"].(string)
		if b := blockOf(f); b != nil {
			k += "/" + b["type"].(string)
			if b["type"] == "text" {
				text.WriteString(b["text"].(string))
			}
		}
		kinds = append(kinds, k)
	}
	want := []string{
		"command_lifecycle", "system",
		"assistant/thinking", "assistant/text", "assistant/text", "assistant/text",
		"assistant/tool_use", "user/tool_result", "assistant/text", "result",
	}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("frames = %v\nwant     %v", kinds, want)
	}
	if got := text.String(); !strings.HasPrefix(got, "I'll check the reminders today.") {
		t.Errorf("text = %q", got)
	}
	init := frames[1]
	if init["subtype"] != "init" || init["session_id"] == "" {
		t.Errorf("init = %v", init)
	}
	caps := init["capabilities"].([]any)
	if len(caps) != 3 {
		t.Errorf("capabilities = %v", caps)
	}
	res := frames[len(frames)-1]
	if res["is_error"] != nil || res["result"] != replyFinal {
		t.Errorf("result = %v", res)
	}
	if got := ledger(res); len(got) != 1 || got[0] != "u1" {
		t.Errorf("ledger = %v", got)
	}
	u := res["usage"].(map[string]any)
	if u["output_tokens"].(float64) != 2*float64(callUsage.Output) {
		t.Errorf("usage = %v", u)
	}
	if _, ok := res["modelUsage"].(map[string]any)["claude-opus-5-5"]; !ok {
		t.Errorf("modelUsage = %v", res["modelUsage"])
	}
	// The tool_use and its result pair up by id.
	if blockOf(frames[6])["id"] != blockOf(frames[7])["tool_use_id"] {
		t.Errorf("tool pair ids differ: %v / %v", frames[6], frames[7])
	}
}

func TestSlowHonoursInterrupt(t *testing.T) {
	h := startEngine(t, config{scenario: scenarioReply, model: "claude-opus-5-5", pause: time.Hour})
	h.user("u1", "Status please [fake:slow]")
	h.until(isText(replyDelta1))
	h.interrupt("int_1", true)
	ack := h.next()
	resp := ack["response"].(map[string]any)
	if ack["type"] != "control_response" || resp["subtype"] != "success" || resp["request_id"] != "int_1" {
		t.Fatalf("ack = %v", ack)
	}
	res := h.next()
	if res["type"] != "result" || res["subtype"] != "error_during_execution" || res["is_error"] != true {
		t.Fatalf("result = %v", res)
	}
	// The process stays warm: the next message runs a normal turn.
	h.user("u2", "again")
	res2 := h.until(isType("result"))
	if last := res2[len(res2)-1]; last["result"] != replyFinal {
		t.Errorf("second turn result = %v", last)
	}
}

func TestFoldLandsAtToolSeam(t *testing.T) {
	// The harness's timers never fire, so a nonzero foldWait holds the
	// turn after the first delta until f1 arrives.
	h := startEngine(t, config{scenario: scenarioFold, model: "claude-sonnet-5", foldWait: time.Hour})
	h.user("t1", "Check the reminders")
	h.until(isText(replyDelta1))
	h.write(map[string]any{
		"type": "user", "uuid": "f1", "parent_tool_use_id": nil,
		"message": map[string]any{"role": "user", "content": "also check the bounces"},
	})
	frames := h.until(isType("result"))
	var states []string
	acked := false
	for _, f := range frames {
		if f["type"] == "command_lifecycle" && f["command_uuid"] == "f1" {
			states = append(states, f["state"].(string))
		}
		if b := blockOf(f); b != nil && b["type"] == "text" && strings.Contains(b["text"].(string), "also check the bounces") {
			acked = true
		}
	}
	if strings.Join(states, ",") != "queued,started" {
		t.Errorf("lifecycle for f1 = %v", states)
	}
	if !acked {
		t.Error("the folded message was not acknowledged in the reply")
	}
	if got := ledger(frames[len(frames)-1]); strings.Join(got, ",") != "t1,f1" {
		t.Errorf("ledger = %v", got)
	}
}

func TestFoldHandedBackOnCancelQueued(t *testing.T) {
	h := startEngine(t, config{scenario: scenarioReply, model: "claude-sonnet-5", pause: time.Hour})
	h.user("t1", "Check the reminders [fake:slow]")
	h.until(isText(replyDelta1))
	h.write(map[string]any{
		"type": "user", "uuid": "f1",
		"message": map[string]any{"role": "user", "content": "one more thing"},
	})
	h.until(func(f map[string]any) bool { return f["type"] == "command_lifecycle" && f["state"] == "queued" })
	h.interrupt("int_1", true)
	ack := h.next()
	inner := ack["response"].(map[string]any)["response"].(map[string]any)
	if c := inner["cancelled"].([]any); len(c) != 1 || c[0] != "f1" {
		t.Fatalf("cancelled = %v", inner)
	}
	res := h.next()
	if res["subtype"] != "error_during_execution" {
		t.Fatalf("result = %v", res)
	}
	if got := ledger(res); strings.Join(got, ",") != "t1" {
		t.Errorf("ledger = %v", got)
	}
	// Handed back means gone: stdin closing ends the process without
	// a continuation turn for f1.
	_ = h.stdin.Close()
	for f := range h.frames {
		t.Errorf("unexpected frame after hand-back: %v", f)
	}
}

// A message that reaches the CLI after the turn's result is the fold
// that missed: the CLI starts a turn of its own for it, reporting
// "started" first, which is what the runner absorbs into the open turn.
func TestLateMessageRunsItsOwnTurn(t *testing.T) {
	h := startEngine(t, config{scenario: scenarioReply, model: "claude-sonnet-5"})
	h.user("t1", "Check the reminders")
	h.until(isType("result"))
	h.write(map[string]any{"type": "user", "uuid": "f1", "message": map[string]any{"role": "user", "content": "late"}})
	frames := h.until(isType("result"))
	if frames[0]["type"] != "command_lifecycle" || frames[0]["command_uuid"] != "f1" || frames[0]["state"] != "started" {
		t.Errorf("first frame = %v", frames[0])
	}
	if got := ledger(frames[len(frames)-1]); strings.Join(got, ",") != "f1" {
		t.Errorf("continuation ledger = %v", got)
	}
}

func TestErrorScenario(t *testing.T) {
	h := startEngine(t, config{scenario: scenarioError, model: "claude-opus-5-5"})
	h.user("u1", "Re-send the bounced reminders")
	frames := h.until(isType("result"))
	synthetic := false
	for _, f := range frames {
		if m, ok := f["message"].(map[string]any); ok && m["model"] == "<synthetic>" {
			synthetic = true
		}
	}
	if !synthetic {
		t.Error("no <synthetic> close-out message")
	}
	res := frames[len(frames)-1]
	if res["is_error"] != true || res["result"] != nil {
		t.Errorf("result = %v (want is_error with no result text, the runner's stop=error shape)", res)
	}
}

func TestSubagentWithoutMCPReportsToolError(t *testing.T) {
	h := startEngine(t, config{scenario: scenarioSubagent, model: "claude-opus-5-5"})
	h.user("u1", "Summarise the log")
	frames := h.until(isType("result"))
	var result map[string]any
	for _, f := range frames {
		if b := blockOf(f); b != nil && b["type"] == "tool_result" {
			result = b
		}
	}
	if result == nil || result["is_error"] != true {
		t.Errorf("tool_result = %v", result)
	}
}

func TestUtilityCallsGetPlainAnswers(t *testing.T) {
	h := startEngine(t, config{
		scenario: scenarioSlow, model: "claude-haiku-4-5",
		systemPrompt: "You are summarizing project files",
	})
	h.user("", "files...")
	frames := h.until(isType("result"))
	if len(frames) != 3 || blockOf(frames[1])["text"] != utilityFiles {
		t.Errorf("frames = %v", frames)
	}
}

// A fresh process is handed the chat since its last session in one
// prompt; only the newest message's tag picks the scenario.
func TestScenarioFollowsTheNewestMessage(t *testing.T) {
	block := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	raw, _ := json.Marshal(map[string]any{"role": "user", "content": []map[string]any{
		block("Walk me through it [fake:slow]"),
		block("Update from the runtime at this wake. Nothing here needs a reply."),
		block("[User pressed Stop.]"),
		block("Just the summary then."),
		block("Update from the runtime at this wake. Nothing here needs a reply."),
	}})
	in := inbound{Type: "user", Message: raw}
	if got := in.newest(); got != "Just the summary then." {
		t.Fatalf("newest = %q", got)
	}
	e := &engine{cfg: config{scenario: scenarioReply}}
	if got := e.scenarioFor(in.newest()); got != scenarioReply {
		t.Errorf("scenario = %s, want reply", got)
	}
	if got := e.scenarioFor("Check [fake:error] again [fake:fold]"); got != scenarioFold {
		t.Errorf("last tag should win, got %s", got)
	}
	// A message that is only a tag is the person's, not a runtime marker.
	raw, _ = json.Marshal(map[string]any{"role": "user", "content": []map[string]any{
		block("Walk me through it"),
		block("[fake:slow]"),
		block("[User pressed Stop.]"),
	}})
	in = inbound{Type: "user", Message: raw}
	if got := e.scenarioFor(in.newest()); got != scenarioSlow {
		t.Errorf("a tag-only message: scenario = %s, want slow", got)
	}
}

// The utility calls are recognised by the opening words of core's
// fixed system prompts. Those prompts live unexported in internal/web,
// so this reads the source: a reworded prompt fails here rather than
// silently falling through to the chat scenarios in the e2e suite.
func TestUtilityPromptsMatchCore(t *testing.T) {
	for file, prefix := range map[string]string{
		"project_summary.go": "You are summarizing project files",
		"episode_writer.go":  "You are writing the episode record",
	} {
		src, err := os.ReadFile(filepath.Join("..", "..", "internal", "web", file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), "= `"+prefix) {
			t.Errorf("internal/web/%s no longer opens a system prompt with %q; update utilityReply", file, prefix)
		}
		if _, ok := utilityReply(prefix + " ..."); !ok {
			t.Errorf("utilityReply does not recognise %q", prefix)
		}
	}
}

func TestParseArgsModels(t *testing.T) {
	env := func(string) string { return "" }
	base := []string{"-p", "--tools", "WebFetch,WebSearch", "--output-format", "stream-json", "--input-format", "stream-json",
		"--verbose", "--mcp-config", "/tmp/x.json", "--strict-mcp-config", "--effort", "high", "--system-prompt", "--model not a flag",
		"--allowedTools", "mcp__kivali__file_view", "--some-future-flag"}
	for _, m := range append(claudeagent.New(claudeagent.Options{}).Models(), provider.ModelInfo{ID: "claude-opus-4-8"}) { // every picker row and a retired one
		for _, id := range []string{m.ID, m.ID + "[1m]", m.ID + "-20260101"} {
			cfg, err := parseArgs(append(append([]string{}, base...), "--model", id), env)
			if err != nil {
				t.Errorf("--model %s rejected: %v", id, err)
				continue
			}
			if cfg.model != id || cfg.mcpConfig != "/tmp/x.json" || cfg.systemPrompt != "--model not a flag" {
				t.Errorf("cfg = %+v", cfg)
			}
		}
	}
	if _, err := parseArgs([]string{"--model", "claude-nonsense-9"}, env); err == nil {
		t.Error("an unknown model was accepted")
	}
	if _, err := parseArgs(nil, func(k string) string {
		if k == "FAKE_CLAUDE_SCENARIO" {
			return "nope"
		}
		return ""
	}); err == nil {
		t.Error("an unknown scenario was accepted")
	}
}
