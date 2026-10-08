package clock

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// blockUntilBound is a safety valve on BlockUntil.
//
// It is the one place this package touches real time, and it is not a
// timing dependency: nothing waits it out in a passing test. It exists
// so that "the code never armed the timer I expected" fails with a
// stack trace naming the test instead of hanging until the whole
// package times out with no indication of which test was stuck.
const blockUntilBound = 10 * time.Second

// Fake is a clock that only moves when a test moves it.
//
// Typical shape:
//
//	clk := clock.NewFake()
//	go thingUnderTest(clk)      // arms a timer somewhere in there
//	clk.BlockUntil(1)           // wait until it actually has
//	clk.Advance(30*time.Second) // fire it
//
// BlockUntil is the part people skip and then chase a flake over: if
// the test advances before the goroutine has armed its timer, the
// timer is created already in the past-but-unfired state and nothing
// happens. Advancing is instant, so there is no wall-clock cost to
// synchronising properly.
//
// Safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []*fakeWaiter
}

// fakeWaiter is one armed timer or ticker.
type fakeWaiter struct {
	ch     chan time.Time
	at     time.Time     // next fire
	period time.Duration // 0 for a one-shot timer
	fired  bool          // one-shots only
	live   bool
}

// NewFake returns a Fake at a fixed, arbitrary instant.
//
// The instant is deliberately not time.Now(): a test whose behaviour
// depends on the real date is a test that fails on some future
// Tuesday. Use NewFakeAt when a specific date actually matters (a
// date-bucketing test, say).
func NewFake() *Fake {
	return NewFakeAt(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
}

// NewFakeAt returns a Fake set to t.
func NewFakeAt(t time.Time) *Fake {
	f := &Fake{now: t}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now returns the fake's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer arms a one-shot.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{ch: make(chan time.Time, 1), at: f.now.Add(d), live: true}
	f.waiters = append(f.waiters, w)
	f.cond.Broadcast()
	return &fakeTimer{f: f, w: w}
}

// NewTicker arms a repeating waiter.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: NewTicker requires a positive duration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{ch: make(chan time.Time, 1), at: f.now.Add(d), period: d, live: true}
	f.waiters = append(f.waiters, w)
	f.cond.Broadcast()
	return &fakeTicker{f: f, w: w}
}

// Advance moves time forward and fires everything that comes due, in
// chronological order.
//
// Ordering matters: a test that advances past two timers should see
// them fire in the order real time would have fired them, not in
// registration order. A ticker crossed several times fires once per
// period, and — like a real ticker — drops ticks its receiver was too
// slow to take.
//
// Firing is a non-blocking send, so a waiter nobody is listening to
// can never wedge the clock.
func (f *Fake) Advance(d time.Duration) {
	if d < 0 {
		panic("clock: Advance requires a non-negative duration")
	}
	f.mu.Lock()
	target := f.now.Add(d)

	for {
		next := f.nextDueLocked(target)
		if next == nil {
			break
		}
		// Move the clock TO the fire instant before delivering, so
		// anything the receiver reads from Now() sees the time the
		// event actually happened rather than the end of the advance.
		f.now = next.at
		select {
		case next.ch <- next.at:
		default: // receiver is behind; drop, as a real ticker does
		}
		if next.period > 0 {
			next.at = next.at.Add(next.period)
		} else {
			next.fired = true
			next.live = false
		}
	}

	f.now = target
	f.cond.Broadcast()
	f.mu.Unlock()
}

// nextDueLocked returns the earliest live waiter due at or before
// target, or nil.
func (f *Fake) nextDueLocked(target time.Time) *fakeWaiter {
	var best *fakeWaiter
	for _, w := range f.waiters {
		if !w.live || w.at.After(target) {
			continue
		}
		if best == nil || w.at.Before(best.at) {
			best = w
		}
	}
	return best
}

// Set jumps the clock to t. Forward jumps fire due waiters exactly as
// Advance does; a backward jump moves Now without firing anything,
// which is the honest model of an NTP correction.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	behind := t.Before(f.now)
	if behind {
		f.now = t
		f.cond.Broadcast()
		f.mu.Unlock()
		return
	}
	d := t.Sub(f.now)
	f.mu.Unlock()
	f.Advance(d)
}

