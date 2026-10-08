package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// stubClaude is a placeholder provider.Client for tests that need
// spawnChatLoopIfIdle to get past its `s.Claude == nil` short-circuit
// without actually launching a Claude call. Stream returns a fake error
// immediately; spawn only reaches Stream after the gate passes, so a
// gate-held test sees a clean false return without exercising Stream.
type stubClaude struct{}

func (stubClaude) Complete(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
	return nil, errors.New("stubClaude: not for real use")
}
func (stubClaude) Stream(ctx context.Context, req provider.CompleteRequest) (provider.Stream, error) {
	return nil, errors.New("stubClaude: not for real use")
}
func (stubClaude) RunSubagent(ctx context.Context, req provider.SubagentRequest) (provider.Stream, error) {
	return nil, errors.New("stubClaude: not for real use")
}
func (stubClaude) FormatUsage(provider.TokenUsage) provider.UsageDisplay {
	return provider.UsageDisplay{}
}
func (stubClaude) HandlesToolLoop() bool { return false }

// TestStopPublishesCancelTurnEvent locks in the wire-cancel
// handshake: when there's an in-flight agent-pod turn, Stop must
// publish EventCancelTurn so the in-pod runtime can short-circuit
// the model stream — without this, Stop only takes effect after the
// current chunk completes, which feels broken to the user.
func TestStopPublishesCancelTurnEvent(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	srv.AgentpodHub = newAgentpodHub()
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.chatHubs = map[string]*chatHub{"alice": hub}
	// Install a per-slug turn state so publishCancelTurn finds it.
	srv.InitAgentpodTurnState("alice", "chat", "tu-active", hub, "mock-large-0", "high")
	// Subscribe a fake on the hub so we can read the published event.
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rr.Code)
	}

	select {
	case ev := <-sub.ch:
		if ev.Type != agentpod.EventCancelTurn {
			t.Fatalf("event type = %q, want %q", ev.Type, agentpod.EventCancelTurn)
		}
		var cancelEv agentpod.CancelTurnEvent
		if err := json.Unmarshal(ev.Data, &cancelEv); err != nil {
			t.Fatalf("decode CancelTurnEvent: %v", err)
		}
		if cancelEv.TurnID != "tu-active" {
			t.Errorf("turn_id = %q, want tu-active", cancelEv.TurnID)
		}
		if cancelEv.Slug != "alice" {
			t.Errorf("slug = %q, want alice", cancelEv.Slug)
		}
	case <-time.After(time.Second):
		t.Fatal("EventCancelTurn never published after Stop")
	}
}

// TestStopWithoutInflightTurnIsNoOpOnHub: Stop publishes nothing to
// the agent-pod hub when there's no active agentpod turn. The hub's
// flag still flips so the post-turn defer (when a future turn does
// fire) can write the marker, but we don't blast a cancel for a
// turn that doesn't exist.
func TestStopWithoutInflightTurnIsNoOpOnHub(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	srv.AgentpodHub = newAgentpodHub()
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.chatHubs = map[string]*chatHub{"alice": hub}
	// No InitAgentpodTurnState — hub is "active" but no agentpod
	// turn was published.
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rr.Code)
	}

	select {
	case ev := <-sub.ch:
		t.Fatalf("unexpected event published with no in-flight turn: %v", ev)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

// TestStopWritesMarkerSynchronously pins the Stop semantics: the
// handler writes a kind:user-interruption row to chat.jsonl
// synchronously at click time, BEFORE any cancellation side effect
// dispatches. The audit trail is durable even if downstream
// cancellation fails.
func TestStopWritesMarkerSynchronously(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	srv.chatHubs = map[string]*chatHub{
		"alice": {hub: newHub(), slug: "alice"},
	}

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Marker is durable in chat.jsonl by the time the handler returns.
	hist, _ := srv.Store.ReadChatHistory("alice")
	count := 0
	for _, m := range hist {
		if m.Kind == store.KindUserInterruption {
			count++
		}
	}
	if count != 1 {
		t.Errorf("user-interruption entries = %d, want exactly 1", count)
	}
	// Flag is set on the hub.
	srv.chatHubs["alice"].mu.Lock()
	if !srv.chatHubs["alice"].interruptRequested {
		t.Error("hub.interruptRequested = false, want true")
	}
	srv.chatHubs["alice"].mu.Unlock()
}

// TestStopEmitsChatMarkerOnHub locks in the live-UI visibility fix:
// handleAgentStop must broadcast a chat_marker SSE event on the hub
// so a browser holding /agents/<slug>/stream open paints the
// "interrupted" divider immediately. Without this, the marker landed
// in chat.jsonl but never surfaced live — the CEO clicked Stop and
// saw no on-screen acknowledgment until a full page reload.
func TestStopEmitsChatMarkerOnHub(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.chatHubs = map[string]*chatHub{"alice": hub}

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rr.Code)
	}

	payload, ok := hubEventByKind(hub, "chat_marker")
	if !ok {
		t.Fatalf("no chat_marker emit on hub after Stop; replay = %v", hubEventKinds(hub))
	}
	if payload["kind"] != store.KindUserInterruption {
		t.Errorf("chat_marker.kind = %v, want %q", payload["kind"], store.KindUserInterruption)
	}
	if payload["content"] != "User pressed Stop." {
		t.Errorf("chat_marker.content = %v, want %q", payload["content"], "User pressed Stop.")
	}
	if _, ok := payload["ts"].(float64); !ok {
		t.Errorf("chat_marker.ts missing or wrong type: %v (%T)", payload["ts"], payload["ts"])
	}
}

