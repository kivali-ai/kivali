package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// A finished background task is delivered to the parent's chat.jsonl
// and must wake the parent. deliverToAgent wakes a MID-TURN parent (it
// stages the message and asks the turn to wind up; the finalize
// flushes and spawns a follow-up), but an IDLE one only gets the row
// appended — and idle is the normal case here, because dispatching the
// batch is what ended the parent's turn. So finishJob wakes it.

// wakeRecorder captures deliveries and wake calls in order, so a test
// can assert not just that both happened but that the wake came after
// the write it is supposed to make readable.
type wakeRecorder struct {
	events    []string
	delivered []store.ChatMessage
	deliverer func(parent string, msg store.ChatMessage) error
}

func (w *wakeRecorder) deliver(parent string, msg store.ChatMessage) error {
	if w.deliverer != nil {
		if err := w.deliverer(parent, msg); err != nil {
			w.events = append(w.events, "deliver-failed:"+parent)
			return err
		}
	}
	w.delivered = append(w.delivered, msg)
	w.events = append(w.events, "deliver:"+parent)
	return nil
}

func (w *wakeRecorder) wake(parent string) {
	w.events = append(w.events, "wake:"+parent)
}

func newWakeTestService(rec *wakeRecorder) *SubagentService {
	return &SubagentService{
		Provider:        provider.MockProvider{},
		EmitToParent:    func(string, string, any) {},
		DeliverToParent: rec.deliver,
		WakeParent:      rec.wake,
	}
}

func finishOneJob(t *testing.T, svc *SubagentService, parent string, runErr error) {
	t.Helper()
	job := &subagentJob{
		ID: "job-1", Parent: parent, Description: "background work",
		State: subagentJobRunning, Depth: 1, done: make(chan struct{}),
	}
	svc.registerJob(job)
	svc.finishJob(job, "tool-1", 0, "all done", runErr)
}

// TestFinishedSubagentWakesTheParent: an idle parent is woken by its
// task's result.
func TestFinishedSubagentWakesTheParent(t *testing.T) {
	rec := &wakeRecorder{}
	finishOneJob(t, newWakeTestService(rec), "alice", nil)

	if len(rec.delivered) != 1 {
		t.Fatalf("delivered %d results, want 1", len(rec.delivered))
	}
	want := []string{"deliver:alice", "wake:alice"}
	if strings.Join(rec.events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v — the result must be written BEFORE the wake, "+
			"or the woken loop reads history that does not have it yet", rec.events, want)
	}
}

// TestSubagentResultIsDeliveredAsReceivedWork pins what makes the wake
// meaningful: the spawn gate reads an unanswered RoleReceived entry as
// work owed a reply, and skips tool plumbing. A result delivered under
// the wrong role/kind would be written, woken for, and then ignored.
func TestSubagentResultIsDeliveredAsReceivedWork(t *testing.T) {
	rec := &wakeRecorder{}
	finishOneJob(t, newWakeTestService(rec), "alice", nil)

	got := rec.delivered[0]
	if got.Role != store.RoleReceived {
		t.Errorf("role = %q, want %q", got.Role, store.RoleReceived)
	}
	if got.Kind != store.KindSubagentResult {
		t.Errorf("kind = %q, want %q", got.Kind, store.KindSubagentResult)
	}
	// The gate ignores tool plumbing and turn-boundary markers; this
	// kind must be neither, or waking achieves nothing.
	hist := []store.ChatMessage{got}
	if store.LastRealReceivedTS(hist).IsZero() {
		t.Error("subagent_result does not count as a real received entry — " +
			"the spawn gate would refuse the turn the wake just asked for")
	}
	if v := store.SpawnDecision(hist); v != store.SpawnNow {
		t.Errorf("SpawnDecision = %v, want SpawnNow", v)
	}
}

// TestNoWakeWhenDeliveryFails: if the write failed there is nothing
// new on disk, so waking would spin up a turn over unchanged history.
func TestNoWakeWhenDeliveryFails(t *testing.T) {
	rec := &wakeRecorder{deliverer: func(string, store.ChatMessage) error {
		return fmt.Errorf("disk full")
	}}
	finishOneJob(t, newWakeTestService(rec), "alice", nil)

	for _, e := range rec.events {
		if strings.HasPrefix(e, "wake:") {
			t.Errorf("woke the parent after a failed delivery: %v", rec.events)
		}
	}
}

