package broker

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

var (
	errPipeMissing = errors.New("the pipe does not exist")
	errPipeOther   = errors.New("access is denied")
)

func isPipeMissing(err error) bool { return errors.Is(err, errPipeMissing) }

// dialResult is what one dialWhenListening call answered.
type dialResult struct {
	conn net.Conn
	err  error
}

// startDial runs dialWhenListening against a dial that fails with each
// of fails in turn and then connects, and counts the tries.
func startDial(ctx context.Context, clk clock.Clock, wait time.Duration, fails ...error) (<-chan dialResult, *atomic.Int32) {
	tries := &atomic.Int32{}
	dial := func(context.Context) (net.Conn, error) {
		n := int(tries.Add(1))
		if n <= len(fails) {
			return nil, fails[n-1]
		}
		c, _ := net.Pipe()
		return c, nil
	}
	out := make(chan dialResult, 1)
	go func() {
		c, err := dialWhenListening(ctx, clk, wait, 250*time.Millisecond, dial, isPipeMissing)
		out <- dialResult{c, err}
	}()
	return out, tries
}

// TestDialWaitsForAPipeStillBeingCreated guards a team resumed at
// Windows sign-in: Kivali can start before Windows has started the
// broker service, and a dial that gave up on the first missing pipe
// made the resume fail with "couldn't start" until the person pressed
// Try again. The dial looks again until the pipe appears.
func TestDialWaitsForAPipeStillBeingCreated(t *testing.T) {
	clk := clock.NewFake()
	out, tries := startDial(context.Background(), clk, 5*time.Second, errPipeMissing, errPipeMissing)
	for range 2 {
		clk.BlockUntil(1)
		clk.Advance(250 * time.Millisecond)
	}
	r := <-out
	if r.err != nil || r.conn == nil {
		t.Fatalf("dial = %v, %v; want a connection once the pipe exists", r.conn, r.err)
	}
	_ = r.conn.Close()
	if n := tries.Load(); n != 3 {
		t.Errorf("dialled %d times, want 3", n)
	}
}

// TestDialGivesUpOnAPipeThatNeverAppears guards the case of a broker
// that is not installed or not running: the wait is bounded, so the
// person still hears that the broker is missing, a few seconds later,
// with the dial's own error.
func TestDialGivesUpOnAPipeThatNeverAppears(t *testing.T) {
	clk := clock.NewFake()
	fails := make([]error, 100)
	for i := range fails {
		fails[i] = errPipeMissing
	}
	out, tries := startDial(context.Background(), clk, time.Second, fails...)
	// Tries at 0, 250, 500 and 750 ms wait; the one at 1 s is the last.
	for range 4 {
		clk.BlockUntil(1)
		clk.Advance(250 * time.Millisecond)
	}
	r := <-out
	if !errors.Is(r.err, errPipeMissing) {
		t.Fatalf("dial error = %v, want the missing pipe's", r.err)
	}
	if n := tries.Load(); n != 5 {
		t.Errorf("dialled %d times, want 5", n)
	}
}

// TestDialAnswersOtherFailuresAtOnce guards the refusals that waiting
// cannot fix (a pipe the caller may not open, for one): only a missing
// pipe is looked for again, so those still reach the person at once.
func TestDialAnswersOtherFailuresAtOnce(t *testing.T) {
	clk := clock.NewFake()
	out, tries := startDial(context.Background(), clk, 5*time.Second, errPipeOther)
	r := <-out
	if !errors.Is(r.err, errPipeOther) {
		t.Fatalf("dial error = %v, want %v", r.err, errPipeOther)
	}
	if n := tries.Load(); n != 1 {
		t.Errorf("dialled %d times, want 1", n)
	}
	if n := clk.Waiters(); n != 0 {
		t.Errorf("%d timer(s) left armed, want none", n)
	}
}

// TestDialStopsWaitingWhenTheCallerGivesUp guards a caller whose own
// deadline is shorter than the wait (a request cancelled by the app or
// a quit): the dial returns when the caller's context ends, rather than
// holding the caller for the rest of the wait.
func TestDialStopsWaitingWhenTheCallerGivesUp(t *testing.T) {
	clk := clock.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	out, _ := startDial(ctx, clk, 5*time.Second, errPipeMissing, errPipeMissing)
	clk.BlockUntil(1)
	cancel()
	r := <-out
	if !errors.Is(r.err, errPipeMissing) {
		t.Fatalf("dial error = %v, want the missing pipe's", r.err)
	}
	if n := clk.Waiters(); n != 0 {
		t.Errorf("%d timer(s) left armed, want none", n)
	}
}
