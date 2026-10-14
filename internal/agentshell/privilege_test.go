package agentshell

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fakeCreds models a process's credentials the way Linux changes them
// for root: Setgid and Setuid set real, effective and saved IDs. It
// records every call, in order, in a log shared with the install
// starter so tests can see what ran before the drop.
type fakeCreds struct {
	ids  IDs
	log  *[]string
	fail map[string]error
	// lie, when set, makes Setuid report success without changing
	// anything: the verification must catch it.
	lie bool
	// keepSaved, when set, leaves the saved uid alone on Setuid.
	keepSaved bool
	// caps is the capability sets; Setuid empties them, as Linux does
	// when a process leaves uid 0, unless keepCaps is set.
	caps     Caps
	keepCaps bool
}

func (f *fakeCreds) IDs() (IDs, error) { return f.ids, f.fail["ids"] }

func (f *fakeCreds) Caps() (Caps, error) { return f.caps, f.fail["caps"] }

func (f *fakeCreds) Setgroups(g []int) error {
	*f.log = append(*f.log, fmt.Sprintf("setgroups %v", g))
	if err := f.fail["setgroups"]; err != nil {
		return err
	}
	f.ids.Groups = append([]int{}, g...)
	return nil
}

func (f *fakeCreds) Setgid(gid int) error {
	*f.log = append(*f.log, fmt.Sprintf("setgid %d", gid))
	if err := f.fail["setgid"]; err != nil {
		return err
	}
	f.ids.RGID, f.ids.EGID, f.ids.SGID = gid, gid, gid
	return nil
}

func (f *fakeCreds) Setuid(uid int) error {
	*f.log = append(*f.log, fmt.Sprintf("setuid %d", uid))
	if err := f.fail["setuid"]; err != nil {
		return err
	}
	if f.lie {
		return nil
	}
	f.ids.RUID, f.ids.EUID = uid, uid
	if !f.keepSaved {
		f.ids.SUID = uid
	}
	if !f.keepCaps {
		f.caps = Caps{}
	}
	return nil
}

func rootCreds(log *[]string) *fakeCreds {
	return &fakeCreds{ids: IDs{Groups: []int{0, 1, 2}}, caps: Caps{Eff: 0xa80425fb}, log: log, fail: map[string]error{}}
}

func userCreds(log *[]string) *fakeCreds {
	return &fakeCreds{ids: IDs{RUID: 65532, EUID: 65532, SUID: 65532, RGID: 65532, EGID: 65532, SGID: 65532}, log: log, fail: map[string]error{}}
}

func TestDropPrivilegesOrderAndResult(t *testing.T) {
	var log []string
	c := rootCreds(&log)
	if err := DropPrivileges(c, 65532, 65532); err != nil {
		t.Fatalf("DropPrivileges: %v", err)
	}
	if want := []string{"setgroups []", "setgid 65532", "setuid 65532"}; !reflect.DeepEqual(log, want) {
		t.Errorf("calls = %q, want %q", log, want)
	}
	want := IDs{RUID: 65532, EUID: 65532, SUID: 65532, RGID: 65532, EGID: 65532, SGID: 65532, Groups: []int{}}
	if !reflect.DeepEqual(c.ids, want) {
		t.Errorf("ids = %s, want %s", c.ids, want)
	}
}

// Every way the drop can fall short is an error: a failing call, a
// call that claims success but changed nothing, a saved uid left at 0.
func TestDropPrivilegesFailures(t *testing.T) {
	cases := map[string]func(c *fakeCreds){
		"setgroups fails": func(c *fakeCreds) { c.fail["setgroups"] = errors.New("EPERM") },
		"setgid fails":    func(c *fakeCreds) { c.fail["setgid"] = errors.New("EPERM") },
		"setuid fails":    func(c *fakeCreds) { c.fail["setuid"] = errors.New("EPERM") },
		"setuid lies":     func(c *fakeCreds) { c.lie = true },
		"saved uid kept":  func(c *fakeCreds) { c.keepSaved = true },
		"ids unreadable":  func(c *fakeCreds) { c.fail["ids"] = errors.New("EIO") },
		"caps kept":       func(c *fakeCreds) { c.keepCaps = true },
		"ambient cap set": func(c *fakeCreds) { c.keepCaps = true; c.caps = Caps{Amb: 1 << 12} },
		"caps unreadable": func(c *fakeCreds) { c.fail["caps"] = errors.New("EIO") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			var log []string
			c := rootCreds(&log)
			setup(c)
			if err := DropPrivileges(c, 65532, 65532); err == nil {
				t.Errorf("DropPrivileges succeeded with ids %s", c.ids)
			}
		})
	}
	// setuid is never attempted after setgid failed: the order is the
	// only one in which all three can succeed.
	var log []string
	c := rootCreds(&log)
	c.fail["setgid"] = errors.New("EPERM")
	_ = DropPrivileges(c, 65532, 65532)
	for _, l := range log {
		if strings.HasPrefix(l, "setuid") {
			t.Errorf("setuid ran after setgid failed: %q", log)
		}
	}
}

