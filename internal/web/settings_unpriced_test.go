package web

import (
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// usage24h reads the last-24-hours column of GET /api/v1/org/usage.
func usage24h(t *testing.T, srv *Server) apitypes.UsageWindow {
	t.Helper()
	return orgOK[apitypes.Usage](t, orgGet(t, srv, "/api/v1/org/usage")).Windows.H24
}

// TestSettingsSurfacesUnpricedTokens: a usage row whose model has no
// price (an id not in DefaultPricing) cannot be priced, and the rollup
// must say so out loud instead of counting real, billed tokens as $0
// of spend.
func TestSettingsSurfacesUnpricedTokens(t *testing.T) {
	srv := newTestServer(t)

	// A priced row: Mock Large 0, 1M input @ $5/MTok = $5.00.
	if err := srv.Store.AppendUsage(store.UsageRecord{
		TS:          time.Now().UTC(),
		Agent:       "cos",
		Purpose:     "chat",
		Model:       "mock-large-0",
		InputTokens: 1_000_000,
	}); err != nil {
		t.Fatalf("append priced usage: %v", err)
	}
	// A row for a model with no price.
	if err := srv.Store.AppendUsage(store.UsageRecord{
		TS:              time.Now().UTC(),
		Agent:           "docs-writer",
		Purpose:         "chat",
		Model:           "model-without-a-price",
		CacheReadTokens: 27_324_359,
	}); err != nil {
		t.Fatalf("append unpriced usage: %v", err)
	}

	w := usage24h(t, srv)
	// The dollar figure still only counts what we can actually price —
	// inventing a rate for an unknown model would be worse than saying
	// we don't have one.
	if !nearUSD(w.Spend, 5.00) {
		t.Errorf("spend = %v, want the priced $5.00", w.Spend)
	}
	// But the unpriced tokens are visible rather than silently dropped.
	if w.Unpriced != 27_324_359 || w.UnpricedCalls != 1 {
		t.Errorf("unpriced = %d tokens over %d calls, want 27324359 over 1", w.Unpriced, w.UnpricedCalls)
	}
}

// TestSettingsPricesStoredSpellingsThroughTheProvider: a usage row's
// model id is stored as the driver reported it and resolved at read
// time, so any spelling the provider resolves (here the "<name>:"
// prefix) is priced, and stays out of the unpriced bucket. The Claude
// provider's own spellings (dated snapshots, [1m]) are tested in
// internal/claudeagent.
func TestSettingsPricesStoredSpellingsThroughTheProvider(t *testing.T) {
	srv := newTestServer(t)

	// 1M input on mock-small @ $1/MTok = $1.00.
	if err := srv.Store.AppendUsage(store.UsageRecord{
		TS:          time.Now().UTC(),
		Agent:       "cos",
		Purpose:     "inbox_summary",
		Model:       "mock:mock-small",
		InputTokens: 1_000_000,
	}); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	w := usage24h(t, srv)
	if !nearUSD(w.Spend, 1.00) {
		t.Errorf("spend = %v, want $1.00", w.Spend)
	}
	if w.Unpriced != 0 || w.UnpricedCalls != 0 {
		t.Errorf("a prefixed id is unpriced: %d tokens, %d calls", w.Unpriced, w.UnpricedCalls)
	}
}

// TestSettingsNoUnpricedNoteWhenAllPriced keeps the annotation quiet in
// the normal case.
func TestSettingsNoUnpricedNoteWhenAllPriced(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.AppendUsage(store.UsageRecord{
		TS:          time.Now().UTC(),
		Agent:       "cos",
		Purpose:     "chat",
		Model:       "mock-large-0",
		InputTokens: 1_000_000,
	}); err != nil {
		t.Fatalf("append usage: %v", err)
	}

	if w := usage24h(t, srv); w.Unpriced != 0 || w.UnpricedCalls != 0 {
		t.Errorf("unpriced = %d tokens over %d calls with nothing unpriced", w.Unpriced, w.UnpricedCalls)
	}
}
