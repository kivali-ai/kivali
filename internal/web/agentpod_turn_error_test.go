package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// These tests pin the user-visible outcome of a turn the driver closed
// on a failed model call — a usage limit, an expired login, a 429. On
// the wire that is a TurnEventError carrying the error text, then a
// done event whose StopReason is provider.StopError. Before the
// turn-error marker existed the text was filed as the agent's reply,
// the done read as a normal completion, the sidebar went idle and the
// spawn gate saw an answered message: a limit hit looked like a reply,
// and nothing said the agent had stopped.

func indexOf(kinds []string, kind string) int {
	for i, k := range kinds {
		if k == kind {
			return i
		}
	}
	return -1
}

func TestAgentpodTurnErrorFromTypedError(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	before := time.Now().UTC().Add(-time.Second)

	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "draft the plan", TS: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := agentpod.NewClient(path, "alice")
	post := func(ev agentpod.TurnEvent) {
		t.Helper()
		if err := c.PostTurnEvent(context.Background(), "turn-1", ev); err != nil {
			t.Fatalf("PostTurnEvent %s: %v", ev.Kind, err)
		}
	}
	// The agent got as far as one sentence before the next call failed.
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "Starting on the plan.", Model: "mock-large-0"})
	// The driver's typed error: the provider's text, never a reply.
	post(agentpod.TurnEvent{Kind: agentpod.TurnEventError, Text: "Claude AI usage limit reached|1759100000"})
	// No model: the pod had none of its own for the failed call, so
	// core prices at the one it asked for.
	post(agentpod.TurnEvent{
		Kind:            agentpod.TurnEventDone,
		StopReason:      provider.StopError,
		CacheReadTokens: 1_000,
		ContextTokens:   5_000,
	})

	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	replyAt, markerAt := -1, -1
	for i, m := range hist {
		switch {
		case m.Role == store.RoleSent && strings.Contains(m.Content, "usage limit reached"):
			t.Errorf("the CLI's error text was filed as the agent's reply: %+v", m)
		case m.Role == store.RoleSent && m.Kind == "direct_chat":
			replyAt = i
			if m.Model != "mock-large-0" {
				t.Errorf("partial reply model = %q, want mock-large-0", m.Model)
			}
		case m.Kind == store.KindTurnError:
			if markerAt != -1 {
				t.Errorf("second turn-error row: %+v", m)
			}
			markerAt = i
			if m.Role != store.RoleReceived {
				t.Errorf("turn-error role = %q, want received", m.Role)
			}
			if !strings.Contains(m.Content, "usage limit reached") {
				t.Errorf("turn-error content lacks the CLI's detail: %q", m.Content)
			}
		}
	}
	if replyAt == -1 {
		t.Errorf("the partial reply before the failure was dropped; hist=%+v", hist)
	}
	if markerAt == -1 {
		t.Fatalf("no turn-error row; hist=%+v", hist)
	}
	if replyAt > markerAt {
		t.Errorf("partial reply (%d) filed after the marker (%d); the transcript must read in the order things happened", replyAt, markerAt)
	}
	if got := store.SpawnDecision(hist); got != store.SpawnHoldOnError {
		t.Errorf("SpawnDecision = %v, want SpawnHoldOnError", got)
	}

	// The browser: the marker is painted BEFORE the terminal error event
	// (whose handler closes the EventSource), and no done event is sent
	// for a turn that did not complete.
	kinds := hubEventKinds(hub)
	if indexOf(kinds, "done") != -1 {
		t.Errorf("a done event was emitted for an errored turn; replay=%v", kinds)
	}
	mk, ek := indexOf(kinds, "chat_marker"), indexOf(kinds, "error")
	if mk == -1 || ek == -1 {
		t.Fatalf("replay lacks chat_marker/error: %v", kinds)
	}
	if mk > ek {
		t.Errorf("chat_marker (%d) emitted after error (%d); the page would never paint it live", mk, ek)
	}
	if p, ok := hubEventByKind(hub, "chat_marker"); !ok || p["kind"] != store.KindTurnError {
		t.Errorf("chat_marker payload = %v, want kind %s", p, store.KindTurnError)
	}

	// The calls before the failure were billed: one usage row, priced
	// against the model that was asked for.
	recs, err := srv.Store.ReadUsageSince(before)
	if err != nil {
		t.Fatalf("ReadUsageSince: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(recs))
	}
	if recs[0].Model != "mock-large-0" || recs[0].CacheReadTokens != 1_000 {
		t.Errorf("usage row = %+v, want mock-large-0 with 1000 cache-read tokens", recs[0])
	}
	// The window occupancy the pod measured is kept for the ring.
	if cw, _ := srv.Store.ReadContextWindow("alice"); cw.ContextTokens != 5_000 {
		t.Errorf("context window = %d, want 5000", cw.ContextTokens)
	}
}

// TestAgentpodTurnErrorDoneWithoutDetail: a done with StopReason
// provider.StopError and no error event before it (a provider that
// reports a failure with no text) still lands the marker — with a
// placeholder detail rather than an empty one.
func TestAgentpodTurnErrorDoneWithoutDetail(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: provider.StopError,
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) != 1 || hist[0].Kind != store.KindTurnError {
		t.Fatalf("hist = %+v, want exactly one turn-error row", hist)
	}
	if !strings.Contains(hist[0].Content, "no detail was reported") {
		t.Errorf("content = %q, want the no-detail placeholder", hist[0].Content)
	}
	if kinds := hubEventKinds(hub); indexOf(kinds, "done") != -1 || indexOf(kinds, "error") == -1 {
		t.Errorf("replay = %v, want error and no done", kinds)
	}
}

// TestAgentpodTurnDoneEndTurnUnchanged: the common case is untouched —
// a done with a real stop reason emits done, writes no marker, and the
// agent's text is its reply.
func TestAgentpodTurnDoneEndTurnUnchanged(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	for _, ev := range []agentpod.TurnEvent{
		{Kind: agentpod.TurnEventDelta, Text: "All done.", Model: "mock-large-0"},
		{Kind: agentpod.TurnEventDone, StopReason: "end_turn", Model: "mock-large-0"},
	} {
		if err := c.PostTurnEvent(context.Background(), "turn-1", ev); err != nil {
			t.Fatalf("PostTurnEvent %s: %v", ev.Kind, err)
		}
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Kind == store.KindTurnError {
			t.Errorf("turn-error row on a completed turn: %+v", m)
		}
	}
	if kinds := hubEventKinds(hub); indexOf(kinds, "done") == -1 || indexOf(kinds, "error") != -1 {
		t.Errorf("replay = %v, want done and no error", kinds)
	}
}

// TestAgentpodTurnFailedSubprocessDiedMarkerBeforeError: the crash path
// keeps its runtime-disruption row, and now paints it live too — the
// marker's chat_marker precedes the error event that closes the stream.
func TestAgentpodTurnFailedSubprocessDiedMarkerBeforeError(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonSubprocessDied, FailedDetail: "exit status 137",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) != 1 || hist[0].Kind != store.KindRuntimeDisruption {
		t.Fatalf("hist = %+v, want exactly one runtime-disruption row", hist)
	}
	kinds := hubEventKinds(hub)
	mk, ek := indexOf(kinds, "chat_marker"), indexOf(kinds, "error")
	if mk == -1 || ek == -1 || mk > ek {
		t.Errorf("replay = %v, want chat_marker before error", kinds)
	}
}
