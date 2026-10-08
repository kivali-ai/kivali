package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// startTurnEventTestServer is the per-test fixture for 4-2 streamed-
// event handling: a real Store, the agent stub seeded, and the
// turn-event endpoint registered on a Unix socket. The returned
// chatHub is pre-installed in the per-slug turn state map so tests
// can assert the broadcast event log without driving the full
// chat-spawn flip (workstream 4-3 territory).
func startTurnEventTestServer(t *testing.T, slug, turnID string) (path string, srv *Server, hub *chatHub, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	// The fleet default sizes the fill gauge at 200k: the tests below
	// seed history against that window.
	srv.AgentModel = provider.MockModelSmall
	if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	hub = &chatHub{hub: newHub(), slug: slug, spawnSource: "chat"}
	// Install the hub on chatHubs too so deliverToAgent's busy-check
	// matches what the production spawn flip will set up: the same
	// hub lives in both chatHubs[slug] and the agentpodTurns state.
	if srv.chatHubs == nil {
		srv.chatHubs = map[string]*chatHub{}
	}
	srv.chatHubs[slug] = hub
	srv.InitAgentpodTurnState(slug, "chat", turnID, hub, "mock-large-0", "high")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turn_id}/event", srv.handleAgentpodTurnEvent)
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return path, srv, hub, cleanup
}

// hubEventKinds reads back the broadcast event log from the hub's
// subscribe() replay buffer in order. Useful for asserting that
// handler dispatch produced the expected sequence of UI emits.
func hubEventKinds(h *chatHub) []string {
	replay, _, _ := h.subscribe()
	out := make([]string, 0, len(replay))
	for _, ev := range replay {
		out = append(out, ev.Kind)
	}
	return out
}

// hubEventByKind returns the first replayed event with the matching
// kind, decoded into a generic map. Returns (nil, false) when no such
// event was broadcast.
func hubEventByKind(h *chatHub, kind string) (map[string]any, bool) {
	replay, _, _ := h.subscribe()
	for _, ev := range replay {
		if ev.Kind != kind {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err == nil {
			return m, true
		}
	}
	return nil, false
}

// TestAgentpodTurnDeltaEmitsAndBuffers locks in the delta path:
// posting a delta to the turn-event endpoint emits a "delta" event
// to the chat hub but does NOT yet write a chat.jsonl row — the
// assistant text only flushes on tool_use_start or done.
func TestAgentpodTurnDeltaEmitsAndBuffers(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDelta, Text: "hello ",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDelta, Text: "world",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	if got := hubEventKinds(hub); len(got) != 2 || got[0] != "delta" || got[1] != "delta" {
		t.Errorf("emit log = %v, want [delta delta]", got)
	}
	// The delta payload carries the friendly model name so the live
	// streaming bubble renders the same model pill the reloaded bubble
	// gets — derived server-side from the turn's resolved model.
	if d, ok := hubEventByKind(hub, "delta"); !ok {
		t.Error("no delta event emitted")
	} else if d["model"] != "Mock Large 0" {
		t.Errorf("delta model = %v, want %q", d["model"], "Mock Large 0")
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Kind == "direct_chat" {
			t.Errorf("delta-only run wrote a direct_chat row prematurely: %+v", m)
		}
	}
}

// TestAgentpodTurnThinkingCountsSteps locks in the extended-reasoning
// progress path: each thinking event increments an absolute step
// counter emitted to the hub (the signal that works even when the model
// encrypts its reasoning), a one-line summary rides along when the text
// is present, and — being ephemeral — nothing is written to chat.jsonl.
func TestAgentpodTurnThinkingCountsSteps(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	// First step: Opus-style encrypted thinking — no text, just the
	// marker. Must still count.
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventThinking,
	}); err != nil {
		t.Fatalf("PostTurnEvent step 1: %v", err)
	}
	// Second step: carries reasoning text (Sonnet/Haiku-style).
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventThinking,
		Text: "Let me check the budget figures.\nThen cross-reference.",
	}); err != nil {
		t.Fatalf("PostTurnEvent step 2: %v", err)
	}

	// The hub should have two thinking events; the latest carries step=2
	// and the summary.
	var last map[string]any
	replay, _, _ := hub.subscribe()
	count := 0
	for _, ev := range replay {
		if ev.Kind != "thinking" {
			continue
		}
		count++
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err == nil {
			last = m
		}
	}
	if count != 2 {
		t.Fatalf("thinking emits = %d, want 2; replay = %v", count, hubEventKinds(hub))
	}
	if step, _ := last["step"].(float64); step != 2 {
		t.Errorf("latest step = %v, want 2", last["step"])
	}
	if summary, _ := last["summary"].(string); summary != "Let me check the budget figures." {
		t.Errorf("summary = %q, want first sentence only", summary)
	}
	// Thinking is ephemeral — it must not land in chat.jsonl.
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) != 0 {
		t.Errorf("thinking wrote %d chat rows, want 0: %+v", len(hist), hist)
	}
}

// TestAgentpodTurnThinkingResetsPerInterval locks in the per-interval
// step semantics: reasoning steps count between messages, so when a
// message goes back to the model (a text delta or a tool call) the
// counter resets and the next reasoning interval restarts at step 1 —
// it is NOT a cumulative turn total.
func TestAgentpodTurnThinkingResetsPerInterval(t *testing.T) {
	path, _, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	post := func(ev agentpod.TurnEvent) {
		if err := c.PostTurnEvent(context.Background(), "turn-1", ev); err != nil {
			t.Fatalf("PostTurnEvent %s: %v", ev.Kind, err)
		}
	}
	// Interval 1: two reasoning steps, then the model emits text.
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventThinking})
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventThinking})
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "partial answer"})
	// Interval 2 (after the text message): one reasoning step, then a tool.
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventThinking})
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventToolUseStart, ToolUseID: "tu-1", ToolName: "file_view"})
	// Interval 3 (after the tool message): one reasoning step.
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventThinking})

	// Collect the step number from each thinking emit in order.
	var steps []int
	replay, _, _ := hub.subscribe()
	for _, ev := range replay {
		if ev.Kind != "thinking" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatalf("decode thinking: %v", err)
		}
		steps = append(steps, int(m["step"].(float64)))
	}
	// Interval 1: 1,2 — interval 2 (post-text): 1 — interval 3 (post-tool): 1.
	want := []int{1, 2, 1, 1}
	if len(steps) != len(want) {
		t.Fatalf("step sequence = %v, want %v", steps, want)
	}
	for i := range want {
		if steps[i] != want[i] {
			t.Errorf("step[%d] = %d, want %d (full seq %v, want %v)", i, steps[i], want[i], steps, want)
		}
	}
}

// TestThinkingSummary covers the condensing heuristic in isolation:
// first line / first sentence, whitespace collapse, markdown stripping,
// word-boundary truncation, and the empty-in/empty-out contract that
// onAgentpodThinking relies on to drop no-op events.
func TestThinkingSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "   \n\t ", ""},
		{"first sentence wins", "I should add the index. The rest follows.", "I should add the index."},
		{"first line wins over later paragraphs", "Planning the migration\n\nstep two details", "Planning the migration"},
		{"whitespace collapsed", "checking    the\tlogs now", "checking the logs now"},
		{"markdown stripped", "**Reviewing** the `auth` flow", "Reviewing the auth flow"},
		{
			"long text truncates on word boundary",
			strings.Repeat("word ", 60),
			// 140-char cap, trimmed to a word boundary, then an ellipsis.
			strings.TrimSpace(strings.Repeat("word ", 28)) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := thinkingSummary(tc.in)
			if got != tc.want {
				t.Errorf("thinkingSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > thinkingMaxSummaryLen+len("…") {
				t.Errorf("summary length %d exceeds cap %d", len(got), thinkingMaxSummaryLen)
			}
		})
	}
}

