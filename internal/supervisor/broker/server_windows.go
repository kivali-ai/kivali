//go:build windows

package broker

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
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// connKey carries a request's pipe connection in its context, put there
// by the HTTP server's ConnContext, so the handler can impersonate the
// client of that very connection.
type connKey struct{}

// NewServer is the production broker: the embedded PowerShell runner,
// the broker's own file operations, and the impersonating caller check.
func NewServer(logf func(string, ...any)) (*Server, error) {
	ps, err := newPowerShell()
	if err != nil {
		return nil, err
	}
	return &Server{
		Runner:   ps,
		Files:    winFiles{},
		CallerOf: callerOf,
		Logf:     logf,
	}, nil
}

// ServiceAccount is the account the broker service runs as: a virtual
// account Windows derives from the service's name, with no password,
// whose SID is ServiceSID. It is not an administrator: at install it is
// made a member of Hyper-V Administrators, which is what managing VMs
// needs, and its token keeps only the two privileges below.
const ServiceAccount = `NT SERVICE\` + ServiceName

// servicePrivileges is every privilege the service's token keeps
// (ChangeServiceConfig2 with SERVICE_CONFIG_REQUIRED_PRIVILEGES_INFO
// strips the rest at logon): impersonating its pipe clients, which every
// check as the caller is, and bypassing traverse checks, which every
// path open needs.
var servicePrivileges = []string{"SeImpersonatePrivilege", "SeChangeNotifyPrivilege"}

// ownerSID is the SID the broker names as owner of what it creates (the
// pipe, the VM directories): its own account when it runs as the
// service, else the Administrators group, which an elevated --console
// broker's token may name and an ordinary account's may not. brokerOwned
// accepts both, so a client's check sees the broker in either mode.
func ownerSID() (string, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("this process's account: %w", err)
	}
	if sid := u.User.Sid.String(); strings.EqualFold(sid, ServiceSID) {
		return sid, nil
	}
	return AdministratorsSID, nil
}

// pipeDACL is the broker pipe's DACL. SYSTEM, Administrators and the
// service's own account have full control; Authenticated Users may
// connect, reading and writing data (FILE_GENERIC_READ and
// FILE_WRITE_DATA, 0x12008b) but not FILE_CREATE_PIPE_INSTANCE, which
// GENERIC_WRITE would include: every instance of a pipe shares its first
// instance's security descriptor, so an account that could add an
// instance could serve clients under the broker's owner, which is what
// Dial checks. The policy, not the DACL, is what protects the broker's
// operations (docs/developers/supervisor.md, "The broker").
var pipeDACL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + ServiceSID + ")(A;;0x12008b;;;AU)"

// pipeSDDL is the broker pipe's security descriptor: pipeDACL, and the
// broker's owner (ownerSID) named as the pipe's. A client's check
// (Dial, brokerOwned) accepts that owner in either mode, and an ordinary
// account cannot name it, so a pipe it created first never shows it.
func pipeSDDL() (string, error) {
	owner, err := ownerSID()
	if err != nil {
		return "", err
	}
	return "O:" + owner + pipeDACL, nil
}

// Listen creates the broker's named pipe (name, or PipeName when
// empty; KIVALI_BROKER_PIPE sets it for a dev run) with pipeSDDL. A pipe
// that already exists means a broker is already running (or a squatter
// holds the name, which clients refuse); go-winio creates the first
// instance exclusively, so the name is never shared.
func Listen(name string) (net.Listener, error) {
	if name == "" {
		name = PipeName
	}
	sddl, err := pipeSDDL()
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", name, err)
	}
	return ln, nil
}

// httpServer wraps the policy handler, storing each connection so the
// handler can impersonate its client.
func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, c)
		},
	}
}

// RunConsole serves on the pipe in the foreground until ctx is done
// (--console, in an elevated terminal).
func (s *Server) RunConsole(ctx context.Context, ln net.Listener) error {
	srv := s.httpServer()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		return nil
	case err := <-errc:
		return err
	}
}

// RunService serves on the pipe as a Windows service, handling stop and
// shutdown.
func (s *Server) RunService(ln net.Listener) error {
	return svc.Run(ServiceName, &svcHandler{srv: s, ln: ln})
}

type svcHandler struct {
	srv *Server
	ln  net.Listener
}

func (h *svcHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	httpSrv := h.srv.httpServer()
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(h.ln) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = httpSrv.Shutdown(ctx)
				cancel()
				return false, 0
			default:
				h.srv.logf("unexpected service control %d", c.Cmd)
			}
		case err := <-errc:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				h.srv.logf("broker serve: %v", err)
				return false, 1
			}
			return false, 0
		}
	}
}

// IsService reports whether this process was started by the service
// control manager (so the broker command knows to call RunService).
func IsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

// LogDir is where the service writes its log (LogFile), made at install
// with a DACL that lets the service's account write there: SYSTEM,
// Administrators and the service have full control, Users may read.
func LogDir() (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", fmt.Errorf("ProgramData: %w", err)
	}
	return filepath.Join(base, "Kivali", "broker"), nil
}

// LogFile is the service's log.
func LogFile() (string, error) {
	dir, err := LogDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "broker.log"), nil
}

// Install registers the broker as an automatically starting service
// whose binary is exePath run with the broker subcommand, under its own
// virtual account (ServiceAccount) with an unrestricted service SID (so
// the DACLs above can name it) and only servicePrivileges in its token;
// grants that account the right to read and run exePath (a binary in
// Program Files allows that already; one in a profile does not); makes
// the log directory; registers the guest agent's Hyper-V socket service;
// makes the account a member of Hyper-V Administrators; registers the
// event source; and starts the service. Run as an administrator (the
// installer, or `broker install` in an elevated terminal).
//
// Everything it leaves on the machine, Uninstall removes: the service
// (a virtual account is no object of its own: it exists only as the
// service's SID, and the group membership is its one trace), the
// membership, the event source's registry key, the log directory and
// the socket service's registry key. The access entry on exePath goes
// with the binary.
func Install(exePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run as administrator): %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	if s, err := m.OpenService(ServiceName); err == nil {
		defer func() { _ = s.Close() }()
		return refresh(s, exePath)
	}
	service, err := m.CreateService(ServiceName, exePath, mgr.Config{
		DisplayName:      "Kivali broker",
		Description:      "Manages Hyper-V virtual machines for Kivali Desktop.",
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: ServiceAccount,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}, "broker")
	if err != nil {
		return fmt.Errorf("create the %s service: %w", ServiceName, err)
	}
	defer func() { _ = service.Close() }()
	// A step that fails after the service exists takes the service with
	// it, so the machine is left as it was and Install can run again.
	err = func() error {
		if err := setRequiredPrivileges(service.Handle, servicePrivileges); err != nil {
			return fmt.Errorf("limit the service's privileges: %w", err)
		}
		if err := grantServiceRead(exePath); err != nil {
			return err
		}
		if err := makeLogDir(); err != nil {
			return err
		}
		if err := vsockServiceKey(); err != nil {
			return err
		}
		if err := addToHyperVAdministrators(); err != nil {
			return err
		}
		if err := eventlog.InstallAsEventCreate(ServiceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
			!errorContains(err, "already exists") {
			return fmt.Errorf("register the event source: %w", err)
		}
		if err := service.Start(); err != nil {
			return fmt.Errorf("start the %s service: %w", ServiceName, err)
		}
		return nil
	}()
	if err != nil {
		_ = removeFromHyperVAdministrators()
		_ = eventlog.Remove(ServiceName)
		_ = service.Delete()
		return err
	}
	return nil
}

// refresh is Install on a machine where the service already exists (an
// upgrade: the installer stopped the service, replaced the binary and
// runs `broker install` again): the service runs exePath, with every
// step of Install applied again, each one idempotent, so a step a newer
// version adds reaches an upgraded machine too; then it starts, unless
// it is running.
func refresh(service *mgr.Service, exePath string) error {
	cfg, err := service.Config()
	if err != nil {
		return fmt.Errorf("read the %s service: %w", ServiceName, err)
	}
	if want := windows.EscapeArg(exePath) + " broker"; cfg.BinaryPathName != want {
		cfg.BinaryPathName = want
		if err := service.UpdateConfig(cfg); err != nil {
			return fmt.Errorf("point the %s service at %s: %w", ServiceName, exePath, err)
		}
	}
	if err := setRequiredPrivileges(service.Handle, servicePrivileges); err != nil {
		return fmt.Errorf("limit the service's privileges: %w", err)
	}
	if err := grantServiceRead(exePath); err != nil {
		return err
	}
	if err := makeLogDir(); err != nil {
		return err
	}
	if err := vsockServiceKey(); err != nil {
		return err
	}
	if err := addToHyperVAdministrators(); err != nil {
		return err
	}
	if err := eventlog.InstallAsEventCreate(ServiceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!errorContains(err, "already exists") {
		return fmt.Errorf("register the event source: %w", err)
	}
	if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start the %s service: %w", ServiceName, err)
	}
	return nil
}

// Uninstall removes everything Install made (its comment lists it),
// each on its own, so a step that fails leaves the rest done and a
// second run finishes it; a service that is not there is not an error
// when something else was removed. The teams' files stay, and so does
// the broker's own directory when it still holds the Hyper-V files of
// teams whose VMs are registered (a team not destroyed before the
// uninstall): those are returned as left, the teams' config directory
// keys, and are not an error. Hyper-V keeps a registered VM's files
// open, and deleting them under it would leave the VM broken.
func Uninstall() (left []string, err error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("connect to the service manager (run as administrator): %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	var errs []error
	if err := removeFromHyperVAdministrators(); err != nil {
		errs = append(errs, err)
	}
	_ = eventlog.Remove(ServiceName)
	if err := removeVsockServiceKey(); err != nil {
		errs = append(errs, err)
	}
	service, err := m.OpenService(ServiceName)
	if err != nil {
		errs = append(errs, fmt.Errorf("the %s service is not installed", ServiceName))
	} else {
		if st, err := service.Control(svc.Stop); err == nil {
			// Wait for the stop, so the binary and the log are free.
			for i := 0; i < 50 && st.State != svc.Stopped; i++ {
				time.Sleep(200 * time.Millisecond)
				if st, err = service.Query(); err != nil {
					break
				}
			}
		}
		if err := service.Delete(); err != nil {
			errs = append(errs, fmt.Errorf("delete the %s service: %w", ServiceName, err))
		}
		_ = service.Close()
	}
	left, err = registeredVMs()
	if err != nil {
		errs = append(errs, err)
	} else if len(left) == 0 {
		if err := removeLogDir(); err != nil {
			errs = append(errs, err)
		}
	}
	return left, errors.Join(errs...)
}

// registeredVMs lists the config directory keys of the VM directories
// still under the broker's VM root: each is a team whose VM is still
// registered in Hyper-V. None when the root is absent.
func registeredVMs() ([]string, error) {
	root, err := (winFiles{}).VMRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", root, err)
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			keys = append(keys, e.Name())
		}
	}
	return keys, nil
}

// grantServiceRead adds, to exePath's DACL, the service account's right
// to read and run it (FILE_GENERIC_READ and FILE_GENERIC_EXECUTE,
// 0x1200a9); the service's SeChangeNotifyPrivilege spares it the
// directories above. The entry is added to what is there.
func grantServiceRead(exePath string) error {
	sid, err := windows.StringToSid(ServiceSID)
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(exePath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the DACL of %s: %w", exePath, err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read the DACL of %s: %w", exePath, err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: 0x1200a9,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN, TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, old)
	if err != nil {
		return fmt.Errorf("the DACL of %s: %w", exePath, err)
	}
	if err := windows.SetNamedSecurityInfo(exePath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("let the service read %s: %w", exePath, err)
	}
	return nil
}

// removeVsockServiceKey deletes what vsockServiceKey made.
func removeVsockServiceKey() error {
	const path = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\00000400-facb-11e6-bd58-64006a7986d3`
	err := registry.DeleteKey(registry.LOCAL_MACHINE, path)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("remove the guest agent's Hyper-V socket service: %w", err)
	}
	return nil
}