// BlockUntil waits until n timers or tickers are armed.
//
// Use it before Advance whenever the thing being timed is armed on
// another goroutine. Panics rather than hanging if that never happens
// — see blockUntilBound.
func (f *Fake) BlockUntil(n int) {
	deadline := time.Now().Add(blockUntilBound)

	// sync.Cond has no timed wait, so a watchdog broadcasts to wake
	// the waiter and let it re-check the wall-clock bound.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				f.mu.Lock()
				f.cond.Broadcast()
				f.mu.Unlock()
			}
		}
	}()

	f.mu.Lock()
	defer f.mu.Unlock()
	for f.liveCountLocked() < n {
		if time.Now().After(deadline) {
			panic(fmt.Sprintf(
				"clock: BlockUntil(%d) gave up after %s with %d waiter(s) armed — "+
					"the code under test never armed the timer this test is waiting for",
				n, blockUntilBound, f.liveCountLocked()))
		}
		f.cond.Wait()
	}
}

// BlockUntilContext is BlockUntil with an escape hatch: it returns
// ctx.Err() instead of panicking if the waiters never appear.
//
// Use it from a helper goroutine, where BlockUntil's panic would take
// down the whole test binary rather than failing one test — for
// instance a goroutine that stands ready to fire a shutdown grace
// timer that some tests never arm.
func (f *Fake) BlockUntilContext(ctx context.Context, n int) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				f.mu.Lock()
				f.cond.Broadcast()
				f.mu.Unlock()
				return
			case <-t.C:
				f.mu.Lock()
				f.cond.Broadcast()
				f.mu.Unlock()
			}
		}
	}()

	f.mu.Lock()
	defer f.mu.Unlock()
	for f.liveCountLocked() < n {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.cond.Wait()
	}
	return nil
}

// Waiters reports how many timers and tickers are currently armed.
// Occasionally useful for asserting that something was cleaned up.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveCountLocked()
}

func (f *Fake) liveCountLocked() int {
	n := 0
	for _, w := range f.waiters {
		if w.live {
			n++
		}
	}
	return n
}

// forget drops a waiter so a long-lived Fake does not accumulate
// stopped timers.
func (f *Fake) forget(w *fakeWaiter) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, cur := range f.waiters {
		if cur == w {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			break
		}
	}
	f.cond.Broadcast()
}

// Deadlines reports the armed fire times, in order. Waiters alone
// can't tell "still armed from before" from "re-armed for a new
// deadline", which is exactly what a test of trailing-edge debouncing
// needs to see before it advances the clock.
func (f *Fake) Deadlines() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Time, 0, len(f.waiters))
	for _, w := range f.waiters {
		if w.live {
			out = append(out, w.at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

type fakeTimer struct {
	f *Fake
	w *fakeWaiter
}

func (t *fakeTimer) C() <-chan time.Time { return t.w.ch }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	was := t.w.live
	t.w.live = false
	t.f.mu.Unlock()
	t.f.forget(t.w)
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.f.mu.Lock()
	was := t.w.live
	t.w.at = t.f.now.Add(d)
	t.w.live = true
	t.w.fired = false
	// Re-register if a previous Stop removed it.
	found := false
	for _, cur := range t.f.waiters {
		if cur == t.w {
			found = true
			break
		}
	}
	if !found {
		t.f.waiters = append(t.f.waiters, t.w)
	}
	t.f.cond.Broadcast()
	t.f.mu.Unlock()
	return was
}

type fakeTicker struct {
	f *Fake
	w *fakeWaiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.ch }

func (t *fakeTicker) Stop() {
	t.f.mu.Lock()
	t.w.live = false
	t.f.mu.Unlock()
	t.f.forget(t.w)
}

// Compile-time checks.
var (
	_ Clock  = (*Fake)(nil)
	_ Timer  = (*fakeTimer)(nil)
	_ Ticker = (*fakeTicker)(nil)
)
