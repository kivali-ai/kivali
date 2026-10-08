package web

import (
	"strings"
	"testing"
	"time"
)

// These cover the pod-death path for BACKGROUND work.
//
// Fan-out is the shape that kills a pod: a batch of subagents is N
// `claude` processes against one memory limit, and the kernel OOM-kills
// the pod rather than any single heap self-limiting. When that happens
// the events SSE just stops.
//
// Subagent turns are registered in a different map from the parent's
// chat turn, and need their own sweep: the OOM case is precisely the
// one where there is no parent turn to sweep, because dispatching the
// batch is what ended the parent's turn. Without it an orphaned job
// would sit in DriveSubagent forever waiting for a terminator from a
// process that is gone.

// shortDisconnectGrace shrinks the grace window for the duration of a
// test. The sweep fires from time.AfterFunc, so tests wait on the
// job's done channel rather than on the clock.
func shortDisconnectGrace(t *testing.T) {
	t.Helper()
	prev := agentpodDisconnectGrace
	agentpodDisconnectGrace = time.Millisecond
	t.Cleanup(func() { agentpodDisconnectGrace = prev })
}

// awaitTurnResult blocks on a subagent turn's done channel. The
// timeout is a failure guard, not a poll interval — the sweep either
// signals promptly or the test has found the bug.
func awaitTurnResult(t *testing.T, st *agentpodTurnState) (subagentTurnResult, bool) {
	t.Helper()
	select {
	case res := <-st.done:
		return res, true
	case <-time.After(5 * time.Second):
		return subagentTurnResult{}, false
	}
}

// TestDisconnectSweepFailsOrphanedSubagentTurns: pod dies, no parent
// turn in flight, background jobs still registered.
func TestDisconnectSweepFailsOrphanedSubagentTurns(t *testing.T) {
	shortDisconnectGrace(t)
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub() // no subscribers == pod gone

	// Two background turns, no parent turn — the state an agent is in
	// after dispatching a batch and ending its turn.
	a := srv.installAgentpodSubagentTurn("alice", "turn-a", "job-a", "claude-test")
	b := srv.installAgentpodSubagentTurn("alice", "turn-b", "job-b", "claude-test")

	srv.handleAgentpodDisconnect("alice")

	for _, st := range []*agentpodTurnState{a, b} {
		res, ok := awaitTurnResult(t, st)
		if !ok {
			t.Fatalf("turn %s never got a terminator — DriveSubagent would block forever "+
				"and the parent would show it as outstanding indefinitely", st.turnID)
		}
		if res.Err == nil {
			t.Errorf("turn %s finished with no error; a dead pod is a failure", st.turnID)
		}
	}
	// Both entries evicted, so nothing counts them as in flight later.
	if n := len(srv.collectInflightSubagentTurns("alice")); n != 0 {
		t.Errorf("%d subagent turn(s) still registered after the sweep", n)
	}
}

// TestDisconnectSweepSparesReconnect: a runtime that reconnects
// inside the grace window is alive, and its work must be left alone.
// This is the normal pod-restart path, not a death.
func TestDisconnectSweepSparesReconnect(t *testing.T) {
	prev := agentpodDisconnectGrace
	agentpodDisconnectGrace = 40 * time.Millisecond
	t.Cleanup(func() { agentpodDisconnectGrace = prev })

	srv := newTestServer(t)
	hub := NewAgentpodHub()
	srv.AgentpodHub = hub
	st := srv.installAgentpodSubagentTurn("alice", "turn-a", "job-a", "claude-test")

	srv.handleAgentpodDisconnect("alice")
	// Runtime comes back before the window closes.
	sub := hub.Subscribe("alice")
	defer hub.Unsubscribe(sub)

	select {
	case res := <-st.done:
		t.Fatalf("swept a turn whose runtime reconnected: %+v", res)
	case <-time.After(200 * time.Millisecond):
		// Window has passed with the turn left alone, which is right.
	}
	if n := len(srv.collectInflightSubagentTurns("alice")); n != 1 {
		t.Errorf("turn count = %d, want 1 (still in flight)", n)
	}
}

// TestDisconnectSweepSkipsTurnsThatFinished: a job that completed
// normally inside the grace window is already out of the registry.
// Re-failing it would deliver a second terminator for one run.
func TestDisconnectSweepSkipsTurnsThatFinished(t *testing.T) {
	shortDisconnectGrace(t)
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	st := srv.installAgentpodSubagentTurn("alice", "turn-a", "job-a", "claude-test")

	// Finish it the ordinary way before the sweep runs.
	srv.finalizeSubagentTurn(st, subagentTurnResult{FinalText: "done properly"})
	res, ok := awaitTurnResult(t, st)
	if !ok || res.Err != nil {
		t.Fatalf("setup: normal finish didn't land (ok=%v res=%+v)", ok, res)
	}

	srv.handleAgentpodDisconnect("alice")

	// Nothing more may arrive on done: the channel is buffered size 1
	// and a second finalize would either queue a bogus failure or be
	// dropped. Assert the registry is clean and no second result came.
	select {
	case extra := <-st.done:
		t.Errorf("second terminator for an already-finished turn: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestDisconnectSweepIgnoresOtherAgents: pod death for one agent must
// not fail another agent's background work.
func TestDisconnectSweepIgnoresOtherAgents(t *testing.T) {
	shortDisconnectGrace(t)
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	mine := srv.installAgentpodSubagentTurn("alice", "turn-a", "job-a", "claude-test")
	theirs := srv.installAgentpodSubagentTurn("bob", "turn-b", "job-b", "claude-test")

	srv.handleAgentpodDisconnect("alice")

	if _, ok := awaitTurnResult(t, mine); !ok {
		t.Fatal("alice's orphaned turn not swept")
	}
	select {
	case res := <-theirs.done:
		t.Errorf("bob's turn swept by alice's pod death: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}
	if n := len(srv.collectInflightSubagentTurns("bob")); n != 1 {
		t.Errorf("bob's turn count = %d, want 1 (untouched)", n)
	}
}

// TestDisconnectSweepNoopWhenNothingInFlight guards the early return:
// an idle agent's pod going away must not schedule work.
func TestDisconnectSweepNoopWhenNothingInFlight(t *testing.T) {
	shortDisconnectGrace(t)
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	// No panic, no goroutine leak, nothing to assert beyond surviving.
	srv.handleAgentpodDisconnect("alice")
	if n := len(srv.collectInflightSubagentTurns("alice")); n != 0 {
		t.Errorf("turn count = %d, want 0", n)
	}
}

// TestSweptSubagentFailureCarriesPodDetail: the error the parent ends
// up seeing should say the pod died, not something generic — this is
// the OOM diagnosis someone will be reading later.
func TestSweptSubagentFailureCarriesPodDetail(t *testing.T) {
	shortDisconnectGrace(t)
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	st := srv.installAgentpodSubagentTurn("alice", "turn-a", "job-a", "claude-test")

	srv.handleAgentpodDisconnect("alice")

	res, ok := awaitTurnResult(t, st)
	if !ok {
		t.Fatal("no terminator")
	}
	if res.Err == nil {
		t.Fatal("no error on a swept turn")
	}
	if got := res.Err.Error(); !strings.Contains(got, "disconnected") || !strings.Contains(got, "pod presumed dead") {
		t.Errorf("error = %q; want it to name the disconnect and the presumed-dead pod", got)
	}
	// The reason must not be Cancelled: nobody asked for this, and
	// cancelled jobs deliver nothing, which would hide a real failure
	// from the parent entirely.
	if res.Err == errSubagentCancelled {
		t.Error("swept turn reported as cancelled; the parent would never hear about the failure")
	}
}
