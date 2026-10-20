//go:build windows

// Package hyperv is the Windows Backend: it runs the Kivali Desktop VM
// as a Hyper-V virtual machine, one per team, managed through the
// privileged broker (internal/supervisor/broker) over its named pipe.
// The supervisor keeps its ordinary rights; only the broker touches
// Hyper-V. docs/developers/supervisor.md, "The Windows backend", is the
// design; it mirrors the macOS backend (internal/supervisor/vz).
//
// The VM boots a differencing child of the broker's root.vhdx (the one
// installed beside it, never one this process names) and a dynamic
// data.vhdx in the team's VM directory under the broker's own
// (broker.VMRoot), which this account can read but only the broker can
// write, on Hyper-V's Default Switch, with COM1 on a named
// pipe (the marker lines out, the poweroff verb and the first-boot
// format answer in) and the guest agent reached over a Hyper-V socket
// to vsock port 1024. The broker derives the VM's name, MAC and console
// pipe from the config directory's key, the same key the RPC pipe uses.
package hyperv

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"golang.org/x/sys/windows"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/broker"
	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// The VM image's files on Windows.
const (
	// RootFile is the bootable root disk, a dynamic VHDX with an EFI
	// partition and the squashfs, which marks a directory as the image
	// (Image). The VM boots a differencing child of the broker's root
	// disk (broker.RootPath), which in an installed layout is this same
	// file: <install dir>\vm\root.vhdx.
	RootFile = broker.RootFile
	// VersionFile holds the image's version ("dev" without one).
	VersionFile = "VERSION"
	// DataDiskFile is the data disk's name: in the team's VM directory
	// (DataDiskPath), and in the config directory, where earlier
	// versions kept it (LegacyDataDiskPath).
	DataDiskFile = broker.DataDiskName
)

// The guest's console verbs.
const (
	poweroffVerb = "KIVALI-POWEROFF\n"
	// askFormatLine is what the guest prints to ask whether to format
	// the data disk on its first boot (docs/developers/supervisor.md, "The
	// image"); the host answers over the console.
	askFormatLine   = "KIVALI-VM ASK format-data"
	formatAnswerFmt = "KIVALI-FORMAT-DATA %d\n"
)

// consoleConnectTimeout bounds how long Start waits, after the broker
// has started the VM, for Hyper-V to connect to the console pipe the
// supervisor serves (it connects as the VM starts: seconds at most).
const consoleConnectTimeout = 30 * time.Second

// The state poll is a backstop, not the signal: a VM that stops ends
// its console pipe, which closes Done at once. The poll catches the
// rare stop that leaves the pipe open (a turn-off, a crash), and each
// one is a PowerShell process in the broker (seconds of CPU, for as
// long as the VM runs), so it runs every 15 seconds; a VM that stops
// that way is noticed within that. A query may take as long as the
// broker allows its script, so its own bound is longer than the period
// (the polls never overlap: the next waits for the last).
const (
	statePoll         = 15 * time.Second
	stateQueryTimeout = time.Minute
)

// Backend creates Hyper-V machines through a broker client.
type Backend struct {
	Client *broker.Client
	// Clock times the progress queries and lines of a long disk
	// operation (a compaction, an import); nil is the system clock.
	Clock clock.Clock
	// VMRoot is the broker's root of VM directories, where the data disk
	// is read (DataDiskPath); "" is broker.VMRoot, the broker's own.
	VMRoot string

	// guestShutdown, for tests, replaces the guest agent's shutdown in
	// StopLeftover (there is no VM to dial).
	guestShutdown func(ctx context.Context) error
}

var (
	_ supervisor.Backend           = Backend{}
	_ supervisor.MachineRemover    = Backend{}
	_ supervisor.LeftoverStopper   = Backend{}
	_ supervisor.DataDiskCompactor = Backend{}
	_ supervisor.DataDiskMigrator  = Backend{}
	_ supervisor.DataDiskKeeper    = Backend{}
)

// compactThreshold is the discarded space below which the data disk is
// left as it is. A dynamic VHDX keeps the space of a block the guest
// discarded (package vhdx) until it is compacted, which only the broker can
// do, with the VM off; a compaction is a broker request (a PowerShell
// state query, then the compaction itself) and a privileged operation
// on every start or stop otherwise, and under 1 GiB the file is within
// a few dozen 32 MiB blocks of its smallest, which is within what partly
// filled blocks cost anyway (a fresh disk's file is 1.7 GB larger than
// what the guest's df reports used). Measured: Quick mode gave 2.5 GB
// back in about a second on an SSD.
const compactThreshold = 1 << 30