// removeLogDir deletes the log directory and, when nothing else is in
// it, its parent, both through handles as the broker's own directories
// (RemoveVMDir's checks: a directory the broker did not make is left).
func removeLogDir() error {
	dir, err := LogDir()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		if err := (winFiles{}).RemoveVMDir(dir, ""); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
	}
	parent, err := windows.UTF16PtrFromString(filepath.Dir(dir))
	if err == nil {
		_ = windows.RemoveDirectory(parent) // only when empty
	}
	return nil
}

// setRequiredPrivileges leaves the service's token with exactly privs.
func setRequiredPrivileges(h windows.Handle, privs []string) error {
	// A multi-string: each name NUL-terminated, then one more NUL.
	var u []uint16
	for _, p := range privs {
		w, err := windows.UTF16FromString(p)
		if err != nil {
			return err
		}
		u = append(u, w...)
	}
	u = append(u, 0)
	info := struct{ RequiredPrivileges *uint16 }{&u[0]}
	return windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_REQUIRED_PRIVILEGES_INFO, (*byte)(unsafe.Pointer(&info)))
}

var (
	netapi32                    = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")
	procNetLocalGroupDelMembers = netapi32.NewProc("NetLocalGroupDelMembers")
)

// The NetLocalGroup*Members answers that mean the membership is already
// as asked.
const (
	nerrSuccess           = 0
	errorMemberInAlias    = 1378 // already a member
	errorMemberNotInAlias = 1377 // not a member
)

