package agentshell

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

const capSetpcap = 8

// dockerDefaultCaps is the capability set a container runtime gives a
// root container by default (CHOWN, DAC_OVERRIDE, FOWNER, FSETID,
// KILL, SETGID, SETUID, SETPCAP, NET_BIND_SERVICE, NET_RAW, SYS_CHROOT,
// MKNOD, AUDIT_WRITE, SETFCAP): the install container's.
const dockerDefaultCaps uint64 = 0xa80425fb

// testIdentity is the image's: daemon 65533, commands 65532:65532.
var testIdentity = Identity{DaemonUID: 65533, UID: 65532, GID: 65532}

// fakeCreds models a thread's credentials the way Linux changes them:
// the checks each call makes (a capability in the effective set, or
// root), and what a uid change does to the capability sets. It records
// every call, in order, in a log shared with the install starter so
// tests can see what ran before the drop.
type fakeCreds struct {
	ids      IDs
	caps     Caps
	keepCaps bool
	lastCap  int
	log      *[]string
	fail     map[string]error
	// lie names a call that reports success without changing anything:
	// the verification must catch it.
	lie map[string]bool
	// keepSaved, when set, leaves the saved uid alone on Setresuid.
	keepSaved bool
}

func (f *fakeCreds) has(capability int) bool { return f.caps.Eff&(1<<capability) != 0 }

func (f *fakeCreds) call(name, line string) (skip bool, err error) {
	*f.log = append(*f.log, line)
	if err := f.fail[name]; err != nil {
		return true, err
	}
	return f.lie[name], nil
}

func (f *fakeCreds) IDs() (IDs, error) { return f.ids, f.fail["ids"] }

func (f *fakeCreds) Caps() (Caps, error) { return f.caps, f.fail["caps"] }

func (f *fakeCreds) CapLastCap() (int, error) { return f.lastCap, f.fail["caplastcap"] }

func (f *fakeCreds) DropBoundingCap(capability int) error {
	if skip, err := f.call("capbsetdrop", fmt.Sprintf("capbset drop %d", capability)); skip || err != nil {
		return err
	}
	if !f.has(capSetpcap) {
		return syscall.EPERM
	}
	f.caps.Bnd &^= 1 << capability
	return nil
}

func (f *fakeCreds) SetNoNewPrivs() error {
	if skip, err := f.call("nonewprivs", "no_new_privs"); skip || err != nil {
		return err
	}
	f.ids.NoNewPrivs = true
	return nil
}

func (f *fakeCreds) SetKeepCaps(keep bool) error {
	if skip, err := f.call("keepcaps", fmt.Sprintf("keepcaps %v", keep)); skip || err != nil {
		return err
	}
	f.keepCaps = keep
	return nil
}

func (f *fakeCreds) Setresgid(r, e, s int) error {
	if skip, err := f.call("setresgid", fmt.Sprintf("setresgid %d %d %d", r, e, s)); skip || err != nil {
		return err
	}
	if !f.has(capSetgid) {
		return syscall.EPERM
	}
	f.ids.RGID, f.ids.EGID, f.ids.SGID, f.ids.FSGID = r, e, s, e
	return nil
}

func (f *fakeCreds) Setgroups(g []int) error {
	if skip, err := f.call("setgroups", fmt.Sprintf("setgroups %v", g)); skip || err != nil {
		return err
	}
	if !f.has(capSetgid) {
		return syscall.EPERM
	}
	f.ids.Groups = append([]int{}, g...)
	return nil
}

func (f *fakeCreds) Setresuid(r, e, s int) error {
	if skip, err := f.call("setresuid", fmt.Sprintf("setresuid %d %d %d", r, e, s)); skip || err != nil {
		return err
	}
	if !f.has(capSetuid) {
		return syscall.EPERM
	}
	wasRoot := f.ids.RUID == 0 || f.ids.EUID == 0 || f.ids.SUID == 0
	f.ids.RUID, f.ids.EUID, f.ids.FSUID = r, e, e
	if !f.keepSaved {
		f.ids.SUID = s
	}
	if wasRoot && f.ids.RUID != 0 && f.ids.EUID != 0 && f.ids.SUID != 0 {
		if !f.keepCaps {
			f.caps.Prm = 0
		}
		f.caps.Eff, f.caps.Amb = 0, 0
	}
	return nil
}