/* ---- cancelled jobs must stay silent, by BOTH routes ---- */

// TestCancelledSubagentNeitherDeliversNorWakes covers the two ways a
// cancellation reaches finishJob: context.Canceled, and the pod's own
// FailedReasonCancelled arriving as errSubagentCancelled. Both must be
// classified "cancelled" and hit the deliver-nothing guard; a "failed"
// delivery would wake the agent Stop just stopped.
func TestCancelledSubagentNeitherDeliversNorWakes(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "local context cancel (subagent_cancel, Stop)", err: context.Canceled},
		{name: "pod-reported cancel (FailedReasonCancelled)", err: errSubagentCancelled},
		{name: "pod-reported cancel, wrapped in transit", err: fmt.Errorf("drive: %w", errSubagentCancelled)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &wakeRecorder{}
			finishOneJob(t, newWakeTestService(rec), "alice", tc.err)

			if len(rec.delivered) != 0 {
				t.Errorf("a cancelled job reported back: %q", rec.delivered[0].Content)
			}
			if len(rec.events) != 0 {
				t.Errorf("events = %v, want none — cancelled work is silent", rec.events)
			}
		})
	}
}

// TestErrSubagentCancelledIsACancellation states the property directly,
// so the classification cannot regress without this failing. Any
// errors.Is(err, context.Canceled) in the pipeline must agree.
func TestErrSubagentCancelledIsACancellation(t *testing.T) {
	if !errors.Is(errSubagentCancelled, context.Canceled) {
		t.Error("errSubagentCancelled does not unwrap to context.Canceled; " +
			"a pod-reported cancel will be classified as a failure again")
	}
	// The message stays subagent-specific — it is what the parent sees
	// in a tool_result body when a cancel surfaces there.
	if got := errSubagentCancelled.Error(); got != "subagent: cancelled" {
		t.Errorf("message = %q, want %q", got, "subagent: cancelled")
	}
}

// TestFailedSubagentStillReportsAndWakes: a genuine failure (the pod
// died, the task errored) is news the parent needs. Only CANCELLED is
// silent.
func TestFailedSubagentStillReportsAndWakes(t *testing.T) {
	rec := &wakeRecorder{}
	finishOneJob(t, newWakeTestService(rec), "alice", fmt.Errorf("subagent failed: pod presumed dead"))

	if len(rec.delivered) != 1 {
		t.Fatalf("delivered %d, want 1 — a failure is not a cancellation", len(rec.delivered))
	}
	if !strings.Contains(rec.delivered[0].Content, "FAILED") {
		t.Errorf("failure not reported as such: %q", rec.delivered[0].Content)
	}
	want := []string{"deliver:alice", "wake:alice"}
	if strings.Join(rec.events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", rec.events, want)
	}
}

// TestNestedSubagentDoesNotWakeTheDurableAgent: a depth>1 job's answer
// goes back to the sub-lead that dispatched it, as its blocking tool
// call's return value. Delivering it upward would hand the durable
// agent intermediate worker output AND wake it for each one.
func TestNestedSubagentDoesNotWakeTheDurableAgent(t *testing.T) {
	rec := &wakeRecorder{}
	svc := newWakeTestService(rec)
	job := &subagentJob{
		ID: "nested-1", Parent: "alice", Description: "worker",
		State: subagentJobRunning, Depth: 2, done: make(chan struct{}),
	}
	svc.registerJob(job)
	svc.finishJob(job, "tool-1", 0, "done", nil)

	if len(rec.events) != 0 {
		t.Errorf("events = %v, want none for a depth-2 job", rec.events)
	}
}

// TestWakeParentUnwiredIsSafe: the hook is nil in fixtures and in any
// deployment that builds the service directly.
func TestWakeParentUnwiredIsSafe(t *testing.T) {
	svc := &SubagentService{
		Provider:        provider.MockProvider{},
		EmitToParent:    func(string, string, any) {},
		DeliverToParent: func(string, store.ChatMessage) error { return nil },
	}
	finishOneJob(t, svc, "alice", nil) // must not panic
}
