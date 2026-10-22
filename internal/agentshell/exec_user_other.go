//go:build !unix

package agentshell

import "os/exec"

// setCommandUser does nothing: a process's uid is a Unix notion, and
// the daemon never drops root elsewhere, so it never asks for one.
func setCommandUser(*exec.Cmd, CommandUser) {}