// compactTimeout bounds one compaction as the client, and importTimeout
// and snapshotTimeout an import's and a snapshot's copy: longer than the
// broker's own bounds (broker.OptimizeTimeout, broker.ImportTimeout,
// broker.SnapshotTimeout), so what comes back is the broker's answer.
const (
	compactTimeout  = broker.OptimizeTimeout + 5*time.Minute
	importTimeout   = broker.ImportTimeout + 5*time.Minute
	snapshotTimeout = broker.SnapshotTimeout + 5*time.Minute
)

// progressPoll is how often a long disk operation (a compaction, an
// import) asks the broker how far it has got, for the desktop app's
// progress bar; a query reads a counter in the broker and runs no
// script. compactHeartbeat is how often such an operation still running
// says so in a line: inside the desktop app's 30 s idle limit on an
// operation.
const (
	progressPoll     = time.Second
	compactHeartbeat = 10 * time.Second
)

func (b Backend) clock() clock.Clock {
	if b.Clock != nil {
		return b.Clock
	}
	return clock.System{}
}

// longOp runs call, a broker request that takes long (an import, a
// compaction), with a goroutine that meanwhile asks the broker every
// progressPoll how far its operation op on the team's data disk has got
// (POST /v1/vhd/progress), reporting that to progress, and logs the
// waiting line every compactHeartbeat. It returns once call has and the
// goroutine is done, so nothing is reported after.
func (b Backend) longOp(ctx context.Context, configDir, op string, progress supervisor.Progress, logf supervisor.Logf, waiting string, call func(ctx context.Context) error) error {
	clk := b.clock()
	start := clk.Now()
	tick := clk.NewTicker(progressPoll)
	callCtx, cancel := context.WithCancel(ctx)
	quiet := make(chan struct{})
	go func() {
		defer close(quiet)
		beat := start
		for {
			select {
			case <-callCtx.Done():
				return
			case now := <-tick.C():
				qctx, qcancel := context.WithTimeout(callCtx, stateQueryTimeout)
				p, err := b.Client.VHDProgress(qctx, configDir)
				qcancel()
				if err == nil && p.Op == op && p.Total > 0 && progress != nil {
					progress(p.Done, p.Total)
				}
				if now.Sub(beat) >= compactHeartbeat {
					beat = now
					logf("%s (%s so far)", waiting, now.Sub(start).Round(time.Second))
				}
			}
		}
	}()
	err := call(callCtx)
	tick.Stop()
	cancel()
	<-quiet
	return err
}

// CompactDataDisk reads the data disk's block allocation table (a few
// dozen KiB, whatever the disk's size; this account can read the
// broker's file) and, when the blocks the guest discarded still hold
// compactThreshold or more of the file, has the broker compact it (POST
// /v1/vhd/optimize: CompactVirtualDisk with no zero scan, as
// Optimize-VHD's Quick mode, which drops those blocks and moves the rest
// down). It is called with the VM off, before a start and after a clean
// stop, and calls began once a compaction is under way; while one runs
// it reports the broker's progress and logs a line every
// compactHeartbeat. The disk's content is the same before and after;
// its file is as small as what the disk holds.
func (b Backend) CompactDataDisk(ctx context.Context, configDir string, began func(), progress supervisor.Progress, logf supervisor.Logf) error {
	configDir = plainPath(configDir)
	l, err := vhdx.ReadFile(b.DataDiskPath(configDir))
	if err != nil {
		return fmt.Errorf("read the data disk's block table: %w", err)
	}
	if l.DiscardedBytes() < compactThreshold {
		return nil
	}
	began()
	logf("the data disk's file holds %s of blocks the guest discarded; compacting it", gib(l.DiscardedBytes()))
	ctx, cancel := context.WithTimeout(ctx, compactTimeout)
	defer cancel()
	start := b.clock().Now()
	var res broker.OptimizeResult
	err = b.longOp(ctx, configDir, broker.OpCompact, progress, logf, "waiting for the data disk's compaction to finish", func(ctx context.Context) error {
		var err error
		res, err = b.Client.OptimizeVHD(ctx, configDir)
		return err
	})
	if err != nil {
		return fmt.Errorf("compact the data disk: %w", err)
	}
	logf("the data disk's file is compacted: %s to %s in %s", gib(res.SizeBefore), gib(res.SizeAfter), b.clock().Now().Sub(start).Round(time.Second))
	return nil
}

