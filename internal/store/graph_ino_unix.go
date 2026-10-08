//go:build unix

package store

import (
	"io/fs"
	"syscall"
)

// fileIno is info's inode, or 0 when the platform does not say.
func fileIno(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}
