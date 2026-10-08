package claudeagent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// usageGaugeReq is the request the "usage-gauge" fake scenario answers.
func usageGaugeReq(agent string) provider.CompleteRequest {
	return provider.CompleteRequest{
		Agent: agent,
		Model: "claude-fake-1",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
}

// runUsageGaugeTurn drives one turn through the client and returns its
// final response.
func runUsageGaugeTurn(t *testing.T, c *Driver, req provider.CompleteRequest) *provider.CompleteResponse {
	t.Helper()
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, final := drainTurn(t, s)
	if err := s.Err(); err != nil {
		t.Fatalf("turn: %v", err)
	}
	_ = s.Close()
	if final == nil {
		t.Fatal("Final() = nil")
	}
	return final
}

// TestRunnerTurnUsageIsPerTurnNotCumulative is the end-to-end guard on
// the warm-runner path, which serves every parent chat turn in prod.
// Two turns on one subprocess: each must bill exactly its own two
// calls — six streamed block events reduced to two calls, output from
// the result frame — and each model's share must be the turn's, not
// the session-cumulative gauge the frame actually carries.
func TestRunnerTurnUsageIsPerTurnNotCumulative(t *testing.T) {
	withFakeCLIEnv(t, "usage-gauge")
	c := New(helperOpts(t, "usage-gauge", &memSessionStore{}))
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	wantTotal := fakeUsageModelOne.Add(fakeUsageModelTwo)
	wantSplit := []provider.ModelUsage{
		{Model: "claude-fake-1", Usage: fakeUsageModelOne},
		{Model: "claude-fake-2", Usage: fakeUsageModelTwo},
	}
	for turn := 1; turn <= 2; turn++ {
		final := runUsageGaugeTurn(t, c, usageGaugeReq("gauge-agent"))
		if final.Usage != wantTotal {
			t.Errorf("turn %d: Usage = %+v, want %+v (this turn's calls only)", turn, final.Usage, wantTotal)
		}
		if !reflect.DeepEqual(final.ByModel, wantSplit) {
			t.Errorf("turn %d: ByModel = %+v, want %+v (the gauge differenced, not read)", turn, final.ByModel, wantSplit)
		}
	}
}

// TestRunnerResumedProcessFirstTurnIsExactInTotal covers the runner
// that spawned with --resume: the CLI reloads the session's history
// into the gauge, so the first frame's modelUsage is useless as a
// per-turn figure. The turn's TOTAL is still exact — it comes from the
// frame's own usage — and the split is rebuilt from the calls: prompt
// counts per model exact, output shared by call count. From the second
// turn on, the gauge has a baseline and the split is exact again.
func TestRunnerResumedProcessFirstTurnIsExactInTotal(t *testing.T) {
	withFakeCLIEnv(t, "usage-gauge")
	store := &memSessionStore{}
	_ = store.WriteClaudeSessionID("resumed-gauge-agent", "gauge-sess-id")
	// Pre-create the on-disk log so resolveResumeSessionID keeps the
	// stored id and the runner spawns with --resume.
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "-")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "gauge-sess-id.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	opts := helperOpts(t, "usage-gauge", store)
	opts.HomeDir = home
	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()

	wantTotal := fakeUsageModelOne.Add(fakeUsageModelTwo)

	first := runUsageGaugeTurn(t, c, usageGaugeReq("resumed-gauge-agent"))
	if first.Usage != wantTotal {
		t.Errorf("turn 1: Usage = %+v, want %+v (the frame's own total, history excluded)", first.Usage, wantTotal)
	}
	if len(first.ByModel) != 2 {
		t.Fatalf("turn 1: ByModel = %+v, want 2 buckets", first.ByModel)
	}
	// Prompt-side counts come from the deduplicated snapshots and are
	// exact per model; output is the frame's 300 shared over two calls.
	wantOne := fakeUsageModelOne
	wantOne.OutputTokens = wantTotal.OutputTokens / 2
	wantTwo := fakeUsageModelTwo
	wantTwo.OutputTokens = wantTotal.OutputTokens - wantOne.OutputTokens
	wantFirst := []provider.ModelUsage{
		{Model: "claude-fake-1", Usage: wantOne},
		{Model: "claude-fake-2", Usage: wantTwo},
	}
	if !reflect.DeepEqual(first.ByModel, wantFirst) {
		t.Errorf("turn 1: ByModel = %+v, want %+v (rebuilt: exact prompt counts, output by call count)", first.ByModel, wantFirst)
	}

	second := runUsageGaugeTurn(t, c, usageGaugeReq("resumed-gauge-agent"))
	if second.Usage != wantTotal {
		t.Errorf("turn 2: Usage = %+v, want %+v", second.Usage, wantTotal)
	}
	wantSecond := []provider.ModelUsage{
		{Model: "claude-fake-1", Usage: fakeUsageModelOne},
		{Model: "claude-fake-2", Usage: fakeUsageModelTwo},
	}
	if !reflect.DeepEqual(second.ByModel, wantSecond) {
		t.Errorf("turn 2: ByModel = %+v, want %+v (gauge baseline known after the first frame)", second.ByModel, wantSecond)
	}
}
