package claudeagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// testdata/stream_haiku_two_tools.jsonl is one real `claude -p
// --output-format stream-json --verbose` session, recorded against
// CLI 2.1.281 on 2026-09-23: a prompt that ran two shell commands, so
// the turn is three API calls. Only the system/init frame was
// trimmed (cwd and tool lists); every assistant, user and result line
// is byte-for-byte what the CLI wrote.
//
// What the CLI's own transcript on disk recorded for the same three
// messages, once each, with their FINAL usage:
//
//	msg_011CfMKwXcJcUezAoBMtYH6y  in 10  out 137  cache_read 13689  cache_create 13376
//	msg_011CfMKwgBVkatZq63XE6HDY  in  8  out 101  cache_read 27065  cache_create  1055
//	msg_011CfMKwnW1GahU6xVj7neot  in  8  out  55  cache_read 28120  cache_create   149
//	                              ── 26     293             68874             14580
//
// The stream, by contrast, carries six assistant events for those
// three messages (thinking + tool_use, thinking + tool_use, thinking +
// text), each repeating its message's start-of-message snapshot with
// output_tokens of 8, 2 and 1. Summing events — what the readers did
// before — gave 52 / 11 / 137748 / 29160: double on every prompt-side
// count and a 27x shortfall on output. The result frame's usage is the
// transcript's sum exactly.
const fixtureStream = "stream_haiku_two_tools.jsonl"

// fixtureWant is the transcript's unique-message final usage above —
// what a usage row for this turn must carry.
var fixtureWant = provider.TokenUsage{InputTokens: 26, OutputTokens: 293, CacheReadTokens: 68874, CacheCreateTokens: 14580}

// fixtureSnapshotsOnly is the same turn reduced from the assistant
// events alone (no result frame): each message once, so the
// prompt-side counts already match the transcript, but output is the
// sum of the three start-of-message snapshots.
var fixtureSnapshotsOnly = provider.TokenUsage{InputTokens: 26, OutputTokens: 11, CacheReadTokens: 68874, CacheCreateTokens: 14580}

func loadStreamFixture(t *testing.T, name string) []*streamJSONEvent {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	var events []*streamJSONEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 128*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		ev := new(streamJSONEvent)
		if err := json.Unmarshal(sc.Bytes(), ev); err != nil {
			t.Fatalf("parse fixture line: %v", err)
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return events
}

// TestFixtureShapeIsBlocksNotCalls documents the wire shape turnUsage
// handles, straight off the recording: more assistant events than message
// ids, every event of a message carrying the same snapshot.
func TestFixtureShapeIsBlocksNotCalls(t *testing.T) {
	events := loadStreamFixture(t, fixtureStream)
	ids := map[string]messageUsage{}
	assistant := 0
	for _, ev := range events {
		if ev.Type != "assistant" {
			continue
		}
		assistant++
		var m message
		if err := json.Unmarshal(ev.Message, &m); err != nil {
			t.Fatalf("assistant message: %v", err)
		}
		if m.ID == "" {
			t.Fatal("assistant message without an id; the dedupe key is missing from the recording")
		}
		if prev, seen := ids[m.ID]; seen && prev != m.Usage {
			t.Errorf("message %s: block events disagree on usage: %+v vs %+v", m.ID, prev, m.Usage)
		}
		ids[m.ID] = m.Usage
	}
	if assistant != 6 || len(ids) != 3 {
		t.Fatalf("got %d assistant events over %d message ids, want 6 over 3", assistant, len(ids))
	}
	var result *streamJSONEvent
	for _, ev := range events {
		if ev.Type == "result" {
			result = ev
		}
	}
	if result == nil || result.Usage == nil {
		t.Fatal("recording has no result frame with usage")
	}
	if got := result.Usage.tokens(); got != fixtureWant {
		t.Errorf("result frame usage = %+v, want the transcript's sum %+v", got, fixtureWant)
	}
}

// TestFixtureSubprocStreamCountsEachCallOnce runs the recording through
// the per-call reader. The turn's usage must be the transcript's
// unique-message sum, under one model, with no split.
func TestFixtureSubprocStreamCountsEachCallOnce(t *testing.T) {
	s := newTestSubprocStream("claude-haiku-4-5-20251001")
	s.gauge = newUsageGauge(true) // a fresh session, as the recording was
	for _, ev := range loadStreamFixture(t, fixtureStream) {
		s.handleEvent(ev)
	}
	assertFixtureFinal(t, s.Final())
}

// TestFixtureRunnerTurnCountsEachCallOnce runs the same recording the
// way the runner drives a turn: assistant and user frames through
// handleEvent, the result frame through recordResult with the share
// the runner's gauge recovered.
func TestFixtureRunnerTurnCountsEachCallOnce(t *testing.T) {
	tn := newRunnerTurn(provider.CompleteRequest{Model: "claude-haiku-4-5-20251001"}, nil, nil, "alice", "", nil)
	gauge := newUsageGauge(true)
	for _, ev := range loadStreamFixture(t, fixtureStream) {
		if ev.Type == "result" {
			tn.recordResult(ev, gauge.Delta(ev.gaugeReading()))
			continue
		}
		tn.handleEvent(ev)
	}
	assertFixtureFinal(t, tn.Final())
}

func assertFixtureFinal(t *testing.T, f *provider.CompleteResponse) {
	t.Helper()
	if f.Usage != fixtureWant {
		t.Errorf("Usage = %+v, want %+v (each call once, output from the result frame)", f.Usage, fixtureWant)
	}
	if f.ByModel != nil {
		t.Errorf("ByModel = %+v, want nil: one model answered", f.ByModel)
	}
	if f.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want the answering model", f.Model)
	}
	// The window gauge is the LAST call's prompt: 8 + 28120 + 149.
	if f.ContextTokens != 28277 {
		t.Errorf("ContextTokens = %d, want 28277", f.ContextTokens)
	}
}

// TestFixtureWithoutResultFrameKeepsDedupedSnapshots is the fallback
// when the process dies before the result frame: still one call per
// message, prompt-side counts exact, output the snapshots' floor.
func TestFixtureWithoutResultFrameKeepsDedupedSnapshots(t *testing.T) {
	s := newTestSubprocStream("claude-haiku-4-5-20251001")
	for _, ev := range loadStreamFixture(t, fixtureStream) {
		if ev.Type == "result" {
			continue
		}
		s.handleEvent(ev)
	}
	if got := s.Final().Usage; got != fixtureSnapshotsOnly {
		t.Errorf("Usage without a result frame = %+v, want %+v", got, fixtureSnapshotsOnly)
	}
}

// TestFixtureResumedProcessStillGetsTheTotal: the same recording on a
// process that resumed a session has no gauge baseline, so the split
// is rebuilt from the calls — a single model here, so the total lands
// on it whole and nothing is approximate.
func TestFixtureResumedProcessStillGetsTheTotal(t *testing.T) {
	s := newTestSubprocStream("claude-haiku-4-5-20251001")
	s.gauge = newUsageGauge(false)
	for _, ev := range loadStreamFixture(t, fixtureStream) {
		s.handleEvent(ev)
	}
	assertFixtureFinal(t, s.Final())
}
