package claudeagent

import (
	"math"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

func TestCostKnownModel(t *testing.T) {
	u := provider.TokenUsage{
		InputTokens:       1_000_000,
		OutputTokens:      500_000,
		CacheReadTokens:   2_000_000,
		CacheCreateTokens: 100_000,
	}
	cost := defaultPricing.Cost("claude-opus-4-7", u)
	// Opus 4.7 at standard rates (no long-context premium up to 1M):
	//   1M  input  × $5   = $5.000
	//   500k output × $25 = $12.500
	//   2M  cache_r × $0.50 = $1.000
	//   100k cache_w × $6.25 = $0.625  (5m TTL rate)
	//   total                 = $19.125
	want := 5.00 + 12.50 + 1.00 + 0.625
	if math.Abs(cost-want) > 1e-6 {
		t.Errorf("Cost = %v, want %v", cost, want)
	}
}

// TestCostOpus46MatchesOpus47 pins that both Opus 4.x models are
// priced identically, and that both are in the table at all — Cost()
// silently returns 0 for an id it does not know. The 4.6 → 4.7 change
// is a tokenizer change, not a price change, so the per-MTok rates are
// the same.
func TestCostOpus46MatchesOpus47(t *testing.T) {
	u := provider.TokenUsage{
		InputTokens:       1_000_000,
		OutputTokens:      500_000,
		CacheReadTokens:   2_000_000,
		CacheCreateTokens: 100_000,
	}
	if got, want := defaultPricing.Cost("claude-opus-4-6", u), defaultPricing.Cost("claude-opus-4-7", u); math.Abs(got-want) > 1e-9 {
		t.Errorf("opus-4-6 Cost = %v, want %v (same as opus-4-7)", got, want)
	}
}

// TestCostStripsContextSuffix pins that a [1m] context-window suffix
// is transparent to pricing: the 1M window is billed at the standard
// per-token rate, so "claude-opus-4-8[1m]" must cost the same as the
// bare "claude-opus-4-8". Before baseModel() the suffixed key missed
// the table and Cost() silently returned 0.
func TestCostStripsContextSuffix(t *testing.T) {
	u := provider.TokenUsage{
		InputTokens:       1_000_000,
		OutputTokens:      500_000,
		CacheReadTokens:   2_000_000,
		CacheCreateTokens: 100_000,
	}
	got := defaultPricing.Cost("claude-opus-4-8[1m]", u)
	want := defaultPricing.Cost("claude-opus-4-8", u)
	if want == 0 {
		t.Fatal("bare opus-4-8 priced at 0; test precondition broken")
	}
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("opus-4-8[1m] Cost = %v, want %v (same as bare)", got, want)
	}
}

// TestCostFable5IsPricedAboveOpus pins the one thing about Fable 5 that
// is easy to get wrong by pattern-matching the rest of the table: it is
// the first model Kivali offers that costs MORE than Opus, at exactly
// 2x on every axis. Every other entry added since Opus 4.6 has been at
// or below the Opus rate, so a future "mirror the Opus rates pending
// re-verification" edit — the comment already sitting above Opus 4.8 —
// would silently halve Fable's reported spend and under-report the cost
// signal on the most expensive model in the fleet.
func TestCostFable5IsPricedAboveOpus(t *testing.T) {
	u := provider.TokenUsage{
		InputTokens:       1_000_000,
		OutputTokens:      500_000,
		CacheReadTokens:   2_000_000,
		CacheCreateTokens: 100_000,
	}
	fable := defaultPricing.Cost("claude-fable-5", u)
	opus := defaultPricing.Cost("claude-opus-4-8", u)
	if opus == 0 {
		t.Fatal("opus-4-8 priced at 0; test precondition broken")
	}
	if math.Abs(fable-2*opus) > 1e-6 {
		t.Errorf("fable-5 Cost = %v, want %v (2x opus-4-8)", fable, 2*opus)
	}
	// And the [1m] variant prices identically — 1M carries no premium.
	if got := defaultPricing.Cost("claude-fable-5[1m]", u); math.Abs(got-fable) > 1e-9 {
		t.Errorf("fable-5[1m] Cost = %v, want %v (same as bare)", got, fable)
	}
}

