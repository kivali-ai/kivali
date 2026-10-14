package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// legacyData is where the data disk was kept before the broker kept it:
// data.vhdx in the config directory, the caller's own file.
var legacyData = winJoin(testCfg, DataDiskName)

// testImport is the copy an import writes before it becomes the data
// disk.
var testImport = winJoin(vmDirFor(testCfg), importName)

// importable sets up the caller's legacy data disk with content (nil:
// a dynamic VHDX) in a config directory the caller can write.
func (h *harness) importable(content []byte) {
	h.caller.dirs[testCfg] = true
	h.caller.files[legacyData] = fakeEntry{read: true, content: content}
}

// The caller's legacy data disk is read through the caller's own pinned
// handle, copied into the VM directory as import.vhdx, checked there,
// and renamed to data.vhdx, never over one; the copy is the broker's own
// file, and nothing of the caller's is changed.
func TestImportVHD(t *testing.T) {
	h := newHarness(t)
	h.importable(nil)
	h.ownVM(StateOff)
	res, err := h.client.ImportVHD(context.Background(), testCfg)
	if err != nil {
		t.Fatalf("ImportVHD: %v", err)
	}
	if res.Path != testData || res.CopiedBytes != int64(len(dynamicDisk)) {
		t.Fatalf("result = %+v", res)
	}
	if b, ok := h.files.disk(testData); !ok || !bytes.Equal(b, dynamicDisk) {
		t.Fatal("the data disk is not the caller's disk's copy")
	}
	if _, ok := h.files.disk(testImport); ok {
		t.Fatal("the copy is still there under its own name")
	}
	if len(h.caller.pinned) != 1 || h.caller.pinned[0] != legacyData {
		t.Fatalf("the caller's files opened: %v, want %s", h.caller.pinned, legacyData)
	}
	if len(h.files.copies) != 1 || h.files.copies[0].dst != testImport {
		t.Fatalf("copies = %+v", h.files.copies)
	}
	if len(h.files.checked) != 1 || h.files.checked[0] != testImport {
		t.Fatalf("checked = %v, want the copy", h.files.checked)
	}
	if r := (fileRename{testImport, testData, false}); len(h.files.renames) != 1 || h.files.renames[0] != r {
		t.Fatalf("renames = %+v, want %+v", h.files.renames, r)
	}
	if p, err := h.client.VHDProgress(context.Background(), testCfg); err != nil || p != (VHDProgress{}) {
		t.Fatalf("progress after the import = %+v, %v", p, err)
	}
}

// An import is refused, and nothing is copied, for a running VM, another
// account's VM or VM directory, a source the caller cannot read, and a
// source that is anything but a dynamic VHDX with no parent (read
// through the caller's handle: a differencing disk's parent is never
// opened).
func TestImportVHDRefusals(t *testing.T) {
	differencing := vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: vhdx.FlagHasParent}.Bytes()
	fixed := vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: vhdx.FlagLeaveBlocksAllocated}.Bytes()
	huge := vhdx.Spec{BlockSize: 1 << 20, SectorSize: 512, VirtualSize: MaxDiskBytes + 1<<20}.Bytes()
	for _, tc := range []struct {
		what    string
		content []byte
		setup   func(h *harness)
		status  int
		want    string
	}{
		{"a running VM", nil, func(h *harness) { h.ownVM(StateRunning) }, http.StatusConflict, "Running"},
		{"another account's VM", nil, func(h *harness) { h.ownVM(StateOff).Notes = OwnerNotes("S-1-5-21-111-222-333-2002") }, http.StatusForbidden, "another account"},
		{"another account's VM directory", nil, func(h *harness) {
			_, _, _ = h.files.VMDir(host.DirKey(testCfg), "S-1-5-21-111-222-333-2002")
		}, http.StatusForbidden, "another account"},
		{"an unreadable source", nil, func(h *harness) { h.caller.files[legacyData] = fakeEntry{} }, http.StatusForbidden, "access denied"},
		{"no source", nil, func(h *harness) { delete(h.caller.files, legacyData) }, http.StatusForbidden, "does not exist"},
		{"a differencing source", differencing, func(*harness) {}, http.StatusForbidden, "differencing"},
		{"a fixed source", fixed, func(*harness) {}, http.StatusForbidden, "fixed"},
		{"a source that is no VHDX", make([]byte, 3<<20), func(*harness) {}, http.StatusForbidden, "not a VHDX"},
		{"a source larger than any disk the broker makes", huge, func(*harness) {}, http.StatusForbidden, "virtual disk size"},
	} {
		h := newHarness(t)
		h.importable(tc.content)
		tc.setup(h)
		_, err := h.client.ImportVHD(context.Background(), testCfg)
		if statusOf(err) != tc.status || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v (status %d), want %d with %q", tc.what, err, statusOf(err), tc.status, tc.want)
		}
		if len(h.files.copies) != 0 || len(h.files.renames) != 0 {
			t.Errorf("%s: copies %+v, renames %+v", tc.what, h.files.copies, h.files.renames)
		}
		if _, ok := h.files.disk(testData); ok {
			t.Errorf("%s: a data disk was made", tc.what)
		}
	}
}

