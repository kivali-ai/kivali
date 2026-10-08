package web

import (
	"testing"
	"time"
)

// The panel listed every live task flat, newest first, labelling a
// nested one with its caller's raw hex id. Three separate problems:
// you could not tell which task spawned which, the order was the
// reverse of the order things were launched, and cancelling a task
// left the work underneath it running.

func job(id, caller string, queuedAtMin int, state string) subagentJob {
	depth := 1
	if caller != "" {
		depth = 2
	}
	return subagentJob{
		ID: id, Parent: "alice", CallerID: caller, Depth: depth,
		Description: id + " work", State: state,
		QueuedAt: time.Date(2026, 9, 16, 12, queuedAtMin, 0, 0, time.UTC),
	}
}

/* ---- cancelling a task cancels what it dispatched ---- */

// TestCancelSubagentCascadesToChildren: cancelling a task cancels its
// sub-tasks, running and queued.
func TestCancelSubagentCascadesToChildren(t *testing.T) {
	svc := &SubagentService{}
	cancelled := map[string]int{}
	reg := func(id, caller, state string) {
		j := job(id, caller, 1, state)
		j.cancel = func() { cancelled[id]++ }
		svc.registerJob(&j)
	}
	reg("lead", "", subagentJobRunning)
	reg("w1", "lead", subagentJobRunning)
	reg("w2", "lead", subagentJobQueued) // queued counts: it would start later
	reg("other", "", subagentJobRunning) // untouched

	if !svc.CancelSubagent("alice", "lead") {
		t.Fatal("CancelSubagent reported nothing to cancel")
	}

	for _, id := range []string{"lead", "w1", "w2"} {
		if cancelled[id] != 1 {
			t.Errorf("%s cancelled %d times, want 1 — work under a cancelled task must stop", id, cancelled[id])
		}
	}
	if cancelled["other"] != 0 {
		t.Errorf("an unrelated task was cancelled %d times", cancelled["other"])
	}
}

// TestCancelSubagentCascadesTransitively: depth is capped at 2 today,
// so this is one hop in practice. Asserting the walk is transitive
// means raising the cap does not quietly reopen the hole.
func TestCancelSubagentCascadesTransitively(t *testing.T) {
	svc := &SubagentService{}
	cancelled := map[string]int{}
	reg := func(id, caller string) {
		j := job(id, caller, 1, subagentJobRunning)
		j.cancel = func() { cancelled[id]++ }
		svc.registerJob(&j)
	}
	reg("a", "")
	reg("b", "a")
	reg("c", "b") // grandchild

	svc.CancelSubagent("alice", "a")

	for _, id := range []string{"a", "b", "c"} {
		if cancelled[id] != 1 {
			t.Errorf("%s cancelled %d times, want 1", id, cancelled[id])
		}
	}
}

// TestCancelSubagentSkipsFinishedChildren: a child that already
// finished has no goroutine to stop and may have delivered its result.
func TestCancelSubagentSkipsFinishedChildren(t *testing.T) {
	svc := &SubagentService{}
	cancelled := map[string]int{}
	reg := func(id, caller, state string) {
		j := job(id, caller, 1, state)
		j.cancel = func() { cancelled[id]++ }
		svc.registerJob(&j)
	}
	reg("lead", "", subagentJobRunning)
	reg("done", "lead", subagentJobCompleted)

	svc.CancelSubagent("alice", "lead")

	if cancelled["done"] != 0 {
		t.Errorf("a finished child was cancelled %d times", cancelled["done"])
	}
}

/* ---- a rotation forgets the finished set ---- */

// TestForgetFinishedDropsOnlyFinishedJobs: the finished history
// belongs to the chat that dispatched it, so a rotation drops it —
// and nothing else. Live jobs are owed to the new chat and must stay
// cancellable; a finished sub-lead whose worker is still going stays
// so the worker keeps the caller it is nested under; and another
// agent's finished jobs are not this agent's to forget.
func TestForgetFinishedDropsOnlyFinishedJobs(t *testing.T) {
	svc := &SubagentService{}
	reg := func(id, caller, state string) {
		j := job(id, caller, 1, state)
		svc.registerJob(&j)
	}
	reg("done", "", subagentJobCompleted)
	reg("failed", "", subagentJobFailed)
	reg("cancelled", "", subagentJobCancelled)
	reg("running", "", subagentJobRunning)
	reg("queued", "", subagentJobQueued)
	reg("lead-live", "", subagentJobCompleted)     // finished, but its worker is not
	reg("w-live", "lead-live", subagentJobRunning) // still going
	reg("lead-done", "", subagentJobCompleted)     // finished, and so is its worker
	reg("w-done", "lead-done", subagentJobCompleted)

	bob := job("bobs", "", 1, subagentJobCompleted)
	bob.Parent = "bob"
	svc.registerJob(&bob)

	if n := svc.ForgetFinished("alice"); n != 5 {
		t.Errorf("forgot %d jobs, want 5 (done, failed, cancelled, lead-done, w-done)", n)
	}

	left := map[string]bool{}
	for _, j := range svc.listJobs("alice") {
		left[j.ID] = true
	}
	for _, id := range []string{"running", "queued", "lead-live", "w-live"} {
		if !left[id] {
			t.Errorf("%s was forgotten; live work and the caller above it must survive a rotation", id)
		}
	}
	for _, id := range []string{"done", "failed", "cancelled", "lead-done", "w-done"} {
		if left[id] {
			t.Errorf("%s survived; a finished job with nothing live under it belongs to the archived chat", id)
		}
	}
	if got := svc.listJobs("bob"); len(got) != 1 {
		t.Errorf("bob's registry = %d jobs, want 1 — alice's rotation must not touch it", len(got))
	}
	// Idempotent, and nil-safe for fixtures without a service.
	if n := svc.ForgetFinished("alice"); n != 0 {
		t.Errorf("second pass forgot %d, want 0", n)
	}
	var nilSvc *SubagentService
	if n := nilSvc.ForgetFinished("alice"); n != 0 {
		t.Errorf("nil service forgot %d", n)
	}
}

// TestCancelSubagentDoesNotCrossAgents: the cascade follows CallerID,
// and ownership is rechecked at every node rather than only at the
// root.
func TestCancelSubagentDoesNotCrossAgents(t *testing.T) {
	svc := &SubagentService{}
	cancelled := map[string]int{}

	mine := job("mine", "", 1, subagentJobRunning)
	mine.cancel = func() { cancelled["mine"]++ }
	svc.registerJob(&mine)

	// Same CallerID, different owner — must not be followed.
	theirs := job("theirs", "mine", 2, subagentJobRunning)
	theirs.Parent = "bob"
	theirs.cancel = func() { cancelled["theirs"]++ }
	svc.registerJob(&theirs)

	svc.CancelSubagent("alice", "mine")

	if cancelled["mine"] != 1 {
		t.Errorf("own task cancelled %d times, want 1", cancelled["mine"])
	}
	if cancelled["theirs"] != 0 {
		t.Error("the cascade crossed an ownership boundary")
	}
}