// hyperVAdministrators is the local Hyper-V Administrators group's name
// on this machine (it is localised), from its well-known SID.
func hyperVAdministrators() (string, error) {
	sid, err := windows.StringToSid(HyperVAdministratorsSID)
	if err != nil {
		return "", err
	}
	name, _, _, err := sid.LookupAccount("")
	if err != nil {
		return "", fmt.Errorf("the Hyper-V Administrators group (is Hyper-V enabled?): %w", err)
	}
	return name, nil
}

// localGroupMembership adds the service's account to, or removes it
// from, the local group named group, by SID (level 0, which takes no
// name, so nothing is parsed from the account's name).
func localGroupMembership(group string, add bool) error {
	sid, err := windows.StringToSid(ServiceSID)
	if err != nil {
		return err
	}
	g, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	member := struct{ Sid *windows.SID }{sid}
	proc, already := procNetLocalGroupDelMembers, uintptr(errorMemberNotInAlias)
	if add {
		proc, already = procNetLocalGroupAddMembers, errorMemberInAlias
	}
	r, _, _ := proc.Call(0, uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&member)), 1)
	if r != nerrSuccess && r != already {
		return fmt.Errorf("local group %q: %w", group, windows.Errno(r))
	}
	return nil
}

func addToHyperVAdministrators() error {
	group, err := hyperVAdministrators()
	if err != nil {
		return err
	}
	if err := localGroupMembership(group, true); err != nil {
		return fmt.Errorf("make %s a member of %s: %w", ServiceAccount, group, err)
	}
	return nil
}