// gib formats bytes as GiB with one decimal.
func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// Image checks dir holds the root disk and reads the image's version
// from VERSION ("dev" without one). The guest is amd64.
func (Backend) Image(dir string) (supervisor.ImageInfo, error) {
	if _, err := os.Stat(filepath.Join(dir, RootFile)); err != nil {
		return supervisor.ImageInfo{}, fmt.Errorf("%s has no %s", dir, RootFile)
	}
	info := supervisor.ImageInfo{Version: "dev", Arch: "amd64"}
	b, err := os.ReadFile(filepath.Join(dir, VersionFile))
	switch {
	case err == nil:
		if v := strings.TrimSpace(string(b)); v != "" {
			info.Version = v
		}
	case !errors.Is(err, fs.ErrNotExist):
		return info, err
	}
	return info, nil
}

// vmRoot is the broker's root of VM directories: VMRoot, else the
// broker's own (ProgramData\Kivali\broker\vm).
func (b Backend) vmRoot() string {
	if b.VMRoot != "" {
		return b.VMRoot
	}
	if root, err := broker.VMRoot(); err == nil {
		return root
	}
	return filepath.Join(os.Getenv("ProgramData"), "Kivali", "broker", broker.VMDirName)
}

// DataDiskPath is data.vhdx in the team's VM directory, the broker's
// <VM root>\<config directory key>, where the broker keeps it: this
// account can read it (its size, its block table) and never write it.
func (b Backend) DataDiskPath(configDir string) string {
	return filepath.Join(b.vmRoot(), host.DirKey(plainPath(configDir)), DataDiskFile)
}

// LegacyDataDiskPath is data.vhdx in the config directory, where earlier
// versions kept the data disk (ImportDataDisk moves it).
func (Backend) LegacyDataDiskPath(configDir string) string {
	return filepath.Join(configDir, DataDiskFile)
}

// CreateDataDisk asks the broker for a new dynamic VHDX of size bytes
// as the team's data disk (POST /v1/vhd), replacing the one there.
func (b Backend) CreateDataDisk(configDir string, size int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := b.Client.NewVHD(ctx, plainPath(configDir), size, true)
	return err
}

// ImportDataDisk has the broker copy the data disk an earlier version
// kept in the config directory into the team's VM directory (POST
// /v1/vhd/import), with the VM off; while the copy runs it reports the
// bytes copied and logs a line every compactHeartbeat. A data disk the
// broker already has is supervisor.ErrAlreadyImported.
func (b Backend) ImportDataDisk(ctx context.Context, configDir string, progress supervisor.Progress, logf supervisor.Logf) error {
	configDir = plainPath(configDir)
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	start := b.clock().Now()
	var res broker.ImportResult
	err := b.longOp(ctx, configDir, broker.OpImport, progress, logf, "copying the data disk", func(ctx context.Context) error {
		var err error
		res, err = b.Client.ImportVHD(ctx, configDir)
		return err
	})
	if broker.IsAlreadyImported(err) {
		return supervisor.ErrAlreadyImported
	}
	if err != nil {
		return err
	}
	logf("the data disk is copied: %s in %s", gib(res.CopiedBytes), b.clock().Now().Sub(start).Round(time.Second))
	return nil
}

// SnapshotDataDisk has the broker copy the data disk to its upgrade
// snapshot beside it (POST /v1/vhd/snapshot), flushed before it answers;
// while the copy runs it reports the bytes copied and logs a line every
// compactHeartbeat, as an import does.
func (b Backend) SnapshotDataDisk(ctx context.Context, configDir string, progress supervisor.Progress, logf supervisor.Logf) error {
	configDir = plainPath(configDir)
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	start := b.clock().Now()
	var res broker.SnapshotResult
	err := b.longOp(ctx, configDir, broker.OpSnapshot, progress, logf, "copying the data disk for the snapshot", func(ctx context.Context) error {
		var err error
		res, err = b.Client.SnapshotVHD(ctx, configDir)
		return err
	})
	if err != nil {
		return err
	}
	logf("the data disk is copied for the snapshot: %s in %s", gib(res.CopiedBytes), b.clock().Now().Sub(start).Round(time.Second))
	return nil
}

