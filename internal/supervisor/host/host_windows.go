package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// DefaultConfigDir is %LOCALAPPDATA%\Kivali: local, never roaming, since
// the data disk is tens of gigabytes.
func (OS) DefaultConfigDir() (string, error) {
	d, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || d == "" {
		if d = os.Getenv("LOCALAPPDATA"); d == "" {
			return "", fmt.Errorf("no local application data folder: %v", err)
		}
	}
	return filepath.Join(d, "Kivali"), nil
}

// Snapshot copies src to dst and flushes it (FlushFileBuffers); it fails
// if dst exists. A sparse src stays sparse: only its allocated ranges
// are copied into a sparse dst. A src that is not sparse is copied
// whole and dst is not made sparse, because Hyper-V refuses to attach a
// sparse virtual disk on older Windows builds.
func (OS) Snapshot(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	ranges := []allocatedRange{{0, size}}
	if isSparse(src) {
		if err := setSparse(out); err == nil {
			if r, err := allocatedRanges(in, size); err == nil {
				ranges = r
			}
		}
	}
	if err := out.Truncate(size); err != nil {
		return fail(err)
	}
	for _, r := range ranges {
		if _, err := io.Copy(io.NewOffsetWriter(out, r.Offset), io.NewSectionReader(in, r.Offset, r.Length)); err != nil {
			return fail(err)
		}
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	return out.Close()
}

// allocatedRange is FILE_ALLOCATED_RANGE_BUFFER.
type allocatedRange struct {
	Offset, Length int64
}

func isSparse(p string) bool {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(name)
	return err == nil && attrs&windows.FILE_ATTRIBUTE_SPARSE_FILE != 0
}

func setSparse(f *os.File) error {
	var n uint32
	return windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil)
}

// allocatedRanges lists the ranges of f that hold data.
func allocatedRanges(f *os.File, size int64) ([]allocatedRange, error) {
	var out []allocatedRange
	buf := make([]allocatedRange, 64)
	q := allocatedRange{0, size}
	for q.Length > 0 {
		var n uint32
		err := windows.DeviceIoControl(windows.Handle(f.Fd()), windows.FSCTL_QUERY_ALLOCATED_RANGES,
			(*byte)(unsafe.Pointer(&q)), uint32(unsafe.Sizeof(q)),
			(*byte)(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), &n, nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, err
		}
		got := buf[:n/uint32(unsafe.Sizeof(buf[0]))]
		out = append(out, got...)
		if err == nil || len(got) == 0 {
			break
		}
		last := got[len(got)-1]
		next := last.Offset + last.Length
		q = allocatedRange{next, size - next}
	}
	return out, nil
}

// SyncDir only checks that dir is a directory. Windows cannot flush a
// directory handle; NTFS journals a rename's metadata itself, and the
// one file the supervisor needs on disk before a rename, the snapshot,
// is flushed by Snapshot.
func (OS) SyncDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

var procGetCompressedFileSizeW = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// FileUsage is p's allocated bytes (GetCompressedFileSizeW, which
// counts what a sparse or compressed file really occupies) and its
// logical size; if the call fails, the size stands for both.
func (OS) FileUsage(p string) (used, size int64, err error) {
	fi, err := os.Stat(p)
	if err != nil {
		return 0, 0, err
	}
	size = fi.Size()
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return size, size, nil
	}
	var high uint32
	low, _, e := procGetCompressedFileSizeW.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF { // INVALID_FILE_SIZE, or a real low word: the error tells
		if errno, ok := e.(syscall.Errno); ok && errno != 0 {
			return size, size, nil
		}
	}
	return int64(high)<<32 | int64(uint32(low)), size, nil
}

// ExcludeFromBackup is a no-op: File History and Windows Backup have no
// per-item exclusion attribute.
func (OS) ExcludeFromBackup(string) error { return nil }

