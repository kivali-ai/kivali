package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// Caller is the pipe client of one request, as the broker sees it
// through impersonation: every check runs with the caller's token, so a
// path the caller cannot open fails here exactly as it would for them.
// Each method that opens a path returns it made canonical from the
// handle it opened (GetFinalPathNameByHandle), refusing it if any
// component is a reparse point, and keeps a handle on it open until
// Close: what was checked cannot be renamed, removed or replaced by a
// link while the request runs.
type Caller interface {
	// SID is the caller's user SID, from the impersonation token.
	SID() string
	// WritableDir opens dir as a directory the caller can add files to.
	WritableDir(dir string) (string, error)
	// File opens an existing regular file with one link that the caller
	// can read, and pins it: the handle kept reads the file's data and
	// shares no delete, since only data access and delete take part in
	// sharing (a handle with no data access would pin nothing).
	File(p string) (string, error)
	// Contents reads the file File pinned at p (the canonical path File
	// returned) through the pinning handle, so what is read is the file
	// that was checked, and gives its size.
	Contents(p string) (io.ReaderAt, int64, error)
	// Close releases what the checks hold open.
	Close() error
}

// FileInfo is what Files.Stat reports of a file: its logical size and
// the bytes the volume allocates to it.
type FileInfo struct {
	Size, Used int64
}

// Files is what the broker does on the file system as itself. Every
// path it is given is in a VM directory, which only the broker can
// write, and every open of one is made without following a link at it.
type Files interface {
	// VMDir makes the VM directory of a config directory key if it is
	// absent, owned by the broker, with a protected DACL (SYSTEM,
	// Administrators and the service's account in full control, and
	// callerSID reading), opens it, checks it is a directory the broker
	// made (brokerOwned) and no link, and keeps it open without delete
	// sharing until release. The directory is bound to the account it
	// was made for: one whose DACL grants another account (DirAccount)
	// is refused, since it holds that account's data disk. The caller
	// cannot write in it, so nothing there is a link the caller planted:
	// the broker creates files there as itself.
	VMDir(key, callerSID string) (dir string, release func(), err error)
	// VMDirPath is the VM directory of a config directory key, whether
	// or not it exists: <the broker's VM root>\<key>.
	VMDirPath(key string) string
	// Canonical is p's final path as the broker sees it, refusing a
	// reparse point along it.
	Canonical(p string) (string, error)
	// ParentPath is the parent the locator of the differencing VHDX at
	// p, a file in a VM directory, names: its bytes parsed through a
	// handle opened without following a link, the parent never opened.
	ParentPath(p string) (string, error)
	// Stat reports the plain file at p; an error wrapping
	// fs.ErrNotExist when nothing is there.
	Stat(p string) (FileInfo, error)
	// Open opens the plain file at p for reading until release.
	Open(p string) (r io.ReaderAt, size int64, release func(), err error)
	// CopyFile copies size bytes of src into dst, a new file (anything
	// at dst, a link included, fails it), flushes it and reports the
	// bytes copied to m as it goes. A src that reads a sparse file stays
	// sparse: only its allocated ranges are copied, into a sparse dst. A
	// copy that fails or is cancelled (ctx) deletes dst.
	CopyFile(ctx context.Context, src io.ReaderAt, size int64, dst string, m *Meter) error
	// CheckDynamicVHDX refuses p unless it is a dynamic VHDX with no
	// parent (an error wrapping errNotDynamic): its region table,
	// metadata and block table read through a handle, and then the
	// virtual disk opened without opening any parent its locator names
	// (which replays the VHDX's log) and its subtype read through that
	// handle.
	CheckDynamicVHDX(p string) error
	// CompactVHDX compacts the dynamic VHDX at p and gives the file's
	// size before and after: the virtual disk is opened without opening
	// any parent its locator names, refused through that handle unless
	// it is a dynamic VHDX (an error wrapping errNotDynamic), and
	// compacted through it, until ctx ends; m reads the compaction's
	// progress while it runs.
	CompactVHDX(ctx context.Context, p string, m *Meter) (before, after int64, err error)
	// Rename moves src to dst durably (MOVEFILE_WRITE_THROUGH),
	// replacing a file at dst only with replace (ErrExists otherwise).
	Rename(src, dst string, replace bool) error
	// DeleteFile deletes p, through a handle opened on it (a link is
	// deleted as the link); nothing at p is no error.
	DeleteFile(p string) error
	// RemoveVMDir deletes the VM directory dir (a canonical VM directory)
	// and everything in it, after checking, on a handle opened without
	// following a link, that it is a directory the broker made, no
	// reparse point, and canonically dir, and, with a callerSID, that it
	// was made for that caller (DirAccount). Entries are opened the same
	// way and deleted through their handles, so a link inside is deleted
	// as a link and never followed.
	RemoveVMDir(dir, callerSID string) error
	// RootDisk checks the broker's root disk at p (Server.Root) and
	// returns its canonical path: p made canonical (Canonical), opened as
	// a VHDX for its metadata only and without opening any parent its
	// locator names, and refused unless its provider subtype is fixed or
	// dynamic. A differencing root would have Hyper-V open whatever parent
	// it names; the install directory's DACL is what keeps one out, and
	// this check is a second line.
	RootDisk(p string) (string, error)
}

// ErrExists is Files.Rename's answer when something is at the
// destination and it may not be replaced.
var ErrExists = errors.New("something already exists there")

// Meter is how far one long disk operation has got, which POST
// /v1/vhd/progress reads while it runs: either the operation sets it as
// it goes (Set), or it gives a function that reads its progress on
// demand (Reader), which is called under the meter's lock, so an
// operation that detaches it (Reader(nil)) before it frees what the
// function reads is never read after. A nil Meter takes nothing.
type Meter struct {
	mu          sync.Mutex
	done, total int64
	read        func() (done, total int64)
}

// Set records done of total.
func (m *Meter) Set(done, total int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.done, m.total = done, total
	m.mu.Unlock()
}

