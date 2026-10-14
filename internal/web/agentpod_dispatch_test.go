package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestSpawnChatLoopAgentpodPathTakes is the happy path: a subscribed
// agent runtime receives a chat-turn event with the inlined
// CompleteRequest, and per-slug turn state gets installed so the
// upcoming TurnEvents can correlate.
func TestSpawnChatLoopAgentpodPathTakes(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "what's up",
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)

	if !srv.spawnChatLoopIfIdle("alice", "chat") {
		t.Fatal("spawnChatLoopIfIdle returned false; expected agent-pod path to claim the spawn")
	}

	select {
	case ev := <-sub.ch:
		if ev.Type != agentpod.EventChatTurn {
			t.Fatalf("ev.Type = %q, want %q", ev.Type, agentpod.EventChatTurn)
		}
		var ct agentpod.ChatTurnEvent
		if err := json.Unmarshal(ev.Data, &ct); err != nil {
			t.Fatalf("decode ChatTurnEvent: %v", err)
		}
		if ct.Slug != "alice" || ct.Source != "chat" {
			t.Errorf("ChatTurnEvent = %+v, want slug=alice source=chat", ct)
		}
		if ct.TurnID == "" {
			t.Errorf("turn_id empty")
		}
		if len(ct.Request) == 0 {
			t.Errorf("request bytes empty; the agent runtime needs the full CompleteRequest inline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent runtime never received a chat-turn event")
	}

	// Per-slug turn state was installed so the upcoming TurnEvents can
	// look it up.
	if st := srv.lookupAgentpodTurn("alice"); st == nil {
		t.Errorf("agentpodTurns entry missing after publishAgentpodChatTurn")
	}
}

// TestSpawnChatLoopFailsWithoutSubscriber covers the no-subscriber
// drop: when no agent runtime is subscribed for slug (fresh hire,
// pod restart in flight), the spawn returns false and tears down the
// hub so the UI doesn't hang on a stuck "thinking" indicator. No turn
// started, so no active-turn marker is left behind: the next boot must
// not record a runtime disruption for it. The pod's subscription starts
// the turn instead (agentpod_subscribe_wake_test.go).
func TestSpawnChatLoopFailsWithoutSubscriber(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "what's up",
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	// AgentpodHub is wired but no one subscribed for alice.
	if srv.spawnChatLoopIfIdle("alice", "chat") {
		t.Fatal("spawnChatLoopIfIdle returned true with no agent-pod subscriber; expected drop")
	}
	if srv.ChatHubActive("alice") {
		t.Error("hub left active after dropped spawn; should be torn down")
	}
	m, err := srv.Store.ReadActiveTurn("alice")
	if err != nil {
		t.Fatalf("ReadActiveTurn: %v", err)
	}
	if !m.StartedAt.IsZero() {
		t.Errorf("active-turn marker left after a dropped spawn: %+v", m)
	}
	orphans, err := srv.Store.ListOrphanActiveTurns()
	if err != nil {
		t.Fatalf("ListOrphanActiveTurns: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphan active turns after a dropped spawn: %v, want none", orphans)
	}
	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	if got := store.SpawnDecision(hist); got != store.SpawnNow {
		t.Errorf("SpawnDecision after a dropped spawn = %v, want SpawnNow: the message is still owed", got)
	}
}

// TestNewAgentpodTurnIDUnique sanity-checks the turnID generator —
// 16 hex chars and unique across calls. Cheap regression on a
// load-bearing correlation key.
func TestNewAgentpodTurnIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newAgentpodTurnID()
		if len(id) != 16 {
			t.Fatalf("turnID %q has %d chars, want 16", id, len(id))
		}
		if seen[id] {
			t.Fatalf("collision after %d ids: %q", i, id)
		}
		seen[id] = true
	}
}
