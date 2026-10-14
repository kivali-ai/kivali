package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// keeperBackend is the fake backend as Hyper-V's is: the data disk is
// kept where only the backend changes it (DataDiskKeeper), and an
// earlier version kept it at a legacy path in the config directory, from
// which up moves it (DataDiskMigrator). It records what it is asked to
// do, with how many boots there had been and whether every VM was off.
type keeperBackend struct {
	*fakeBackend
	mu  sync.Mutex
	ops []keeperOp
	// importErr fails the import; importProgress is what it reports as
	// it copies.
	importErr      error
	importProgress [][2]int64
}

type keeperOp struct {
	op         string
	boots      int
	allStopped bool
}

var (
	_ DataDiskMigrator = (*keeperBackend)(nil)
	_ DataDiskKeeper   = (*keeperBackend)(nil)
)

// LegacyDataDiskPath is a file in the config directory, as Hyper-V's
// data.vhdx was.
func (k *keeperBackend) LegacyDataDiskPath(configDir string) string {
	return filepath.Join(configDir, "data.legacy")
}

func (k *keeperBackend) record(op string) {
	k.fakeBackend.mu.Lock()
	stopped := true
	for _, m := range k.machines {
		stopped = stopped && !m.running()
	}
	boots := len(k.boots)
	k.fakeBackend.mu.Unlock()
	k.mu.Lock()
	k.ops = append(k.ops, keeperOp{op: op, boots: boots, allStopped: stopped})
	k.mu.Unlock()
}

func (k *keeperBackend) opNames() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var names []string
	for _, o := range k.ops {
		names = append(names, o.op)
	}
	return names
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// ImportDataDisk copies the legacy disk into place, as the broker does,
// never over a disk already there.
func (k *keeperBackend) ImportDataDisk(_ context.Context, configDir string, progress Progress, _ Logf) error {
	k.record("import")
	k.mu.Lock()
	err, steps := k.importErr, k.importProgress
	k.mu.Unlock()
	if err != nil {
		return err
	}
	data := k.DataDiskPath(configDir)
	if exists(data) {
		return ErrAlreadyImported
	}
	for _, s := range steps {
		progress(s[0], s[1])
	}
	return copyFile(k.LegacyDataDiskPath(configDir), data)
}

func (k *keeperBackend) SnapshotDataDisk(_ context.Context, configDir string) error {
	k.record("snapshot")
	p := k.DataDiskPath(configDir)
	return copyFile(p, p+SnapshotSuffix)
}

func (k *keeperBackend) RestoreDataDiskSnapshot(_ context.Context, configDir string) error {
	k.record("restore")
	p := k.DataDiskPath(configDir)
	return os.Rename(p+SnapshotSuffix, p)
}

func (k *keeperBackend) DeleteDataDiskSnapshot(_ context.Context, configDir string) error {
	k.record("delete-snapshot")
	return removeIfExists(k.DataDiskPath(configDir) + SnapshotSuffix)
}

func (k *keeperBackend) DeleteDataDisk(_ context.Context, configDir string) (int64, error) {
	k.record("delete")
	var freed int64
	p := k.DataDiskPath(configDir)
	for _, f := range []string{p, p + SnapshotSuffix} {
		if fi, err := os.Stat(f); err == nil {
			freed += fi.Size()
		}
		if err := removeIfExists(f); err != nil {
			return freed, err
		}
	}
	_ = os.Remove(filepath.Dir(p))
	return freed, nil
}

// keeperHarness is a harness whose backend keeps the data disk itself.
func keeperHarness(t *testing.T) (*harness, *keeperBackend) {
	h := newHarness(t)
	k := &keeperBackend{fakeBackend: h.vm}
	h.backend = k
	h.restart()
	return h, k
}