// Reader makes read the source of the progress; nil detaches it,
// keeping the last value read.
func (m *Meter) Reader(read func() (done, total int64)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.read = read
	m.mu.Unlock()
}

// Get is the progress now.
func (m *Meter) Get() (done, total int64) {
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.read != nil {
		m.done, m.total = m.read()
	}
	return m.done, m.total
}

// Runner runs one of the broker's embedded PowerShell scripts (by name)
// with args as its JSON input on stdin, and returns the JSON object the
// script printed. Nothing a caller sent is ever part of a command line.
type Runner interface {
	Run(ctx context.Context, script string, args any) (json.RawMessage, error)
}

// Error is an answer other than 200: an HTTP status and one sentence.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func answer(status int, format string, args ...any) *Error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// Server is the broker's HTTP handler and the policy it applies.
type Server struct {
	Runner Runner
	Files  Files
	// Root is the root disk every VM's differencing child has as its
	// parent (RootPath: beside the broker's executable, or the console
	// broker's --root), checked (Files.RootDisk) on every POST /v1/vm
	// before anything else is opened or run.
	Root string
	// CallerOf identifies the caller of a request (on Windows, by
	// impersonating the pipe client of the request's connection).
	CallerOf func(*http.Request) (Caller, error)
	Logf     func(format string, args ...any)

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// running is the long disk operation in flight on each VM directory
	// (by config directory key), for POST /v1/vhd/progress, which must
	// not wait for the VM's lock that operation holds.
	runningMu sync.Mutex
	running   map[string]*inflight
}

// inflight is one long disk operation as POST /v1/vhd/progress reports
// it: what it is, the account whose request runs it, and its meter.
type inflight struct {
	op, sid string
	meter   *Meter
}

// track records a long operation on the VM directory of key, run for
// sid, until the returned function is called.
func (s *Server) track(key, op, sid string) (*Meter, func()) {
	m := &Meter{}
	s.runningMu.Lock()
	if s.running == nil {
		s.running = map[string]*inflight{}
	}
	s.running[key] = &inflight{op: op, sid: sid, meter: m}
	s.runningMu.Unlock()
	return m, func() {
		s.runningMu.Lock()
		delete(s.running, key)
		s.runningMu.Unlock()
	}
}

// OptimizeTimeout bounds a compaction. It moves the blocks after a
// hole down into it, so its cost is the data moved: measured at about a
// second for 2.5 GB on an SSD, which puts the whole of a 64 GiB disk at
// a minute or two there and at some minutes on a slow disk. The bound is
// generous because the compaction is cancelled past it, and a
// compaction cut short is left to the VHDX's own log to tidy.
const OptimizeTimeout = 20 * time.Minute

// ImportTimeout bounds an import's copy (and SnapshotTimeout an upgrade
// snapshot's): the whole of a full 64 GiB disk at 30 MB/s, a slow hard
// disk's rate, is about 40 minutes. A copy cut short is deleted.
const (
	ImportTimeout   = 60 * time.Minute
	SnapshotTimeout = 60 * time.Minute
)

// How long each script may run.
var scriptTimeouts = map[string]time.Duration{
	scriptGetVM:       time.Minute,
	scriptConfigureVM: 5 * time.Minute,
	scriptStartVM:     2 * time.Minute,
	scriptStopVM:      6 * time.Minute, // a graceful stop waits for the guest
	scriptRemoveVM:    3 * time.Minute,
	scriptNewVHD:      3 * time.Minute,
	scriptSetupStatus: time.Minute,
	scriptSetupApply:  time.Minute,
}

// getVMArgs is get-vm's input. StateOnly asks for the VM's id, state,
// notes (its owner) and assigned memory and nothing else: the devices
// (the disks, the memory settings, the adapter, the COM port) cost a
// cmdlet each, and only a configuration, a start and a removal check
// them, while the backend polls a plain state query for as long as the
// VM runs.
type getVMArgs struct {
	Name      string `json:"name"`
	StateOnly bool   `json:"state_only"`
}

// The VM as get-vm reports it. With StateOnly, the fields after
// MemoryAssignedMB are zero.
type vmInfo struct {
	Exists           bool     `json:"exists"`
	ID               string   `json:"id"`
	State            string   `json:"state"`
	Notes            string   `json:"notes"`
	MemoryAssignedMB uint64   `json:"memory_assigned_mb"`
	Path             string   `json:"path"` // the configuration location: the VM directory it was made in
	CPUs             uint     `json:"cpus"`
	MemoryStartupMB  uint64   `json:"memory_startup_mb"`
	MemoryMaxMB      uint64   `json:"memory_max_mb"`
	DynamicMemory    bool     `json:"dynamic_memory"`
	MAC              string   `json:"mac"`
	Switch           string   `json:"switch"`
	ComPipe          string   `json:"com_pipe"`
	Disks            []vmDisk `json:"disks"`
}

// vmDisk is an attached disk as get-vm reports it: where it is attached
// and its file. What a disk's file says (its parent) is read by the
// broker itself (Files.ParentPath), never by a cmdlet, which would open
// the parents a file's locator names.
type vmDisk struct {
	Controller int    `json:"controller"`
	Location   int    `json:"location"`
	Path       string `json:"path"`
}

func (v vmInfo) disk(location int) (vmDisk, bool) {
	for _, d := range v.Disks {
		if d.Controller == 0 && d.Location == location {
			return d, true
		}
	}
	return vmDisk{}, false
}

// configureArgs is configure-vm's input: every value already checked.
type configureArgs struct {
	Name        string `json:"name"`
	VMDir       string `json:"vm_dir"`
	Child       string `json:"child"`
	Root        string `json:"root"`
	Data        string `json:"data"`
	CPUs        uint   `json:"cpus"`
	MemoryBytes uint64 `json:"memory_bytes"`
	MAC         string `json:"mac"`
	ComPipe     string `json:"com_pipe"`
	Switch      string `json:"switch"`
	Notes       string `json:"notes"`
}

// What a reconfiguration may change on a running VM: nothing, but these
// two wait for the next configuration while off instead of refusing.
const (
	changeMemory = "memory"
	changeCPUs   = "CPUs"
)

