package agentshell

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// RunAsDefault is the uid:gid the daemon drops to when it starts as
// root: the image's unprivileged user, the same as the agent
// container's, so both write /files/ as one owner.
const RunAsDefault = "65532:65532"

// IDs is a process's credentials: real, effective and saved user and
// group IDs, and its supplementary groups.
type IDs struct {
	RUID, EUID, SUID int
	RGID, EGID, SGID int
	Groups           []int
}

// String formats ids for a log line.
func (ids IDs) String() string {
	return fmt.Sprintf("uid=%d/%d/%d gid=%d/%d/%d groups=%v", ids.RUID, ids.EUID, ids.SUID, ids.RGID, ids.EGID, ids.SGID, ids.Groups)
}

// Privileged reports whether any of the user IDs is root.
func (ids IDs) Privileged() bool {
	return ids.RUID == 0 || ids.EUID == 0 || ids.SUID == 0
}

// Creds reads and changes the process's credentials. HostCreds is the
// real one; tests fake it.
type Creds interface {
	IDs() (IDs, error)
	Setgroups(gids []int) error
	Setgid(gid int) error
	Setuid(uid int) error
	// Caps reads the process's effective and ambient capability sets.
	Caps() (Caps, error)
}

// Caps is a process's effective and ambient capability sets, as the
// bit masks /proc/<pid>/status shows (CapEff, CapAmb).
type Caps struct {
	Eff, Amb uint64
}

// ParseProcStatusCaps reads CapEff and CapAmb from the text of
// /proc/<pid>/status. CapEff must be there; a kernel without ambient
// capabilities has no CapAmb line, which is an empty set.
func ParseProcStatusCaps(status string) (Caps, error) {
	var c Caps
	sawEff := false
	for _, l := range strings.Split(status, "\n") {
		key, val, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		var dst *uint64
		switch key {
		case "CapEff":
			dst = &c.Eff
			sawEff = true
		case "CapAmb":
			dst = &c.Amb
		default:
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(val), 16, 64)
		if err != nil {
			return Caps{}, fmt.Errorf("%s: %w", key, err)
		}
		*dst = v
	}
	if !sawEff {
		return Caps{}, errors.New("no CapEff line")
	}
	return c, nil
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

// DropPrivileges makes the process uid:gid with no supplementary
// groups, in the only order that works: groups and gid while still
// root, uid last (after it, the process may not change the others).
// On Linux, Go applies each call to every thread of the process.
//
// It then reads the credentials back and returns an error unless every
// user and group ID (real, effective, saved) is the one asked for, and
// there are no supplementary groups. A caller that gets an error must
// not go on: the process may still be root.
func DropPrivileges(c Creds, uid, gid int) error {
	if uid <= 0 || gid <= 0 {
		return fmt.Errorf("drop privileges: refusing uid %d gid %d", uid, gid)
	}
	if err := c.Setgroups([]int{}); err != nil {
		return fmt.Errorf("drop privileges: setgroups: %w", err)
	}
	if err := c.Setgid(gid); err != nil {
		return fmt.Errorf("drop privileges: setgid %d: %w", gid, err)
	}
	if err := c.Setuid(uid); err != nil {
		return fmt.Errorf("drop privileges: setuid %d: %w", uid, err)
	}
	return VerifyUnprivileged(c, uid, gid)
}

// VerifyUnprivileged returns an error unless the process's user and
// group IDs (real, effective, saved) are all uid and gid, none of them
// root, it has no supplementary groups, and its effective and ambient
// capability sets are empty: a process that kept a capability through
// setuid could still act as root in part.
func VerifyUnprivileged(c Creds, uid, gid int) error {
	ids, err := c.IDs()
	if err != nil {
		return fmt.Errorf("verify privileges: %w", err)
	}
	for _, id := range []int{ids.RUID, ids.EUID, ids.SUID} {
		if id != uid || id == 0 {
			return fmt.Errorf("verify privileges: still %s, want uid %d gid %d", ids, uid, gid)
		}
	}
	for _, id := range []int{ids.RGID, ids.EGID, ids.SGID} {
		if id != gid || id == 0 {
			return fmt.Errorf("verify privileges: still %s, want uid %d gid %d", ids, uid, gid)
		}
	}
	if len(ids.Groups) != 0 {
		return fmt.Errorf("verify privileges: still %s, want no supplementary groups", ids)
	}
	caps, err := c.Caps()
	if err != nil {
		return fmt.Errorf("verify privileges: capabilities: %w", err)
	}
	if caps.Eff != 0 || caps.Amb != 0 {
		return fmt.Errorf("verify privileges: capabilities effective=%016x ambient=%016x, want none", caps.Eff, caps.Amb)
	}
	return nil
}

// ErrNoPrivilegeDrop is returned by HostCreds' setters on a platform
// where the daemon does not change credentials.
var ErrNoPrivilegeDrop = errors.New("changing credentials is supported on Linux only")
