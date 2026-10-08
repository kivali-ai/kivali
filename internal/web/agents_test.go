package web

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestToolResultsByID covers the tool_result lookup map the transcript
// builder uses to pair each tool_use with its result in one merged
// row. A broken map means post-refresh tool bubbles
// lose their output section.
func TestToolResultsByID(t *testing.T) {
	cases := []struct {
		name string
		in   []store.ChatMessage
		want map[string]store.ChatMessage
	}{
		{
			name: "empty history",
			in:   nil,
			want: map[string]store.ChatMessage{},
		},
		{
			name: "no tool_result entries",
			in: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "hi"},
				{Role: store.RoleSent, Kind: "direct_chat", Content: "hello"},
			},
			want: map[string]store.ChatMessage{},
		},
		{
			name: "single tool_result — indexed by ToolUseID",
			in: []store.ChatMessage{
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_1", ToolName: "file_view"},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "Directory: /files/"},
			},
			want: map[string]store.ChatMessage{
				"tu_1": {Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "Directory: /files/"},
			},
		},
		{
			name: "multiple tool_results — all indexed",
			in: []store.ChatMessage{
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_a"},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_a", Content: "a"},
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_b"},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_b", Content: "b", IsError: true},
			},
			want: map[string]store.ChatMessage{
				"tu_a": {Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_a", Content: "a"},
				"tu_b": {Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_b", Content: "b", IsError: true},
			},
		},
		{
			name: "tool_result without ToolUseID is skipped",
			// Defensive: a bad persist path could write a tool_result
			// with no ID. Indexing by empty string would collide with
			// anything else empty, so we simply exclude these.
			in: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "", Content: "orphan"},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_x", Content: "good"},
			},
			want: map[string]store.ChatMessage{
				"tu_x": {Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_x", Content: "good"},
			},
		},
		{
			name: "duplicate ToolUseID — last write wins",
			// Shouldn't happen in practice (a tool_use_id is a UUID
			// minted once) but nail down the deterministic behavior
			// in case history replay ever double-appends.
			in: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_z", Content: "first"},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_z", Content: "second"},
			},
			want: map[string]store.ChatMessage{
				"tu_z": {Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_z", Content: "second"},
			},
		},
		{
			name: "doc_published entries are not tool_results",
			in: []store.ChatMessage{
				{Role: store.RoleSent, Kind: "doc_published", ToolUseID: "tu_p", Content: "📄 ..."},
			},
			want: map[string]store.ChatMessage{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolResultsByID(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("toolResultsByID\n  got  %+v\n  want %+v", got, tc.want)
			}
		})
	}
}