func (f *fakeCreds) Capset(c Caps) error {
	if skip, err := f.call("capset", fmt.Sprintf("capset prm=%x eff=%x inh=%x", c.Prm, c.Eff, c.Inh)); skip || err != nil {
		return err
	}
	if c.Prm&^f.caps.Prm != 0 || c.Eff&^c.Prm != 0 {
		return syscall.EPERM
	}
	f.caps.Prm, f.caps.Eff, f.caps.Inh = c.Prm, c.Eff, c.Inh
	return nil
}

func (f *fakeCreds) Setfsuid(uid int) error {
	if skip, err := f.call("setfsuid", fmt.Sprintf("setfsuid %d", uid)); skip || err != nil {
		return err
	}
	if !f.has(capSetuid) && uid != f.ids.RUID && uid != f.ids.EUID && uid != f.ids.SUID && uid != f.ids.FSUID {
		return syscall.EPERM
	}
	f.ids.FSUID = uid
	return nil
}

// rootCreds is a root container's process: capabilities caps in the
// permitted, effective and bounding sets.
func rootCreds(log *[]string, caps uint64) *fakeCreds {
	return &fakeCreds{
		ids:     IDs{Groups: []int{0, 1, 2}},
		caps:    Caps{Prm: caps, Eff: caps, Bnd: caps},
		lastCap: 40,
		log:     log,
		fail:    map[string]error{},
		lie:     map[string]bool{},
	}
}

func userCreds(log *[]string) *fakeCreds {
	return &fakeCreds{
		ids:     IDs{RUID: 65532, EUID: 65532, SUID: 65532, FSUID: 65532, RGID: 65532, EGID: 65532, SGID: 65532, FSGID: 65532},
		lastCap: 40,
		log:     log,
		fail:    map[string]error{},
		lie:     map[string]bool{},
	}
}

// dropSequence is DropPrivileges' calls after the bounding set.
var dropSequence = []string{
	"no_new_privs",
	"keepcaps true",
	"setresgid 65532 65532 65532",
	"setgroups []",
	"setresuid 65533 65533 65533",
	"keepcaps false",
	"capset prm=e0 eff=e0 inh=0",
	"setfsuid 65532",
}

// The capability mask is CAP_KILL, CAP_SETGID and CAP_SETUID: a wrong
// bit here would hand the daemon a capability, or take away the one a
// command's start or kill needs.
func TestDaemonCapsMask(t *testing.T) {
	if DaemonCaps != 0xe0 {
		t.Errorf("DaemonCaps = %#x, want 0xe0", DaemonCaps)
	}
}

// From the runtime's default root set (the install container), every
// capability but the three leaves the bounding set first, while
// CAP_SETPCAP is still effective, and the process ends as the daemon's
// user with the commands' filesystem uid.
func TestDropPrivilegesOrderAndResult(t *testing.T) {
	var log []string
	c := rootCreds(&log, dockerDefaultCaps)
	if err := DropPrivileges(c, testIdentity); err != nil {
		t.Fatalf("DropPrivileges: %v", err)
	}
	var want []string
	for _, n := range []int{0, 1, 3, 4, 8, 10, 13, 18, 27, 29, 31} {
		want = append(want, fmt.Sprintf("capbset drop %d", n))
	}
	want = append(want, dropSequence...)
	if !reflect.DeepEqual(log, want) {
		t.Errorf("calls = %q\nwant  %q", log, want)
	}
	wantIDs := IDs{RUID: 65533, EUID: 65533, SUID: 65533, FSUID: 65532, RGID: 65532, EGID: 65532, SGID: 65532, FSGID: 65532, Groups: []int{}, NoNewPrivs: true}
	if !reflect.DeepEqual(c.ids, wantIDs) {
		t.Errorf("ids = %s, want %s", c.ids, wantIDs)
	}
	if want := (Caps{Prm: 0xe0, Eff: 0xe0, Bnd: 0xe0}); c.caps != want {
		t.Errorf("caps = %s, want %s", c.caps, want)
	}
}

