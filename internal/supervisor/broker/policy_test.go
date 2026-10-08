package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// --- fakes (the Windows-only parts the policy talks to) ---

type fakeEntry struct{ read, write bool }

// fakeCaller answers the file checks from sets a test fills, as the
// impersonating caller would: a path it does not list is one the caller
// cannot open. Paths are already canonical in the tests.
type fakeCaller struct {
	sid   string
	dirs  map[string]bool      // directories the caller can add files to
	files map[string]fakeEntry // existing regular files
	// renameErr is Rename's answer (nil: the move happened).
	renameErr error
	renames   []rename
}

type rename struct{ src, dst string }

func (c *fakeCaller) SID() string { return c.sid }

func (c *fakeCaller) WritableDir(dir string) (string, error) {
	if c.dirs[dir] {
		return dir, nil
	}
	return "", errors.New("access denied")
}

func (c *fakeCaller) File(p string, access Access) (string, error) {
	e, ok := c.files[p]
	if !ok {
		return "", errors.New("no such file")
	}
	if access == AccessRead && !e.read {
		return "", errors.New("access denied")
	}
	if access == AccessWrite && !e.write {
		return "", errors.New("access denied")
	}
	return p, nil
}

func (c *fakeCaller) Absent(p string) error {
	if _, ok := c.files[p]; ok {
		return ErrExists
	}
	return nil
}

func (c *fakeCaller) Rename(src, dst string) error {
	c.renames = append(c.renames, rename{src, dst})
	return c.renameErr
}

func (c *fakeCaller) Close() error { return nil }

// fakeFiles is the broker's own file work, recorded: the VM directories
// it was asked to make, the files it moved into place and the VM
// directories it removed, each with whether the script that had to run
// first (new-vhd, remove-vm) had run when it was asked.
type fakeFiles struct {
	runner *fakeRunner
	canon  map[string]string

	mu        sync.Mutex
	vmDirs    []string // VMDir's keys
	granted   []grant
	placed    []placement
	removed   []removal
	removeErr error
}

// grant is a GrantFile call: the broker's new file, the caller it was
// granted to, and whether new-vhd had made it by then.
type grant struct {
	src, owner string
	created    bool
}

// placement is a PlacedFile call: the identity GrantFile returned, the
// destination and the caller.
type placement struct {
	id         FileID
	dst, owner string
}

type removal struct {
	dir       string
	vmRemoved bool // remove-vm had run when the directory was asked for
}

// fakeVMRoot stands in for the broker's own VM directory root.
const fakeVMRoot = `C:\ProgramData\Kivali\broker\vm`

// vmDirFor is the VM directory of a config directory, as the fake makes it.
func vmDirFor(configDir string) string { return winJoin(fakeVMRoot, host.DirKey(configDir)) }

func (f *fakeFiles) VMDir(key, _ string) (string, func(), error) {
	f.mu.Lock()
	f.vmDirs = append(f.vmDirs, key)
	f.mu.Unlock()
	return winJoin(fakeVMRoot, key), func() {}, nil
}

func (f *fakeFiles) VMDirPath(key string) string { return winJoin(fakeVMRoot, key) }

func (f *fakeFiles) Canonical(p string) (string, error) {
	if c, ok := f.canon[p]; ok {
		return c, nil
	}
	return p, nil
}

func (f *fakeFiles) GrantFile(p, owner string) (FileID, error) {
	var na struct {
		Path string `json:"path"`
	}
	if args, ok := f.runner.lastCall(scriptNewVHD); ok {
		_ = json.Unmarshal(args, &na)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.granted = append(f.granted, grant{src: p, owner: owner, created: na.Path == p})
	return FileID{Volume: 1, High: 0, Low: uint32(len(f.granted))}, nil
}

func (f *fakeFiles) PlacedFile(dst string, id FileID, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.placed = append(f.placed, placement{id: id, dst: dst, owner: owner})
	return nil
}

func (f *fakeFiles) DeleteFile(string) error { return nil }

func (f *fakeFiles) RemoveVMDir(dir, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, removal{dir: dir, vmRemoved: f.runner.called(scriptRemoveVM)})
	return f.removeErr
}

