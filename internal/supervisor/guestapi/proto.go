// Package guestapi is the protocol between kivali-supervisor on the host
// and kivali-guest, the agent inside the Kivali Desktop VM. It holds the
// wire types, the stream framing, the client the supervisor uses and the
// server the guest runs. See docs/developers/supervisor.md for the protocol.
//
// The transport is the backend's, host-only by construction so that
// nothing in the pod network can reach the agent: virtio-vsock on macOS,
// a Hyper-V socket on Windows, the guest's vsock port 1024 either way.
// Each connection carries exactly one HTTP/1.1
// request. Plain requests (status, write-file, image import, shutdown)
// are ordinary request/response pairs; the streaming ones (exec, pty,
// proxy) answer 101 Switching Protocols and then carry either frames
// (exec, pty) or raw bytes (proxy).
package guestapi

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Port is the vsock port the guest agent listens on.
const Port = 1024

// UpgradeToken is the Upgrade header value of the streaming requests.
const UpgradeToken = "kivali-stream"

// Routes.
const (
	PathStatus   = "/v1/status"
	PathExec     = "/v1/exec"
	PathPTY      = "/v1/pty"
	PathFile     = "/v1/file"
	PathImport   = "/v1/images/import"
	PathProxy    = "/v1/proxy"
	PathShutdown = "/v1/shutdown"
)

// DataDir is where the guest mounts the data disk; write-file paths are
// relative to it.
const DataDir = "/var/lib/kivali"

// Guest states, as recorded by the boot scripts under /run/kivali.
const (
	StateBooting = "booting"
	StateReady   = "ready"
	StateFatal   = "fatal"
)

// Status is the answer to GET /v1/status.
type Status struct {
	// BootID is this boot's /proc/sys/kernel/random/boot_id.
	BootID string `json:"boot_id"`
	// State is booting, ready or fatal: ready once the `ready` script
	// printed KIVALI-VM READY for this boot, fatal once any boot script
	// printed a FATAL (Fatal then holds its reason).
	State string `json:"state"`
	Fatal string `json:"fatal,omitempty"`
	// NodeReady is the node's Ready condition, read now.
	NodeReady bool     `json:"node_ready"`
	Versions  Versions `json:"versions"`
	// BakedImages are the image references inside the root disk's
	// airgap tarballs (/usr/share/kivali/images), k3s's own included.
	BakedImages []string `json:"baked_images"`
	// BakedCharts are the chart tarballs baked into the root disk
	// (/usr/share/kivali/charts).
	BakedCharts []BakedChart `json:"baked_charts"`
	// DataDisk is the data disk's filesystem health, read now.
	DataDisk DataDiskHealth `json:"data_disk"`
	Time     time.Time      `json:"time"`
}

// Versions of the guest's parts.
type Versions struct {
	Agent   string `json:"agent"`
	VMImage string `json:"vm_image"`
	Kernel  string `json:"kernel"`
	K3s     string `json:"k3s"`
}

// BakedChart is one chart tarball on the root disk.
type BakedChart struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	AppVersion string `json:"app_version"`
}

// ExecSpec is the JSON in the `spec` query parameter of exec and pty.
type ExecSpec struct {
	Argv []string `json:"argv"`
	// Env is added to the agent's default environment (PATH with k3s's
	// binaries, HOME=/root).
	Env []string `json:"env,omitempty"`
	Dir string   `json:"dir,omitempty"`
	// Rows and Cols size the PTY (pty only).
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// ImportResult is the answer to POST /v1/images/import.
type ImportResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// Frame types of the exec and pty streams.
const (
	FrameStdin      byte = 0 // host -> guest: bytes for the process's stdin (or the PTY)
	FrameStdout     byte = 1 // guest -> host: stdout (or the PTY's output)
	FrameStderr     byte = 2 // guest -> host: stderr (exec only)
	FrameExit       byte = 3 // guest -> host: JSON Exit; the last frame
	FrameStdinClose byte = 4 // host -> guest: close stdin
	FrameResize     byte = 5 // host -> guest: JSON Resize (pty only)
)

// MaxFrame bounds one frame's payload; writers split larger buffers.
const MaxFrame = 1 << 20

// Exit is the payload of FrameExit. Code is -1 and Error set when the
// process could not be started or was killed.
type Exit struct {
	Code  int    `json:"code"`
	Error string `json:"error,omitempty"`
}

// Resize is the payload of FrameResize.
type Resize struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// WriteFrame writes one frame: a type byte, a big-endian uint32 length
// and the payload.
func WriteFrame(w io.Writer, typ byte, p []byte) error {
	if len(p) > MaxFrame {
		return fmt.Errorf("guestapi: frame of %d bytes exceeds %d", len(p), MaxFrame)
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(p) == 0 {
		return nil
	}
	_, err := w.Write(p)
	return err
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("guestapi: frame of %d bytes exceeds %d", n, MaxFrame)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// FrameWriter serialises frames from several goroutines onto one stream.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter wraps w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Frame writes one frame, splitting p into MaxFrame pieces.
func (f *FrameWriter) Frame(typ byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		n := len(p)
		if n > MaxFrame {
			n = MaxFrame
		}
		if err := WriteFrame(f.w, typ, p[:n]); err != nil {
			return err
		}
		p = p[n:]
		if len(p) == 0 {
			return nil
		}
	}
}

// JSON writes one frame whose payload is v as JSON.
func (f *FrameWriter) JSON(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.Frame(typ, b)
}

// Writer returns an io.Writer that sends everything written as frames
// of type typ.
func (f *FrameWriter) Writer(typ byte) io.Writer { return frameTypeWriter{f, typ} }

type frameTypeWriter struct {
	f   *FrameWriter
	typ byte
}

func (w frameTypeWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.f.Frame(w.typ, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
