//go:build linux

package agentshell

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// BecomeSubreaper makes the calling process the reaper of its orphaned
// descendants: a process apt-get or a package's maintainer script
// leaves running reparents to it, not to the daemon.
func BecomeSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// KillDescendants kills every process left under this one and reaps
// it, until none is left. The install child calls it last, so no
// process it started outlives it: nothing keeps running as root once
// the install is over.
func KillDescendants() {
	self := os.Getpid()
	for {
		killed := 0
		for _, pid := range childrenOf(self) {
			if unix.Kill(pid, unix.SIGKILL) == nil {
				killed++
			}
		}
		flags := 0
		if killed == 0 {
			// Nothing seen to wait for: reap what has already exited,
			// never block on a child /proc did not show.
			flags = unix.WNOHANG
		}
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, flags, nil)
		switch {
		case errors.Is(err, unix.EINTR):
		case err != nil: // ECHILD: no children left
			return
		case pid == 0: // WNOHANG with live children not in /proc's list
			return
		}
	}
}

// childrenOf lists the processes whose parent is ppid, from /proc.
func childrenOf(ppid int) []int {
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	var out []int
	for _, p := range stats {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...: comm may hold spaces and parens,
		// so the fields after it start past the last ')'.
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 2 {
			continue
		}
		if pp, err := strconv.Atoi(f[1]); err == nil && pp == ppid {
			if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(p))); err == nil {
				out = append(out, pid)
			}
		}
	}
	return out
}