// FreeBytes is the space on dir's volume available to this user.
func (OS) FreeBytes(dir string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(name, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

// Endpoint is the RPC named pipe of dir (PipeName of its absolute path).
func (OS) Endpoint(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return PipeName(dir)
}

// ownerOnly is the pipe's security descriptor: protected (no inherited
// ACEs), with one ACE granting the current user full access, so no other
// account can open it. The owner keeps WRITE_DAC (it could loosen the
// DACL itself), and an administrator can take ownership and then do the
// same: the pipe is owner-only against other ordinary accounts, not
// against an administrator.
func ownerOnly() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return "D:P(A;;GA;;;" + u.User.Sid.String() + ")", nil
}

// errForeignPipe is the answer when the pipe's server runs as another
// account: a squatter that created the name first.
var errForeignPipe = errors.New("the supervisor pipe is held by another account")

// verifyServer checks that the server end of a dialled pipe runs as the
// current user. Anyone can create a pipe name first, with any DACL, so
// a successful dial proves nothing about who answers.
func verifyServer(c net.Conn) error {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("pipe connection has no handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return fmt.Errorf("pipe server process: %w", err)
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// A process of another account is usually not even openable.
		return fmt.Errorf("%w (server pid %d: %v)", errForeignPipe, pid, err)
	}
	defer func() { _ = windows.CloseHandle(p) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("%w (server pid %d: %v)", errForeignPipe, pid, err)
	}
	defer func() { _ = tok.Close() }()
	theirs, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("pipe server's user: %w", err)
	}
	mine, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("current user: %w", err)
	}
	if !windows.EqualSid(theirs.User.Sid, mine.User.Sid) {
		return fmt.Errorf("%w (server pid %d runs as %s)", errForeignPipe, pid, theirs.User.Sid)
	}
	return nil
}

// Listen creates the owner-only RPC pipe. A pipe of this user that
// already answers is an error, because one process must own the VM; a
// pipe has no stale file to replace, it goes when its server does. A
// pipe another account answers on is not ours: Listen goes on to create
// its own first instance, which Windows refuses while the squatter
// holds the name, and that refusal is reported as such.
func (h OS) Listen(dir string) (net.Listener, error) {
	name := h.Endpoint(dir)
	timeout := time.Second
	foreign := false
	if c, err := winio.DialPipe(name, &timeout); err == nil {
		verr := verifyServer(c)
		_ = c.Close()
		if verr == nil {
			return nil, fmt.Errorf("a supervisor is already serving on %s", name)
		}
		foreign = true
	}
	sd, err := ownerOnly()
	if err != nil {
		return nil, fmt.Errorf("pipe security: %w", err)
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: sd})
	if err != nil && (foreign || errors.Is(err, windows.ERROR_ACCESS_DENIED)) {
		return nil, fmt.Errorf("%w: %s exists and is not this user's, so serve cannot create it (%v); "+
			"find and stop the process holding it, or use another --config-dir", errForeignPipe, name, err)
	}
	return ln, err
}

// Dial connects to the RPC pipe and checks it is served by this user.
func (h OS) Dial(ctx context.Context, dir string) (net.Conn, error) {
	c, err := winio.DialPipeContext(ctx, h.Endpoint(dir))
	if err != nil {
		return nil, err
	}
	if err := verifyServer(c); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("%s: %w", h.Endpoint(dir), err)
	}
	return c, nil
}

// Rename renames oldpath to newpath, replacing it, with
// MOVEFILE_WRITE_THROUGH: MoveFileEx returns only once the rename is on
// the disk.
func (OS) Rename(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}

// LockServe takes an exclusive LockFileEx lock on serve.lock in dir;
// Windows drops it when the handle closes or the process ends.
func (OS) LockServe(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, lockName)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("%w (%s)", ErrServeRunning, p)
		}
		return nil, fmt.Errorf("lock %s: %w", p, err)
	}
	return func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
		_ = f.Close()
	}, nil
}

// Detach starts cmd with no console and no window, in its own process
// group, so it outlives the console that started it.
func (OS) Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// AddrInUse reports whether err is a bind to a port another program
// holds.
func (OS) AddrInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }
