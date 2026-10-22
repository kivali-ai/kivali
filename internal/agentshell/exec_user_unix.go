//go:build unix

package agentshell

import (
	"os/exec"
	"syscall"
)

// setCommandUser makes cmd start as u, with no supplementary groups.
func setCommandUser(cmd *exec.Cmd, u CommandUser) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: []uint32{}}
}
