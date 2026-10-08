//go:build windows

// Package hyperv is the Windows Backend: it runs the Kivali Desktop VM
// as a Hyper-V virtual machine, one per team, managed through the
// privileged broker (internal/supervisor/broker) over its named pipe.
// The supervisor keeps its ordinary rights; only the broker touches
// Hyper-V. docs/developers/supervisor.md, "The Windows backend", is the
// design; it mirrors the macOS backend (internal/supervisor/vz).
//
// The VM boots a differencing child of the release's root.vhdx and a
// dynamic data.vhdx, on Hyper-V's Default Switch, with COM1 on a named
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

	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/broker"
	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// The VM image's files on Windows.
const (
	// RootFile is the bootable root disk, a dynamic VHDX with an EFI
	// partition and the squashfs. The VM boots a differencing child of
	// it.
	RootFile = "root.vhdx"
	// VersionFile holds the image's version ("dev" without one).
	VersionFile = "VERSION"
	// DataDiskFile is the data disk's name in the config directory.
	DataDiskFile = "data.vhdx"
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

	// guestShutdown, for tests, replaces the guest agent's shutdown in
	// StopLeftover (there is no VM to dial).
	guestShutdown func(ctx context.Context) error
}

var (
	_ supervisor.Backend         = Backend{}
	_ supervisor.MachineRemover  = Backend{}
	_ supervisor.LeftoverStopper = Backend{}
)

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

// DataDiskPath is data.vhdx in the config directory.
func (Backend) DataDiskPath(configDir string) string {
	return filepath.Join(configDir, DataDiskFile)
}

// CreateDataDisk removes any file at p and asks the broker for a new
// dynamic VHDX of size bytes there. The supervisor owns the file (the
// broker grants it full control), so the supervisor removes any
// existing one itself; the broker refuses to create over anything present.
func (b Backend) CreateDataDisk(p string, size int64) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := b.Client.NewVHD(ctx, p, size)
	return err
}

// NewMachine derives the team's VM identity from its config directory
// and makes the VM exist through the broker (POST /v1/vm), ready to
// start. The config directory is the data disk's directory.
func (b Backend) NewMachine(c supervisor.MachineConfig) (supervisor.Machine, error) {
	dataDisk := plainPath(c.DataDisk)
	configDir := filepath.Dir(dataDisk)
	key := host.DirKey(configDir)
	// Hyper-V reaches the data disk as the broker service's account,
	// which needs an entry on the directory the disk is in (folder.go).
	if err := grantBrokerFolder(configDir); err != nil {
		return nil, fmt.Errorf("let the broker reach the data disk: %w", err)
	}
	req := broker.VMRequest{
		Name:      broker.VMName(key),
		ConfigDir: configDir,
		RootVHDX:  filepath.Join(plainPath(c.ImageDir), RootFile),
		DataVHDX:  dataDisk,
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
// /v1/vm), which takes its differencing disk and the broker's VM
// directory with it but never the data disk: a Hyper-V VM stays
// registered after it stops, so destroy calls this once it is stopped.
// The name derives from the config directory's key, as NewMachine's
// does from the data disk's directory, which is the same one. A VM that
// is already gone is no error.
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