// A container started with the three capabilities alone (the pod's
// drop ALL, add SETUID, SETGID, KILL) has no CAP_SETPCAP to drop with
// and nothing to drop: no PR_CAPBSET_DROP is attempted, which would
// fail.
func TestDropPrivilegesFromTheThreeCapabilities(t *testing.T) {
	var log []string
	c := rootCreds(&log, DaemonCaps)
	if err := DropPrivileges(c, testIdentity); err != nil {
		t.Fatalf("DropPrivileges: %v", err)
	}
	if !reflect.DeepEqual(log, dropSequence) {
		t.Errorf("calls = %q\nwant  %q", log, dropSequence)
	}
}

// Every way the drop can fall short is an error: a failing call, a call
// that claims success but changed nothing, a saved uid left at 0, a
// capability left anywhere it should not be.
func TestDropPrivilegesFailures(t *testing.T) {
	cases := map[string]func(c *fakeCreds){
		"caps unreadable":        func(c *fakeCreds) { c.fail["caps"] = errors.New("EIO") },
		"cap_last_cap fails":     func(c *fakeCreds) { c.fail["caplastcap"] = errors.New("ENOENT") },
		"cap_last_cap too big":   func(c *fakeCreds) { c.lastCap = 64 },
		"bounding drop fails":    func(c *fakeCreds) { c.fail["capbsetdrop"] = errors.New("EPERM") },
		"bounding drop lies":     func(c *fakeCreds) { c.lie["capbsetdrop"] = true },
		"no CAP_SETPCAP to drop": func(c *fakeCreds) { c.caps.Eff &^= 1 << capSetpcap },
		"no_new_privs fails":     func(c *fakeCreds) { c.fail["nonewprivs"] = errors.New("EINVAL") },
		"no_new_privs lies":      func(c *fakeCreds) { c.lie["nonewprivs"] = true },
		"keepcaps fails":         func(c *fakeCreds) { c.fail["keepcaps"] = errors.New("EINVAL") },
		"keepcaps lies":          func(c *fakeCreds) { c.lie["keepcaps"] = true },
		"setresgid fails":        func(c *fakeCreds) { c.fail["setresgid"] = errors.New("EPERM") },
		"setresgid lies":         func(c *fakeCreds) { c.lie["setresgid"] = true },
		"setgroups fails":        func(c *fakeCreds) { c.fail["setgroups"] = errors.New("EPERM") },
		"setgroups lies":         func(c *fakeCreds) { c.lie["setgroups"] = true },
		"setresuid fails":        func(c *fakeCreds) { c.fail["setresuid"] = errors.New("EPERM") },
		"setresuid lies":         func(c *fakeCreds) { c.lie["setresuid"] = true },
		"saved uid kept":         func(c *fakeCreds) { c.keepSaved = true },
		"capset fails":           func(c *fakeCreds) { c.fail["capset"] = errors.New("EPERM") },
		"capset lies":            func(c *fakeCreds) { c.lie["capset"] = true },
		"setfsuid fails":         func(c *fakeCreds) { c.fail["setfsuid"] = errors.New("ENOTSUP") },
		"setfsuid lies":          func(c *fakeCreds) { c.lie["setfsuid"] = true },
		"ids unreadable":         func(c *fakeCreds) { c.fail["ids"] = errors.New("EIO") },
		"bounding cap kept":      func(c *fakeCreds) { c.lastCap = 20 }, // 27, 29, 31 stay
		"one of the three gone":  func(c *fakeCreds) { c.caps.Prm &^= 1 << capKill },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			var log []string
			c := rootCreds(&log, dockerDefaultCaps)
			setup(c)
			if err := DropPrivileges(c, testIdentity); err == nil {
				t.Errorf("DropPrivileges succeeded with ids %s caps %s; calls %q", c.ids, c.caps, log)
			}
		})
	}
	// Nothing changes the uid after the gid failed: the order is the only
	// one in which all of them can succeed.
	var log []string
	c := rootCreds(&log, dockerDefaultCaps)
	c.fail["setresgid"] = errors.New("EPERM")
	_ = DropPrivileges(c, testIdentity)
	for _, l := range log {
		if strings.HasPrefix(l, "setresuid") {
			t.Errorf("setresuid ran after setresgid failed: %q", log)
		}
	}
}

