package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// seedOwedAgent creates slug with one unanswered received row.
func seedOwedAgent(t *testing.T, srv *Server, slug string) {
	t.Helper()
	if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := srv.Store.AppendChatMessage(slug, store.ChatMessage{
		Role: store.RoleReceived, Kind: "notice", Content: "the numbers are in",
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
}

// chatTurnsQueued drains sub's buffer and returns the chat-turn events
// in it. Publishing is synchronous, so everything a wake published is
// already buffered when the wake returns.
func chatTurnsQueued(t *testing.T, sub *agentpodSubscriber) []agentpod.ChatTurnEvent {
	t.Helper()
	var out []agentpod.ChatTurnEvent
	for {
		select {
		case ev := <-sub.ch:
			if ev.Type != agentpod.EventChatTurn {
				continue
			}
			var ct agentpod.ChatTurnEvent
			if err := json.Unmarshal(ev.Data, &ct); err != nil {
				t.Fatalf("decode ChatTurnEvent: %v", err)
			}
			out = append(out, ct)
		default:
			return out
		}
	}
}

// A release that wakes an agent whose pod is not subscribed starts
// nothing; the agent's turn starts once, when its pod subscribes, and a
// second subscribe (a reconnect) does not start another.
func TestReleaseWakeWithoutPodRunsWhenPodSubscribes(t *testing.T) {
	srv := newChatServer(t)
	seedOwedAgent(t, srv, "alice")

	if srv.deliveryHooks().WakeAgent("alice") {
		t.Fatal("release wake started a turn with no agent pod subscribed")
	}
	if orphans, _ := srv.Store.ListOrphanActiveTurns(); len(orphans) != 0 {
		t.Errorf("orphan active turns after the dropped wake: %v; the next boot would record a disruption that never happened", orphans)
	}

	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)
	srv.wakeOnAgentpodSubscribe("alice")

	turns := chatTurnsQueued(t, sub)
	if len(turns) != 1 {
		t.Fatalf("chat-turn events after the pod subscribed: %d, want 1", len(turns))
	}
	if turns[0].Slug != "alice" || turns[0].Source != "release" {
		t.Errorf("chat-turn = slug %q source %q, want alice/release", turns[0].Slug, turns[0].Source)
	}

	srv.wakeOnAgentpodSubscribe("alice")
	if extra := chatTurnsQueued(t, sub); len(extra) != 0 {
		t.Errorf("a second subscribe started %d more turns while the first was running", len(extra))
	}
}

// A pod subscribing starts nothing for an agent with nothing to answer
// or one the spawn gate holds.
func TestPodSubscribeWithoutOwedWorkStartsNothing(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, srv *Server)
	}{
		{"empty chat", func(t *testing.T, srv *Server) {
			if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
				t.Fatal(err)
			}
		}},
		{"already answered", func(t *testing.T, srv *Server) {
			seedOwedAgent(t, srv, "alice")
			if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
				Role: store.RoleSent, Content: "noted",
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"offboarded", func(t *testing.T, srv *Server) {
			seedOwedAgent(t, srv, "alice")
			if err := srv.Store.ArchiveAgent("alice"); err != nil {
				t.Fatal(err)
			}
		}},
		{"quarantined", func(t *testing.T, srv *Server) {
			seedOwedAgent(t, srv, "alice")
			for range chatHubQuarantineThreshold {
				if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
					Role: store.RoleReceived, Kind: store.KindRuntimeDisruption, Content: "The runtime failed mid-task.",
				}); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newChatServer(t)
			tc.seed(t, srv)
			sub := srv.AgentpodHub.Subscribe("alice")
			defer srv.AgentpodHub.Unsubscribe(sub)
			srv.wakeOnAgentpodSubscribe("alice")
			if turns := chatTurnsQueued(t, sub); len(turns) != 0 {
				t.Errorf("chat-turn events: %d, want none", len(turns))
			}
		})
	}
}

