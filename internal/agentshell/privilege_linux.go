//go:build linux

package agentshell

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// HostCreds is the process's own credentials.
//
// Credentials, capability sets, securebits (keepcaps) and the
// filesystem uid are per thread on Linux, and a Go process has many
// threads. Every setter therefore runs on every thread of the process:
// syscall.Setresgid, Setgroups and Setresuid do so themselves, and the
// rest go through syscall.AllThreadsSyscall, which stops the world and
// makes the call on each thread. A thread started later copies the one
// that starts it. AllThreadsSyscall returns ENOTSUP in a binary linked
// with cgo; the agent-shell is built with CGO_ENABLED=0, and a setter
// that cannot reach every thread fails rather than change one.
type HostCreds struct{}

// IDs reads the real, effective, saved and filesystem IDs and the
// supplementary groups from /proc/self/status.
func (HostCreds) IDs() (IDs, error) {
	ids, _, err := readProcStatus()
	return ids, err
}

// Caps reads the capability sets from /proc/self/status. The setters
// change every thread together, so the main thread's sets stand for
// all.
func (HostCreds) Caps() (Caps, error) {
	_, c, err := readProcStatus()
	return c, err
}

func readProcStatus() (IDs, Caps, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return IDs{}, Caps{}, err
	}
	return ParseProcStatus(string(b))
}

// CapLastCap reads /proc/sys/kernel/cap_last_cap.
func (HostCreds) CapLastCap() (int, error) {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// DropBoundingCap is PR_CAPBSET_DROP on every thread.
func (HostCreds) DropBoundingCap(capability int) error {
	return allThreadsPrctl(unix.PR_CAPBSET_DROP, uintptr(capability))
}

// SetKeepCaps is PR_SET_KEEPCAPS on every thread.
func (HostCreds) SetKeepCaps(keep bool) error {
	var v uintptr
	if keep {
		v = 1
	}
	return allThreadsPrctl(unix.PR_SET_KEEPCAPS, v)
}

// Setresgid sets the real, effective and saved group ID.
func (HostCreds) Setresgid(rgid, egid, sgid int) error { return syscall.Setresgid(rgid, egid, sgid) }

// Setgroups sets the supplementary groups.
func (HostCreds) Setgroups(gids []int) error { return syscall.Setgroups(gids) }

// Setresuid sets the real, effective and saved user ID.
func (HostCreds) Setresuid(ruid, euid, suid int) error { return syscall.Setresuid(ruid, euid, suid) }

// Capset sets the permitted, effective and inheritable sets of every
// thread (capset(2) with pid 0 is the calling thread's).
func (HostCreds) Capset(c Caps) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{
		{Effective: uint32(c.Eff), Permitted: uint32(c.Prm), Inheritable: uint32(c.Inh)},
		{Effective: uint32(c.Eff >> 32), Permitted: uint32(c.Prm >> 32), Inheritable: uint32(c.Inh >> 32)},
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return e
	}
	return nil
}

// Setfsuid sets the filesystem uid of every thread. setfsuid(2) reports
// no error: it returns the previous filesystem uid whether or not it
// changed it. So it asks again with an invalid uid, which changes
// nothing and returns the current one (the same on every thread, or
// AllThreadsSyscall ends the process), and fails unless that is uid.
func (HostCreds) Setfsuid(uid int) error {
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_SETFSUID, uintptr(uid), 0, 0); e != 0 {
		return e
	}
	cur, _, e := syscall.AllThreadsSyscall(syscall.SYS_SETFSUID, uintptr(^uint32(0)), 0, 0)
	if e != 0 {
		return e
	}
	if int(cur) != uid {
		return fmt.Errorf("filesystem uid is %d", cur)
	}
	return nil
}

func allThreadsPrctl(option int, arg uintptr) error {
	if _, _, e := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, uintptr(option), arg, 0, 0, 0, 0); e != 0 {
		return e
	}
	return nil
}