func (f *fakeFiles) removedDirs() []removal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]removal(nil), f.removed...)
}

type recorded struct {
	script string
	args   json.RawMessage
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []recorded
	vm    *vmInfo                    // what get-vm returns (nil: exists:false)
	resp  map[string]json.RawMessage // other scripts' canned output
	errs  map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{resp: map[string]json.RawMessage{}, errs: map[string]error{}}
}

func (r *fakeRunner) Run(_ context.Context, script string, args any) (json.RawMessage, error) {
	raw, _ := json.Marshal(args)
	r.mu.Lock()
	r.calls = append(r.calls, recorded{script, raw})
	r.mu.Unlock()
	if e := r.errs[script]; e != nil {
		return nil, e
	}
	if script == scriptGetVM {
		if r.vm == nil {
			return json.RawMessage(`{"exists":false}`), nil
		}
		b, _ := json.Marshal(r.vm)
		return b, nil
	}
	if resp, ok := r.resp[script]; ok {
		return resp, nil
	}
	return json.RawMessage(`{}`), nil
}

func (r *fakeRunner) lastCall(script string) (json.RawMessage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].script == script {
			return r.calls[i].args, true
		}
	}
	return nil, false
}

func (r *fakeRunner) called(script string) bool {
	_, ok := r.lastCall(script)
	return ok
}

// --- harness ---

const testSID = "S-1-5-21-111-222-333-1001"

type harness struct {
	t      *testing.T
	runner *fakeRunner
	files  *fakeFiles
	caller *fakeCaller
	client *Client
}

func newHarness(t *testing.T) *harness {
	runner := newFakeRunner()
	h := &harness{
		t:      t,
		runner: runner,
		files:  &fakeFiles{runner: runner, canon: map[string]string{}},
		caller: &fakeCaller{sid: testSID, dirs: map[string]bool{}, files: map[string]fakeEntry{}},
	}
	s := &Server{
		Runner:   h.runner,
		Files:    h.files,
		CallerOf: func(*http.Request) (Caller, error) { return h.caller, nil },
		Logf:     t.Logf,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")
	h.client = &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}}
	return h
}

const testCfg = `C:\Kivali\team1`

func (h *harness) req() VMRequest {
	key := host.DirKey(testCfg)
	return VMRequest{
		Name:      VMName(key),
		ConfigDir: testCfg,
		RootVHDX:  `C:\Program Files\Kivali\vm\root.vhdx`,
		DataVHDX:  winJoin(testCfg, DataDiskName),
		CPUs:      4,
		MemoryMB:  4096,
		MAC:       MAC(key),
		ComPipe:   ComPipe(key),
	}
}

// allowCreate lets the caller write the config dir and open both disks.
func (h *harness) allowCreate(r VMRequest) {
	h.caller.dirs[r.ConfigDir] = true
	h.caller.files[r.DataVHDX] = fakeEntry{read: true, write: true}
	h.caller.files[r.RootVHDX] = fakeEntry{read: true}
}

// ownVM makes get-vm report a VM owned by the caller in the given state,
// with the disks the backend attaches.
func (h *harness) ownVM(state string) *vmInfo {
	key := host.DirKey(testCfg)
	vm := &vmInfo{
		Exists: true, ID: "VMID-1", State: state, Notes: OwnerNotes(testSID),
		Path: vmDirFor(testCfg),
		CPUs: 4, MemoryStartupMB: 4096, MemoryMaxMB: 4096, DynamicMemory: true,
		MAC: MAC(key), Switch: DefaultSwitch, ComPipe: ComPipe(key),
		Disks: []vmDisk{
			{Controller: 0, Location: 0, Path: winJoin(vmDirFor(testCfg), ChildDiskName), Parent: `C:\Program Files\Kivali\vm\root.vhdx`},
			{Controller: 0, Location: 1, Path: winJoin(testCfg, DataDiskName)},
		},
	}
	h.runner.vm = vm
	return vm
}

