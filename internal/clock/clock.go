// Package clock is the one time source for Kivali.
//
// Production code takes a Clock instead of calling time.Now,
// time.NewTimer, or time.NewTicker directly. Tests inject a Fake and
// drive time forward by hand.
//
// Why this exists rather than three near-identical private clocks
// (which is what the codebase had): a test that waits for real
// seconds to pass is slow when it passes and flaky when the machine
// is loaded, and the failure looks like a bug in the code under test
// rather than a bug in the test. A shared, injectable clock makes
// "advance twenty seconds" exact and instant.
//
// The interface is deliberately small — Now, a timer, a ticker. There
// is no Sleep: blocking a goroutine on a duration is not something
// production code here should do, and offering it would invite it.
// Use a timer and a select so cancellation still works.
//
// Everything here is safe for concurrent use.
package clock

import "time"

// Clock is the seam. System is production; Fake is tests.
type Clock interface {
	// Now is the current time.
	Now() time.Time

	// NewTimer returns a Timer that fires once after d.
	NewTimer(d time.Duration) Timer

	// NewTicker returns a Ticker that fires every d until stopped.
	// Panics if d <= 0, matching time.NewTicker.
	NewTicker(d time.Duration) Ticker
}

// Timer fires once. Mirrors *time.Timer, minus the exported struct
// field — C is a method so a Fake can implement it.
type Timer interface {
	// C is the fire channel. Buffered by one, so a timer that fires
	// while nobody is receiving does not block the clock.
	C() <-chan time.Time

	// Stop prevents the timer from firing, reporting whether it was
	// still pending. Safe to call more than once.
	Stop() bool

	// Reset restarts the timer for a new duration, reporting whether
	// it was still pending.
	Reset(d time.Duration) bool
}

// Ticker fires repeatedly.
type Ticker interface {
	// C is the fire channel, buffered by one. A tick delivered while
	// the previous one is unread is DROPPED, matching time.Ticker: a
	// slow receiver falls behind rather than building a backlog it can
	// never work through.
	C() <-chan time.Time

	// Stop halts the ticker. Safe to call more than once. Does not
	// close the channel, matching time.Ticker.
	Stop()
}

// Since is Now().Sub(t) against a Clock. A helper rather than an
// interface method — every implementation would write the same line.
func Since(c Clock, t time.Time) time.Duration {
	return c.Now().Sub(t)
}

// Until is t.Sub(Now()) against a Clock.
func Until(c Clock, t time.Time) time.Duration {
	return t.Sub(c.Now())
}

// ─── production ─────────────────────────────────────────────────────

// System is the real clock.
type System struct{}

// New returns the production clock. Prefer this at composition roots
// so a nil Clock never has to be handled deeper in.
func New() Clock { return System{} }

// Now returns the current wall time.
func (System) Now() time.Time { return time.Now() }

// NewTimer wraps time.NewTimer.
func (System) NewTimer(d time.Duration) Timer { return &systemTimer{t: time.NewTimer(d)} }

// NewTicker wraps time.NewTicker.
func (System) NewTicker(d time.Duration) Ticker { return &systemTicker{t: time.NewTicker(d)} }

type systemTimer struct{ t *time.Timer }

func (s *systemTimer) C() <-chan time.Time        { return s.t.C }
func (s *systemTimer) Stop() bool                 { return s.t.Stop() }
func (s *systemTimer) Reset(d time.Duration) bool { return s.t.Reset(d) }

type systemTicker struct{ t *time.Ticker }

func (s *systemTicker) C() <-chan time.Time { return s.t.C }
func (s *systemTicker) Stop()               { s.t.Stop() }

// Compile-time checks.
var (
	_ Clock  = System{}
	_ Timer  = (*systemTimer)(nil)
	_ Ticker = (*systemTicker)(nil)
)
