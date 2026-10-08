//go:build darwin || linux

package host

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// SocketName is the RPC socket's name in the config directory.
const SocketName = "supervisor.sock"

// Endpoint is the RPC socket's path.
func (OS) Endpoint(dir string) string { return filepath.Join(dir, SocketName) }

// Listen binds the owner-only RPC socket (mode 0600). A stale socket
// file (nothing answers on it) is replaced; a live one is an error,
// because one process must own the VM.
func (h OS) Listen(dir string) (net.Listener, error) {
	path := h.Endpoint(dir)
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		_ = c.Close()
		return nil, fmt.Errorf("a supervisor is already serving on %s", path)
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Dial connects to the RPC socket.
func (h OS) Dial(ctx context.Context, dir string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", h.Endpoint(dir))
}

// LockServe takes the exclusive flock on serve.lock in dir.
func (OS) LockServe(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrServeRunning, p)
		}
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	return func() { _ = f.Close() }, nil
}

// Detach starts cmd in its own session, so it outlives the terminal
// that started it.
func (OS) Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Rename renames oldpath to newpath, replacing it, and makes the rename
// durable with SyncDir on newpath's directory.
func (h OS) Rename(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); err != nil {
		return err
	}
	return h.SyncDir(filepath.Dir(newpath))
}

// FileUsage is p's allocated bytes (st_blocks, in 512-byte units on
// both macOS and Linux) and its logical size: a sparse data disk
// allocates far less than its size.
func (OS) FileUsage(p string) (used, size int64, err error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Blocks) * 512, st.Size, nil
}

// AddrInUse reports whether err is a bind to a port another program
// holds.
func (OS) AddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