// diff names what configuring want would change on cur, whose root
// disk's parent is rootParent ("" when it could not be read).
func diff(cur vmInfo, want configureArgs, rootParent string) []string {
	var changes []string
	root, ok := cur.disk(0)
	if !ok || !strings.EqualFold(root.Path, want.Child) || !strings.EqualFold(rootParent, want.Root) {
		changes = append(changes, "root disk")
	}
	if data, ok := cur.disk(1); !ok || !strings.EqualFold(data.Path, want.Data) {
		changes = append(changes, "data disk")
	}
	if !strings.EqualFold(cur.MAC, want.MAC) {
		changes = append(changes, "MAC")
	}
	if !strings.EqualFold(cur.ComPipe, want.ComPipe) {
		changes = append(changes, "console pipe")
	}
	if cur.Switch != want.Switch {
		changes = append(changes, "network switch")
	}
	mb := want.MemoryBytes >> 20
	if !cur.DynamicMemory || cur.MemoryStartupMB != mb || cur.MemoryMaxMB != mb {
		changes = append(changes, changeMemory)
	}
	if cur.CPUs != want.CPUs {
		changes = append(changes, changeCPUs)
	}
	return changes
}

// Handler serves the broker's interface (docs/developers/supervisor.md, "The
// broker").
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vm", s.handle(s.ensureVM))
	mux.HandleFunc("GET /v1/vm/{name}", s.handle(s.getVM))
	mux.HandleFunc("POST /v1/vm/{name}/start", s.handle(s.startVM))
	mux.HandleFunc("POST /v1/vm/{name}/stop", s.handle(s.stopVM))
	mux.HandleFunc("DELETE /v1/vm/{name}", s.handle(s.removeVM))
	mux.HandleFunc("POST /v1/vhd", s.handle(s.newVHD))
	mux.HandleFunc("DELETE /v1/vhd", s.handle(s.deleteVHD))
	mux.HandleFunc("POST /v1/vhd/optimize", s.handle(s.optimizeVHD))
	mux.HandleFunc("POST /v1/vhd/import", s.handle(s.importVHD))
	mux.HandleFunc("POST /v1/vhd/snapshot", s.handle(s.snapshotVHD))
	mux.HandleFunc("DELETE /v1/vhd/snapshot", s.handle(s.deleteSnapshot))
	mux.HandleFunc("POST /v1/vhd/restore", s.handle(s.restoreVHD))
	mux.HandleFunc("POST /v1/vhd/progress", s.handle(s.vhdProgress))
	mux.HandleFunc("GET /v1/setup", s.handle(s.setupStatus))
	mux.HandleFunc("POST /v1/setup", s.handle(s.setupApply))
	return mux
}

// handle identifies the caller (every request has one, whatever it
// asks), runs fn, logs the outcome and writes the JSON answer or the
// error's status and sentence.
func (s *Server) handle(fn func(r *http.Request, c Caller) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.CallerOf(r)
		if err != nil {
			s.logf("%s %s: caller refused: %v", r.Method, r.URL.Path, err)
			http.Error(w, "refused: the caller could not be identified: "+err.Error(), http.StatusForbidden)
			return
		}
		defer func() { _ = c.Close() }()
		res, err := fn(r, c)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) {
				e = answer(http.StatusInternalServerError, "%v", err)
			}
			s.logf("%s %s by %s: %d %s", r.Method, r.URL.Path, c.SID(), e.Status, e.Msg)
			http.Error(w, e.Msg, e.Status)
			return
		}
		// A progress query comes every second while an import or a
		// compaction runs; only its refusals are logged.
		if r.URL.Path != "/v1/vhd/progress" {
			s.logf("%s %s by %s: ok", r.Method, r.URL.Path, c.SID())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// lock serialises the requests on one VM (or one disk).
func (s *Server) lock(key string) (unlock func()) {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// decode reads a JSON body strictly: at most 64 KiB, no unknown fields,
// nothing after the object. An empty body leaves v as it is.
func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
	if err != nil {
		return answer(http.StatusBadRequest, "the request body could not be read: %v", err)
	}
	if len(b) > 64<<10 {
		return answer(http.StatusRequestEntityTooLarge, "the request body is larger than 64 KiB")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return answer(http.StatusBadRequest, "the request body is not the expected JSON: %v", err)
	}
	if dec.More() {
		return answer(http.StatusBadRequest, "the request body holds more than one JSON value")
	}
	return nil
}

// run runs a script with its timeout and decodes its output into out.
func (s *Server) run(script string, args, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeouts[script])
	defer cancel()
	raw, err := s.Runner.Run(ctx, script, args)
	if err != nil {
		return answer(http.StatusInternalServerError, "Hyper-V (%s): %v", script, err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return answer(http.StatusInternalServerError, "Hyper-V (%s): unexpected output: %v", script, err)
		}
	}
	return nil
}

// inspect runs get-vm: the VM's state only, or with its devices too.
func (s *Server) inspect(name string, stateOnly bool) (vmInfo, error) {
	var cur vmInfo
	err := s.run(scriptGetVM, getVMArgs{Name: name, StateOnly: stateOnly}, &cur)
	return cur, err
}

// ownedBy is the ownership rule: a VM is touched only by the account
// whose SID its notes carry.
func ownedBy(cur vmInfo, name, sid string) error {
	owner, ok := OwnerOf(cur.Notes)
	if !ok {
		return answer(http.StatusForbidden, "refused: %s was not made by the broker (its notes name no owner)", name)
	}
	if !strings.EqualFold(owner, sid) {
		return answer(http.StatusForbidden, "refused: %s belongs to another account", name)
	}
	return nil
}