// RestoreDataDiskSnapshot has the broker put the upgrade snapshot in the
// data disk's place (POST /v1/vhd/restore).
func (b Backend) RestoreDataDiskSnapshot(ctx context.Context, configDir string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return b.Client.RestoreVHD(ctx, plainPath(configDir))
}

// DeleteDataDiskSnapshot has the broker delete the upgrade snapshot
// (DELETE /v1/vhd/snapshot).
func (b Backend) DeleteDataDiskSnapshot(ctx context.Context, configDir string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := b.Client.DeleteSnapshot(ctx, plainPath(configDir))
	return err
}

// DeleteDataDisk has the broker delete the data disk and its snapshot,
// and the VM directory once the VM is gone (DELETE /v1/vhd).
func (b Backend) DeleteDataDisk(ctx context.Context, configDir string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := b.Client.DeleteVHD(ctx, plainPath(configDir))
	return res.FreedBytes, err
}

// NewMachine derives the team's VM identity from its config directory
// and makes the VM exist through the broker (POST /v1/vm), ready to
// start. Its disks are the broker's own: the root disk is root.vhdx
// installed beside it (broker.RootPath), whose differencing child and
// the data disk are in the team's VM directory; the image directory
// here gives only the image's version (Image).
func (b Backend) NewMachine(c supervisor.MachineConfig) (supervisor.Machine, error) {
	configDir := plainPath(c.ConfigDir)
	key := host.DirKey(configDir)
	req := broker.VMRequest{
		Name:      broker.VMName(key),
		ConfigDir: configDir,
		CPUs:      c.CPUs,
		MemoryMB:  c.MemoryMB,
		MAC:       broker.MAC(key),
		ComPipe:   broker.ComPipe(key),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := b.Client.EnsureVM(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("configure the VM: %w", err)
	}
	m := &machine{
		client:       b.Client,
		name:         req.Name,
		comPipe:      req.ComPipe,
		format:       c.FormatData,
		log:          c.ConsoleLog,
		pollEvery:    statePoll,
		offPoll:      2 * time.Second,
		leftoverWait: leftoverStopTimeout,
		events:       make(chan supervisor.ConsoleEvent, 64),
		done:         make(chan struct{}),
	}
	m.setID(st.ID)
	return m, nil
}

// StopLeftover shuts down, cleanly, the team's VM if an earlier serve
// left it running (settleLeftover); no VM, or one that is off, is no
// error. The name derives from the config directory as NewMachine's
// does.
func (b Backend) StopLeftover(configDir string, logf supervisor.Logf) error {
	m := &machine{
		client:        b.Client,
		name:          broker.VMName(host.DirKey(plainPath(configDir))),
		offPoll:       2 * time.Second,
		leftoverWait:  leftoverStopTimeout,
		logfn:         logf,
		guestShutdown: b.guestShutdown,
		done:          make(chan struct{}),
	}
	return m.settleLeftover()
}

// RemoveMachine removes the team's VM through the broker (DELETE
// /v1/vm), which takes its differencing disk with it but never the data
// disk, and so keeps the VM directory that holds it (DeleteDataDisk
// deletes both): a Hyper-V VM stays registered after it stops, so
// destroy calls this once it is stopped. The name derives from the
// config directory's key, as NewMachine's does. A VM that is already
// gone is no error.
func (b Backend) RemoveMachine(configDir string) error {
	name := broker.VMName(host.DirKey(configDir))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := b.Client.Remove(ctx, name); err != nil && !broker.IsNotFound(err) {
		return fmt.Errorf("remove the VM %s: %w", name, err)
	}
	return nil
}

type machine struct {
	client    *broker.Client
	name      string
	comPipe   string
	format    bool
	log       io.Writer
	pollEvery time.Duration // statePoll; the tests shorten it
	offPoll   time.Duration // how often waitOff asks; the tests shorten it
	// leftoverWait bounds each clean stop settleLeftover tries:
	// leftoverStopTimeout; the tests shorten it.
	leftoverWait time.Duration
	// logfn, when set, takes the machine's own lines instead of log.
	logfn supervisor.Logf
	// guestShutdown asks the guest agent for its clean shutdown; nil is
	// the agent over a Hyper-V socket (the tests have no VM to dial).
	guestShutdown func(ctx context.Context) error

	events chan supervisor.ConsoleEvent
	done   chan struct{}

	mu       sync.Mutex
	id       string             // the VM's GUID, for DialAgent
	conn     io.ReadWriteCloser // the console pipe, once Hyper-V has connected
	doneOnce sync.Once
}

func (m *machine) setID(id string) {
	m.mu.Lock()
	m.id = id
	m.mu.Unlock()
}

func (m *machine) getID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.id
}

// Start serves the VM's console pipe, starts the VM through the broker,
// waits for Hyper-V to connect to the pipe, scans the console for the
// marker lines and the first-boot format question, and begins polling
// the broker (slowly: the pipe's end is the signal) so a VM that stops
// is noticed even if the pipe does not end.
//
// The supervisor is the pipe's server, not its client. Hyper-V creates
// a COM port's pipe itself if nothing holds the name, with a security
// descriptor an ordinary account cannot open (the spike's first start
// failed with access denied); if the name exists it connects to it as a
// client, as LocalSystem (the spike tried the VM's virtual account and
// the Virtual Machines group: neither connected). So the pipe is made
// here, as this account, before the VM starts, as one instance with a
// connect already pending, admitting this account and LocalSystem only:
// a name already taken means the VM is not started.
func (m *machine) Start() error {
	if err := m.settleLeftover(); err != nil {
		m.markDone()
		return err
	}
	srv, err := m.serveConsole()
	if err != nil {
		m.markDone()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := m.client.Start(ctx, m.name)
	if err != nil {
		srv.close()
		m.markDone()
		return fmt.Errorf("start the VM: %w", err)
	}
	if st.ID != "" {
		m.setID(st.ID)
	}
	conn, err := srv.connected(consoleConnectTimeout)
	if err != nil {
		// A VM that started but whose console we cannot reach is stopped,
		// so boot fails cleanly rather than leaving it running unseen.
		_, _ = m.client.Stop(context.Background(), m.name, false)
		m.markDone()
		return fmt.Errorf("the VM did not connect to its console pipe %s: %w", m.comPipe, err)
	}
	m.mu.Lock()
	m.conn = conn
	m.mu.Unlock()

	// ScanConsole drives the reads and does the marker and log work, as
	// on macOS; a TeeReader copies every byte to a second line scanner
	// that answers the format question. ScanConsole and the vz backend
	// are untouched: the hook lives entirely here.
	pr, pw := io.Pipe()
	go func() {
		supervisor.ScanConsole(io.TeeReader(conn, pw), m.log, m.events)
		_ = pw.Close()
		m.markDone()
	}()
	go m.answerFormat(pr)
	go m.pollState()
	return nil
}

// leftoverStopTimeout bounds each clean way settleLeftover tries.
const leftoverStopTimeout = 3 * time.Minute

// settleLeftover makes sure the VM is Off before Start starts it. A
// Hyper-V VM outlives the supervisor that started it (a logoff, a crash,
// the app killing serve), and the broker answers a start of a running VM
// without starting anything, so Hyper-V would never connect to the new
// console pipe, and Start's cleanup would turn the VM off: a power cut
// on a guest with its data disk mounted and k3s writing. So a leftover
// VM is shut down cleanly first, through the guest agent's shutdown (the
// whole clean sequence), else Hyper-V's shutdown integration service.
// It is never turned off here: one that will not stop cleanly, or is in
// any state but Running and Off (Saved, Paused; PausedCritical is
// Hyper-V's answer to a full host disk), is an error for a person, with
// the guest's unwritten data still intact.
func (m *machine) settleLeftover() error {
	ctx, cancel := context.WithTimeout(context.Background(), stateQueryTimeout)
	st, err := m.client.VM(ctx, m.name)
	cancel()
	if broker.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the VM's state: %w", err)
	}
	switch st.State {
	case broker.StateOff:
		return nil
	case broker.StateRunning:
	default:
		return fmt.Errorf("the VM %s is %s, left so by an earlier run; it was not turned off, which could lose data the guest has not written. "+
			"Resume or start it in Hyper-V Manager (a paused VM usually means the disk holding Kivali's files is full: free space first), then try again", m.name, st.State)
	}
	if st.ID != "" {
		m.setID(st.ID)
	}
	m.logf("the VM is still running from an earlier run; shutting it down cleanly before starting it again")
	shutdown := m.guestShutdown
	if shutdown == nil {
		shutdown = (&guestapi.Client{Dial: m.DialAgent}).Shutdown
	}
	sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = shutdown(sctx)
	scancel()
	if err == nil {
		if m.waitOff(m.leftoverWait) {
			m.logf("the leftover VM shut down cleanly")
			return nil
		}
	} else {
		m.logf("the guest agent did not take the shutdown (%v); asking Hyper-V to shut the guest down", err)
	}
	gctx, gcancel := context.WithTimeout(context.Background(), m.leftoverWait)
	_, err = m.client.Stop(gctx, m.name, true)
	gcancel()
	if err == nil && m.waitOff(m.leftoverWait) {
		m.logf("the leftover VM shut down cleanly through Hyper-V")
		return nil
	}
	return fmt.Errorf("the VM %s, left running by an earlier run, did not shut down cleanly (%v); it was not turned off, which could lose data. "+
		"Shut it down from Hyper-V Manager, then try again", m.name, err)
}

