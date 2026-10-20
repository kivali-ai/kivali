package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A dispatch that reads large upload files can outlast the control
// client's 10-second timeout. The agent then reads a failure and
// dispatches again, so a dispatch that went on to start its tasks
// anyway ran them twice and put two upload approvals in front of a
// person. This guards that a dispatch whose caller has gone starts
// nothing: no job, no approval, nothing on the chip, nothing charged.
func TestADispatchWhoseCallerHasGoneStartsNothing(t *testing.T) {
	svc, parentRoot, _, _ := webService(t, &fakeSubagentDriver{})
	endTasksAtCleanup(t, svc)
	if err := os.WriteFile(filepath.Join(parentRoot, "artifacts", "private", "quote.pdf"), []byte("pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &hubRecorder{}
	svc.EmitToParent = rec.emit
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for name, args := range map[string][]byte{
		"an upload task": uploadArgs(t),
		"a plain task":   nestedArgs(1),
	} {
		if _, err := svc.StartBatch(gone, "alice", args); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: StartBatch = %v, want context.Canceled", name, err)
		}
	}
	if jobs := svc.listJobs("alice"); len(jobs) != 0 {
		t.Errorf("an abandoned dispatch registered %d jobs", len(jobs))
	}
	if slices.Contains(rec.kinds(), "subagent_started") {
		t.Error("an abandoned dispatch announced its tasks")
	}
	if spent := spentToday(svc); spent != 0 {
		t.Errorf("an abandoned dispatch charged %d to the web budget", spent)
	}

	// The same dispatch from a caller still waiting starts as before.
	if _, err := svc.StartBatch(context.Background(), "alice", uploadArgs(t)); err != nil {
		t.Fatal(err)
	}
	if jobs := svc.listJobs("alice"); len(jobs) != 1 || jobs[0].State != subagentJobAwaitingApproval {
		t.Errorf("jobs after a live dispatch = %+v, want one awaiting approval", jobs)
	}
}
