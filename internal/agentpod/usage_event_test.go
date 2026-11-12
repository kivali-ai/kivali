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
