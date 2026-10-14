package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// --- fakes (the Windows-only parts the policy talks to) ---

type fakeEntry struct {
	read bool
	// content is what Contents reads; nil reads as a dynamic VHDX with
	// no parent, the data disk the backend makes.
	content []byte
}

// dynamicDisk is a dynamic VHDX with no parent.
var dynamicDisk = vhdx.Spec{BlockSize: 32 << 20, SectorSize: 512, VirtualSize: 64 << 20}.Bytes()

// fakeCaller answers the file checks from sets a test fills, as the
// impersonating caller would: a path it does not list is one the caller
// cannot open. Paths are already canonical in the tests.
type fakeCaller struct {
	sid   string
	dirs  map[string]bool      // directories the caller can add files to
	files map[string]fakeEntry // existing regular files
	// pinned lists the files File opened.
	pinned []string
}

func (c *fakeCaller) SID() string { return c.sid }

func (c *fakeCaller) WritableDir(dir string) (string, error) {
	if c.dirs[dir] {
		return dir, nil
	}
	return "", errors.New("access denied")
}

func (c *fakeCaller) File(p string) (string, error) {
	e, ok := c.files[p]
	if !ok {
		return "", fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
	}
	if !e.read {
		return "", errors.New("access denied")
	}
	c.pinned = append(c.pinned, p)
	return p, nil
}

func (c *fakeCaller) Contents(p string) (io.ReaderAt, int64, error) {
	e, ok := c.files[p]
	if !ok || !e.read {
		return nil, 0, errors.New("not pinned for reading")
	}
	b := e.content
	if b == nil {
		b = dynamicDisk
	}
	return bytes.NewReader(b), int64(len(b)), nil
}

func (c *fakeCaller) Close() error { return nil }

// fakeFiles is the broker's own file work, recorded: the VM directories
// it was asked to make, the disks in them (a map of path to content),
// and every copy, compaction, rename, deletion and removal, each with
// whether the script that had to run first had run when it was asked.
type fakeFiles struct {
	runner *fakeRunner
	canon  map[string]string
	// parents is what ParentPath reads from each differencing disk; a
	// path it does not list fails to read.
	parents map[string]string
	// rootErr is RootDisk's refusal (nil: the root disk is a fixed or
	// dynamic VHDX); rootChecks lists the paths it was asked about.
	rootErr    error
	rootChecks []string

	mu sync.Mutex
	// dirAccount binds each VM directory (by key) to the account VMDir
	// first made it for, as the DACL entry does.
	dirAccount map[string]string
	vmDirs     []string // VMDir's keys
	// disks are the files in the VM directories, by path.
	disks map[string][]byte
	// copyHook, when set, runs in each copy once it has begun, with its
	// meter (a test reads the progress there, or blocks it); copyErr,
	// when set, fails the copy after it.
	copyHook func(ctx context.Context, m *Meter) error
	copies   []fileCopy
	// checkErr, when set, is CheckDynamicVHDX's refusal; without it the
	// file's content is read as a VHDX (dynamicNoParent).
	checkErr error
	checked  []string
	// compactHook, compactSizes and compactErr make CompactVHDX's
	// answer; compacted lists its paths.
	compactHook  func(m *Meter)
	compactSizes [2]int64
	compactErr   error
	compacted    []string
	renames      []fileRename
	deleted      []string
	removed      []removal
	removeErr    error
}

type fileCopy struct {
	dst  string
	size int64
}

type fileRename struct {
	src, dst string
	replace  bool
}

type removal struct {
	dir       string
	vmRemoved bool // remove-vm had run when the directory was asked for
}

// fakeVMRoot stands in for the broker's own VM directory root.
const fakeVMRoot = `C:\ProgramData\Kivali\broker\vm`

// vmDirFor is the VM directory of a config directory, as the fake makes it.
func vmDirFor(configDir string) string { return winJoin(fakeVMRoot, host.DirKey(configDir)) }

