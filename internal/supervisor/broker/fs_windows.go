//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/kivali-ai/kivali/internal/supervisor/vhdx"
)

// The file rights the broker opens paths with. FILE_ADD_FILE is the
// right to create a file in a directory; it is the same bit as
// FILE_WRITE_DATA on a file.
const fileAddFile = 0x0002

var (
	advapi32                 = windows.NewLazySystemDLL("advapi32.dll")
	procImpersonateNamedPipe = advapi32.NewProc("ImpersonateNamedPipeClient")
	errNotRegularOneLink     = errors.New("not a regular file with a single hard link")
	errIsReparse             = errors.New("a reparse point (a link or junction)")
	errNotDir                = errors.New("not a directory")
	errReparseOnPath         = errors.New("a reparse point lies along the path")
)

// pipeHandleOf is the server-side handle of a pipe connection, which
// winio exposes through Fd (as the RPC owner check does).
func pipeHandleOf(c net.Conn) (windows.Handle, error) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return 0, errors.New("the broker pipe connection has no handle")
	}
	return windows.Handle(f.Fd()), nil
}

// impersonateNamedPipeClient makes the current thread run as the pipe's
// client until RevertToSelf.
func impersonateNamedPipeClient(pipe windows.Handle) error {
	r, _, e := procImpersonateNamedPipe.Call(uintptr(pipe))
	if r == 0 {
		return e
	}
	return nil
}

// winCaller is a request's caller: the client of its pipe connection.
// Every file check re-impersonates that client on a locked OS thread,
// opens the path with the client's token, and holds a handle open until
// Close, reading, so what was checked cannot be swapped (File). The SID
// is read once from the impersonation token.
type winCaller struct {
	pipe    windows.Handle
	sid     string
	handles []windows.Handle
	// pinned is File's reading handle on each file it pinned, by the
	// canonical path in lower case, for Contents.
	pinned map[string]windows.Handle
}

