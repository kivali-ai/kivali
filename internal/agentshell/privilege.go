package agentshell

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// RunAsDefault is the uid:gid every command the daemon runs has: the
// image's kivali user, the same as the agent container's, so both
// write /files/ as one owner. It is also the daemon's group and its
// filesystem uid, so the daemon's own file access is that user's too.
const RunAsDefault = "65532:65532"

// DaemonUIDDefault is the uid the daemon itself runs as once it has
// left root: the image's agent-shell user, which no command runs as.
const DaemonUIDDefault = 65533

// The capabilities the daemon keeps (linux/capability.h numbers).
const (
	capKill   = 5
	capSetgid = 6
	capSetuid = 7
)

// DaemonCaps is the daemon's permitted, effective and bounding
// capability set once it has dropped root: CAP_SETUID, to start each
// command as the commands' uid and to set its own filesystem uid;
// CAP_SETGID, for a command's gid and groups; CAP_KILL, to kill a
// command that ran out of time, which the kernel otherwise refuses
// across uids. 0xe0.
const DaemonCaps uint64 = 1<<capKill | 1<<capSetgid | 1<<capSetuid

// Identity is who the daemon becomes when it starts as root.
type Identity struct {
	// DaemonUID is the daemon's real, effective and saved uid.
	DaemonUID int
	// UID and GID are every command's uid and gid. GID is also the
	// daemon's real, effective, saved and filesystem gid, and UID its
	// filesystem uid: the daemon reads and writes files as the
	// commands' user, and the files it creates are that user's.
	UID, GID int
}

// IDs is a process's credentials: real, effective, saved and
// filesystem user and group IDs, and its supplementary groups.
type IDs struct {
	RUID, EUID, SUID, FSUID int
	RGID, EGID, SGID, FSGID int
	Groups                  []int
	// NoNewPrivs is the no_new_privs flag: set, an exec cannot gain
	// privileges (a setuid or file-capability binary).
	NoNewPrivs bool
}

// String formats ids for a log line.
func (ids IDs) String() string {
	return fmt.Sprintf("uid=%d/%d/%d/%d gid=%d/%d/%d/%d groups=%v no_new_privs=%t",
		ids.RUID, ids.EUID, ids.SUID, ids.FSUID, ids.RGID, ids.EGID, ids.SGID, ids.FSGID, ids.Groups, ids.NoNewPrivs)
}

// Privileged reports whether any of the user IDs is root.
func (ids IDs) Privileged() bool {
	return ids.RUID == 0 || ids.EUID == 0 || ids.SUID == 0 || ids.FSUID == 0
}

// Caps is a process's capability sets, as the bit masks
// /proc/<pid>/status shows them.
type Caps struct {
	Inh, Prm, Eff, Bnd, Amb uint64
}

// String formats caps for a log line.
func (c Caps) String() string {
	return fmt.Sprintf("inh=%016x prm=%016x eff=%016x bnd=%016x amb=%016x", c.Inh, c.Prm, c.Eff, c.Bnd, c.Amb)
}

// Creds reads and changes the process's credentials. HostCreds is the
// real one; tests fake it. Every setter changes every thread of the
// process.
type Creds interface {
	IDs() (IDs, error)
	Caps() (Caps, error)
	// CapLastCap is the highest capability number the kernel knows.
	CapLastCap() (int, error)
	// DropBoundingCap removes capability from the bounding set
	// (PR_CAPBSET_DROP; needs CAP_SETPCAP in the effective set).
	DropBoundingCap(capability int) error
	// SetNoNewPrivs sets no_new_privs (PR_SET_NO_NEW_PRIVS, 1), which
	// the process and everything it starts keep for good. Setting it
	// when it is already set is not an error.
	SetNoNewPrivs() error
	// SetKeepCaps sets or clears PR_SET_KEEPCAPS: with it set, the
	// permitted set survives a change from uid 0 to nonzero uids.
	SetKeepCaps(keep bool) error
	Setresgid(rgid, egid, sgid int) error
	Setgroups(gids []int) error
	Setresuid(ruid, euid, suid int) error
	// Capset sets the permitted, effective and inheritable sets to
	// c.Prm, c.Eff and c.Inh; the bounding and ambient sets are not
	// capset's.
	Capset(c Caps) error
	Setfsuid(uid int) error
}

