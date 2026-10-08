package claudeagent

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestIsSentinelModel pins the shape rule: angle-bracketed IDs are the
// CLI's placeholders, everything else is a real model. Matching by
// shape rather than by the literal "<synthetic>" is the point — a
// sentinel we haven't seen yet must be caught the day it ships, because
// the cost of missing one is a silent $0 valuation of real spend.
func TestIsSentinelModel(t *testing.T) {
	sentinels := []string{
		"<synthetic>",
		"<synthetic>[1m]", // the suffix is stripped before the check
		"<interrupted>",   // hypothetical future sentinel
		"<>",
	}
	for _, m := range sentinels {
		if !isSentinelModel(m) {
			t.Errorf("isSentinelModel(%q) = false, want true", m)
		}
	}

	real := []string{
		"",
		"claude-opus-5",
		"claude-fable-5-1",
		"claude-sonnet-5[1m]",
		"<", // a lone bracket is not a sentinel, and must not panic
		"claude-<weird>-5",
	}
	for _, m := range real {
		if isSentinelModel(m) {
			t.Errorf("isSentinelModel(%q) = true, want false", m)
		}
	}
}

// TestCostOKSeparatesUnpricedFromFree is the reason CostOK exists. Cost
// returns 0 both for a model we have no rate for and for a row that
// genuinely cost nothing; an aggregator that can't tell them apart
// reports billed tokens as $0 of spend — which is what <synthetic>
// rows, carrying real cache tokens, would otherwise show.
func TestCostOKSeparatesUnpricedFromFree(t *testing.T) {
	billed := provider.TokenUsage{CacheReadTokens: 27_324_359, CacheCreateTokens: 1_660_852}

	if cost, priced := defaultPricing.CostOK("<synthetic>", billed); priced || cost != 0 {
		t.Errorf("CostOK(<synthetic>) = (%v, %v), want (0, false)", cost, priced)
	}
	// A real model with no tokens is free, but it IS priced — the
	// distinction the bool carries.
	if cost, priced := defaultPricing.CostOK("claude-opus-5", provider.TokenUsage{}); !priced || cost != 0 {
		t.Errorf("CostOK(opus-5, empty) = (%v, %v), want (0, true)", cost, priced)
	}
	// And the same tokens under the model that actually served them are
	// worth real money — the sum the operator was owed.
	cost, priced := defaultPricing.CostOK("claude-fable-5-1", billed)
	if !priced {
		t.Fatal("CostOK(claude-fable-5-1) reported unpriced")
	}
	// 27,324,359 cache reads @ $0.25/MTok + 1,660,852 cache writes @
	// $12.50/MTok ≈ $6.83 + $20.76.
	if cost < 27 || cost > 28 {
		t.Errorf("CostOK(claude-fable-5-1) = %v, want ~27.59", cost)
	}

	// Cost stays the thin wrapper, so existing callers are unchanged.
	if got := defaultPricing.Cost("claude-fable-5-1", billed); got != cost {
		t.Errorf("Cost = %v, CostOK = %v; want identical", got, cost)
	}
}