// VerifyDaemon reads back every ID and capability set, so one left
// wrong is caught however it came about.
func TestVerifyDaemon(t *testing.T) {
	good := func() *fakeCreds {
		var log []string
		c := userCreds(&log)
		c.ids = IDs{RUID: 65533, EUID: 65533, SUID: 65533, FSUID: 65532, RGID: 65532, EGID: 65532, SGID: 65532, FSGID: 65532, Groups: []int{}, NoNewPrivs: true}
		c.caps = Caps{Prm: DaemonCaps, Eff: DaemonCaps, Bnd: DaemonCaps}
		return c
	}
	if err := VerifyDaemon(good(), testIdentity); err != nil {
		t.Fatalf("VerifyDaemon: %v", err)
	}
	cases := map[string]func(c *fakeCreds){
		"real uid":         func(c *fakeCreds) { c.ids.RUID = 0 },
		"effective uid":    func(c *fakeCreds) { c.ids.EUID = 65532 },
		"saved uid":        func(c *fakeCreds) { c.ids.SUID = 0 },
		"filesystem uid":   func(c *fakeCreds) { c.ids.FSUID = 65533 },
		"real gid":         func(c *fakeCreds) { c.ids.RGID = 0 },
		"effective gid":    func(c *fakeCreds) { c.ids.EGID = 0 },
		"saved gid":        func(c *fakeCreds) { c.ids.SGID = 0 },
		"filesystem gid":   func(c *fakeCreds) { c.ids.FSGID = 0 },
		"groups":           func(c *fakeCreds) { c.ids.Groups = []int{65532} },
		"no_new_privs":     func(c *fakeCreds) { c.ids.NoNewPrivs = false },
		"permitted extra":  func(c *fakeCreds) { c.caps.Prm |= 1 << 21 },
		"effective short":  func(c *fakeCreds) { c.caps.Eff = 1 << capSetuid },
		"bounding extra":   func(c *fakeCreds) { c.caps.Bnd = dockerDefaultCaps },
		"inheritable":      func(c *fakeCreds) { c.caps.Inh = DaemonCaps },
		"ambient":          func(c *fakeCreds) { c.caps.Amb = 1 << capKill },
		"caps unreadable":  func(c *fakeCreds) { c.fail["caps"] = errors.New("EIO") },
		"ids unreadable":   func(c *fakeCreds) { c.fail["ids"] = errors.New("EIO") },
		"same uid as cmds": func(c *fakeCreds) { c.ids.RUID, c.ids.EUID, c.ids.SUID = 65532, 65532, 65532 },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := good()
			setup(c)
			if err := VerifyDaemon(c, testIdentity); err == nil {
				t.Errorf("VerifyDaemon passed ids %s caps %s", c.ids, c.caps)
			}
		})
	}
}

func TestDropPrivilegesRefusesBadIdentity(t *testing.T) {
	for _, id := range []Identity{
		{DaemonUID: 0, UID: 65532, GID: 65532},
		{DaemonUID: 65533, UID: 0, GID: 65532},
		{DaemonUID: 65533, UID: 65532, GID: 0},
		{DaemonUID: -1, UID: 65532, GID: 65532},
		// The daemon as the commands' own user is no split at all.
		{DaemonUID: 65532, UID: 65532, GID: 65532},
	} {
		var log []string
		if err := DropPrivileges(rootCreds(&log, dockerDefaultCaps), id); err == nil {
			t.Errorf("DropPrivileges(%+v) succeeded", id)
		}
		if len(log) != 0 {
			t.Errorf("DropPrivileges(%+v) changed credentials: %q", id, log)
		}
	}
}