// TestCostFable51CacheReadIsCheaperThanFable5 pins the single rate that
// makes Fable 5.1 a different row rather than an alias of Fable 5.
//
// Every other model in the table reads cache at 0.1x its input rate, so
// the whole column looks derivable — and a "tidy up the duplication"
// edit that computed it as InputPerMTok/10 would pass every other test
// in this file while quietly quadrupling 5.1's reported cache-read
// spend. Cache reads dominate the bill on exactly the long-horizon
// sessions Fable is chosen for, so that error would land on the most
// expensive agents in the fleet.
func TestCostFable51CacheReadIsCheaperThanFable5(t *testing.T) {
	// Cache reads only — isolate the one axis where the two differ.
	u := provider.TokenUsage{CacheReadTokens: 1_000_000}

	got := defaultPricing.Cost("claude-fable-5-1", u)
	if math.Abs(got-0.25) > 1e-9 {
		t.Errorf("fable-5-1 cache-read Cost = %v, want 0.25", got)
	}
	if fable5 := defaultPricing.Cost("claude-fable-5", u); math.Abs(fable5-4*got) > 1e-9 {
		t.Errorf("fable-5 cache-read Cost = %v, want %v (4x fable-5-1)", fable5, 4*got)
	}
	// It also undercuts Opus 5, which is the counterintuitive part: the
	// pricier model reads cache for half of what the cheaper one does.
	if opus := defaultPricing.Cost("claude-opus-5", u); got >= opus {
		t.Errorf("fable-5-1 cache-read %v not below opus-5 %v", got, opus)
	}

	// On every other axis the two Fable rows must stay identical —
	// input, output and cache-write are unchanged between 5 and 5.1.
	same := provider.TokenUsage{
		InputTokens:       1_000_000,
		OutputTokens:      500_000,
		CacheCreateTokens: 100_000,
	}
	a := defaultPricing.Cost("claude-fable-5-1", same)
	b := defaultPricing.Cost("claude-fable-5", same)
	if a == 0 {
		t.Fatal("fable-5-1 priced at 0; test precondition broken")
	}
	if math.Abs(a-b) > 1e-9 {
		t.Errorf("fable-5-1 non-cache-read Cost = %v, want %v (same as fable-5)", a, b)
	}
}

// TestCostPricesDatedSnapshotIDs: the SDK transport records the id the
// CLI echoes back, which for Haiku is the API's dated snapshot even
// though the request asked for the bare id. That id is in no catalog
// row, so without normalizing it CostOK would report (0, false) and the
// settings page would count all summarizer traffic as "+N unpriced".
func TestCostPricesDatedSnapshotIDs(t *testing.T) {
	u := provider.TokenUsage{
		InputTokens:       1_000_000,
		OutputTokens:      500_000,
		CacheReadTokens:   2_000_000,
		CacheCreateTokens: 100_000,
	}
	want, ok := defaultPricing.CostOK("claude-haiku-4-5", u)
	if !ok || want == 0 {
		t.Fatal("bare haiku-4-5 is unpriced; test precondition broken")
	}
	for _, m := range []string{
		"claude-haiku-4-5-20251001",
		"claude-haiku-4-5-20251001[1m]",
	} {
		got, priced := defaultPricing.CostOK(m, u)
		if !priced {
			t.Errorf("CostOK(%q) reported unpriced; a dated snapshot is still Haiku 4.5", m)
			continue
		}
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("CostOK(%q) = %v, want %v (bare haiku-4-5 rate)", m, got, want)
		}
	}

	// A dated id for a model with no catalog row stays unpriced. That
	// is the intended answer, not an oversight: normalizing the key
	// does not invent a rate, and Opus 4.5 has no row. If one is ever
	// added this assertion is the thing that says so out loud.
	if cost, priced := defaultPricing.CostOK("claude-opus-4-5-20251101", u); priced || cost != 0 {
		t.Errorf("CostOK(claude-opus-4-5-20251101) = (%v, %v), want (0, false) — "+
			"no catalog row for Opus 4.5", cost, priced)
	}
}