// waitOff reports whether the broker reports the VM Off within d,
// asking every offPoll.
func (m *machine) waitOff(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), stateQueryTimeout)
		st, err := m.client.VM(ctx, m.name)
		cancel()
		if err == nil && st.State == broker.StateOff {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(m.offPoll)
	}
}

func (m *machine) logf(format string, args ...any) {
	if m.logfn != nil {
		m.logfn(format, args...)
		return
	}
	if m.log != nil {
		_, _ = fmt.Fprintf(m.log, "%s host: %s\n", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	}
}

// consoleServer is the console pipe's one instance with its connect
// pending (serveConsole), until Hyper-V connects or the wait ends.
type consoleServer struct {
	h     windows.Handle
	event windows.Handle
	o     windows.Overlapped
	// pending is false when a client had connected by the time the
	// connect was issued, which completes it at once.
	pending bool
}

// serveConsole creates the VM's COM1 pipe: one byte-mode, overlapped
// instance, created exclusively (so a name already held by anyone fails
// here, and nothing of the VM's reaches a pipe someone else serves), with
// a protected DACL that admits this account and the VM's virtual
// account only, and a connect already pending so that Hyper-V, which
// connects as the VM starts and does not retry, finds the instance
// listening.
func (m *machine) serveConsole() (*consoleServer, error) {
	me, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("this account's token: %w", err)
	}
	defer func() { _ = me.Close() }()
	u, err := me.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("this account's SID: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + u.User.Sid.String() + ")(A;;GA;;;SY)")
	if err != nil {
		return nil, fmt.Errorf("the console pipe's security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(m.comPipe)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateNamedPipe(name,
		windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 64<<10, 64<<10, 0, sa)
	if err != nil {
		return nil, fmt.Errorf("create the console pipe %s (a name another process holds is refused): %w", m.comPipe, err)
	}
	s := &consoleServer{h: h}
	s.event, err = windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("the console pipe's connect event: %w", err)
	}
	s.o.HEvent = s.event
	switch err := windows.ConnectNamedPipe(h, &s.o); {
	case err == nil, errors.Is(err, windows.ERROR_PIPE_CONNECTED):
	case errors.Is(err, windows.ERROR_IO_PENDING):
		s.pending = true
	default:
		s.close()
		return nil, fmt.Errorf("listen on the console pipe %s: %w", m.comPipe, err)
	}
	return s, nil
}