// A data disk already in the VM directory is an import that finished:
// 409 AlreadyImported, before the caller's file is opened; and one that
// lands while the copy runs makes the rename fail the same way, the copy
// deleted.
func TestImportVHDAlreadyImported(t *testing.T) {
	h := newHarness(t)
	h.importable(nil)
	h.files.putDisk(testData, []byte("imported"))
	_, err := h.client.ImportVHD(context.Background(), testCfg)
	if !IsAlreadyImported(err) {
		t.Fatalf("err = %v (status %d), want already-imported", err, statusOf(err))
	}
	if len(h.caller.pinned) != 0 || len(h.files.copies) != 0 {
		t.Fatalf("opened %v, copied %+v", h.caller.pinned, h.files.copies)
	}

	h = newHarness(t)
	h.importable(nil)
	h.files.copyHook = func(context.Context, *Meter) error {
		h.files.putDisk(testData, []byte("imported"))
		return nil
	}
	_, err = h.client.ImportVHD(context.Background(), testCfg)
	if !IsAlreadyImported(err) {
		t.Fatalf("err = %v (status %d), want already-imported", err, statusOf(err))
	}
	if b, _ := h.files.disk(testData); string(b) != "imported" {
		t.Fatalf("the data disk was replaced: %q", b)
	}
	if _, ok := h.files.disk(testImport); ok {
		t.Fatal("the copy was left behind")
	}
	if IsAlreadyImported(&StatusError{Status: http.StatusConflict, Msg: "kivali-1 is Running; its data disk is changed only while it is off"}) {
		t.Fatal("another conflict reads as already-imported")
	}
}

// A copy an earlier import left (it was cut short) is deleted before the
// new copy, which then succeeds.
func TestImportVHDClearsALeftoverCopy(t *testing.T) {
	h := newHarness(t)
	h.importable(nil)
	h.files.putDisk(testImport, []byte("half a copy"))
	if _, err := h.client.ImportVHD(context.Background(), testCfg); err != nil {
		t.Fatalf("ImportVHD: %v", err)
	}
	if len(h.files.deleted) == 0 || h.files.deleted[0] != testImport {
		t.Fatalf("deleted = %v, want the leftover copy first", h.files.deleted)
	}
	if b, _ := h.files.disk(testData); !bytes.Equal(b, dynamicDisk) {
		t.Fatal("the data disk is not the caller's disk's copy")
	}
}

// The copy is checked again in the VM directory, where the caller cannot
// change it: refused there, it is deleted and no data disk is made.
func TestImportVHDChecksTheCopy(t *testing.T) {
	h := newHarness(t)
	h.importable(nil)
	h.files.checkErr = fmt.Errorf("%s: %w: a differencing disk", testImport, errNotDynamic)
	_, err := h.client.ImportVHD(context.Background(), testCfg)
	if statusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), "differencing") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if _, ok := h.files.disk(testImport); ok {
		t.Fatal("the refused copy was left behind")
	}
	if _, ok := h.files.disk(testData); ok || len(h.files.renames) != 0 {
		t.Fatalf("a data disk was made (renames %+v)", h.files.renames)
	}
}