// existing finds the caller's own VM of the request's {name}, with its
// devices unless stateOnly (getVMArgs).
func (s *Server) existing(r *http.Request, c Caller, stateOnly bool) (string, vmInfo, error) {
	name := r.PathValue("name")
	if err := ValidName(name); err != nil {
		return "", vmInfo{}, answer(http.StatusBadRequest, "%v", err)
	}
	cur, err := s.inspect(name, stateOnly)
	if err != nil {
		return "", vmInfo{}, err
	}
	if !cur.Exists {
		return "", vmInfo{}, answer(http.StatusNotFound, "there is no VM named %s", name)
	}
	if err := ownedBy(cur, name, c.SID()); err != nil {
		return "", vmInfo{}, err
	}
	return name, cur, nil
}

func refused(what string, err error) error {
	if errors.Is(err, ErrExists) {
		return answer(http.StatusConflict, "%s: %v", what, err)
	}
	return answer(http.StatusForbidden, "refused: %s: %v", what, err)
}

// rootDisk is the broker's root disk, checked (Files.RootDisk): 503
// when it is missing or not a disk the broker boots from, which is the
// installation's fault, not the request's.
func (s *Server) rootDisk() (string, error) {
	if s.Root == "" {
		return "", answer(http.StatusServiceUnavailable, "the broker has no root disk; reinstall Kivali, or start a console broker with --root")
	}
	root, err := s.Files.RootDisk(s.Root)
	if err != nil {
		return "", answer(http.StatusServiceUnavailable, "the broker's root disk %s cannot be used (%v); reinstall Kivali", s.Root, err)
	}
	return root, nil
}

// ensureVM is POST /v1/vm: the VM exists with exactly the requested
// configuration (created, or reconfigured while off), or the request is
// refused. Its root disk is the broker's own (Server.Root), never one a
// caller names.
func (s *Server) ensureVM(r *http.Request, c Caller) (any, error) {
	var req VMRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, answer(http.StatusBadRequest, "%v", err)
	}
	root, err := s.rootDisk()
	if err != nil {
		return nil, err
	}
	defer s.lock(req.Name)()

	cfg, err := c.WritableDir(req.ConfigDir)
	if err != nil {
		return nil, refused("config_dir", err)
	}
	key := host.DirKey(cfg)
	if req.Name != VMName(key) {
		return nil, answer(http.StatusForbidden, "refused: the VM of %s is %s, not %s", cfg, VMName(key), req.Name)
	}
	if !strings.EqualFold(req.MAC, MAC(key)) {
		return nil, answer(http.StatusForbidden, "refused: the MAC of %s is %s, not %s", cfg, MAC(key), req.MAC)
	}
	if !strings.EqualFold(req.ComPipe, ComPipe(key)) {
		return nil, answer(http.StatusForbidden, "refused: the console pipe of %s is %s, not %s", cfg, ComPipe(key), req.ComPipe)
	}

	cur, err := s.inspect(req.Name, false)
	if err != nil {
		return nil, err
	}
	if cur.Exists {
		if err := ownedBy(cur, req.Name, c.SID()); err != nil {
			return nil, err
		}
	}
	running := cur.Exists && cur.State != StateOff
	vmDir, release, err := s.Files.VMDir(key, c.SID())
	if err != nil {
		return nil, refused("the VM directory", err)
	}
	defer release()
	// The data disk is the broker's own file in the VM directory, which
	// the caller can read and never write (POST /v1/vhd made it, or POST
	// /v1/vhd/import copied it there).
	data := winJoin(vmDir, DataDiskName)
	if !running {
		if _, err := s.Files.Stat(data); errors.Is(err, fs.ErrNotExist) {
			return nil, answer(http.StatusConflict, "%s has no data disk; make one with POST /v1/vhd, or import one with POST /v1/vhd/import", req.Name)
		} else if err != nil {
			return nil, answer(http.StatusInternalServerError, "the data disk %s: %v", data, err)
		}
	}

	args := configureArgs{
		Name:        req.Name,
		VMDir:       vmDir,
		Child:       winJoin(vmDir, ChildDiskName),
		Root:        root,
		Data:        data,
		CPUs:        req.CPUs,
		MemoryBytes: req.MemoryMB << 20,
		MAC:         strings.ToUpper(req.MAC),
		ComPipe:     ComPipe(key),
		Switch:      DefaultSwitch,
		Notes:       OwnerNotes(c.SID()),
	}
	if running {
		// The running child's parent, read from the broker's own file in
		// its VM directory, which the caller cannot write.
		rootParent := ""
		if d, ok := cur.disk(0); ok && strings.EqualFold(d.Path, args.Child) {
			if rootParent, err = s.Files.ParentPath(args.Child); err != nil {
				s.logf("%s: the parent of %s: %v", req.Name, args.Child, err)
			}
		}
		changes := diff(cur, args, rootParent)
		var other []string
		for _, ch := range changes {
			if ch != changeMemory && ch != changeCPUs {
				other = append(other, ch)
			}
		}
		if len(other) > 0 {
			return nil, answer(http.StatusConflict, "%s is running; stop it to change its %s", req.Name, strings.Join(other, ", "))
		}
		return VMState{ID: cur.ID, State: cur.State, MemoryAssignedMB: cur.MemoryAssignedMB, Deferred: changes}, nil
	}
	var st VMState
	if err := s.run(scriptConfigureVM, args, &st); err != nil {
		return nil, err
	}
	return st, nil
}

// getVM is GET /v1/vm/{name}: the state only, since the backend polls it
// while the VM runs.
func (s *Server) getVM(r *http.Request, c Caller) (any, error) {
	_, cur, err := s.existing(r, c, true)
	if err != nil {
		return nil, err
	}
	return VMState{ID: cur.ID, State: cur.State, MemoryAssignedMB: cur.MemoryAssignedMB}, nil
}

