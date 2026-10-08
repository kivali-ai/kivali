package claudeagent

import "github.com/kivali-ai/kivali/internal/provider"

// modelPricing is the per-million-token rate for one model.
//
// Values are USD per 1,000,000 tokens. Cache-write is typically a small
// premium over input; cache-read is typically a deep discount. Values are
// approximate and intended for user-facing cost signal, not billing.
type modelPricing struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheWritePerMTok float64
	CacheReadPerMTok  float64
}

// pricingTable maps model IDs to pricing.
type pricingTable map[string]modelPricing

// defaultPricing has the public per-MTok rates for every model in the
// catalog. Source: platform.claude.com/docs/en/about-claude/pricing
// (last re-verified 2026-09-01 — every row, not just the newest).
// Tune via config when exact accounting matters.
//
// Derived from catalog; the rates are declared there alongside each
// model's selectability and context window. catalog.go explains why
// entries outlive the picker and when one may be deleted.
//
// Every model here prices the full 1M context window at the standard
// rate — there is no >200k premium tier.
//
// Cache-write rate: this table holds the 5-minute ephemeral rate
// (1.25x input). All Kivali cache_control blocks ship plain
// {"type":"ephemeral"} (no ttl="1h" override anywhere in the tree),
// so the 5m rate is correct for everything we send today. If a
// future change starts emitting ttl="1h" for any breakpoint, the
// 1-hour rate (2x input) needs to be applied to those tokens —
// which requires splitting provider.TokenUsage.CacheCreateTokens into 5m /
// 1h buckets (the API reports them separately under `cache_creation`).
//
// Cache-read rate: 0.1x input on every model EXCEPT Fable 5.1, which
// reads cache at $0.25/MTok — 0.025x its input rate, a quarter of what
// Fable 5 charges and half of Opus 5's. It is the first model to break
// the 0.1x rule, so do not derive that column from InputPerMTok; read
// each one off the published table.
//
// Opus 4.7 note: the new tokenizer can use up to ~35% more tokens
// for the same text. Per-token price is unchanged from Opus 4.6,
// but effective $/request for the same prose is up.
var defaultPricing = pricingFromCatalog()

// pricingFromCatalog projects the catalog into the map shape callers
// already use, so defaultPricing.Cost / .CostOK keep working unchanged
// while the rates themselves live in exactly one place.
func pricingFromCatalog() pricingTable {
	t := make(pricingTable, len(catalog))
	for _, m := range catalog {
		t[m.ID] = m.Pricing
	}
	return t
}

// Cost returns the USD cost of a provider.TokenUsage at the given model's rate.
// Unknown models cost 0. Prefer CostOK when the caller aggregates: a
// bare 0 can't be told apart from a genuinely free row.
func (t pricingTable) Cost(model string, u provider.TokenUsage) float64 {
	cost, _ := t.CostOK(model, u)
	return cost
}

// CostOK returns the USD cost of a provider.TokenUsage at the given model's
// rate, plus whether the model was priceable at all.
//
// The second return separates "this row genuinely cost nothing" (zero
// tokens) from "we hold no rate for this model ID" — two different
// facts that Cost's bare 0 collapses into one. An aggregator that can't
// tell them apart reports unpriced tokens as $0 of spend, which is how
// 47M cache tokens on <synthetic> rows vanished from a prod rollup.
// Callers should surface unpriced tokens as their own figure rather
// than folding them into a dollar total.
func (t pricingTable) CostOK(model string, u provider.TokenUsage) (float64, bool) {
	// Pricing is keyed on the bare model ID. A [1m] context-window
	// suffix doesn't change the per-token rate (1M is billed at the
	// standard rate, no >200k premium for these models), and neither
	// does the dated snapshot suffix the CLI reports back for some
	// models — "claude-haiku-4-5-20251001" is Haiku 4.5 and is billed
	// at Haiku 4.5's rates. catalogKey strips both.
	p, ok := t[catalogKey(model)]
	if !ok {
		return 0, false
	}
	cost := 0.0
	cost += float64(u.InputTokens) * p.InputPerMTok / 1_000_000
	cost += float64(u.OutputTokens) * p.OutputPerMTok / 1_000_000
	cost += float64(u.CacheReadTokens) * p.CacheReadPerMTok / 1_000_000
	cost += float64(u.CacheCreateTokens) * p.CacheWritePerMTok / 1_000_000
	return cost, true
}
