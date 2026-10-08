package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// Logf reports progress of one operation.
type Logf func(format string, args ...any)

// NodePort is the Service port the chart exposes the server on.
const NodePort = 30080

// Namespace the chart is installed into.
const Namespace = "kivali"

// Timeouts of the flows. They bound waits on the guest, not work.
const (
	bootTimeout    = 10 * time.Minute
	agentTimeout   = 2 * time.Minute
	stopTimeout    = 3 * time.Minute
	consoleTimeout = 2 * time.Minute
	deployTimeout  = 10 * time.Minute
	pollInterval   = 2 * time.Second
	// stopHeartbeat is how often a stop that is waiting for the guest
	// says so: well inside the desktop app's 30 s idle limit on a down.
	stopHeartbeat = 10 * time.Second
)

// Options configures a Supervisor.
type Options struct {
	// ConfigDir holds local.json, the data disk, the RPC endpoint and
	// logs.
	ConfigDir string
	// VMDir holds the VM image Backend boots.
	VMDir string
	// Backend runs the VM; required.
	Backend Backend
	// Host is the operating system; host.Default() when nil.
	Host Host
	// GuestFor returns the agent client of a machine; AgentGuest by
	// default.
	GuestFor func(Machine) Guest
	Clock    Clock
	// HTTP fetches the feed, release assets and /readyz.
	HTTP *http.Client
	// Version is this supervisor's version, compared with a release's
	// minimum desktop version. "dev" satisfies every minimum.
	Version string
	// Logf is the serve log; every operation's progress also goes here.
	Logf Logf
	// Home is the user's home directory, where backups may also be
	// written; os.UserHomeDir by default.
	Home string
	// Models are the model ids the org runs the CLI with, which a
	// sign-in setup checks one by one; the composition root takes them
	// from the Claude driver.
	Models []string

	// terminalsChanged sees the terminal count change, in tests.
	terminalsChanged func(n int)
}

// Supervisor owns the VM and local.json. One operation that changes
// the org runs at a time; status is always answered.
type Supervisor struct {
	o Options
	// image is the VM image's description, read once by New.
	image ImageInfo

	// op is the operation lock, held for the whole of every mutating
	// operation: a one-slot semaphore, so waiting for it can be
	// abandoned (a `down` queued behind an upgrade).
	op chan struct{}

	mu      sync.Mutex // guards the fields below; never held while waiting on anything
	state   State
	machine Machine
	guest   Guest
	// console is the running machine's console log, closed as soon as the
	// machine is known to be done (stopLocked, watch): Windows will not
	// rename a file that is open, and the next boot renames it.
	console *os.File
	fwd     *Forwarder
	// fwdGuest is the guest fwd dials; a new boot needs a new forward.
	fwdGuest Guest
	// opName is the running operation ("" when none).
	opName string
	// cancelOp cancels the running operation if it may be cancelled
	// (everything but an upgrade, which owns the journal).
	cancelOp context.CancelFunc

	// terminals counts the open terminal sessions.
	terminals atomic.Int32
}

// begin takes the operation lock, giving up if ctx ends first. A
// cancellable operation's context is cancelled by Down, so a stop never
// waits behind a long wait.
func (s *Supervisor) begin(ctx context.Context, name string, cancellable bool) (context.Context, func(), error) {
	select {
	case s.op <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	return s.started(ctx, name, cancellable)
}

// beginNow is begin for an operation that refuses, rather than waits,
// while another one runs.
func (s *Supervisor) beginNow(ctx context.Context, name string, cancellable bool) (context.Context, func(), error) {
	select {
	case s.op <- struct{}{}:
	default:
		running := "another operation"
		if b := s.busy(); b != "" {
			running = withArticle(b)
		}
		return nil, nil, fmt.Errorf("%s is running; try again once it has finished", running)
	}
	return s.started(ctx, name, cancellable)
}

// withArticle is "an upgrade", "a down".
func withArticle(name string) string {
	if name != "" && strings.ContainsAny(name[:1], "aeiou") {
		return "an " + name
	}
	return "a " + name
}

// started registers the operation whose lock the caller just took.
func (s *Supervisor) started(ctx context.Context, name string, cancellable bool) (context.Context, func(), error) {
	opCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.opName = name
	if cancellable {
		s.cancelOp = cancel
	}
	s.mu.Unlock()
	return opCtx, func() {
		cancel()
		s.mu.Lock()
		s.cancelOp = nil
		s.opName = ""
		s.mu.Unlock()
		<-s.op
	}, nil
}

// busy reports the running operation's name.
func (s *Supervisor) busy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opName
}