// checkDisks re-derives, before a start, that the VM's disks are still
// the ones its configuration attached: both, canonically, the broker's
// own files in the VM directory of the VM's key, the root disk's
// differencing child and the data disk, which the caller cannot write.
// Between requests nothing is pinned, so a start checks again.
func (s *Server) checkDisks(name string, cur vmInfo) error {
	key := KeyOf(name)
	root, okRoot := cur.disk(0)
	data, okData := cur.disk(1)
	if !okRoot || !okData {
		return errors.New("its disks are not the two the broker attaches")
	}
	for _, d := range []struct{ what, path, want string }{
		{"root disk", root.Path, winJoin(s.Files.VMDirPath(key), ChildDiskName)},
		{"data disk", data.Path, winJoin(s.Files.VMDirPath(key), DataDiskName)},
	} {
		p, err := s.Files.Canonical(d.path)
		if err != nil {
			return fmt.Errorf("the %s: %w", d.what, err)
		}
		if !strings.EqualFold(p, d.want) {
			return fmt.Errorf("the %s is %s, not %s", d.what, p, d.want)
		}
	}
	if !strings.EqualFold(cur.ComPipe, ComPipe(key)) {
		return fmt.Errorf("its console pipe is %q, not %s", cur.ComPipe, ComPipe(key))
	}
	return nil
}

func (s *Server) startVM(r *http.Request, c Caller) (any, error) {
	defer s.lock(r.PathValue("name"))()
	name, cur, err := s.existing(r, c, false)
	if err != nil {
		return nil, err
	}
	if cur.State != StateOff {
		return VMState{ID: cur.ID, State: cur.State, MemoryAssignedMB: cur.MemoryAssignedMB}, nil
	}
	if err := s.checkDisks(name, cur); err != nil {
		return nil, answer(http.StatusConflict, "%s no longer matches its configuration (%v); configure it again with POST /v1/vm", name, err)
	}
	var st VMState
	if err := s.run(scriptStartVM, map[string]string{"name": name}, &st); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Server) stopVM(r *http.Request, c Caller) (any, error) {
	var req StopRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	defer s.lock(r.PathValue("name"))()
	// A stop needs only the state: it touches no file.
	name, cur, err := s.existing(r, c, true)
	if err != nil {
		return nil, err
	}
	if cur.State == StateOff {
		return VMState{ID: cur.ID, State: cur.State}, nil
	}
	var st VMState
	err = s.run(scriptStopVM, struct {
		Name     string `json:"name"`
		Graceful bool   `json:"graceful"`
	}{name, req.Graceful}, &st)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// isVMDirOf reports whether p, a canonical path, is the broker's VM
// directory of the VM name: the one of the name's key under the
// broker's VM root.
func (s *Server) isVMDirOf(p, name string) bool {
	return strings.EqualFold(p, s.Files.VMDirPath(KeyOf(name)))
}

// removeVM is DELETE /v1/vm/{name}: the VM, its differencing child and
// the broker's VM directory, never the data disk. The child is deleted
// only if it is, canonically, the broker's child in the VM directory of
// the VM's own key; otherwise it is left where it is and the VM is
// removed all the same. The VM directory goes last, after Remove-VM,
// and only once it holds neither the data disk nor its snapshot
// (holdsData): the team's data goes only with DELETE /v1/vhd. Without a
// VM, the VM directory of the name's key is removed on its own, on the
// same terms and if it was made for the caller (removeOrphanDir: a team
// whose first up made the directory but never the VM).
func (s *Server) removeVM(r *http.Request, c Caller) (any, error) {
	defer s.lock(r.PathValue("name"))()
	name, cur, err := s.existing(r, c, false)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Status == http.StatusNotFound {
			return s.removeOrphanDir(r.PathValue("name"), c)
		}
		return nil, err
	}
	child := ""
	if root, ok := cur.disk(0); ok && root.Path != "" {
		p, err := s.Files.Canonical(root.Path)
		switch {
		case err != nil:
			s.logf("%s: leaving its root disk %s: %v", name, root.Path, err)
		case !strings.EqualFold(winBase(p), ChildDiskName) || !s.isVMDirOf(winDir(p), name):
			s.logf("%s: leaving its root disk %s: not the broker's differencing child of this VM", name, p)
		default:
			child = p
		}
	}
	vmDir := s.vmDirOf(name, cur, child)
	err = s.run(scriptRemoveVM, struct {
		Name  string `json:"name"`
		Child string `json:"child"`
	}{name, child}, nil)
	if err != nil {
		return nil, err
	}
	if vmDir == "" {
		return struct{}{}, nil
	}
	if s.holdsData(vmDir) {
		s.logf("%s: keeping its directory %s, which holds the team's data disk", name, vmDir)
		return struct{}{}, nil
	}
	if err := s.Files.RemoveVMDir(vmDir, ""); err != nil {
		return nil, answer(http.StatusInternalServerError, "%s is removed, but not its directory %s: %v", name, vmDir, err)
	}
	return struct{}{}, nil
}

// removeOrphanDir removes the broker's VM directory of the name's key
// when no VM of name exists, if the directory is there, canonical, the
// broker's own and made for the caller (its DACL carries the caller's
// entry; RemoveVMDir checks all of that on the handle it deletes
// through). Nothing there is 404, as for the VM.
func (s *Server) removeOrphanDir(name string, c Caller) (any, error) {
	dir := s.Files.VMDirPath(KeyOf(name))
	p, err := s.Files.Canonical(dir)
	if err != nil {
		s.logf("%s: no VM directory to remove either: %v", name, err)
		return nil, answer(http.StatusNotFound, "there is no VM named %s", name)
	}
	if !strings.EqualFold(p, dir) {
		return nil, answer(http.StatusForbidden, "refused: %s resolves to %s", dir, p)
	}
	if s.holdsData(p) {
		s.logf("%s: keeping its directory %s, which holds the team's data disk", name, p)
		return struct{}{}, nil
	}
	if err := s.Files.RemoveVMDir(p, c.SID()); err != nil {
		return nil, answer(http.StatusForbidden, "refused: remove %s: %v", p, err)
	}
	return struct{}{}, nil
}

