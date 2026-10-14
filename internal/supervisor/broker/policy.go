package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// component is a reparse point, and keeps a handle on it open, reading
// and without delete sharing, until Close: what was checked cannot be
// renamed, removed or replaced by a link while the request runs (a
// handle with no data access would pin nothing, and one that writes
// would keep Hyper-V from attaching the disk; AccessName pins nothing,
// for a disk a running VM holds itself).
type Caller interface {
	// SID is the caller's user SID, from the impersonation token.
	SID() string
	// WritableDir opens dir as a directory the caller can add files to.
	WritableDir(dir string) (string, error)
	// File opens an existing regular file with one link, for access.
	File(p string, access Access) (string, error)
	// Contents reads the file File pinned for AccessRead or AccessWrite
	// at p (the canonical path File returned) through the pinning
	// handle, so what is read is the file that was checked, and gives
	// its size.
	Contents(p string) (io.ReaderAt, int64, error)
	// Absent succeeds only if nothing, not even a dangling link, is at p
	// as the caller sees it; ErrExists if something is.
	Absent(p string) error
	// Rename moves src to dst as the caller, replacing nothing (ErrExists
	// if anything is at dst): how a new data disk, granted to the caller
	// in the broker's VM directory, reaches the caller's own directory,
	// where the broker, which is no administrator, cannot write.
	Rename(src, dst string) error
	// CompactVHDX compacts the dynamic VHDX that File pinned for
	// AccessWrite at p (its canonical path) and gives the file's size
	// before and after: the virtual disk is opened as the caller and
	// without opening any parent its locator names, refused through
	// that handle unless it is a dynamic VHDX (an error wrapping
	// errNotDynamic), and compacted through it, until ctx ends.
	CompactVHDX(ctx context.Context, p string) (before, after int64, err error)
	// Close releases what the checks hold open.
	Close() error
}

// FileID names a file whatever its path: the volume and the file's
// index on it. It is what GrantFile returns and PlacedFile checks.
type FileID struct {
	Volume, High, Low uint32
}

// Access is what File opens a file for.
type Access int

// File access.
const (
	// AccessRead: the caller can read the file's data.
	AccessRead Access = iota + 1
	// AccessWrite: the caller can read and write the file's data.
	AccessWrite
	// AccessName: only the canonical name, no data access, for a disk a
	// running VM holds open.
	AccessName
)

// ErrExists is Absent's answer when something is at the path.
var ErrExists = errors.New("something already exists there")

// Files is what the broker does on the file system as itself.
type Files interface {
	// VMDir makes <configDir>\vm if it is absent, owned by the broker,
	// with a protected DACL (SYSTEM and Administrators full control, the
	// owner read), opens it, checks it is a directory the broker made
	// (brokerOwned) and no link, and keeps it open without delete sharing
	// until release. The caller cannot write in it, so nothing there is a
	// link the caller planted: the broker creates files there as itself.
	VMDir(key, callerSID string) (dir string, release func(), err error)
	// VMDirPath is the VM directory of a config directory key, whether
	// or not it exists: <the broker's root>\<key>.
	VMDirPath(key string) string
	// Canonical is p's final path as the broker sees it, refusing a
	// reparse point along it.
	Canonical(p string) (string, error)
	// ParentPath is the parent the locator of the differencing VHDX at
	// p, a file in a VM directory, names: its bytes parsed through a
	// handle opened without following a link, the parent never opened.
	ParentPath(p string) (string, error)
	// GrantFile grants callerSID full control of p, a file the broker
	// made in a VM directory, through a handle opened on it without
	// following a link, and returns its identity. The caller then moves
	// it (Caller.Rename) and PlacedFile checks where it landed.
	GrantFile(p, callerSID string) (FileID, error)
	// PlacedFile checks that dst, opened without following a link, is a
	// plain file with the identity GrantFile returned and the canonical
	// path dst (the caller could have renamed it away and put something
	// else there), and applies the disk's DACL again through that
	// handle, so the grant can never land on a link's target.
	PlacedFile(dst string, id FileID, callerSID string) error
	// RemoveVMDir deletes the VM directory dir (a canonical
	// <config>\vm) and everything in it, after checking, on a handle
	// opened without following a link, that it is a directory the broker
	// made, no reparse point, and canonically dir, and, with a callerSID,
	// that its DACL carries that caller's entry (VMDir wrote it: the
	// directory is that caller's team's). Entries are opened the same way
	// and deleted through their handles, so a link inside is deleted as
	// a link and never followed.
	RemoveVMDir(dir, callerSID string) error
	// DeleteFile deletes p, a file in a VM directory, through a handle
	// opened without following a link (a link is deleted as the link);
	// nothing at p is no error.
	DeleteFile(p string) error
	// RootDisk checks the broker's root disk at p (Server.Root) and
	// returns its canonical path: p made canonical (Canonical), opened as
	// a VHDX for its metadata only and without opening any parent its
	// locator names, and refused unless its provider subtype is fixed or
	// dynamic. A differencing root would have Hyper-V open whatever parent
	// it names; the install directory's DACL is what keeps one out, and
	// this check is a second line.
	RootDisk(p string) (string, error)
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
}

