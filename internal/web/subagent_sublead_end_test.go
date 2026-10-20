package web

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

// A sub-lead whose run ends on its own, by an error, a crash or even an
// answer, leaves no dispatch waiting on its workers. Without this they
// ran on, holding pod slots and spending tokens, and their answers were
// dropped. This guards that finishJob cancels a sub-lead's unfinished
// workers however it ended, and touches nobody else's work.
func TestASubLeadThatEndsCancelsItsWorkers(t *testing.T) {
	for name, runErr := range map[string]error{"failed": errors.New("the agent pod went away"), "completed": nil} {
		t.Run(name, func(t *testing.T) {
			svc, _, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
			workerCtx, workerCancel := context.WithCancel(context.Background())
			doneCtx, doneCancel := context.WithCancel(context.Background())
			defer doneCancel()
			otherCtx, otherCancel := context.WithCancel(context.Background())
			defer otherCancel()
			lead := &subagentJob{ID: "lead", Parent: "alice", Depth: 1, State: subagentJobRunning, done: make(chan struct{})}
			svc.registerJob(lead)
			svc.registerJob(&subagentJob{ID: "w1", Parent: "alice", Depth: 2, CallerID: "lead", State: subagentJobRunning, cancel: workerCancel, done: make(chan struct{})})
			// A worker that already finished is left as it ended.
			svc.registerJob(&subagentJob{ID: "w2", Parent: "alice", Depth: 2, CallerID: "lead", State: subagentJobCompleted, cancel: doneCancel, done: make(chan struct{})})
			svc.registerJob(&subagentJob{ID: "other", Parent: "alice", Depth: 1, State: subagentJobRunning, cancel: otherCancel, done: make(chan struct{})})

			svc.finishJob(lead, "", 0, "answer", "", runErr)

			awaitCancel(t, "the running worker", workerCtx)
			if w, _ := svc.lookupJob("w1"); !w.cancelled {
				t.Error("the worker is not marked cancelled, so its end would read as a failure")
			}
			if w, _ := svc.lookupJob("w2"); w.cancelled || doneCtx.Err() != nil {
				t.Error("a finished worker was cancelled")
			}
			if o, _ := svc.lookupJob("other"); o.cancelled || otherCtx.Err() != nil {
				t.Error("an unrelated task was cancelled")
			}
		})
	}
}

// blockingDriver runs a task until its context is cancelled, and says
// when each has started.
type blockingDriver struct{ started chan string }

func (d *blockingDriver) DriveSubagent(ctx context.Context, _, _, subagentID string, _ agentpod.SubagentSpec) (string, error) {
	d.started <- subagentID
	<-ctx.Done()
	return "", ctx.Err()
}

// A sub-lead's dispatch call can end while its workers still run: its
// process died, or it stopped waiting. Nothing then reads their
// answers. This guards that RunNestedBatch cancels the workers it was
// waiting on when its call ends, instead of leaving them to run to the
// end and be dropped.
func TestANestedDispatchThatStopsWaitingCancelsItsWorkers(t *testing.T) {
	driver := &blockingDriver{started: make(chan string, 1)}
	svc, _, _, _ := newSubagentTestService(t, driver)
	registerTestJob(svc, "lead0001", "alice", 1, subagentJobRunning)
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := svc.RunNestedBatch(ctx, "alice", "lead0001", nestedArgs(1))
		returned <- err
	}()
	var worker string
	select {
	case worker = <-driver.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never started")
	}
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Errorf("RunNestedBatch = %v, want context.Canceled", err)
	}
	j, _ := svc.lookupJob(worker)
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker ran on after its caller stopped waiting")
	}
	if j, _ := svc.lookupJob(worker); j.State != subagentJobCancelled || !j.cancelled {
		t.Errorf("worker ended %s (cancelled %v), want cancelled", j.State, j.cancelled)
	}
}
