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
)

// A Meter reads its reader while one is attached, keeps the last value
// once it is detached, and calls its attached hook each time a reader
// is attached, never for a detach.
func TestMeterReader(t *testing.T) {
	m := &Meter{}
	hooks := 0
	m.attached = func() {
		hooks++
		if d, tot := m.Get(); d != 3 || tot != 10 {
			t.Errorf("Get in the hook = %d of %d, want the reader's 3 of 10", d, tot)
		}
	}
	m.Reader(func() (int64, int64) { return 3, 10 })
	m.Reader(nil)
	if hooks != 1 {
		t.Fatalf("hook called %d times, want once", hooks)
	}
	if d, tot := m.Get(); d != 3 || tot != 10 {
		t.Fatalf("Get after the detach = %d of %d, want the last value read", d, tot)
	}
	var none *Meter
	none.Reader(func() (int64, int64) { return 1, 1 })
	if d, tot := none.Get(); d != 0 || tot != 0 {
		t.Fatalf("a nil Meter = %d of %d", d, tot)
	}
}

func TestDiskRequestValidate(t *testing.T) {
	for _, ok := range []string{`C:\Kivali\team1`, `D:\x`, `C:\Kivali\[team1]`} {
		if err := (DiskRequest{ConfigDir: ok}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{``, `\\server\share\team`, `C:\a\..\b`, `team1`, `C:\Kivali\team*`, `C:\Kivali\team?`} {
		if err := (DiskRequest{ConfigDir: bad}).Validate(); err == nil {
			t.Errorf("Validate(%q) accepted", bad)
		}
	}
}

// The team's data disk, its VM off, is compacted where the broker keeps
// it, its own file in the VM directory, and the sizes come back. The
// caller's files are not opened, and no script runs but the state
// query.
func TestOptimizeVHD(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	h.files.putDisk(testData, dynamicDisk)
	h.files.compactSizes = [2]int64{8627683328, 6580862976}
	h.ownVM(StateOff)
	res, err := h.client.OptimizeVHD(context.Background(), testCfg)
	if err != nil {
		t.Fatalf("OptimizeVHD: %v", err)
	}
	if res.SizeBefore != 8627683328 || res.SizeAfter != 6580862976 {
		t.Fatalf("result = %+v", res)
	}
	if len(h.files.compacted) != 1 || h.files.compacted[0] != testData {
		t.Fatalf("compacted = %v, want %s", h.files.compacted, testData)
	}
	if len(h.caller.pinned) != 0 {
		t.Errorf("the caller's files were opened: %v", h.caller.pinned)
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
	h.caller.dirs[testCfg] = true
	h.files.putDisk(testData, dynamicDisk)
	if _, err := h.client.OptimizeVHD(context.Background(), testCfg); err != nil {
		t.Fatalf("OptimizeVHD: %v", err)
	}
	if len(h.files.compacted) != 1 {
		t.Fatal("not compacted")
	}
}

// Refusals: a running VM (Hyper-V would refuse, and the answer says
// why), another account's VM or VM directory, a config directory the
// caller cannot write, and no data disk.
func TestOptimizeVHDRefusals(t *testing.T) {
	for _, tc := range []struct {
		what   string
		setup  func(h *harness)
		status int
		want   string
	}{
		{"a running VM", func(h *harness) { h.ownVM(StateRunning) }, http.StatusConflict, "Running"},
		{"another account's VM", func(h *harness) { h.ownVM(StateOff).Notes = OwnerNotes("S-1-5-21-111-222-333-2002") }, http.StatusForbidden, "another account"},
		{"another account's VM directory", func(h *harness) {
			h.files.dirAccount[host.DirKey(testCfg)] = "S-1-5-21-111-222-333-2002"
		}, http.StatusForbidden, "another account"},
		{"a config directory the caller cannot write", func(h *harness) { h.caller.dirs[testCfg] = false }, http.StatusForbidden, "config_dir"},
		{"no data disk", func(h *harness) { h.files.drop(testData) }, http.StatusNotFound, "no data disk"},
	} {
		h := newHarness(t)
		h.caller.dirs[testCfg] = true
		h.files.putDisk(testData, dynamicDisk)
		tc.setup(h)
		_, err := h.client.OptimizeVHD(context.Background(), testCfg)
		if statusOf(err) != tc.status || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v (status %d), want %d", tc.what, err, statusOf(err), tc.status)
		}
		if len(h.files.compacted) != 0 {
			t.Errorf("%s: compacted", tc.what)
		}
	}
}

// The disk's kind is checked on the virtual disk's own handle: a
// refusal there is a refusal, any other failure the broker's.
func TestOptimizeVHDCompactionErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("%s: %w: a differencing disk", testData, errNotDynamic), http.StatusForbidden},
		{errors.New("compact: the device is not ready"), http.StatusInternalServerError},
	} {
		h := newHarness(t)
		h.caller.dirs[testCfg] = true
		h.files.putDisk(testData, dynamicDisk)
		h.files.compactErr = tc.err
		h.ownVM(StateOff)
		_, err := h.client.OptimizeVHD(context.Background(), testCfg)
		if statusOf(err) != tc.status {
			t.Errorf("%v: err = %v (status %d), want %d", tc.err, err, statusOf(err), tc.status)
		}
	}
}

// A config directory with [ or ] is a plain path: it reaches no cmdlet.
func TestOptimizeVHDTakesABracketedPath(t *testing.T) {
	dir := `C:\Kivali\[team1]`
	h := newHarness(t)
	h.caller.dirs[dir] = true
	data := winJoin(vmDirFor(dir), DataDiskName)
	h.files.putDisk(data, dynamicDisk)
	if _, err := h.client.OptimizeVHD(context.Background(), dir); err != nil {
		t.Fatalf("OptimizeVHD: %v", err)
	}
	if len(h.files.compacted) != 1 || h.files.compacted[0] != data {
		t.Fatalf("compacted = %v", h.files.compacted)
	}
}

// The VM asked about is the one of the config directory, by its key.
func TestOptimizeVHDAsksAboutTheDirectorysVM(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	h.files.putDisk(testData, dynamicDisk)
	if _, err := h.client.OptimizeVHD(context.Background(), testCfg); err != nil {
		t.Fatal(err)
	}
	args, _ := h.runner.lastCall(scriptGetVM)
	var ga getVMArgs
	_ = json.Unmarshal(args, &ga)
	if ga.Name != VMName(host.DirKey(testCfg)) || !ga.StateOnly {
		t.Fatalf("get-vm args = %+v", ga)
	}
}
