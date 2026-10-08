package claudeagent

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestAttributeModel covers the fallback chain: reported id, then the
// last genuine id on the stream, then the id we asked for.
func TestAttributeModel(t *testing.T) {
	tests := []struct {
		name                          string
		reported, lastReal, requested string
		want                          string
	}{
		{"reported wins", "claude-opus-5", "claude-sonnet-5", "claude-sonnet-5", "claude-opus-5"},
		{"sentinel falls back to last real", "<synthetic>", "claude-fable-5-1", "claude-opus-5", "claude-fable-5-1"},
		{"empty falls back to last real", "", "claude-fable-5-1", "claude-opus-5", "claude-fable-5-1"},
		{"sentinel first message falls back to requested", "<synthetic>", "", "claude-opus-5", "claude-opus-5"},
		{"nothing usable yields blank bucket", "<synthetic>", "<synthetic>", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := attributeModel(tc.reported, tc.lastReal, tc.requested); got != tc.want {
				t.Errorf("attributeModel(%q, %q, %q) = %q, want %q",
					tc.reported, tc.lastReal, tc.requested, got, tc.want)
			}
		})
	}
}

// TestSubprocStreamSyntheticKeepsAnsweringModel: a turn that dies on
// an API error ends with an assistant
// message whose model is "<synthetic>". Its tokens are real and were
// billed. If the sentinel wins the latest-wins model assignment, the
// turn's usage row is written under an ID that has no pricing row and
// the whole turn is valued at $0.
func TestSubprocStreamSyntheticKeepsAnsweringModel(t *testing.T) {
	s := newTestSubprocStream("claude-fable-5-1")
	s.model = "claude-fable-5-1" // as the "system" init event would set it

	// A normal answering call, then the synthetic close-out the CLI
	// emits when the turn exceeds the output-token maximum.
	s.handleAssistantMessage(assistantMsg(t, "claude-fable-5-1", 100, 20, 1000, 200))
	s.handleAssistantMessage(assistantMsg(t, "<synthetic>", 0, 0, 27_324_359, 1_660_852))

	final := s.Final()
	if final.Model != "claude-fable-5-1" {
		t.Errorf("Model = %q, want claude-fable-5-1 (sentinel must not win)", final.Model)
	}
	// Both calls' tokens are counted — the synthetic message's usage is
	// re-attributed, not dropped.
	if final.Usage.CacheReadTokens != 27_324_359+1000 {
		t.Errorf("CacheReadTokens = %d, want %d", final.Usage.CacheReadTokens, 27_324_359+1000)
	}
	// One answering model => no split, and the flat {Model, Usage} pair
	// prices the whole turn at Fable 5.1 rates.
	if final.ByModel != nil {
		t.Errorf("ByModel = %+v, want nil (single answering model)", final.ByModel)
	}
	if cost, priced := defaultPricing.CostOK(final.Model, final.Usage); !priced || cost <= 0 {
		t.Errorf("turn priced at (%v, %v), want a real non-zero cost", cost, priced)
	}
}

// TestRunnerTurnSyntheticKeepsAnsweringModel is the same check on the
// warm-runner path, which serves every parent chat turn in production.
func TestRunnerTurnSyntheticKeepsAnsweringModel(t *testing.T) {
	tn := newRunnerTurn(provider.CompleteRequest{Model: "claude-opus-5"}, nil, nil, "alice", "", nil)
	tn.model = "claude-opus-5"

	tn.handleAssistantMessage(assistantMsg(t, "claude-opus-5", 500, 100, 2000, 300))
	tn.handleAssistantMessage(assistantMsg(t, "<synthetic>", 0, 0, 900_000, 0))

	final := tn.Final()
	if final.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5 (sentinel must not win)", final.Model)
	}
	if final.Usage.CacheReadTokens != 902_000 {
		t.Errorf("CacheReadTokens = %d, want 902000", final.Usage.CacheReadTokens)
	}
	if final.ByModel != nil {
		t.Errorf("ByModel = %+v, want nil (single answering model)", final.ByModel)
	}
}

