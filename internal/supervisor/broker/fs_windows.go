//go:build windows

package broker

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
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
// Close, reading, so what was checked cannot be swapped (File; a
// name-only check holds an attributes-only handle, which pins nothing).
// The SID is read once from the impersonation token.
type winCaller struct {
	pipe    windows.Handle
	sid     string
	handles []windows.Handle
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
	return nil
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
// as the caller, for the asked access, and canonicalises it.
//
// The handle kept until Close pins the file against a rename, removal
// or replacement while the request runs: it is opened without delete
// sharing and reads the file's data, since only data access and delete
// take part in sharing (an attributes-only handle pins nothing). It
// never writes: Hyper-V opens a disk to attach or start it with a share
// mode that admits readers but no other writer, so the attach of
// data.vhdx failed with a sharing violation while the broker held it
// for writing, and every attach and start since has run with a reading
// handle held). So AccessWrite is checked with a first open that is
// closed once a second, read-only open is confirmed to be the same file
// (by identity: the first pins the name meanwhile, so the second cannot
// land elsewhere). AccessName, for a disk a running VM holds, opens
// attributes only, which that VM's own open admits, and pins nothing:
// the names are compared, and the running VM holds the files itself.
func (c *winCaller) File(p string, access Access) (string, error) {
	const pinAccess = windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES
	var da uint32
	switch access {
	case AccessRead:
		da = pinAccess
	case AccessWrite:
		da = pinAccess | windows.FILE_WRITE_DATA
	case AccessName:
		da = windows.FILE_READ_ATTRIBUTES
	default:
		return "", fmt.Errorf("unknown access %d", access)
	}
	h, canon, err := c.open(p, da, false)
	if err != nil {
		return "", err
	}
	if access != AccessWrite {
		c.keep(h)
		return canon, nil
	}
	defer func() { _ = windows.CloseHandle(h) }()
	checked, err := handleIdentity(h)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", p, err)
	}
	pin, pinCanon, err := c.open(p, pinAccess, false)
	if err != nil {
		return "", err
	}
	pinned, err := handleIdentity(pin)
	if err != nil || pinned != checked || !strings.EqualFold(pinCanon, canon) {
		_ = windows.CloseHandle(pin)
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", p, err)
		}
		return "", fmt.Errorf("%s changed while it was being checked", p)
	}
	c.keep(pin)
	return canon, nil
}

// handleIdentity is the identity of the file open as h.
func handleIdentity(h windows.Handle) (fileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fileIdentity{}, err
	}
	return identityOf(&info), nil
}

// Absent succeeds only if nothing is at p as the caller sees it. A
// dangling link counts as present: opening it with the reparse-point
// flag opens the link itself and succeeds.
func (c *winCaller) Absent(p string) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	return onCallerThread(c.pipe, func() error {
		h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err == nil {
			_ = windows.CloseHandle(h)
			return ErrExists
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	})
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

// winFiles does the broker's own file-system work (as the service's account, or an elevated console broker):
// making, canonicalising and removing the per-VM directory, and putting
// a new data disk in place. Root, when set, is where the VM directories
// go instead of the broker's own tree (VMRoot).
type winFiles struct{ Root string }

// errNotBrokerDir is the refusal of a VM directory the broker did not
// make (brokerOwned): the caller created it first, so the caller can
// write in it, and the broker neither creates nor deletes anything
// there as itself.
var errNotBrokerDir = errors.New("not made by the broker (its owner is neither the broker service's account, LocalSystem nor Administrators); remove it and it is made again")

// VMDir makes <configDir>\vm if absent, owned by the Administrators
// group with a protected DACL that grants SYSTEM and that group full
// control and the owner read, opens it without delete sharing (so it
// cannot be swapped under the request), and checks it is a real
// directory, no reparse point, and the broker's own (brokerOwned): a
// directory of that name the caller made first is refused, since the
// broker creates files in it as itself.
//
// The owner is named rather than left to the token's default, so it is
// the same whether the broker runs as the service or elevated with
// --console: both tokens may name the Administrators group as owner,
// and an ordinary account's may not.
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

// fileIdentity is what names a file whatever its path: its volume and
// its file index there.
type fileIdentity struct{ volume, high, low uint32 }

func identityOf(info *windows.ByHandleFileInformation) fileIdentity {
	return fileIdentity{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow}
}

// openPlainFile opens p without following a link at it, with access
// and without delete sharing, and checks it is a plain file: no reparse
// point, no directory, one link (a hard link to some other file has
// more). It returns the handle and the file's identity.
func openPlainFile(p string, access uint32) (windows.Handle, fileIdentity, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, fileIdentity{}, err
	}
	h, err := windows.CreateFile(name, access|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, fileIdentity{}, fmt.Errorf("open %s: %w", p, err)
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
		return 0, fileIdentity{}, err
	}
	return h, identityOf(&info), nil
}

