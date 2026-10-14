//go:build unix

package agentshell

import "golang.org/x/sys/unix"

// closeOnExec marks fd close-on-exec. It fails when fd is not open.
func closeOnExec(fd int) error {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	return err
}
