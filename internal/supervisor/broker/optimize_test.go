package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

func TestOptimizeRequestValidate(t *testing.T) {
	for _, ok := range []string{`C:\Kivali\team1\data.vhdx`, `D:\x\DATA.VHDX`, `C:\Kivali\[team1]\data.vhdx`} {
		if err := (OptimizeRequest{Path: ok}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{``, `C:\Kivali\team1\other.vhdx`, `C:\Kivali\team1\vm\root-child.vhdx`, `\\server\share\data.vhdx`, `C:\a\..\b\data.vhdx`, `data.vhdx`,
		`C:\Kivali\team*\data.vhdx`, `C:\Kivali\team?\data.vhdx`} {
		if err := (OptimizeRequest{Path: bad}).Validate(); err == nil {
			t.Errorf("Validate(%q) accepted", bad)
		}
	}
}

// The caller's own data disk, its VM off: the caller's pinned file is
// compacted, on the canonical path the caller's handle checked, and
// the sizes come back. No script runs but the state query.
func TestOptimizeVHD(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.files[p] = fakeEntry{read: true, write: true}
	h.caller.compactSizes = [2]int64{8627683328, 6580862976}
	h.ownVM(StateOff)
	res, err := h.client.OptimizeVHD(context.Background(), p)
	if err != nil {
		t.Fatalf("OptimizeVHD: %v", err)
	}
	if res.SizeBefore != 8627683328 || res.SizeAfter != 6580862976 {
		t.Fatalf("result = %+v", res)
	}
	if len(h.caller.compacted) != 1 || h.caller.compacted[0] != p {
		t.Fatalf("compacted = %v", h.caller.compacted)
	}
	for _, c := range h.runner.calls {
		if c.script != scriptGetVM {
			t.Errorf("%s ran", c.script)
		}
	}
}

// Without a VM of the directory's key (a team whose VM was removed, or
// never made), the disk is still the caller's: compacted.
func TestOptimizeVHDWithoutVM(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.files[p] = fakeEntry{read: true, write: true}
	if _, err := h.client.OptimizeVHD(context.Background(), p); err != nil {
		t.Fatalf("OptimizeVHD: %v", err)
	}
	if len(h.caller.compacted) != 1 {
		t.Fatal("not compacted")
	}
}

// A disk a running VM holds is never compacted: Hyper-V would refuse,
// and the answer says why.
func TestOptimizeVHDRefusesRunningVM(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.files[p] = fakeEntry{read: true, write: true}
	h.ownVM(StateRunning)
	_, err := h.client.OptimizeVHD(context.Background(), p)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "Running") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if len(h.caller.compacted) != 0 {
		t.Error("a running VM's disk was compacted")
	}
}

// The VM of the directory belongs to another account: refused, whatever
// the caller can do to the file.
func TestOptimizeVHDRefusesForeignVM(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.files[p] = fakeEntry{read: true, write: true}
	h.ownVM(StateOff).Notes = OwnerNotes("S-1-5-21-111-222-333-2002")
	_, err := h.client.OptimizeVHD(context.Background(), p)
	if statusOf(err) != http.StatusForbidden {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if len(h.caller.compacted) != 0 {
		t.Error("another account's VM's disk was compacted")
	}
}

// The path must be data.vhdx in a directory the caller can write, and a
// file the caller can write; the file checked is the one in the
// canonical directory, not the request's spelling.
func TestOptimizeVHDRefusals(t *testing.T) {
	p := winJoin(testCfg, DataDiskName)
	for _, tc := range []struct {
		what   string
		path   string
		setup  func(h *harness)
		status int
	}{
		{"not data.vhdx", winJoin(testCfg, "other.vhdx"), func(h *harness) {
			h.caller.dirs[testCfg] = true
			h.caller.files[winJoin(testCfg, "other.vhdx")] = fakeEntry{read: true, write: true}
		}, http.StatusBadRequest},
		{"directory not writable", p, func(h *harness) {
			h.caller.files[p] = fakeEntry{read: true, write: true}
		}, http.StatusForbidden},
		{"file not writable", p, func(h *harness) {
			h.caller.dirs[testCfg] = true
			h.caller.files[p] = fakeEntry{read: true}
		}, http.StatusForbidden},
		{"no file", p, func(h *harness) {
			h.caller.dirs[testCfg] = true
		}, http.StatusForbidden},
	} {
		h := newHarness(t)
		tc.setup(h)
		h.ownVM(StateOff)
		_, err := h.client.OptimizeVHD(context.Background(), tc.path)
		if statusOf(err) != tc.status {
			t.Errorf("%s: err = %v (status %d), want %d", tc.what, err, statusOf(err), tc.status)
		}
		if len(h.caller.compacted) != 0 {
			t.Errorf("%s: compacted", tc.what)
		}
	}
}

// Only a dynamic VHDX with no parent is compacted: the broker reads the
// file's metadata through the caller's pinning handle first, and
// refuses anything else with the reason before the virtual disk is
// opened.
func TestOptimizeVHDRefusesAllButADynamicDisk(t *testing.T) {
	p := winJoin(testCfg, DataDiskName)
	for _, tc := range []struct {
		what    string
		content []byte
		want    string
	}{
		{"differencing", vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: vhdx.FlagHasParent}.Bytes(), "differencing"},
		{"fixed", vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20, Flags: vhdx.FlagLeaveBlocksAllocated}.Bytes(), "fixed"},
		{"not a VHDX", make([]byte, 3<<20), "not a VHDX"},
		{"empty", []byte{}, "not a VHDX"},
	} {
		h := newHarness(t)
		h.caller.dirs[testCfg] = true
		h.caller.files[p] = fakeEntry{read: true, write: true, content: tc.content}
		h.ownVM(StateOff)
		_, err := h.client.OptimizeVHD(context.Background(), p)
		if statusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v (status %d)", tc.what, err, statusOf(err))
		}
		if len(h.caller.compacted) != 0 {
			t.Errorf("%s: compacted", tc.what)
		}
	}
}