func removeFromHyperVAdministrators() error {
	group, err := hyperVAdministrators()
	if err != nil {
		return nil // no Hyper-V, no membership
	}
	if err := localGroupMembership(group, false); err != nil {
		return fmt.Errorf("remove %s from %s: %w", ServiceAccount, group, err)
	}
	return nil
}

// logDirSDDL is the log directories' security descriptor: owned by the
// Administrators group (the installer's), protected, with SYSTEM,
// Administrators and the service's account in full control and Users
// reading. ProgramData lets any user create directories in it, so the
// two directories (ProgramData\Kivali and its broker) are made with
// this descriptor and, when one exists already, opened without
// following a link and accepted only if the broker made it
// (brokerOwned): a user who made ProgramData\Kivali first, with a
// junction named broker inside, would otherwise have the installer put
// this DACL on wherever the junction pointed.
func logDirSDDL() (string, error) {
	owner, err := ownerSID()
	if err != nil {
		return "", err
	}
	return "O:" + owner + "D:P(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)(A;OICI;GA;;;" + ServiceSID + ")(A;OICI;GR;;;BU)", nil
}

// makeLogDir makes LogDir and its parent (brokerDir).
func makeLogDir() error {
	dir, err := LogDir()
	if err != nil {
		return err
	}
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := brokerDir(d); err != nil {
			return err
		}
	}
	return nil
}

// brokerDir makes the directory d as logDirSDDL says if it is absent,
// opens it without following a link, refuses it unless the broker made
// it (brokerOwned), and re-applies the DACL through that handle (an
// earlier install's directory has no entry for the service).
func brokerDir(d string) error {
	sddl, err := logDirSDDL()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(d)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create %s: %w", d, err)
	}
	h, _, err := openDir(d, windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return err
	}
	err = checkBrokerOwned(h, d)
	if err == nil {
		err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	_ = windows.CloseHandle(h)
	if err != nil {
		return fmt.Errorf("the broker's directory %s: %w", d, err)
	}
	return nil
}

// OpenLog opens the service's log for appending (LogFile), without
// following a link at it, in a directory the broker made (brokerOwned,
// checked on a handle that followed no link either): the directory's
// owner cannot be anyone else, but a file or link there could be left
// by an earlier install, and the service, which writes as itself with
// full control of every team's data disk, must never append to a link's
// target.
func OpenLog() (*os.File, error) {
	path, err := LogFile()
	if err != nil {
		return nil, err
	}
	dir, _, err := openDir(filepath.Dir(path), windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return nil, err
	}
	err = checkBrokerOwned(dir, filepath.Dir(path))
	_ = windows.CloseHandle(dir)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.FILE_APPEND_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("%s is a link or a directory; remove it", path)
	}
	return os.NewFile(uintptr(h), path), nil
}

// vsockServiceKey registers the guest agent's vsock port (1024) as a
// Hyper-V socket service, so the host can dial it: the fixed key under
// HKLM, which only an administrator can write, so it is the installer's
// (Install), not the service's (POST /v1/setup reports it, and cannot
// make it).
func vsockServiceKey() error {
	const path = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\GuestCommunicationServices\00000400-facb-11e6-bd58-64006a7986d3`
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("register the guest agent's Hyper-V socket service: %w", err)
	}
	defer func() { _ = k.Close() }()
	if err := k.SetStringValue("ElementName", "Kivali guest agent (vsock 1024)"); err != nil {
		return fmt.Errorf("register the guest agent's Hyper-V socket service: %w", err)
	}
	return nil
}

func errorContains(err error, s string) bool {
	return err != nil && strings.Contains(err.Error(), s)
}
