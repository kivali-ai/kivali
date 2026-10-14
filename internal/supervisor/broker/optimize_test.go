package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

func TestOptimizeRequestValidate(t *testing.T) {
	for _, ok := range []string{`C:\Kivali\team1\data.vhdx`, `D:\x\DATA.VHDX`} {
		if err := (OptimizeRequest{Path: ok}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{``, `C:\Kivali\team1\other.vhdx`, `C:\Kivali\team1\vm\root-child.vhdx`, `\\server\share\data.vhdx`, `C:\a\..\b\data.vhdx`, `data.vhdx`} {
		if err := (OptimizeRequest{Path: bad}).Validate(); err == nil {
			t.Errorf("Validate(%q) accepted", bad)
		}
	}
}

// The caller's own data disk, its VM off: optimize-vhd runs on the
// canonical path the caller's handle checked, and the sizes come back.
func TestOptimizeVHD(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.files[p] = fakeEntry{read: true, write: true}
	h.ownVM(StateOff)
	h.runner.resp[scriptOptimizeVHD] = json.RawMessage(`{"size_before":8627683328,"size_after":6580862976}`)
	res, err := h.client.OptimizeVHD(context.Background(), p)
	if err != nil {
		t.Fatalf("OptimizeVHD: %v", err)
	}
	if res.SizeBefore != 8627683328 || res.SizeAfter != 6580862976 {
		t.Fatalf("result = %+v", res)
	}
	args, ok := h.runner.lastCall(scriptOptimizeVHD)
	if !ok {
		t.Fatal("optimize-vhd not run")
	}
	var oa map[string]any
	_ = json.Unmarshal(args, &oa)
	if oa["path"] != p || len(oa) != 1 {
		t.Fatalf("optimize-vhd args = %v", oa)
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
	if !h.runner.called(scriptOptimizeVHD) {
		t.Fatal("optimize-vhd not run")
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
	if h.runner.called(scriptOptimizeVHD) {
		t.Error("optimize-vhd ran on a running VM's disk")
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
	if h.runner.called(scriptOptimizeVHD) {
		t.Error("optimize-vhd ran on another account's VM's disk")
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
		if h.runner.called(scriptOptimizeVHD) {
			t.Errorf("%s: optimize-vhd ran", tc.what)
		}
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