// holdsData reports whether the VM directory dir holds the data disk or
// its snapshot, or might: a file that cannot be checked counts as there.
func (s *Server) holdsData(dir string) bool {
	for _, n := range []string{DataDiskName, SnapshotName} {
		if _, err := s.Files.Stat(winJoin(dir, n)); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// vmDirOf is the VM directory DELETE removes: the directory of the VM's
// verified differencing child, else the VM's configuration location as
// Hyper-V reports it (a VM whose configuration failed before its child
// was made has none), made canonical and kept only if it is the vm
// folder of a config directory of the VM's key. "" leaves the
// directory; RemoveVMDir checks the path again on the handle it deletes
// through.
func (s *Server) vmDirOf(name string, cur vmInfo, child string) string {
	if child != "" {
		return winDir(child)
	}
	if cur.Path == "" {
		return ""
	}
	p, err := s.Files.Canonical(cur.Path)
	switch {
	case err != nil:
		s.logf("%s: leaving its directory %s: %v", name, cur.Path, err)
		return ""
	case s.isVMDirOf(p, name):
		return p
	case strings.EqualFold(winBase(p), name) && s.isVMDirOf(winDir(p), name):
		// Hyper-V keeps a VM made with -Path <dir> under <dir>\<name>.
		return winDir(p)
	}
	s.logf("%s: leaving its directory %s: not the broker's VM directory of this VM", name, p)
	return ""
}

// teamDisk is what every data disk request resolves before it touches
// anything: the team's config directory, checked as one the caller can
// write, its key, its VM's name, and the VM's own path to its VM
// directory.
type teamDisk struct {
	cfg, key, name, vmDir string
}

func (t teamDisk) data() string     { return winJoin(t.vmDir, DataDiskName) }
func (t teamDisk) snapshot() string { return winJoin(t.vmDir, SnapshotName) }

// diskRequest decodes a disk request into req (a DiskRequest or a
// VHDRequest, whose config directory configDir reads) and resolves its
// team: the config directory as the caller can write it, from which the
// key and the VM's name are derived as POST /v1/vm derives them.
func (s *Server) diskRequest(r *http.Request, c Caller, req interface{ Validate() error }, configDir func() string) (teamDisk, error) {
	if err := decode(r, req); err != nil {
		return teamDisk{}, err
	}
	if err := req.Validate(); err != nil {
		return teamDisk{}, answer(http.StatusBadRequest, "%v", err)
	}
	cfg, err := c.WritableDir(configDir())
	if err != nil {
		return teamDisk{}, refused("config_dir", err)
	}
	key := host.DirKey(cfg)
	return teamDisk{cfg: cfg, key: key, name: VMName(key), vmDir: s.Files.VMDirPath(key)}, nil
}

// offOrAbsent refuses unless the team's VM is the caller's and, unless
// mayRun, Off; or does not exist: a disk a VM has open is neither
// copied, replaced, compacted nor deleted, and a VM cannot start on a
// disk being any of those while its lock is held, so a start waits
// rather than fails. mayRun is for a file the VM never opens (the
// upgrade snapshot).
func (s *Server) offOrAbsent(t teamDisk, c Caller, mayRun bool) (exists bool, err error) {
	cur, err := s.inspect(t.name, true)
	if err != nil {
		return false, err
	}
	if !cur.Exists {
		return false, nil
	}
	if err := ownedBy(cur, t.name, c.SID()); err != nil {
		return true, err
	}
	if cur.State != StateOff && !mayRun {
		return true, answer(http.StatusConflict, "%s is %s; its data disk is changed only while it is off", t.name, cur.State)
	}
	return true, nil
}

// lockedDisk is the start of every data disk request that changes a
// disk: the request resolved (diskRequest), the VM's lock taken, the VM
// checked (offOrAbsent), and the VM directory made or opened for the
// caller (Files.VMDir, which refuses one made for another account). The
// returned function releases the directory and then the lock.
func (s *Server) lockedDisk(r *http.Request, c Caller, req interface{ Validate() error }, configDir func() string, mayRun bool) (teamDisk, func(), error) {
	t, err := s.diskRequest(r, c, req, configDir)
	if err != nil {
		return t, nil, err
	}
	unlock := s.lock(t.name)
	if _, err := s.offOrAbsent(t, c, mayRun); err != nil {
		unlock()
		return t, nil, err
	}
	vmDir, release, err := s.Files.VMDir(t.key, c.SID())
	if err != nil {
		unlock()
		return t, nil, refused("the VM directory", err)
	}
	t.vmDir = vmDir
	return t, func() { release(); unlock() }, nil
}

// fileExists reports whether a plain file is at p; an error for
// anything but its absence.
func (s *Server) fileExists(p string) (bool, error) {
	_, err := s.Files.Stat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, answer(http.StatusInternalServerError, "%s: %v", p, err)
}

// newVHD is POST /v1/vhd: a new dynamic data disk in the team's VM
// directory, made by New-VHD as the broker. The directory is the
// broker's own and the caller cannot write in it, so nothing at the
// path New-VHD is given (which it would follow) is a link the caller
// planted, and the disk is never a file the caller can write: the
// caller reads it (the directory's DACL passes that on), and only the
// broker changes it.
func (s *Server) newVHD(r *http.Request, c Caller) (any, error) {
	var req VHDRequest
	t, done, err := s.lockedDisk(r, c, &req, func() string { return req.ConfigDir }, false)
	if err != nil {
		return nil, err
	}
	defer done()
	there, err := s.fileExists(t.data())
	if err != nil {
		return nil, err
	}
	if there {
		if !req.Replace {
			return nil, answer(http.StatusConflict, "%s already has a data disk", t.name)
		}
		if err := s.Files.DeleteFile(t.data()); err != nil {
			return nil, answer(http.StatusInternalServerError, "delete the data disk %s: %v", t.data(), err)
		}
	}
	err = s.run(scriptNewVHD, struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
	}{t.data(), req.SizeBytes}, nil)
	if err != nil {
		return nil, err
	}
	return VHDResult{Path: t.data()}, nil
}

// optimizeVHD is POST /v1/vhd/optimize: the team's data disk, the
// broker's own file, compacted while the VM is off (offOrAbsent). The
// broker compacts it through the virtual disk API itself
// (Files.CompactVHDX): opened without parents, refused on that handle
// unless it is a dynamic VHDX, and compacted through it, so no parent is
// opened whatever the file says. Its progress is POST
// /v1/vhd/progress's while it runs.
func (s *Server) optimizeVHD(r *http.Request, c Caller) (any, error) {
	var req DiskRequest
	t, done, err := s.lockedDisk(r, c, &req, func() string { return req.ConfigDir }, false)
	if err != nil {
		return nil, err
	}
	defer done()
	there, err := s.fileExists(t.data())
	if err != nil {
		return nil, err
	}
	if !there {
		return nil, answer(http.StatusNotFound, "%s has no data disk", t.name)
	}
	m, untrack := s.track(t.key, OpCompact, c.SID())
	defer untrack()
	ctx, cancel := context.WithTimeout(r.Context(), OptimizeTimeout)
	defer cancel()
	before, after, err := s.Files.CompactVHDX(ctx, t.data(), m)
	if errors.Is(err, errNotDynamic) {
		return nil, refused("the data disk", err)
	}
	if err != nil {
		return nil, answer(http.StatusInternalServerError, "compact the data disk: %v", err)
	}
	return OptimizeResult{SizeBefore: before, SizeAfter: after}, nil
}

// TODO: POST /v1/vhd/import goes with the move of the data disk
// (migrateLocked in internal/supervisor, whose TODO lists what removing
// it takes: a check that refuses up while a disk is at the legacy path,
// and everything else that depends on the move). Its only caller is
// that move: a new team's disk is made by POST /v1/vhd, and restoring a
// backup goes through the app, never a disk file. It is the one
// endpoint that reads a disk the caller wrote, and so a way in for as
// long as it exists.
//
// importVHD is POST /v1/vhd/import: data.vhdx in the config directory,
// where the data disk was kept before the broker kept it, copied into
// the team's VM directory as its data disk, with the VM off.
//
// The source is the caller's file, which the caller can rewrite at
// will, so the broker reads it only through a handle opened as the
// caller (Caller.File: what the caller could not read is not read, and
// the file is pinned while it is copied), checks its metadata through
// that handle (a dynamic VHDX with no parent, and nothing else, is
// copied), and never opens it as a virtual disk: Hyper-V or virtdisk
// opening a differencing disk opens the parent its locator names, a
// share on another host as easily as a local file. The copy, import.vhdx,
// is the broker's own file, which the caller cannot write; it is checked
// again (Files.CheckDynamicVHDX: the VHDX's log replayed and its subtype
// read without parents) and only then renamed to data.vhdx, never over
// one: a data disk already there is an earlier import that finished, and
// the answer is 409 AlreadyImported. A copy left by an import that was
// cut short is deleted first, and one that fails or is cancelled is
// deleted. Its progress is POST /v1/vhd/progress's while it runs.
func (s *Server) importVHD(r *http.Request, c Caller) (any, error) {
	var req DiskRequest
	t, done, err := s.lockedDisk(r, c, &req, func() string { return req.ConfigDir }, false)
	if err != nil {
		return nil, err
	}
	defer done()
	there, err := s.fileExists(t.data())
	if err != nil {
		return nil, err
	}
	if there {
		return nil, answer(http.StatusConflict, "%s: %s holds a data disk already", AlreadyImported, t.vmDir)
	}
	src, err := c.File(winJoin(t.cfg, DataDiskName))
	if err != nil {
		return nil, refused("the data disk to import", err)
	}
	in, size, err := c.Contents(src)
	if err != nil {
		return nil, refused("the data disk to import", err)
	}
	if err := dynamicNoParent(in, size); err != nil {
		return nil, refused("the data disk to import", fmt.Errorf("%s: %w", src, err))
	}
	tmp := winJoin(t.vmDir, importName)
	if err := s.Files.DeleteFile(tmp); err != nil {
		return nil, answer(http.StatusInternalServerError, "clear %s: %v", tmp, err)
	}
	m, untrack := s.track(t.key, OpImport, c.SID())
	defer untrack()
	ctx, cancel := context.WithTimeout(r.Context(), ImportTimeout)
	defer cancel()
	if err := s.Files.CopyFile(ctx, in, size, tmp, m); err != nil {
		_ = s.Files.DeleteFile(tmp)
		return nil, answer(http.StatusInternalServerError, "copy %s: %v", src, err)
	}
	if err := s.Files.CheckDynamicVHDX(tmp); err != nil {
		_ = s.Files.DeleteFile(tmp)
		return nil, refused("the copied data disk", err)
	}
	if err := s.Files.Rename(tmp, t.data(), false); err != nil {
		_ = s.Files.DeleteFile(tmp)
		if errors.Is(err, ErrExists) {
			return nil, answer(http.StatusConflict, "%s: %s holds a data disk already", AlreadyImported, t.vmDir)
		}
		return nil, answer(http.StatusInternalServerError, "put the copy in place: %v", err)
	}
	return ImportResult{Path: t.data(), CopiedBytes: size}, nil
}

// snapshotVHD is POST /v1/vhd/snapshot: the data disk copied to its
// upgrade snapshot beside it (replacing an earlier one), flushed before
// the answer, with the VM off.
func (s *Server) snapshotVHD(r *http.Request, c Caller) (any, error) {
	var req DiskRequest
	t, done, err := s.lockedDisk(r, c, &req, func() string { return req.ConfigDir }, false)
	if err != nil {
		return nil, err
	}
	defer done()
	in, size, release, err := s.Files.Open(t.data())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, answer(http.StatusNotFound, "%s has no data disk", t.name)
	}
	if err != nil {
		return nil, answer(http.StatusInternalServerError, "open the data disk: %v", err)
	}
	defer release()
	if err := s.Files.DeleteFile(t.snapshot()); err != nil {
		return nil, answer(http.StatusInternalServerError, "clear %s: %v", t.snapshot(), err)
	}
	m, untrack := s.track(t.key, OpSnapshot, c.SID())
	defer untrack()
	ctx, cancel := context.WithTimeout(r.Context(), SnapshotTimeout)
	defer cancel()
	if err := s.Files.CopyFile(ctx, in, size, t.snapshot(), m); err != nil {
		_ = s.Files.DeleteFile(t.snapshot())
		return nil, answer(http.StatusInternalServerError, "snapshot the data disk: %v", err)
	}
	return SnapshotResult{CopiedBytes: size}, nil
}

