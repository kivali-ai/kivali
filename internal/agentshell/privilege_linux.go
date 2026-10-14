//go:build linux

package agentshell

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// HostCreds is the process's own credentials. On Linux, Go's
// syscall.Setgroups, Setgid and Setuid change every thread of the
// process, not only the calling one.
type HostCreds struct{}

// IDs reads the real, effective and saved IDs and the supplementary
// groups.
func (HostCreds) IDs() (IDs, error) {
	var ids IDs
	ids.RUID, ids.EUID, ids.SUID = unix.Getresuid()
	ids.RGID, ids.EGID, ids.SGID = unix.Getresgid()
	groups, err := syscall.Getgroups()
	if err != nil {
		return IDs{}, err
	}
	ids.Groups = groups
	return ids, nil
}

// Setgroups sets the supplementary groups.
func (HostCreds) Setgroups(gids []int) error { return syscall.Setgroups(gids) }

// Setgid sets the real, effective and saved group ID (as root).
func (HostCreds) Setgid(gid int) error { return syscall.Setgid(gid) }

// Setuid sets the real, effective and saved user ID (as root).
func (HostCreds) Setuid(uid int) error { return syscall.Setuid(uid) }

// Caps reads the effective and ambient capability sets from
// /proc/self/status. Capabilities are per thread; Go's Setuid changes
// every thread together, so the calling thread's sets stand for all.
func (HostCreds) Caps() (Caps, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return Caps{}, err
	}
	return ParseProcStatusCaps(string(b))
}
