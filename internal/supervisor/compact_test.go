package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The data disk is compacted with the VM off: after a clean down, and
// before a start of a disk that has booted before (a VM a host reboot
// shut down had no down). A disk still to be made is not.
func TestDataDiskCompactedAfterDownAndBeforeUp(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if c := h.vm.compacted(); len(c) != 0 {
		t.Fatalf("a first up compacted a disk it was about to make: %+v", c)
	}
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	c := h.vm.compacted()
	if len(c) != 1 || c[0].path != h.dataPath() || !c[0].allStopped || c[0].boots != 1 {
		t.Fatalf("after down: %+v, want one compaction of %s with the VM stopped", c, h.dataPath())
	}
	h.mustUp(UpOptions{})
	c = h.vm.compacted()
	if len(c) != 2 || c[1].path != h.dataPath() || !c[1].allStopped || c[1].boots != 1 {
		t.Fatalf("after the second up: %+v, want a compaction before its boot", c)
	}
}

// A bounded stop (serve's exit as the host logs off or shuts down)
// compacts nothing after it, though the stop was clean: a compaction
// would be killed partway with the host. The next up compacts the disk
// before its boot; an unbounded down still compacts after the stop.
func TestBoundedShutdownDoesNotCompact(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Shutdown(time.Hour, h.logf()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("the VM is still running after the shutdown")
	}
	if c := h.vm.compacted(); len(c) != 0 {
		t.Fatalf("compactions after a bounded shutdown: %+v", c)
	}
	h.mustUp(UpOptions{})
	if c := h.vm.compacted(); len(c) != 1 || c[0].boots != 1 {
		t.Fatalf("after the next up: %+v, want a compaction before its boot", c)
	}
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	if c := h.vm.compacted(); len(c) != 2 || !c[1].allStopped {
		t.Fatalf("after an unbounded down: %+v, want a compaction after the stop", c)
	}
}

// A down for serve's exit (`down --exit`, the desktop app's quit and its
// exit at a logoff) compacts nothing after its clean stop: the host may
// be about to kill everything. The next up compacts the disk before its
// boot.
func TestDownToExitDoesNotCompact(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.DownToExit(context.Background(), h.logf()); err != nil {
		t.Fatalf("down: %v", err)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("the VM is still running after the down")
	}
	if c := h.vm.compacted(); len(c) != 0 {
		t.Fatalf("compactions after a down to exit: %+v", c)
	}
	h.mustUp(UpOptions{})
	if c := h.vm.compacted(); len(c) != 1 || !c[0].allStopped || c[0].boots != 1 {
		t.Fatalf("after the next up: %+v, want a compaction before its boot", c)
	}
}

// A down while an up compacts the data disk before its boot cancels the
// up there: the compaction ends, nothing boots, and the down finds the
// VM stopped.
func TestDownDuringPreBootCompactionBootsNothing(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	boots := h.vm.bootCount()
	entered, wait := make(chan struct{}), make(chan struct{})
	defer close(wait)
	h.vm.mu.Lock()
	h.vm.compactEntered, h.vm.compactWait = entered, wait
	h.vm.mu.Unlock()
	upped := make(chan error, 1)
	go func() { upped <- h.up(UpOptions{}) }()
	<-entered // the up is compacting the disk before its boot

	log := &serveLog{}
	if err := h.sup.Down(context.Background(), log.logf); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := <-upped; !errors.Is(err, context.Canceled) {
		t.Fatalf("up: %v, want it cancelled", err)
	}
	if n := h.vm.bootCount(); n != boots {
		t.Fatalf("%d boots after the cancelled up, want %d", n, boots)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("a VM is running after the down")
	}
	if !strings.Contains(strings.Join(log.get(), "\n"), "the VM is not running") {
		t.Fatalf("down's log = %q", log.get())
	}
}

// An up cancelled while the compaction before its boot runs to its end
// anyway boots nothing either.
func TestUpCancelledDuringACompactionThatFinishes(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	boots := h.vm.bootCount()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.vm.mu.Lock()
	h.vm.compactDone = cancel
	h.vm.mu.Unlock()
	if err := h.sup.Up(ctx, UpOptions{Port: h.port}, h.logf()); !errors.Is(err, context.Canceled) {
		t.Fatalf("up: %v, want it cancelled", err)
	}
	if n := h.vm.bootCount(); n != boots {
		t.Fatalf("%d boots after the cancelled up, want %d", n, boots)
	}
	if c := h.vm.compacted(); len(c) != 2 {
		t.Fatalf("compactions = %+v, want the down's and the up's", c)
	}
}

// A compaction that fails fails nothing: down and up go on, the log
// says so.
func TestDataDiskCompactionFailureIsOnlyLogged(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.mu.Lock()
	h.vm.compactErr = errors.New("broker: kivali-96e3ef74 is Running (409)")
	h.vm.mu.Unlock()
	log := &serveLog{}
	if err := h.sup.Down(context.Background(), log.logf); err != nil {
		t.Fatalf("down: %v", err)
	}
	if !strings.Contains(strings.Join(log.get(), "\n"), "the data disk was not compacted: broker: kivali-96e3ef74 is Running") {
		t.Fatalf("log = %q", log.get())
	}
	h.mustUp(UpOptions{})
	if m, _ := h.sup.current(); m == nil {
		t.Fatal("the VM is not running after up")
	}
	if c := h.vm.compacted(); len(c) != 2 {
		t.Fatalf("compactions = %+v", c)
	}
}

// A stop that had to cut the VM's power compacts nothing: the disk was
// not unmounted, and the next start's e2fsck comes first.
func TestNoCompactionAfterAHardStop(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.mu.Lock()
	h.vm.slowShutdown = true // the guest never stops; the stop's bound fires at once
	h.vm.mu.Unlock()
	err := h.sup.Down(context.Background(), h.logf())
	if err == nil || !strings.Contains(err.Error(), "powered off hard") {
		t.Fatalf("down: %v, want a hard stop", err)
	}
	if c := h.vm.compacted(); len(c) != 0 {
		t.Fatalf("compactions after a hard stop: %+v", c)
	}
}

// Neither an upgrade's stops and boots nor a destroy's stop compact the
// disk: the upgrade snapshots and swaps it, and destroy deletes it.
func TestUpgradeAndDestroyDoNotCompact(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if c := h.vm.compacted(); len(c) != 0 {
		t.Fatalf("the upgrade compacted: %+v", c)
	}
	if _, err := h.sup.Destroy(context.Background(), h.logf()); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if c := h.vm.compacted(); len(c) != 0 {
		t.Fatalf("destroy compacted: %+v", c)
	}
}

// The compaction runs inside the down's operation, with the VM already
// stopped: another operation (an up) queues behind it rather than
// booting on a disk being compacted, and status names the down.
func TestCompactionRunsInsideTheDown(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	entered, wait := make(chan struct{}), make(chan struct{})
	h.vm.mu.Lock()
	h.vm.compactEntered, h.vm.compactWait = entered, wait
	h.vm.mu.Unlock()
	downDone := make(chan error, 1)
	go func() { downDone <- h.sup.Down(context.Background(), h.logf()) }()
	<-entered
	if op := h.sup.busy(); op != "down" {
		t.Fatalf("operation = %q while the compaction runs, want down", op)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("the VM is still current while its disk is compacted")
	}
	close(wait)
	if err := <-downDone; err != nil {
		t.Fatalf("down: %v", err)
	}
	if c := h.vm.compacted(); len(c) != 1 || !c[0].allStopped {
		t.Fatalf("compactions = %+v", c)
	}
}
