package web

import (
	"context"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
)

// TestAgentpodEmptyModelPricedAgainstRequestedModel asserts the
// user-visible outcome a turn that errored out must keep: its billed
// tokens land in a usage row priced against the model that was
// requested, not a $0 row. The driver never reports a provider
// placeholder id (it attributes those calls to the model that answered
// — claudeagent.attributeModel); when it has no id at all it sends
// none, and core falls back to the model it asked for.
func TestAgentpodEmptyModelPricedAgainstRequestedModel(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "tu-empty")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	// The shape of the worst prod row: 42 minutes of work, killed by an
	// output-token-maximum error, ~29M billed cache tokens.
	if err := c.PostTurnEvent(context.Background(), "tu-empty", agentpod.TurnEvent{
		Kind:              agentpod.TurnEventDone,
		StopReason:        provider.StopError,
		CacheReadTokens:   27_324_359,
		CacheCreateTokens: 1_660_852,
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	recs, err := srv.Store.ReadUsageSince(before)
	if err != nil {
		t.Fatalf("ReadUsageSince: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("usage records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Model != "mock-large-0" {
		t.Errorf("Model = %q, want mock-large-0 (the model requested at publish time)", r.Model)
	}
	if r.CacheReadTokens != 27_324_359 || r.CacheCreateTokens != 1_660_852 {
		t.Errorf("tokens = cr=%d cw=%d, want 27324359/1660852", r.CacheReadTokens, r.CacheCreateTokens)
	}
	if r.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want >0 — a $0 row is what the operator saw in prod", r.CostUSD)
	}
}

// TestAgentpodEmptyBucketPricedAgainstTurnModel covers the same
// substitution inside a per-model split: a bucket with no model prices
// at the turn's model, so the split never yields a $0 row.
func TestAgentpodEmptyBucketPricedAgainstTurnModel(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "tu-split")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "tu-split", agentpod.TurnEvent{
		Kind:       agentpod.TurnEventDone,
		StopReason: provider.StopError,
		Model:      "mock-large-0",
		ByModel: []agentpod.TurnModelUsage{
			{Model: "mock-large-0", InputTokens: 1000, OutputTokens: 100},
			{Model: "", CacheReadTokens: 5_000_000},
		},
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	recs, err := srv.Store.ReadUsageSince(before)
	if err != nil {
		t.Fatalf("ReadUsageSince: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("usage records = %d, want 2 (one per bucket)", len(recs))
	}
	if recs[1].Model != "mock-large-0" {
		t.Errorf("bucket model = %q, want the turn's model", recs[1].Model)
	}
	// The second row carries no driver cost (that rides row 0), so it
	// must have been priced locally — which only works with a real id.
	if recs[1].CostUSD <= 0 {
		t.Errorf("split row CostUSD = %v, want >0", recs[1].CostUSD)
	}
}
