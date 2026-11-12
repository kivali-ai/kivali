package web

import (
	"context"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// directChatRows returns the agent's persisted direct_chat rows in
// order — the bubbles a page reload would render.
func directChatRows(t *testing.T, srv *Server, slug string) []store.ChatMessage {
	t.Helper()
	hist, err := srv.Store.ReadChatHistory(slug)
	if err != nil {
		t.Fatalf("read hist: %v", err)
	}
	var out []store.ChatMessage
	for _, m := range hist {
		if m.Kind == "direct_chat" {
			out = append(out, m)
		}
	}
	return out
}

// TestBubbleStampsAnsweringModelNotRequestedModel: the bubble's model
// comes from the id the CLI reports on the assistant message that
// produced the text, not from the CompleteRequest. A request can be
// served by a different model than it named, so the requested id
// would be an assumption rendered as fact.
func TestBubbleStampsAnsweringModelNotRequestedModel(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	// The harness asks for mock-large-0 (see startTurnEventTestServer).
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	if err := c.PostTurnEvent(ctx, "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDelta, Text: "served by fable", Model: "mock-large",
	}); err != nil {
		t.Fatalf("post delta: %v", err)
	}
	if err := c.PostTurnEvent(ctx, "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("post done: %v", err)
	}

	rows := directChatRows(t, srv, "alice")
	if len(rows) != 1 {
		t.Fatalf("direct_chat rows = %d, want 1", len(rows))
	}
	if got, want := rows[0].Model, "mock-large"; got != want {
		t.Errorf("bubble model = %q, want %q (what answered, not mock-large-0 which is what we asked for)", got, want)
	}
}

// TestBubbleSplitsOnMidTurnModelChange covers a turn whose calls were
// not all served by the same model. Each bubble has to carry the model
// that wrote ITS text — one merged bubble would attribute the second
// model's words to the first.
func TestBubbleSplitsOnMidTurnModelChange(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	for _, d := range []struct{ text, model string }{
		{"fable wrote this ", "mock-large"},
		{"and this ", "mock-large"},
		{"opus wrote this", "mock-small"},
	} {
		if err := c.PostTurnEvent(ctx, "turn-1", agentpod.TurnEvent{
			Kind: agentpod.TurnEventDelta, Text: d.text, Model: d.model,
		}); err != nil {
			t.Fatalf("post delta: %v", err)
		}
	}
	if err := c.PostTurnEvent(ctx, "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("post done: %v", err)
	}

	rows := directChatRows(t, srv, "alice")
	if len(rows) != 2 {
		t.Fatalf("direct_chat rows = %d, want 2 (one per answering model)", len(rows))
	}
	if got, want := rows[0].Model, "mock-large"; got != want {
		t.Errorf("rows[0].Model = %q, want %q", got, want)
	}
	if got, want := rows[0].Content, "fable wrote this and this "; got != want {
		t.Errorf("rows[0].Content = %q, want %q (both fable deltas, nothing from opus)", got, want)
	}
	if got, want := rows[1].Model, "mock-small"; got != want {
		t.Errorf("rows[1].Model = %q, want %q", got, want)
	}
	if got, want := rows[1].Content, "opus wrote this"; got != want {
		t.Errorf("rows[1].Content = %q, want %q", got, want)
	}
}

