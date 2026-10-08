package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// beatClock is the fake clock for a stop that outlasts several
// heartbeats: the stop's deadline never fires, each heartbeat fires at
// once and calls onBeat with its number (from 1).
type beatClock struct {
	*fakeClock
	beats  int
	onBeat func(n int)
}

func (c *beatClock) After(d time.Duration) <-chan time.Time {
	switch d {
	case stopTimeout:
		return make(chan time.Time)
	case stopHeartbeat:
		c.beats++
		c.onBeat(c.beats)
	}
	return c.fakeClock.After(d)
}

// A guest whose clean shutdown takes a while keeps the stop talking: a
// caller that reads a silent stop as wedged (the desktop app kills serve
// after 30 s of silence, and on macOS the VM dies with it) hears a line
// every stopHeartbeat, and the stop still ends clean, never hard.
func TestStopHeartbeatsWhileTheGuestShutsDown(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.mu.Lock()
	h.vm.slowShutdown = true
	m := h.vm.machines[len(h.vm.machines)-1]
	h.vm.mu.Unlock()
	h.sup.o.Clock = &beatClock{fakeClock: h.clock, onBeat: func(n int) {
		if n == 4 { // three heartbeats said, then the guest finishes
			m.stop(true)
		}
	}}
	log := &serveLog{}
	if err := h.sup.Down(context.Background(), log.logf); err != nil {
		t.Fatalf("down: %v", err)
	}
	var beats int
	for _, l := range log.get() {
		if strings.HasPrefix(l, "waiting for the VM to finish its clean shutdown") {
			beats++
		}
		if strings.Contains(l, "powered off hard") {
			t.Fatalf("a slow clean shutdown was cut: %q", log.get())
		}
	}
	if beats != 3 {
		t.Fatalf("%d heartbeat lines, want 3: %q", beats, log.get())
	}
	if !strings.Contains(strings.Join(log.get(), "\n"), "data disk unmounted cleanly") {
		t.Fatalf("no clean unmount: %q", log.get())
	}
}

// A boot given up after the guest is READY (here: local.json cannot be
// saved) stops its VM, and cleanly: left running untracked, the next up
// would boot a second VM on the same data disk, and a hard stop would
// cut power on a mounted one.
func TestAbandonedBootStopsItsVMCleanly(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	h.renameErr = func(_, p string) error {
		if filepath.Base(p) != "local.json" {
			return nil
		}
		h.vm.mu.Lock()
		defer h.vm.mu.Unlock()
		for _, m := range h.vm.machines {
			if m.running() {
				return errors.New("disk full")
			}
		}
		return nil
	}
	h.mu.Unlock()
	if err := h.up(UpOptions{Install: owner}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("up: %v, want the save's error", err)
	}
	h.vm.mu.Lock()
	defer h.vm.mu.Unlock()
	for i, m := range h.vm.machines {
		if m.running() {
			t.Fatalf("VM %d still running after its boot was given up", i+1)
		}
	}
	if h.vm.poweroffs != 1 || h.vm.hardStops != 0 {
		t.Fatalf("%d poweroff requests and %d hard stops, want 1 and 0", h.vm.poweroffs, h.vm.hardStops)
	}
}

// A data disk that cannot be checked is never treated as missing: that
// would recreate it, wiping the org.
func TestUnreadableDataDiskIsNeverRecreated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block a stat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads through permissions")
	}
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(h.dataPath())
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := h.up(UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "cannot check the data disk") {
		t.Fatalf("up: %v, want a refusal", err)
	}
	h.vm.mu.Lock()
	created := len(h.vm.created)
	h.vm.mu.Unlock()
	if created != 1 {
		t.Fatalf("the data disk was created %d times, want once", created)
	}
}

// An upgrade that worked but whose commit cannot be recorded leaves the
// journal at applying, so the next start rolls back to the snapshot: the
// org is stopped rather than left taking writes that rollback would
// silently discard.
func TestUpgradeCommitThatCannotBeRecordedStopsTheOrg(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	h.mu.Lock()
	h.renameErr = func(old, p string) error {
		if filepath.Base(p) != "local.json" {
			return nil
		}
		b, err := os.ReadFile(old)
		if err == nil && strings.Contains(string(b), `"step": "`+StepCommitted+`"`) {
			return errors.New("disk full")
		}
		return nil
	}
	h.mu.Unlock()
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "could not be recorded") || !strings.Contains(err.Error(), "rolls back to 0.15.0") {
		t.Fatalf("upgrade: %v", err)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("the org kept running on an upgrade the journal will roll back")
	}
	h.mu.Lock()
	h.renameErr = nil
	h.mu.Unlock()
	h.mustUp(UpOptions{})
	if st := h.sup.State(); st.Kivali != "0.15.0" || st.Upgrade != nil {
		t.Fatalf("after the next up: state %+v, want rolled back to 0.15.0", st)
	}
}
