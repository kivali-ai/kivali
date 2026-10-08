package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// A connection accepted by the forward whose dial is blocked in the
// guest must not wedge Down (or anything else that stops the forward).
func TestDownWithAConnectionInFlight(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	entered := make(chan struct{})
	h.vm.mu.Lock()
	h.vm.proxyEntered = entered
	h.vm.mu.Unlock()
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", h.port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	<-entered // the forward is now inside the guest's Proxy

	done := make(chan error, 1)
	go func() { done <- h.sup.Down(context.Background(), h.logf()) }()
	// Status takes s.mu; it must answer while Down tears the forward down.
	_ = h.sup.Status(context.Background())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := h.sup.Status(context.Background()); r.Running || r.Forward != "" {
		t.Fatalf("after down: %+v", r)
	}
}

// Down behind an upgrade says so at once and can stop waiting; the
// upgrade itself carries on.
func TestDownWaitsForAnUpgradeAndCanGiveUp(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.importEntered, h.vm.importRelease = make(chan struct{}), make(chan struct{})
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	upgraded := make(chan error, 1)
	go func() { upgraded <- h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()) }()
	<-h.vm.importEntered

	if r := h.sup.Status(context.Background()); r.Operation != "upgrade" || !r.Busy {
		t.Fatalf("status during upgrade: %+v", r)
	}
	lines := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	downed := make(chan error, 1)
	go func() {
		downed <- h.sup.Down(ctx, func(f string, a ...any) { lines <- fmt.Sprintf(f, a...) })
	}()
	if l := <-lines; l != "an upgrade is running; waiting for it to finish" {
		t.Fatalf("first line %q", l)
	}
	cancel()
	if err := <-downed; !errors.Is(err, context.Canceled) {
		t.Fatalf("down: %v", err)
	}
	close(h.vm.importRelease)
	if err := <-upgraded; err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Kivali != "0.16.0" {
		t.Fatalf("state %+v", h.sup.State())
	}
}

// serve's signal path waits for a running upgrade, however long, and
// only then stops the VM: it never abandons the upgrade.
func TestShutdownWaitsForAnUpgrade(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.importEntered, h.vm.importRelease = make(chan struct{}), make(chan struct{})
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	upgraded := make(chan error, 1)
	go func() { upgraded <- h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()) }()
	<-h.vm.importEntered

	lines := make(chan string, 8)
	shut := make(chan error, 1)
	// A tiny bound: it must not start counting until the upgrade is done.
	go func() {
		shut <- h.sup.Shutdown(time.Nanosecond, func(f string, a ...any) { lines <- fmt.Sprintf(f, a...) })
	}()
	if l := <-lines; l != "an upgrade is running; waiting for it to finish" {
		t.Fatalf("first line %q", l)
	}
	close(h.vm.importRelease)
	if err := <-upgraded; err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if err := <-shut; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("VM still current after shutdown")
	}
	if st := h.sup.State(); st.Kivali != "0.16.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
}

// A down whose client is already gone still stops the VM once it has
// cancelled a running up.
func TestDownAfterCancellingAnUpStillStops(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	entered := make(chan struct{})
	h.vm.mu.Lock()
	h.vm.execEntered = entered
	h.vm.mu.Unlock()
	upped := make(chan error, 1)
	go func() { upped <- h.up(UpOptions{}) }()
	<-entered // the up is inside a guest exec

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.sup.Down(gone, h.logf()); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := <-upped; !errors.Is(err, context.Canceled) {
		t.Fatalf("up: %v", err)
	}
	if h.vm.machines[0].running() {
		t.Fatal("the VM was left running")
	}
}

// The documented way past a refused recovery: edit local.json while
// serve runs, then up again.
func TestRefusedRecoveryEditThenUp(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	snap := h.snapPath()
	h.crashAt(Journal{From: "0.15.0", To: "0.16.0", Step: StepApplying, Snapshot: snap, SnapshotComplete: true})
	err := h.up(UpOptions{})
	if err == nil || !strings.Contains(err.Error(), `"upgrade" is null, then run`) {
		t.Fatalf("up: %v", err)
	}
	boots := len(h.vm.boots)
	st, err := LoadState(h.dir, h.dataPath())
	if err != nil {
		t.Fatal(err)
	}
	st.Upgrade = nil
	if err := SaveState(h.sup.o.Host, h.dir, st); err != nil {
		t.Fatal(err)
	}
	h.mustUp(UpOptions{}) // the same serve: no restart
	if len(h.vm.boots) != boots+1 || h.sup.State().Upgrade != nil {
		t.Fatalf("after the edit: %d boots, state %+v", len(h.vm.boots), h.sup.State())
	}
}