// dropForward stops the port forward. The forward's Close waits for
// its connections, so it runs outside s.mu.
func (s *Supervisor) dropForward() {
	s.mu.Lock()
	f := s.fwd
	s.fwd, s.fwdGuest = nil, nil
	s.mu.Unlock()
	if f != nil {
		f.Close()
	}
}

// New loads local.json from o.ConfigDir (creating the directory,
// owner-only) and returns a supervisor with no VM running.
func New(o Options) (*Supervisor, error) {
	if o.ConfigDir == "" {
		return nil, errors.New("supervisor: no config directory")
	}
	if o.Backend == nil {
		return nil, errors.New("supervisor: no VM backend")
	}
	if o.Host == nil {
		o.Host = host.Default()
	}
	image, err := o.Backend.Image(o.VMDir)
	if err != nil {
		return nil, fmt.Errorf("VM image: %w", err)
	}
	if err := os.MkdirAll(o.ConfigDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(o.ConfigDir, "logs"), 0o700); err != nil {
		return nil, err
	}
	if o.GuestFor == nil {
		o.GuestFor = AgentGuest
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	st, err := LoadState(o.ConfigDir, o.Backend.DataDiskPath(o.ConfigDir))
	if err != nil {
		return nil, err
	}
	return &Supervisor{o: o, image: image, state: st, op: make(chan struct{}, 1)}, nil
}

// platform is the guest's image platform, the key of a release's image
// bundles.
func (s *Supervisor) platform() string { return "linux/" + s.image.Arch }

// tee sends a line to the operation's log and the serve log; the serve
// log shows the operation's stage (ctx's) in brackets.
func (s *Supervisor) tee(ctx context.Context, logf Logf) Logf {
	h := stageHolderOf(ctx)
	return func(format string, args ...any) {
		if st := h.get(); st != "" {
			s.o.Logf("[%s] "+format, append([]any{st}, args...)...)
		} else {
			s.o.Logf(format, args...)
		}
		if logf != nil {
			logf(format, args...)
		}
	}
}

// State returns a copy of local.json's current content.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// update changes the state and saves it.
func (s *Supervisor) update(fn func(st *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	if next.Upgrade != nil {
		j := *next.Upgrade
		next.Upgrade = &j
	}
	fn(&next)
	if err := SaveState(s.o.Host, s.o.ConfigDir, next); err != nil {
		return fmt.Errorf("save local.json: %w", err)
	}
	s.state = next
	return nil
}

// dataPath is the data disk, where the backend keeps it.
func (s *Supervisor) dataPath() string { return s.o.Backend.DataDiskPath(s.o.ConfigDir) }

// dataDir is the directory holding the data disk and its snapshot,
// whose renames must be durable.
func (s *Supervisor) dataDir() string { return filepath.Dir(s.dataPath()) }

func (s *Supervisor) snapshotPath() string { return s.dataPath() + SnapshotSuffix }

func (s *Supervisor) current() (Machine, Guest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.machine, s.guest
}

// running returns the guest of the running VM, or an error.
func (s *Supervisor) running() (Guest, error) {
	_, g := s.current()
	if g == nil {
		return nil, errors.New("the VM is not running (run `kivali-supervisor up`)")
	}
	return g, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Windows refuses to rename a file that is open. The console log of a
// machine known to be done is closed already (stopLocked, watch), but a
// boot that failed closes its own on a goroutine, and a person may have
// the file open in a viewer: rotateConsole waits this long for it.
const (
	consoleRenameWait = 2 * time.Second
	consoleRenameStep = 50 * time.Millisecond
)

// rotateConsole opens a fresh console log, keeping the previous boot's
// as console.log.1. If the previous one still cannot be moved, it stays
// where it is and the new boot's lines are appended after a marker
// line: the boot someone goes looking for is usually the one that just
// failed, so a log holding two boots loses less than a truncated one.
func (s *Supervisor) rotateConsole(logf Logf) (*os.File, string, error) {
	p := filepath.Join(s.o.ConfigDir, "logs", "console.log")
	// A destroy removes the logs directory.
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, "", err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	var kept error
	if exists(p) {
		kept = os.Rename(p, p+".1")
		for waited := time.Duration(0); kept != nil && waited < consoleRenameWait; waited += consoleRenameStep {
			<-s.o.Clock.After(consoleRenameStep)
			kept = os.Rename(p, p+".1")
		}
		if kept != nil {
			flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
	}
	f, err := os.OpenFile(p, flags, 0o600)
	if err == nil && kept != nil {
		logf("the previous console log could not be moved to console.log.1 (%v); this boot is appended to %s", kept, p)
		_, _ = fmt.Fprintf(f, "%s supervisor: the previous console log could not be moved to console.log.1 (%v); this boot follows it\n",
			s.o.Clock.Now().UTC().Format("15:04:05.000"), kept)
	}
	return f, p, err
}

// bootLocked starts the VM and waits for READY and the guest agent.
// The caller holds s.op and no VM is running.
func (s *Supervisor) bootLocked(ctx context.Context, logf Logf) error {
	st := s.State()
	data := s.dataPath()
	fresh := false
	_, statErr := os.Stat(data)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		setStage(ctx, StageMakingRoom)
		logf("creating the data disk %s (%d GiB)", data, DataDiskSize>>30)
		fresh = true
	case statErr != nil:
		// Only a disk that is certainly absent is created: an error
		// reading it (permissions, a volume not mounted yet) must never
		// lead to the recreate below.
		return fmt.Errorf("cannot check the data disk %s: %w", data, statErr)
	case !st.DataDiskFormatted:
		// vm/README.md: a data disk whose first boot never reached
		// READY is disposable; never retry the flag on the old file.
		setStage(ctx, StageMakingRoom)
		logf("the data disk's first boot never reached READY; recreating %s", data)
		fresh = true
	}
	if fresh {
		if err := s.update(func(st *State) { st.DataDiskFormatted = false }); err != nil {
			return err
		}
		if err := s.o.Backend.CreateDataDisk(data, DataDiskSize); err != nil {
			return fmt.Errorf("create data disk: %w", err)
		}
	}
	// The disk holds the org's secrets in plaintext (docs/developers/supervisor.md);
	// keep it out of backups. Idempotent, so every boot re-asserts it.
	if err := s.o.Host.ExcludeFromBackup(data); err != nil {
		logf("exclude %s from backups: %v", data, err)
	}
	console, consolePath, err := s.rotateConsole(logf)
	if err != nil {
		return err
	}
	m, err := s.o.Backend.NewMachine(MachineConfig{
		ImageDir:   s.o.VMDir,
		DataDisk:   data,
		FormatData: fresh,
		CPUs:       st.CPUs,
		MemoryMB:   st.MemoryMB,
		ConsoleLog: console,
	})
	if err != nil {
		_ = console.Close()
		return fmt.Errorf("configure VM: %w", err)
	}
	start := s.o.Clock.Now()
	if err := m.Start(); err != nil {
		_ = console.Close()
		return fmt.Errorf("start VM: %w", err)
	}
	// The backstop for every path that does not know the machine is done
	// (a boot that fails from here on, a stop that gave up): the paths that
	// do close the file first, and closing it twice is harmless.
	go func() { <-m.Done(); _ = console.Close() }()
	firstBoot := ""
	if fresh {
		firstBoot = "; first boot, the guest formats the data disk"
	}
	logf("VM started (%d CPUs, %d MiB, console %s%s)", st.CPUs, st.MemoryMB, consolePath, firstBoot)

	if err := s.waitReady(ctx, m); err != nil {
		s.stopAbandoned(m, logf)
		return fmt.Errorf("%w (console log: %s)", err, consolePath)
	}
	logf("%s after %s", ReadyLine, s.o.Clock.Now().Sub(start).Round(time.Second))
	if fresh {
		if err := s.update(func(st *State) { st.DataDiskFormatted = true }); err != nil {
			s.stopAbandoned(m, logf)
			return err
		}
		setStage(ctx, StageStarting)
	}

	g := s.o.GuestFor(m)
	var gs guestapi.Status
	err = poll(ctx, s.o.Clock, agentTimeout, time.Second, "the guest agent to report READY", func(ctx context.Context) error {
		c, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		st, err := g.Status(c)
		if err != nil {
			return err
		}
		switch st.State {
		case guestapi.StateReady:
			gs = st
			return nil
		case guestapi.StateFatal:
			return permanent(fmt.Errorf("guest reported FATAL: %s", st.Fatal))
		}
		return fmt.Errorf("guest state %s", st.State)
	})
	if err != nil {
		s.stopAbandoned(m, logf)
		return err
	}
	logf("guest agent %s: boot %s, vm image %q, %s", gs.Versions.Agent, gs.BootID, gs.Versions.VMImage, gs.Versions.K3s)
	if p := gs.DataDisk.Problem(); p != "" {
		logf("WARNING: %s; make a backup now", p)
	}
	if err := s.update(func(st *State) { st.VMImage = gs.Versions.VMImage }); err != nil {
		s.stopAbandoned(m, logf)
		return err
	}
	s.mu.Lock()
	s.machine, s.guest, s.console = m, g, console
	s.mu.Unlock()
	go s.watch(m)
	return nil
}

// stopAbandoned stops a machine whose boot is being given up (a down or
// a shutdown during the boot, a timeout, a failed state save), which is
// not yet s.machine, so nothing else would ever stop it: left running,
// the next boot would start a second VM on the same data disk. By then
// the guest may have the disk mounted (e2fsck, k3s), so the stop is the
// clean one: the poweroff verb, which the guest takes once its boot
// script has finished, running the whole shutdown. Only a guest that has
// not stopped within stopTimeout is powered off hard. A guest that
// reported FATAL is already powering itself off.
func (s *Supervisor) stopAbandoned(m Machine, logf Logf) {
	select {
	case <-m.Done():
		return
	default:
	}
	logf("stopping the VM of the abandoned boot cleanly")
	if err := m.RequestPoweroff(); err != nil {
		logf("poweroff request: %v", err)
	}
	start := s.o.Clock.Now()
	deadline := s.o.Clock.After(stopTimeout)
	beat := s.o.Clock.After(stopHeartbeat)
	for {
		select {
		case <-m.Done():
			return
		default:
		}
		select {
		case <-deadline:
			logf("the VM did not stop within %s; powering it off hard", stopTimeout)
			if err := m.HardStop(); err != nil {
				logf("hard stop: %v", err)
				return
			}
			<-m.Done()
			return
		default:
		}
		select {
		case <-m.Done():
			return
		case <-deadline:
		case <-beat:
			logf("waiting for the VM to finish its clean shutdown (%s so far)", s.o.Clock.Now().Sub(start).Round(time.Second))
			beat = s.o.Clock.After(stopHeartbeat)
		}
	}
}

// waitReady waits for READY on the console. FATAL, the VM stopping or
// the timeout fail the boot.
func (s *Supervisor) waitReady(ctx context.Context, m Machine) error {
	deadline := s.o.Clock.After(bootTimeout)
	handle := func(ev ConsoleEvent) (bool, error) {
		switch ev.Kind {
		case EventReady:
			return true, nil
		case EventFatal:
			return true, fmt.Errorf("guest failed to boot: %s", ev.Line)
		}
		return false, nil
	}
	for {
		// A marker already seen wins over the VM stopping, which wins
		// over the timeout: a FATAL is followed by a power-off.
		select {
		case ev := <-m.Events():
			if done, err := handle(ev); done {
				return err
			}
			continue
		default:
		}
		select {
		case <-m.Done():
			for {
				select {
				case ev := <-m.Events():
					if done, err := handle(ev); done && err != nil {
						return err
					}
				default:
					return errors.New("the VM stopped before " + ReadyLine)
				}
			}
		default:
		}
		select {
		case ev := <-m.Events():
			if done, err := handle(ev); done {
				return err
			}
		case <-m.Done():
		case <-deadline:
			return fmt.Errorf("no %s within %s", ReadyLine, bootTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// watch forgets a machine that stopped on its own.
func (s *Supervisor) watch(m Machine) {
	<-m.Done()
	s.mu.Lock()
	if s.machine != m {
		s.mu.Unlock()
		return
	}
	// Closed before the machine is forgotten, so that a boot that finds
	// no machine finds the console log closed. Closing a file waits for
	// nothing.
	_ = s.console.Close()
	s.machine, s.guest, s.console = nil, nil, nil
	s.mu.Unlock()
	s.dropForward()
	s.o.Logf("the VM stopped")
}

// stopLocked shuts the VM down cleanly: the agent's shutdown, the
// backend's poweroff request if the agent cannot be reached, and a hard
// stop only if neither stops the VM in time. The caller holds s.op.
func (s *Supervisor) stopLocked(ctx context.Context, logf Logf) error {
	s.mu.Lock()
	m, g, console := s.machine, s.guest, s.console
	s.mu.Unlock()
	if m == nil {
		return nil
	}
	s.dropForward()
	defer func() {
		// A machine that is done writes no more, and the next boot renames
		// this file: on Windows that fails while it is open. Closed here,
		// not left to watch's goroutine, which may not have run yet. A stop
		// that gave up with the VM still running leaves it to bootLocked's.
		select {
		case <-m.Done():
			_ = console.Close()
		default:
		}
		s.mu.Lock()
		if s.machine == m {
			s.machine, s.guest, s.console = nil, nil, nil
		}
		s.mu.Unlock()
	}()

	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := g.Shutdown(c)
	cancel()
	if err != nil {
		logf("the guest agent did not take the shutdown (%v); asking the VM to power off", err)
		if err := m.RequestPoweroff(); err != nil {
			logf("poweroff request: %v", err)
		}
	} else {
		logf("clean shutdown requested through the guest agent")
	}
	clean := false
	stopped := func() error {
		// The stop can overtake the last console lines.
		for drained := false; !drained; {
			select {
			case ev := <-m.Events():
				clean = clean || ev.Kind == EventClean
			default:
				drained = true
			}
		}
		if clean {
			logf("VM stopped; data disk unmounted cleanly")
		} else {
			logf("VM stopped (the clean-unmount line was not seen; see logs/console.log)")
		}
		return nil
	}
	start := s.o.Clock.Now()
	deadline := s.o.Clock.After(stopTimeout)
	beat := s.o.Clock.After(stopHeartbeat)
	for {
		// The VM having stopped wins over the timeout, and the timeout
		// over a heartbeat.
		select {
		case <-m.Done():
			return stopped()
		default:
		}
		select {
		case <-deadline:
			return hardStopLate(m)
		default:
		}
		select {
		case ev := <-m.Events():
			if ev.Kind == EventClean {
				clean = true
			}
		case <-m.Done():
			return stopped()
		case <-deadline:
			return hardStopLate(m)
		case <-beat:
			// A caller that treats a silent operation as wedged (the
			// desktop app's quit) must not kill this process while the
			// guest is still shutting down: on macOS the VM lives in it,
			// so that kill is a power cut.
			logf("waiting for the VM to finish its clean shutdown (%s so far)", s.o.Clock.Now().Sub(start).Round(time.Second))
			beat = s.o.Clock.After(stopHeartbeat)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// hardStopLate powers off a VM that did not stop within stopTimeout. A
// backend may refuse (a Hyper-V VM paused on a full host disk is never
// turned off): that is the error, and nothing waits for a stop that will
// not come.
func hardStopLate(m Machine) error {
	if err := m.HardStop(); err != nil {
		return fmt.Errorf("the VM did not stop within %s and was not powered off: %w", stopTimeout, err)
	}
	<-m.Done()
	return fmt.Errorf("the VM did not stop within %s; it was powered off hard", stopTimeout)
}

// ensureForwardLocked listens on 127.0.0.1:port and forwards to the
// guest's NodePort. The forward dials the guest of the VM running now,
// captured once: connections never take s.mu, so stopping the forward
// (which waits for them) can never wait on a lock its caller holds.
func (s *Supervisor) ensureForwardLocked(port int, logf Logf) error {
	g, err := s.running()
	if err != nil {
		return err
	}
	s.mu.Lock()
	same := s.fwd != nil && s.fwd.Port() == port && s.fwdGuest == g
	s.mu.Unlock()
	if same {
		return nil
	}
	s.dropForward()
	f, err := StartForward(port, func(ctx context.Context) (net.Conn, error) {
		return g.Proxy(ctx, NodePort)
	}, s.o.Logf)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.fwd, s.fwdGuest = f, g
	s.mu.Unlock()
	logf("forwarding http://%s to the guest's NodePort %d", f.Addr(), NodePort)
	return nil
}

// forwardForUpLocked starts the forward on the recorded port (the
// default until the first forward records one). When another program
// holds that port and this up did not ask for it by name, the forward
// takes a free port and the choice is recorded, so a desktop whose 8080
// is busy still comes up; the shell reads the port from status, and the
// next up tries the chosen port first. A port given explicitly is never
// moved.
func (s *Supervisor) forwardForUpLocked(explicit bool, logf Logf) error {
	st := s.State()
	port := st.port()
	err := s.ensureForwardLocked(port, logf)
	if err == nil && st.Port == 0 {
		return s.update(func(st *State) { st.Port = port })
	}
	if err == nil || explicit || !s.o.Host.AddrInUse(err) {
		return err
	}
	logf("127.0.0.1:%d is in use by another program; taking a free port instead", port)
	if err := s.ensureForwardLocked(0, logf); err != nil {
		return err
	}
	s.mu.Lock()
	chosen := s.fwd.Port()
	s.mu.Unlock()
	return s.update(func(st *State) { st.Port = chosen })
}

func (s *Supervisor) baseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.State().port())
}

// UpOptions are the flags of `up`.
type UpOptions struct {
	Install InstallOptions `json:"install"`
	// Port is recorded and never moved: up fails if it is busy.
	Port int `json:"port,omitempty"`
	// PortHint is the first port of an org with none recorded yet (the
	// shell spreads its teams' orgs apart); unlike Port it moves to a
	// free one when busy, and an org with a recorded port ignores it.
	PortHint int    `json:"port_hint,omitempty"`
	MemoryMB uint64 `json:"memory_mb,omitempty"`
	CPUs     uint   `json:"cpus,omitempty"`
	// Prepare boots the VM and starts the forward but stops short of
	// installing Kivali when it is not installed yet: the desktop starts a
	// new team's machine while setup is still asking for its owner, and
	// installs with a second up once it knows.
	Prepare bool `json:"prepare,omitempty"`
}

// Up recovers an interrupted upgrade, boots the VM if it is not
// running, starts the forward and installs Kivali if it is not
// installed yet.
func (s *Supervisor) Up(ctx context.Context, o UpOptions, logf Logf) error {
	ctx, end, err := s.begin(ctx, "up", true)
	if err != nil {
		return err
	}
	defer end()
	setStage(ctx, StageStarting)
	logf = s.tee(ctx, logf)
	if m, _ := s.current(); m == nil {
		// With a journal in play, local.json on disk is the truth: a
		// person may have edited it to get past a refused recovery
		// (docs/developers/supervisor.md), and saving the in-memory copy first would
		// silently undo that edit.
		disk, err := LoadState(s.o.ConfigDir, s.dataPath())
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.state.Upgrade != nil || disk.Upgrade != nil {
			s.state = disk
		}
		s.mu.Unlock()
	}
	if err := s.update(func(st *State) {
		if o.Port != 0 {
			st.Port = o.Port
		} else if st.Port == 0 && o.PortHint != 0 {
			st.Port = o.PortHint
		}
		if o.MemoryMB != 0 {
			st.MemoryMB = o.MemoryMB
		}
		if o.CPUs != 0 {
			st.CPUs = o.CPUs
		}
	}); err != nil {
		return err
	}
	if m, _ := s.current(); m == nil {
		if ls, ok := s.o.Backend.(LeftoverStopper); ok {
			if err := ls.StopLeftover(s.o.ConfigDir, logf); err != nil {
				return err
			}
		}
		if err := s.recoverLocked(logf); err != nil {
			return err
		}
		if err := s.bootLocked(ctx, logf); err != nil {
			return err
		}
	} else {
		logf("the VM is already running")
	}
	if err := s.forwardForUpLocked(o.Port != 0, logf); err != nil {
		return err
	}
	setStage(ctx, StageSettingUp)
	installed, err := s.installedLocked(ctx)
	if err != nil {
		return err
	}
	if installed {
		logf("Kivali %s is installed; waiting for it to serve", s.State().Kivali)
		return s.waitServing(ctx, "", "", logf)
	}
	if o.Prepare {
		logf("the VM is ready; Kivali installs once its owner is known")
		return nil
	}
	return s.installLocked(ctx, o.Install, logf)
}

// Down shuts the VM down cleanly. It first cancels a running up,
// install or load-images; an upgrade it waits for, saying so at once.
// ctx bounds only that wait: once the stop has begun it runs to the end.
//
// ctx governs only the wait behind an operation that is not cancelled
// (an upgrade): ending it abandons that wait, and nothing has changed.
// Once Down has cancelled a running operation it always goes on to the
// stop, whatever happens to ctx, so a cancelled up is never left without
// one. The stop itself runs to its own bound.
func (s *Supervisor) Down(ctx context.Context, logf Logf) error {
	setStage(ctx, StagePausing)
	logf = s.tee(ctx, logf)
	if s.cancelRunning(logf) {
		ctx = context.WithoutCancel(ctx)
	}
	return s.downLocked(ctx, 0, logf)
}

// Shutdown is serve's exit on a signal: it waits for any running
// operation however long it takes (an upgrade is never abandoned, or
// the VM would die with serve in the middle of it), then stops the VM,
// bounding only the stop by stopBound, counted from when it starts.
func (s *Supervisor) Shutdown(stopBound time.Duration, logf Logf) error {
	logf = s.tee(context.Background(), logf)
	s.cancelRunning(logf)
	return s.downLocked(context.Background(), stopBound, logf)
}

// cancelRunning cancels a running cancellable operation and reports
// whether it did; otherwise it says what it will wait for.
func (s *Supervisor) cancelRunning(logf Logf) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelOp != nil {
		s.cancelOp()
		return true
	}
	if s.opName != "" {
		logf("%s is running; waiting for it to finish", withArticle(s.opName))
	}
	return false
}

// downLocked takes the operation lock (giving up only if waitCtx ends)
// and stops the VM; a non-zero stopBound limits the stop alone (the
// stop has its own bounds besides).
func (s *Supervisor) downLocked(waitCtx context.Context, stopBound time.Duration, logf Logf) error {
	_, end, err := s.begin(waitCtx, "down", false)
	if err != nil {
		return fmt.Errorf("stopped waiting: %w", err)
	}
	defer end()
	if m, _ := s.current(); m == nil {
		logf("the VM is not running")
		return nil
	}
	stopCtx := context.WithoutCancel(waitCtx)
	if stopBound > 0 {
		var cancel context.CancelFunc
		stopCtx, cancel = context.WithTimeout(stopCtx, stopBound)
		defer cancel()
	}
	return s.stopLocked(stopCtx, logf)
}

// Forward moves the host port forward to port and records it.
func (s *Supervisor) Forward(ctx context.Context, port int, logf Logf) error {
	_, end, err := s.begin(ctx, "forward", false)
	if err != nil {
		return err
	}
	defer end()
	logf = s.tee(ctx, logf)
	if port == 0 {
		port = s.State().port()
	}
	if err := s.update(func(st *State) { st.Port = port }); err != nil {
		return err
	}
	if m, _ := s.current(); m == nil {
		logf("the VM is not running; the forward to 127.0.0.1:%d starts with the next `up`", port)
		return nil
	}
	return s.ensureForwardLocked(port, logf)
}

// Report is the answer to status.
type Report struct {
	Running    bool             `json:"running"`
	Forward    string           `json:"forward,omitempty"`
	URL        string           `json:"url,omitempty"`
	Guest      *guestapi.Status `json:"guest,omitempty"`
	GuestError string           `json:"guest_error,omitempty"`
	State      State            `json:"state"`
	ConfigDir  string           `json:"config_dir"`
	Version    string           `json:"supervisor_version"`
	Busy       bool             `json:"busy"`
	// Operation is the running operation (up, down, install,
	// load-images, forward, upgrade, credential, destroy), "" when
	// idle.
	Operation string `json:"operation,omitempty"`
	// Terminals is the number of open terminal sessions: the shell sees
	// the one running `claude` login close.
	Terminals int `json:"terminals"`
	// DiskUsedBytes and DiskSizeBytes are the data disk's allocated
	// bytes and logical size (the Host's FileUsage); 0 with no disk.
	DiskUsedBytes int64 `json:"disk_used_bytes"`
	DiskSizeBytes int64 `json:"disk_size_bytes"`
	// DiskProblem is the guest's account of damage to, or a failing,
	// data disk filesystem (guestapi.DataDiskHealth.Problem), "" when it
	// is healthy or the guest did not answer.
	DiskProblem string `json:"disk_problem,omitempty"`
}

// Status reports the VM, the guest and local.json. It never waits for
// a running operation.
func (s *Supervisor) Status(ctx context.Context) Report {
	r := Report{State: s.State(), ConfigDir: s.o.ConfigDir, Version: s.o.Version}
	r.Operation = s.busy()
	r.Busy = r.Operation != ""
	r.Terminals = int(s.terminals.Load())
	if used, size, err := s.o.Host.FileUsage(s.dataPath()); err == nil {
		r.DiskUsedBytes, r.DiskSizeBytes = used, size
	}
	m, g := s.current()
	s.mu.Lock()
	if s.fwd != nil {
		r.Forward = s.fwd.Addr()
		r.URL = "http://" + s.fwd.Addr() + "/"
	}
	s.mu.Unlock()
	if m == nil {
		return r
	}
	r.Running = true
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	st, err := g.Status(c)
	if err != nil {
		r.GuestError = err.Error()
	} else {
		r.Guest = &st
		r.DiskProblem = st.DataDisk.Problem()
	}
	return r
}