func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// --- tests ---

func TestEnsureCreates(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	h.runner.resp[scriptConfigureVM] = json.RawMessage(`{"id":"VMID-1","state":"Off"}`)

	st, err := h.client.EnsureVM(context.Background(), r)
	if err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	if st.State != StateOff || st.ID != "VMID-1" {
		t.Fatalf("state = %+v", st)
	}
	args, ok := h.runner.lastCall(scriptConfigureVM)
	if !ok {
		t.Fatal("configure-vm not run")
	}
	var ca configureArgs
	if err := json.Unmarshal(args, &ca); err != nil {
		t.Fatal(err)
	}
	if ca.Child != winJoin(vmDirFor(testCfg), ChildDiskName) {
		t.Errorf("child = %q", ca.Child)
	}
	if ca.Data != r.DataVHDX || ca.Root != r.RootVHDX {
		t.Errorf("disks = %q, %q", ca.Data, ca.Root)
	}
	if ca.MAC != strings.ToUpper(r.MAC) {
		t.Errorf("mac = %q", ca.MAC)
	}
	if ca.MemoryBytes != 4096<<20 || ca.CPUs != 4 {
		t.Errorf("memory/cpus = %d, %d", ca.MemoryBytes, ca.CPUs)
	}
	if owner, ok := OwnerOf(ca.Notes); !ok || owner != testSID {
		t.Errorf("notes owner = %q, %v", owner, ok)
	}
	if ca.Switch != DefaultSwitch {
		t.Errorf("switch = %q", ca.Switch)
	}
}

func TestEnsureRejectsWrongName(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	r.Name = "kivali-00000000" // valid form, wrong key
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), "not kivali-00000000") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

func TestEnsureRejectsWrongMAC(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	r.MAC = "02ffffffffff" // well-formed, not the key's
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), "MAC") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

func TestEnsureRejectsMalformedRequest(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	r.MemoryMB = 100 // below the minimum: refused before anything opens
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusBadRequest {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if h.runner.called(scriptGetVM) {
		t.Error("a malformed request reached Hyper-V")
	}
}

func TestEnsureDataDiskMustBeInConfigDir(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	// A data disk that validates (ends .vhdx, local, clean) but is not
	// inside the config directory.
	r.DataVHDX = `C:\Elsewhere\data.vhdx`
	h.caller.dirs[r.ConfigDir] = true
	h.caller.files[r.DataVHDX] = fakeEntry{read: true, write: true}
	h.caller.files[r.RootVHDX] = fakeEntry{read: true}
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), "not in") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

func TestEnsureRefusedWhenConfigDirNotWritable(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	// The caller cannot write the config dir.
	h.caller.files[r.DataVHDX] = fakeEntry{read: true, write: true}
	h.caller.files[r.RootVHDX] = fakeEntry{read: true}
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusForbidden || !strings.Contains(err.Error(), "config_dir") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

func TestEnsureReconfiguresWhileOff(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	h.ownVM(StateOff)
	h.runner.resp[scriptConfigureVM] = json.RawMessage(`{"id":"VMID-1","state":"Off"}`)
	if _, err := h.client.EnsureVM(context.Background(), r); err != nil {
		t.Fatalf("EnsureVM while off: %v", err)
	}
	if !h.runner.called(scriptConfigureVM) {
		t.Error("an off VM was not reconfigured")
	}
}

func TestEnsureRunningDefersMemory(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	r.MemoryMB = 8192 // only memory changes
	h.allowCreate(r)
	h.ownVM(StateRunning)
	st, err := h.client.EnsureVM(context.Background(), r)
	if err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	if len(st.Deferred) == 0 || st.Deferred[0] != "memory" {
		t.Fatalf("deferred = %v", st.Deferred)
	}
	if h.runner.called(scriptConfigureVM) {
		t.Error("a running VM was reconfigured")
	}
}

func TestEnsureRunningRefusesStructuralChange(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	vm := h.ownVM(StateRunning)
	vm.Disks[1].Path = `C:\Kivali\team1\other.vhdx` // data disk differs
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "data disk") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

func TestOwnershipRefused(t *testing.T) {
	h := newHarness(t)
	vm := h.ownVM(StateOff)
	vm.Notes = OwnerNotes("S-1-5-21-999-999-999-2002") // another account
	key := host.DirKey(testCfg)
	for _, call := range []func() error{
		func() error { _, err := h.client.VM(context.Background(), VMName(key)); return err },
		func() error { _, err := h.client.Start(context.Background(), VMName(key)); return err },
		func() error { _, err := h.client.Stop(context.Background(), VMName(key), true); return err },
		func() error { return h.client.Remove(context.Background(), VMName(key)) },
	} {
		if err := call(); statusOf(err) != http.StatusForbidden {
			t.Errorf("ownership not enforced: %v (status %d)", err, statusOf(err))
		}
	}
}

func TestGetMissingVM(t *testing.T) {
	h := newHarness(t)
	_, err := h.client.VM(context.Background(), VMName(host.DirKey(testCfg)))
	if !IsNotFound(err) {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

func TestStartChecksDisks(t *testing.T) {
	h := newHarness(t)
	key := host.DirKey(testCfg)
	h.ownVM(StateOff)
	h.caller.files[winJoin(testCfg, DataDiskName)] = fakeEntry{read: true, write: true}
	h.runner.resp[scriptStartVM] = json.RawMessage(`{"id":"VMID-1","state":"Running"}`)
	st, err := h.client.Start(context.Background(), VMName(key))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.State != StateRunning {
		t.Fatalf("state = %q", st.State)
	}
	if !h.runner.called(scriptStartVM) {
		t.Error("start-vm not run")
	}
}

func TestStartRefusesWhenDataDiskMoved(t *testing.T) {
	h := newHarness(t)
	key := host.DirKey(testCfg)
	vm := h.ownVM(StateOff)
	vm.Disks[1].Path = `C:\Elsewhere\data.vhdx`
	h.caller.files[`C:\Elsewhere\data.vhdx`] = fakeEntry{read: true, write: true}
	_, err := h.client.Start(context.Background(), VMName(key))
	if statusOf(err) != http.StatusConflict {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if h.runner.called(scriptStartVM) {
		t.Error("a VM whose disk moved was started")
	}
}

func TestRemoveDeletesChildNotData(t *testing.T) {
	h := newHarness(t)
	key := host.DirKey(testCfg)
	h.ownVM(StateOff)
	if err := h.client.Remove(context.Background(), VMName(key)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	args, ok := h.runner.lastCall(scriptRemoveVM)
	if !ok {
		t.Fatal("remove-vm not run")
	}
	var ra struct {
		Name  string `json:"name"`
		Child string `json:"child"`
	}
	if err := json.Unmarshal(args, &ra); err != nil {
		t.Fatal(err)
	}
	if ra.Child != winJoin(vmDirFor(testCfg), ChildDiskName) {
		t.Errorf("child to delete = %q", ra.Child)
	}
	if strings.Contains(strings.ToLower(ra.Child), strings.ToLower(DataDiskName)) {
		t.Errorf("the data disk would be deleted: %q", ra.Child)
	}
	// The broker's VM directory goes too, after Remove-VM, and nothing
	// else is asked for.
	got := h.files.removedDirs()
	if len(got) != 1 || got[0].dir != vmDirFor(testCfg) || !got[0].vmRemoved {
		t.Fatalf("VM directories removed = %+v, want only %s, after remove-vm", got, vmDirFor(testCfg))
	}
}

func TestRemoveLeavesForeignRootDisk(t *testing.T) {
	h := newHarness(t)
	key := host.DirKey(testCfg)
	vm := h.ownVM(StateOff)
	vm.Disks[0].Path = `C:\Windows\System32\config\SAM` // not the broker's child
	vm.Path = ""                                        // and no configuration location either
	if err := h.client.Remove(context.Background(), VMName(key)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	args, _ := h.runner.lastCall(scriptRemoveVM)
	var ra struct {
		Child string `json:"child"`
	}
	_ = json.Unmarshal(args, &ra)
	if ra.Child != "" {
		t.Errorf("a foreign root disk would be deleted: %q", ra.Child)
	}
	if got := h.files.removedDirs(); len(got) != 0 {
		t.Errorf("a directory was removed with nothing to vouch for it: %+v", got)
	}
}

// A VM whose configuration failed before its child was made still has
// its VM directory removed, found from where Hyper-V keeps its
// configuration.
func TestRemoveFindsVMDirWithoutChild(t *testing.T) {
	h := newHarness(t)
	key := host.DirKey(testCfg)
	vm := h.ownVM(StateOff)
	vm.Disks = nil
	if err := h.client.Remove(context.Background(), VMName(key)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := h.files.removedDirs()
	if len(got) != 1 || got[0].dir != vmDirFor(testCfg) {
		t.Fatalf("VM directories removed = %+v", got)
	}
}

// The VM directory is removed only if, canonically, it is the vm folder
// of a config directory of the VM's own key: a location elsewhere, one
// reached through a link, or another team's vm folder is left alone
// (and the VM is removed all the same).
func TestRemoveRefusesForeignVMDir(t *testing.T) {
	for why, set := range map[string]func(h *harness, vm *vmInfo){
		"Hyper-V's default location": func(_ *harness, vm *vmInfo) { vm.Path = `C:\ProgramData\Microsoft\Windows\Hyper-V` },
		"another team's vm folder":   func(_ *harness, vm *vmInfo) { vm.Path = `C:\Kivali\team2\vm` },
		"a vm folder a level down":   func(_ *harness, vm *vmInfo) { vm.Path = `C:\Kivali\team1\nested\vm` },
		"a link along the path": func(h *harness, vm *vmInfo) {
			h.files.canon[vm.Path] = `C:\Windows\System32\vm`
		},
	} {
		t.Run(why, func(t *testing.T) {
			h := newHarness(t)
			key := host.DirKey(testCfg)
			vm := h.ownVM(StateOff)
			vm.Disks[0].Path = `C:\Elsewhere\root-child.vhdx` // the child does not vouch for it
			set(h, vm)
			if err := h.client.Remove(context.Background(), VMName(key)); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if !h.runner.called(scriptRemoveVM) {
				t.Fatal("the VM was not removed")
			}
			if got := h.files.removedDirs(); len(got) != 0 {
				t.Fatalf("removed %+v", got)
			}
		})
	}
}

// When the directory's own check fails (on the handle it would delete
// through), the VM is gone but DELETE says the directory is not, and it
// asked for nothing but the VM's directory.
func TestRemoveReportsVMDirRefusal(t *testing.T) {
	h := newHarness(t)
	key := host.DirKey(testCfg)
	h.ownVM(StateOff)
	h.files.removeErr = errors.New("a reparse point lies along the path")
	err := h.client.Remove(context.Background(), VMName(key))
	if statusOf(err) != http.StatusInternalServerError || !strings.Contains(err.Error(), "not its directory") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	got := h.files.removedDirs()
	if len(got) != 1 || got[0].dir != vmDirFor(testCfg) {
		t.Fatalf("VM directories asked for = %+v", got)
	}
}

// The data disk is created in the broker's VM directory, never at the
// caller's path, and then moved to exactly <dir>\data.vhdx and granted
// to the caller.
func TestNewVHD(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	res, err := h.client.NewVHD(context.Background(), p, 64<<30)
	if err != nil {
		t.Fatalf("NewVHD: %v", err)
	}
	if res.Path != p {
		t.Fatalf("path = %q", res.Path)
	}
	args, ok := h.runner.lastCall(scriptNewVHD)
	if !ok {
		t.Fatal("new-vhd not run")
	}
	var na map[string]any
	_ = json.Unmarshal(args, &na)
	made, _ := na["path"].(string)
	if !Under(vmDirFor(testCfg), made) || made != winJoin(vmDirFor(testCfg), newDiskName) {
		t.Fatalf("new-vhd creates %q, not in the VM directory", made)
	}
	if size, _ := na["size_bytes"].(float64); size != 64<<30 {
		t.Fatalf("size_bytes = %v", na["size_bytes"])
	}
	if _, ok := na["owner_sid"]; ok || len(na) != 2 {
		t.Fatalf("new-vhd args = %v; the grant is the broker's, after the move", na)
	}
	if len(h.files.vmDirs) != 1 || h.files.vmDirs[0] != host.DirKey(testCfg) {
		t.Fatalf("VM directories made = %v", h.files.vmDirs)
	}
	if g := (grant{src: made, owner: testSID, created: true}); len(h.files.granted) != 1 || h.files.granted[0] != g {
		t.Fatalf("grants = %+v, want %+v", h.files.granted, g)
	}
	if r := (rename{made, p}); len(h.caller.renames) != 1 || h.caller.renames[0] != r {
		t.Fatalf("renames = %+v, want %+v", h.caller.renames, r)
	}
	if pl := (placement{id: FileID{Volume: 1, Low: 1}, dst: p, owner: testSID}); len(h.files.placed) != 1 || h.files.placed[0] != pl {
		t.Fatalf("placed = %+v, want %+v", h.files.placed, pl)
	}
}

func TestNewVHDRefusesExisting(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.files[p] = fakeEntry{read: true, write: true} // already there
	_, err := h.client.NewVHD(context.Background(), p, 64<<30)
	if statusOf(err) != http.StatusConflict {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if h.runner.called(scriptNewVHD) {
		t.Error("new-vhd ran over an existing file")
	}
	if len(h.caller.renames) != 0 {
		t.Errorf("a move was asked for: %+v", h.caller.renames)
	}
}

// Something put at the destination after the check and before the move
// makes the rename fail: a conflict, as if it had been there before.
func TestNewVHDConflictAtTheMove(t *testing.T) {
	h := newHarness(t)
	p := winJoin(testCfg, DataDiskName)
	h.caller.dirs[testCfg] = true
	h.caller.renameErr = ErrExists
	_, err := h.client.NewVHD(context.Background(), p, 64<<30)
	if statusOf(err) != http.StatusConflict {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

// A plain state query (GET, which the backend polls, and a stop) asks
// get-vm for the state only; a configuration, a start and a removal,
// which check the disks, ask for everything.
func TestStateOnlyQueries(t *testing.T) {
	key := host.DirKey(testCfg)
	stateOnly := func(h *harness) bool {
		t.Helper()
		args, ok := h.runner.lastCall(scriptGetVM)
		if !ok {
			t.Fatal("get-vm not run")
		}
		var ga getVMArgs
		if err := json.Unmarshal(args, &ga); err != nil {
			t.Fatal(err)
		}
		if ga.Name != VMName(key) {
			t.Fatalf("get-vm name = %q", ga.Name)
		}
		return ga.StateOnly
	}
	for _, tc := range []struct {
		what      string
		call      func(h *harness) error
		stateOnly bool
	}{
		{"GET", func(h *harness) error { _, err := h.client.VM(context.Background(), VMName(key)); return err }, true},
		{"stop", func(h *harness) error { _, err := h.client.Stop(context.Background(), VMName(key), true); return err }, true},
		{"start", func(h *harness) error { _, err := h.client.Start(context.Background(), VMName(key)); return err }, false},
		{"DELETE", func(h *harness) error { return h.client.Remove(context.Background(), VMName(key)) }, false},
		{"POST /v1/vm", func(h *harness) error {
			r := h.req()
			h.allowCreate(r)
			_, err := h.client.EnsureVM(context.Background(), r)
			return err
		}, false},
	} {
		h := newHarness(t)
		h.ownVM(StateOff)
		h.caller.files[winJoin(testCfg, DataDiskName)] = fakeEntry{read: true, write: true}
		if err := tc.call(h); err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if got := stateOnly(h); got != tc.stateOnly {
			t.Errorf("%s: state_only = %v, want %v", tc.what, got, tc.stateOnly)
		}
	}
}

func TestSetup(t *testing.T) {
	h := newHarness(t)
	h.runner.resp[scriptSetupStatus] = json.RawMessage(`{"hyperv":true,"default_switch":true,"vsock_service":false}`)
	st, err := h.client.Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !st.HyperV || !st.DefaultSwitch || st.VsockService {
		t.Fatalf("setup = %+v", st)
	}
	if st.Ready() {
		t.Error("Ready with the vsock service missing")
	}

	h.runner.resp[scriptSetupStatus] = json.RawMessage(`{"hyperv":true,"default_switch":true,"vsock_service":true}`)
	st, err = h.client.ApplySetup(context.Background())
	if err != nil {
		t.Fatalf("ApplySetup: %v", err)
	}
	if !st.Ready() || !h.runner.called(scriptSetupApply) {
		t.Fatalf("after apply: %+v, applied=%v", st, h.runner.called(scriptSetupApply))
	}
}

// Without a VM, DELETE removes the broker's VM directory of the name's
// key (a first up that made the data disk but never the VM leaves one
// behind, which the account cannot delete), if it is there and
// canonical; RemoveVMDir then checks it was made for the caller.
func TestRemoveOrphanVMDir(t *testing.T) {
	key := host.DirKey(testCfg)
	vmDir := vmDirFor(testCfg)
	t.Run("removed", func(t *testing.T) {
		h := newHarness(t)
		if err := h.client.Remove(context.Background(), VMName(key)); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if h.runner.called(scriptRemoveVM) {
			t.Fatal("remove-vm ran without a VM")
		}
		if got := h.files.removedDirs(); len(got) != 1 || got[0].dir != vmDir {
			t.Fatalf("removed %+v, want %s", got, vmDir)
		}
	})
	t.Run("a link", func(t *testing.T) {
		h := newHarness(t)
		h.files.canon[vmDir] = `C:\Windows\System32\vm`
		err := h.client.Remove(context.Background(), VMName(key))
		if statusOf(err) != http.StatusForbidden {
			t.Fatalf("err = %v (status %d)", err, statusOf(err))
		}
		if got := h.files.removedDirs(); len(got) != 0 {
			t.Fatalf("removed %+v", got)
		}
	})
	t.Run("refused by the directory's own check", func(t *testing.T) {
		h := newHarness(t)
		h.files.removeErr = errors.New("not made for this account")
		err := h.client.Remove(context.Background(), VMName(key))
		if statusOf(err) != http.StatusForbidden {
			t.Fatalf("err = %v (status %d)", err, statusOf(err))
		}
	})
}

// A VM whose configuration failed before its child disk was made has
// only the configuration location Hyper-V reports, <VM directory>\<name>
// (New-VM -Path keeps a VM under a folder of its name): DELETE removes
// the VM directory from that too.
func TestRemoveVMDirFromHyperVsLayout(t *testing.T) {
	h := newHarness(t)
	key := host.DirKey(testCfg)
	vm := h.ownVM(StateOff)
	vm.Disks = nil
	vm.Path = winJoin(vmDirFor(testCfg), VMName(key))
	if err := h.client.Remove(context.Background(), VMName(key)); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := h.files.removedDirs(); len(got) != 1 || got[0].dir != vmDirFor(testCfg) || !got[0].vmRemoved {
		t.Fatalf("removed %+v, want %s after remove-vm", got, vmDirFor(testCfg))
	}
}