func TestRecoverMissingSnapshotByStep(t *testing.T) {
	t.Run("applying, missing: refused, journal kept", func(t *testing.T) {
		h := newHarness(t)
		h.mustUp(UpOptions{Install: owner})
		snap := h.snapPath()
		h.crashAt(Journal{From: "0.15.0", To: "0.16.0", Step: StepApplying, Snapshot: snap, SnapshotComplete: true})
		boots := len(h.vm.boots)
		err := h.up(UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "that file is missing") || !strings.Contains(err.Error(), "journal is kept") {
			t.Fatalf("up: %v", err)
		}
		if j := h.sup.State().Upgrade; j == nil || j.Step != StepApplying {
			t.Fatalf("journal %+v", j)
		}
		if len(h.vm.boots) != boots {
			t.Fatal("booted the possibly half-applied disk")
		}
	})
	t.Run("rolling-back, missing: completed", func(t *testing.T) {
		h := newHarness(t)
		h.mustUp(UpOptions{Install: owner})
		snap := h.snapPath()
		h.crashAt(Journal{From: "0.15.0", To: "0.16.0", Step: StepRollingBack, Snapshot: snap, SnapshotComplete: true})
		h.mustUp(UpOptions{})
		if h.sup.State().Upgrade != nil || h.sup.State().Kivali != "0.15.0" {
			t.Fatalf("state %+v", h.sup.State())
		}
	})
	t.Run("rolling-back, present: renamed back", func(t *testing.T) {
		h := newHarness(t)
		h.mustUp(UpOptions{Install: owner})
		before := h.disk()[ManifestRel]
		snap := h.snapPath()
		if err := host.Default().Snapshot(h.dataPath(), snap); err != nil {
			t.Fatal(err)
		}
		fs := h.disk()
		fs[ManifestRel] = "half-applied"
		if err := writeDisk(h.dataPath(), fs); err != nil {
			t.Fatal(err)
		}
		h.crashAt(Journal{From: "0.15.0", To: "0.16.0", Step: StepRollingBack, Snapshot: snap, SnapshotComplete: true})
		h.mustUp(UpOptions{})
		if exists(snap) || h.sup.State().Upgrade != nil || h.disk()[ManifestRel] != before {
			t.Fatalf("not renamed back: snapshot %v, state %+v", exists(snap), h.sup.State())
		}
	})
}

func TestUpgradeRefusesChartVersionMismatch(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeReleaseChart(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "0.16.1", "v0.16.0")
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "version 0.16.0") || !strings.Contains(err.Error(), "says 0.16.1") {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Upgrade != nil || len(h.vm.boots) != 1 {
		t.Fatal("a refused release touched the org")
	}
}

func TestUpgradeDeletesDownloads(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	dl := filepath.Join(h.dir, "downloads", "0.16.0")
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "a"), "0.16.0", "v0.16.0")}, h.logf()); err != nil {
		t.Fatal(err)
	}
	if exists(dl) {
		t.Fatal("downloads kept after a commit")
	}
	h.vm.broken["v0.17.0"] = true
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "b"), "0.17.0", "v0.17.0")}, h.logf()); err == nil {
		t.Fatal("broken upgrade succeeded")
	}
	if exists(filepath.Join(h.dir, "downloads", "0.17.0")) {
		t.Fatal("downloads kept after a rollback")
	}
}

func TestRollbackBootFailureWording(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.broken["v0.16.0"] = true
	// Boots: 1 install, 2 the upgrade, 3 the rollback.
	h.vm.fatalAt = map[int]string{3: "ready: no node"}
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "the data disk was restored to 0.15.0, but starting it failed") || strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Upgrade != nil || h.sup.State().Kivali != "0.15.0" {
		t.Fatalf("state %+v", h.sup.State())
	}
}

func TestBackupFailureLeavesNoFile(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.startOrg(t)
	h.vm.backupFails = true
	out := filepath.Join(h.dir, "org.zip")
	if _, err := h.sup.BackupToFile(context.Background(), out, h.logf()); err == nil || !strings.Contains(err.Error(), "download stopped") {
		t.Fatalf("backup: %v", err)
	}
	ents, _ := os.ReadDir(h.dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), "backup") || e.Name() == "org.zip" {
			t.Fatalf("left %s behind", e.Name())
		}
	}
}

func TestExecLogOmitsArguments(t *testing.T) {
	var logged []string
	h := newHarness(t)
	h.sup.o.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	h.mustUp(UpOptions{Install: owner})
	_, _ = h.sup.Exec(context.Background(), []string{"sh", "-c", "echo sk-secret-key"})
	for _, l := range logged {
		if strings.Contains(l, "sk-secret-key") {
			t.Fatalf("argument logged: %q", l)
		}
	}
	if !strings.Contains(strings.Join(logged, "\n"), "exec: sh (2 args)") {
		t.Fatalf("logs %v", logged)
	}
}

func TestLockServe(t *testing.T) {
	var hst Host = host.Default()
	dir := t.TempDir()
	release, err := hst.LockServe(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hst.LockServe(dir); !errors.Is(err, host.ErrServeRunning) {
		t.Fatalf("second lock: %v", err)
	}
	release()
	again, err := hst.LockServe(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}
