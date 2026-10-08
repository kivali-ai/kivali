package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Clock is the supervisor's time source; tests substitute one that
// never sleeps.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// permanentError ends a poll at once.
type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// permanent marks err as ending a poll.
func permanent(err error) error { return permanentError{err} }

// errNotYet is returned by a poll function that has nothing to report
// but is not done.
var errNotYet = errors.New("not yet")

// poll calls fn every interval until it returns nil, a permanent error,
// ctx ends or timeout passes. A non-permanent error is remembered and
// reported if the wait times out.
func poll(ctx context.Context, clk Clock, timeout, interval time.Duration, what string, fn func(ctx context.Context) error) error {
	deadline := clk.Now().Add(timeout)
	var last error
	for {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		var p permanentError
		if errors.As(err, &p) {
			return p.err
		}
		if !errors.Is(err, errNotYet) {
			last = err
		}
		if !clk.Now().Before(deadline) {
			if last != nil {
				return fmt.Errorf("timed out after %s waiting for %s (last: %v)", timeout, what, last)
			}
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clk.After(interval):
		}
	}
}