// TestAgentpodTurnToolUseStartFlushesAssistant locks in the
// flushAssistantText behavior: a tool_use_start after deltas writes
// the assembled text as a kind:direct_chat row before forwarding the
// tool_use_start event.
func TestAgentpodTurnToolUseStartFlushesAssistant(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	for _, text := range []string{"thinking ", "out loud "} {
		if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
			Kind: agentpod.TurnEventDelta, Text: text,
		}); err != nil {
			t.Fatalf("delta: %v", err)
		}
	}
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventToolUseStart, ToolUseID: "tu-1", ToolName: "file_view",
	}); err != nil {
		t.Fatalf("tool_use_start: %v", err)
	}

	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("read hist: %v", err)
	}
	var direct *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "direct_chat" {
			m := hist[i]
			direct = &m
			break
		}
	}
	if direct == nil {
		t.Fatal("no direct_chat row written; flush did not happen")
	}
	if direct.Content != "thinking out loud " {
		t.Errorf("direct_chat content = %q, want %q", direct.Content, "thinking out loud ")
	}
	// The flushed row records the turn's resolved model so the UI can
	// pin a friendly-name pill on the model's own bubble.
	if direct.Model != "mock-large-0" {
		t.Errorf("direct_chat model = %q, want mock-large-0", direct.Model)
	}
	// It records the resolved effort the same way, so the UI can pin an
	// effort pill alongside the model pill.
	if direct.Effort != "high" {
		t.Errorf("direct_chat effort = %q, want high", direct.Effort)
	}
	// flushAssistant calls hub.TrimDeltas, which prunes the prior
	// delta replay entries so a refresh after persisting the
	// direct_chat row doesn't double-paint via the SSE replay. After
	// the trim the log is just [tool_use_start].
	kinds := hubEventKinds(hub)
	if len(kinds) != 1 || kinds[0] != "tool_use_start" {
		t.Errorf("emit log = %v, want [tool_use_start] post-trim", kinds)
	}
}

// TestAgentpodTurnToolUseEndPersistsAndEmits asserts that a
// tool_use_end event writes a kind:tool_use chat row carrying the
// full input + emits the tool_use UI event with the corresponding
// payload shape.
func TestAgentpodTurnToolUseEndPersistsAndEmits(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:      agentpod.TurnEventToolUseEnd,
		ToolUseID: "tu-1",
		ToolName:  "file_view",
		ToolInput: []byte(`{"path":"/files/notes/ideas.md"}`),
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	var tu *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "tool_use" {
			m := hist[i]
			tu = &m
			break
		}
	}
	if tu == nil {
		t.Fatal("no tool_use row written")
	}
	if tu.ToolUseID != "tu-1" || tu.ToolName != "file_view" {
		t.Errorf("tool_use row = %+v, want id=tu-1 name=file_view", tu)
	}
	if !bytes.Equal([]byte(tu.ToolInput), []byte(`{"path":"/files/notes/ideas.md"}`)) {
		t.Errorf("tool_use input = %q, want exact JSON roundtrip", tu.ToolInput)
	}
	got, ok := hubEventByKind(hub, "tool_use")
	if !ok {
		t.Fatal("no tool_use emit on hub")
	}
	if got["tool_use_id"] != "tu-1" || got["tool_name"] != "file_view" {
		t.Errorf("tool_use emit payload = %+v, want id=tu-1 name=file_view", got)
	}
}

// TestAgentpodTurnToolResultPersistsAndEmits asserts the tool_result
// path: chat row written, hub emit fires, no synthetic doc_published
// for a non-publish/non-share_file tool.
func TestAgentpodTurnToolResultPersistsAndEmits(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	// Pretend the model already issued a file_view tool_use. The pending
	// tool entry is what triggers (or skips) the doc_published synthesis;
	// for file_view we expect tool_result emit only.
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:      agentpod.TurnEventToolUseEnd,
		ToolUseID: "tu-1", ToolName: "file_view",
		ToolInput: []byte(`{"path":"/files/x"}`),
	}); err != nil {
		t.Fatalf("tool_use_end: %v", err)
	}
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:           agentpod.TurnEventToolResult,
		ToolUseID:      "tu-1",
		ToolResultText: "x: 42 bytes",
	}); err != nil {
		t.Fatalf("tool_result: %v", err)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	var tr *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == "tool_result" {
			m := hist[i]
			tr = &m
			break
		}
	}
	if tr == nil {
		t.Fatal("no tool_result row written")
	}
	if tr.Content != "x: 42 bytes" || tr.ToolUseID != "tu-1" {
		t.Errorf("tool_result row = %+v", tr)
	}
	kinds := hubEventKinds(hub)
	saw := map[string]bool{}
	for _, k := range kinds {
		saw[k] = true
	}
	if !saw["tool_result"] {
		t.Errorf("expected tool_result emit, got %v", kinds)
	}
	if saw["doc_published"] || saw["file_shared"] {
		t.Errorf("file_view result must not synthesize doc_published/file_shared, got %v", kinds)
	}
}

// TestAgentpodTurnToolResultErrorSkipsSynthesis guards the
// is_error=true path: even for a publish_* pending tool, an errored
// tool_result must not synthesize doc_published — the publish failed.
func TestAgentpodTurnToolResultErrorSkipsSynthesis(t *testing.T) {
	path, _, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:      agentpod.TurnEventToolUseEnd,
		ToolUseID: "tu-1", ToolName: "publish_notice",
		ToolInput: []byte(`{"title":"x","to":["alice"]}`),
	}); err != nil {
		t.Fatalf("tool_use_end: %v", err)
	}
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:              agentpod.TurnEventToolResult,
		ToolUseID:         "tu-1",
		ToolResultText:    "publish_notice: validation error",
		ToolResultIsError: true,
	}); err != nil {
		t.Fatalf("tool_result: %v", err)
	}
	if _, ok := hubEventByKind(hub, "doc_published"); ok {
		t.Errorf("errored publish must not emit doc_published")
	}
}