// An import whose client goes away (the supervisor's up was cancelled)
// stops its copy and deletes it.
func TestImportVHDCancelledDeletesTheCopy(t *testing.T) {
	h := newHarness(t)
	h.importable(nil)
	entered := make(chan struct{})
	h.files.copyHook = func(ctx context.Context, _ *Meter) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	finished := h.finishedOn()
	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan error, 1)
	go func() { _, err := h.client.ImportVHD(ctx, testCfg); answered <- err }()
	<-entered
	cancel()
	if err := <-answered; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	for p := range finished {
		if p == "/v1/vhd/import" {
			break
		}
	}
	if _, ok := h.files.disk(testImport); ok {
		t.Fatal("the cancelled copy was left behind")
	}
	if _, ok := h.files.disk(testData); ok {
		t.Fatal("a data disk was made")
	}
	if p, err := h.client.VHDProgress(context.Background(), testCfg); err != nil || p.Op != "" {
		t.Fatalf("progress after the cancelled import = %+v, %v", p, err)
	}
}

// While a copy runs, POST /v1/vhd/progress says how far it has got,
// without waiting for the VM's lock the import holds, and answers only
// the account whose import it is; once it ends there is no operation.
func TestVHDProgressDuringAnImport(t *testing.T) {
	h := newHarness(t)
	h.importable(nil)
	size := int64(len(dynamicDisk))
	entered, release := make(chan struct{}), make(chan struct{})
	h.files.copyHook = func(_ context.Context, m *Meter) error {
		m.Set(size/2, size)
		close(entered)
		<-release
		return nil
	}
	var mu sync.Mutex
	other := &fakeCaller{sid: "S-1-5-21-111-222-333-2002", dirs: map[string]bool{testCfg: true}}
	asOther := false
	h.server.CallerOf = func(r *http.Request) (Caller, error) {
		mu.Lock()
		defer mu.Unlock()
		if asOther && r.URL.Path == "/v1/vhd/progress" {
			return other, nil
		}
		return h.caller, nil
	}
	done := make(chan error, 1)
	go func() { _, err := h.client.ImportVHD(context.Background(), testCfg); done <- err }()
	<-entered
	p, err := h.client.VHDProgress(context.Background(), testCfg)
	if err != nil || p != (VHDProgress{Op: OpImport, Done: size / 2, Total: size}) {
		t.Fatalf("progress mid-copy = %+v, %v", p, err)
	}
	mu.Lock()
	asOther = true
	mu.Unlock()
	if _, err := h.client.VHDProgress(context.Background(), testCfg); statusOf(err) != http.StatusForbidden {
		t.Fatalf("another account's progress query: err = %v (status %d)", err, statusOf(err))
	}
	mu.Lock()
	asOther = false
	mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("ImportVHD: %v", err)
	}
	if p, err := h.client.VHDProgress(context.Background(), testCfg); err != nil || p != (VHDProgress{}) {
		t.Fatalf("progress after = %+v, %v", p, err)
	}
}