// ParseProcStatus reads the credentials and capability sets from the
// text of /proc/<pid>/status: the Uid and Gid lines (real, effective,
// saved, filesystem), Groups, NoNewPrivs, and CapInh, CapPrm, CapEff
// and CapBnd, which must all be there. A kernel without ambient capabilities has no
// CapAmb line, which is an empty set.
func ParseProcStatus(status string) (IDs, Caps, error) {
	var ids IDs
	var c Caps
	seen := map[string]bool{}
	for _, l := range strings.Split(status, "\n") {
		key, val, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Uid", "Gid":
			f := strings.Fields(val)
			if len(f) != 4 {
				return IDs{}, Caps{}, fmt.Errorf("%s: want four IDs, got %q", key, val)
			}
			var n [4]int
			for i, s := range f {
				v, err := strconv.Atoi(s)
				if err != nil {
					return IDs{}, Caps{}, fmt.Errorf("%s: %w", key, err)
				}
				n[i] = v
			}
			if key == "Uid" {
				ids.RUID, ids.EUID, ids.SUID, ids.FSUID = n[0], n[1], n[2], n[3]
			} else {
				ids.RGID, ids.EGID, ids.SGID, ids.FSGID = n[0], n[1], n[2], n[3]
			}
		case "Groups":
			ids.Groups = []int{}
			for _, s := range strings.Fields(val) {
				v, err := strconv.Atoi(s)
				if err != nil {
					return IDs{}, Caps{}, fmt.Errorf("%s: %w", key, err)
				}
				ids.Groups = append(ids.Groups, v)
			}
		case "NoNewPrivs":
			v, err := strconv.Atoi(val)
			if err != nil {
				return IDs{}, Caps{}, fmt.Errorf("%s: %w", key, err)
			}
			ids.NoNewPrivs = v != 0
		case "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb":
			v, err := strconv.ParseUint(val, 16, 64)
			if err != nil {
				return IDs{}, Caps{}, fmt.Errorf("%s: %w", key, err)
			}
			switch key {
			case "CapInh":
				c.Inh = v
			case "CapPrm":
				c.Prm = v
			case "CapEff":
				c.Eff = v
			case "CapBnd":
				c.Bnd = v
			case "CapAmb":
				c.Amb = v
			}
		default:
			continue
		}
		seen[key] = true
	}
	for _, k := range []string{"Uid", "Gid", "Groups", "NoNewPrivs", "CapInh", "CapPrm", "CapEff", "CapBnd"} {
		if !seen[k] {
			return IDs{}, Caps{}, fmt.Errorf("no %s line", k)
		}
	}
	return ids, c, nil
}

// ParseRunAs parses a "uid:gid" pair. Both must be positive: the point
// is to run as someone other than root.
func ParseRunAs(s string) (uid, gid int, err error) {
	u, g, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("run-as %q: want uid:gid", s)
	}
	uid, uerr := strconv.Atoi(u)
	gid, gerr := strconv.Atoi(g)
	if uerr != nil || gerr != nil || uid <= 0 || gid <= 0 {
		return 0, 0, fmt.Errorf("run-as %q: want two positive numbers, uid:gid", s)
	}
	return uid, gid, nil
}