// TestAgentpodTurnUnknownSlugDropped guards the no-state path: an
// event for a slug with no in-flight turn returns 204 (no error)
// AND leaves chat.jsonl untouched. Stale post-turn events from a
// flushing runtime must not write rows.
func TestAgentpodTurnUnknownSlugDropped(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turn_id}/event", srv.handleAgentpodTurnEvent)

	// Direct in-process call — no socket needed for this case.
	body := `{"kind":"delta","text":"orphan"}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/agent/alice/chat-turn/turn-stale/event", bytes.NewReader([]byte(body)))
	req.Header.Set(agentpod.SlugHeader, "alice")
	rr := newRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for stale event", rr.Code)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) > 0 {
		t.Errorf("stale event wrote rows: %+v", hist)
	}
}

// TestAgentpodTurnIDMismatchDropped guards against a fresh chat-turn
// racing ahead of the prior turn's runtime drain: an event for slug
// alice but with a different turnID than the registered state must
// 204 silently and not write rows. Mirrors the spawnReceivedTS race
// guard in the in-process spawn defer.
func TestAgentpodTurnIDMismatchDropped(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-current")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-stale", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDelta, Text: "from a previous life",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	if got := hubEventKinds(hub); len(got) != 0 {
		t.Errorf("stale-turn event emitted %v, want none", got)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) > 0 {
		t.Errorf("stale-turn event wrote rows: %+v", hist)
	}
}

// TestAgentpodTurnShareFileSynthesizesEmit asserts the share_file
// follow-up: a successful tool_result for a share_file tool_use
// emits a synthetic file_shared event so the live UI paints the
// inline-attachment chip immediately. Multi-file: the emit's `files`
// array carries one entry per file in the bubble.
func TestAgentpodTurnShareFileSynthesizesEmit(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	at := time.Date(2026, 9, 28, 14, 3, 0, 0, time.UTC)
	srv.Clock = clock.NewFakeAt(at)

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:      agentpod.TurnEventToolUseEnd,
		ToolUseID: "tu-1", ToolName: agent.ShareFileToolName,
		ToolInput: []byte(`{"files":[{"path":"/files/project/plan.md"},{"path":"/files/project/chart.png"}],"caption":"see chart 4"}`),
	}); err != nil {
		t.Fatalf("tool_use_end: %v", err)
	}
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:      agentpod.TurnEventToolResult,
		ToolUseID: "tu-1",
		ToolResultText: "Shared in this conversation:\n" +
			"- plan.md (sha=abc123)\n" +
			"- chart.png (sha=def456)\n" +
			"Caption: see chart 4",
	}); err != nil {
		t.Fatalf("tool_result: %v", err)
	}
	got, ok := hubEventByKind(hub, "file_shared")
	if !ok {
		t.Fatal("share_file: no synthetic file_shared emit")
	}
	files, ok := got["files"].([]any)
	if !ok {
		t.Fatalf("file_shared.files missing or wrong type: %+v", got)
	}
	if len(files) != 2 {
		t.Fatalf("files len = %d, want 2 (multi-file emit)", len(files))
	}
	first, _ := files[0].(map[string]any)
	if first["sha"] != "abc123" || first["name"] != "plan.md" {
		t.Errorf("files[0] = %+v, want sha=abc123 name=plan.md", first)
	}
	second, _ := files[1].(map[string]any)
	if second["sha"] != "def456" || second["name"] != "chart.png" {
		t.Errorf("files[1] = %+v, want sha=def456 name=chart.png", second)
	}
	if got["caption"] != "see chart 4" {
		t.Errorf("caption = %v, want 'see chart 4'", got["caption"])
	}
	// ts is unix ms, as chat_message carries, so the transcript can
	// place the chip.
	if ts, _ := got["ts"].(float64); int64(ts) != at.UnixMilli() {
		t.Errorf("ts = %v, want %d", got["ts"], at.UnixMilli())
	}
}

// TestAgentpodTurnDoneTearsDownState locks in the post-loop teardown:
// after a TurnEventDone the per-slug state is dropped, the chatHub is
// marked completed, the active-turn marker is cleared, and a "done"
// SSE event lands on the hub with the wire-shape browsers expect.
func TestAgentpodTurnDoneTearsDownState(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	// Mark an active turn so we can verify Clear runs.
	if err := srv.Store.MarkActiveTurn("alice", store.ActiveTurnMarker{
		StartedAt: time.Now().UTC(),
		Source:    "chat",
		Model:     "claude-test",
	}); err != nil {
		t.Fatalf("seed active turn: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventDone,
		StopReason:   "end_turn",
		InputTokens:  100,
		OutputTokens: 25,
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	if got, ok := hubEventByKind(hub, "done"); !ok {
		t.Errorf("no done emit on hub; replay = %v", hubEventKinds(hub))
	} else {
		if got["stop"] != "end_turn" {
			t.Errorf("done.stop = %v, want end_turn", got["stop"])
		}
		if got["rotation"] != false {
			t.Errorf("done.rotation = %v, want false (no rotation queued)", got["rotation"])
		}
	}
	if !hub.isCompleted() {
		t.Errorf("hub not marked completed after done")
	}
	if srv.lookupAgentpodTurn("alice") != nil {
		t.Errorf("agentpodTurns entry not dropped after done")
	}
	// ReadActiveTurn returns zero value (no error) when the marker
	// has been cleared; a non-zero StartedAt means Clear didn't run.
	if m, _ := srv.Store.ReadActiveTurn("alice"); !m.StartedAt.IsZero() {
		t.Errorf("active-turn marker still present after done: %+v", m)
	}
}

// TestAgentpodTurnDoneAppendsUsageRecord locks in usage persistence
// across the agent-pod boundary: the model call happens inside the
// agent pod whose claudeagent.Client has no Recorder, so the only
// place core can write usage.jsonl is the TurnEventDone handler.
// Without it, the settings page's "API calls (last 24h)" counter
// would sit at 0 regardless of actual traffic.
func TestAgentpodTurnDoneAppendsUsageRecord(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:              agentpod.TurnEventDone,
		StopReason:        "end_turn",
		InputTokens:       1000,
		OutputTokens:      200,
		CacheReadTokens:   500,
		CacheCreateTokens: 100,
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
	if r.Agent != "alice" {
		t.Errorf("Agent = %q, want alice", r.Agent)
	}
	if r.Purpose != "chat" {
		t.Errorf("Purpose = %q, want chat", r.Purpose)
	}
	if r.Model != "mock-large-0" {
		t.Errorf("Model = %q, want mock-large-0", r.Model)
	}
	if r.InputTokens != 1000 || r.OutputTokens != 200 || r.CacheReadTokens != 500 || r.CacheCreateTokens != 100 {
		t.Errorf("tokens = in=%d out=%d cr=%d cw=%d, want 1000/200/500/100",
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreateTokens)
	}
	if r.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want >0 (priced by the provider)", r.CostUSD)
	}
}

// TestAgentpodSubagentDoneAppendsUsageRecord locks in the
// single-chokepoint property: subagent runs land their own
// usage.jsonl row via the same recording site as parent chat turns.
// Without this, subagent runs (which can dominate token spend on a
// busy day) silently disappeared from the daily counter. The
// model + cost arrive on the wire from the agent pod; the parent
// agent's slug is the billing roll-up.
func TestAgentpodSubagentDoneAppendsUsageRecord(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "ignored")
	defer cleanup()

	turnID := "subagent-tu-1"
	srv.installAgentpodSubagentTurn("alice", turnID, "sa-1", "mock-small")

	before := time.Now().UTC().Add(-time.Second)
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), turnID, agentpod.TurnEvent{
		Kind:              agentpod.TurnEventDone,
		Text:              "subagent final",
		Model:             "mock-small",
		InputTokens:       2000,
		OutputTokens:      400,
		CacheReadTokens:   100,
		CacheCreateTokens: 50,
		CostUSD:           0.0123,
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
	if r.Agent != "alice" {
		t.Errorf("Agent = %q, want alice (parent slug rolls up subagent spend)", r.Agent)
	}
	if r.Purpose != "subagent" {
		t.Errorf("Purpose = %q, want subagent", r.Purpose)
	}
	if r.Model != "mock-small" {
		t.Errorf("Model = %q, want mock-small", r.Model)
	}
	if r.InputTokens != 2000 || r.OutputTokens != 400 {
		t.Errorf("tokens = in=%d out=%d, want 2000/400", r.InputTokens, r.OutputTokens)
	}
	// Wire-reported cost wins over the local pricing fallback.
	if r.CostUSD != 0.0123 {
		t.Errorf("CostUSD = %v, want 0.0123 (CLI-reported)", r.CostUSD)
	}
}

// TestAgentpodTurnFailedAppendsUsageRecord locks in the failure-path
// usage recording: a TurnEventFailed carrying non-zero token counts
// (mid-stream subprocess death, Stop after assistant text already
// landed) must write a usage.jsonl row — Anthropic billed for every
// assistant message that ran before the failure, so we can't drop
// that spend just because the turn ended on the failure exit. A
// failed event with all-zero tokens (synthesized SSE-disconnect,
// stream-open errors) writes nothing, since there's nothing to bill.
func TestAgentpodTurnFailedAppendsUsageRecord(t *testing.T) {
	t.Run("subprocess-died with partial tokens lands a row", func(t *testing.T) {
		path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
		defer cleanup()

		before := time.Now().UTC().Add(-time.Second)
		c := agentpod.NewClient(path, "alice")
		if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
			Kind:              agentpod.TurnEventFailed,
			FailedReason:      agentpod.FailedReasonSubprocessDied,
			FailedDetail:      "oom mid-stream",
			InputTokens:       1500,
			OutputTokens:      300,
			CacheReadTokens:   200,
			CacheCreateTokens: 50,
		}); err != nil {
			t.Fatalf("PostTurnEvent: %v", err)
		}

		recs, err := srv.Store.ReadUsageSince(before)
		if err != nil {
			t.Fatalf("ReadUsageSince: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("usage records = %d, want 1 (partial spend must be recorded on failure)", len(recs))
		}
		r := recs[0]
		if r.InputTokens != 1500 || r.OutputTokens != 300 {
			t.Errorf("tokens = in=%d out=%d, want 1500/300", r.InputTokens, r.OutputTokens)
		}
		if r.CostUSD <= 0 {
			t.Errorf("CostUSD = %v, want >0 (priced by the provider)", r.CostUSD)
		}
	})

	t.Run("cancelled with partial tokens lands a row", func(t *testing.T) {
		path, srv, hub, cleanup := startTurnEventTestServer(t, "bob", "turn-1")
		defer cleanup()
		// Simulate handleAgentStop: synchronous marker write + flag set
		// so the cancel path runs (no "error" emit, but still records).
		hub.requestInterrupt()
		if err := srv.Store.AppendChatMessage("bob", store.ChatMessage{
			Role: store.RoleReceived, Kind: store.KindUserInterruption,
			Content: "User pressed Stop.",
		}); err != nil {
			t.Fatalf("append marker: %v", err)
		}

		before := time.Now().UTC().Add(-time.Second)
		c := agentpod.NewClient(path, "bob")
		if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
			Kind:         agentpod.TurnEventFailed,
			FailedReason: agentpod.FailedReasonCancelled,
			InputTokens:  800,
			OutputTokens: 120,
		}); err != nil {
			t.Fatalf("PostTurnEvent: %v", err)
		}

		recs, err := srv.Store.ReadUsageSince(before)
		if err != nil {
			t.Fatalf("ReadUsageSince: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("usage records = %d, want 1 (Stop with partial tokens still bills)", len(recs))
		}
		if recs[0].InputTokens != 800 || recs[0].OutputTokens != 120 {
			t.Errorf("tokens = in=%d out=%d, want 800/120", recs[0].InputTokens, recs[0].OutputTokens)
		}
	})

	t.Run("zero-token failure writes no row", func(t *testing.T) {
		path, srv, _, cleanup := startTurnEventTestServer(t, "carol", "turn-1")
		defer cleanup()

		before := time.Now().UTC().Add(-time.Second)
		c := agentpod.NewClient(path, "carol")
		if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
			Kind:         agentpod.TurnEventFailed,
			FailedReason: agentpod.FailedReasonOtherError,
			FailedDetail: "stream open failed before any tokens billed",
		}); err != nil {
			t.Fatalf("PostTurnEvent: %v", err)
		}
		recs, err := srv.Store.ReadUsageSince(before)
		if err != nil {
			t.Fatalf("ReadUsageSince: %v", err)
		}
		if len(recs) != 0 {
			t.Errorf("usage records = %d, want 0 (no tokens means nothing to bill)", len(recs))
		}
	})
}

// TestAgentpodTurnDoneEmitsChatFillSnap locks in the post-turn
// context-ring/banner/footer-stat snap: a "chat_fill" event lands
// on the hub before the terminal "done" event with values matching
// what computeChatFillStats would return on the freshly-flushed
// history. Without this, the JS done handler closes the
// EventSource before any later emit can land, and the ring stays
// frozen at its page-load value until manual refresh.
func TestAgentpodTurnDoneEmitsChatFillSnap(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	// Seed history sized to land at a clean 10% fill against the
	// 200k default context window: 80,000 chars → 20,000 tokens →
	// 10% of 200,000.
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat",
		Content: strings.Repeat("x", 80_000),
		TS:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	got, ok := hubEventByKind(hub, "chat_fill")
	if !ok {
		t.Fatalf("no chat_fill emit on hub; replay = %v", hubEventKinds(hub))
	}
	assertChatFillPayload(t, got, chatFillExpect{
		fillPct:    10,
		tokenEst:   20_000,
		ctxLimit:   200_000,
		longThresh: 150_000,
		bucket:     "low",
	})
	// Order matters: chat_fill must land before done so the
	// EventSource is still open when the browser receives it.
	kinds := hubEventKinds(hub)
	cfIdx, doneIdx := -1, -1
	for i, k := range kinds {
		if k == "chat_fill" && cfIdx == -1 {
			cfIdx = i
		}
		if k == "done" && doneIdx == -1 {
			doneIdx = i
		}
	}
	if cfIdx == -1 || doneIdx == -1 || cfIdx > doneIdx {
		t.Errorf("chat_fill must precede done; replay order = %v", kinds)
	}
}

// TestAgentpodTurnDoneUsesRealContextOccupancy: a tiny transcript (80k chars → chars/4 ≈
// 20k → 10% of the 200k window) coexists with a done event reporting
// a real single-call occupancy of 150k. The ring must reflect the
// real 75% — proving it's driven by ContextTokens, not the chat
// transcript — and that figure must survive a page reload via the
// persisted sidecar.
func TestAgentpodTurnDoneUsesRealContextOccupancy(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat",
		Content: strings.Repeat("x", 80_000),
		TS:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
		// Real single-call window occupancy: 100k input + 40k cache_read
		// + 10k cache_create = 150k of the 200k window. The accumulated
		// billing tokens (which a multi-step turn would inflate) are
		// deliberately NOT what the ring reads.
		ContextTokens: 150_000,
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	got, ok := hubEventByKind(hub, "chat_fill")
	if !ok {
		t.Fatalf("no chat_fill emit on hub; replay = %v", hubEventKinds(hub))
	}
	assertChatFillPayload(t, got, chatFillExpect{
		fillPct:    75,
		tokenEst:   150_000,
		ctxLimit:   200_000,
		longThresh: 150_000,
		bucket:     "high",
	})

	// The occupancy persisted, so a subsequent page-load render reads
	// the same 150k rather than collapsing back to the chars/4 estimate.
	cw, err := srv.Store.ReadContextWindow("alice")
	if err != nil {
		t.Fatalf("ReadContextWindow: %v", err)
	}
	if cw.ContextTokens != 150_000 {
		t.Errorf("persisted ContextTokens = %d, want 150000", cw.ContextTokens)
	}
}

// TestAgentpodTurnFailedEmitsChatFillSnap covers the error-path
// version: a chat_fill snap should land before the "error" emit so
// the ring stays in sync even when the turn ends with a runtime
// failure (subprocess died, model error, etc.). Without this, a
// failed turn that grew the chat (user message + partial output)
// leaves the ring stuck at its pre-turn value until refresh.
func TestAgentpodTurnFailedEmitsChatFillSnap(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat",
		Content: strings.Repeat("x", 80_000),
		TS:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonSubprocessDied,
		FailedDetail: "test failure",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	got, ok := hubEventByKind(hub, "chat_fill")
	if !ok {
		t.Fatalf("no chat_fill emit on hub; replay = %v", hubEventKinds(hub))
	}
	assertChatFillPayload(t, got, chatFillExpect{
		fillPct:    10,
		tokenEst:   20_000,
		ctxLimit:   200_000,
		longThresh: 150_000,
		bucket:     "low",
	})
}

// TestAgentpodTurnCancelEmitsChatFillSnap covers the Stop-click
// path: cancel doesn't emit an "error" event (handleAgentStop
// already wrote the user-interruption marker), but the chat ring
// still needs to refresh because the partial assistant text
// flushed at cancel time grew the chat. Without this, hitting Stop
// leaves the ring stale.
func TestAgentpodTurnCancelEmitsChatFillSnap(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat",
		Content: strings.Repeat("x", 80_000),
		TS:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonCancelled,
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	if _, ok := hubEventByKind(hub, "chat_fill"); !ok {
		t.Fatalf("no chat_fill emit on hub; replay = %v", hubEventKinds(hub))
	}
	// The cancel path must NOT emit an "error" — that's the
	// existing "no error emit on Stop" contract — so chat_fill is
	// the only ring update path here.
	for _, k := range hubEventKinds(hub) {
		if k == "error" {
			t.Errorf("cancel path should not emit error; replay = %v", hubEventKinds(hub))
		}
	}
}

// chatFillExpect is the value bundle TestAgentpodTurn{Done,Failed,Cancel}
// EmitsChatFillSnap asserts against. JSON numbers decode as float64,
// so the int fields below match by value, not type.
type chatFillExpect struct {
	fillPct    float64
	tokenEst   float64
	ctxLimit   float64
	longThresh float64
	bucket     string
}

func assertChatFillPayload(t *testing.T, got map[string]any, want chatFillExpect) {
	t.Helper()
	pairs := map[string]float64{
		"chat_fill_pct":      want.fillPct,
		"chat_token_est":     want.tokenEst,
		"chat_context_limit": want.ctxLimit,
		"chat_long_thresh":   want.longThresh,
	}
	for k, v := range pairs {
		got, ok := got[k].(float64)
		if !ok {
			t.Errorf("chat_fill.%s missing or wrong type", k)
			continue
		}
		if got != v {
			t.Errorf("chat_fill.%s = %v, want %v", k, got, v)
		}
	}
	if got["chat_fill_bucket"] != want.bucket {
		t.Errorf("chat_fill.chat_fill_bucket = %v, want %q", got["chat_fill_bucket"], want.bucket)
	}
	if _, ok := got["chat_resolved_model"].(string); !ok {
		t.Errorf("chat_fill.chat_resolved_model missing or wrong type")
	}
}

// drainAgentpodEvents pulls everything currently queued on sub without
// blocking. Publish is a synchronous send onto a buffered channel, so
// by the time the call under test returns, its event is already here —
// no sleep, no poll.
func drainAgentpodEvents(sub *agentpodSubscriber) []agentpod.Event {
	var out []agentpod.Event
	for {
		select {
		case ev := <-sub.ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// cancelledTurnIDs extracts the TurnID of every EventCancelTurn in evs.
func cancelledTurnIDs(t *testing.T, evs []agentpod.Event) []string {
	t.Helper()
	var ids []string
	for _, ev := range evs {
		if ev.Type != agentpod.EventCancelTurn {
			continue
		}
		var ct agentpod.CancelTurnEvent
		if err := json.Unmarshal(ev.Data, &ct); err != nil {
			t.Fatalf("unmarshal cancel-turn: %v", err)
		}
		ids = append(ids, ct.TurnID)
	}
	return ids
}

// foldedDeliveries extracts (turnID, deliveryID) of every
// EventFoldMessage in evs.
func foldedDeliveries(t *testing.T, evs []agentpod.Event) [][2]string {
	t.Helper()
	var out [][2]string
	for _, ev := range evs {
		if ev.Type != agentpod.EventFoldMessage {
			continue
		}
		var fm agentpod.FoldMessageEvent
		if err := json.Unmarshal(ev.Data, &fm); err != nil {
			t.Fatalf("unmarshal fold-message: %v", err)
		}
		out = append(out, [2]string{fm.TurnID, fm.DeliveryID})
	}
	return out
}

// TestDeliveryFoldsIntoBusyTurn is the user-visible behaviour: a
// message released to an agent that is mid-turn is offered to that
// turn to be answered INSIDE it, rather than ending the turn to
// deliver it. Every delivery gets its own offer — unlike the cancel it
// replaced, which had to be latched to one per turn.
//
// Also pins the narrowing that keeps this safe: an in-flight subagent
// is never targeted (that is expensive work a delivery has no business
// destroying).
func TestDeliveryFoldsIntoBusyTurn(t *testing.T) {
	_, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	// A subagent batch is running under this parent. It must survive.
	srv.installAgentpodSubagentTurn("alice", "subagent-turn-9", "sub9", "claude-sonnet-5")

	for _, body := range []string{"first", "second", "third"} {
		if err := srv.deliverToAgent("alice", store.ChatMessage{
			Role: store.RoleReceived, Kind: "direct_chat", Content: body,
		}); err != nil {
			t.Fatalf("deliver %s: %v", body, err)
		}
	}

	evs := drainAgentpodEvents(sub)
	folds := foldedDeliveries(t, evs)
	if len(folds) != 3 {
		t.Fatalf("published %d fold-message events, want 3 (one per delivery): %v", len(folds), folds)
	}
	seen := map[string]bool{}
	for _, f := range folds {
		if f[0] != "turn-1" {
			t.Errorf("fold targeted turn %q, want turn-1 (never the subagent turn)", f[0])
		}
		if f[1] == "" || seen[f[1]] {
			t.Errorf("fold delivery id %q is empty or duplicated; core could not tell the outcomes apart", f[1])
		}
		seen[f[1]] = true
	}
	// Folding does not stop the turn, so nothing should be cancelled.
	if ids := cancelledTurnIDs(t, evs); len(ids) != 0 {
		t.Errorf("folding published cancels %v; want none — the turn keeps running", ids)
	}

	// Nothing was dropped or written early: all three wait for either a
	// landed report or the end-of-turn flush.
	srv.streamMu.Lock()
	buffered := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if buffered != 3 {
		t.Errorf("buffered deliveries = %d, want 3", buffered)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Role == store.RoleReceived && m.Kind == "direct_chat" {
			t.Errorf("delivery reached chat.jsonl before it was folded or flushed: %q", m.Content)
		}
	}
}

// TestFoldLandedWritesMessageAtTheSeam: when the pod reports that the
// running turn consumed a message, it belongs in chat.jsonl at that
// point — the turn is answering it right now. Only that message moves;
// the others stay buffered.
func TestFoldLandedWritesMessageAtTheSeam(t *testing.T) {
	_, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	for _, body := range []string{"first", "second"} {
		if err := srv.deliverToAgent("alice", store.ChatMessage{
			Role: store.RoleReceived, Kind: "direct_chat", Content: body,
		}); err != nil {
			t.Fatalf("deliver %s: %v", body, err)
		}
	}
	folds := foldedDeliveries(t, drainAgentpodEvents(sub))
	if len(folds) != 2 {
		t.Fatalf("want 2 fold offers, got %d", len(folds))
	}

	srv.streamMu.Lock()
	st := srv.agentpodTurns["alice"]
	srv.streamMu.Unlock()
	if st == nil {
		t.Fatal("no in-flight turn state")
	}

	srv.onAgentpodFolded(st, agentpod.TurnEvent{
		Kind:       agentpod.TurnEventFolded,
		DeliveryID: folds[0][1],
		Landed:     true,
	})

	hist, _ := srv.Store.ReadChatHistory("alice")
	var got []string
	for _, m := range hist {
		if m.Role == store.RoleReceived && m.Kind == "direct_chat" {
			got = append(got, m.Content)
		}
	}
	if len(got) != 1 || got[0] != "first" {
		t.Errorf("chat.jsonl received rows = %v, want [first] only", got)
	}
	srv.streamMu.Lock()
	buffered := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if buffered != 1 {
		t.Errorf("buffered deliveries = %d, want 1 (only the landed one was written)", buffered)
	}

	// The user-visible half: the bubble paints while the turn that is
	// answering it is still streaming. Without this the agent appears
	// to reply to a message nobody can see until a reload.
	replay, _, cancel := hub.subscribe()
	close(cancel)
	var painted []string
	for _, ev := range replay {
		if ev.Kind != "chat_message" {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(ev.Payload, &m)
		if c, _ := m["content"].(string); c != "" {
			painted = append(painted, c)
		}
	}
	if len(painted) != 1 || painted[0] != "first" {
		t.Errorf("chat_message events = %v, want [first] painted at the fold seam", painted)
	}
}

// TestFoldLandedClosesTheOpenBubbleFirst: chat.jsonl is read in file
// order, so a message that arrives mid-reply must be filed BELOW the
// text the agent had already produced. The assembled buffer is only
// flushed at a tool boundary or at the end of the turn, so without an
// explicit flush here the CEO's message files above a reply that came
// before it — most visibly when the fold landed as the continuation
// of a turn that had just written its final words.
func TestFoldLandedClosesTheOpenBubbleFirst(t *testing.T) {
	_, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	srv.streamMu.Lock()
	st := srv.agentpodTurns["alice"]
	srv.streamMu.Unlock()
	if st == nil {
		t.Fatal("no in-flight turn state")
	}

	// The turn writes its answer to the ORIGINAL prompt. Nothing has
	// flushed it yet — no tool call followed it.
	srv.onAgentpodDelta(st, agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "done with the first thing"})

	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "also do this",
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	folds := foldedDeliveries(t, drainAgentpodEvents(sub))
	if len(folds) != 1 {
		t.Fatalf("want 1 fold offer, got %d", len(folds))
	}
	srv.onAgentpodFolded(st, agentpod.TurnEvent{
		Kind:       agentpod.TurnEventFolded,
		DeliveryID: folds[0][1],
		Landed:     true,
	})

	hist, _ := srv.Store.ReadChatHistory("alice")
	var order []string
	for _, m := range hist {
		switch {
		case m.Role == store.RoleSent && m.Content == "done with the first thing":
			order = append(order, "reply")
		case m.Role == store.RoleReceived && m.Content == "also do this":
			order = append(order, "message")
		}
	}
	if len(order) != 2 || order[0] != "reply" || order[1] != "message" {
		t.Errorf("chat.jsonl order = %v, want [reply message]", order)
	}
}

// TestFoldMissedFallsBackToPreempt: a fold that does not land leaves
// the message ours to deliver, so we go back to winding the turn up —
// the behaviour folds replaced. A batch of misses must still produce
// exactly ONE cancel, or a release of twenty messages sends twenty.
func TestFoldMissedFallsBackToPreempt(t *testing.T) {
	_, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	for _, body := range []string{"first", "second", "third"} {
		if err := srv.deliverToAgent("alice", store.ChatMessage{
			Role: store.RoleReceived, Kind: "direct_chat", Content: body,
		}); err != nil {
			t.Fatalf("deliver %s: %v", body, err)
		}
	}
	folds := foldedDeliveries(t, drainAgentpodEvents(sub))
	if len(folds) != 3 {
		t.Fatalf("want 3 fold offers, got %d", len(folds))
	}

	srv.streamMu.Lock()
	st := srv.agentpodTurns["alice"]
	srv.streamMu.Unlock()

	for _, f := range folds {
		srv.onAgentpodFolded(st, agentpod.TurnEvent{
			Kind:       agentpod.TurnEventFolded,
			DeliveryID: f[1],
			Landed:     false,
		})
	}

	ids := cancelledTurnIDs(t, drainAgentpodEvents(sub))
	if len(ids) != 1 || ids[0] != "turn-1" {
		t.Fatalf("cancelled turn ids = %v, want exactly [turn-1] — one interrupt per turn", ids)
	}

	// Still buffered: the fallback delivers them via the end-of-turn
	// flush, exactly as before folds existed.
	srv.streamMu.Lock()
	buffered := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if buffered != 3 {
		t.Errorf("buffered deliveries = %d, want 3", buffered)
	}
}

// TestDeliveryToIdleAgentDoesNotPreempt is the other half: when nothing
// is running there is no turn to wind up, so the message is written
// straight through and no cancel goes out. A spurious cancel here would
// land on whatever turn started next.
func TestDeliveryToIdleAgentDoesNotPreempt(t *testing.T) {
	_, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	// Retire the turn: completed hub, no agentpod turn state.
	hub.markCompleted()
	srv.streamMu.Lock()
	delete(srv.agentpodTurns, "alice")
	srv.streamMu.Unlock()

	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "hello",
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if ids := cancelledTurnIDs(t, drainAgentpodEvents(sub)); len(ids) != 0 {
		t.Errorf("idle agent published cancels %v; want none", ids)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	var got []string
	for _, m := range hist {
		if m.Role == store.RoleReceived {
			got = append(got, m.Content)
		}
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("chat.jsonl received rows = %v, want [hello] written directly", got)
	}
}

// TestAgentpodTurnDoneWithBufferedFlushes locks in the buffer-drain
// behavior: deliveries that arrived during the turn (buffered while
// the hub was busy) flush to chat.jsonl in arrival order when the
// done teardown runs flushAndComplete.
func TestAgentpodTurnDoneWithBufferedFlushes(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	// While the hub is busy, queue two deliveries via deliverToAgent.
	// The path is busy=hub-not-completed, so they buffer.
	for i, body := range []string{"first arrival", "second arrival"} {
		_ = i
		if err := srv.deliverToAgent("alice", store.ChatMessage{
			Role: store.RoleReceived, Kind: "direct_chat", Content: body,
			TS: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("buffer delivery %d: %v", i, err)
		}
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("done: %v", err)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	var bodies []string
	for _, m := range hist {
		if m.Kind == "direct_chat" && m.Role == store.RoleReceived {
			bodies = append(bodies, m.Content)
		}
	}
	if len(bodies) != 2 || bodies[0] != "first arrival" || bodies[1] != "second arrival" {
		t.Errorf("flushed bodies = %v, want [first arrival, second arrival]", bodies)
	}
}

// TestAgentpodTurnDoneInterruptOrdering locks in the interrupt-marker
// placement rule: when Stop fired during the turn (synchronous write
// at click time), the kind:user-interruption row lands BEFORE any
// buffered deliveries on chat.jsonl — the next BuildRequest's spawn
// gate sees the marker as the boundary before the redirect message.
func TestAgentpodTurnDoneInterruptOrdering(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	// User pressed Stop mid-turn — handleAgentStop wrote the marker
	// synchronously and flipped the flag.
	first, alreadyDone := hub.requestInterrupt()
	if !first || alreadyDone {
		t.Fatalf("requestInterrupt: first=%v alreadyDone=%v", first, alreadyDone)
	}
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: store.KindUserInterruption,
		Content: "User pressed Stop.",
	}); err != nil {
		t.Fatalf("append marker: %v", err)
	}
	// And one buffered delivery arrived (busy hub → buffer path).
	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "redirect",
	}); err != nil {
		t.Fatalf("buffer delivery: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("done: %v", err)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	interruptIdx, redirectIdx := -1, -1
	for i, m := range hist {
		if m.Kind == store.KindUserInterruption {
			interruptIdx = i
		}
		if m.Role == store.RoleReceived && m.Content == "redirect" {
			redirectIdx = i
		}
	}
	if interruptIdx < 0 {
		t.Fatal("no user-interruption row written")
	}
	if redirectIdx < 0 {
		t.Fatal("buffered redirect did not flush")
	}
	if interruptIdx > redirectIdx {
		t.Errorf("ordering wrong: interrupt at %d, redirect at %d (want interrupt first)", interruptIdx, redirectIdx)
	}
}

// TestAgentpodTurnFailedSubprocessTearsDown asserts the failed +
// subprocess-died path runs the same teardown as done: state
// dropped, hub completed, active-turn marker cleared, AND the
// disruption row is written.
func TestAgentpodTurnFailedSubprocessTearsDown(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	if err := srv.Store.MarkActiveTurn("alice", store.ActiveTurnMarker{
		StartedAt: time.Now().UTC(), Source: "chat", Model: "claude-test",
	}); err != nil {
		t.Fatalf("seed active turn: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonSubprocessDied,
		FailedDetail: "kubelet OOMKilled",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) == 0 || hist[len(hist)-1].Kind != store.KindRuntimeDisruption {
		t.Errorf("expected runtime-disruption row at tail; hist tail = %+v", hist)
	}
	if !hub.isCompleted() {
		t.Errorf("hub not marked completed after failed teardown")
	}
	if srv.lookupAgentpodTurn("alice") != nil {
		t.Errorf("agentpodTurns entry not dropped after failed teardown")
	}
	if m, _ := srv.Store.ReadActiveTurn("alice"); !m.StartedAt.IsZero() {
		t.Errorf("active-turn marker still present after failed teardown: %+v", m)
	}
	if _, ok := hubEventByKind(hub, "error"); !ok {
		t.Errorf("no error emit on hub")
	}
}

// TestAgentpodTurnFailedCancelledSkipsDisruptionRow asserts the
// cancelled path: the agent runtime POSTs a failed event with
// FailedReasonCancelled (after EventCancelTurn), and the handler
// runs the same teardown as done — but skips the disruption-row
// write and the "error" UI emit. handleAgentStop already wrote the
// user-interruption marker synchronously at click time; a
// runtime-disruption row would mislead the agent into thinking
// their pod crashed.
func TestAgentpodTurnFailedCancelledSkipsDisruptionRow(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	if err := srv.Store.MarkActiveTurn("alice", store.ActiveTurnMarker{
		StartedAt: time.Now().UTC(), Source: "chat", Model: "claude-test",
	}); err != nil {
		t.Fatalf("seed active turn: %v", err)
	}
	// Simulate handleAgentStop: synchronous marker write + flag set.
	hub.requestInterrupt()
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: store.KindUserInterruption,
		Content: "User pressed Stop.",
	}); err != nil {
		t.Fatalf("append marker: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonCancelled,
		FailedDetail: "cancelled by Stop",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Kind == store.KindRuntimeDisruption {
			t.Errorf("cancelled failed wrote a runtime-disruption row; should not: %+v", m)
		}
	}
	// Marker landed via handleAgentStop's sync write.
	hasInterrupt := false
	for _, m := range hist {
		if m.Kind == store.KindUserInterruption {
			hasInterrupt = true
			break
		}
	}
	if !hasInterrupt {
		t.Errorf("expected user-interruption marker after cancelled teardown; hist = %+v", hist)
	}
	if !hub.isCompleted() {
		t.Errorf("hub not marked completed after cancelled teardown")
	}
	if srv.lookupAgentpodTurn("alice") != nil {
		t.Errorf("agentpodTurns entry not dropped after cancelled teardown")
	}
	if m, _ := srv.Store.ReadActiveTurn("alice"); !m.StartedAt.IsZero() {
		t.Errorf("active-turn marker still present after cancelled teardown: %+v", m)
	}
	if _, ok := hubEventByKind(hub, "error"); ok {
		t.Errorf("cancelled failed emitted an error UI event; should be silent (cancel isn't an error)")
	}
}

// TestAgentpodTurnDoneRotationFinalizes locks in the rotation flow:
// when a rotation is queued at the moment done arrives, the teardown
// archives the chat, emits "rotation_done", and the "done" event
// itself carries rotation:true so the browser holds the stream open
// past "done" to receive rotation_done. Mirrors the chat-rotation
// invariants in handleNewChat / consumeRotationIfReady.
func TestAgentpodTurnDoneRotationFinalizes(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	// Queue a rotation directly via the same on-disk marker handleNewChat
	// writes. PriorMemory empty is fine — finalizeRotation only writes
	// the snapshot when priorMemory is non-empty.
	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{RequestedBy: "test"}); err != nil {
		t.Fatalf("WritePendingRotation: %v", err)
	}

	// Seed a chat entry so ArchiveChat has something to archive.
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "kicker",
	}); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	got, ok := hubEventByKind(hub, "done")
	if !ok {
		t.Fatal("no done emit")
	}
	if got["rotation"] != true {
		t.Errorf("done.rotation = %v, want true (rotation pending)", got["rotation"])
	}
	if _, ok := hubEventByKind(hub, "rotation_done"); !ok {
		t.Errorf("no rotation_done emit; replay = %v", hubEventKinds(hub))
	}
	// Archive directory exists (created by ArchiveChat under chats/).
	archived, err := srv.Store.ListArchivedChats("alice")
	if err != nil {
		t.Fatalf("ListArchivedChats: %v", err)
	}
	if len(archived) == 0 {
		t.Errorf("no archived chat after rotation finalize")
	}
	// On-disk marker is removed after successful finalize so a future
	// done event doesn't re-finalize.
	if _, has, _ := srv.Store.ReadPendingRotation("alice"); has {
		t.Errorf("pending_rotation marker survived finalize")
	}
}

// TestRotationFinalizeSignalsSessionReset is the core-side half of the
// rotation-amnesia fix: finalizeRotation must publish an
// EventSessionReset to the agent pod after clearing the stored CLI
// session id. Without it the pod's warm `claude -p` runner keeps the
// whole pre-rotation conversation in memory and --resumes it, so the
// rotation never reaches the model (transcript rotates, context window
// doesn't).
func TestRotationFinalizeSignalsSessionReset(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	// Wire the agent-pod hub and subscribe as the pod would, so we can
	// observe what finalizeRotation publishes.
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)

	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{RequestedBy: "test"}); err != nil {
		t.Fatalf("WritePendingRotation: %v", err)
	}
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "kicker",
	}); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	select {
	case ev := <-sub.ch:
		if ev.Type != agentpod.EventSessionReset {
			t.Fatalf("published event = %q, want %q", ev.Type, agentpod.EventSessionReset)
		}
		var sr agentpod.SessionResetEvent
		if err := json.Unmarshal(ev.Data, &sr); err != nil {
			t.Fatalf("decode SessionResetEvent: %v", err)
		}
		if sr.Slug != "alice" {
			t.Errorf("SessionResetEvent.Slug = %q, want alice", sr.Slug)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rotation finalize never published a session-reset event to the pod")
	}
}

// TestRotationMarkerSurvivesCrashAndFinalizes locks in the on-disk
// recovery property: a Kivali crash between marking the rotation and
// the post-turn finalize must not lose the rotation semantics. The
// marker persists across the restart, the next turn-end's finalize
// finds it, and the chat ends up archived.
func TestRotationMarkerSurvivesCrashAndFinalizes(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	// Simulate the pre-crash state: handleNewChat would have written
	// the marker AND injected a rotation_prompt into chat.jsonl. Then
	// (hypothetically) Kivali crashed. On restart nothing in memory
	// knows about the rotation — the on-disk marker is the only
	// durable signal, and it is still here.
	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{
		PriorMemory: "snapshot-of-old-memory",
		RequestedBy: "ceo",
	}); err != nil {
		t.Fatalf("WritePendingRotation: %v", err)
	}
	// Some chat content needs to exist for ArchiveChat to do anything
	// observable.
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "rotation_prompt", Content: "update your memory",
	}); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	// Recovered chat-turn completes — same wire as the normal path.
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	if _, ok := hubEventByKind(hub, "rotation_done"); !ok {
		t.Errorf("no rotation_done emit after crash recovery; replay = %v", hubEventKinds(hub))
	}
	archived, err := srv.Store.ListArchivedChats("alice")
	if err != nil {
		t.Fatalf("ListArchivedChats: %v", err)
	}
	if len(archived) == 0 {
		t.Errorf("crash-mid-rotation: no archived chat after recovered turn finalized")
	}
	if _, has, _ := srv.Store.ReadPendingRotation("alice"); has {
		t.Errorf("crash-mid-rotation: pending_rotation marker survived finalize")
	}
}

func newRecorder() *recorder {
	return &recorder{header: http.Header{}}
}

type recorder struct {
	Code   int
	header http.Header
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header  { return r.header }
func (r *recorder) WriteHeader(code int) { r.Code = code }
func (r *recorder) Write(p []byte) (int, error) {
	if r.Code == 0 {
		r.Code = http.StatusOK
	}
	return r.body.Write(p)
}

// foldTexts extracts the Text of every EventFoldMessage in evs.
func foldTexts(t *testing.T, evs []agentpod.Event) []string {
	t.Helper()
	var out []string
	for _, ev := range evs {
		if ev.Type != agentpod.EventFoldMessage {
			continue
		}
		var fm agentpod.FoldMessageEvent
		if err := json.Unmarshal(ev.Data, &fm); err != nil {
			t.Fatalf("unmarshal fold-message: %v", err)
		}
		out = append(out, fm.Text)
	}
	return out
}

// TestFoldNamesTheFilesItCarries: a message that reaches a busy agent
// is folded into the running turn as text. When it carries files the
// text must say where they are, and they must already be there. The
// prompt path gets both from chat.jsonl after the top-of-turn sync; a
// fold gets neither, because the message is not on disk and the turn
// was synced before it existed. Folding the bare content left the
// agent to infer a path that did not resolve until its next turn —
// for its own file_view, and for a subagent handed the path, which
// then reported the file absent as a finding rather than as a failed
// read. The read here goes through a subagent overlay, the same
// Backend a subagent's file_view runs against.
func TestFoldNamesTheFilesItCarries(t *testing.T) {
	_, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	att, err := srv.Store.AddAttachmentFromText("pinout.txt", "GPIO4 -> LED\n")
	if err != nil {
		t.Fatalf("AddAttachmentFromText: %v", err)
	}
	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "extract the pinout",
		Attachments: []store.MessageAttachment{{SHA: att.SHA, Name: "pinout.txt"}},
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	evs := drainAgentpodEvents(sub)
	texts := foldTexts(t, evs)
	if len(texts) != 1 {
		t.Fatalf("published %d fold-message events, want 1", len(texts))
	}
	if !strings.Contains(texts[0], "extract the pinout") {
		t.Errorf("fold text lost the message: %q", texts[0])
	}
	if !strings.Contains(texts[0], "/files/attachments/pinout.txt") {
		t.Errorf("fold text does not say where the file is: %q", texts[0])
	}
	if ids := cancelledTurnIDs(t, evs); len(ids) != 0 {
		t.Errorf("turn was preempted %v; want the message folded", ids)
	}

	// The path the fold quotes resolves now, before anything has been
	// flushed — read the way a subagent dispatched by this turn would
	// read it.
	overlayRoot := subagentFilesRoot(srv.Store, "alice", "sub1")
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: overlayRoot}); err != nil {
		t.Fatalf("BuildSubagentOverlay: %v", err)
	}
	// Wired as the pod wires a subagent's backend (mcp_cmd.go): the
	// parent's tree is a write root, and the attachment blobs the farm
	// points into are a read root (/data/attachments in the pod).
	subFiles := &files.Backend{
		Root:       overlayRoot,
		ReadRoots:  []string{filepath.Join(srv.Store.Root(), "attachments")},
		WriteRoots: []string{files.SubagentParentRoot(overlayRoot)},
	}
	view, err := subFiles.View("/files/attachments/pinout.txt", files.ViewOptions{})
	if err != nil {
		t.Fatalf("the fold names /files/attachments/pinout.txt but a subagent's file_view cannot read it: %v", err)
	}
	if !strings.Contains(view, "GPIO4 -> LED") {
		t.Errorf("subagent view = %q, want the attachment's bytes", view)
	}
	// And the per-agent mirror the pod's mount resolves through has
	// the blob, not only core's canonical store.
	if _, err := os.Stat(filepath.Join(srv.Store.Root(), "agents", "alice", "attachments", att.SHA, "blob.txt")); err != nil {
		t.Errorf("pending SHA is missing from the per-agent mirror: %v", err)
	}

	// The message itself still waits for its seam.
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Role == store.RoleReceived && m.Kind == "direct_chat" {
			t.Errorf("delivery reached chat.jsonl before it was folded or flushed: %q", m.Content)
		}
	}
}

// TestFoldWithoutFilesCarriesOnlyTheText pins that the attached-files
// block is not appended to a message that has nothing attached.
func TestFoldWithoutFilesCarriesOnlyTheText(t *testing.T) {
	_, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "just words",
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	texts := foldTexts(t, drainAgentpodEvents(sub))
	if len(texts) != 1 || texts[0] != "just words" {
		t.Errorf("fold texts = %q, want exactly the message", texts)
	}
}

// TestFoldFallsBackWhenFilesCannotBeLinked: if the links cannot be
// made, the message is not folded with a path that would not resolve.
// It stays buffered and the turn is asked to wind up, so the message
// reaches the model the way it did before folds: flushed to disk,
// synced at the top of the follow-up turn, rendered from chat.jsonl.
func TestFoldFallsBackWhenFilesCannotBeLinked(t *testing.T) {
	_, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	att, err := srv.Store.AddAttachmentFromText("pinout.txt", "GPIO4 -> LED\n")
	if err != nil {
		t.Fatalf("AddAttachmentFromText: %v", err)
	}
	// A regular file where the per-agent mirror directory belongs
	// makes the hardlink reconcile fail.
	mirror := filepath.Join(srv.Store.Root(), "agents", "alice", "attachments")
	_ = os.RemoveAll(mirror)
	if err := os.WriteFile(mirror, []byte("in the way"), 0o644); err != nil {
		t.Fatalf("block mirror dir: %v", err)
	}

	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "extract the pinout",
		Attachments: []store.MessageAttachment{{SHA: att.SHA, Name: "pinout.txt"}},
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	evs := drainAgentpodEvents(sub)
	if texts := foldTexts(t, evs); len(texts) != 0 {
		t.Errorf("folded %q although its file could not be linked", texts)
	}
	if ids := cancelledTurnIDs(t, evs); len(ids) != 1 || ids[0] != "turn-1" {
		t.Errorf("cancelled turns = %v, want [turn-1] (wind up so the follow-up turn delivers from disk)", ids)
	}
	srv.streamMu.Lock()
	buffered := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if buffered != 1 {
		t.Errorf("buffered deliveries = %d, want 1 (the message is still owed)", buffered)
	}
}
