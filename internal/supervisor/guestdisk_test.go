package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// floorBytes is the free space kubelet keeps on the fake's data disk.
const floorBytes = DataDiskSize / 100 * imagefsAvailablePct

func TestParseDF(t *testing.T) {
	for name, out := range map[string]string{
		"busybox":   "Filesystem           1024-blocks    Used Available Capacity Mounted on\n/dev/vdb              65793780  57256180   8537600  87% /var/lib/kivali\n",
		"coreutils": "Filesystem     1024-blocks     Used Available Capacity Mounted on\n/dev/vdb          65793780 57256180   8537600      87% /var/lib/kivali\n",
	} {
		d, err := parseDF(out)
		if err != nil || d.size != 65793780<<10 || d.avail != 8537600<<10 {
			t.Errorf("%s: %+v, %v", name, d, err)
		}
	}
	for _, out := range []string{"", "Filesystem 1024-blocks Used Available Capacity Mounted on\n", "Filesystem\n/dev/vdb x y z 1% /var/lib/kivali\n"} {
		if d, err := parseDF(out); err == nil {
			t.Errorf("parseDF(%q) = %+v, want an error", out, d)
		}
	}
}

// bundleBytes is the size of a release's image bundle for the fake's
// platform.
func bundleBytes(t *testing.T, h *harness, feed string) uint64 {
	t.Helper()
	b, err := os.ReadFile(feed)
	if err != nil {
		t.Fatal(err)
	}
	var rel Release
	if err := json.Unmarshal(b, &rel); err != nil {
		t.Fatal(err)
	}
	return uint64(rel.Images[h.sup.platform()].Size)
}

func (h *harness) setGuest(f func(vm *fakeBackend)) {
	h.vm.mu.Lock()
	defer h.vm.mu.Unlock()
	f(h.vm)
}

// The preflight measures the team's data disk inside the VM: the new
// images on top of the free space kubelet keeps (15% of the disk) and a
// gigabyte for the release to start in. Short of that it refuses before
// anything is downloaded. Without it, a team whose own data had filled
// most of its disk imported the new images, the node went under disk
// pressure, the new server never scheduled, and the upgrade waited out
// its whole timeout before rolling back.
func TestUpgradeRefusesADataDiskWithoutRoomForTheImages(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	need := floorBytes + guestHeadroom + importFactor*bundleBytes(t, h, feed)
	h.setGuest(func(vm *fakeBackend) { vm.guestAvail = need - 1 })
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	refusedForRoom(t, h, err, need-1, nothingChanged)
	if !strings.Contains(err.Error(), "needs "+gb(need)+" free on its data disk") || !strings.Contains(err.Error(), "of the team's files") {
		t.Fatalf("upgrade: %v", err)
	}
	if h.importedNew() {
		t.Fatal("the refusal came after the import")
	}
	// df counts whole KiB.
	h.setGuest(func(vm *fakeBackend) { vm.guestAvail = (need + 1023) &^ 1023 })
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()); err != nil {
		t.Fatalf("upgrade with room: %v", err)
	}
}

// The import is measured once it is done: images that leave the disk
// short of the floor and headroom stop the upgrade before the org is
// touched, and are removed again, so the running org is not left under
// disk pressure by images nothing uses.
func TestUpgradeChecksTheDataDiskAgainAfterTheImport(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	h.setGuest(func(vm *fakeBackend) {
		vm.guestAvail = floorBytes + 2<<30
		vm.importCost = 2 << 30
	})
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	refusedForRoom(t, h, err, floorBytes, stillRunning)
	if h.importedNew() {
		t.Fatal("the new release's images were kept")
	}
	h.vm.mu.Lock()
	removed := slices.Sorted(slices.Values(h.vm.removed))
	h.vm.mu.Unlock()
	want := []string{"docker.io/library/kivali-agent-shell:v0.16.0", "docker.io/library/kivali-browser:v0.16.0", "docker.io/library/kivali-egress-proxy:v0.16.0", "docker.io/library/kivali:v0.16.0"}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
}

// A baked upgrade imports nothing itself (k3s imported the VM image's
// images as it started), but a disk already short of the floor and
// headroom is refused the same way, with nothing changed.
func TestBakedUpgradeChecksTheDataDisk(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.bakeRelease("0.16.0", "v0.16.0")
	h.setGuest(func(vm *fakeBackend) { vm.guestAvail = floorBytes + guestHeadroom - 1 })
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf())
	refusedForRoom(t, h, err, floorBytes+guestHeadroom-1, nothingChanged)
}