// TestSubprocStreamMixedModelSplitUnaffected guards the neighbouring
// behaviour: a genuinely mixed turn must still split per answering
// model. The sentinel fix narrows what counts as an answering model, so
// it could plausibly collapse a real split.
func TestSubprocStreamMixedModelSplitUnaffected(t *testing.T) {
	s := newTestSubprocStream("claude-opus-5")
	s.model = "claude-opus-5"

	s.handleAssistantMessage(assistantMsg(t, "claude-opus-5", 100, 10, 0, 0))
	s.handleAssistantMessage(assistantMsg(t, "claude-sonnet-5", 200, 20, 0, 0))
	s.handleAssistantMessage(assistantMsg(t, "<synthetic>", 50, 0, 0, 0))

	split := s.Final().ByModel
	if len(split) != 2 {
		t.Fatalf("ByModel = %+v, want 2 buckets (opus, sonnet)", split)
	}
	if split[0].Model != "claude-opus-5" || split[1].Model != "claude-sonnet-5" {
		t.Errorf("bucket models = %q/%q, want claude-opus-5/claude-sonnet-5",
			split[0].Model, split[1].Model)
	}
	// The synthetic call's 50 input tokens land on Sonnet — the model
	// that answered most recently — rather than an unpriced bucket.
	if split[1].Usage.InputTokens != 250 {
		t.Errorf("sonnet InputTokens = %d, want 250 (200 + the synthetic call's 50)",
			split[1].Usage.InputTokens)
	}
	// The split must still sum to the flat total, or a billing number
	// silently disagrees with itself.
	sum := split[0].Usage.InputTokens + split[1].Usage.InputTokens
	if sum != s.Final().Usage.InputTokens {
		t.Errorf("split sums to %d, flat total is %d", sum, s.Final().Usage.InputTokens)
	}
}

// TestRunnerTurnSentinelIsTypedError: on the warm-runner path (every
// parent chat turn) the sentinel close-out message is reported as
// StreamError and StopError with its text in Final.Error — never as a
// delta, never in Content — and a result frame that carries result
// text after it does not turn the error back into end_turn.
func TestRunnerTurnSentinelIsTypedError(t *testing.T) {
	tn := newRunnerTurn(provider.CompleteRequest{Model: "claude-opus-5"}, nil, nil, "alice", "", nil)
	tn.model = "claude-opus-5"

	tn.handleAssistantMessage(assistantMsg(t, "claude-opus-5", 500, 100, 2000, 300))
	tn.handleAssistantMessage(assistantMsg(t, "<synthetic>", 0, 0, 900_000, 0))
	tn.recordResult(&streamJSONEvent{Type: "result", IsError: true, Result: "API Error"}, nil)

	close(tn.events)
	var kinds []provider.StreamEventKind
	var errText string
	for ev := range tn.events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == provider.StreamError {
			errText = ev.Text
		}
	}
	if want := []provider.StreamEventKind{provider.StreamDelta, provider.StreamError}; !slices.Equal(kinds, want) {
		t.Errorf("events = %v, want %v", kinds, want)
	}
	final := tn.Final()
	if final.StopReason != provider.StopError || final.Error != errText || errText != "..." {
		t.Errorf("Final stop/error = %q/%q (event text %q), want error and the close-out text", final.StopReason, final.Error, errText)
	}
	if len(final.Content) != 1 {
		t.Errorf("Content = %+v, want only the model's own text", final.Content)
	}
}

// drainKinds closes events and collects what was emitted.
func drainKinds(ch chan provider.StreamEvent) ([]provider.StreamEventKind, []string) {
	close(ch)
	var kinds []provider.StreamEventKind
	var errs []string
	for ev := range ch {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == provider.StreamError {
			errs = append(errs, ev.Text)
		}
	}
	return kinds, errs
}

// TestSentinelOnCompletedTurnIsDropped: sentinel text on a turn whose
// result frame does not report an error is logged and dropped, as it
// always was — the turn completes normally, in both readers.
func TestSentinelOnCompletedTurnIsDropped(t *testing.T) {
	result := &streamJSONEvent{Type: "result", Subtype: "success", Result: "done"}

	tn := newRunnerTurn(provider.CompleteRequest{Model: "claude-opus-5"}, nil, nil, "alice", "", nil)
	tn.handleAssistantMessage(assistantMsg(t, "claude-opus-5", 5, 1, 0, 0))
	tn.handleAssistantMessage(assistantMsg(t, "<synthetic>", 0, 0, 10, 0))
	tn.recordResult(result, nil)
	if kinds, _ := drainKinds(tn.events); slices.Contains(kinds, provider.StreamError) {
		t.Errorf("runner emitted %v; sentinel text on a completed turn must be dropped", kinds)
	}
	if f := tn.Final(); f.StopReason != provider.StopEndTurn || f.Error != "" {
		t.Errorf("runner stop/error = %q/%q, want end_turn and none", f.StopReason, f.Error)
	}

	s := newTestSubprocStream("claude-opus-5")
	s.handleAssistantMessage(assistantMsg(t, "claude-opus-5", 5, 1, 0, 0))
	s.handleAssistantMessage(assistantMsg(t, "<synthetic>", 0, 0, 10, 0))
	s.handleEvent(result)
	if kinds, _ := drainKinds(s.events); slices.Contains(kinds, provider.StreamError) {
		t.Errorf("stream emitted %v; sentinel text on a completed turn must be dropped", kinds)
	}
	if f := s.Final(); f.StopReason != provider.StopEndTurn || f.Error != "" {
		t.Errorf("stream stop/error = %q/%q, want end_turn and none", f.StopReason, f.Error)
	}
}