// TestStopRepeatClickEmitsMarkerOnce: the marker write is gated by
// the `first` flag, so spam-clicking Stop emits exactly one
// chat_marker even though every click re-dispatches EventCancelTurn.
// Without this, repeat clicks would broadcast duplicate dividers and
// the chat would visually fill with redundant "interrupted" rows.
func TestStopRepeatClickEmitsMarkerOnce(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.chatHubs = map[string]*chatHub{"alice": hub}

	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
		rr := httptest.NewRecorder()
		authedHandler(t, srv).ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("click %d: code = %d", i, rr.Code)
		}
	}

	markers := 0
	for _, k := range hubEventKinds(hub) {
		if k == "chat_marker" {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("chat_marker emits = %d, want exactly 1 (one per marker write, not one per click)", markers)
	}
}

// TestStopIsIdempotentOnSpamClick locks in: clicking Stop multiple
// times writes exactly ONE marker (not one per click), and every
// click returns 202.
func TestStopIsIdempotentOnSpamClick(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	srv.chatHubs = map[string]*chatHub{
		"alice": {hub: newHub(), slug: "alice"},
	}

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
		rr := httptest.NewRecorder()
		authedHandler(t, srv).ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("click %d: code = %d", i, rr.Code)
		}
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	count := 0
	for _, m := range hist {
		if m.Kind == store.KindUserInterruption {
			count++
		}
	}
	if count != 1 {
		t.Errorf("user-interruption entries = %d, want exactly 1", count)
	}
}

// TestStopOnCompletedHubDoesNotWriteMarker locks in the L5 fix:
// if the loop has already finished (post-loop defer ran, hub
// completed) by the time Stop fires, the handler must NOT silently
// quarantine the agent with a marker no in-flight loop produced.
func TestStopOnCompletedHubDoesNotWriteMarker(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	hub := &chatHub{hub: newHub(), slug: "alice"}
	hub.markCompleted() // simulate post-loop defer already ran
	srv.chatHubs = map[string]*chatHub{"alice": hub}

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204 (loop already finished)", rr.Code)
	}
	hub.mu.Lock()
	if hub.interruptRequested {
		t.Error("interruptRequested set on a completed hub; should be no-op")
	}
	hub.mu.Unlock()
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Kind == store.KindUserInterruption {
			t.Errorf("user-interruption entry on completed hub: %+v", m)
		}
	}
}

// TestStopOnIdleHubReturnsNoContent: pressing Stop when no hub
// exists at all is a no-op (204).
func TestStopOnIdleHubReturnsNoContent(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rr.Code)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Kind == store.KindUserInterruption {
			t.Errorf("found a user-interruption entry on idle stop: %+v", m)
		}
	}
}

// TestStopMarkerLandsBeforeLoopCompletes locks in the sync-write
// invariant: the user-interruption row is durable in chat.jsonl
// before handleAgentStop returns, regardless of whether the loop
// has continued to emit tool events. Trailing tool_use/tool_result
// rows from the dying turn append AFTER the marker. SpawnDecision
// and chatHistoryToMessages tolerate this ordering.
func TestStopMarkerLandsBeforeLoopCompletes(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.chatHubs = map[string]*chatHub{"alice": hub}

	// Pretend the loop already emitted a tool_use (the parent CLI is
	// mid-subagent when Stop fires).
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleSent, Kind: "tool_use",
		ToolUseID: "u1", ToolName: "subagent",
	}); err != nil {
		t.Fatalf("append tool_use: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("stop code = %d", rr.Code)
	}

	// The cancelled subagent returns its tool_result a moment later
	// (still completes via the parent ctx; only streamCtx was killed).
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "tool_result",
		ToolUseID: "u1", Content: "[error: subagent: cancelled]", IsError: true,
	}); err != nil {
		t.Fatalf("append tool_result: %v", err)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) != 3 {
		t.Fatalf("len(hist) = %d, want 3 (tool_use, user-interruption, tool_result)", len(hist))
	}
	if hist[0].Kind != "tool_use" {
		t.Errorf("hist[0].Kind = %q, want tool_use", hist[0].Kind)
	}
	if hist[1].Kind != store.KindUserInterruption {
		t.Errorf("hist[1].Kind = %q, want user-interruption (must land at click time, not deferred)", hist[1].Kind)
	}
	if hist[2].Kind != "tool_result" {
		t.Errorf("hist[2].Kind = %q, want tool_result", hist[2].Kind)
	}
}

