package agentpod

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestUsageEventCarriesLongPromptTier is the pod's half of the
// long-prompt tier. The driver decides each call's tier, and core
// prices the row it writes from the event alone, so a flag dropped here
// bills a Haiku 5.5 turn above 100k prompt tokens at a fifth of its
// cost. Both the flat counts (one bucket) and each ByModel entry (a
// turn on both sides of the threshold) carry it, under the JSON key
// core reads.
func TestUsageEventCarriesLongPromptTier(t *testing.T) {
	flat := usageEvent(TurnEventDone, &provider.CompleteResponse{
		Model: "claude-haiku-5-5",
		Usage: provider.TokenUsage{InputTokens: 10, CacheReadTokens: 150_000, LongPrompt: true},
	})
	if !flat.LongPrompt {
		t.Errorf("flat event LongPrompt = false, want the driver's tier")
	}
	raw, err := json.Marshal(flat)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"long_prompt":true`) {
		t.Errorf("flat event JSON %s has no long_prompt key", raw)
	}

	split := usageEvent(TurnEventDone, &provider.CompleteResponse{
		Model: "claude-haiku-5-5",
		Usage: provider.TokenUsage{InputTokens: 30, LongPrompt: true},
		ByModel: []provider.ModelUsage{
			{Model: "claude-haiku-5-5", Usage: provider.TokenUsage{InputTokens: 20}},
			{Model: "claude-haiku-5-5", Usage: provider.TokenUsage{InputTokens: 10, LongPrompt: true}},
		},
	})
	if len(split.ByModel) != 2 || split.ByModel[0].LongPrompt || !split.ByModel[1].LongPrompt {
		t.Errorf("ByModel = %+v, want the standard bucket then the long one", split.ByModel)
	}
}

// TestUsageEventCarriesOneHourCacheWrites is the pod's half of the
// cache-write split. Core prices one-hour writes at 2x input and the
// rest at 1.25x from the counts on the event, so a count dropped here
// under-reports those writes by 37.5%.
func TestUsageEventCarriesOneHourCacheWrites(t *testing.T) {
	ev := usageEvent(TurnEventDone, &provider.CompleteResponse{
		Model: "claude-opus-5-5",
		Usage: provider.TokenUsage{CacheCreateTokens: 900, CacheCreate1hTokens: 700},
		ByModel: []provider.ModelUsage{
			{Model: "claude-opus-5-5", Usage: provider.TokenUsage{CacheCreateTokens: 600, CacheCreate1hTokens: 500}},
			{Model: "claude-fable-5-2", Usage: provider.TokenUsage{CacheCreateTokens: 300, CacheCreate1hTokens: 200}},
		},
	})
	if ev.CacheCreate1hTokens != 700 || len(ev.ByModel) != 2 || ev.ByModel[0].CacheCreate1hTokens != 500 || ev.ByModel[1].CacheCreate1hTokens != 200 {
		t.Errorf("event = %+v, want the one-hour counts on the flat fields and each bucket", ev)
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cache_create_1h_tokens":700`) {
		t.Errorf("event JSON %s has no cache_create_1h_tokens key", raw)
	}
}
