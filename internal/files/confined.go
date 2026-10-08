package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Core reads and writes inside trees an agent can fill with anything.
// run_shell can plant a symlink at any path under the agent's /files/,
// and on core — which mounts the whole data volume — following that
// link reaches every other agent's files and the org's Claude
// credentials. The helpers here are how core opens a path an agent
// named, without ever following a link the agent may have planted.
//
// Two rules, both enforced on the handle actually opened rather than
// on a separate check of the path:
//
//   - confinement: every open goes through an os.Root, so nothing the
//     open resolves can leave the directory it started from;
//   - no symlinks: every component is Lstat'd before it is opened, and
//     the opened handle is compared with that Lstat (os.SameFile). A
//     link swapped in between the two opens something else, and the
//     comparison refuses it. The only thing ever returned is the inode
//     that was a real directory or regular file at that name.

// ErrSymlink is returned (wrapped, with the offending path) when a
// component of a path handed to OpenNoFollow or OpenDirNoFollow is a
// symbolic link.
var ErrSymlink = errors.New("is a symbolic link")

// ErrNotRegular is returned (wrapped) when the path names something
// other than a regular file: a directory, a FIFO, a device, a socket.
var ErrNotRegular = errors.New("is not a regular file")

// ErrNotDir is returned (wrapped) when a component before the last is
// something other than a directory.
var ErrNotDir = errors.New("is not a directory")

// ErrRaced is returned when what was opened is not what was Lstat'd
// a moment before: the entry was swapped mid-open, which is how a
// symlink swap is staged.
var ErrRaced = errors.New("changed while it was being opened")

// PathError names the path a no-follow open refused and why. Err is
// one of the sentinels above or the underlying filesystem error, so
// callers can errors.Is on it and still print the path.
type PathError struct {
	Path string // slash-separated, relative to the directory opened
	Err  error
}

func (e *PathError) Error() string { return e.Path + " " + e.Err.Error() }
func (e *PathError) Unwrap() error { return e.Err }

// OpenNoFollow opens the regular file named by rel beneath dir for
// reading. rel is slash- or OS-separated and relative; it may not
// climb out with "..". Every component of rel must be a real
// directory, and the last a regular file — a symbolic link anywhere is
// an error wrapping ErrSymlink, a directory or special file one
// wrapping ErrNotRegular. dir itself is trusted: it is a path core
// built, not one the agent named.
func OpenNoFollow(dir, rel string) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return OpenNoFollowIn(root, rel)
}

// OpenNoFollowIn is OpenNoFollow against an already open root.
func OpenNoFollowIn(root *os.Root, rel string) (*os.File, error) {
	parts, err := splitRel(rel)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, &PathError{Path: ".", Err: ErrNotRegular}
	}
	parent, err := walkDirsNoFollow(root, parts[:len(parts)-1])
	if err != nil {
		return nil, err
	}
	if parent != root {
		defer func() { _ = parent.Close() }()
	}
	name := parts[len(parts)-1]
	shown := strings.Join(parts, "/")
	li, err := parent.Lstat(name)
	if err != nil {
		return nil, &PathError{Path: shown, Err: err}
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, &PathError{Path: shown, Err: ErrSymlink}
	}
	if !li.Mode().IsRegular() {
		return nil, &PathError{Path: shown, Err: ErrNotRegular}
	}
	// O_NONBLOCK so a FIFO swapped in after the Lstat cannot hang the
	// open waiting for a writer; the SameFile check below refuses it.
	// On a regular file the flag changes nothing.
	f, err := parent.OpenFile(name, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, &PathError{Path: shown, Err: err}
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, &PathError{Path: shown, Err: err}
	}
	if !os.SameFile(li, fi) {
		_ = f.Close()
		return nil, &PathError{Path: shown, Err: ErrRaced}
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, &PathError{Path: shown, Err: ErrNotRegular}
	}
	return f, nil
}

// ReadFileNoFollow reads the whole regular file OpenNoFollow opens.
func ReadFileNoFollow(dir, rel string) ([]byte, error) {
	f, err := OpenNoFollow(dir, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// OpenDirNoFollow opens the directory named by rel beneath dir as an
// os.Root, refusing a symbolic link at any component. Anything done
// through the returned root is confined to that directory, so a
// caller that walks or writes a tree an agent controls opens the top
// of it here and never builds a host path under it again. rel may be
// "" or "." for dir itself.
func OpenDirNoFollow(dir, rel string) (*os.Root, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	parts, err := splitRel(rel)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	sub, err := walkDirsNoFollow(root, parts)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if sub != root {
		_ = root.Close()
	}
	return sub, nil
}

// walkDirsNoFollow descends from root through each of parts, each of
// which must be a real directory. Returns root itself for no parts.
// Intermediate roots are closed as the walk moves past them; the
// caller owns (and closes) the one returned when it is not root.
func walkDirsNoFollow(root *os.Root, parts []string) (*os.Root, error) {
	cur := root
	for i, name := range parts {
		shown := strings.Join(parts[:i+1], "/")
		next, err := openSubdirNoFollow(cur, name, shown)
		if cur != root {
			_ = cur.Close()
		}
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// openSubdirNoFollow opens one real child directory of r. The Lstat
// says it is a directory and not a link; the SameFile check says the
// directory opened is that one. os.Root keeps even a racing swap
// inside r.
func openSubdirNoFollow(r *os.Root, name, shown string) (*os.Root, error) {
	li, err := r.Lstat(name)
	if err != nil {
		return nil, &PathError{Path: shown, Err: err}
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return nil, &PathError{Path: shown, Err: ErrSymlink}
	}
	if !li.IsDir() {
		return nil, &PathError{Path: shown, Err: ErrNotDir}
	}
	sub, err := r.OpenRoot(name)
	if err != nil {
		return nil, &PathError{Path: shown, Err: err}
	}
	si, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, &PathError{Path: shown, Err: err}
	}
	if !os.SameFile(li, si) {
		_ = sub.Close()
		return nil, &PathError{Path: shown, Err: ErrRaced}
	}
	return sub, nil
}

// splitRel cleans rel and splits it into components, refusing an
// absolute path or one that climbs out with "..".
func splitRel(rel string) ([]string, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || clean == "" {
		return nil, nil
	}
	if strings.HasPrefix(clean, "/") {
		return nil, fmt.Errorf("%s: path must be relative", rel)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("%s: path escapes its root", rel)
	}
	return strings.Split(clean, "/"), nil
}