// A compaction's progress is read from the operation on demand.
func TestVHDProgressDuringACompaction(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	h.files.putDisk(testData, dynamicDisk)
	entered, release := make(chan struct{}), make(chan struct{})
	h.files.compactHook = func(m *Meter) {
		m.Reader(func() (int64, int64) { return 40, 100 })
		close(entered)
		<-release
		m.Reader(nil)
	}
	done := make(chan error, 1)
	go func() { _, err := h.client.OptimizeVHD(context.Background(), testCfg); done <- err }()
	<-entered
	p, err := h.client.VHDProgress(context.Background(), testCfg)
	close(release)
	if err != nil || p != (VHDProgress{Op: OpCompact, Done: 40, Total: 100}) {
		t.Fatalf("progress mid-compaction = %+v, %v", p, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// With nothing running there is no operation; a config directory the
// caller cannot write is refused.
func TestVHDProgressIdleAndRefused(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	if p, err := h.client.VHDProgress(context.Background(), testCfg); err != nil || p != (VHDProgress{}) {
		t.Fatalf("idle progress = %+v, %v", p, err)
	}
	h.caller.dirs[testCfg] = false
	if _, err := h.client.VHDProgress(context.Background(), testCfg); statusOf(err) != http.StatusForbidden {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if h.runner.count() != 0 {
		t.Errorf("a progress query ran %d scripts", h.runner.count())
	}
}

var testSnapshot = winJoin(vmDirFor(testCfg), SnapshotName)

// The snapshot is a copy of the data disk beside it, replacing an
// earlier one; a restore renames it over the data disk, durably and
// replacing it; a snapshot is deleted on its own.
func TestSnapshotRestoreDelete(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	h.ownVM(StateOff)
	h.files.putDisk(testData, []byte("before the upgrade"))
	h.files.putDisk(testSnapshot, []byte("an older snapshot"))
	res, err := h.client.SnapshotVHD(context.Background(), testCfg)
	if err != nil {
		t.Fatalf("SnapshotVHD: %v", err)
	}
	if b, _ := h.files.disk(testSnapshot); string(b) != "before the upgrade" || res.CopiedBytes != int64(len(b)) {
		t.Fatalf("snapshot = %q, result %+v", b, res)
	}
	h.files.putDisk(testData, []byte("half-applied"))
	if err := h.client.RestoreVHD(context.Background(), testCfg); err != nil {
		t.Fatalf("RestoreVHD: %v", err)
	}
	if b, _ := h.files.disk(testData); string(b) != "before the upgrade" {
		t.Fatalf("data disk after the restore = %q", b)
	}
	if _, ok := h.files.disk(testSnapshot); ok {
		t.Fatal("the snapshot is still there after the restore")
	}
	if r := h.files.renames[len(h.files.renames)-1]; r != (fileRename{testSnapshot, testData, true}) {
		t.Fatalf("restore rename = %+v", r)
	}

	h.files.putDisk(testSnapshot, []byte("snap"))
	d, err := h.client.DeleteSnapshot(context.Background(), testCfg)
	if err != nil || d.FreedBytes != 4 {
		t.Fatalf("DeleteSnapshot = %+v, %v", d, err)
	}
	if _, ok := h.files.disk(testSnapshot); ok {
		t.Fatal("the snapshot is still there")
	}
	if _, ok := h.files.disk(testData); !ok {
		t.Fatal("the data disk went with the snapshot")
	}
}

// A restore with no snapshot changes nothing; neither does any of the
// three for another account's VM or VM directory, nor a snapshot or a
// restore while the VM runs. A snapshot, which the VM never opens, is
// deleted while it runs (the upgrade's cleanup, with the new release
// up).
func TestSnapshotRefusals(t *testing.T) {
	calls := map[string]func(h *harness) error{
		"snapshot": func(h *harness) error { _, err := h.client.SnapshotVHD(context.Background(), testCfg); return err },
		"restore":  func(h *harness) error { return h.client.RestoreVHD(context.Background(), testCfg) },
		"delete":   func(h *harness) error { _, err := h.client.DeleteSnapshot(context.Background(), testCfg); return err },
	}
	for name, call := range calls {
		for _, tc := range []struct {
			what   string
			setup  func(h *harness)
			status int
		}{
			{"a running VM", func(h *harness) { h.ownVM(StateRunning) }, map[bool]int{true: 0, false: http.StatusConflict}[name == "delete"]},
			{"another account's VM", func(h *harness) { h.ownVM(StateOff).Notes = OwnerNotes("S-1-5-21-111-222-333-2002") }, http.StatusForbidden},
			{"another account's VM directory", func(h *harness) {
				h.files.dirAccount[host.DirKey(testCfg)] = "S-1-5-21-111-222-333-2002"
			}, http.StatusForbidden},
		} {
			h := newHarness(t)
			h.caller.dirs[testCfg] = true
			h.files.putDisk(testData, []byte("data"))
			h.files.putDisk(testSnapshot, []byte("snap"))
			tc.setup(h)
			if err := call(h); statusOf(err) != tc.status {
				t.Errorf("%s, %s: err = %v (status %d), want %d", name, tc.what, err, statusOf(err), tc.status)
			}
			if d, _ := h.files.disk(testData); string(d) != "data" {
				t.Errorf("%s, %s: the data disk changed", name, tc.what)
			}
			s, there := h.files.disk(testSnapshot)
			if tc.status == 0 {
				if there {
					t.Errorf("%s, %s: the snapshot is still there", name, tc.what)
				}
			} else if string(s) != "snap" {
				t.Errorf("%s, %s: the snapshot changed", name, tc.what)
			}
		}
	}
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	h.files.putDisk(testData, []byte("data"))
	if err := h.client.RestoreVHD(context.Background(), testCfg); statusOf(err) != http.StatusNotFound {
		t.Fatalf("restore without a snapshot: err = %v (status %d)", err, statusOf(err))
	}
	if d, _ := h.files.disk(testData); string(d) != "data" || len(h.files.renames) != 0 {
		t.Fatal("a restore without a snapshot changed the data disk")
	}
	h.files.drop(testData)
	if _, err := h.client.SnapshotVHD(context.Background(), testCfg); statusOf(err) != http.StatusNotFound {
		t.Fatalf("snapshot without a data disk: err = %v (status %d)", err, statusOf(err))
	}
}

// DELETE /v1/vhd deletes the data disk, its snapshot and a leftover
// copy, and answers the bytes they held; without a VM it removes the VM
// directory too, which a VM still registered keeps.
func TestDeleteVHD(t *testing.T) {
	for _, vm := range []bool{false, true} {
		h := newHarness(t)
		h.caller.dirs[testCfg] = true
		if vm {
			h.ownVM(StateOff)
		}
		h.files.putDisk(testData, []byte("data"))
		h.files.putDisk(testSnapshot, []byte("snap"))
		h.files.putDisk(testImport, []byte("copy"))
		res, err := h.client.DeleteVHD(context.Background(), testCfg)
		if err != nil || res.FreedBytes != 12 {
			t.Fatalf("VM %v: DeleteVHD = %+v, %v", vm, res, err)
		}
		for _, p := range []string{testData, testSnapshot, testImport} {
			if _, ok := h.files.disk(p); ok {
				t.Errorf("VM %v: %s is still there", vm, p)
			}
		}
		got := h.files.removedDirs()
		if !vm && (len(got) != 1 || got[0].dir != vmDirFor(testCfg)) {
			t.Errorf("without a VM: removed %+v, want the VM directory", got)
		}
		if vm && len(got) != 0 {
			t.Errorf("with a VM: removed %+v", got)
		}
	}
}

// DELETE /v1/vhd with no VM directory deletes nothing and makes none;
// it refuses a running VM and another account's directory.
func TestDeleteVHDRefusals(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	res, err := h.client.DeleteVHD(context.Background(), testCfg)
	if err != nil || res.FreedBytes != 0 || len(h.files.vmDirs) != 0 {
		t.Fatalf("no VM directory: %+v, %v, VM directories made %v", res, err, h.files.vmDirs)
	}
	for _, tc := range []struct {
		what   string
		setup  func(h *harness)
		status int
	}{
		{"a running VM", func(h *harness) { h.ownVM(StateRunning) }, http.StatusConflict},
		{"another account's VM directory", func(h *harness) {
			h.files.dirAccount[host.DirKey(testCfg)] = "S-1-5-21-111-222-333-2002"
		}, http.StatusForbidden},
	} {
		h := newHarness(t)
		h.caller.dirs[testCfg] = true
		h.files.putDisk(testData, []byte("data"))
		tc.setup(h)
		if _, err := h.client.DeleteVHD(context.Background(), testCfg); statusOf(err) != tc.status {
			t.Errorf("%s: err = %v (status %d), want %d", tc.what, err, statusOf(err), tc.status)
		}
		if _, ok := h.files.disk(testData); !ok {
			t.Errorf("%s: the data disk was deleted", tc.what)
		}
	}
}
