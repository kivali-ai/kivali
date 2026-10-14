package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// startTurnOn subscribes runtime for slug, as a pod's events stream
// does, and starts slug's owed turn on it. Returns the subscription,
// drained, and the turn.
func startTurnOn(t *testing.T, srv *Server, slug, runtime string) (*agentpodSubscriber, *agentpodTurnState) {
	t.Helper()
	sub := srv.AgentpodHub.SubscribeRuntime(slug, runtime)
	srv.adoptRuntime(slug, runtime)
	if !srv.spawnChatLoopIfIdle(slug, "release") {
		t.Fatal("setup: no turn started")
	}
	st := srv.lookupAgentpodTurn(slug)
	if st == nil || st.runtime != runtime {
		t.Fatalf("setup: turn = %+v, want one on runtime %q", st, runtime)
	}
	chatTurnsQueued(t, sub)
	return sub, st
}

// awaitHubClosed waits for a turn's chat hub to close, which its
// finalize does once the turn's rows are written.
func awaitHubClosed(t *testing.T, st *agentpodTurnState) {
	t.Helper()
	select {
	case <-st.hub.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never ended")
	}
}

// disruptions is slug's runtime-disruption rows.
func disruptions(t *testing.T, srv *Server, slug string) []store.ChatMessage {
	t.Helper()
	hist, err := srv.Store.ReadChatHistory(slug)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.ChatMessage
	for _, m := range hist {
		if m.Kind == store.KindRuntimeDisruption {
			out = append(out, m)
		}
	}
	return out
}

// A pod replaced mid-turn: the new pod's runtime subscribes while the
// old one's turn is still registered. The turn ends as a disruption at
// once, and the new pod's subscribe wake resumes it there: one turn,
// never two. When Kivali recreated the pod, the disruption is Planned
// and the crash-loop breaker does not count it.
func TestNewRuntimeEndsTheOldRuntimesTurnAndResumesIt(t *testing.T) {
	for _, planned := range []bool{true, false} {
		name := "pod died"
		if planned {
			name = "Kivali recreated the pod"
		}
		t.Run(name, func(t *testing.T) {
			srv := newChatServer(t)
			seedOwedAgent(t, srv, "alice")
			old, st := startTurnOn(t, srv, "alice", "runtime-1")
			srv.AgentpodHub.Unsubscribe(old)
			if planned {
				srv.markRestartPlanned("alice")
			}

			sub := srv.AgentpodHub.SubscribeRuntime("alice", "runtime-2")
			defer srv.AgentpodHub.Unsubscribe(sub)
			srv.adoptRuntime("alice", "runtime-2")
			awaitHubClosed(t, st)
			srv.wakeOnAgentpodSubscribe("alice")
			srv.spawnRecoveredOnce("alice")

			rows := disruptions(t, srv, "alice")
			if len(rows) != 1 || rows[0].Planned != planned {
				t.Fatalf("disruption rows = %+v, want one with Planned=%v", rows, planned)
			}
			turns := chatTurnsQueued(t, sub)
			if len(turns) != 1 {
				t.Fatalf("chat-turn events on the new runtime: %d, want 1", len(turns))
			}
			if cur := srv.lookupAgentpodTurn("alice"); cur == nil || cur.turnID != turns[0].TurnID || cur.runtime != "runtime-2" {
				t.Errorf("turn in flight = %+v, want the resumed one on runtime-2", cur)
			}
			hist, _ := srv.Store.ReadChatHistory("alice")
			want := 1
			if planned {
				want = 0
			}
			if got := store.ConsecutiveRuntimeDisruptions(hist); got != want {
				t.Errorf("crash-loop count = %d, want %d", got, want)
			}
			if srv.restartPlanned("alice") {
				t.Error("the planned restart is still recorded after the new runtime subscribed")
			}
		})
	}
}

// The same runtime reconnecting (a dropped stream) is alive: its turn
// stays in flight and nothing new starts.
func TestSameRuntimeReconnectKeepsItsTurn(t *testing.T) {
	srv := newChatServer(t)
	seedOwedAgent(t, srv, "alice")
	old, st := startTurnOn(t, srv, "alice", "runtime-1")
	srv.AgentpodHub.Unsubscribe(old)

	sub := srv.AgentpodHub.SubscribeRuntime("alice", "runtime-1")
	defer srv.AgentpodHub.Unsubscribe(sub)
	srv.adoptRuntime("alice", "runtime-1")
	srv.wakeOnAgentpodSubscribe("alice")
	if cur := srv.lookupAgentpodTurn("alice"); cur != st {
		t.Errorf("turn in flight = %+v, want the original", cur)
	}
	if turns := chatTurnsQueued(t, sub); len(turns) != 0 {
		t.Errorf("chat-turn events: %d, want none", len(turns))
	}
	if rows := disruptions(t, srv, "alice"); len(rows) != 0 {
		t.Errorf("disruption rows = %+v, want none", rows)
	}
}