// The socket is made with the daemon's own filesystem uid, and the
// commands' is back before anything else touches a file.
func TestAsDaemonUser(t *testing.T) {
	var log []string
	c := rootCreds(&log, DaemonCaps)
	if err := DropPrivileges(c, testIdentity); err != nil {
		t.Fatal(err)
	}
	log = nil
	var fsuidInside int
	if err := AsDaemonUser(c, testIdentity, func() error {
		fsuidInside = c.ids.FSUID
		log = append(log, "f")
		return nil
	}); err != nil {
		t.Fatalf("AsDaemonUser: %v", err)
	}
	if fsuidInside != 65533 || c.ids.FSUID != 65532 {
		t.Errorf("fsuid inside %d, after %d; want 65533, 65532", fsuidInside, c.ids.FSUID)
	}
	if want := []string{"setfsuid 65533", "f", "setfsuid 65532"}; !reflect.DeepEqual(log, want) {
		t.Errorf("calls = %q, want %q", log, want)
	}

	// f's error comes back, after the filesystem uid is restored.
	boom := errors.New("listen: address in use")
	if err := AsDaemonUser(c, testIdentity, func() error { return boom }); !errors.Is(err, boom) || c.ids.FSUID != 65532 {
		t.Errorf("AsDaemonUser = %v, fsuid %d", err, c.ids.FSUID)
	}
	// A restore that does not take is an error.
	c.lie["setfsuid"] = true
	c.ids.FSUID = 65533
	if err := AsDaemonUser(c, testIdentity, func() error { return nil }); err == nil {
		t.Error("AsDaemonUser succeeded with the daemon's fsuid left in place")
	}
}

// As root with software to install, the install starts first (as
// root), then the process drops, and only then does the caller go on.
func TestBecomeUnprivilegedInstallsBeforeDropping(t *testing.T) {
	var log []string
	c := rootCreds(&log, DaemonCaps)
	var started []string
	start := func(pkgs []string) (*Install, error) {
		log = append(log, "start install")
		if !c.ids.Privileged() {
			t.Error("install started after the drop")
		}
		started = pkgs
		return NewInstall(pkgs), nil
	}
	inst, err := BecomeUnprivileged(c, testIdentity, []string{"ffmpeg", "jq"}, start, t.Logf)
	if err != nil {
		t.Fatalf("BecomeUnprivileged: %v", err)
	}
	if want := append([]string{"start install"}, dropSequence...); !reflect.DeepEqual(log, want) {
		t.Errorf("sequence = %q, want %q", log, want)
	}
	if !reflect.DeepEqual(started, []string{"ffmpeg", "jq"}) || inst == nil || inst.Finished() {
		t.Errorf("started %v, inst %+v", started, inst)
	}
	if c.ids.Privileged() {
		t.Errorf("still privileged: %s", c.ids)
	}
}

// As root with nothing to install, the daemon still drops.
func TestBecomeUnprivilegedDropsWithoutAnInstall(t *testing.T) {
	var log []string
	c := rootCreds(&log, DaemonCaps)
	start := func([]string) (*Install, error) { t.Error("install started with nothing to install"); return nil, nil }
	inst, err := BecomeUnprivileged(c, testIdentity, nil, start, t.Logf)
	if err != nil || inst != nil {
		t.Fatalf("BecomeUnprivileged = %v, %v", inst, err)
	}
	if !reflect.DeepEqual(log, dropSequence) {
		t.Errorf("calls = %q, want %q", log, dropSequence)
	}
}

// Any root user ID is still root, the effective one or the filesystem
// one alone included.
func TestBecomeUnprivilegedTreatsAnyRootUIDAsRoot(t *testing.T) {
	for _, set := range []func(*IDs){
		func(ids *IDs) { ids.EUID = 0 },
		func(ids *IDs) { ids.FSUID = 0 },
	} {
		var log []string
		c := userCreds(&log)
		set(&c.ids)
		_, _ = BecomeUnprivileged(c, testIdentity, nil, nil, t.Logf)
		if len(log) == 0 {
			t.Errorf("%s did not drop", c.ids)
		}
	}
}

// When the installer cannot start, the daemon still drops and serves,
// and every package is reported failed.
func TestBecomeUnprivilegedInstallerFailsToStart(t *testing.T) {
	var log []string
	c := rootCreds(&log, dockerDefaultCaps)
	start := func([]string) (*Install, error) { return nil, errors.New("exec format error") }
	inst, err := BecomeUnprivileged(c, testIdentity, []string{"jq"}, start, t.Logf)
	if err != nil {
		t.Fatalf("BecomeUnprivileged: %v", err)
	}
	if c.ids.Privileged() {
		t.Errorf("still privileged: %s", c.ids)
	}
	_, _, failed := inst.State()
	if len(failed) != 1 || !strings.Contains(failed[0].Error, "the installer could not start: exec format error") {
		t.Errorf("failed = %v", failed)
	}
}

