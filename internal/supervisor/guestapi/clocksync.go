package guestapi

import (
	"context"
	"fmt"
	"time"
)

// ClockSync keeps the guest's system clock on its hardware clock. The
// guest's system clock stops with its CPUs: while the host sleeps, or
// the VM is paused, no time passes in the guest, and the kernel reads
// the hardware clock only at boot, so afterwards the guest runs behind
// the host by however long it stood still. The hardware clock the
// hypervisor shows the guest follows the host's clock all along. The
// agent compares the two every ClockSyncEvery and, when they differ by
// more than ClockSyncTolerance, steps the system clock to the hardware
// clock. What depends on the guest's clock agreeing with the host's
// then holds again: the handoff tokens the supervisor mints, which the
// server refuses when their expiry stands too far from its own clock,
// the JSON Web Tokens an integration signs for a service, and TLS.
type ClockSync struct {
	// ReadRTC reads the hardware clock, to the second.
	ReadRTC func() (time.Time, error)
	// Now reads the system clock.
	Now func() time.Time
	// Set steps the system clock to the time given.
	Set func(time.Time) error
	// Logf logs; nil discards.
	Logf func(format string, args ...any)

	// said is the last reason the hardware clock could not be read,
	// logged once rather than every ClockSyncEvery.
	said string
}

// ClockSyncEvery is how often the system clock is compared with the
// hardware clock: soon enough after the host wakes that a person who
// opens the team then finds its clock right.
const ClockSyncEvery = 10 * time.Second

// ClockSyncTolerance is the difference the agent lets stand. The
// hardware clock is read to the second, so a system clock set from it
// is within a second of it at best; a step at every tick would follow.
const ClockSyncTolerance = 2 * time.Second

// Once compares the clocks and steps the system clock when they differ
// by more than ClockSyncTolerance. It returns the step applied, zero
// for none, and any error reading or setting a clock.
func (c *ClockSync) Once() (time.Duration, error) {
	rtc, err := c.ReadRTC()
	if err != nil {
		if s := err.Error(); s != c.said {
			c.said = s
			c.logf("clock: cannot read the hardware clock, so the system clock is left as it is: %v", err)
		}
		return 0, err
	}
	c.said = ""
	now := c.Now()
	step := rtc.Sub(now)
	if step.Abs() <= ClockSyncTolerance {
		return 0, nil
	}
	if err := c.Set(rtc); err != nil {
		c.logf("clock: the system clock stands %s from the hardware clock and could not be set: %v", signed(step), err)
		return 0, fmt.Errorf("set the system clock: %w", err)
	}
	c.logf("clock: stepped the system clock %s, from %s to the hardware clock's %s", signed(step), now.UTC().Format(time.RFC3339), rtc.UTC().Format(time.RFC3339))
	return step, nil
}

// Run calls Once at each tick until ticks closes or ctx ends.
func (c *ClockSync) Run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			_, _ = c.Once()
		}
	}
}

func (c *ClockSync) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// signed is d with its sign, rounded to the second: "+10h0m17s".
func signed(d time.Duration) string {
	d = d.Round(time.Second)
	if d >= 0 {
		return "+" + d.String()
	}
	return d.String()
}
