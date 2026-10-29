package claudeagent

import (
	"errors"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
)

// TestRunnerTurnEndsWhileAnotherGoroutineEmits covers a turn ended off
// the dispatcher goroutine (a Stop while a fold is absorbed, the absorb
// deadline, the subprocess exiting) while the dispatcher is still
// emitting into it. The emit gives up instead of sending on the closed
// events channel, and the caller sees every event that was delivered
// and then the close.
func TestRunnerTurnEndsWhileAnotherGoroutineEmits(t *testing.T) {
	clk := clock.NewFake()
	turn := newRunnerTurn(provider.CompleteRequest{}, nil, nil, "alice", "", clk)

	// Fill the buffer so the next emit waits: nobody is reading yet.
	buffered := cap(turn.events)
	for i := 0; i < buffered; i++ {
		turn.emit(provider.StreamEvent{Kind: provider.StreamDelta, Text: "early"})
	}

	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		turn.emit(provider.StreamEvent{Kind: provider.StreamDelta, Text: "late"})
	}()
	failed := make(chan struct{})
	go func() {
		defer close(failed)
		turn.fail(errors.New("subprocess exited"))
	}()

	// finalize arms its StreamEnd timer once it holds the send side;
	// let it give up on the full buffer so it closes the channel.
	clk.BlockUntil(1)
	clk.Advance(finalizeEmitTimeout)
	<-failed
	<-emitted

	got := 0
	for ev := range turn.Events() {
		if ev.Text != "early" {
			t.Fatalf("event %d = %+v, want only the buffered deltas", got, ev)
		}
		got++
	}
	if got != buffered {
		t.Errorf("drained %d events, want %d", got, buffered)
	}

	// A late emit on the ended turn is dropped, not a panic.
	turn.emit(provider.StreamEvent{Kind: provider.StreamDelta, Text: "after"})
}