// restoreVHD is POST /v1/vhd/restore: the upgrade snapshot renamed over
// the data disk, durably, with the VM off. Without a snapshot the
// answer is 404 and nothing changes.
func (s *Server) restoreVHD(r *http.Request, c Caller) (any, error) {
	var req DiskRequest
	t, done, err := s.lockedDisk(r, c, &req, func() string { return req.ConfigDir }, false)
	if err != nil {
		return nil, err
	}
	defer done()
	there, err := s.fileExists(t.snapshot())
	if err != nil {
		return nil, err
	}
	if !there {
		return nil, answer(http.StatusNotFound, "%s has no snapshot of its data disk", t.name)
	}
	if err := s.Files.Rename(t.snapshot(), t.data(), true); err != nil {
		return nil, answer(http.StatusInternalServerError, "restore the snapshot: %v", err)
	}
	return struct{}{}, nil
}

// deleteFiles deletes the files named in the VM directory and gives the
// bytes they held.
func (s *Server) deleteFiles(t teamDisk, names ...string) (int64, error) {
	var freed int64
	for _, n := range names {
		p := winJoin(t.vmDir, n)
		fi, err := s.Files.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return freed, answer(http.StatusInternalServerError, "%s: %v", p, err)
		}
		if err := s.Files.DeleteFile(p); err != nil {
			return freed, answer(http.StatusInternalServerError, "delete %s: %v", p, err)
		}
		freed += fi.Used
	}
	return freed, nil
}

