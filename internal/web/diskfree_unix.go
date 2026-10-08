//go:build unix

package web

import "golang.org/x/sys/unix"

// diskFree is the space an unprivileged writer can still use on the
// filesystem holding path, or -1 when it cannot be read.
func diskFree(path string) int64 {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)) //nolint:gosec // a filesystem's free bytes fit in an int64
}