// A runtime that is gone and not back within the grace: its turn ends
// as a disruption (Planned when Kivali recreated the pod) rather than an
// error that would hold the agent, so the agent resumes on its next pod.
func TestRuntimeGoneForTheGraceEndsItsTurnAsADisruption(t *testing.T) {
	for _, planned := range []bool{true, false} {
		t.Run(map[bool]string{true: "planned", false: "pod died"}[planned], func(t *testing.T) {
			srv := newChatServer(t)
			fc := clock.NewFake()
			srv.Clock = fc
			seedOwedAgent(t, srv, "alice")
			sub, st := startTurnOn(t, srv, "alice", "runtime-1")
			if planned {
				srv.markRestartPlanned("alice")
			}
			srv.AgentpodHub.Unsubscribe(sub)
			srv.handleAgentpodDisconnect("alice")
			fc.Advance(agentpodDisconnectGrace)
			awaitHubClosed(t, st)

			rows := disruptions(t, srv, "alice")
			if len(rows) != 1 || rows[0].Planned != planned {
				t.Fatalf("disruption rows = %+v, want one with Planned=%v", rows, planned)
			}
			hist, _ := srv.Store.ReadChatHistory("alice")
			if v := store.SpawnDecision(hist); v != store.SpawnNow {
				t.Errorf("spawn verdict = %v, want SpawnNow: the agent resumes when its pod is back", v)
			}
		})
	}
}

// The runtime the turn runs on is still subscribed when another stream
// of the agent's drops: the grace ends nothing.
func TestDisconnectOfAnotherStreamSparesALiveRuntimesTurn(t *testing.T) {
	srv := newChatServer(t)
	fc := clock.NewFake()
	srv.Clock = fc
	seedOwedAgent(t, srv, "alice")
	sub, st := startTurnOn(t, srv, "alice", "runtime-1")
	defer srv.AgentpodHub.Unsubscribe(sub)
	srv.handleAgentpodDisconnect("alice")
	if fc.Waiters() != 0 {
		t.Errorf("a grace timer is armed for a turn whose runtime is subscribed")
	}
	if cur := srv.lookupAgentpodTurn("alice"); cur != st {
		t.Errorf("turn in flight = %+v, want the original", cur)
	}
}

// A runtime shutting down mid-turn reports the turn stopped; Kivali's
// own pod restart makes the disruption Planned.
func TestRuntimeStoppedEndsTheTurnAsADisruption(t *testing.T) {
	for _, planned := range []bool{true, false} {
		t.Run(map[bool]string{true: "planned", false: "evicted"}[planned], func(t *testing.T) {
			srv := newChatServer(t)
			seedOwedAgent(t, srv, "alice")
			sub, st := startTurnOn(t, srv, "alice", "runtime-1")
			defer srv.AgentpodHub.Unsubscribe(sub)
			if planned {
				srv.markRestartPlanned("alice")
			}
			srv.onAgentpodFailed(st, agentpod.TurnEvent{Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonRuntimeStopped, FailedDetail: "the agent runtime is shutting down"})
			rows := disruptions(t, srv, "alice")
			if len(rows) != 1 || rows[0].Planned != planned {
				t.Fatalf("disruption rows = %+v, want one with Planned=%v", rows, planned)
			}
		})
	}
}

// At boot, a pod still running from before the restart dials in before
// the startup pass has provisioned it: its subscribe wake starts
// nothing, and the turn starts once the pass is done with the agent.
func TestStartupHoldsTurnsUntilThePodIsProvisioned(t *testing.T) {
	srv := newChatServer(t)
	seedOwedAgent(t, srv, "alice")
	srv.AgentPod = &fakeAgentPodLifecycle{}
	srv.HoldTurnsForStartup()

	sub := srv.AgentpodHub.SubscribeRuntime("alice", "runtime-1")
	defer srv.AgentpodHub.Unsubscribe(sub)
	srv.adoptRuntime("alice", "runtime-1")
	srv.wakeOnAgentpodSubscribe("alice")
	if turns := chatTurnsQueued(t, sub); len(turns) != 0 {
		t.Fatalf("a turn started on a pod the startup pass has yet to provision: %d", len(turns))
	}

	srv.BulkProvisionAgentPods(context.Background())
	if turns := chatTurnsQueued(t, sub); len(turns) != 1 {
		t.Errorf("chat-turn events once the pod is provisioned: %d, want 1", len(turns))
	}
	srv.spawnRecoveredOnce("alice")
	if turns := chatTurnsQueued(t, sub); len(turns) != 0 {
		t.Errorf("a second turn started: %d", len(turns))
	}
}