// TestErrorResultWithoutSentinelHasDetail: an is_error frame with no
// sentinel text (error_during_execution) still reports why, from the
// frame, rather than leaving Final.Error empty.
func TestErrorResultWithoutSentinelHasDetail(t *testing.T) {
	tn := newRunnerTurn(provider.CompleteRequest{Model: "claude-opus-5"}, nil, nil, "alice", "", nil)
	tn.recordResult(&streamJSONEvent{Type: "result", Subtype: "error_during_execution", IsError: true}, nil)
	f := tn.Final()
	if f.StopReason != provider.StopError || !strings.Contains(f.Error, "error_during_execution") {
		t.Errorf("stop/error = %q/%q, want error naming the subtype", f.StopReason, f.Error)
	}
}

// TestSettleExit: held sentinel text with no result frame is the error
// when the process failed, dropped when it exited cleanly; a StopError
// with no detail takes the exit's text.
func TestSettleExit(t *testing.T) {
	exitErr := errors.New("exit status 1")
	if out := settleExit("", "", "API Error", exitErr, "t"); out.stopReason != provider.StopError || out.detail != "API Error" || out.emit != "API Error" {
		t.Errorf("failed exit: %+v", out)
	}
	if out := settleExit("", "", "API Error", nil, "t"); out.stopReason != "" || out.emit != "" {
		t.Errorf("clean exit: %+v, want dropped", out)
	}
	if out := settleExit(provider.StopError, "", "", exitErr, "t"); out.detail != "exit status 1" {
		t.Errorf("detail-less error: %+v, want the exit text", out)
	}
}

// TestQuoteLine: a malformed stream-json line is named by its type when
// that decodes, else by its first 80 characters.
func TestQuoteLine(t *testing.T) {
	if got := quoteLine([]byte(`{"type":"assistant","message":7}`)); got != `of type "assistant"` {
		t.Errorf("typed line = %s", got)
	}
	long := strings.Repeat("x", 200)
	if got := quoteLine([]byte(long)); !strings.HasPrefix(got, `"`+strings.Repeat("x", 80)+`"`) {
		t.Errorf("long line = %s, want the first 80 characters", got)
	}
}

// TestResultStopReason pins the stop reasons a result frame yields.
func TestResultStopReason(t *testing.T) {
	cases := []struct {
		cur  string
		ev   streamJSONEvent
		want string
	}{
		{"", streamJSONEvent{Result: "done"}, provider.StopEndTurn},
		{"", streamJSONEvent{IsError: true}, provider.StopError},
		{"", streamJSONEvent{}, ""},
		{provider.StopError, streamJSONEvent{Result: "API Error"}, provider.StopError},
	}
	for _, tc := range cases {
		if got := resultStopReason(tc.cur, &tc.ev); got != tc.want {
			t.Errorf("resultStopReason(%q, %+v) = %q, want %q", tc.cur, tc.ev, got, tc.want)
		}
	}
}

// newTestSubprocStream builds a subprocStream with no subprocess behind
// it, for driving handleAssistantMessage directly.
//
// The channels must be real: emit() selects on events/done, and a nil
// channel blocks forever on both arms. The events buffer is sized well
// past what these tests emit so nothing has to drain it concurrently —
// the assertions all read accumulated state via Final(), not the event
// stream.
func newTestSubprocStream(model string) *subprocStream {
	return &subprocStream{
		req:    provider.CompleteRequest{Model: model},
		events: make(chan provider.StreamEvent, 64),
		done:   make(chan struct{}),
	}
}

// assistantMsg builds one stream-json assistant message with the given
// model id and usage — the shape Claude Code emits per /v1/messages
// call.
func assistantMsg(t *testing.T, model string, in, out, cacheRead, cacheCreate int) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"role":    "assistant",
		"model":   model,
		"content": []map[string]any{{"type": "text", "text": "..."}},
		"usage": map[string]any{
			"input_tokens":                in,
			"output_tokens":               out,
			"cache_read_input_tokens":     cacheRead,
			"cache_creation_input_tokens": cacheCreate,
		},
	})
	if err != nil {
		t.Fatalf("marshal assistant message: %v", err)
	}
	return raw
}