// OptimizeTimeout bounds a compaction. It moves the blocks after a
// hole down into it, so its cost is the data moved: measured at about a
// second for 2.5 GB on an SSD, which puts the whole of a 64 GiB disk at
// a minute or two there and at some minutes on a slow disk. The bound is
// generous because the compaction is cancelled past it, and a
// compaction cut short is left to the VHDX's own log to tidy.
const OptimizeTimeout = 20 * time.Minute

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
	mux.HandleFunc("POST /v1/vhd/optimize", s.handle(s.optimizeVHD))
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
		s.logf("%s %s by %s: ok", r.Method, r.URL.Path, c.SID())
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
	dataAccess := AccessWrite
	if running {
		// A running VM holds its disks; only their names are compared.
		dataAccess = AccessName
	}
	data, err := c.File(req.DataVHDX, dataAccess)
	if err != nil {
		return nil, refused("data_vhdx", err)
	}
	// Directly inside, so every later request can re-derive the config
	// directory, and so the VM, from the disk alone.
	if !strings.EqualFold(winDir(data), cfg) {
		return nil, answer(http.StatusForbidden, "refused: data_vhdx %s is not in %s", data, cfg)
	}
	if strings.EqualFold(root, data) {
		return nil, answer(http.StatusForbidden, "refused: data_vhdx %s is the broker's root disk", data)
	}
	vmDir, release, err := s.Files.VMDir(key, c.SID())
	if err != nil {
		return nil, refused("the VM directory", err)
	}
	defer release()

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
// the ones its configuration checked: the data disk a file the caller
// can write, directly inside the config directory of the VM's key, and
// the root disk the broker's differencing child in that directory's vm
// folder. Between requests nothing is pinned, so a start checks again.
func (s *Server) checkDisks(c Caller, name string, cur vmInfo) error {
	key := KeyOf(name)
	root, okRoot := cur.disk(0)
	data, okData := cur.disk(1)
	if !okRoot || !okData {
		return errors.New("its disks are not the two the broker attaches")
	}
	dataPath, err := c.File(data.Path, AccessWrite)
	if err != nil {
		return fmt.Errorf("the data disk: %w", err)
	}
	cfg := winDir(dataPath)
	if host.DirKey(cfg) != key {
		return fmt.Errorf("the data disk %s is not in the config directory of %s", dataPath, name)
	}
	child, err := s.Files.Canonical(root.Path)
	if err != nil {
		return fmt.Errorf("the root disk: %w", err)
	}
	if want := winJoin(s.Files.VMDirPath(key), ChildDiskName); !strings.EqualFold(child, want) {
		return fmt.Errorf("the root disk is %s, not %s", child, want)
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
	if err := s.checkDisks(c, name, cur); err != nil {
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
// directory of the VM name: a folder named vm directly inside a config
// directory whose key is the name's.
func (s *Server) isVMDirOf(p, name string) bool {
	return strings.EqualFold(p, s.Files.VMDirPath(KeyOf(name)))
}

// removeVM is DELETE /v1/vm/{name}: the VM, its differencing child and
// the broker's VM directory, never the data disk. The child is deleted
// only if it is, canonically, the broker's child in the VM directory of
// the VM's own key; otherwise it is left where it is and the VM is
// removed all the same. The VM directory goes last, after Remove-VM.
// Without a VM, the VM directory of the name's key is removed on its
// own, if it was made for the caller (removeOrphanDir: a team whose
// first up made the data disk, and so the directory, but never the VM).
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
	if err := s.Files.RemoveVMDir(p, c.SID()); err != nil {
		return nil, answer(http.StatusForbidden, "refused: remove %s: %v", p, err)
	}
	return struct{}{}, nil
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

// newDiskName is the name POST /v1/vhd creates the data disk under in
// the broker's VM directory, before moving it into place. A file of
// that name left by an earlier attempt that failed is replaced.
const newDiskName = "new-data.vhdx"

// newVHD is POST /v1/vhd: a new dynamic data.vhdx in a directory the
// caller can write, where nothing is yet; the caller gets full control
// of it.
//
// New-VHD runs with the broker's rights (Hyper-V's own service creates
// the file) and follows a link at the path it is given, so it never
// creates in the caller's directory: between the check that nothing is
// at <dir>\data.vhdx and the creation, a caller who can make symbolic
// links could put one there and have the broker create a file wherever
// it pointed. The disk is made in the broker's own VM directory
// (<dir>\vm, which the caller cannot write), granted to the caller
// there (Files.GrantFile), moved into place by the caller, as the
// caller (Caller.Rename: a rename never follows a link and fails if
// anything is at the destination, and the broker, no administrator,
// cannot write in the caller's directory anyway), and then checked to be
// the file the broker made, where it was asked for (Files.PlacedFile).
func (s *Server) newVHD(r *http.Request, c Caller) (any, error) {
	var req VHDRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, answer(http.StatusBadRequest, "%v", err)
	}
	defer s.lock("vhd:" + strings.ToLower(req.Path))()
	dir, err := c.WritableDir(winDir(req.Path))
	if err != nil {
		return nil, refused("the disk's directory", err)
	}
	p := winJoin(dir, DataDiskName)
	if err := c.Absent(p); err != nil {
		return nil, refused(p, err)
	}
	vmDir, release, err := s.Files.VMDir(host.DirKey(dir), c.SID())
	if err != nil {
		return nil, refused("the VM directory", err)
	}
	defer release()
	made := winJoin(vmDir, newDiskName)
	// Whatever an earlier attempt left under that name goes first,
	// through a handle: the caller could delete it, had its move failed,
	// but could not have turned it into a link, having no write on it;
	// still, New-VHD, which follows a link at the path it is given, never
	// sees one here.
	if err := s.Files.DeleteFile(made); err != nil {
		return nil, answer(http.StatusInternalServerError, "clear %s: %v", made, err)
	}
	err = s.run(scriptNewVHD, struct {
		Path      string `json:"path"`
		SizeBytes int64  `json:"size_bytes"`
	}{made, req.SizeBytes}, nil)
	if err != nil {
		return nil, err
	}
	id, err := s.Files.GrantFile(made, c.SID())
	if err != nil {
		return nil, answer(http.StatusInternalServerError, "grant the new disk %s: %v", made, err)
	}
	if err := c.Rename(made, p); err != nil {
		// The disk that did not move is deleted (through a handle, as
		// the link it could have become): nothing the caller can delete
		// but not write stays in the broker's directory.
		_ = s.Files.DeleteFile(made)
		if errors.Is(err, ErrExists) {
			return nil, answer(http.StatusConflict, "%s: %v", p, err)
		}
		return nil, refused("move the new disk into place", err)
	}
	if err := s.Files.PlacedFile(p, id, c.SID()); err != nil {
		return nil, answer(http.StatusInternalServerError, "the new disk at %s: %v", p, err)
	}
	return VHDResult{Path: p}, nil
}

// optimizeVHD is POST /v1/vhd/optimize: the caller's own data disk,
// compacted while the VM of its directory is off. The disk must be
// data.vhdx directly inside a directory the caller can write, a regular
// file the caller can write, pinned by the caller's handle for the
// request (Caller.File), so the compaction runs on the file that was
// checked. The VM of that directory, if it exists, must be the caller's
// and Off: a disk a running VM holds cannot be compacted, and a VM
// cannot start on a disk being compacted, so the VM's lock is held
// meanwhile and a start of it waits rather than fails. The caller writes
// the file's bytes, and opening a differencing disk would open the
// parent its locator names (a share on another host as easily as a
// local file); so the broker reads the file's metadata through the
// pinning handle and refuses anything but a dynamic VHDX with no parent,
// and then compacts it through the virtual disk API itself
// (Caller.CompactVHDX): opened as the caller and without parents, its
// kind checked again on that very handle, and compacted through it, so
// no parent is opened whatever the file says by then.
func (s *Server) optimizeVHD(r *http.Request, c Caller) (any, error) {
	var req OptimizeRequest
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, answer(http.StatusBadRequest, "%v", err)
	}
	dir, err := c.WritableDir(winDir(req.Path))
	if err != nil {
		return nil, refused("the disk's directory", err)
	}
	name := VMName(host.DirKey(dir))
	defer s.lock(name)()
	p, err := c.File(winJoin(dir, DataDiskName), AccessWrite)
	if err != nil {
		return nil, refused("the data disk", err)
	}
	cur, err := s.inspect(name, true)
	if err != nil {
		return nil, err
	}
	if cur.Exists {
		if err := ownedBy(cur, name, c.SID()); err != nil {
			return nil, err
		}
		if cur.State != StateOff {
			return nil, answer(http.StatusConflict, "%s is %s; its data disk is compacted only while it is off", name, cur.State)
		}
	}
	if err := dynamicNoParent(c, p); err != nil {
		return nil, refused("the data disk", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), OptimizeTimeout)
	defer cancel()
	before, after, err := c.CompactVHDX(ctx, p)
	if errors.Is(err, errNotDynamic) {
		return nil, refused("the data disk", err)
	}
	if err != nil {
		return nil, answer(http.StatusInternalServerError, "compact the data disk: %v", err)
	}
	return OptimizeResult{SizeBefore: before, SizeAfter: after}, nil
}

// dynamicNoParent reads the VHDX the caller pinned at p (its region
// table, metadata and block table; never a parent locator) and refuses
// it unless it is a dynamic disk: a differencing disk, a fixed one or a
// file that is no VHDX is not the data disk the backend made.
func dynamicNoParent(c Caller, p string) error {
	f, size, err := c.Contents(p)
	if err != nil {
		return err
	}
	l, err := vhdx.Read(f, size)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if l.Fixed {
		return fmt.Errorf("%s: a fixed disk, not a dynamic one", p)
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