func TestDropPrivilegesRefusesRoot(t *testing.T) {
	for _, ids := range [][2]int{{0, 65532}, {65532, 0}, {-1, 5}} {
		var log []string
		if err := DropPrivileges(rootCreds(&log), ids[0], ids[1]); err == nil {
			t.Errorf("DropPrivileges(%d, %d) succeeded", ids[0], ids[1])
		}
		if len(log) != 0 {
			t.Errorf("DropPrivileges(%d, %d) changed credentials: %q", ids[0], ids[1], log)
		}
	}
}

// As root with software to install, the install starts first (as
// root), then the process drops, and only then does the caller go on.
func TestBecomeUnprivilegedInstallsBeforeDropping(t *testing.T) {
	var log []string
	c := rootCreds(&log)
	var started []string
	start := func(pkgs []string) (*Install, error) {
		log = append(log, "start install")
		if !c.ids.Privileged() {
			t.Error("install started after the drop")
		}
		started = pkgs
		return NewInstall(pkgs), nil
	}
	inst, err := BecomeUnprivileged(c, 65532, 65532, []string{"ffmpeg", "jq"}, start, t.Logf)
	if err != nil {
		t.Fatalf("BecomeUnprivileged: %v", err)
	}
	if want := []string{"start install", "setgroups []", "setgid 65532", "setuid 65532"}; !reflect.DeepEqual(log, want) {
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
	c := rootCreds(&log)
	start := func([]string) (*Install, error) { t.Error("install started with nothing to install"); return nil, nil }
	inst, err := BecomeUnprivileged(c, 65532, 65532, nil, start, t.Logf)
	if err != nil || inst != nil {
		t.Fatalf("BecomeUnprivileged = %v, %v", inst, err)
	}
	if want := []string{"setgroups []", "setgid 65532", "setuid 65532"}; !reflect.DeepEqual(log, want) {
		t.Errorf("calls = %q, want %q", log, want)
	}
}

// Only the effective (or saved) uid being 0 is still root.
func TestBecomeUnprivilegedTreatsAnyRootUIDAsRoot(t *testing.T) {
	var log []string
	c := userCreds(&log)
	c.ids.EUID = 0
	if _, err := BecomeUnprivileged(c, 65532, 65532, nil, nil, t.Logf); err != nil {
		t.Fatalf("BecomeUnprivileged: %v", err)
	}
	if len(log) == 0 {
		t.Error("euid 0 did not drop")
	}
}

// When the installer cannot start, the daemon still drops and serves,
// and every package is reported failed.
func TestBecomeUnprivilegedInstallerFailsToStart(t *testing.T) {
	var log []string
	c := rootCreds(&log)
	start := func([]string) (*Install, error) { return nil, errors.New("exec format error") }
	inst, err := BecomeUnprivileged(c, 65532, 65532, []string{"jq"}, start, t.Logf)
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
	c := rootCreds(&log)
	c.lie = true
	inst, err := BecomeUnprivileged(c, 65532, 65532, []string{"jq"}, func(p []string) (*Install, error) { return NewInstall(p), nil }, t.Logf)
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
	inst, err := BecomeUnprivileged(c, 65532, 65532, []string{"ffmpeg", "jq"}, start, t.Logf)
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

	inst, err = BecomeUnprivileged(userCreds(&log), 65532, 65532, nil, start, t.Logf)
	if err != nil || inst != nil {
		t.Errorf("no software: BecomeUnprivileged = %v, %v", inst, err)
	}
}

// A non-root user in group root is refused.
func TestBecomeUnprivilegedRefusesGroupRoot(t *testing.T) {
	var log []string
	c := userCreds(&log)
	c.ids.EGID = 0
	if _, err := BecomeUnprivileged(c, 65532, 65532, nil, nil, t.Logf); err == nil {
		t.Error("served with group root")
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

// The real host credentials of the test process are readable, and on
// a platform without the drop the setters refuse.
func TestHostCredsReadable(t *testing.T) {
	ids, err := HostCreds{}.IDs()
	if err != nil {
		t.Fatalf("IDs: %v", err)
	}
	if ids.EUID != os.Geteuid() || ids.EGID != os.Getegid() {
		t.Errorf("IDs = %s, want euid %d egid %d", ids, os.Geteuid(), os.Getegid())
	}
	if _, err := (HostCreds{}).Caps(); err != nil {
		t.Errorf("Caps: %v", err)
	}
}

func TestParseProcStatusCaps(t *testing.T) {
	status := "Name:\tagent-shell\nUid:\t65532\t65532\t65532\t65532\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t00000000a80425fb\nCapBnd:\t00000000a80425fb\nCapAmb:\t0000000000001000\n"
	got, err := ParseProcStatusCaps(status)
	if err != nil || got != (Caps{Eff: 0xa80425fb, Amb: 0x1000}) {
		t.Errorf("ParseProcStatusCaps = %+v, %v", got, err)
	}
	// A kernel without ambient capabilities has no CapAmb line.
	got, err = ParseProcStatusCaps("CapEff:\t0000000000000000\n")
	if err != nil || got != (Caps{}) {
		t.Errorf("without CapAmb = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "Name:\tx\n", "CapEff:\tzz\n"} {
		if _, err := ParseProcStatusCaps(bad); err == nil {
			t.Errorf("ParseProcStatusCaps(%q) succeeded", bad)
		}
	}
}