// After a restart, an agent with an interrupted turn is woken both by
// SpawnRecovered and by its pod's subscription; it runs one turn,
// whichever comes first.
func TestPodSubscribeAndSpawnRecoveredStartOneTurn(t *testing.T) {
	for _, recoveredFirst := range []bool{false, true} {
		name := "subscribe wake first"
		if recoveredFirst {
			name = "SpawnRecovered first"
		}
		t.Run(name, func(t *testing.T) {
			srv := newChatServer(t)
			seedOwedAgent(t, srv, "alice")
			if err := srv.Store.MarkActiveTurn("alice", store.ActiveTurnMarker{
				StartedAt: time.Now().UTC(), Source: "release", Model: "mock-large-0",
			}); err != nil {
				t.Fatal(err)
			}
			recovered := srv.RecoverInterruptedTurns()
			if len(recovered) != 1 || recovered[0] != "alice" {
				t.Fatalf("recovered = %v, want [alice]", recovered)
			}

			sub := srv.AgentpodHub.Subscribe("alice")
			defer srv.AgentpodHub.Unsubscribe(sub)
			// The pod is subscribed, so spawnRecoveredOnce starts at once.
			if recoveredFirst {
				srv.spawnRecoveredOnce("alice")
				srv.wakeOnAgentpodSubscribe("alice")
			} else {
				srv.wakeOnAgentpodSubscribe("alice")
				srv.spawnRecoveredOnce("alice")
			}
			if turns := chatTurnsQueued(t, sub); len(turns) != 1 {
				t.Errorf("chat-turn events: %d, want 1", len(turns))
			}
		})
	}
}

// End to end over the socket: the turn a release left owed reaches the
// pod as the first event on its events stream.
func TestAgentpodEventsStreamDeliversOwedTurn(t *testing.T) {
	srv := newChatServer(t)
	seedOwedAgent(t, srv, "alice")
	if srv.deliveryHooks().WakeAgent("alice") {
		t.Fatal("release wake started a turn with no agent pod subscribed")
	}

	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "s")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/events", srv.handleAgentpodEvents)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs, err := agentpod.NewClient(sockPath, "alice").OpenEvents(ctx)
	if err != nil {
		t.Fatalf("OpenEvents: %v", err)
	}
	select {
	case ev, ok := <-evs:
		if !ok {
			t.Fatal("events stream closed before the owed turn arrived")
		}
		if ev.Type != agentpod.EventChatTurn {
			t.Fatalf("first event = %q, want %q", ev.Type, agentpod.EventChatTurn)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owed turn never reached the pod")
	}
}

// A pod that subscribes while a spawn holds the agent's hub finds it
// claimed, so its subscribe wake starts nothing. The spawn found no pod
// when it published, so the dropped path starts the turn itself once it
// lets go of the hub: one turn, on the pod that subscribed.
func TestDroppedSpawnStartsTheTurnForAPodThatSubscribedMeanwhile(t *testing.T) {
	srv := newChatServer(t)
	seedOwedAgent(t, srv, "alice")
	var sub *agentpodSubscriber
	srv.spawnDropped = func(slug string) {
		sub = srv.AgentpodHub.Subscribe(slug)
		srv.wakeOnAgentpodSubscribe(slug)
	}
	if !srv.spawnChatLoopIfIdle("alice", "release") {
		t.Fatal("spawn reported no turn started; the pod that subscribed meanwhile was left idle")
	}
	defer srv.AgentpodHub.Unsubscribe(sub)
	if turns := chatTurnsQueued(t, sub); len(turns) != 1 {
		t.Errorf("chat-turn events: %d, want 1", len(turns))
	}
}

// A spawn that started no turn lets go of only its own hub. Once it has
// completed that hub, a pod's subscribe wake can claim a fresh one and
// start a turn on it; the abandoned spawn must leave that hub in place,
// so its retry finds the turn running and does not start a second one.
func TestAbandonedSpawnLeavesTheNextSpawnsHub(t *testing.T) {
	srv := newChatServer(t)
	seedOwedAgent(t, srv, "alice")
	first, isOriginator := srv.getOrCreateHub("alice")
	if !isOriginator {
		t.Fatal("setup: first claim is not the originator")
	}
	// The abandoned spawn has completed its hub...
	srv.flushAndComplete("alice", first)
	// ...when the pod subscribes and its wake starts the turn.
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)
	srv.wakeOnAgentpodSubscribe("alice")
	srv.streamMu.Lock()
	second := srv.chatHubs["alice"]
	srv.streamMu.Unlock()
	if second == nil || second == first {
		t.Fatal("setup: the subscribe wake did not claim a hub of its own")
	}

	srv.abandonSpawn("alice", first, "release")
	srv.streamMu.Lock()
	cur := srv.chatHubs["alice"]
	srv.streamMu.Unlock()
	if cur != second {
		t.Fatalf("hub after the abandoned spawn let go = %p, want the running turn's %p", cur, second)
	}
	// The dropped path's retry, now that a pod is subscribed.
	srv.spawnChatLoopIfIdle("alice", "release")
	if turns := chatTurnsQueued(t, sub); len(turns) != 1 {
		t.Errorf("chat-turn events: %d, want 1", len(turns))
	}
}