// A drop that fails is the caller's fatal error, with no install
// returned to serve.
func TestBecomeUnprivilegedDropFailureIsAnError(t *testing.T) {
	var log []string
	c := rootCreds(&log, dockerDefaultCaps)
	c.lie["setresuid"] = true
	inst, err := BecomeUnprivileged(c, testIdentity, []string{"jq"}, func(p []string) (*Install, error) { return NewInstall(p), nil }, t.Logf)
	if err == nil || inst != nil {
		t.Errorf("BecomeUnprivileged = %v, %v; want an error", inst, err)
	}
}

// Not root: nothing changes, and software asked for is reported as
// failed with a plain reason.
func TestBecomeUnprivilegedNotRoot(t *testing.T) {
	var log []string
	c := userCreds(&log)
	start := func([]string) (*Install, error) { t.Error("install started without root"); return nil, nil }
	inst, err := BecomeUnprivileged(c, testIdentity, []string{"ffmpeg", "jq"}, start, t.Logf)
	if err != nil {
		t.Fatalf("BecomeUnprivileged: %v", err)
	}
	if len(log) != 0 {
		t.Errorf("credentials changed: %q", log)
	}
	_, installed, failed := inst.State()
	if len(installed) != 0 || len(failed) != 2 || failed[0].Error != reasonNotRoot {
		t.Errorf("installed %v, failed %v", installed, failed)
	}
	if !strings.Contains(inst.TakeNotice(), "does not run as root") {
		t.Error("the notice does not say why")
	}

	inst, err = BecomeUnprivileged(userCreds(&log), testIdentity, nil, start, t.Logf)
	if err != nil || inst != nil {
		t.Errorf("no software: BecomeUnprivileged = %v, %v", inst, err)
	}
}

// A non-root user in group root is refused.
func TestBecomeUnprivilegedRefusesGroupRoot(t *testing.T) {
	for _, set := range []func(*IDs){
		func(ids *IDs) { ids.EGID = 0 },
		func(ids *IDs) { ids.FSGID = 0 },
	} {
		var log []string
		c := userCreds(&log)
		set(&c.ids)
		if _, err := BecomeUnprivileged(c, testIdentity, nil, nil, t.Logf); err == nil {
			t.Errorf("served with group root: %s", c.ids)
		}
	}
}

func TestParseRunAs(t *testing.T) {
	if uid, gid, err := ParseRunAs("65532:65533"); err != nil || uid != 65532 || gid != 65533 {
		t.Errorf("ParseRunAs = %d, %d, %v", uid, gid, err)
	}
	for _, bad := range []string{"", "65532", "0:0", "0:65532", "65532:0", "a:b", "-1:5", "65532:65532:1"} {
		if _, _, err := ParseRunAs(bad); err == nil {
			t.Errorf("ParseRunAs(%q) succeeded", bad)
		}
	}
}

// The real host credentials of the test process are readable (on
// Linux, from /proc/self/status, so the parser meets a real kernel's
// text).
func TestHostCredsReadable(t *testing.T) {
	ids, err := HostCreds{}.IDs()
	if err != nil {
		t.Fatalf("IDs: %v", err)
	}
	if ids.EUID != os.Geteuid() || ids.EGID != os.Getegid() || ids.RUID != os.Getuid() {
		t.Errorf("IDs = %s, want uid %d euid %d egid %d", ids, os.Getuid(), os.Geteuid(), os.Getegid())
	}
	if _, err := (HostCreds{}).Caps(); err != nil {
		t.Errorf("Caps: %v", err)
	}
}