// TestStopRepeatClickRedispatchesCancel: the second (and Nth) Stop
// click must reach whatever's currently in flight, even though the
// marker was already written by the first click. This is the fix
// for failure mode (b) in the SRE report — the prior `if first`
// gate around publishCancelTurn meant a Stop press during a parent's
// retry-after-cancel never propagated.
func TestStopRepeatClickRedispatchesCancel(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	srv.AgentpodHub = newAgentpodHub()
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.chatHubs = map[string]*chatHub{"alice": hub}
	srv.InitAgentpodTurnState("alice", "chat", "tu-active", hub, "mock-large-0", "high")
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)

	const clicks = 3
	for i := 0; i < clicks; i++ {
		req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
		rr := httptest.NewRecorder()
		authedHandler(t, srv).ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("click %d: code = %d", i, rr.Code)
		}
	}

	cancels := 0
	deadline := time.After(time.Second)
loop:
	for cancels < clicks {
		select {
		case ev := <-sub.ch:
			if ev.Type == agentpod.EventCancelTurn {
				cancels++
			}
		case <-deadline:
			break loop
		}
	}
	if cancels != clicks {
		t.Errorf("cancel publishes = %d, want %d (one per click)", cancels, clicks)
	}
}

// TestRecoverInterruptedTurnsWritesDisruptionEntry: the boot scan
// writes a kind:runtime-disruption entry for each orphan marker and
// clears the marker. Auto-spawn is deferred (returned to the
// caller); this test only asserts the disk writes.
func TestRecoverInterruptedTurnsWritesDisruptionEntry(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := srv.Store.MarkActiveTurn("alice", store.ActiveTurnMarker{
		Source: "chat",
		Model:  "mock-large-0",
	}); err != nil {
		t.Fatalf("MarkActiveTurn: %v", err)
	}

	recovered := srv.RecoverInterruptedTurns()
	if len(recovered) != 1 || recovered[0] != "alice" {
		t.Errorf("recovered = %v, want [alice]", recovered)
	}

	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	if len(hist) == 0 {
		t.Fatal("chat.jsonl is empty; expected runtime-disruption entry")
	}
	last := hist[len(hist)-1]
	if last.Kind != store.KindRuntimeDisruption {
		t.Errorf("last entry kind = %q, want %q", last.Kind, store.KindRuntimeDisruption)
	}
	if !strings.Contains(last.Content, "runtime failed") {
		t.Errorf("disruption content unexpectedly empty / wrong: %q", last.Content)
	}
	m, _ := srv.Store.ReadActiveTurn("alice")
	if !m.StartedAt.IsZero() {
		t.Errorf("marker not cleared: %+v", m)
	}
	if got := store.SpawnDecision(hist); got != store.SpawnNow {
		t.Errorf("SpawnDecision after recovery = %v, want SpawnNow", got)
	}
}

// TestRecoverInterruptedTurnsRespectsCircuitBreaker: with
// chatHubQuarantineThreshold-1 prior runtime-disruption entries
// already in chat history, the recovery scan adds the Nth and the
// org snapshot reflects Quarantined=true.
func TestRecoverInterruptedTurnsRespectsCircuitBreaker(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for i := 0; i < chatHubQuarantineThreshold-1; i++ {
		if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
			Role:    store.RoleReceived,
			Kind:    store.KindRuntimeDisruption,
			Content: "prior crash",
		}); err != nil {
			t.Fatalf("seed disruption: %v", err)
		}
	}
	if err := srv.Store.MarkActiveTurn("alice", store.ActiveTurnMarker{Source: "chat"}); err != nil {
		t.Fatalf("MarkActiveTurn: %v", err)
	}

	srv.RecoverInterruptedTurns()

	hist, _ := srv.Store.ReadChatHistory("alice")
	if got := store.ConsecutiveRuntimeDisruptions(hist); got != chatHubQuarantineThreshold {
		t.Errorf("consecutive disruptions after recovery = %d, want %d", got, chatHubQuarantineThreshold)
	}

	body := srv.buildOrgSnapshot()
	var snap orgSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	var alice *agentLiveness
	for i := range snap.Agents {
		if snap.Agents[i].Slug == "alice" {
			alice = &snap.Agents[i]
			break
		}
	}
	if alice == nil {
		t.Fatal("alice not in snapshot")
	}
	if alice.State != "quarantined" {
		t.Errorf("alice.state = %q, want quarantined", alice.State)
	}
}

