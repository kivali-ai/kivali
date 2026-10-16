//go:build unix

package browserpod

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own, so Kill
// reaches Chromium's helpers and, under xvfb-run, Chromium itself.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
