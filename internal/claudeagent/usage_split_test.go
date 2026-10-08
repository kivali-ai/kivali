package claudeagent

import (
	"encoding/json"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// mkModelMsg builds one stream-json assistant message: a single text
// block, an answering model id, and that call's usage.
func mkModelMsg(t *testing.T, model, text string, in, out int) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(message{
		Role:    "assistant",
		Model:   model,
		Content: []messageContent{{Type: "text", Text: text}},
		Usage:   messageUsage{InputTokens: in, OutputTokens: out},
	})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return b
}

// drain collects the StreamEvents buffered on a channel without
// blocking. The stream's emit path is buffered (cap 16) and these tests
// stay well under that, so everything the handler emitted is already
// sitting there when we look.
func drain(ch <-chan provider.StreamEvent) []provider.StreamEvent {
	var out []provider.StreamEvent
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestUsageSplitsByAnsweringModel is the core guard on mixed turns.
//
// A turn is a loop of N /v1/messages calls whose token counts
// accumulate, and the model that serves a call is per-call — a request
// can be answered by a model other than the one it named, and not every
// call in a turn need land on the same one. A single last-write-wins
// model beside the accumulated total would bill every token of a mixed
// turn at whichever id happened to answer last; Fable and Opus differ
// by 2x, so that is a real mispricing in either direction.
func TestUsageSplitsByAnsweringModel(t *testing.T) {
	// Three calls: fable, opus, fable. The turn ends on fable, so the
	// old single-model shape would have billed all 6,600 tokens at
	// fable's rate — including the 2,000 opus actually produced.
	call1 := mkModelMsg(t, "claude-fable-5-1", "a", 1000, 100)
	call2 := mkModelMsg(t, "claude-opus-5", "b", 2000, 200)
	call3 := mkModelMsg(t, "claude-fable-5-1", "c", 3000, 300)

	assertFinal := func(t *testing.T, f *provider.CompleteResponse) {
		t.Helper()
		// The flat total is unchanged — the split is the same tokens
		// bucketed, never a different sum. Anything reading only the
		// flat fields sees exactly what it saw before.
		if got, want := f.Usage.InputTokens, 6000; got != want {
			t.Errorf("Usage.InputTokens = %d, want %d", got, want)
		}
		if got, want := f.Usage.OutputTokens, 600; got != want {
			t.Errorf("Usage.OutputTokens = %d, want %d", got, want)
		}
		// Model stays last-write-wins, the label for a single-model turn.
		if got, want := f.Model, "claude-fable-5-1"; got != want {
			t.Errorf("Model = %q, want %q (last to answer)", got, want)
		}

		if len(f.ByModel) != 2 {
			t.Fatalf("ByModel = %+v, want 2 entries", f.ByModel)
		}
		// First-answered order, so the rows read in the order the models
		// actually spoke.
		if got, want := f.ByModel[0].Model, "claude-fable-5-1"; got != want {
			t.Errorf("ByModel[0].Model = %q, want %q", got, want)
		}
		// Both fable calls fold into one bucket: 1000+3000 in, 100+300 out.
		if got, want := f.ByModel[0].Usage.InputTokens, 4000; got != want {
			t.Errorf("ByModel[0] InputTokens = %d, want %d (both fable calls)", got, want)
		}
		if got, want := f.ByModel[0].Usage.OutputTokens, 400; got != want {
			t.Errorf("ByModel[0] OutputTokens = %d, want %d (both fable calls)", got, want)
		}
		if got, want := f.ByModel[1].Model, "claude-opus-5"; got != want {
			t.Errorf("ByModel[1].Model = %q, want %q", got, want)
		}
		if got, want := f.ByModel[1].Usage.InputTokens, 2000; got != want {
			t.Errorf("ByModel[1] InputTokens = %d, want %d", got, want)
		}

		// The invariant that makes the split safe to bill from: the
		// buckets reconstruct the total exactly. A drift here means
		// spend is being invented or dropped.
		var sum provider.TokenUsage
		for _, mu := range f.ByModel {
			sum = sum.Add(mu.Usage)
		}
		if sum != f.Usage {
			t.Errorf("ByModel sums to %+v, want %+v (must reconstruct the total)", sum, f.Usage)
		}
	}

	t.Run("subprocStream", func(t *testing.T) {
		s := &subprocStream{
			events: make(chan provider.StreamEvent, 16),
			done:   make(chan struct{}),
		}
		s.handleAssistantMessage(call1)
		s.handleAssistantMessage(call2)
		s.handleAssistantMessage(call3)
		assertFinal(t, s.Final())
	})

	t.Run("runnerTurn", func(t *testing.T) {
		tn := &runnerTurn{
			events: make(chan provider.StreamEvent, 16),
			done:   make(chan struct{}),
		}
		tn.handleAssistantMessage(call1)
		tn.handleAssistantMessage(call2)
		tn.handleAssistantMessage(call3)
		assertFinal(t, tn.Final())
	})
}

// TestSingleModelTurnHasNoSplit pins the common case: when every call
// was served by the same model, ByModel stays nil so the wire and the
// usage log keep exactly the shape they had before splitting existed.
func TestSingleModelTurnHasNoSplit(t *testing.T) {
	call1 := mkModelMsg(t, "claude-opus-5", "a", 1000, 100)
	call2 := mkModelMsg(t, "claude-opus-5", "b", 2000, 200)

	s := &subprocStream{
		events: make(chan provider.StreamEvent, 16),
		done:   make(chan struct{}),
	}
	s.handleAssistantMessage(call1)
	s.handleAssistantMessage(call2)
	f := s.Final()

	if f.ByModel != nil {
		t.Errorf("ByModel = %+v, want nil for a single-model turn", f.ByModel)
	}
	if got, want := f.Model, "claude-opus-5"; got != want {
		t.Errorf("Model = %q, want %q", got, want)
	}
	if got, want := f.Usage.InputTokens, 3000; got != want {
		t.Errorf("Usage.InputTokens = %d, want %d", got, want)
	}
}

// TestDeltasCarryTheAnsweringModel: the model rides out with the text
// WHILE the turn runs, not only in
// the final response. Core labels each chat bubble from it, so a value
// that only arrived at the end would mislabel every bubble the turn had
// already flushed.
func TestDeltasCarryTheAnsweringModel(t *testing.T) {
	s := &subprocStream{
		events: make(chan provider.StreamEvent, 16),
		done:   make(chan struct{}),
	}
	s.handleAssistantMessage(mkModelMsg(t, "claude-fable-5-1", "from fable", 10, 1))
	s.handleAssistantMessage(mkModelMsg(t, "claude-opus-5", "from opus", 10, 1))

	var deltas []provider.StreamEvent
	for _, ev := range drain(s.events) {
		if ev.Kind == provider.StreamDelta {
			deltas = append(deltas, ev)
		}
	}
	if len(deltas) != 2 {
		t.Fatalf("got %d deltas, want 2", len(deltas))
	}
	if got, want := deltas[0].Model, "claude-fable-5-1"; got != want {
		t.Errorf("deltas[0].Model = %q, want %q", got, want)
	}
	if got, want := deltas[1].Model, "claude-opus-5"; got != want {
		t.Errorf("deltas[1].Model = %q, want %q", got, want)
	}
	// Sanity: the text is paired with the right model, not merely
	// present in the right order.
	if got, want := deltas[0].Text, "from fable"; got != want {
		t.Errorf("deltas[0].Text = %q, want %q", got, want)
	}
}

// TestUsageEventsCostLandsOnce guards the one number that must not be
// duplicated when a turn expands into several rows. The CLI reports a
// single total_cost_usd for the whole turn and cannot attribute it per
// model, so repeating it on each row would multiply the turn's cost for
// anything that sums the column.
func TestUsageEventsCostLandsOnce(t *testing.T) {
	base := provider.UsageEvent{Agent: "alice", Purpose: "chat", Model: "claude-fable-5-1"}
	total := provider.TokenUsage{InputTokens: 3000, OutputTokens: 300}
	split := []provider.ModelUsage{
		{Model: "claude-fable-5-1", Usage: provider.TokenUsage{InputTokens: 1000, OutputTokens: 100}},
		{Model: "claude-opus-5", Usage: provider.TokenUsage{InputTokens: 2000, OutputTokens: 200}},
	}

	rows := usageEvents(base, total, split, 0.42)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if got, want := rows[0].Model, "claude-fable-5-1"; got != want {
		t.Errorf("rows[0].Model = %q, want %q", got, want)
	}
	if got, want := rows[1].Model, "claude-opus-5"; got != want {
		t.Errorf("rows[1].Model = %q, want %q", got, want)
	}
	var sum float64
	for _, r := range rows {
		sum += r.CostUSD
	}
	if sum != 0.42 {
		t.Errorf("summed CostUSD = %v, want 0.42 (the turn's total, counted once)", sum)
	}
	// Non-cost fields still come from the base row.
	if rows[1].Agent != "alice" || rows[1].Purpose != "chat" {
		t.Errorf("rows[1] lost base fields: %+v", rows[1])
	}
}

// TestUsageEventsSingleBucket pins the unmixed path: no split means one
// row carrying the flat total.
func TestUsageEventsSingleBucket(t *testing.T) {
	base := provider.UsageEvent{Agent: "alice", Model: "claude-opus-5"}
	total := provider.TokenUsage{InputTokens: 3000, OutputTokens: 300, CacheReadTokens: 50}

	rows := usageEvents(base, total, nil, 0.11)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].InputTokens != 3000 || rows[0].OutputTokens != 300 || rows[0].CacheReadTokens != 50 {
		t.Errorf("row = %+v, want the flat total", rows[0])
	}
	if rows[0].CostUSD != 0.11 {
		t.Errorf("CostUSD = %v, want 0.11", rows[0].CostUSD)
	}
	if rows[0].Model != "claude-opus-5" {
		t.Errorf("Model = %q, want the turn's model", rows[0].Model)
	}
}