func TestCostUnknownModelIsZero(t *testing.T) {
	u := provider.TokenUsage{InputTokens: 1_000_000}
	if c := defaultPricing.Cost("not-a-model", u); c != 0 {
		t.Errorf("Cost = %v, want 0 for unknown model", c)
	}
}

func TestCostSmallNumbers(t *testing.T) {
	u := provider.TokenUsage{InputTokens: 100, OutputTokens: 50}
	// 100/1M * 1.00 + 50/1M * 5.00 = 0.0001 + 0.00025 = 0.00035
	cost := defaultPricing.Cost("claude-haiku-4-5", u)
	want := 0.0001 + 0.00025
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("Cost = %v, want %v", cost, want)
	}
}

// TestCostOpus55IsCheaperThanOpus5OnEveryAxis pins the first Opus that
// costs less than its predecessor. Input and output are $4/$20 against
// $5/$25, and cache reads are $0.20 — 0.05x its input rate, half the
// 0.1x multiplier most rows follow. Like Fable 5.1's row, that column
// cannot be derived from InputPerMTok; a tidy-up that did so would
// double the reported cache-read spend of every agent on the fleet
// default.
func TestCostOpus55IsCheaperThanOpus5OnEveryAxis(t *testing.T) {
	reads := provider.TokenUsage{CacheReadTokens: 1_000_000}
	if got := defaultPricing.Cost("claude-opus-5-5", reads); math.Abs(got-0.20) > 1e-9 {
		t.Errorf("opus-5-5 cache-read Cost = %v, want 0.20", got)
	}
	for _, axis := range []struct {
		name string
		u    provider.TokenUsage
	}{
		{"input", provider.TokenUsage{InputTokens: 1_000_000}},
		{"output", provider.TokenUsage{OutputTokens: 1_000_000}},
		{"cache write", provider.TokenUsage{CacheCreateTokens: 1_000_000}},
		{"cache read", reads},
	} {
		newer := defaultPricing.Cost("claude-opus-5-5", axis.u)
		older := defaultPricing.Cost("claude-opus-5", axis.u)
		if newer == 0 || older == 0 {
			t.Fatalf("%s: a row priced at 0; test precondition broken", axis.name)
		}
		if newer >= older {
			t.Errorf("%s: opus-5-5 Cost %v is not below opus-5 %v", axis.name, newer, older)
		}
	}
	// And the [1m] variant prices identically — 1M carries no premium.
	u := provider.TokenUsage{InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 2_000_000}
	if a, b := defaultPricing.Cost("claude-opus-5-5[1m]", u), defaultPricing.Cost("claude-opus-5-5", u); math.Abs(a-b) > 1e-9 {
		t.Errorf("opus-5-5[1m] Cost = %v, want %v (same as bare)", a, b)
	}
}

// TestCostHaiku55LongPromptTier pins Haiku 5.5's two tiers. A call
// whose prompt is above 100k tokens bills every token, output
// included, at 5x the standard rate. Pricing that call at the standard
// rate would under-report a long-running Haiku agent's spend by up to
// 5x, and charging the long rate without the flag would over-report
// every housekeeping summary by the same factor.
func TestCostHaiku55LongPromptTier(t *testing.T) {
	for _, axis := range []struct {
		name       string
		u          provider.TokenUsage
		short, lng float64
	}{
		{"input", provider.TokenUsage{InputTokens: 1_000_000}, 0.10, 0.50},
		{"output", provider.TokenUsage{OutputTokens: 1_000_000}, 0.50, 2.50},
		{"cache write", provider.TokenUsage{CacheCreateTokens: 1_000_000}, 0.125, 0.625},
		{"cache read", provider.TokenUsage{CacheReadTokens: 1_000_000}, 0.01, 0.05},
	} {
		if got := defaultPricing.Cost("claude-haiku-5-5", axis.u); math.Abs(got-axis.short) > 1e-9 {
			t.Errorf("%s: standard Cost = %v, want %v", axis.name, got, axis.short)
		}
		long := axis.u
		long.LongPrompt = true
		if got := defaultPricing.Cost("claude-haiku-5-5", long); math.Abs(got-axis.lng) > 1e-9 {
			t.Errorf("%s: long-prompt Cost = %v, want %v", axis.name, got, axis.lng)
		}
	}
	// A model with one rate ignores the flag rather than pricing at 0.
	u := provider.TokenUsage{InputTokens: 1_000_000}
	flagged := u
	flagged.LongPrompt = true
	if a, b := defaultPricing.Cost("claude-opus-5-5", flagged), defaultPricing.Cost("claude-opus-5-5", u); a != b {
		t.Errorf("opus-5-5 with LongPrompt = %v, want its one rate %v", a, b)
	}
}