// deleteSnapshot is DELETE /v1/vhd/snapshot: the upgrade snapshot
// deleted. The VM never opens the snapshot, so it may be running (an
// upgrade deletes the snapshot once the new release is up). No snapshot
// is no error.
func (s *Server) deleteSnapshot(r *http.Request, c Caller) (any, error) {
	var req DiskRequest
	t, done, err := s.lockedDisk(r, c, &req, func() string { return req.ConfigDir }, true)
	if err != nil {
		return nil, err
	}
	defer done()
	freed, err := s.deleteFiles(t, SnapshotName)
	if err != nil {
		return nil, err
	}
	return DeleteResult{FreedBytes: freed}, nil
}

// deleteVHD is DELETE /v1/vhd: the team's data disk, its snapshot and
// any copy an import left, deleted with the VM off, and then the VM
// directory too when there is no VM (a destroy removes the VM first). A
// VM directory that is not there is nothing to delete; one made for
// another account is refused (Files.VMDir).
func (s *Server) deleteVHD(r *http.Request, c Caller) (any, error) {
	var req DiskRequest
	t, err := s.diskRequest(r, c, &req, func() string { return req.ConfigDir })
	if err != nil {
		return nil, err
	}
	unlock := s.lock(t.name)
	defer unlock()
	exists, err := s.offOrAbsent(t, c, false)
	if err != nil {
		return nil, err
	}
	if _, err := s.Files.Canonical(t.vmDir); errors.Is(err, fs.ErrNotExist) {
		return DeleteResult{}, nil
	}
	vmDir, release, err := s.Files.VMDir(t.key, c.SID())
	if err != nil {
		return nil, refused("the VM directory", err)
	}
	t.vmDir = vmDir
	freed, err := s.deleteFiles(t, DataDiskName, SnapshotName, importName)
	release()
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := s.Files.RemoveVMDir(vmDir, c.SID()); err != nil {
			return nil, answer(http.StatusInternalServerError, "the data disk is deleted, but not its directory %s: %v", vmDir, err)
		}
	}
	return DeleteResult{FreedBytes: freed}, nil
}

// vhdProgress is POST /v1/vhd/progress: how far the long operation on
// the team's data disk has got (an import's or a snapshot's bytes
// copied, a compaction's progress as the virtual disk API counts it),
// or no operation. It takes no lock: the operation holds the VM's. The
// operation must be one the caller's own request runs.
func (s *Server) vhdProgress(r *http.Request, c Caller) (any, error) {
	var req DiskRequest
	t, err := s.diskRequest(r, c, &req, func() string { return req.ConfigDir })
	if err != nil {
		return nil, err
	}
	s.runningMu.Lock()
	op := s.running[t.key]
	s.runningMu.Unlock()
	if op == nil {
		return VHDProgress{}, nil
	}
	if !strings.EqualFold(op.sid, c.SID()) {
		return nil, answer(http.StatusForbidden, "refused: the operation on %s is another account's", t.name)
	}
	done, total := op.meter.Get()
	return VHDProgress{Op: op.op, Done: done, Total: total}, nil
}

// dynamicNoParent reads a VHDX's region table, metadata and block table
// (never a parent locator) and refuses it unless it is a dynamic disk: a
// differencing disk, a fixed one or a file that is no VHDX is not the
// data disk the backend made.
func dynamicNoParent(f io.ReaderAt, size int64) error {
	l, err := vhdx.Read(f, size)
	if err != nil {
		return fmt.Errorf("%w: %w", errNotDynamic, err)
	}
	if l.Fixed {
		return fmt.Errorf("%w: a fixed disk", errNotDynamic)
	}
	return nil
}

func (s *Server) setupStatus(*http.Request, Caller) (any, error) {
	var st Setup
	err := s.run(scriptSetupStatus, struct{}{}, &st)
	return st, err
}

// setupApply registers what is missing that the broker can register
// (the vsock service); Hyper-V itself needs an administrator and a
// reboot, and its Default Switch comes with it.
func (s *Server) setupApply(r *http.Request, c Caller) (any, error) {
	defer s.lock("setup")()
	if err := s.run(scriptSetupApply, struct{}{}, nil); err != nil {
		return nil, err
	}
	return s.setupStatus(r, c)
}