// A startup pass that gives up still lets the agents' turns go.
func TestStartupHoldsEndWithThePass(t *testing.T) {
	srv := newChatServer(t)
	seedOwedAgent(t, srv, "alice")
	srv.AgentPod = &fakeAgentPodLifecycle{provErr: context.DeadlineExceeded}
	srv.HoldTurnsForStartup()
	sub := srv.AgentpodHub.SubscribeRuntime("alice", "runtime-1")
	defer srv.AgentpodHub.Unsubscribe(sub)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv.BulkProvisionAgentPods(ctx)
	if turns := chatTurnsQueued(t, sub); len(turns) != 1 {
		t.Errorf("chat-turn events after the pass gave up: %d, want 1", len(turns))
	}
}

// signalingSoftwarePod is softwarePod that also reports each provision,
// and whether the agent had a turn running then, on a channel.
type signalingSoftwarePod struct {
	*softwarePod
	srv *Server
	got chan bool
}

func (p *signalingSoftwarePod) Provision(ctx context.Context, slug string) error {
	err := p.softwarePod.Provision(ctx, slug)
	p.got <- p.srv.ChatHubActive(slug)
	return err
}

// Approving software for an agent at work does not cut its turn short:
// the answer says the shell restarts once the work has ended, and the
// pod is recreated when the turn ends, before the next turn starts.
func TestApproveSoftwareForABusyAgentRestartsWhenItsTurnEnds(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.AgentpodHub = newAgentpodHub()
	softwareTeam(t, srv)
	pod := &signalingSoftwarePod{softwarePod: &softwarePod{st: srv.Store}, srv: srv, got: make(chan bool, 4)}
	srv.AgentPod = pod
	rel := seedSoftwareProposal(t, srv, "buyer", "ffmpeg")
	if err := srv.Store.AppendChatMessage("buyer", store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "work on the report"}); err != nil {
		t.Fatal(err)
	}
	sub, st := startTurnOn(t, srv, "buyer", "runtime-1")
	defer srv.AgentpodHub.Unsubscribe(sub)

	body, ct := needsForm(t, rel, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil)
	got := decodeAPI[apitypes.NeedActionResponse](t, rr)
	if rr.Code != http.StatusOK || got.Result != "Buyer restarts to install the software once it has finished what it is doing" {
		t.Errorf("approve: code %d result %+v", rr.Code, got)
	}
	if si := approvedSoftware(t, srv); si.Outcome != store.SoftwareRestartWhenIdle {
		t.Errorf("outcome = %q", si.Outcome)
	}
	select {
	case <-pod.got:
		t.Fatal("the pod was recreated while the agent's turn ran")
	default:
	}

	srv.onAgentpodDone(st, agentpod.TurnEvent{Kind: agentpod.TurnEventDone})
	select {
	case busy := <-pod.got:
		if busy {
			t.Error("the pod was recreated with a turn running")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pod was never recreated after the turn ended")
	}
	// The answer reached the agent; the follow-up turn that reads it
	// starts once the pod is recreated, on the pod that is subscribed.
	hist, _ := srv.Store.ReadChatHistory("buyer")
	found := false
	for _, m := range hist {
		if strings.Contains(m.Content, "Your shell restarts to install it once your current turn and background tasks have ended") {
			found = true
		}
	}
	if !found {
		t.Errorf("the agent was not told when its shell restarts; chat = %+v", hist)
	}
}

// Approving software for an idle agent recreates its pod before the
// answer, with turns held meanwhile, as before.
func TestApproveSoftwareForAnIdleAgentRestartsAtOnce(t *testing.T) {
	srv, _ := newTurnServer(t)
	srv.AgentpodHub = newAgentpodHub()
	softwareTeam(t, srv)
	pod := &signalingSoftwarePod{softwarePod: &softwarePod{st: srv.Store}, srv: srv, got: make(chan bool, 4)}
	srv.AgentPod = pod
	rel := seedSoftwareProposal(t, srv, "buyer", "ffmpeg")
	if _, err := srv.respondAsCEO(context.Background(), responseKindApproval, boolPtr(true), rel, "", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pod.got:
	default:
		t.Fatal("the idle agent's pod was not recreated before the answer")
	}
	if si := approvedSoftware(t, srv); si.Outcome != store.SoftwareRestarted {
		t.Errorf("outcome = %q", si.Outcome)
	}
	srv.streamMu.Lock()
	held := srv.turnsHeldLocked("buyer")
	srv.streamMu.Unlock()
	if held {
		t.Error("turns still held after the restart")
	}
}
