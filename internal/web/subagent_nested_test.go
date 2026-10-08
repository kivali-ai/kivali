package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// nestedTestService builds a fully configured service whose driver
// would happily run anything it was handed. Every case below still
// refuses before reaching it — which is the point: these gates must
// hold on their own, not because the pod happens to be unreachable.
func nestedTestService(t *testing.T) *SubagentService {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	driver := &fakeSubagentDriver{
		respond: func(req fakeDriveCall) (string, error) {
			t.Errorf("driver ran %q: this batch should have been refused before dispatch", req.SubagentID)
			return "", nil
		},
	}
	return &SubagentService{Store: st, Driver: driver, Provider: provider.MockProvider{}}
}

// registerTestJob puts a job in the registry directly, standing in for
// one the service dispatched earlier.
func registerTestJob(s *SubagentService, id, parent string, depth int, state string) {
	s.registerJob(&subagentJob{
		ID:       id,
		Parent:   parent,
		Depth:    depth,
		State:    state,
		QueuedAt: time.Now().UTC(),
		done:     make(chan struct{}),
	})
}

func nestedArgs(n int) json.RawMessage {
	tasks := make([]map[string]string, n)
	for i := range tasks {
		tasks[i] = map[string]string{"description": "task", "prompt": "do it"}
	}
	b, _ := json.Marshal(map[string]any{"tasks": tasks})
	return b
}

// TestNestedBatchRejectsUnknownCaller covers the case where a request
// names a job that does not exist. The caller id is a claim, and this
// is what makes the claim harmless.
func TestNestedBatchRejectsUnknownCaller(t *testing.T) {
	s := nestedTestService(t)
	_, err := s.RunNestedBatch(context.Background(), "alice", "nope", nestedArgs(1))
	if err == nil || !strings.Contains(err.Error(), "unknown caller") {
		t.Fatalf("err = %v, want an unknown-caller refusal", err)
	}
}

// TestNestedBatchRejectsForeignCaller proves one agent cannot dispatch
// under another's slug by naming a job it does not own. The job exists
// — it just belongs to someone else.
func TestNestedBatchRejectsForeignCaller(t *testing.T) {
	s := nestedTestService(t)
	registerTestJob(s, "job1", "bob", 1, subagentJobRunning)

	_, err := s.RunNestedBatch(context.Background(), "alice", "job1", nestedArgs(1))
	if err == nil || !strings.Contains(err.Error(), "unknown caller") {
		t.Fatalf("err = %v, want alice refused bob's job", err)
	}
}

// TestNestedBatchRejectsCancelledCaller stops a sub-lead that was
// cancelled mid-dispatch from starting work nobody is waiting for —
// work that would take pod slots from jobs someone IS waiting for.
func TestNestedBatchRejectsCancelledCaller(t *testing.T) {
	s := nestedTestService(t)
	registerTestJob(s, "job1", "alice", 1, subagentJobCancelled)

	_, err := s.RunNestedBatch(context.Background(), "alice", "job1", nestedArgs(1))
	if err == nil || !strings.Contains(err.Error(), "no longer running") {
		t.Fatalf("err = %v, want a refusal for a cancelled caller", err)
	}
}

// TestNestedBatchEnforcesDepthCap is the server-side half of the depth
// rule. The toolkit already withholds the tool at the last tier, so
// reaching here means the two disagree — and when they disagree the
// authoritative side must refuse rather than defer.
func TestNestedBatchEnforcesDepthCap(t *testing.T) {
	s := nestedTestService(t)
	registerTestJob(s, "deep", "alice", agentpod.MaxSubagentDepth, subagentJobRunning)

	_, err := s.RunNestedBatch(context.Background(), "alice", "deep", nestedArgs(1))
	if err == nil || !strings.Contains(err.Error(), "last tier") {
		t.Fatalf("err = %v, want a depth-cap refusal", err)
	}
}

// TestNestedBatchEnforcesFanoutCap keeps a sub-lead's batch below the
// durable agent's, because each of its workers holds a pod slot while
// their lead is also holding one.
func TestNestedBatchEnforcesFanoutCap(t *testing.T) {
	s := nestedTestService(t)
	registerTestJob(s, "job1", "alice", 1, subagentJobRunning)

	_, err := s.RunNestedBatch(context.Background(), "alice", "job1", nestedArgs(agentpod.SubagentMaxNestedFanout+1))
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("err = %v, want a fan-out refusal", err)
	}
}

// TestNestedResultsRenderFailuresAlongsideAnswers checks the shape a
// sub-lead actually reads. A batch where one worker failed must still
// hand back the answers that succeeded AND name the failure: collapsing
// the whole call into an error throws away good work, and omitting the
// failure invites a confident answer with a silent hole in it.
func TestNestedResultsRenderFailuresAlongsideAnswers(t *testing.T) {
	out := renderNestedSubagentResults([]subagentJob{
		{ID: "a1", Description: "check the invoices", State: subagentJobCompleted, FinalText: "Seven overdue."},
		{ID: "b2", Description: "check the contracts", State: subagentJobFailed, Err: "transcript ended early"},
	})

	if !strings.Contains(out, "Seven overdue.") {
		t.Error("successful worker's answer is missing")
	}
	if !strings.Contains(out, "transcript ended early") {
		t.Error("failed worker's error is missing")
	}
	if !strings.Contains(out, "1 succeeded, 1 did not") {
		t.Errorf("header does not state the tally: %q", nestedFirstLine(out))
	}
	if !strings.Contains(out, "Do not silently drop it.") {
		t.Error("no instruction about what to do with the missing piece")
	}
	if !strings.Contains(out, "not your answer") {
		t.Error("results are not framed as raw material")
	}
}

func nestedFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