// diskDACL is the DACL a new data disk gets (GrantFile, PlacedFile):
// SYSTEM, Administrators and the service keep full control, and the
// caller gets it, so the supervisor can snapshot, rename and delete the
// disk as itself. It is not protected: what the directory the disk is
// in passes on to its files applies too.
func diskDACL(callerSID string) (*windows.ACL, error) {
	sd, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + ServiceSID + ")(A;;FA;;;" + callerSID + ")")
	if err != nil {
		return nil, fmt.Errorf("the disk's security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, fmt.Errorf("the disk's security descriptor: %w", err)
	}
	return dacl, nil
}

// GrantFile grants callerSID what a rename needs on p, the broker's new
// file in a VM directory (Files.GrantFile): DELETE, with the attributes
// and synchronisation a rename's open asks for, and nothing more, through
// a handle opened on it without following a link; it returns the file's
// identity. The caller then moves the file into its own directory as
// itself (Caller.Rename: the broker, no administrator, cannot write
// there) and gets full control once it has landed (PlacedFile). A file
// the caller could write while it sits in the broker's directory could
// be turned into a link there, which the next New-VHD would follow.
func (winFiles) GrantFile(p, callerSID string) (FileID, error) {
	if err := ValidSID(callerSID); err != nil {
		return FileID{}, err
	}
	// DELETE 0x10000, SYNCHRONIZE 0x100000, FILE_READ_ATTRIBUTES 0x80.
	sd, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + ServiceSID + ")(A;;0x110080;;;" + callerSID + ")")
	if err != nil {
		return FileID{}, fmt.Errorf("the new disk's security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return FileID{}, fmt.Errorf("the new disk's security descriptor: %w", err)
	}
	h, id, err := openPlainFile(p, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return FileID{}, fmt.Errorf("the new disk: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return FileID{}, fmt.Errorf("grant the caller %s: %w", p, err)
	}
	return FileID{Volume: id.volume, High: id.high, Low: id.low}, nil
}

// PlacedFile checks that dst, opened as itself without following a link,
// is a plain file, the very one GrantFile returned id for, and
// canonically dst (Files.PlacedFile): the caller moved it there, and
// could have renamed it away and put something else at dst. It then
// applies the disk's DACL again through that handle, so what dst's
// directory passes on to its files replaces what the VM directory did.
func (winFiles) PlacedFile(dst string, id FileID, callerSID string) error {
	dacl, err := diskDACL(callerSID)
	if err != nil {
		return err
	}
	h, got, err := openPlainFile(dst, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return fmt.Errorf("after the move: %w", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if got != (fileIdentity{id.Volume, id.High, id.Low}) {
		return fmt.Errorf("%s is no longer the disk the broker made", dst)
	}
	canon, err := finalPath(h)
	if err != nil {
		return fmt.Errorf("canonicalise %s: %w", dst, err)
	}
	if !strings.EqualFold(canon, dst) {
		return fmt.Errorf("%w (%s resolves to %s)", errReparseOnPath, dst, canon)
	}
	err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	if err != nil {
		return fmt.Errorf("grant the caller %s: %w", dst, err)
	}
	return nil
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

// Rename moves src to dst as the caller (Caller.Rename): MoveFileEx
// without MOVEFILE_REPLACE_EXISTING, which never follows a link at dst
// and fails if anything is there (ErrExists). As the caller, the move
// can land only where the caller may write, and the file was granted to
// the caller first (GrantFile), so DELETE on it is theirs.
func (c *winCaller) Rename(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return onCallerThread(c.pipe, func() error {
		err := windows.MoveFileEx(from, to, 0)
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return ErrExists
		}
		return err
	})
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
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the DACL of %s: %w", dir, err)
		}
		if !strings.Contains(strings.ToUpper(sd.String()), ";;;"+strings.ToUpper(callerSID)+")") {
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
