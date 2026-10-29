package guestapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClocks is a guest whose system clock and host clock the test
// moves by hand. mu guards every field: Run reads them from its own
// goroutine.
type fakeClocks struct {
	mu       sync.Mutex
	sys, rtc time.Time
	rtcErr   error
	setErr   error
	set      []time.Time
	log      []string
}

func (f *fakeClocks) sync() *ClockSync {
	return &ClockSync{
		ReadHost: func() (time.Time, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.rtc, f.rtcErr
		},
		Now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.sys
		},
		Set: func(t time.Time) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.setErr != nil {
				return f.setErr
			}
			f.set = append(f.set, t)
			f.sys = t
			return nil
		},
		Logf: func(format string, args ...any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.log = append(f.log, strings.TrimSpace(format))
		},
	}
}

// setRTC moves the host clock under the lock.
func (f *fakeClocks) setRTC(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rtc = t
}

// Why this test exists: after the host sleeps, the guest's system clock
// stands as many hours behind the hardware clock as the host slept, and
// nothing in the kernel corrects it. Every check must step the system
// clock to the hardware clock, forward or back, while a difference
// inside the tolerance, which the hardware clock's one-second reading
// leaves after every step, must not move it: a step at every tick
// would make the clock jump around.
func TestClockSyncStepsASystemClockThatFellBehind(t *testing.T) {
	base := time.Date(2026, 10, 25, 7, 30, 0, 0, time.UTC)
	f := &fakeClocks{sys: base.Add(-10 * time.Hour), rtc: base}
	step, err := f.sync().Once()
	if err != nil || step != 10*time.Hour || len(f.set) != 1 || !f.set[0].Equal(base) {
		t.Fatalf("step %v, err %v, set %v", step, err, f.set)
	}
	if len(f.log) != 1 || !strings.Contains(f.log[0], "stepped the system clock") {
		t.Errorf("log %q", f.log)
	}

	// Within the tolerance: left alone, nothing logged.
	f.sys = base.Add(1500 * time.Millisecond)
	if step, err := f.sync().Once(); err != nil || step != 0 || len(f.set) != 1 {
		t.Fatalf("within tolerance: step %v, err %v, set %v", step, err, f.set)
	}
	f.sys = base.Add(-ClockSyncTolerance)
	if step, _ := f.sync().Once(); step != 0 || len(f.set) != 1 {
		t.Fatalf("at the tolerance: step %v, set %v", step, f.set)
	}
	if len(f.log) != 1 {
		t.Errorf("log %q", f.log)
	}

	// Ahead by more than the tolerance (the host's clock was set back):
	// stepped back.
	f.sys = base.Add(3 * time.Second)
	if step, err := f.sync().Once(); err != nil || step != -3*time.Second || len(f.set) != 2 || !f.set[1].Equal(base) {
		t.Fatalf("ahead: step %v, err %v, set %v", step, err, f.set)
	}
}

// Why this test exists: a guest with no readable hardware clock (a
// hypervisor that shows none) must keep its system clock as it is and
// say so once in its log, not at every tick for the life of the VM;
// and a step the kernel refuses must be reported, not lost.
func TestClockSyncSaysOnceWhenItCannotRead(t *testing.T) {
	base := time.Date(2026, 10, 25, 7, 30, 0, 0, time.UTC)
	f := &fakeClocks{sys: base, rtc: base.Add(time.Hour), rtcErr: errors.New("the guest has no hardware clock")}
	s := f.sync()
	for range 3 {
		if _, err := s.Once(); err == nil {
			t.Fatal("no error from an unreadable hardware clock")
		}
	}
	if len(f.set) != 0 || len(f.log) != 1 || !strings.Contains(f.log[0], "cannot read the host's clock") {
		t.Fatalf("set %v, log %q", f.set, f.log)
	}
	// Readable again, then not: said again.
	f.rtcErr = nil
	if step, err := s.Once(); err != nil || step != time.Hour {
		t.Fatalf("readable again: step %v, err %v", step, err)
	}
	f.rtcErr = errors.New("the guest has no hardware clock")
	_, _ = s.Once()
	if len(f.log) != 3 {
		t.Fatalf("log %q", f.log)
	}

	f = &fakeClocks{sys: base, rtc: base.Add(time.Hour), setErr: errors.New("operation not permitted")}
	if _, err := f.sync().Once(); err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("set refused: err %v", err)
	}
}

// Why this test exists: the agent checks the clocks at each tick for as
// long as it runs; it must check at every tick it is given and stop
// when its context ends, or a stuck loop would hold the agent's
// shutdown.
func TestClockSyncRunsAtEachTick(t *testing.T) {
	base := time.Date(2026, 10, 25, 7, 30, 0, 0, time.UTC)
	f := &fakeClocks{sys: base, rtc: base}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.sync().Run(ctx, ticks); close(done) }()
	f.setRTC(base.Add(time.Hour))
	ticks <- base
	f.setRTC(base.Add(2 * time.Hour))
	ticks <- base
	cancel()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.set) != 2 || !f.set[1].Equal(base.Add(2*time.Hour)) {
		t.Fatalf("set %v", f.set)
	}
}