// legacyOrg is an org an earlier version ran: up, installed and down,
// its data disk then at the legacy path (as Hyper-V's data.vhdx sat in
// the config directory). It returns the disk's content.
func legacyOrg(t *testing.T) (*harness, *keeperBackend, diskFS) {
	t.Helper()
	h, k := keeperHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	fs := h.disk()
	legacy := k.LegacyDataDiskPath(h.dir)
	if err := os.Rename(h.dataPath(), legacy); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	k.ops = nil
	k.mu.Unlock()
	h.restart()
	return h, k, fs
}

// The first up of this version moves the data disk before anything
// boots: the stage is updating-data, the import runs with every VM off
// and before the compaction and the boot, the legacy file is set aside
// and deleted once the moved disk has booted to READY, and the org on
// it is the one that was there.
func TestUpMovesTheDataDiskFirst(t *testing.T) {
	h, k, before := legacyOrg(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	ctx, l := newStageLog()
	if err := h.sup.Up(ctx, UpOptions{Port: h.port}, l.logf); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got, want := l.sequence(), []string{StageUpdatingData, StageStarting, StageSettingUp}; !slices.Equal(got, want) {
		t.Fatalf("stages %v, want %v\n%s", got, want, strings.Join(l.lines, "\n"))
	}
	k.mu.Lock()
	ops := slices.Clone(k.ops)
	k.mu.Unlock()
	if len(ops) != 1 || ops[0].op != "import" || !ops[0].allStopped || ops[0].boots != 1 {
		t.Fatalf("ops = %+v, want one import before the second boot, every VM off", ops)
	}
	if c := h.vm.compacted(); len(c) < 2 || c[len(c)-1].boots != 1 {
		t.Fatalf("compactions %+v: the moved disk was not compacted before its boot", c)
	}
	if exists(legacy) || exists(legacy+MigratedSuffix) {
		t.Fatalf("legacy %v, set aside %v: both should be gone", exists(legacy), exists(legacy+MigratedSuffix))
	}
	if got := h.disk(); got[ManifestRel] != before[ManifestRel] {
		t.Fatalf("the org on the moved disk is not the one that was there")
	}
	if h.sup.State().Kivali == "" {
		t.Fatal("not installed after the move")
	}
	// The next up has nothing to move.
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	h.mustUp(UpOptions{})
	if ops := k.opNames(); !slices.Equal(ops, []string{"import"}) {
		t.Fatalf("ops after another up = %v", ops)
	}
}

// An import that fails changes nothing: the legacy file is where it
// was, nothing booted, and the next up moves it.
func TestMoveFailsBeforeTheCopyIsInPlace(t *testing.T) {
	h, k, before := legacyOrg(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	boots := len(h.vm.boots)
	k.importErr = errors.New("broker: copy C:\\Users\\a\\data.vhdx: the device is not ready (500)")
	err := h.up(UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "the device is not ready") || !strings.Contains(err.Error(), legacy+" is unchanged") {
		t.Fatalf("up: %v", err)
	}
	if !exists(legacy) || exists(legacy+MigratedSuffix) || exists(h.dataPath()) || len(h.vm.boots) != boots {
		t.Fatalf("after the failure: legacy %v, set aside %v, data %v, boots %d (was %d)",
			exists(legacy), exists(legacy+MigratedSuffix), exists(h.dataPath()), len(h.vm.boots), boots)
	}
	k.importErr = nil
	h.mustUp(UpOptions{})
	if got := h.disk(); got[ManifestRel] != before[ManifestRel] || exists(legacy) {
		t.Fatal("the retry did not move the disk")
	}
}

// The copy is in place but the legacy file was not set aside (the up
// stopped there): the next up's import says it is already done, and the
// up goes on from there.
func TestMoveStoppedAfterTheCopy(t *testing.T) {
	h, k, before := legacyOrg(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	h.mu.Lock()
	h.renameErr = func(oldpath, _ string) error {
		if oldpath == legacy {
			return errors.New("the process was killed")
		}
		return nil
	}
	h.mu.Unlock()
	if err := h.up(UpOptions{}); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("up: %v", err)
	}
	if !exists(legacy) || !exists(h.dataPath()) {
		t.Fatalf("legacy %v, data %v: want both", exists(legacy), exists(h.dataPath()))
	}
	h.mu.Lock()
	h.renameErr = nil
	h.mu.Unlock()
	h.restart()
	h.mustUp(UpOptions{})
	if ops := k.opNames(); !slices.Equal(ops, []string{"import", "import"}) {
		t.Fatalf("ops = %v", ops)
	}
	if exists(legacy) || exists(legacy+MigratedSuffix) || h.disk()[ManifestRel] != before[ManifestRel] {
		t.Fatal("the second up did not finish the move")
	}
}

// The legacy file is set aside but the moved disk never booted to
// READY: the set-aside copy stays, the next up boots the moved disk
// without moving anything, and deletes the copy once it is READY. A
// set-aside copy with no legacy file (an up that stopped after the boot)
// goes the same way.
func TestMoveStoppedBeforeTheMovedDiskBooted(t *testing.T) {
	h, k, _ := legacyOrg(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	h.vm.mu.Lock()
	h.vm.fatalOnBoot = "boot: k3s did not start"
	h.vm.mu.Unlock()
	if err := h.up(UpOptions{}); err == nil {
		t.Fatal("up succeeded with a failing boot")
	}
	if exists(legacy) || !exists(legacy+MigratedSuffix) || !exists(h.dataPath()) {
		t.Fatalf("legacy %v, set aside %v, data %v", exists(legacy), exists(legacy+MigratedSuffix), exists(h.dataPath()))
	}
	h.mustUp(UpOptions{})
	if exists(legacy + MigratedSuffix) {
		t.Fatal("the set-aside copy outlived a READY")
	}
	if ops := k.opNames(); !slices.Equal(ops, []string{"import"}) {
		t.Fatalf("ops = %v: the second up moved again", ops)
	}

	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy+MigratedSuffix, []byte("left"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustUp(UpOptions{})
	if exists(legacy + MigratedSuffix) {
		t.Fatal("a set-aside copy left after a boot was not deleted at the next READY")
	}
}

// A volume short of room for the copy (and 2 GiB beside it) refuses the
// up with what to free and where, and nothing changes.
func TestMoveNeedsRoom(t *testing.T) {
	h, k, _ := legacyOrg(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	boots := len(h.vm.boots)
	h.mu.Lock()
	h.freeBytes = func(string) (uint64, error) { return 1 << 30, nil }
	h.mu.Unlock()
	err := h.up(UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "only 1.0 GB is free") || !strings.Contains(err.Error(), "Free up") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("up: %v", err)
	}
	if len(k.opNames()) != 0 || !exists(legacy) || exists(legacy+MigratedSuffix) || exists(h.dataPath()) || len(h.vm.boots) != boots {
		t.Fatalf("something changed: ops %v, legacy %v, data %v, boots %d", k.opNames(), exists(legacy), exists(h.dataPath()), len(h.vm.boots))
	}
	h.mu.Lock()
	dirs := slices.Clone(h.freeDirs)
	h.mu.Unlock()
	// Measured where the disk goes (the nearest directory that exists),
	// not the config directory's volume.
	if len(dirs) == 0 || !strings.HasPrefix(filepath.Dir(h.dataPath()), dirs[len(dirs)-1]) {
		t.Fatalf("free space measured in %v, want the data disk's directory %s or above", dirs, filepath.Dir(h.dataPath()))
	}
}

// A legacy disk the backend refuses to import (a differencing disk, say)
// fails the up with the backend's reason, and the disk stays.
func TestMoveOfAnInvalidDiskIsRefused(t *testing.T) {
	h, k, _ := legacyOrg(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	k.importErr = errors.New("broker: refused: the data disk to import: not a dynamic VHDX: a differencing disk (403)")
	err := h.up(UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "differencing") || !strings.Contains(err.Error(), legacy) {
		t.Fatalf("up: %v", err)
	}
	if !exists(legacy) || exists(h.dataPath()) {
		t.Fatal("the refused disk was not kept as it was")
	}
}

// An upgrade interrupted by an earlier version, its snapshot beside the
// legacy disk, is rolled back there first, the Host's way; then the
// rolled-back disk is the one moved.
func TestLegacyJournalIsRecoveredBeforeTheMove(t *testing.T) {
	h, k, before := legacyOrg(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	snap := legacy + SnapshotSuffix
	if err := copyFile(legacy, snap); err != nil {
		t.Fatal(err)
	}
	half := diskFS{}
	for k, v := range before {
		half[k] = v
	}
	half[ManifestRel] = "half-applied"
	if err := writeDisk(legacy, half); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.update(func(st *State) {
		st.Upgrade = &Journal{From: "0.15.0", To: "0.16.0", Step: StepApplying, Snapshot: snap, SnapshotComplete: true}
	}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.mu.Lock()
	h.renamed = nil
	h.mu.Unlock()
	h.mustUp(UpOptions{})
	h.mu.Lock()
	renamed := slices.Clone(h.renamed)
	h.mu.Unlock()
	if !slices.Contains(renamed, legacy) {
		t.Fatalf("Host renames %v: the snapshot was not put back over the legacy disk", renamed)
	}
	if ops := k.opNames(); !slices.Equal(ops, []string{"import"}) {
		t.Fatalf("ops = %v: the recovery went through the backend", ops)
	}
	if h.disk()[ManifestRel] != before[ManifestRel] || exists(snap) || h.sup.State().Upgrade != nil {
		t.Fatalf("after recovery and the move: manifest %q, snapshot %v, journal %+v", h.disk()[ManifestRel], exists(snap), h.sup.State().Upgrade)
	}
}

// A legacy disk whose first boot never reached READY is disposable: it
// is set aside, not copied, and a new disk is made in its place.
func TestMoveOfADisposableDisk(t *testing.T) {
	h, k := keeperHarness(t)
	legacy := k.LegacyDataDiskPath(h.dir)
	if err := os.WriteFile(legacy, make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.update(func(st *State) { st.DataDiskFormatted = false }); err != nil {
		t.Fatal(err)
	}
	h.mustUp(UpOptions{Install: owner})
	if ops := k.opNames(); len(ops) != 0 {
		t.Fatalf("ops = %v: a disposable disk was copied", ops)
	}
	if exists(legacy) || exists(legacy+MigratedSuffix) || !h.vm.lastFormat() {
		t.Fatalf("legacy %v, set aside %v, formatted %v", exists(legacy), exists(legacy+MigratedSuffix), h.vm.lastFormat())
	}
}

// With no local.json, a disk at the legacy path counts as formatted, as
// one where the backend keeps it does: it is moved, not made again.
func TestALegacyDiskWithoutLocalJSONIsKept(t *testing.T) {
	h, k, before := legacyOrg(t)
	if err := os.Remove(filepath.Join(h.dir, "local.json")); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if !h.sup.State().DataDiskFormatted {
		t.Fatal("a legacy disk did not count as formatted")
	}
	h.mustUp(UpOptions{Install: owner})
	if ops := k.opNames(); !slices.Equal(ops, []string{"import"}) || h.disk()[ManifestRel] != before[ManifestRel] {
		t.Fatalf("ops %v; the disk was not moved as it was", ops)
	}
}

// Where the backend keeps the data disk, the upgrade's snapshot, its
// cleanup and its rollback go through it, never the Host's copy or
// rename of the disk; so does destroy's deletion, and destroy deletes
// whatever an earlier version left at the legacy path too.
func TestUpgradeAndDestroyGoThroughTheKeeper(t *testing.T) {
	h, k := keeperHarness(t)
	hostSnapshots := 0
	h.snapshot = func(string, string) error { hostSnapshots++; return errors.New("the Host's snapshot was used") }
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "a"), "0.16.0", "v0.16.0")}, h.logf()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if ops := k.opNames(); !slices.Equal(ops, []string{"snapshot", "delete-snapshot"}) {
		t.Fatalf("ops = %v", ops)
	}
	before := h.disk()[ManifestRel]
	h.vm.broken["v0.17.0"] = true
	h.mu.Lock()
	h.renamed = nil
	h.mu.Unlock()
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "b"), "0.17.0", "v0.17.0")}, h.logf()); err == nil {
		t.Fatal("broken upgrade succeeded")
	}
	if ops := k.opNames(); !slices.Equal(ops, []string{"snapshot", "delete-snapshot", "snapshot", "restore"}) {
		t.Fatalf("ops = %v", ops)
	}
	h.mu.Lock()
	renamed := slices.Clone(h.renamed)
	h.mu.Unlock()
	if slices.Contains(renamed, h.dataPath()) || hostSnapshots != 0 {
		t.Fatalf("the Host renamed %v and snapshotted %d times", renamed, hostSnapshots)
	}
	if h.disk()[ManifestRel] != before {
		t.Fatal("the rollback did not restore the disk")
	}
	// The snapshot and the restore run with the VM off; the snapshot is
	// deleted with the new release up, the VM never having opened it.
	k.mu.Lock()
	for _, o := range k.ops {
		if !o.allStopped && o.op != "delete-snapshot" {
			k.mu.Unlock()
			t.Fatalf("%s ran with a VM running", o.op)
		}
	}
	k.mu.Unlock()

	legacy := k.LegacyDataDiskPath(h.dir)
	for _, p := range []string{legacy + MigratedSuffix, legacy + SnapshotSuffix} {
		if err := os.WriteFile(p, []byte("left"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := h.sup.Destroy(context.Background(), h.logf())
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if ops := k.opNames(); ops[len(ops)-1] != "delete" {
		t.Fatalf("ops = %v", ops)
	}
	for _, p := range []string{h.dataPath(), legacy + MigratedSuffix, legacy + SnapshotSuffix} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if res.FreedBytes == 0 {
		t.Error("destroy freed nothing")
	}
}

// Over the RPC, the import's and the compaction's progress arrive as
// events of their own, with done and total, in their stages, and the
// lines of the stage after a report carry the last one; a new stage
// starts with none.
func TestRPCEventsCarryProgress(t *testing.T) {
	h, k, _ := legacyOrg(t)
	k.importProgress = [][2]int64{{0, 8 << 30}, {3 << 30, 8 << 30}, {8 << 30, 8 << 30}}
	h.vm.compactProgress = [][2]int64{{40, 100}}
	srv := httptest.NewServer(NewRPCServer(h.sup).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/up", "application/json", strings.NewReader(fmt.Sprintf(`{"port":%d}`, h.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var events []Event
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			if ev.Error != "" {
				t.Fatalf("up: %s", ev.Error)
			}
			break
		}
		events = append(events, ev)
	}
	var reports []string
	for _, ev := range events {
		if ev.Log == "" {
			if ev.Progress == nil {
				t.Fatalf("an event with neither a line nor progress: %+v", ev)
			}
			reports = append(reports, fmt.Sprintf("%s %d/%d", ev.Stage, ev.Progress.Done, ev.Progress.Total))
		}
	}
	want := []string{
		"updating-data 0/8589934592", "updating-data 3221225472/8589934592", "updating-data 8589934592/8589934592",
		"making-room 40/100",
	}
	if !slices.Equal(reports, want) {
		t.Fatalf("progress events %v, want %v", reports, want)
	}
	for _, ev := range events {
		if ev.Log == "" {
			continue
		}
		switch {
		case strings.HasPrefix(ev.Log, "the data disk is in its new place"):
			if ev.Stage != StageUpdatingData || ev.Progress == nil || ev.Progress.Done != 8<<30 {
				t.Errorf("the line after the copy: %+v", ev)
			}
		case ev.Stage == StageStarting || ev.Stage == StageSettingUp:
			if ev.Progress != nil {
				t.Errorf("a line of a stage with no progress carries some: %+v", ev)
			}
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}