// DropPrivileges makes the process, started as root, id.DaemonUID with
// group id.GID, no supplementary groups, filesystem uid id.UID, and
// DaemonCaps alone, with no_new_privs set. The order is the only one
// that works:
//
//  1. every capability but DaemonCaps leaves the bounding set, while
//     the process still has CAP_SETPCAP (only those the set holds: a
//     container started with DaemonCaps alone has nothing to drop);
//  2. no_new_privs, which the daemon sets itself so that it does not
//     depend on the pod spec alone: a command that execs a setuid-root
//     binary from the image would otherwise get uid 0 with the
//     daemon's bounding set. Set here, every later step and every
//     child has it;
//  3. PR_SET_KEEPCAPS, so the permitted set survives the uid change;
//  4. setresgid and setgroups([]), while still root;
//  5. setresuid to id.DaemonUID, which empties the effective set, and
//     PR_SET_KEEPCAPS cleared again;
//  6. capset: permitted and effective DaemonCaps, inheritable empty;
//  7. setfsuid(id.UID), which needs CAP_SETUID.
//
// It then reads the credentials back (VerifyDaemon). A caller that gets
// an error must not go on: the process may still be root.
func DropPrivileges(c Creds, id Identity) error {
	if id.DaemonUID <= 0 || id.UID <= 0 || id.GID <= 0 || id.DaemonUID == id.UID {
		return fmt.Errorf("drop privileges: refusing daemon uid %d, command uid %d gid %d", id.DaemonUID, id.UID, id.GID)
	}
	caps, err := c.Caps()
	if err != nil {
		return fmt.Errorf("drop privileges: read capabilities: %w", err)
	}
	last, err := c.CapLastCap()
	if err != nil {
		return fmt.Errorf("drop privileges: cap_last_cap: %w", err)
	}
	if last < 0 || last > 63 {
		return fmt.Errorf("drop privileges: cap_last_cap %d out of range", last)
	}
	for capability := 0; capability <= last; capability++ {
		bit := uint64(1) << capability
		if caps.Bnd&bit == 0 || DaemonCaps&bit != 0 {
			continue
		}
		if err := c.DropBoundingCap(capability); err != nil {
			return fmt.Errorf("drop privileges: drop capability %d from the bounding set: %w", capability, err)
		}
	}
	if err := c.SetNoNewPrivs(); err != nil {
		return fmt.Errorf("drop privileges: set no_new_privs: %w", err)
	}
	if err := c.SetKeepCaps(true); err != nil {
		return fmt.Errorf("drop privileges: set keepcaps: %w", err)
	}
	if err := c.Setresgid(id.GID, id.GID, id.GID); err != nil {
		return fmt.Errorf("drop privileges: setresgid %d: %w", id.GID, err)
	}
	if err := c.Setgroups([]int{}); err != nil {
		return fmt.Errorf("drop privileges: setgroups: %w", err)
	}
	if err := c.Setresuid(id.DaemonUID, id.DaemonUID, id.DaemonUID); err != nil {
		return fmt.Errorf("drop privileges: setresuid %d: %w", id.DaemonUID, err)
	}
	if err := c.SetKeepCaps(false); err != nil {
		return fmt.Errorf("drop privileges: clear keepcaps: %w", err)
	}
	if err := c.Capset(Caps{Prm: DaemonCaps, Eff: DaemonCaps}); err != nil {
		return fmt.Errorf("drop privileges: capset %016x: %w", DaemonCaps, err)
	}
	if err := c.Setfsuid(id.UID); err != nil {
		return fmt.Errorf("drop privileges: setfsuid %d: %w", id.UID, err)
	}
	return VerifyDaemon(c, id)
}

// VerifyDaemon returns an error unless the process's real, effective
// and saved uid are id.DaemonUID and its filesystem uid id.UID; its
// real, effective, saved and filesystem gid id.GID; it has no
// supplementary groups; its permitted, effective and bounding sets are
// exactly DaemonCaps; its inheritable and ambient sets are empty; and
// no_new_privs is set.
func VerifyDaemon(c Creds, id Identity) error {
	ids, err := c.IDs()
	if err != nil {
		return fmt.Errorf("verify privileges: %w", err)
	}
	want := fmt.Sprintf("want uid %d/%d/%d/%d gid %d/%d/%d/%d", id.DaemonUID, id.DaemonUID, id.DaemonUID, id.UID, id.GID, id.GID, id.GID, id.GID)
	if ids.RUID != id.DaemonUID || ids.EUID != id.DaemonUID || ids.SUID != id.DaemonUID || ids.FSUID != id.UID {
		return fmt.Errorf("verify privileges: %s, %s", ids, want)
	}
	if ids.RGID != id.GID || ids.EGID != id.GID || ids.SGID != id.GID || ids.FSGID != id.GID {
		return fmt.Errorf("verify privileges: %s, %s", ids, want)
	}
	if ids.Privileged() || id.GID == 0 {
		return fmt.Errorf("verify privileges: %s is root", ids)
	}
	if len(ids.Groups) != 0 {
		return fmt.Errorf("verify privileges: %s, want no supplementary groups", ids)
	}
	if !ids.NoNewPrivs {
		return fmt.Errorf("verify privileges: %s, want no_new_privs set", ids)
	}
	caps, err := c.Caps()
	if err != nil {
		return fmt.Errorf("verify privileges: capabilities: %w", err)
	}
	if want := (Caps{Prm: DaemonCaps, Eff: DaemonCaps, Bnd: DaemonCaps}); caps != want {
		return fmt.Errorf("verify privileges: capabilities %s, want %s", caps, want)
	}
	return nil
}