func (f *fakeFiles) VMDir(key, sid string) (string, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirAccount == nil {
		f.dirAccount = map[string]string{}
	}
	if a, ok := f.dirAccount[key]; ok && a != sid {
		return "", nil, errors.New("made for another account")
	}
	f.dirAccount[key] = sid
	f.vmDirs = append(f.vmDirs, key)
	return winJoin(fakeVMRoot, key), func() {}, nil
}

func (f *fakeFiles) VMDirPath(key string) string { return winJoin(fakeVMRoot, key) }

func (f *fakeFiles) Canonical(p string) (string, error) {
	if c, ok := f.canon[p]; ok {
		return c, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(p, fakeVMRoot+`\`) && strings.Count(p[len(fakeVMRoot)+1:], `\`) == 0 {
		// A VM directory exists once VMDir made it.
		if _, ok := f.dirAccount[p[len(fakeVMRoot)+1:]]; !ok {
			return "", fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
		}
	}
	return p, nil
}

func (f *fakeFiles) ParentPath(p string) (string, error) {
	if parent, ok := f.parents[p]; ok {
		return parent, nil
	}
	return "", errors.New("not a differencing disk")
}

func (f *fakeFiles) Stat(p string) (FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.disks[p]
	if !ok {
		return FileInfo{}, fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
	}
	return FileInfo{Size: int64(len(b)), Used: int64(len(b))}, nil
}

func (f *fakeFiles) Open(p string) (io.ReaderAt, int64, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.disks[p]
	if !ok {
		return nil, 0, nil, fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
	}
	return bytes.NewReader(b), int64(len(b)), func() {}, nil
}

func (f *fakeFiles) CopyFile(ctx context.Context, src io.ReaderAt, size int64, dst string, m *Meter) error {
	f.mu.Lock()
	if _, ok := f.disks[dst]; ok {
		f.mu.Unlock()
		return ErrExists
	}
	f.disks[dst] = nil
	f.copies = append(f.copies, fileCopy{dst, size})
	hook := f.copyHook
	f.mu.Unlock()
	m.Set(0, size)
	if hook != nil {
		if err := hook(ctx, m); err != nil {
			f.drop(dst)
			return err
		}
	}
	b := make([]byte, size)
	if _, err := src.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		f.drop(dst)
		return err
	}
	m.Set(size, size)
	f.mu.Lock()
	f.disks[dst] = b
	f.mu.Unlock()
	return nil
}

func (f *fakeFiles) drop(p string) {
	f.mu.Lock()
	delete(f.disks, p)
	f.mu.Unlock()
}

func (f *fakeFiles) CheckDynamicVHDX(p string) error {
	f.mu.Lock()
	f.checked = append(f.checked, p)
	b, ok := f.disks[p]
	err := f.checkErr
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("open %s: %w", p, fs.ErrNotExist)
	}
	if err != nil {
		return err
	}
	return dynamicNoParent(bytes.NewReader(b), int64(len(b)))
}

func (f *fakeFiles) CompactVHDX(_ context.Context, p string, m *Meter) (int64, int64, error) {
	f.mu.Lock()
	f.compacted = append(f.compacted, p)
	hook, sizes, err := f.compactHook, f.compactSizes, f.compactErr
	f.mu.Unlock()
	if hook != nil {
		hook(m)
	}
	if err != nil {
		return 0, 0, err
	}
	return sizes[0], sizes[1], nil
}

func (f *fakeFiles) Rename(src, dst string, replace bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renames = append(f.renames, fileRename{src, dst, replace})
	b, ok := f.disks[src]
	if !ok {
		return fmt.Errorf("rename %s: %w", src, fs.ErrNotExist)
	}
	if _, there := f.disks[dst]; there && !replace {
		return ErrExists
	}
	delete(f.disks, src)
	f.disks[dst] = b
	return nil
}

func (f *fakeFiles) DeleteFile(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.disks[p]; ok {
		f.deleted = append(f.deleted, p)
		delete(f.disks, p)
	}
	return nil
}

func (f *fakeFiles) RootDisk(p string) (string, error) {
	f.mu.Lock()
	f.rootChecks = append(f.rootChecks, p)
	f.mu.Unlock()
	if f.rootErr != nil {
		return "", f.rootErr
	}
	return f.Canonical(p)
}

func (f *fakeFiles) roots() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rootChecks...)
}

func (f *fakeFiles) RemoveVMDir(dir, sid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, removal{dir: dir, vmRemoved: f.runner.called(scriptRemoveVM)})
	if f.removeErr != nil {
		return f.removeErr
	}
	key := dir[strings.LastIndexByte(dir, '\\')+1:]
	if sid != "" && f.dirAccount[key] != sid {
		return errors.New("not made for this account")
	}
	delete(f.dirAccount, key)
	return nil
}

func (f *fakeFiles) removedDirs() []removal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]removal(nil), f.removed...)
}

// disk is the content of the file at p in a VM directory, and whether
// it is there.
func (f *fakeFiles) disk(p string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.disks[p]
	return b, ok
}

// putDisk puts a file at p in a VM directory (whose account becomes the
// test caller's, as VMDir would have made it).
func (f *fakeFiles) putDisk(p string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks[p] = b
	dir := winDir(p)
	if f.dirAccount == nil {
		f.dirAccount = map[string]string{}
	}
	if _, ok := f.dirAccount[winBase(dir)]; !ok {
		f.dirAccount[winBase(dir)] = testSID
	}
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
	// files, when set, are where new-vhd creates its disk.
	files *fakeFiles
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
	switch script {
	case scriptGetVM:
		if r.vm == nil {
			return json.RawMessage(`{"exists":false}`), nil
		}
		b, _ := json.Marshal(r.vm)
		return b, nil
	case scriptNewVHD:
		var na struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(raw, &na)
		if r.files != nil {
			r.files.putDisk(na.Path, dynamicDisk)
		}
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

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
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
	server *Server
	client *Client

	mu sync.Mutex
	// finished, when set, receives the path of every request the server
	// has finished answering, even one whose client has gone.
	finished chan string
}

// finishedOn makes the harness report each request the server finishes.
func (h *harness) finishedOn() chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = make(chan string, 16)
	return h.finished
}

func newHarness(t *testing.T) *harness {
	runner := newFakeRunner()
	h := &harness{
		t:      t,
		runner: runner,
		files:  &fakeFiles{runner: runner, canon: map[string]string{}, parents: map[string]string{}, disks: map[string][]byte{}},
		caller: &fakeCaller{sid: testSID, dirs: map[string]bool{}, files: map[string]fakeEntry{}},
	}
	runner.files = h.files
	h.server = &Server{
		Runner:   h.runner,
		Files:    h.files,
		Root:     testRoot,
		CallerOf: func(*http.Request) (Caller, error) { return h.caller, nil },
		Logf:     t.Logf,
	}
	handler := h.server.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		h.mu.Lock()
		finished := h.finished
		h.mu.Unlock()
		if finished != nil {
			finished <- r.URL.Path
		}
	}))
	t.Cleanup(ts.Close)
	addr := strings.TrimPrefix(ts.URL, "http://")
	h.client = &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}}
	return h
}

const testCfg = `C:\Kivali\team1`

// testRoot is the broker's root disk in the tests (Server.Root).
const testRoot = `C:\Program Files\Kivali\vm\root.vhdx`

// testData is the data disk of testCfg's team, in its VM directory.
var testData = winJoin(vmDirFor(testCfg), DataDiskName)

func (h *harness) req() VMRequest {
	key := host.DirKey(testCfg)
	return VMRequest{
		Name:      VMName(key),
		ConfigDir: testCfg,
		CPUs:      4,
		MemoryMB:  4096,
		MAC:       MAC(key),
		ComPipe:   ComPipe(key),
	}
}

// allowCreate lets the caller write the config dir, and puts the data
// disk in the VM directory.
func (h *harness) allowCreate(r VMRequest) {
	h.caller.dirs[r.ConfigDir] = true
	h.files.putDisk(testData, dynamicDisk)
}

// ownVM makes get-vm report a VM owned by the caller in the given state,
// with the disks the backend attaches, the child's parent being the
// broker's root disk.
func (h *harness) ownVM(state string) *vmInfo {
	key := host.DirKey(testCfg)
	h.files.parents[winJoin(vmDirFor(testCfg), ChildDiskName)] = testRoot
	// The VM directory exists, made for the caller, as the VM's
	// configuration made it.
	h.files.mu.Lock()
	if h.files.dirAccount == nil {
		h.files.dirAccount = map[string]string{}
	}
	if _, ok := h.files.dirAccount[key]; !ok {
		h.files.dirAccount[key] = testSID
	}
	h.files.mu.Unlock()
	vm := &vmInfo{
		Exists: true, ID: "VMID-1", State: state, Notes: OwnerNotes(testSID),
		Path: vmDirFor(testCfg),
		CPUs: 4, MemoryStartupMB: 4096, MemoryMaxMB: 4096, DynamicMemory: true,
		MAC: MAC(key), Switch: DefaultSwitch, ComPipe: ComPipe(key),
		Disks: []vmDisk{
			{Controller: 0, Location: 0, Path: winJoin(vmDirFor(testCfg), ChildDiskName)},
			{Controller: 0, Location: 1, Path: testData},
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
	if ca.Data != testData || ca.Root != testRoot {
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

// A request that names a data disk is refused before anything opens or
// runs: the data disk is the broker's own file in the VM directory, and
// a client that still sends one (a supervisor older than the broker)
// learns so from the field's name.
func TestEnsureRefusesADataDiskFromTheCaller(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	body, _ := json.Marshal(r)
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	fields["data_vhdx"] = winJoin(testCfg, DataDiskName)
	body, _ = json.Marshal(fields)
	status, msg := h.post("/v1/vm", string(body))
	if status != http.StatusBadRequest || !strings.Contains(msg, "data_vhdx") {
		t.Fatalf("answer = %d %q, want 400 naming data_vhdx", status, msg)
	}
	if h.runner.count() != 0 || len(h.files.roots()) != 0 {
		t.Errorf("a refused request ran %d scripts and checked the root %d times", h.runner.count(), len(h.files.roots()))
	}
}

// The VM attaches the data disk in its VM directory, and only once it
// is there: without one, configuring the VM is refused, saying how to
// make one.
func TestEnsureNeedsTheDataDisk(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.caller.dirs[r.ConfigDir] = true
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "POST /v1/vhd") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if h.runner.called(scriptConfigureVM) {
		t.Error("configure-vm ran without a data disk")
	}
}

func TestEnsureRefusedWhenConfigDirNotWritable(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	// The caller cannot write the config dir.
	h.files.putDisk(testData, dynamicDisk)
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
	vm.Disks[1].Path = winJoin(testCfg, DataDiskName) // the data disk of an earlier layout
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "data disk") {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
}

// A running VM's root disk is compared by the parent the broker reads
// from its own differencing child (Files.ParentPath): another parent,
// or one that cannot be read, is a change of the root disk.
func TestEnsureRunningComparesTheChildsParent(t *testing.T) {
	child := winJoin(vmDirFor(testCfg), ChildDiskName)
	for _, tc := range []struct {
		what   string
		parent *string
		change bool
	}{
		{"the same parent, in another case", ptr(`c:\program files\kivali\vm\ROOT.vhdx`), false},
		{"another parent", ptr(`C:\Users\Public\root.vhdx`), true},
		{"unreadable", nil, true},
	} {
		h := newHarness(t)
		r := h.req()
		h.allowCreate(r)
		h.ownVM(StateRunning)
		delete(h.files.parents, child)
		if tc.parent != nil {
			h.files.parents[child] = *tc.parent
		}
		_, err := h.client.EnsureVM(context.Background(), r)
		if tc.change {
			if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "root disk") {
				t.Errorf("%s: err = %v (status %d)", tc.what, err, statusOf(err))
			}
		} else if err != nil {
			t.Errorf("%s: %v", tc.what, err)
		}
	}
}

func ptr(s string) *string { return &s }

// post sends body to path as raw JSON, past the client's types.
func (h *harness) post(path, body string) (int, string) {
	h.t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return h.client.Dial(ctx)
	}}}
	resp, err := hc.Post("http://broker"+path, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A request that names a root disk is refused before anything opens or
// runs: the root is the broker's own, and a client that still sends one
// learns so from the field's name.
func TestEnsureRefusesARootFromTheCaller(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	body, _ := json.Marshal(r)
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	fields["root_vhdx"] = `C:\Users\Public\root.vhdx`
	body, _ = json.Marshal(fields)
	status, msg := h.post("/v1/vm", string(body))
	if status != http.StatusBadRequest || !strings.Contains(msg, "root_vhdx") {
		t.Fatalf("answer = %d %q, want 400 naming root_vhdx", status, msg)
	}
	if h.runner.count() != 0 || len(h.files.roots()) != 0 {
		t.Errorf("a refused request ran %d scripts and checked the root %d times", h.runner.count(), len(h.files.roots()))
	}
}

// configure-vm gets the broker's root disk, as RootDisk made it
// canonical, whatever the caller sent.
func TestEnsureGivesTheBrokersRoot(t *testing.T) {
	h := newHarness(t)
	const configured = `C:\PROGRA~1\Kivali\vm\root.vhdx`
	h.server.Root = configured
	h.files.canon[configured] = testRoot
	r := h.req()
	h.allowCreate(r)
	if _, err := h.client.EnsureVM(context.Background(), r); err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	if len(h.files.roots()) != 1 || h.files.roots()[0] != configured {
		t.Errorf("root checks = %q, want [%s]", h.files.roots(), configured)
	}
	args, _ := h.runner.lastCall(scriptConfigureVM)
	var ca configureArgs
	if err := json.Unmarshal(args, &ca); err != nil {
		t.Fatal(err)
	}
	if ca.Root != testRoot {
		t.Errorf("configure-vm root = %q, want %s", ca.Root, testRoot)
	}
}

// A root disk that fails its check, or none, refuses every
// configuration with 503 and the reason, before any script runs.
func TestEnsureRefusedWithoutAUsableRoot(t *testing.T) {
	for _, tc := range []struct {
		what, root string
		err        error
		want       string
	}{
		{"differencing", testRoot, errors.New("not a fixed or dynamic VHDX: a differencing disk"), "differencing"},
		{"missing", testRoot, errors.New("The system cannot find the file specified."), "cannot find"},
		{"none configured", "", nil, "no root disk"},
	} {
		h := newHarness(t)
		h.server.Root = tc.root
		h.files.rootErr = tc.err
		r := h.req()
		h.allowCreate(r)
		h.ownVM(StateOff)
		_, err := h.client.EnsureVM(context.Background(), r)
		if statusOf(err) != http.StatusServiceUnavailable || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.root) {
			t.Errorf("%s: err = %v (status %d)", tc.what, err, statusOf(err))
		}
		if h.runner.count() != 0 {
			t.Errorf("%s: %d scripts ran", tc.what, h.runner.count())
		}
	}
}

// A running VM's child is compared with the broker's root disk: a VM
// made from another root (the one an earlier broker had) is a change of
// the root disk, left for a stop.
func TestEnsureRunningComparesWithTheBrokersRoot(t *testing.T) {
	h := newHarness(t)
	r := h.req()
	h.allowCreate(r)
	h.ownVM(StateRunning)
	h.server.Root = `D:\Kivali\vm\root.vhdx`
	_, err := h.client.EnsureVM(context.Background(), r)
	if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "root disk") {
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
	if len(h.caller.pinned) != 0 {
		t.Errorf("a start opened the caller's files: %v", h.caller.pinned)
	}
}

// A start refuses a VM whose data disk slot holds anything but the
// broker's data disk in the VM directory of the VM's key: a file in the
// config directory (where the caller could write it, as an earlier
// layout kept it), another team's, or the right path reached through a
// link.
func TestStartRefusesADataDiskOutsideTheVMDir(t *testing.T) {
	for why, set := range map[string]func(h *harness, vm *vmInfo){
		"in the config directory": func(_ *harness, vm *vmInfo) { vm.Disks[1].Path = winJoin(testCfg, DataDiskName) },
		"elsewhere":               func(_ *harness, vm *vmInfo) { vm.Disks[1].Path = `C:\Elsewhere\data.vhdx` },
		"another team's": func(_ *harness, vm *vmInfo) {
			vm.Disks[1].Path = winJoin(vmDirFor(`C:\Kivali\team2`), DataDiskName)
		},
		"through a link": func(h *harness, _ *vmInfo) { h.files.canon[testData] = `C:\Users\Public\data.vhdx` },
	} {
		t.Run(why, func(t *testing.T) {
			h := newHarness(t)
			set(h, h.ownVM(StateOff))
			_, err := h.client.Start(context.Background(), VMName(host.DirKey(testCfg)))
			if statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "data disk") {
				t.Fatalf("err = %v (status %d)", err, statusOf(err))
			}
			if h.runner.called(scriptStartVM) {
				t.Error("a VM whose data disk is not the broker's was started")
			}
		})
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

// The VM directory holds the team's data disk (or its snapshot): the
// VM goes, its directory stays, with the disk, for DELETE /v1/vhd. The
// same holds without a VM.
func TestRemoveKeepsAVMDirHoldingData(t *testing.T) {
	key := host.DirKey(testCfg)
	for _, name := range []string{DataDiskName, SnapshotName} {
		for _, vm := range []bool{true, false} {
			h := newHarness(t)
			if vm {
				h.ownVM(StateOff)
			}
			h.files.putDisk(winJoin(vmDirFor(testCfg), name), dynamicDisk)
			if err := h.client.Remove(context.Background(), VMName(key)); err != nil {
				t.Fatalf("%s, VM %v: Remove: %v", name, vm, err)
			}
			if vm && !h.runner.called(scriptRemoveVM) {
				t.Errorf("%s: the VM was not removed", name)
			}
			if got := h.files.removedDirs(); len(got) != 0 {
				t.Errorf("%s, VM %v: removed %+v", name, vm, got)
			}
			if _, ok := h.files.disk(winJoin(vmDirFor(testCfg), name)); !ok {
				t.Errorf("%s, VM %v: the disk is gone", name, vm)
			}
		}
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

// The data disk is created straight into the team's VM directory as
// data.vhdx, as the broker: New-VHD is given that path and no other,
// and the caller is granted nothing (the file takes the directory's
// DACL, which lets the caller read it).
func TestNewVHD(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	res, err := h.client.NewVHD(context.Background(), testCfg, 64<<30, false)
	if err != nil {
		t.Fatalf("NewVHD: %v", err)
	}
	if res.Path != testData {
		t.Fatalf("path = %q, want %s", res.Path, testData)
	}
	args, ok := h.runner.lastCall(scriptNewVHD)
	if !ok {
		t.Fatal("new-vhd not run")
	}
	var na map[string]any
	_ = json.Unmarshal(args, &na)
	if na["path"] != testData || na["size_bytes"] != float64(64<<30) || len(na) != 2 {
		t.Fatalf("new-vhd args = %v", na)
	}
	if len(h.files.vmDirs) != 1 || h.files.vmDirs[0] != host.DirKey(testCfg) {
		t.Fatalf("VM directories made = %v", h.files.vmDirs)
	}
	if len(h.files.renames) != 0 || len(h.caller.pinned) != 0 {
		t.Fatalf("renames %v, caller's files opened %v: the disk is made in place", h.files.renames, h.caller.pinned)
	}
}

// A data disk already there is replaced only when asked (a disk whose
// first boot never reached READY); otherwise 409 and nothing runs.
func TestNewVHDReplacesOnlyWhenAsked(t *testing.T) {
	h := newHarness(t)
	h.caller.dirs[testCfg] = true
	h.files.putDisk(testData, []byte("the team's disk"))
	_, err := h.client.NewVHD(context.Background(), testCfg, 64<<30, false)
	if statusOf(err) != http.StatusConflict {
		t.Fatalf("err = %v (status %d)", err, statusOf(err))
	}
	if b, _ := h.files.disk(testData); string(b) != "the team's disk" || h.runner.called(scriptNewVHD) {
		t.Fatalf("the disk was touched: %q, new-vhd ran %v", b, h.runner.called(scriptNewVHD))
	}
	if _, err := h.client.NewVHD(context.Background(), testCfg, 64<<30, true); err != nil {
		t.Fatalf("NewVHD with replace: %v", err)
	}
	if b, _ := h.files.disk(testData); !bytes.Equal(b, dynamicDisk) {
		t.Fatalf("the disk was not replaced: %q", b)
	}
}

// A disk is made only for the caller's own VM, and only while it is off
// or absent, and only in a VM directory made for the caller.
func TestNewVHDRefusals(t *testing.T) {
	for _, tc := range []struct {
		what   string
		setup  func(h *harness)
		status int
	}{
		{"a running VM", func(h *harness) { h.ownVM(StateRunning) }, http.StatusConflict},
		{"another account's VM", func(h *harness) { h.ownVM(StateOff).Notes = OwnerNotes("S-1-5-21-111-222-333-2002") }, http.StatusForbidden},
		{"another account's VM directory", func(h *harness) {
			_, _, _ = h.files.VMDir(host.DirKey(testCfg), "S-1-5-21-111-222-333-2002")
		}, http.StatusForbidden},
		{"a config directory the caller cannot write", func(h *harness) { h.caller.dirs[testCfg] = false }, http.StatusForbidden},
	} {
		h := newHarness(t)
		h.caller.dirs[testCfg] = true
		tc.setup(h)
		_, err := h.client.NewVHD(context.Background(), testCfg, 64<<30, true)
		if statusOf(err) != tc.status {
			t.Errorf("%s: err = %v (status %d), want %d", tc.what, err, statusOf(err), tc.status)
		}
		if h.runner.called(scriptNewVHD) {
			t.Errorf("%s: new-vhd ran", tc.what)
		}
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
		{"optimize", func(h *harness) error {
			h.caller.dirs[testCfg] = true
			_, err := h.client.OptimizeVHD(context.Background(), testCfg)
			return err
		}, true},
		{"import", func(h *harness) error {
			h.caller.dirs[testCfg] = true
			h.files.drop(testData)
			h.caller.files[winJoin(testCfg, DataDiskName)] = fakeEntry{read: true}
			_, err := h.client.ImportVHD(context.Background(), testCfg)
			return err
		}, true},
		{"snapshot", func(h *harness) error {
			h.caller.dirs[testCfg] = true
			_, err := h.client.SnapshotVHD(context.Background(), testCfg)
			return err
		}, true},
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
		h.files.putDisk(testData, dynamicDisk)
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
// key (a first up that made the directory but never the VM leaves one
// behind, which the account cannot delete), if it is there and
// canonical and holds no data disk; RemoveVMDir then checks it was made
// for the caller.
func TestRemoveOrphanVMDir(t *testing.T) {
	key := host.DirKey(testCfg)
	vmDir := vmDirFor(testCfg)
	t.Run("removed", func(t *testing.T) {
		h := newHarness(t)
		_, _, _ = h.files.VMDir(key, testSID)
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
	t.Run("nothing there", func(t *testing.T) {
		h := newHarness(t)
		if err := h.client.Remove(context.Background(), VMName(key)); !IsNotFound(err) {
			t.Fatalf("err = %v (status %d)", err, statusOf(err))
		}
	})
	t.Run("refused by the directory's own check", func(t *testing.T) {
		h := newHarness(t)
		_, _, _ = h.files.VMDir(key, "S-1-5-21-111-222-333-2002")
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