// A new release on a node under disk pressure is never scheduled; the
// wait sees the taint and rolls back at once, saying it is the disk,
// instead of waiting out the rollout timeout.
func TestUpgradeUnderDiskPressureRollsBackAtOnce(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.pending["v0.16.0"] = true
	// Boots: 1 install, 2 the upgrade, 3 the rollback.
	h.vm.onBoot = func(n int) { h.setGuest(func(vm *fakeBackend) { vm.diskTaint = n == 2 }) }
	// When the wait for the new release began and ended.
	var began, failed time.Time
	logf := func(f string, a ...any) {
		line := fmt.Sprintf(f, a...)
		switch {
		case strings.HasPrefix(line, "waiting for the server deployment to roll out at chart 0.16.0"):
			began = h.clock.Now()
		case strings.HasPrefix(line, "upgrade failed:"):
			failed = h.clock.Now()
		}
		h.logf()(f, a...)
	}
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")}, logf)
	if err == nil || !strings.Contains(err.Error(), "still on 0.15.0") || !strings.Contains(err.Error(), "data disk is too full to run on: Kubernetes reports the VM under disk pressure") {
		t.Fatalf("upgrade: %v", err)
	}
	if began.IsZero() || failed.IsZero() || failed.Sub(began) >= pollInterval {
		t.Fatalf("the wait for the new release began %v and failed %v; want it to fail at its first look", began, failed)
	}
}

// A rollback whose node comes up under disk pressure answers /readyz
// once and then has its server evicted: a taint, an evicted server pod,
// or free space under kubelet's floor is a failed start, and the error
// says it is the disk. Without it, the rollback reported success and the
// team stopped serving seconds later.
func TestRollbackUnderDiskPressureIsAFailedStart(t *testing.T) {
	for name, tc := range map[string]struct {
		press func(vm *fakeBackend)
		why   string
	}{
		"tainted":         {func(vm *fakeBackend) { vm.diskTaint = true }, "Kubernetes reports the VM under disk pressure"},
		"evicted":         {func(vm *fakeBackend) { vm.evicted = true }, "Kubernetes evicted the server (The node was low on resource: ephemeral-storage.)"},
		"below the floor": {func(vm *fakeBackend) { vm.guestAvail = floorBytes - 1 }, "its free space is below what Kubernetes keeps free"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.mustUp(UpOptions{Install: owner})
			h.vm.broken["v0.16.0"] = true
			h.vm.onBoot = func(n int) {
				if n == 3 {
					h.setGuest(tc.press)
				}
			}
			err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")}, h.logf())
			if err == nil || !strings.Contains(err.Error(), "the data disk was restored to 0.15.0, but starting it failed: the team's data disk is too full to run on: "+tc.why) {
				t.Fatalf("upgrade: %v", err)
			}
			if name == "below the floor" && !strings.Contains(err.Error(), "Free up "+gb(guestHeadroom+1)+" of the team's files") {
				t.Fatalf("upgrade: %v, want how much to free", err)
			}
			if st := h.sup.State(); st.Upgrade != nil || st.Kivali != "0.15.0" {
				t.Fatalf("state %+v", st)
			}
		})
	}
}

// A release's images exist only in the VM, so the upgrade pins the
// installed release's and the new release's against kubelet's image
// garbage collection before the snapshot, unpins the old release's once
// the upgrade commits, and the failed release's once it rolls back.
// Without it, a rollback onto a disk under pressure had the old
// release's images deleted, and neither release could start again.
func TestUpgradePinsTheImagesARollbackNeeds(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel16"), "0.16.0", "v0.16.0")}, h.logf()); err != nil {
		t.Fatalf("upgrade to 0.16.0: %v", err)
	}
	refs16 := []string{"docker.io/library/kivali-agent-shell:v0.16.0", "docker.io/library/kivali-browser:v0.16.0", "docker.io/library/kivali-egress-proxy:v0.16.0", "docker.io/library/kivali:v0.16.0"}
	if got := h.vm.pinnedImages(); !slices.Equal(got, refs16) {
		t.Fatalf("pinned after the commit %v, want %v", got, refs16)
	}
	h.vm.requireImages = true
	h.vm.broken["v0.17.0"] = true
	// Boots: 1 install, 2 and 3 the upgrades, 4 the rollback, onto a
	// disk under pressure.
	h.vm.onBoot = func(n int) {
		if n == 4 {
			h.vm.collectImages()
		}
	}
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel17"), "0.17.0", "v0.17.0")}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "still on 0.16.0") {
		t.Fatalf("upgrade to 0.17.0: %v", err)
	}
	if got := h.vm.pinnedImages(); !slices.Equal(got, refs16) {
		t.Fatalf("pinned after the rollback %v, want %v", got, refs16)
	}
}

// An org whose images kubelet already deleted cannot start, and the
// preflight would refuse it as unhealthy; when the image it cannot get
// is its own release's and containerd no longer has it, the upgrade
// goes ahead and brings the new release's.
func TestUpgradeProceedsWhenTheOrgsImagesAreGone(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel16"), "0.16.0", "v0.16.0")}, h.logf()); err != nil {
		t.Fatalf("upgrade to 0.16.0: %v", err)
	}
	h.vm.requireImages = true
	h.setGuest(func(vm *fakeBackend) { delete(vm.imported, "docker.io/library/kivali:v0.16.0") })
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel17"), "0.17.0", "v0.17.0")}, h.logf()); err != nil {
		t.Fatalf("upgrade to 0.17.0: %v", err)
	}
	if st := h.sup.State(); st.Kivali != "0.17.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
}

// An image the org cannot get while containerd has it (a broken
// release) still fails the preflight: the upgrade goes ahead only for
// images that are gone.
func TestUpgradePreflightRefusesAnImageErrorWithTheImagePresent(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.broken["v0.15.0"] = true
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "preflight: the org is not healthy") || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("upgrade: %v", err)
	}
}
