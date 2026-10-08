//go:build !unix

package store

import "io/fs"

// fileIno is 0 where the platform gives no inode: size and mtime
// alone decide whether a file moved.
func fileIno(fs.FileInfo) uint64 { return 0 }
