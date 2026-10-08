package web

import (
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// TestSettingsSpendPricedFromTokens guards the usage rollup against the
// cumulative-cost trap: on the subscription transport the recorded
// CostUSD comes from the CLI's session-cumulative total_cost_usd, which
// we snapshot per turn. Summing those snapshots multiplies real spend,
// so the spend GET /api/v1/org/usage reports must be derived from the
// per-call token columns at published rates instead.
func TestSettingsSpendPricedFromTokens(t *testing.T) {
	srv := newTestServer(t)

	// One record with a deliberately absurd CostUSD (as a cumulative
	// gauge snapshot would produce) but modest, known token counts.
	// Mock Large 0 rates: input $5, output $25, cache_read $0.50 per MTok.
	//   1M input      → $5.00
	//   2M cache_read → $1.00
	//   0 output      → $0.00
	//   total           $6.00
	if err := srv.Store.AppendUsage(store.UsageRecord{
		TS:              time.Now().UTC(),
		Agent:           "cos",
		Purpose:         "chat",
		Model:           "mock-large-0",
		InputTokens:     1_000_000,
		OutputTokens:    0,
		CacheReadTokens: 2_000_000,
		CostUSD:         999.99, // bogus cumulative snapshot — must be ignored
	}); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	if w := usage24h(t, srv); !nearUSD(w.Spend, 6.00) {
		t.Errorf("spend = %v, want the token-derived $6.00 (not the recorded CostUSD $999.99)", w.Spend)
	}
}
