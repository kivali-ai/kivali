package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSubagentStatusReportsRunningAndFinished covers what the status
// tool is for: telling an agent what it still has outstanding, and
// what landed while it was away.
func TestSubagentStatusReportsRunningAndFinished(t *testing.T) {
	release := make(chan struct{})
	driver := &fakeSubagentDriver{
		respond: func(req fakeDriveCall) (string, error) {
			// Only the second task blocks, so the test has one job in
			// each state at the same time.
			if strings.Contains(req.Spec.Description, "slow") {
				<-release
			}
			return "done", nil
		},
	}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)

	args, _ := json.Marshal(map[string]any{
		"tasks": []map[string]any{
			{"description": "quick lookup", "prompt": "a"},
			{"description": "slow build", "prompt": "b"},
		},
	})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}

	// Wait for the quick one, leaving the slow one running.
	del.waitFor(1)

	status := svc.SubagentStatus("alice")
	if !strings.Contains(status, "RUNNING:") || !strings.Contains(status, "slow build") {
		t.Errorf("status should list the still-running task:\n%s", status)
	}
	if !strings.Contains(status, "FINISHED:") || !strings.Contains(status, "quick lookup") {
		t.Errorf("status should list the finished task:\n%s", status)
	}
	if !strings.Contains(status, "1 background task running") {
		t.Errorf("status should count exactly one running:\n%s", status)
	}

	close(release)
	del.waitFor(2)
	if n := svc.OutstandingSubagents("alice"); n != 0 {
		t.Errorf("outstanding = %d, want 0", n)
	}
}

// TestSubagentStatusEmptyIsUnambiguous guards the wording an agent
// sees when it has dispatched nothing. "No background tasks" has to
// read as a fact, not as an error it should retry around.
func TestSubagentStatusEmptyIsUnambiguous(t *testing.T) {
	svc, _, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	got := svc.SubagentStatus("alice")
	if !strings.Contains(got, "no background tasks") {
		t.Errorf("empty status = %q", got)
	}
}

// TestSubagentCancelStopsJobAndDeliversNothing pins the rule that
// keeps Stop meaning stopped: a cancelled job reports nothing back.
//
// If it did deliver, the RoleReceived entry would look to the spawn
// gate like work owed a reply, and an agent the CEO had just stopped
// would wake straight back up to read about its own cancellation.
func TestSubagentCancelStopsJobAndDeliversNothing(t *testing.T) {
	started := make(chan struct{})
	driver := &fakeSubagentDriver{
		respond: func(_ fakeDriveCall) (string, error) {
			close(started)
			// Park until the job's context is cancelled. The fake has
			// no ctx of its own, so block forever and let the test
			// assert via the registry instead.
			select {}
		},
	}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)

	args, _ := json.Marshal(map[string]any{
		"tasks": []map[string]any{{"description": "long one", "prompt": "a"}},
	})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	<-started

	jobs := svc.listJobs("alice")
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	id := jobs[0].ID

	// Wrong owner must not be able to cancel by guessing the id.
	if svc.CancelSubagent("mallory", id) {
		t.Error("another agent cancelled alice's job")
	}
	// Unknown id is a miss, not a panic.
	if svc.CancelSubagent("alice", "deadbeef") {
		t.Error("cancelled a job that does not exist")
	}

	if !svc.CancelSubagent("alice", id) {
		t.Fatal("owner could not cancel their own running job")
	}
	if len(del.msgs) != 0 {
		t.Errorf("cancelled job delivered %d messages; want none", len(del.msgs))
	}

	// And the model-facing wording says so, so the agent does not sit
	// waiting for a result that is never coming.
	out := svc.SubagentCancel("alice", id)
	_ = out // second cancel is a miss; asserted below via a fresh id
	if msg := svc.SubagentCancel("alice", "nope"); !strings.Contains(msg, "No running background task") {
		t.Errorf("cancel miss wording = %q", msg)
	}
}

// TestOutstandingSubagentsIsPerParent guards the isolation the working
// indicator depends on: one agent's background work must not light up
// another agent's dot.
func TestOutstandingSubagentsIsPerParent(t *testing.T) {
	release := make(chan struct{})
	driver := &fakeSubagentDriver{
		respond: func(_ fakeDriveCall) (string, error) {
			<-release
			return "ok", nil
		},
	}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)

	args, _ := json.Marshal(map[string]any{
		"tasks": []map[string]any{{"description": "x", "prompt": "y"}},
	})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	if n := svc.OutstandingSubagents("alice"); n != 1 {
		t.Errorf("alice outstanding = %d, want 1", n)
	}
	if n := svc.OutstandingSubagents("bob"); n != 0 {
		t.Errorf("bob outstanding = %d, want 0 — alice's work is not bob's", n)
	}
	close(release)
	del.waitFor(1)
}

// TestSubagentServiceNilIsSafe: OutstandingSubagents is called from the
// agent-detail render path, which runs in deployments where the
// service was never configured. A nil receiver there would take down
// the page rather than showing an idle agent.
func TestSubagentServiceNilIsSafe(t *testing.T) {
	var svc *SubagentService
	if n := svc.OutstandingSubagents("alice"); n != 0 {
		t.Errorf("nil service outstanding = %d, want 0", n)
	}
	if got := svc.SubagentStatus("alice"); !strings.Contains(got, "not available") {
		t.Errorf("nil service status = %q", got)
	}
	if got := svc.SubagentCancel("alice", "x"); !strings.Contains(got, "not available") {
		t.Errorf("nil service cancel = %q", got)
	}
}
