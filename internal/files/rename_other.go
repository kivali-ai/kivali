//go:build !linux

package files

import (
	"os"
	"path/filepath"
)

// renameIn renames oldRel to newRel, both relative to root. Off Linux
// (developer machines; every agent pod runs Linux) there is no
// renameat in package syscall, so this falls back to a rename by path:
// the parents are resolved again by the kernel and a symlink swapped
// into one between the caller's checks and this call is followed. The
// Linux build does not have that gap.
func renameIn(root *os.Root, oldRel, newRel string) error {
	return renameAcross(root, oldRel, root, newRel)
}

// renameAcross is renameIn between two roots on one filesystem, with
// the same fallback to a rename by path.
func renameAcross(from *os.Root, oldRel string, to *os.Root, newRel string) error {
	return os.Rename(filepath.Join(from.Name(), filepath.FromSlash(oldRel)), filepath.Join(to.Name(), filepath.FromSlash(newRel)))
}
