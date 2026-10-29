package agentpod

import (
	"context"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// runUntilCancelled is a subagent run that goes on until its context is
// cancelled, as a CLI process does until it is killed.
type runUntilCancelled struct {
	events chan provider.StreamEvent
	err    error
}

func newRunUntilCancelled(ctx context.Context) *runUntilCancelled {
	s := &runUntilCancelled{events: make(chan provider.StreamEvent)}
	go func() {
		<-ctx.Done()
		s.err = ctx.Err()
		close(s.events)
	}()
	return s
}

func (s *runUntilCancelled) Events() <-chan provider.StreamEvent { return s.events }
func (s *runUntilCancelled) Err() error                          { return s.err }
func (s *runUntilCancelled) Close() error                        { return nil }
func (s *runUntilCancelled) Final() *provider.CompleteResponse   { return nil }

// A restart of core loses track of a subagent run that goes on in the
// pod, and core then resumes the task in a new run of the same
// subagent: two processes on one CLI session and one run dir would
// corrupt both. This guards that the new run first ends the earlier
// one, which reports itself cancelled, and starts only once that run
// has posted its end, and that the new run then goes ahead.
func TestSubagentTurnEndsAnEarlierRunOfTheSameTask(t *testing.T) {
	fc := startFakeCore(t)
	started := make(chan string, 2)
	var oldEndedFirst bool
	mc := &provider.MockClient{
		RunSubagentFn: func(ctx context.Context, req provider.SubagentRequest) (provider.Stream, error) {
			started <- req.Prompt
			if req.Prompt == "first" {
				return newRunUntilCancelled(ctx), nil
			}
			posted := fc.postedFor("t-old")
			oldEndedFirst = len(posted) > 0 && posted[len(posted)-1].Kind == TurnEventFailed
			return provider.NewMockStream(
				[]provider.StreamEvent{{Kind: provider.StreamEnd}},
				&provider.CompleteResponse{StopReason: provider.StopEndTurn},
			), nil
		},
	}
	rt := subagentRuntime(t, fc, mc)

	oldDone := make(chan error, 1)
	go func() {
		oldDone <- rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-old", 1, SubagentSpec{SubagentID: "sub1", UserPrompt: "first"}))
	}()
	if got := <-started; got != "first" {
		t.Fatalf("first run started with %q", got)
	}
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-new", 1, SubagentSpec{SubagentID: "sub1", UserPrompt: "carry on", Resume: "sess-1"})); err != nil {
		t.Fatal(err)
	}
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if !oldEndedFirst {
		t.Error("the new run started before the earlier one had ended")
	}
	if old := lastPosted(t, fc, "t-old"); old.Kind != TurnEventFailed || old.FailedReason != FailedReasonCancelled {
		t.Errorf("earlier run ended %s/%s, want failed/cancelled", old.Kind, old.FailedReason)
	}
	if got := lastPosted(t, fc, "t-new"); got.Kind != TurnEventDone {
		t.Errorf("new run ended %s, want done", got.Kind)
	}
	rt.mu.Lock()
	left := len(rt.subagentRuns)
	rt.mu.Unlock()
	if left != 0 {
		t.Errorf("%d runs still tracked after both ended", left)
	}
}

// Runs of different subagents are independent: one must not end
// another. This guards that a run of one task leaves a run of another
// in flight.
func TestSubagentTurnLeavesOtherTasksRunning(t *testing.T) {
	fc := startFakeCore(t)
	started := make(chan string, 2)
	mc := &provider.MockClient{
		RunSubagentFn: func(ctx context.Context, req provider.SubagentRequest) (provider.Stream, error) {
			started <- req.Prompt
			if req.Prompt == "long" {
				return newRunUntilCancelled(ctx), nil
			}
			return provider.NewMockStream(
				[]provider.StreamEvent{{Kind: provider.StreamEnd}},
				&provider.CompleteResponse{StopReason: provider.StopEndTurn},
			), nil
		},
	}
	rt := subagentRuntime(t, fc, mc)
	ctx, cancel := context.WithCancel(context.Background())
	longDone := make(chan error, 1)
	go func() {
		longDone <- rt.handleSubagentTurn(ctx, subagentTurnEvent(t, "t-long", 1, SubagentSpec{SubagentID: "sub1", UserPrompt: "long"}))
	}()
	<-started
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-short", 1, SubagentSpec{SubagentID: "sub2", UserPrompt: "short"})); err != nil {
		t.Fatal(err)
	}
	if posted := fc.postedFor("t-long"); len(posted) != 0 {
		t.Errorf("the other task's run posted %v", posted)
	}
	cancel()
	if err := <-longDone; err != nil {
		t.Fatal(err)
	}
}