// TestCostSonnet55Rates pins Sonnet 5.5's four published rates ($2
// input, $10 output, $2.50 5-minute cache write, $0.10 cache read).
// Every rate but the cache read matches Sonnet 5, so a row copied from
// its predecessor passes every check except this one, and it would
// double the reported cache-read spend of every subagent on the
// default model.
func TestCostSonnet55Rates(t *testing.T) {
	for _, axis := range []struct {
		name string
		u    provider.TokenUsage
		want float64
	}{
		{"input", provider.TokenUsage{InputTokens: 1_000_000}, 2.00},
		{"output", provider.TokenUsage{OutputTokens: 1_000_000}, 10.00},
		{"cache write", provider.TokenUsage{CacheCreateTokens: 1_000_000}, 2.50},
		{"cache read", provider.TokenUsage{CacheReadTokens: 1_000_000}, 0.10},
	} {
		if got := defaultPricing.Cost("claude-sonnet-5-5", axis.u); math.Abs(got-axis.want) > 1e-9 {
			t.Errorf("%s: sonnet-5-5 Cost = %v, want %v", axis.name, got, axis.want)
		}
	}
}

// TestCostFable52RatesAndBelowFable51 pins Fable 5.2's four published
// rates ($8 input, $40 output, $10 5-minute cache write, $0.20 cache
// read). Its cache read is 0.025x its input rate, so a row copied from
// Fable 5.1 or derived from InputPerMTok would misprice the line that
// dominates a long-running Fable agent's bill. It also checks 5.2 is
// below 5.1 on every axis: agents pinned to 5.1 move to 5.2 at boot,
// and a row priced above its predecessor would make that move raise
// their reported spend.
func TestCostFable52RatesAndBelowFable51(t *testing.T) {
	for _, axis := range []struct {
		name string
		u    provider.TokenUsage
		want float64
	}{
		{"input", provider.TokenUsage{InputTokens: 1_000_000}, 8.00},
		{"output", provider.TokenUsage{OutputTokens: 1_000_000}, 40.00},
		{"cache write", provider.TokenUsage{CacheCreateTokens: 1_000_000}, 10.00},
		{"cache read", provider.TokenUsage{CacheReadTokens: 1_000_000}, 0.20},
	} {
		newer := defaultPricing.Cost("claude-fable-5-2", axis.u)
		if math.Abs(newer-axis.want) > 1e-9 {
			t.Errorf("%s: fable-5-2 Cost = %v, want %v", axis.name, newer, axis.want)
		}
		if older := defaultPricing.Cost("claude-fable-5-1", axis.u); newer >= older {
			t.Errorf("%s: fable-5-2 Cost %v is not below fable-5-1 %v", axis.name, newer, older)
		}
	}
}

// TestCostPricesFoundryDeployments: on Microsoft Foundry, --model names
// a deployment, and Kivali needs each deployment named after the
// model's catalog id (SelectableModelIDs), so the model a Foundry usage
// row carries is that id, prefixed or not, and prices at its row's
// rate like any other.
func TestCostPricesFoundryDeployments(t *testing.T) {
	d := New(Options{})
	u := provider.TokenUsage{InputTokens: 1_000_000, OutputTokens: 100_000}
	ids := SelectableModelIDs()
	if len(ids) == 0 {
		t.Fatal("no selectable models")
	}
	for _, id := range ids {
		want, ok := defaultPricing.CostOK(id, u)
		if !ok || want == 0 {
			t.Errorf("%s is unpriced", id)
			continue
		}
		for _, row := range []string{id, ProviderName + ":" + id} {
			if got := d.Price(row, u); got != want {
				t.Errorf("Price(%q) = %v, want %v", row, got, want)
			}
		}
	}
}