// connected waits up to timeout for the pending connect, then hands the
// instance to go-winio as an overlapped file, which reads and writes
// concurrently (the format answer and the poweroff verb are written
// while the console is being read).
func (s *consoleServer) connected(timeout time.Duration) (io.ReadWriteCloser, error) {
	if s.pending {
		ev, err := windows.WaitForSingleObject(s.event, uint32(timeout/time.Millisecond))
		if err != nil || ev != windows.WAIT_OBJECT_0 {
			_ = windows.CancelIoEx(s.h, &s.o)
			s.close()
			if err == nil {
				err = fmt.Errorf("no connection within %s", timeout)
			}
			return nil, err
		}
		var n uint32
		if err := windows.GetOverlappedResult(s.h, &s.o, &n, false); err != nil {
			s.close()
			return nil, err
		}
	}
	_ = windows.CloseHandle(s.event)
	s.event = 0
	f, err := winio.NewOpenFile(s.h)
	if err != nil {
		s.close()
		return nil, err
	}
	return f, nil
}

func (s *consoleServer) close() {
	if s.event != 0 {
		_ = windows.CloseHandle(s.event)
		s.event = 0
	}
	_ = windows.CloseHandle(s.h)
}

// answerFormat watches the raw console for the first-boot format
// question and answers it once, from MachineConfig.FormatData. Only the
// host ever says yes, and only on the boot right after it made the disk.
// It reads to the end so the tee it drives never blocks ScanConsole.
func (m *machine) answerFormat(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	answered := false
	for sc.Scan() {
		if answered || strings.TrimRight(sc.Text(), "\r") != askFormatLine {
			continue
		}
		answered = true
		ans := 0
		if m.format {
			ans = 1
		}
		_ = m.writeConsole(fmt.Sprintf(formatAnswerFmt, ans))
	}
	_, _ = io.Copy(io.Discard, r)
}

// writeConsole writes s to the console pipe under a lock, so the format
// answer and the poweroff verb can never interleave.
func (m *machine) writeConsole(s string) error {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn == nil {
		return errors.New("the console is not connected")
	}
	_, err := io.WriteString(conn, s)
	return err
}

// pollState closes Done when the broker reports the VM Off, for a stop
// that the console pipe did not end (a turn-off, a crash): the backstop
// statePoll describes.
func (m *machine) pollState() {
	t := time.NewTicker(m.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), stateQueryTimeout)
			st, err := m.client.VM(ctx, m.name)
			cancel()
			if err == nil && st.State == broker.StateOff {
				m.markDone()
				return
			}
		}
	}
}

func (m *machine) markDone() {
	m.doneOnce.Do(func() {
		close(m.done)
		m.mu.Lock()
		conn := m.conn
		m.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
	})
}

func (m *machine) Events() <-chan supervisor.ConsoleEvent { return m.events }

func (m *machine) Done() <-chan struct{} { return m.done }

// DialAgent connects to the guest agent over a Hyper-V socket: the VM's
// id and the vsock service id of the agent's port. It fails cleanly
// before the VM has an id.
func (m *machine) DialAgent(ctx context.Context) (net.Conn, error) {
	id := m.getID()
	if id == "" {
		return nil, errors.New("the VM has no id yet")
	}
	vmID, err := guid.FromString(id)
	if err != nil {
		return nil, fmt.Errorf("the VM id %q is not a GUID: %w", id, err)
	}
	addr := &winio.HvsockAddr{VMID: vmID, ServiceID: winio.VsockServiceID(uint32(guestapi.Port))}
	c, err := winio.Dial(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("dial the guest agent over a Hyper-V socket: %w", err)
	}
	return c, nil
}

// RequestPoweroff writes the poweroff verb to the console, as on macOS.
func (m *machine) RequestPoweroff() error {
	return m.writeConsole(poweroffVerb)
}

// HardStop turns the VM off through the broker and waits for Done.
func (m *machine) HardStop() error {
	select {
	case <-m.done:
		return nil
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// A paused VM is frozen, not wedged: Hyper-V pauses one whose host
	// volume is full (PausedCritical) rather than fail its writes, and it
	// resumes intact once there is room. Turning it off would throw away
	// everything the guest had not yet written, so it is left paused.
	if st, err := m.client.VM(ctx, m.name); err == nil && strings.HasPrefix(st.State, "Paused") {
		return fmt.Errorf("the VM is %s (Hyper-V pauses a VM whose host disk is full); it was not turned off, which would lose data the guest has not written: free space on the disk holding Kivali's files, then resume it in Hyper-V Manager", st.State)
	}
	if _, err := m.client.Stop(ctx, m.name, false); err != nil {
		return fmt.Errorf("turn the VM off: %w", err)
	}
	<-m.done
	return nil
}
