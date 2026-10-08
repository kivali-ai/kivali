//go:build linux

package files

import (
	"os"
	"path"
	"syscall"
)

// renameIn renames oldRel to newRel, both relative to root, without a
// path lookup that could leave root. Each parent directory is opened
// through root (so a symlink along the way stays inside it) and the
// rename is renameat(2) on the two directory handles with plain
// names, so the entries themselves are never followed: a symlink at
// oldRel moves as a symlink, one at newRel is replaced.
//
// os.Root has Rename from Go 1.25; the module targets 1.24.
func renameIn(root *os.Root, oldRel, newRel string) error {
	return renameAcross(root, oldRel, root, newRel)
}

// renameAcross is renameIn between two roots on one filesystem: oldRel
// beneath from moves to newRel beneath to. Each parent is opened
// through its own root, so neither lookup can leave its root.
func renameAcross(from *os.Root, oldRel string, to *os.Root, newRel string) error {
	od, err := from.Open(path.Dir(oldRel))
	if err != nil {
		return err
	}
	defer func() { _ = od.Close() }()
	nd, err := to.Open(path.Dir(newRel))
	if err != nil {
		return err
	}
	defer func() { _ = nd.Close() }()
	if err := syscall.Renameat(int(od.Fd()), path.Base(oldRel), int(nd.Fd()), path.Base(newRel)); err != nil {
		return &os.LinkError{Op: "rename", Old: oldRel, New: newRel, Err: err}
	}
	return nil
}