// callerOf impersonates the pipe client to read its SID, then reverts;
// the SID identifies the caller for the ownership rule and the logs.
func callerOf(r *http.Request) (Caller, error) {
	conn, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok {
		return nil, errors.New("the request carries no pipe connection")
	}
	pipe, err := pipeHandleOf(conn)
	if err != nil {
		return nil, err
	}
	c := &winCaller{pipe: pipe}
	err = onCallerThread(pipe, func() error {
		tok := windows.GetCurrentThreadToken()
		u, err := tok.GetTokenUser()
		if err != nil {
			return fmt.Errorf("read the caller's token: %w", err)
		}
		c.sid = u.User.Sid.String()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// onCallerThread runs fn impersonating the pipe's client, on an OS
// thread locked for the duration. If the token cannot be reset, the
// thread stays locked so the Go runtime discards it rather than run
// another goroutine on it still impersonating.
func onCallerThread(pipe windows.Handle, fn func() error) error {
	runtime.LockOSThread()
	unlock := false
	defer func() {
		if unlock {
			runtime.UnlockOSThread()
		}
	}()
	if err := impersonateNamedPipeClient(pipe); err != nil {
		return fmt.Errorf("impersonate the caller: %w", err)
	}
	err := fn()
	if rerr := windows.RevertToSelf(); rerr != nil {
		return fmt.Errorf("revert impersonation: %w", rerr)
	}
	unlock = true
	return err
}

func (c *winCaller) SID() string { return c.sid }

func (c *winCaller) keep(h windows.Handle) { c.handles = append(c.handles, h) }

func (c *winCaller) Close() error {
	for _, h := range c.handles {
		_ = windows.CloseHandle(h)
	}
	c.handles = nil
	c.pinned = nil
	return nil
}

// pin keeps h, a handle that reads the file at canon, until Close, and
// records it for Contents.
func (c *winCaller) pin(h windows.Handle, canon string) {
	c.keep(h)
	if c.pinned == nil {
		c.pinned = map[string]windows.Handle{}
	}
	c.pinned[strings.ToLower(canon)] = h
}

// Contents reads the file File pinned at p through the pinning handle.
// The handle's access was checked as the caller when it was opened;
// reading through it opens nothing.
func (c *winCaller) Contents(p string) (io.ReaderAt, int64, error) {
	h, ok := c.pinned[strings.ToLower(p)]
	if !ok {
		return nil, 0, fmt.Errorf("%s is not pinned for reading", p)
	}
	size, err := handleSize(h)
	if err != nil {
		return nil, 0, fmt.Errorf("stat %s: %w", p, err)
	}
	return handleReader{h}, size, nil
}

// handleSize is the size of the file open as h.
func handleSize(h windows.Handle) (int64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, err
	}
	return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), nil
}

// handleReader reads a file at an offset through a handle opened for
// synchronous I/O (the offset goes in the OVERLAPPED; the handle's own
// position is not used). CopyFile asks it for the file's allocated
// ranges (ranges).
type handleReader struct{ h windows.Handle }

// allocatedRange is FILE_ALLOCATED_RANGE_BUFFER.
type allocatedRange struct {
	Offset, Length int64
}

// ranges is what CopyFile copies of the file's first size bytes: its
// allocated ranges, and true, when the file is sparse; the whole of it,
// and false, otherwise (or when the ranges cannot be read).
func (r handleReader) ranges(size int64) ([]allocatedRange, bool) {
	whole := []allocatedRange{{0, size}}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(r.h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_SPARSE_FILE == 0 {
		return whole, false
	}
	var out []allocatedRange
	buf := make([]allocatedRange, 64)
	q := allocatedRange{0, size}
	for q.Length > 0 {
		var n uint32
		err := windows.DeviceIoControl(r.h, windows.FSCTL_QUERY_ALLOCATED_RANGES,
			(*byte)(unsafe.Pointer(&q)), uint32(unsafe.Sizeof(q)),
			(*byte)(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), &n, nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return whole, false
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
	return out, true
}

func (r handleReader) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d", off)
	}
	n := 0
	for n < len(b) {
		pos := off + int64(n)
		o := windows.Overlapped{Offset: uint32(pos), OffsetHigh: uint32(pos >> 32)}
		chunk := b[n:]
		if len(chunk) > 1<<30 {
			chunk = chunk[:1<<30]
		}
		var done uint32
		err := windows.ReadFile(r.h, chunk, &done, &o)
		if errors.Is(err, windows.ERROR_HANDLE_EOF) || (err == nil && done == 0) {
			return n, io.EOF
		}
		if err != nil {
			return n, err
		}
		n += int(done)
	}
	return n, nil
}

// WritableDir opens dir as the caller with the right to add a file to
// it (FILE_ADD_FILE), which is a pure access check: no probe file is
// created, so there is no side effect and no race, and the broker is
// told exactly what it needs to know. It then canonicalises the handle
// and refuses any reparse point along the path.
func (c *winCaller) WritableDir(dir string) (string, error) {
	h, canon, err := c.open(dir, fileAddFile|windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE, true)
	if err != nil {
		return "", err
	}
	c.keep(h)
	return canon, nil
}

// File opens an existing regular file (one hard link, not a directory)
// as the caller, for reading, and canonicalises it (Caller.File). The
// handle kept until Close pins the file against a rename, removal or
// replacement while the request runs: it is opened without delete
// sharing and reads the file's data, since only data access and delete
// take part in sharing (an attributes-only handle pins nothing).
func (c *winCaller) File(p string) (string, error) {
	h, canon, err := c.open(p, windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES, false)
	if err != nil {
		return "", err
	}
	c.pin(h, canon)
	return canon, nil
}

// open opens p as the caller with FILE_FLAG_OPEN_REPARSE_POINT (so a
// final reparse point is opened as the link, not followed), checks the
// handle is of the expected kind and no reparse point, and returns its
// canonical path, refusing it if resolving the path crossed any reparse
// point (the canonical path then differs from the clean input).
func (c *winCaller) open(p string, access uint32, dir bool) (windows.Handle, string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, "", err
	}
	var (
		h     windows.Handle
		canon string
	)
	err = onCallerThread(c.pipe, func() error {
		// Share read and write so a running VM's own open does not clash,
		// but never delete: while the broker holds a handle that reads
		// the file, it cannot be renamed or removed, which is what pins
		// the check (a handle with no data access pins nothing).
		var e error
		h, e = windows.CreateFile(name, access,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		return e
	})
	if err != nil {
		return 0, "", err
	}
	fail := func(err error) (windows.Handle, string, error) {
		_ = windows.CloseHandle(h)
		return 0, "", err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fail(fmt.Errorf("stat %s: %w", p, err))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fail(errIsReparse)
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	switch {
	case dir && !isDir:
		return fail(errNotDir)
	case !dir && isDir:
		return fail(errNotRegularOneLink)
	case !dir && info.NumberOfLinks != 1:
		return fail(errNotRegularOneLink)
	}
	canon, err = finalPath(h)
	if err != nil {
		return fail(fmt.Errorf("canonicalise %s: %w", p, err))
	}
	// The input was already a clean absolute local path; if the real path
	// differs by anything but case, a reparse point was crossed.
	if !strings.EqualFold(canon, filepath.Clean(p)) {
		return fail(fmt.Errorf("%w (%s resolves to %s)", errReparseOnPath, p, canon))
	}
	return h, canon, nil
}

// finalPath is the handle's normalised DOS path without the \\?\
// prefix (flags 0: FILE_NAME_NORMALIZED and VOLUME_NAME_DOS).
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return "", err
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
	}
	return stripNTPrefix(windows.UTF16ToString(buf[:n])), nil
}

// stripNTPrefix removes the \\?\ long-path prefix GetFinalPathNameByHandle
// returns (never a UNC path here: the broker refuses those before open).
func stripNTPrefix(p string) string {
	return strings.TrimPrefix(p, `\\?\`)
}

// winFiles does the broker's own file-system work (as the service's
// account, or an elevated console broker): making, canonicalising and
// removing the per-VM directory, and making, copying, compacting,
// renaming and deleting the disks in it. Root, when set, is where the
// VM directories go instead of the broker's own tree (VMRoot).
type winFiles struct{ Root string }

// errNotBrokerDir is the refusal of a VM directory the broker did not
// make (brokerOwned): the caller created it first, so the caller can
// write in it, and the broker neither creates nor deletes anything
// there as itself.
var errNotBrokerDir = errors.New("not made by the broker (its owner is neither the broker service's account, LocalSystem nor Administrators); remove it and it is made again")

// errOtherAccount is the refusal of a VM directory made for another
// account: it holds that account's team's data disk.
var errOtherAccount = errors.New("made for another account")

// VMRoot is the root of the broker's VM directories (VMRoot of
// winFiles{}), for the supervisor, which reads the data disk there.
func VMRoot() (string, error) { return winFiles{}.VMRoot() }

// VMRoot is where the broker keeps every VM directory: a directory of
// its own under its log directory (ProgramData\Kivali\broker\vm), made
// as the log directories are (logDirSDDL), so the service, which is no
// administrator and cannot create anything in a person's folders, owns
// the whole tree. Root overrides it (the tests).
func (f winFiles) VMRoot() (string, error) {
	if f.Root != "" {
		return f.Root, nil
	}
	dir, err := LogDir()
	if err != nil {
		return "", err
	}
	return winJoin(dir, VMDirName), nil
}

// VMDirPath is the VM directory of a config directory key (Files.VMDirPath).
func (f winFiles) VMDirPath(key string) string {
	root, err := f.VMRoot()
	if err != nil {
		return ""
	}
	return winJoin(root, key)
}

// VMDir makes the VM directory of key if absent (Files.VMDir): the
// root first (makeLogDir's directories and the root, each the broker's
// own or refused), then <root>\<key> owned by the broker (ownerSID),
// with a protected DACL that grants SYSTEM, Administrators and the
// service's account full control and the caller read, opened without
// delete sharing (so it cannot be swapped under the request), and
// checked to be a real directory, no reparse point, and the broker's
// own (brokerOwned). The caller cannot write in it, so nothing there is
// a link the caller planted: the broker creates files in it as itself.
// A directory that exists already is bound to the account it was made
// for: its DACL's account (DirAccount, read through the handle) must
// be the caller, or it is refused (errOtherAccount) before anything is
// written to it.
//
// The owner is named rather than left to the token's default, so it is
// the service's account as the service and the Administrators group
// elevated with --console; an ordinary account's token may name
// neither.
func (f winFiles) VMDir(key, callerSID string) (string, func(), error) {
	if err := ValidSID(callerSID); err != nil {
		return "", nil, err
	}
	if !keyRE.MatchString(key) {
		return "", nil, fmt.Errorf("%q is not a config directory key", key)
	}
	root, err := f.VMRoot()
	if err != nil {
		return "", nil, err
	}
	if err := f.ensureBrokerDirs(root); err != nil {
		return "", nil, err
	}
	owner, err := ownerSID()
	if err != nil {
		return "", nil, err
	}
	dir := winJoin(root, key)
	sddl := "O:" + owner + "D:P(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)(A;OICI;GA;;;" + ServiceSID + ")(A;OICI;GR;;;" + callerSID + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return "", nil, fmt.Errorf("the VM directory's security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", nil, err
	}
	if err := windows.CreateDirectory(name, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return "", nil, fmt.Errorf("create %s: %w", dir, err)
	}
	h, canon, err := openDir(dir, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.WRITE_DAC)
	if err != nil {
		return "", nil, err
	}
	if err := checkBrokerOwned(h, dir); err != nil {
		_ = windows.CloseHandle(h)
		return "", nil, err
	}
	if err := checkMadeFor(h, dir, callerSID); err != nil {
		_ = windows.CloseHandle(h)
		return "", nil, err
	}
	// The DACL again, through the handle: a directory an earlier broker
	// made (elevated, before the service had an account of its own)
	// carries no entry for the service, which could then neither grant
	// files in it nor delete it.
	dacl, _, err := sd.DACL()
	if err == nil {
		err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	if err != nil {
		_ = windows.CloseHandle(h)
		return "", nil, fmt.Errorf("the VM directory's DACL: %w", err)
	}
	return canon, func() { _ = windows.CloseHandle(h) }, nil
}

// ensureBrokerDirs makes the broker's directories down to root, each as
// logDirSDDL says and each the broker's own (brokerDir): the log
// directory and its parent for the broker's own tree, root alone when
// it was overridden.
func (f winFiles) ensureBrokerDirs(root string) error {
	dirs := []string{root}
	if f.Root == "" {
		dirs = []string{winDir(winDir(root)), winDir(root), root}
	}
	for _, d := range dirs {
		if err := brokerDir(d); err != nil {
			return err
		}
	}
	return nil
}

// openDir opens dir without following a link at it and without delete
// sharing (so it cannot be renamed, removed or replaced while the
// handle is open, nor can anything above it be renamed), checks it is a
// directory and no reparse point, and returns the handle (with
// READ_CONTROL added to access, for the owner check) and its canonical
// path.
func openDir(dir string, access uint32) (windows.Handle, string, error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, "", err
	}
	h, err := windows.CreateFile(name, access|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, "", fmt.Errorf("open %s: %w", dir, err)
	}
	fail := func(err error) (windows.Handle, string, error) {
		_ = windows.CloseHandle(h)
		return 0, "", err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fail(fmt.Errorf("stat %s: %w", dir, err))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fail(fmt.Errorf("%s is %w", dir, errIsReparse))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fail(fmt.Errorf("%s is %w", dir, errNotDir))
	}
	canon, err := finalPath(h)
	if err != nil {
		return fail(fmt.Errorf("canonicalise %s: %w", dir, err))
	}
	return h, canon, nil
}

// checkBrokerOwned refuses the directory open as h unless its owner is
// one brokerOwned accepts.
func checkBrokerOwned(h windows.Handle, dir string) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the owner of %s: %w", dir, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read the owner of %s: %w", dir, err)
	}
	if owner == nil || !brokerOwned(owner.String()) {
		return fmt.Errorf("%s is %w", dir, errNotBrokerDir)
	}
	return nil
}

// dirAccountOf is the account the directory open as h (with
// READ_CONTROL) was made for: DirAccount of the SIDs its DACL's entries
// name, each read from the entry itself, so its numeric form is
// compared whatever alias SDDL would print for it.
func dirAccountOf(h windows.Handle, dir string) (string, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("read the DACL of %s: %w", dir, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return "", fmt.Errorf("read the DACL of %s: %w", dir, err)
	}
	sids, err := aceSIDs(dacl)
	if err != nil {
		return "", fmt.Errorf("read the DACL of %s: %w", dir, err)
	}
	account, err := DirAccount(sids)
	if err != nil {
		return "", fmt.Errorf("%s: %w", dir, err)
	}
	return account, nil
}

// aceSIDs lists the SIDs acl's allow entries grant to: every entry the
// broker writes on a VM directory is one, and so is the entry Hyper-V
// adds for a virtual machine. Its SID follows its mask, as
// ACCESS_ALLOWED_ACE lays it out.
func aceSIDs(acl *windows.ACL) ([]string, error) {
	if acl == nil {
		return nil, nil
	}
	var sids []string
	for i := uint16(0); i < acl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(i), &ace); err != nil {
			return nil, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sids = append(sids, (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String())
	}
	return sids, nil
}

// checkMadeFor refuses the VM directory open as h when it was made for
// an account other than callerSID (errOtherAccount). A directory whose
// DACL names no account is the caller's to take: VMDir writes the
// caller's entry into it next.
func checkMadeFor(h windows.Handle, dir, callerSID string) error {
	account, err := dirAccountOf(h, dir)
	if err != nil {
		return err
	}
	if account != "" && !strings.EqualFold(account, callerSID) {
		return fmt.Errorf("%s is %w", dir, errOtherAccount)
	}
	return nil
}

// openPlainFile opens p without following a link at it, with access
// and without delete sharing, and checks it is a plain file: no reparse
// point, no directory, one link (a hard link to some other file has
// more).
func openPlainFile(p string, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(name, access|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", p, err)
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &info)
	switch {
	case err != nil:
		err = fmt.Errorf("stat %s: %w", p, err)
	case info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		err = fmt.Errorf("%s is %w", p, errIsReparse)
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.NumberOfLinks != 1:
		err = fmt.Errorf("%s is %w", p, errNotRegularOneLink)
	}
	if err != nil {
		_ = windows.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

// fileStandardInfo is FILE_STANDARD_INFO.
type fileStandardInfo struct {
	AllocationSize, EndOfFile int64
	NumberOfLinks             uint32
	DeletePending, Directory  bool
}

// Stat reports the plain file at p (Files.Stat), opened without
// following a link: its size and what the volume allocates to it. A
// missing file is an error wrapping fs.ErrNotExist (the Errno does).
func (winFiles) Stat(p string) (FileInfo, error) {
	h, err := openPlainFile(p, 0)
	if err != nil {
		return FileInfo{}, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var si fileStandardInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&si)), uint32(unsafe.Sizeof(si))); err != nil {
		return FileInfo{}, fmt.Errorf("stat %s: %w", p, err)
	}
	return FileInfo{Size: si.EndOfFile, Used: si.AllocationSize}, nil
}

// Open opens the plain file at p for reading (Files.Open), without
// following a link and without delete sharing, until release.
func (winFiles) Open(p string) (io.ReaderAt, int64, func(), error) {
	h, err := openPlainFile(p, windows.FILE_READ_DATA)
	if err != nil {
		return nil, 0, nil, err
	}
	size, err := handleSize(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, 0, nil, fmt.Errorf("stat %s: %w", p, err)
	}
	return handleReader{h}, size, func() { _ = windows.CloseHandle(h) }, nil
}

// copyChunk is how much CopyFile reads and writes at a time, and so how
// often it reports and checks for cancellation.
const copyChunk = 4 << 20

// CopyFile copies size bytes of src into dst (Files.CopyFile), a file
// it creates (CREATE_NEW: anything at dst, a dangling link among them,
// fails it), as itself, so the copy inherits the VM directory's DACL:
// the caller reads it and only the broker writes it. When src is a
// handleReader on a sparse file, dst is made sparse and only src's
// allocated ranges are copied; otherwise all of it is, and dst is not
// made sparse, since Hyper-V refuses to attach a sparse virtual disk on
// older Windows builds. dst is flushed (FlushFileBuffers) before the
// answer; a copy that fails, or whose ctx ends, deletes it through its
// own handle.
func (winFiles) CopyFile(ctx context.Context, src io.ReaderAt, size int64, dst string, m *Meter) (err error) {
	name, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE, 0, nil,
		windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	out := os.NewFile(uintptr(h), dst)
	defer func() {
		if err != nil {
			_ = deleteByHandle(h)
		}
		_ = out.Close()
	}()
	ranges := []allocatedRange{{0, size}}
	if hr, ok := src.(handleReader); ok {
		if r, sparse := hr.ranges(size); sparse {
			var n uint32
			if err := windows.DeviceIoControl(h, windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &n, nil); err == nil {
				ranges = r
			}
		}
	}
	if err := out.Truncate(size); err != nil {
		return fmt.Errorf("size %s: %w", dst, err)
	}
	var total, done int64
	for _, r := range ranges {
		total += r.Length
	}
	m.Set(0, total)
	buf := make([]byte, copyChunk)
	for _, r := range ranges {
		for off := r.Offset; off < r.Offset+r.Length; {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("the copy was stopped: %w", err)
			}
			n := int(min(int64(len(buf)), r.Offset+r.Length-off))
			got, err := src.ReadAt(buf[:n], off)
			if got < n {
				if err == nil || errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return fmt.Errorf("read at %d: %w", off, err)
			}
			if _, err := out.WriteAt(buf[:n], off); err != nil {
				return fmt.Errorf("write %s: %w", dst, err)
			}
			off += int64(n)
			done += int64(n)
			m.Set(done, total)
		}
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("flush %s: %w", dst, err)
	}
	return nil
}

// CheckDynamicVHDX refuses p unless it is a dynamic VHDX with no parent
// (Files.CheckDynamicVHDX): its metadata read through a handle opened
// without following a link (dynamicNoParent), then the virtual disk
// opened as itself without opening any parent its locator names, and
// not for its metadata only, so that the VHDX's log is replayed as any
// open of it would, and its subtype read through that handle
// (checkDynamicVHDX).
func (winFiles) CheckDynamicVHDX(p string) error {
	h, err := openPlainFile(p, windows.FILE_READ_DATA)
	if err != nil {
		return err
	}
	size, err := handleSize(h)
	if err == nil {
		err = dynamicNoParent(handleReader{h}, size)
	}
	_ = windows.CloseHandle(h)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	vd, err := openVHDXNoParents(p, false)
	if err != nil {
		return fmt.Errorf("open %s as a VHDX: %w", p, err)
	}
	defer func() { _ = windows.CloseHandle(vd) }()
	if err := checkDynamicVHDX(vd); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

// CompactVHDX compacts the VHDX at p (Files.CompactVHDX), the broker's
// own file: the virtual disk is opened as itself without parents
// (OPEN_VIRTUAL_DISK_FLAG_NO_PARENTS), its kind is checked through that
// handle, and that handle compacts it while m reads its progress. The
// sizes are read before the open and after the virtual disk is closed.
func (f winFiles) CompactVHDX(ctx context.Context, p string, m *Meter) (before, after int64, err error) {
	fi, err := f.Stat(p)
	if err != nil {
		return 0, 0, err
	}
	if err := compact(ctx, p, m); err != nil {
		return 0, 0, err
	}
	if fi2, err := f.Stat(p); err == nil {
		return fi.Size, fi2.Size, nil
	}
	return fi.Size, fi.Size, nil
}

func compact(ctx context.Context, p string, m *Meter) error {
	vd, err := openVHDXNoParents(p, false)
	if err != nil {
		return fmt.Errorf("open %s as a VHDX: %w", p, err)
	}
	defer func() { _ = windows.CloseHandle(vd) }()
	if err := checkDynamicVHDX(vd); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if err := compactVirtualDisk(ctx, vd, m); err != nil {
		return fmt.Errorf("compact %s: %w", p, err)
	}
	return nil
}

// Rename moves src to dst as itself (Files.Rename): MoveFileEx with
// MOVEFILE_WRITE_THROUGH, so the move is on the disk before the answer,
// and MOVEFILE_REPLACE_EXISTING only with replace. A rename never
// follows a link at dst; without replace, anything there fails it
// (ErrExists).
func (winFiles) Rename(src, dst string, replace bool) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	err = windows.MoveFileEx(from, to, flags)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return ErrExists
	}
	return err
}

// DeleteFile deletes p, a file in a VM directory, through a handle
// opened on it without following a link (Files.DeleteFile): a link left
// there is deleted as the link. Nothing at p is no error.
func (winFiles) DeleteFile(p string) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := deleteByHandle(h); err != nil {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	return nil
}

// RemoveVMDir deletes the VM directory dir and everything in it
// (Files.RemoveVMDir): opened as itself, it must be a directory, no
// reparse point, canonically dir and the broker's own, and it is then
// emptied and deleted through handles (removeTree).
func (winFiles) RemoveVMDir(dir, callerSID string) error {
	h, canon, err := openDir(dir, windows.DELETE|windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if !strings.EqualFold(canon, dir) {
		return fmt.Errorf("%w (%s resolves to %s)", errReparseOnPath, dir, canon)
	}
	if err := checkBrokerOwned(h, dir); err != nil {
		return err
	}
	if callerSID != "" {
		// The directory must be the caller's own team's: VMDir wrote the
		// caller's read entry into it when it made it.
		account, err := dirAccountOf(h, dir)
		if err != nil {
			return err
		}
		if !strings.EqualFold(account, callerSID) {
			return fmt.Errorf("%s was not made for this account", dir)
		}
	}
	return removeTree(dir, h)
}

// maxTreeDepth bounds how deep removeTree descends: Hyper-V writes two
// levels (Virtual Machines\<id>), and a deeper tree is no VM's.
const maxTreeDepth = 16

// removeTree deletes everything in the directory dir, open as h, and
// then dir itself through h. Each entry is opened without following a
// link and without delete sharing, checked to be canonically dir\name,
// and deleted through that handle: a link is deleted as the link and
// never followed, and a directory is emptied the same way first, its
// handle held meanwhile so it cannot be swapped. Nothing is deleted by a
// path resolved again after its check.
func removeTree(dir string, h windows.Handle) error {
	if err := removeEntries(dir, 0); err != nil {
		return err
	}
	if err := deleteByHandle(h); err != nil {
		return fmt.Errorf("delete %s: %w", dir, err)
	}
	return nil
}

func removeEntries(dir string, depth int) error {
	if depth >= maxTreeDepth {
		return fmt.Errorf("%s is nested more than %d directories deep", dir, maxTreeDepth)
	}
	names, err := dirNames(dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := removeEntry(winJoin(dir, n), depth); err != nil {
			return err
		}
	}
	return nil
}

func removeEntry(p string, depth int) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", p, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("stat %s: %w", p, err)
	}
	canon, err := finalPath(h)
	if err != nil {
		return fmt.Errorf("canonicalise %s: %w", p, err)
	}
	if !strings.EqualFold(canon, p) {
		return fmt.Errorf("%w (%s resolves to %s)", errReparseOnPath, p, canon)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		if err := removeEntries(p, depth+1); err != nil {
			return err
		}
	}
	if err := deleteByHandle(h); err != nil {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	return nil
}

// dirNames lists dir's entries, without . and .., before any is
// deleted.
func dirNames(dir string) ([]string, error) {
	pattern, err := windows.UTF16PtrFromString(dir + `\*`)
	if err != nil {
		return nil, err
	}
	var fd windows.Win32finddata
	fh, err := windows.FindFirstFile(pattern, &fd)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	defer func() { _ = windows.FindClose(fh) }()
	var names []string
	for {
		if n := windows.UTF16ToString(fd.FileName[:]); n != "." && n != ".." {
			names = append(names, n)
		}
		if err := windows.FindNextFile(fh, &fd); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return names, nil
			}
			return nil, fmt.Errorf("list %s: %w", dir, err)
		}
	}
}

// deleteByHandle deletes what h is open on (with DELETE access, as
// itself: a link is the link). The name goes at once (POSIX semantics,
// so an emptied directory can follow at once), and a read-only
// attribute does not stop it. Where that information class is not
// supported, the plain disposition, which deletes at the last close.
func deleteByHandle(h windows.Handle) error {
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
	if err == nil || (!errors.Is(err, windows.ERROR_INVALID_PARAMETER) && !errors.Is(err, windows.ERROR_NOT_SUPPORTED) && !errors.Is(err, windows.ERROR_INVALID_FUNCTION)) {
		return err
	}
	del := byte(1) // FILE_DISPOSITION_INFO: one BOOLEAN, DeleteFile
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &del, 1)
}

// ParentPath is the parent the locator of the differencing VHDX at p
// names (Files.ParentPath), read through a handle the broker opens as
// itself without following a link, sharing reading and writing with a
// running VM that holds the disk. Only the locator's bytes are parsed;
// the parent is never opened.
func (winFiles) ParentPath(p string) (string, error) {
	h, err := openPlainFile(p, windows.FILE_READ_DATA)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	size, err := handleSize(h)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", p, err)
	}
	parent, err := vhdx.ParentPath(handleReader{h}, size)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return parent, nil
}

// Canonical is p's final path as the broker sees it (no reparse point
// along it), used to decide whether a VM's recorded disk is one the
// broker may delete.
func (winFiles) Canonical(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", errIsReparse
	}
	return finalPath(h)
}

// RootDisk checks the broker's root disk (Files.RootDisk), as the
// broker: p made canonical, refusing a link at it (Canonical), then
// opened through virtdisk for its metadata only, without opening any
// parent its locator names, and refused unless it is a fixed or dynamic
// VHDX (checkRootVHDX).
func (f winFiles) RootDisk(p string) (string, error) {
	canon, err := f.Canonical(p)
	if err != nil {
		return "", err
	}
	if err := checkRootVHDX(canon); err != nil {
		return "", err
	}
	return canon, nil
}
