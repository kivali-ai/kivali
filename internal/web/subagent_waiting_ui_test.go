package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// An agent dispatches background subagents, says what it's doing, and
// ends its turn. The work it owns is still running, and the live
// surfaces must not render it idle. An explicit waiting_tasks count
// rides the two wires the browser actually reads after a turn ends (the "done" SSE payload and
// the /org/stream snapshot), because "done" closes the EventSource and
// there is no other channel left.

// withOutstandingJobs attaches a SubagentService to srv holding n
// running jobs for parent. Registering directly keeps these tests off
// the driver/dispatch path — what is under test is how an outstanding
// count reaches the UI, not how jobs come to exist.
func withOutstandingJobs(t *testing.T, srv *Server, parent string, n int) {
	t.Helper()
	if srv.SubagentService == nil {
		srv.SubagentService = &SubagentService{}
	}
	for i := 0; i < n; i++ {
		srv.SubagentService.registerJob(&subagentJob{
			ID:          parent + "-job-" + string(rune('a'+i)),
			Parent:      parent,
			Description: "background work",
			State:       subagentJobRunning,
			StartedAt:   time.Now().UTC(),
			Depth:       1,
		})
	}
	if got := srv.SubagentService.OutstandingSubagents(parent); got != n {
		t.Fatalf("setup: outstanding = %d, want %d", got, n)
	}
}

// TestOrgSnapshotCarriesWaitingTasksApartFromWorking is the load-bearing
// one. waiting_tasks must travel as its OWN field and must not turn
// working on.
//
// If waiting were folded into working, chat.js's steady-state
// reconciler would see working=true with no live EventSource and call
// openStream on every snapshot. The server has no hub for an agent
// that is merely parked on background work, so it answers 204, the
// EventSource error path fires, and the thinking indicator is torn
// down — once per snapshot, forever. The separation is what keeps the
// bubble up.
func TestOrgSnapshotCarriesWaitingTasksApartFromWorking(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"},
		{Slug: "bob", Role: "Researcher", ReportsTo: "ceo"},
	} {
		if err := srv.Store.CreateAgent(a, "k"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	// alice has background work outstanding and NO chat hub — her turn
	// is over. This is exactly the reported state.
	withOutstandingJobs(t, srv, "alice", 2)

	var snap orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byslug := map[string]agentLiveness{}
	for _, a := range snap.Agents {
		byslug[a.Slug] = a
	}

	if got := byslug["alice"].WaitingTasks; got != 2 {
		t.Errorf("alice.waiting_tasks = %d, want 2 — the sidebar and the "+
			"thinking bubble have nothing to read without it", got)
	}
	if byslug["alice"].State != "waiting" {
		t.Errorf("alice.state = %q, want waiting; waiting on background work must NOT read as running. "+
			"running means \"there is a turn to attach to\", and attaching to "+
			"a parked agent 204s and tears the indicator down every snapshot", byslug["alice"].State)
	}
	if got := byslug["bob"].WaitingTasks; got != 0 {
		t.Errorf("bob.waiting_tasks = %d, want 0 — the count must be per-parent", got)
	}
}

// TestOrgSnapshotOmitsWaitingTasksWhenZero pins the wire shape: an idle
// agent carries no waiting_tasks key at all (omitempty), so `working:
// false` with nothing else still decodes to a clean idle state on the
// client.
func TestOrgSnapshotOmitsWaitingTasksWhenZero(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := string(srv.buildOrgSnapshot())
	if strings.Contains(body, "waiting_tasks") {
		t.Errorf("idle snapshot carries waiting_tasks; want it omitted:\n%s", body)
	}
}

// TestAgentpodDonePayloadCarriesWaitingTasks covers the handoff moment.
// "done" is the last thing the client hears on that stream — the
// handler closes the EventSource — so the count has to ride along with
// it. Without it the client cannot tell "turn finished, nothing left"
// from "turn finished, three subagents still going" and clears the
// indicator for both.
func TestAgentpodDonePayloadCarriesWaitingTasks(t *testing.T) {
	path, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	withOutstandingJobs(t, srv, "alice", 3)

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:       agentpod.TurnEventDone,
		StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}

	got, ok := hubEventByKind(hub, "done")
	if !ok {
		t.Fatalf("no done emit on hub; replay = %v", hubEventKinds(hub))
	}
	// JSON numbers decode as float64.
	n, isNum := got["waiting_tasks"].(float64)
	if !isNum {
		t.Fatalf("done.waiting_tasks = %#v (%T), want a number — chat.js reads "+
			"this to decide whether to keep the thinking bubble up", got["waiting_tasks"], got["waiting_tasks"])
	}
	if int(n) != 3 {
		t.Errorf("done.waiting_tasks = %d, want 3", int(n))
	}
}

// TestAgentpodDonePayloadWaitingTasksZeroWhenIdle is the other half:
// an ordinary turn with no background work must report 0, so the
// client takes its existing teardown path and the bubble does not
// linger forever.
func TestAgentpodDonePayloadWaitingTasksZeroWhenIdle(t *testing.T) {
	path, _, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:       agentpod.TurnEventDone,
		StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	got, ok := hubEventByKind(hub, "done")
	if !ok {
		t.Fatalf("no done emit on hub")
	}
	if n, isNum := got["waiting_tasks"].(float64); !isNum || int(n) != 0 {
		t.Errorf("done.waiting_tasks = %#v, want 0", got["waiting_tasks"])
	}
}