// TestSpawnRecoveredFiresGate: SpawnRecovered runs spawnChatLoopIfIdle
// for each slug. With a stub Claude, the gate returns true on a
// runtime-disruption entry (SpawnNow verdict).
func TestSpawnRecoveredFiresGate(t *testing.T) {
	srv := newTestServer(t)
	srv.Claude = stubClaude{}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// Seed one runtime-disruption (just below quarantine threshold).
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role:    store.RoleReceived,
		Kind:    store.KindRuntimeDisruption,
		Content: "prior crash",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// SpawnRecovered should call spawnChatLoopIfIdle, which
	// reaches the runtime-not-configured branch (Server.Runtime is
	// nil in the test scaffold) and returns false. We can't
	// observe the spawn's effects without a real runtime, but we
	// can assert it didn't panic and didn't write extra entries.
	srv.SpawnRecovered([]string{"alice"})

	hist, _ := srv.Store.ReadChatHistory("alice")
	count := 0
	for _, m := range hist {
		if m.Kind == store.KindRuntimeDisruption {
			count++
		}
	}
	if count != 1 {
		t.Errorf("runtime-disruption entries = %d, want 1 (SpawnRecovered must not append)", count)
	}
}

// TestOrgSnapshotInterruptedFlag: an agent whose chat history ends
// with a user-interruption (Stop pressed, no redirect yet) shows
// Interrupted=true in the org snapshot.
func TestOrgSnapshotInterruptedFlag(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "go"},
		{Role: store.RoleSent, Content: "starting"},
		{Role: store.RoleReceived, Kind: store.KindUserInterruption, Content: "User pressed Stop."},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	body := srv.buildOrgSnapshot()
	var snap orgSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	var alice *agentLiveness
	for i := range snap.Agents {
		if snap.Agents[i].Slug == "alice" {
			alice = &snap.Agents[i]
			break
		}
	}
	if alice == nil {
		t.Fatal("alice not in snapshot")
	}
	if alice.State != "waiting" {
		t.Errorf("alice.state = %q, want waiting (interrupted, not quarantined)", alice.State)
	}
}

// TestSpawnGateHeldByCircuitBreaker: with ConsecutiveRuntimeDisruptions
// at threshold, spawnChatLoopIfIdle returns false even though
// SpawnDecision says SpawnNow.
func TestSpawnGateHeldByCircuitBreaker(t *testing.T) {
	srv := newTestServer(t)
	srv.Claude = stubClaude{}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for i := 0; i < chatHubQuarantineThreshold; i++ {
		if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
			Role:    store.RoleReceived,
			Kind:    store.KindRuntimeDisruption,
			Content: "prior crash",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if got := store.SpawnDecision(hist); got != store.SpawnNow {
		t.Fatalf("precondition SpawnDecision = %v, want SpawnNow", got)
	}
	if srv.spawnChatLoopIfIdle("alice", "chat") {
		t.Errorf("spawnChatLoopIfIdle returned true; circuit breaker did not fire (threshold=%d)", chatHubQuarantineThreshold)
	}
}

// TestSpawnGateAllowsBelowThreshold: ConsecutiveRuntimeDisruptions
// just below threshold → breaker does NOT fire. Returns false from a
// later branch (Runtime is nil in test scaffold).
func TestSpawnGateAllowsBelowThreshold(t *testing.T) {
	srv := newTestServer(t)
	srv.Claude = stubClaude{}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for i := 0; i < chatHubQuarantineThreshold-1; i++ {
		if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
			Role:    store.RoleReceived,
			Kind:    store.KindRuntimeDisruption,
			Content: "prior crash",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if got := store.ConsecutiveRuntimeDisruptions(hist); got != chatHubQuarantineThreshold-1 {
		t.Fatalf("consecutive count = %d, want %d", got, chatHubQuarantineThreshold-1)
	}
	_ = srv.spawnChatLoopIfIdle("alice", "chat")
	hist2, _ := srv.Store.ReadChatHistory("alice")
	if got := store.ConsecutiveRuntimeDisruptions(hist2); got != chatHubQuarantineThreshold-1 {
		t.Errorf("consecutive count after gate call = %d, want unchanged %d", got, chatHubQuarantineThreshold-1)
	}
}