// TestComputeChatFillStats locks in the math the agent_detail
// page-load render and the chat "done" SSE event share. Drift
// between the two is what causes the visible jump on refresh.
func TestComputeChatFillStats(t *testing.T) {
	cases := []struct {
		name              string
		agentModel        string
		defaultModel      string
		hist              []store.ChatMessage
		realContextTokens int
		wantTokenEst      int
		wantContextLim    int
		wantFillPct       int
		wantBucket        string
		wantResolved      string
	}{
		{
			name:           "empty history → 0% low — a 1M model",
			agentModel:     provider.MockModelLarge,
			hist:           nil,
			wantTokenEst:   0,
			wantContextLim: 1_000_000,
			wantFillPct:    0,
			wantBucket:     "low",
			wantResolved:   provider.MockModelLarge,
		},
		{
			// A small transcript (chars/4 ≈
			// 20k → 2%) hides a prompt that's actually ~66% full once
			// system prompt + schemas + memory + files are counted. The
			// real per-call occupancy wins and the ring reads "high".
			name:       "real occupancy beats tiny transcript estimate",
			agentModel: provider.MockModelLarge,
			hist: []store.ChatMessage{
				{Content: strings.Repeat("x", 80_000)},
			},
			realContextTokens: 660_000,
			wantTokenEst:      660_000,
			wantContextLim:    1_000_000,
			wantFillPct:       66,
			wantBucket:        "mid",
			wantResolved:      provider.MockModelLarge,
		},
		{
			// Cold start (no turn has run yet): realContextTokens is 0,
			// so we fall back to the chars/4 transcript estimate rather
			// than showing an empty ring.
			name:       "cold start falls back to chars/4 estimate",
			agentModel: provider.MockModelSmall,
			hist: []store.ChatMessage{
				{Content: strings.Repeat("x", 80_000)},
			},
			realContextTokens: 0,
			wantTokenEst:      20_000,
			wantContextLim:    200_000,
			wantFillPct:       10,
			wantBucket:        "low",
			wantResolved:      provider.MockModelSmall,
		},
		{
			name:         "agent override beats default — the 200k model stays 200k",
			agentModel:   provider.MockModelSmall,
			defaultModel: provider.MockModelLarge,
			hist: []store.ChatMessage{
				{Content: strings.Repeat("x", 80_000)},
			},
			wantTokenEst:   20_000,
			wantContextLim: 200_000,
			wantFillPct:    10,
			wantBucket:     "low",
			wantResolved:   provider.MockModelSmall,
		},
		{
			// The window is ModelInfo.ContextWindow, not a rule of
			// thumb applied to every model: a rule learned on one
			// model once under-reported a whole fleet by 5x. How the
			// Claude provider sizes its ids ([1m], native 1M) is tested
			// in internal/claudeagent.
			name:       "the window is the provider's, per model",
			agentModel: provider.MockModelLarge,
			hist: []store.ChatMessage{
				{Content: strings.Repeat("x", 400_000)},
			},
			wantTokenEst:   100_000,
			wantContextLim: 1_000_000,
			wantFillPct:    10,
			wantBucket:     "low",
			wantResolved:   provider.MockModelLarge,
		},
		{
			// Unknown is sized small: under-reporting capacity warns
			// early rather than reading "calm" while the session
			// compacts.
			name:       "a model the provider does not know → the 200k fallback",
			agentModel: "some-model-nobody-knows",
			hist: []store.ChatMessage{
				{Content: strings.Repeat("x", 80_000)},
			},
			wantTokenEst:   20_000,
			wantContextLim: 200_000,
			wantFillPct:    10,
			wantBucket:     "low",
			wantResolved:   "some-model-nobody-knows",
		},
		{
			name:         "default model fills in when agent unset",
			defaultModel: provider.MockModelLarge,
			hist: []store.ChatMessage{
				{Content: strings.Repeat("x", 3_000_000), ToolInput: strings.Repeat("y", 1_000_000)},
			},
			wantTokenEst:   1_000_000,
			wantContextLim: 1_000_000,
			wantFillPct:    100,
			wantBucket:     "critical",
			wantResolved:   provider.MockModelLarge,
		},
		{
			name:       "runaway estimate clamps to 100%",
			agentModel: provider.MockModelLarge,
			hist: []store.ChatMessage{
				{Content: strings.Repeat("x", 25_000_000)},
			},
			wantTokenEst:   6_250_000,
			wantContextLim: 1_000_000,
			wantFillPct:    100,
			wantBucket:     "critical",
			wantResolved:   provider.MockModelLarge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&Server{Provider: provider.MockProvider{}}).computeChatFillStats(tc.agentModel, tc.defaultModel, tc.hist, tc.realContextTokens)
			if got.TokenEst != tc.wantTokenEst {
				t.Errorf("TokenEst = %d, want %d", got.TokenEst, tc.wantTokenEst)
			}
			if got.ContextLimit != tc.wantContextLim {
				t.Errorf("ContextLimit = %d, want %d", got.ContextLimit, tc.wantContextLim)
			}
			if got.FillPct != tc.wantFillPct {
				t.Errorf("FillPct = %d, want %d", got.FillPct, tc.wantFillPct)
			}
			if got.FillBucket != tc.wantBucket {
				t.Errorf("FillBucket = %q, want %q", got.FillBucket, tc.wantBucket)
			}
			if got.ResolvedModel != tc.wantResolved {
				t.Errorf("ResolvedModel = %q, want %q", got.ResolvedModel, tc.wantResolved)
			}
			if got.LongThresh != tc.wantContextLim*3/4 {
				t.Errorf("LongThresh = %d, want %d", got.LongThresh, tc.wantContextLim*3/4)
			}
		})
	}
}
