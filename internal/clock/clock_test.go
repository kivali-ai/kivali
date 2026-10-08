package clock

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The Fake is about to be load-bearing for the whole test suite, so
// it gets tested harder than most things. A subtly wrong fake clock
// produces tests that pass for the wrong reason, which is worse than
// having no fake at all.

func recv(t *testing.T, ch <-chan time.Time) (time.Time, bool) {
	t.Helper()
	select {
	case v := <-ch:
		return v, true
	default:
		return time.Time{}, false
	}
}

func mustRecv(t *testing.T, ch <-chan time.Time, what string) time.Time {
	t.Helper()
	v, ok := recv(t, ch)
	if !ok {
		t.Fatalf("%s: expected a tick, got none", what)
	}
	return v
}

func mustNotRecv(t *testing.T, ch <-chan time.Time, what string) {
	t.Helper()
	if v, ok := recv(t, ch); ok {
		t.Fatalf("%s: unexpected tick at %s", what, v)
	}
}

func TestFakeStartsAtAFixedInstant(t *testing.T) {
	// Not time.Now(): a test whose behaviour depends on the real date
	// is a test that fails on some future Tuesday.
	a, b := NewFake(), NewFake()
	if !a.Now().Equal(b.Now()) {
		t.Fatalf("two fakes disagree: %s vs %s", a.Now(), b.Now())
	}
	if a.Now().Location() != time.UTC {
		t.Fatalf("fake should start in UTC, got %s", a.Now().Location())
	}
}

func TestFakeNowOnlyMovesWhenTold(t *testing.T) {
	f := NewFake()
	start := f.Now()
	f.Advance(90 * time.Second)
	if got := f.Now().Sub(start); got != 90*time.Second {
		t.Fatalf("advanced by %s, want 90s", got)
	}
}

func TestTimerFiresExactlyOnceAtItsDeadline(t *testing.T) {
	f := NewFake()
	timer := f.NewTimer(10 * time.Second)

	f.Advance(9 * time.Second)
	mustNotRecv(t, timer.C(), "before the deadline")

	f.Advance(time.Second)
	at := mustRecv(t, timer.C(), "at the deadline")

	// The delivered value is the instant the timer was due, not the
	// end of the advance — code that reads it is entitled to the truth
	// about when the event happened.
	if want := f.Now(); !at.Equal(want) {
		t.Fatalf("fired with %s, want %s", at, want)
	}

	f.Advance(time.Hour)
	mustNotRecv(t, timer.C(), "a one-shot must not fire twice")
}

// A timer landing exactly on the boundary has to fire. Off-by-one here
// would make every "advance exactly the grace window" test wrong.
func TestTimerFiresOnTheExactBoundary(t *testing.T) {
	f := NewFake()
	timer := f.NewTimer(20 * time.Second)
	f.Advance(20 * time.Second)
	mustRecv(t, timer.C(), "exactly on the boundary")
}

func TestStoppedTimerNeverFires(t *testing.T) {
	f := NewFake()
	timer := f.NewTimer(10 * time.Second)

	if !timer.Stop() {
		t.Fatal("Stop on a pending timer should report true")
	}
	f.Advance(time.Hour)
	mustNotRecv(t, timer.C(), "after Stop")

	// Idempotent, and honest about having already been stopped.
	if timer.Stop() {
		t.Fatal("second Stop should report false")
	}
	if n := f.Waiters(); n != 0 {
		t.Fatalf("stopped timer still armed: %d waiters", n)
	}
}

func TestTimerReset(t *testing.T) {
	f := NewFake()
	timer := f.NewTimer(10 * time.Second)

	f.Advance(5 * time.Second)
	if !timer.Reset(10 * time.Second) {
		t.Fatal("Reset on a pending timer should report true")
	}
	f.Advance(5 * time.Second)
	mustNotRecv(t, timer.C(), "the reset pushed the deadline out")

	f.Advance(5 * time.Second)
	mustRecv(t, timer.C(), "after the reset deadline")
}

// Resetting a stopped timer has to re-arm it, or a retry loop that
// stops and resets goes permanently silent.
func TestResetReArmsAStoppedTimer(t *testing.T) {
	f := NewFake()
	timer := f.NewTimer(10 * time.Second)
	timer.Stop()

	if timer.Reset(5 * time.Second) {
		t.Fatal("Reset on a stopped timer should report false")
	}
	f.Advance(5 * time.Second)
	mustRecv(t, timer.C(), "after re-arming")
}

func TestTickerFiresEveryPeriod(t *testing.T) {
	f := NewFake()
	ticker := f.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for i := 1; i <= 3; i++ {
		f.Advance(10 * time.Second)
		at := mustRecv(t, ticker.C(), "tick")
		if want := f.Now(); !at.Equal(want) {
			t.Fatalf("tick %d at %s, want %s", i, at, want)
		}
	}
}

// A real ticker drops ticks its receiver was too slow to take rather
// than building a backlog. The fake has to behave the same, or a test
// sees a burst of catch-up ticks that production would never deliver.
func TestTickerDropsTicksNobodyRead(t *testing.T) {
	f := NewFake()
	ticker := f.NewTicker(time.Second)
	defer ticker.Stop()

	f.Advance(10 * time.Second)

	mustRecv(t, ticker.C(), "the one buffered tick")
	mustNotRecv(t, ticker.C(), "the nine that were dropped")
}

func TestStoppedTickerNeverFires(t *testing.T) {
	f := NewFake()
	ticker := f.NewTicker(time.Second)
	ticker.Stop()

	f.Advance(time.Hour)
	mustNotRecv(t, ticker.C(), "after Stop")
	ticker.Stop() // idempotent
	if n := f.Waiters(); n != 0 {
		t.Fatalf("stopped ticker still armed: %d waiters", n)
	}
}

// Waiters must fire in the order real time would have fired them, not
// in registration order.
func TestAdvanceFiresInChronologicalOrder(t *testing.T) {
	f := NewFake()
	late := f.NewTimer(30 * time.Second)
	early := f.NewTimer(10 * time.Second)
	middle := f.NewTimer(20 * time.Second)

	f.Advance(time.Minute)

	e := mustRecv(t, early.C(), "early")
	m := mustRecv(t, middle.C(), "middle")
	l := mustRecv(t, late.C(), "late")

	if !e.Before(m) || !m.Before(l) {
		t.Fatalf("out of order: early=%s middle=%s late=%s", e, m, l)
	}
}

// Nobody is reading these channels. If firing were a blocking send the
// clock would deadlock, and every test using it would hang.
func TestAdvanceDoesNotBlockOnUnreadWaiters(t *testing.T) {
	f := NewFake()
	for i := 0; i < 50; i++ {
		f.NewTimer(time.Duration(i+1) * time.Second)
		f.NewTicker(time.Duration(i+1) * time.Second)
	}
	f.Advance(time.Hour) // must return
}

func TestSetJumpsForwardAndFires(t *testing.T) {
	f := NewFake()
	timer := f.NewTimer(10 * time.Second)

	f.Set(f.Now().Add(30 * time.Second))
	mustRecv(t, timer.C(), "after jumping past the deadline")
}

// A backwards jump is what an NTP correction looks like. Moving Now is
// honest; firing timers that were never due is not.
func TestSetBackwardsDoesNotFire(t *testing.T) {
	f := NewFake()
	timer := f.NewTimer(10 * time.Second)
	start := f.Now()

	f.Set(start.Add(-time.Hour))
	mustNotRecv(t, timer.C(), "after a backwards jump")
	if !f.Now().Equal(start.Add(-time.Hour)) {
		t.Fatalf("Now = %s, want the earlier instant", f.Now())
	}
}

// The synchronisation primitive that stops the classic flake: advance
// before the goroutine armed its timer and nothing ever fires.
func TestBlockUntilWaitsForWaitersToArm(t *testing.T) {
	f := NewFake()
	fired := make(chan struct{})

	go func() {
		timer := f.NewTimer(10 * time.Second)
		<-timer.C()
		close(fired)
	}()

	f.BlockUntil(1)
	f.Advance(10 * time.Second)

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the goroutine never observed its timer firing")
	}
}

func TestBlockUntilReturnsImmediatelyWhenAlreadyArmed(t *testing.T) {
	f := NewFake()
	f.NewTimer(time.Second)
	f.NewTicker(time.Second)
	f.BlockUntil(2) // must not block
}

// The goroutine-safe variant: a helper that stands ready to fire a
// timer some tests never arm must not take down the binary.
func TestBlockUntilContextReturnsInsteadOfPanicking(t *testing.T) {
	f := NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.BlockUntilContext(ctx, 1); err == nil {
		t.Fatal("a cancelled context should return an error, not block")
	}
}

func TestBlockUntilContextSucceedsWhenWaitersArm(t *testing.T) {
	f := NewFake()
	go func() { f.NewTimer(time.Second) }()
	if err := f.BlockUntilContext(context.Background(), 1); err != nil {
		t.Fatalf("BlockUntilContext: %v", err)
	}
}

func TestWaitersCountsOnlyLiveTimers(t *testing.T) {
	f := NewFake()
	if n := f.Waiters(); n != 0 {
		t.Fatalf("fresh fake has %d waiters", n)
	}
	timer := f.NewTimer(time.Second)
	ticker := f.NewTicker(time.Second)
	if n := f.Waiters(); n != 2 {
		t.Fatalf("waiters = %d, want 2", n)
	}

	// A fired one-shot stops being a waiter.
	f.Advance(time.Second)
	<-timer.C()
	if n := f.Waiters(); n != 1 {
		t.Fatalf("after firing, waiters = %d, want 1 (the ticker)", n)
	}
	ticker.Stop()
	if n := f.Waiters(); n != 0 {
		t.Fatalf("after stopping, waiters = %d", n)
	}
}

func TestSortedDeadlines(t *testing.T) {
	f := NewFake()
	f.NewTimer(30 * time.Second)
	f.NewTimer(10 * time.Second)
	f.NewTimer(20 * time.Second)

	got := f.Deadlines()
	if len(got) != 3 {
		t.Fatalf("deadlines = %v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Before(got[i-1]) {
			t.Fatalf("deadlines out of order: %v", got)
		}
	}
}

// Every Kivali process has concurrent goroutines reading the clock
// while others arm timers. Under -race this is the test that catches a
// missing lock.
func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	f := NewFake()
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = f.Now()
				timer := f.NewTimer(time.Second)
				timer.Stop()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				f.Advance(time.Millisecond)
			}
		}()
	}
	wg.Wait()
}

func TestAdvanceRejectsNegativeDurations(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Advance with a negative duration should panic")
		}
	}()
	NewFake().Advance(-time.Second)
}

func TestNewTickerRejectsNonPositiveDurations(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewTicker(0) should panic, matching time.NewTicker")
		}
	}()
	NewFake().NewTicker(0)
}

func TestSinceAndUntil(t *testing.T) {
	f := NewFake()
	start := f.Now()
	f.Advance(30 * time.Second)

	if got := Since(f, start); got != 30*time.Second {
		t.Fatalf("Since = %s, want 30s", got)
	}
	if got := Until(f, start.Add(time.Minute)); got != 30*time.Second {
		t.Fatalf("Until = %s, want 30s", got)
	}
}

// ─── the production clock ───────────────────────────────────────────
//
// Thin wrappers, but a swapped Stop/Reset or an unwired channel would
// be invisible until something misbehaved in production.

func TestSystemClockMoves(t *testing.T) {
	c := New()
	first := c.Now()
	second := c.Now()
	if second.Before(first) {
		t.Fatalf("time went backwards: %s then %s", first, second)
	}
}

func TestSystemTimerFires(t *testing.T) {
	timer := New().NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("the system timer never fired")
	}
	if timer.Stop() {
		t.Fatal("Stop on an already-fired timer should report false")
	}
}

func TestSystemTickerFires(t *testing.T) {
	ticker := New().NewTicker(time.Millisecond)
	defer ticker.Stop()
	for i := 0; i < 2; i++ {
		select {
		case <-ticker.C():
		case <-time.After(5 * time.Second):
			t.Fatalf("the system ticker stopped after %d ticks", i)
		}
	}
}