// The disk's kind is checked again on the virtual disk's own handle (a
// caller can rewrite the file after the first read): a refusal there
// is a refusal, any other failure the broker's.
func TestOptimizeVHDCompactionErrors(t *testing.T) {
	p := winJoin(testCfg, DataDiskName)
	for _, tc := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("%s: %w: a differencing disk", p, errNotDynamic), http.StatusForbidden},
		{errors.New("compact: the device is not ready"), http.StatusInternalServerError},
	} {
		h := newHarness(t)
		h.caller.dirs[testCfg] = true
		h.caller.files[p] = fakeEntry{read: true, write: true}
		h.caller.compactErr = tc.err
		h.ownVM(StateOff)
		_, err := h.client.OptimizeVHD(context.Background(), p)
		if statusOf(err) != tc.status {
			t.Errorf("%v: err = %v (status %d), want %d", tc.err, err, statusOf(err), tc.status)
		}
	}
}

// A path with [ or ] is a plain path: it reaches no cmdlet.
func TestOptimizeVHDTakesABracketedPath(t *testing.T) {
	dir := `C:\Kivali\[team1]`
	p := winJoin(dir, DataDiskName)
	h := newHarness(t)
	h.caller.dirs[dir] = true
	h.caller.files[p] = fakeEntry{read: true, write: true}
	if _, err := h.client.OptimizeVHD(context.Background(), p); err != nil {
		t.Fatalf("OptimizeVHD: %v", err)
	}
	if len(h.caller.compacted) != 1 || h.caller.compacted[0] != p {
		t.Fatalf("compacted = %v", h.caller.compacted)
	}
}

// The VM asked about is the one of the disk's directory, by its key.
func TestOptimizeVHDAsksAboutTheDirectorysVM(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.files[p] = fakeEntry{read: true, write: true}
	if _, err := h.client.OptimizeVHD(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	args, _ := h.runner.lastCall(scriptGetVM)
	var ga getVMArgs
	_ = json.Unmarshal(args, &ga)
	if ga.Name != VMName(host.DirKey(testCfg)) || !ga.StateOnly {
		t.Fatalf("get-vm args = %+v", ga)
	}
}
