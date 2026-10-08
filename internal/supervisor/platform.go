// Package supervisor runs a local Kivali org: it boots the Kivali
// Desktop VM through a Backend, talks to the guest agent over the
// backend's agent connection (./guestapi), forwards the org's port to
// the host's loopback, installs and upgrades Kivali through k3s's Helm
// controller, and serves all of it on the Host's owner-only RPC
// endpoint. Everything that differs between operating systems sits
// behind Backend (./vz on macOS, ./wsl on Windows) and Host (./host).
// docs/developers/supervisor.md is the reference; docs/developers/desktop-app.md, "The
// platform boundary", is the design.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// Backend is one way of running the Kivali Desktop VM:
// Virtualization.framework on macOS (package vz), WSL2 on Windows
// (package wsl).
type Backend interface {
	// Image describes the VM image artifacts this backend boots from
	// dir: their version and the guest architecture.
	Image(dir string) (ImageInfo, error)
	// DataDiskPath is where this backend keeps the org's data disk
	// under the config directory.
	DataDiskPath(configDir string) string
	// CreateDataDisk makes an empty data disk of the given size the
	// guest will format on its first boot, replacing any file at path.
	CreateDataDisk(path string, size int64) error
	NewMachine(cfg MachineConfig) (Machine, error)
}

// MachineRemover is optional for a Backend: one whose VM outlives the
// serve process implements it (Hyper-V's VM is a persistent object,
// registered with the host until it is removed, with a differencing
// disk and a directory the person cannot delete). destroy calls
// RemoveMachine after the VM is stopped and before the files go, with
// the config directory, whether or not this serve booted the VM; a
// failure is logged and the files go all the same. A backend whose VM
// is gone once it stops (Virtualization.framework's) does not implement
// it.
type MachineRemover interface {
	RemoveMachine(configDir string) error
}

// LeftoverStopper is optional for a Backend whose VM outlives the serve
// process: a VM an earlier serve left running (it crashed, or the person
// logged off) still has the data disk mounted. Up calls StopLeftover
// with the config directory before anything touches the disk (an
// interrupted upgrade's recovery swaps it) or boots; it shuts such a VM
// down cleanly, never by cutting its power, and is no error when there
// is no VM or it is off.
type LeftoverStopper interface {
	StopLeftover(configDir string, logf Logf) error
}

// ImageInfo describes a VM image.
type ImageInfo struct {
	Version string
	Arch    string // "arm64" or "amd64": docker --platform, release assets
}

// MachineConfig is one boot of the VM.
type MachineConfig struct {
	ImageDir   string // the backend reads what it needs here
	DataDisk   string
	FormatData bool // first boot: the guest formats the data disk
	CPUs       uint
	MemoryMB   uint64
	ConsoleLog io.Writer // the guest's boot and console output, line by line
}

// Machine is one running (or stopped) VM.
type Machine interface {
	// Start boots the VM.
	Start() error
	// Events carries the guest's marker lines (READY, FATAL, CLEAN),
	// from a serial console or from a boot command's output. It is
	// buffered and lossy: a marker nobody waits for is dropped once the
	// buffer is full.
	Events() <-chan ConsoleEvent
	// Done is closed once the VM has stopped (or failed).
	Done() <-chan struct{}
	// DialAgent connects to the guest agent, by whatever transport the
	// backend has: vsock, Hyper-V sockets, or a multiplexed child.
	DialAgent(ctx context.Context) (net.Conn, error)
	// RequestPoweroff asks the guest to stop cleanly when the agent
	// cannot be reached: the console verb, or a shutdown command.
	RequestPoweroff() error
	// HardStop powers the VM off at once.
	HardStop() error
}