// LeaveRoot makes the process, started as root, uid and gid for good,
// with nothing of root left: no capability in any set, no supplementary
// groups and no_new_privs set, so neither it nor anything it starts can
// gain one back. An integration's shim calls it after installing the
// integration's software and before it starts the integration's server.
// The order:
//
//  1. every capability leaves the bounding set, while the process still
//     has CAP_SETPCAP (only those the set holds);
//  2. no_new_privs;
//  3. PR_SET_KEEPCAPS cleared, so the uid change empties the permitted
//     and effective sets;
//  4. setresgid and setgroups([]), while still root;
//  5. setresuid to uid;
//  6. capset to nothing, which empties the inheritable set too.
//
// It then reads the credentials back (VerifyUser). A caller that gets
// an error must not go on: the process may still be root.
func LeaveRoot(c Creds, uid, gid int) error {
	if uid <= 0 || gid <= 0 {
		return fmt.Errorf("leave root: refusing uid %d gid %d", uid, gid)
	}
	caps, err := c.Caps()
	if err != nil {
		return fmt.Errorf("leave root: read capabilities: %w", err)
	}
	last, err := c.CapLastCap()
	if err != nil {
		return fmt.Errorf("leave root: cap_last_cap: %w", err)
	}
	if last < 0 || last > 63 {
		return fmt.Errorf("leave root: cap_last_cap %d out of range", last)
	}
	for capability := 0; capability <= last; capability++ {
		if caps.Bnd&(uint64(1)<<capability) == 0 {
			continue
		}
		if err := c.DropBoundingCap(capability); err != nil {
			return fmt.Errorf("leave root: drop capability %d from the bounding set: %w", capability, err)
		}
	}
	if err := c.SetNoNewPrivs(); err != nil {
		return fmt.Errorf("leave root: set no_new_privs: %w", err)
	}
	if err := c.SetKeepCaps(false); err != nil {
		return fmt.Errorf("leave root: clear keepcaps: %w", err)
	}
	if err := c.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("leave root: setresgid %d: %w", gid, err)
	}
	if err := c.Setgroups([]int{}); err != nil {
		return fmt.Errorf("leave root: setgroups: %w", err)
	}
	if err := c.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("leave root: setresuid %d: %w", uid, err)
	}
	if err := c.Capset(Caps{}); err != nil {
		return fmt.Errorf("leave root: capset: %w", err)
	}
	return VerifyUser(c, uid, gid)
}

// VerifyUser returns an error unless the process's real, effective,
// saved and filesystem uid are uid and its gids gid; it has no
// supplementary groups; every capability set, the bounding and ambient
// sets included, is empty; and no_new_privs is set.
func VerifyUser(c Creds, uid, gid int) error {
	ids, err := c.IDs()
	if err != nil {
		return fmt.Errorf("verify privileges: %w", err)
	}
	if ids.RUID != uid || ids.EUID != uid || ids.SUID != uid || ids.FSUID != uid ||
		ids.RGID != gid || ids.EGID != gid || ids.SGID != gid || ids.FSGID != gid {
		return fmt.Errorf("verify privileges: %s, want uid %d gid %d throughout", ids, uid, gid)
	}
	if ids.Privileged() || gid == 0 {
		return fmt.Errorf("verify privileges: %s is root", ids)
	}
	if len(ids.Groups) != 0 {
		return fmt.Errorf("verify privileges: %s, want no supplementary groups", ids)
	}
	if !ids.NoNewPrivs {
		return fmt.Errorf("verify privileges: %s, want no_new_privs set", ids)
	}
	caps, err := c.Caps()
	if err != nil {
		return fmt.Errorf("verify privileges: capabilities: %w", err)
	}
	if caps != (Caps{}) {
		return fmt.Errorf("verify privileges: capabilities %s, want none", caps)
	}
	return nil
}

// AsDaemonUser runs f with the filesystem uid id.DaemonUID, so a file f
// creates is the daemon's own, then sets it back to id.UID and checks
// it. Nothing else may touch files meanwhile: the filesystem uid is the
// whole process's.
func AsDaemonUser(c Creds, id Identity, f func() error) error {
	if err := c.Setfsuid(id.DaemonUID); err != nil {
		return fmt.Errorf("setfsuid %d: %w", id.DaemonUID, err)
	}
	ferr := f()
	if err := c.Setfsuid(id.UID); err != nil {
		return fmt.Errorf("setfsuid %d: %w", id.UID, err)
	}
	if err := VerifyDaemon(c, id); err != nil {
		return err
	}
	return ferr
}

// ErrNoPrivilegeDrop is returned by HostCreds' setters on a platform
// where the daemon does not change credentials.
var ErrNoPrivilegeDrop = errors.New("changing credentials is supported on Linux only")