// The daemon's verification reads all four uid and gid columns and
// every capability line; a status it misreads would pass a daemon that
// is not what it should be.
func TestParseProcStatus(t *testing.T) {
	status := "Name:\tagent-shell\n" +
		"Uid:\t65533\t65533\t65533\t65532\n" +
		"Gid:\t65532\t65532\t65532\t65532\n" +
		"FDSize:\t64\n" +
		"Groups:\t\n" +
		"CapInh:\t0000000000000000\n" +
		"CapPrm:\t00000000000000e0\n" +
		"CapEff:\t00000000000000e0\n" +
		"CapBnd:\t00000000000000e0\n" +
		"CapAmb:\t0000000000000000\n" +
		"NoNewPrivs:\t1\n"
	ids, caps, err := ParseProcStatus(status)
	if err != nil {
		t.Fatalf("ParseProcStatus: %v", err)
	}
	wantIDs := IDs{RUID: 65533, EUID: 65533, SUID: 65533, FSUID: 65532, RGID: 65532, EGID: 65532, SGID: 65532, FSGID: 65532, Groups: []int{}, NoNewPrivs: true}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Errorf("ids = %s, want %s", ids, wantIDs)
	}
	if want := (Caps{Prm: 0xe0, Eff: 0xe0, Bnd: 0xe0}); caps != want {
		t.Errorf("caps = %s, want %s", caps, want)
	}

	// Root in a default container, with groups and an ambient set.
	ids, caps, err = ParseProcStatus("Uid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\nGroups:\t0 1 2 \nCapInh:\t0000000000000000\nCapPrm:\t00000000a80425fb\nCapEff:\t00000000a80425fb\nCapBnd:\t00000000a80425fb\nCapAmb:\t0000000000001000\nNoNewPrivs:\t0\n")
	if err != nil || !reflect.DeepEqual(ids.Groups, []int{0, 1, 2}) || caps != (Caps{Prm: dockerDefaultCaps, Eff: dockerDefaultCaps, Bnd: dockerDefaultCaps, Amb: 0x1000}) {
		t.Errorf("root status = %s %s, %v", ids, caps, err)
	}

	// A kernel without ambient capabilities has no CapAmb line.
	if _, caps, err := ParseProcStatus("Uid:\t1\t1\t1\t1\nGid:\t1\t1\t1\t1\nGroups:\nCapInh:\t0\nCapPrm:\t0\nCapEff:\t0\nCapBnd:\t0\nNoNewPrivs:\t0\n"); err != nil || caps != (Caps{}) {
		t.Errorf("without CapAmb = %s, %v", caps, err)
	}

	full := "Uid:\t1\t1\t1\t1\nGid:\t1\t1\t1\t1\nGroups:\t\nCapInh:\t0\nCapPrm:\t0\nCapEff:\t0\nCapBnd:\t0\nNoNewPrivs:\t0\n"
	bad := map[string]string{
		"empty":          "",
		"three uids":     strings.Replace(full, "Uid:\t1\t1\t1\t1", "Uid:\t1\t1\t1", 1),
		"bad gid":        strings.Replace(full, "Gid:\t1\t1\t1\t1", "Gid:\t1\tx\t1\t1", 1),
		"bad group":      strings.Replace(full, "Groups:\t", "Groups:\tx", 1),
		"bad cap":        strings.Replace(full, "CapEff:\t0", "CapEff:\tzz", 1),
		"no Uid":         strings.Replace(full, "Uid:", "Xid:", 1),
		"no Gid":         strings.Replace(full, "Gid:", "Xid:", 1),
		"no Groups":      strings.Replace(full, "Groups:", "Xroups:", 1),
		"no CapInh":      strings.Replace(full, "CapInh:", "XapInh:", 1),
		"no CapPrm":      strings.Replace(full, "CapPrm:", "XapPrm:", 1),
		"no CapEff":      strings.Replace(full, "CapEff:", "XapEff:", 1),
		"no CapBnd":      strings.Replace(full, "CapBnd:", "XapBnd:", 1),
		"no NoNewPrivs":  strings.Replace(full, "NoNewPrivs:", "XoNewPrivs:", 1),
		"bad NoNewPrivs": strings.Replace(full, "NoNewPrivs:\t0", "NoNewPrivs:\tx", 1),
		"name, nothing":  "Name:\tx\n",
	}
	for name, s := range bad {
		if _, _, err := ParseProcStatus(s); err == nil {
			t.Errorf("%s: ParseProcStatus(%q) succeeded", name, s)
		}
	}
}