// TestMixedModelTurnWritesUsageRowPerModel is the billing half.
//
// Token counts accumulate across a turn's calls, and a turn can be
// served by more than one model. One row per model keeps each token
// priced at the model that served it: Fable and Opus differ by 2x, and
// the settings page derives spend from tokens × the row's model (it
// deliberately does not sum CostUSD).
func TestMixedModelTurnWritesUsageRowPerModel(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:       agentpod.TurnEventDone,
		StopReason: "end_turn",
		// Flat fields stay the turn total.
		InputTokens:  3000,
		OutputTokens: 300,
		Model:        "mock-large",
		ByModel: []agentpod.TurnModelUsage{
			{Model: "mock-large", InputTokens: 1000, OutputTokens: 100},
			{Model: "mock-small", InputTokens: 2000, OutputTokens: 200},
		},
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	recs, err := srv.Store.ReadUsageSince(before)
	if err != nil {
		t.Fatalf("ReadUsageSince: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("usage records = %d, want 2 (one per answering model)", len(recs))
	}

	byModel := map[string]store.UsageRecord{}
	for _, r := range recs {
		byModel[r.Model] = r
		if r.Agent != "alice" {
			t.Errorf("Agent = %q, want alice", r.Agent)
		}
		if r.Purpose != "chat" {
			t.Errorf("Purpose = %q, want chat", r.Purpose)
		}
	}
	fable, ok := byModel["mock-large"]
	if !ok {
		t.Fatalf("no row for mock-large: %+v", recs)
	}
	opus, ok := byModel["mock-small"]
	if !ok {
		t.Fatalf("no row for mock-small: %+v", recs)
	}
	if fable.InputTokens != 1000 || fable.OutputTokens != 100 {
		t.Errorf("fable row tokens = in=%d out=%d, want 1000/100", fable.InputTokens, fable.OutputTokens)
	}
	if opus.InputTokens != 2000 || opus.OutputTokens != 200 {
		t.Errorf("opus row tokens = in=%d out=%d, want 2000/200", opus.InputTokens, opus.OutputTokens)
	}

	// The rows reconstruct the turn's total — no tokens invented or lost.
	if got := fable.InputTokens + opus.InputTokens; got != 3000 {
		t.Errorf("rows sum to %d input tokens, want 3000 (the turn total)", got)
	}

	// Each row prices at its OWN model's rate. The whole point: billing
	// opus's 2,000 tokens at fable's rate is the bug.
	wantOpus := srv.Provider.Price("mock-small", provider.TokenUsage{
		InputTokens: 2000, OutputTokens: 200,
	})
	if opus.CostUSD != wantOpus {
		t.Errorf("opus row CostUSD = %v, want %v (priced as opus, not fable)", opus.CostUSD, wantOpus)
	}
	if wantOpus == 0 {
		t.Fatal("provider priced mock-small at 0 — test proves nothing")
	}
}

// TestSingleModelTurnStillWritesOneRow pins the unmixed path: a pod
// that sends no split (every current single-model turn, and any pod
// predating the field) produces exactly one row, as before.
func TestSingleModelTurnStillWritesOneRow(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventDone,
		StopReason:   "end_turn",
		InputTokens:  1000,
		OutputTokens: 200,
		Model:        "mock-small",
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
	if recs[0].Model != "mock-small" {
		t.Errorf("Model = %q, want mock-small", recs[0].Model)
	}
	if recs[0].InputTokens != 1000 || recs[0].OutputTokens != 200 {
		t.Errorf("tokens = in=%d out=%d, want 1000/200", recs[0].InputTokens, recs[0].OutputTokens)
	}
}

// TestLongPromptTierReachesUsageRows is core's half of the long-prompt
// tier. A model's calls above its long-prompt threshold bill at other
// rates, and usage.jsonl is re-priced on read, so the tier has to be
// stored on the row: a turn on both sides of the threshold writes one
// row per tier.
func TestLongPromptTierReachesUsageRows(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventDone,
		StopReason:   "end_turn",
		InputTokens:  30,
		OutputTokens: 300,
		LongPrompt:   true,
		Model:        "mock-large",
		ByModel: []agentpod.TurnModelUsage{
			{Model: "mock-large", InputTokens: 20, OutputTokens: 200},
			{Model: "mock-large", InputTokens: 10, OutputTokens: 100, LongPrompt: true},
		},
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	recs, err := srv.Store.ReadUsageSince(before)
	if err != nil {
		t.Fatalf("ReadUsageSince: %v", err)
	}
	if len(recs) != 2 || recs[0].LongPrompt || !recs[1].LongPrompt || recs[1].InputTokens != 10 {
		t.Errorf("usage records = %+v, want a standard row then a long-prompt row of 10 input tokens", recs)
	}
}

// TestSingleBucketLongPromptTurnKeepsItsTier: a turn whose calls were
// all above the threshold sends no split, only the flat counts and
// their flag, and its one row must keep the flag.
func TestSingleBucketLongPromptTurnKeepsItsTier(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventDone,
		StopReason:   "end_turn",
		InputTokens:  10,
		OutputTokens: 100,
		LongPrompt:   true,
		Model:        "mock-large",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	recs, err := srv.Store.ReadUsageSince(before)
	if err != nil {
		t.Fatalf("ReadUsageSince: %v", err)
	}
	if len(recs) != 1 || !recs[0].LongPrompt {
		t.Errorf("usage records = %+v, want one long-prompt row", recs)
	}
}

// TestOneHourCacheWritesReachUsageRows is core's half of the
// cache-write split. usage.jsonl is re-priced on read, so a row has to
// keep how many of its cache writes had a one-hour lifetime, on the
// flat path and on each bucket of a split, or every spend readout
// prices them at the cheaper five-minute rate.
func TestOneHourCacheWritesReachUsageRows(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:                agentpod.TurnEventDone,
		StopReason:          "end_turn",
		CacheCreateTokens:   900,
		CacheCreate1hTokens: 700,
		Model:               "mock-large",
		ByModel: []agentpod.TurnModelUsage{
			{Model: "mock-large", CacheCreateTokens: 600, CacheCreate1hTokens: 500},
			{Model: "mock-small", CacheCreateTokens: 300, CacheCreate1hTokens: 200},
		},
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	recs, err := srv.Store.ReadUsageSince(before)
	if err != nil {
		t.Fatalf("ReadUsageSince: %v", err)
	}
	if len(recs) != 2 || recs[0].CacheCreate1hTokens != 500 || recs[1].CacheCreate1hTokens != 200 {
		t.Errorf("usage records = %+v, want 500 and 200 one-hour writes", recs)
	}

	var got provider.TokenUsage
	if _, ok := priceUsage(pricingSpy{got: &got}, recs[0]); !ok {
		t.Fatal("mock-large is unpriced")
	}
	if got.CacheCreate1hTokens != 500 {
		t.Errorf("provider priced %+v, want the row's 500 one-hour writes", got)
	}
}

// pricingSpy is the mock provider with Price recording what it was
// asked to price.
type pricingSpy struct {
	provider.MockProvider
	got *provider.TokenUsage
}

func (p pricingSpy) Price(id string, u provider.TokenUsage) float64 {
	*p.got = u
	return p.MockProvider.Price(id, u)
}

// TestPriceUsageReadsLongPromptTier: the dashboards re-price every row
// from its tokens, so the stored tier has to reach the provider.
// Without it a Haiku 5.5 row above 100k prompt tokens reads at a fifth
// of its cost on every spend readout.
func TestPriceUsageReadsLongPromptTier(t *testing.T) {
	var got provider.TokenUsage
	p := pricingSpy{got: &got}
	row := store.UsageRecord{Model: "mock-large", InputTokens: 10, LongPrompt: true}
	if _, ok := priceUsage(p, row); !ok {
		t.Fatal("mock-large is unpriced")
	}
	if !got.LongPrompt || got.InputTokens != 10 {
		t.Errorf("provider priced %+v, want the row's tokens with LongPrompt", got)
	}
}