// Host is what the supervisor needs from the operating system.
// host.Default() is the one of the OS the binary is built for.
type Host interface {
	DefaultConfigDir() (string, error)
	// Snapshot makes dst a copy of src: an APFS clone, a sparse-aware
	// copy on NTFS, a plain copy elsewhere. It fails if dst exists.
	Snapshot(src, dst string) error
	// SyncDir makes a rename in dir durable: F_FULLFSYNC on macOS,
	// fsync on Linux; on Windows only a check, since a directory handle
	// cannot be flushed (Rename writes through instead).
	SyncDir(dir string) error
	// Rename replaces newpath with oldpath durably: rename and SyncDir
	// on Unix, MoveFileEx with MOVEFILE_WRITE_THROUGH on Windows.
	Rename(oldpath, newpath string) error
	ExcludeFromBackup(path string) error // Time Machine; a no-op elsewhere
	FreeBytes(dir string) (uint64, error)
	// FileUsage is a file's allocated bytes and its logical size: st_blocks
	// on Unix, GetCompressedFileSizeW on Windows (the size, if that fails).
	FileUsage(path string) (used, size int64, err error)
	// Listen and Dial are the owner-only RPC endpoint: a Unix socket
	// with mode 0600, or a named pipe with an owner-only ACL.
	Listen(dir string) (net.Listener, error)
	Dial(ctx context.Context, dir string) (net.Conn, error)
	Endpoint(dir string) string // for messages and status
	// LockServe holds the one-serve-per-directory lock: flock, or
	// LockFileEx. Another holder yields host.ErrServeRunning.
	LockServe(dir string) (release func(), err error)
	// Detach prepares cmd to outlive its parent without a window.
	Detach(cmd *exec.Cmd)
	AddrInUse(err error) bool
}

var _ Host = host.Default()

// Marker lines printed by the guest (vm/README.md, "Console markers").
const (
	ReadyLine   = "KIVALI-VM READY"
	FatalPrefix = "KIVALI-VM FATAL"
	CleanLine   = "KIVALI-VM: shutdown: data disk unmounted cleanly"
)

// EventKind is a marker line's kind.
type EventKind int

// Marker kinds.
const (
	EventReady EventKind = iota + 1
	EventFatal
	EventClean
)

// ConsoleEvent is one marker line the guest printed.
type ConsoleEvent struct {
	Kind EventKind
	Line string
}

// Guest is what the supervisor needs from the guest agent;
// *guestapi.Client implements it.
type Guest interface {
	Status(ctx context.Context) (guestapi.Status, error)
	Exec(ctx context.Context, spec guestapi.ExecSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	WriteFile(ctx context.Context, rel string, mode os.FileMode, r io.Reader) error
	ImportImages(ctx context.Context, r io.Reader) (guestapi.ImportResult, error)
	Proxy(ctx context.Context, port int) (net.Conn, error)
	OpenExec(ctx context.Context, spec guestapi.ExecSpec) (net.Conn, error)
	OpenPTY(ctx context.Context, spec guestapi.ExecSpec) (net.Conn, error)
	Shutdown(ctx context.Context) error
}

// AgentGuest is the production Guest: the guest agent over the
// machine's agent connection.
func AgentGuest(m Machine) Guest {
	return &guestapi.Client{Dial: m.DialAgent}
}

// maxConsoleLine is the longest console line ScanConsole handles whole;
// a longer one is passed on in pieces of this size.
const maxConsoleLine = 1 << 20

// ScanConsole copies the guest's output to log, one line at a time
// (timestamped), and sends marker lines on events without blocking.
// It returns when r ends, and only then: a line longer than
// maxConsoleLine (binary noise on a serial port) is split, never the
// end of the scan. Returning early would stop draining the console, so
// the guest's console writes (its shutdown script's among them) would
// block, and on Hyper-V it would end the machine's Done while the VM
// still runs.
func ScanConsole(r io.Reader, log io.Writer, events chan<- ConsoleEvent) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxConsoleLine)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		if advance == 0 && token == nil && err == nil && len(data) >= maxConsoleLine {
			return len(data), data, nil
		}
		return advance, token, err
	})
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if log != nil {
			_, _ = fmt.Fprintf(log, "%s %s\n", time.Now().UTC().Format("15:04:05.000"), line)
		}
		var kind EventKind
		switch {
		case line == ReadyLine:
			kind = EventReady
		case strings.HasPrefix(line, FatalPrefix):
			kind = EventFatal
		case line == CleanLine:
			kind = EventClean
		default:
			continue
		}
		select {
		case events <- ConsoleEvent{Kind: kind, Line: line}:
		default:
		}
	}
}
