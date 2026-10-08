package files

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// Where the backend's reads and writes may land.
//
// Resolve and the read-only prefix rule are lexical: they look at the
// path the model typed. They say nothing about where the bytes are,
// because run_shell can plant a symlink anywhere under /files/, and
// core, which reads body_path through this backend, mounts the whole
// data volume — the org's Claude credentials among it. So every read
// and write also checks the real location of what it touches:
//
//   - a read may land in Root, a WriteRoot, or a ReadRoot (or in a
//     ReadAliases key, and is then served from its value), and nowhere
//     else; anywhere else reads as not found. The path is resolved
//     with EvalSymlinks, the base it lands in is opened as an os.Root,
//     and the file is opened through that root, so a link swapped
//     after the check still cannot leave the base.
//   - a write goes through an os.Root opened on Root (or on the
//     WriteRoot that contains it), so no symlink the write follows can
//     take it outside that base. Inside the base, the real directory
//     the write lands in must not be one of Root's read-only subtrees.
//
// That last check is the one residual race: it resolves the directory,
// then writes, and a link swapped in between can steer the write into
// a read-only subtree of the same /files/ tree — never outside it.
// Those subtrees are core-managed symlink farms that the agent pod
// mounts read-only (see agentpod's manifest), so the kernel refuses
// such a write whatever this check concludes; the check is for the
// paths where no such mount exists (tests, core-side dispatch).

// errOutsideRoots is returned when a path resolves somewhere the
// backend may not touch.
func errOutsideRoots(modelPath string) error {
	return fmt.Errorf("path %s resolves outside the directories file tools may reach (a symbolic link to somewhere else?)", modelPath)
}

// realPath evaluates every symlink in p. A path that does not exist
// yet resolves its longest existing ancestor and appends the rest, so
// a directory a write is about to create still has a real location to
// check.
func realPath(p string) (string, error) {
	p = filepath.Clean(p)
	var rest []string
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		rest = append(rest, filepath.Base(p))
		p = parent
	}
}

// within reports whether p is base or beneath it.
func within(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+string(filepath.Separator))
}

// realBases evaluates each directory's real path, skipping any that do
// not exist (a subagent overlay has no past-chats/, a fixture may have
// no project_files/).
func realBases(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" {
			continue
		}
		r, err := filepath.EvalSymlinks(d)
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// readBases lists, as real paths, every directory a read may land in.
func (b *Backend) readBases() []string {
	dirs := append([]string{b.Root}, b.WriteRoots...)
	return realBases(append(append(dirs, b.ReadRoots...), b.mountHosts()...))
}

// mountHosts lists the host directories of b.Mounts.
func (b *Backend) mountHosts() []string {
	out := make([]string, 0, len(b.Mounts))
	for _, host := range b.Mounts {
		out = append(out, host)
	}
	return out
}

// isPublishedReal reports whether real (a resolved path) lies in one of
// the published trees as this Backend sees them: under Root or a write
// root, or behind a Mounts entry for one.
func (b *Backend) isPublishedReal(real string) bool {
	var pub []string
	for _, d := range PublishedMountPoints {
		for _, base := range append([]string{b.Root}, b.WriteRoots...) {
			pub = append(pub, filepath.Join(base, filepath.FromSlash(d)))
		}
		if host, ok := b.Mounts[d]; ok {
			pub = append(pub, host)
		}
	}
	for _, r := range realBases(pub) {
		if within(real, r) {
			return true
		}
	}
	return false
}

// readBaseFor returns the read base real (a resolved path) lies in and
// real relative to it, or "" when it lies in none. A path in one of
// ReadAliases' keys is served from the same relative path under the
// key's value instead.
func (b *Backend) readBaseFor(real string) (string, string) {
	for from, to := range b.ReadAliases {
		rf, err := filepath.EvalSymlinks(from)
		if err != nil || !within(real, rf) {
			continue
		}
		rt, err := filepath.EvalSymlinks(to)
		if err != nil {
			return "", ""
		}
		rel, err := filepath.Rel(rf, real)
		if err != nil {
			return "", ""
		}
		return rt, rel
	}
	for _, rb := range b.readBases() {
		if within(real, rb) {
			rel, err := filepath.Rel(rb, real)
			if err != nil {
				return "", ""
			}
			return rb, rel
		}
	}
	return "", ""
}

// openRead opens the file or directory abs names (a path Resolve
// produced) for reading, following symlinks only as far as the read
// bases. Returns the open handle and its fstat. A FIFO, device or
// socket is refused: file_view reads files and lists directories.
//
// A path that does not resolve, and one that resolves outside the read
// bases, are both ErrNotFound. Telling them apart would answer, for
// any path a link can name, whether it exists: core reads body_path
// through this backend with the whole data volume mounted.
func (b *Backend) openRead(abs, modelPath string) (*os.File, os.FileInfo, error) {
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	base, rel := b.readBaseFor(real)
	if base == "" {
		return nil, nil, ErrNotFound
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(rel, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("%s: %w", modelPath, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file or directory", modelPath)
	}
	return f, info, nil
}

// readFile reads the regular file abs names through openRead.
func (b *Backend) readFile(abs, modelPath string) ([]byte, error) {
	f, info, err := b.openRead(abs, modelPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", modelPath)
	}
	return io.ReadAll(f)
}

// writeBase picks the directory a write to abs goes through: the
// outermost of Root and the WriteRoots that lexically contains abs. A
// subagent's Root sits inside its parent's /files/, and its
// background/ link climbs out of Root into that parent tree, so the
// write is confined to the parent tree rather than Root.
//
// The choice is made on real paths: dir (abs's parent) is resolved,
// and the base is the outermost real write root containing it.
// Returns the base and abs relative to it, slash-separated, through
// the resolved directory — so the os.Root ops that follow meet no
// link at all unless one is swapped in meanwhile, and an absolute link
// that stays inside /files/ (which os.Root would refuse to follow)
// still works.
func (b *Backend) writeBase(abs, modelPath string) (string, string, error) {
	realDir, err := realPath(filepath.Dir(abs))
	if err != nil {
		return "", "", err
	}
	base := ""
	for _, w := range realBases(append([]string{b.Root}, b.WriteRoots...)) {
		if within(realDir, w) && (base == "" || len(w) < len(base)) {
			base = w
		}
	}
	if base == "" {
		return "", "", errOutsideRoots(modelPath)
	}
	relDir, err := filepath.Rel(base, realDir)
	if err != nil {
		return "", "", err
	}
	return base, path.Join(filepath.ToSlash(relDir), filepath.Base(abs)), nil
}

// checkWritableDir refuses a write whose real directory is one of
// Root's read-only subtrees or a read root. dir is a host path; it
// need not exist yet. See the file comment for the race this leaves.
func (b *Backend) checkWritableDir(dir, modelPath string) error {
	real, err := realPath(dir)
	if err != nil {
		return err
	}
	roots := b.ReadOnlyRoots
	if roots == nil {
		roots = DefaultReadOnlyRoots
	}
	ro := make([]string, 0, len(roots)*(1+len(b.WriteRoots))+len(b.ReadRoots))
	for _, r := range roots {
		// The read-only subtrees of every tree a write may land in: a
		// subagent overlay has no past-chats/, but its parent's does.
		for _, base := range append([]string{b.Root}, b.WriteRoots...) {
			ro = append(ro, filepath.Join(base, filepath.FromSlash(r)))
		}
	}
	ro = append(ro, b.ReadRoots...)
	ro = append(ro, b.mountHosts()...)
	for _, r := range realBases(ro) {
		if within(real, r) {
			if b.isPublishedReal(real) {
				return ErrPublishedReadOnly
			}
			return ErrReadOnly
		}
	}
	ok := false
	for _, w := range realBases(append([]string{b.Root}, b.WriteRoots...)) {
		if within(real, w) {
			ok = true
			break
		}
	}
	if !ok {
		return errOutsideRoots(modelPath)
	}
	return nil
}

// openWrite opens the base a write to abs goes through, creates abs's
// parent directories inside it, and checks where they really are.
// Returns the root and abs relative to it; the caller closes the root.
func (b *Backend) openWrite(abs, modelPath string) (*os.Root, string, error) {
	dir := filepath.Dir(abs)
	// Checked before the directories exist (so MkdirAll cannot build
	// them in a read-only farm) and again after (so what now exists is
	// where it should be).
	if err := b.checkWritableDir(dir, modelPath); err != nil {
		return nil, "", err
	}
	base, rel, err := b.writeBase(abs, modelPath)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, "", err
	}
	if d := path.Dir(rel); d != "." {
		if err := mkdirAllIn(root, d); err != nil {
			_ = root.Close()
			return nil, "", confinedErr(err, modelPath)
		}
	}
	if err := b.checkWritableDir(dir, modelPath); err != nil {
		_ = root.Close()
		return nil, "", err
	}
	return root, rel, nil
}

// confinedErr rewrites os.Root's "path escapes from parent" into the
// backend's own wording, so the model reads why its write was refused.
func confinedErr(err error, modelPath string) error {
	if err != nil && strings.Contains(err.Error(), "escapes from parent") {
		return errOutsideRoots(modelPath)
	}
	return err
}

// writeFileAtomicIn replaces rel inside root with data in one rename,
// so no reader ever sees a truncated or half-written file. Readers are
// not just the agent's next file_view: artifact_publish copies these
// files into the published tree, where the knowledge graph versions
// them, and a torn read would become a permanent, rejected version.
// The temp file is a dotfile in the same directory, which publishing
// and the graph both skip; a leftover from a crash is swept at
// bootstrap.
//
// Every step goes through root: the temp file is created with O_EXCL
// (never through an existing link), its mode is set on the open
// handle, and the rename replaces rel's directory entry itself — a
// symlink at rel is replaced, not written through.
func writeFileAtomicIn(root *os.Root, rel string, data []byte) error {
	_, err := writeReaderAtomicIn(root, rel, bytes.NewReader(data), -1)
	return err
}

// writeReaderAtomicIn is writeFileAtomicIn from a reader, returning the
// bytes written. With max >= 0 a reader that yields more than max bytes
// fails the write with errTooLarge and leaves rel as it was: the check
// is on the bytes actually copied, not on a size read beforehand.
func writeReaderAtomicIn(root *os.Root, rel string, r io.Reader, max int64) (n int64, err error) {
	dir, base := path.Split(rel)
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return 0, err
	}
	name := dir + ".tmp-" + tmpBase(base) + "-" + hex.EncodeToString(rnd[:])
	tmp, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = root.Remove(name)
		}
	}()
	src := r
	if max >= 0 {
		src = io.LimitReader(r, max+1)
	}
	if n, err = io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return n, err
	}
	if max >= 0 && n > max {
		_ = tmp.Close()
		err = errTooLarge
		return n, err
	}
	if err = tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return n, err
	}
	// Synced before the rename: ext4 (delayed allocation) can commit the
	// rename of a NEW file seconds before its data, so a power cut in
	// between leaves the name pointing at an empty file.
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return n, err
	}
	if err = tmp.Close(); err != nil {
		return n, err
	}
	if err = renameIn(root, name, rel); err != nil {
		return n, err
	}
	// And the directory after it, so a power cut cannot undo the rename
	// of a write already reported as done. The rename happened: a failed
	// sync is reported, but the temp file is gone, so nothing to remove.
	if serr := syncDirIn(root, dir); serr != nil {
		return n, fmt.Errorf("sync %s: %w", rel, serr)
	}
	return n, nil
}

// syncDirIn fsyncs dir (relative to root, "" for root itself), making
// the entries renamed or created in it durable. Windows cannot open a
// directory for a sync, and NTFS journals the rename itself; there it is
// a no-op.
func syncDirIn(root *os.Root, dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := root.Open(path.Clean("./" + dir))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// tmpBaseMaxBytes bounds the part of a temp file's name taken from the
// file it replaces, so ".tmp-<base>-<16 hex>" stays within the 255-byte
// name limit when base is itself up to 255 bytes long.
const tmpBaseMaxBytes = 200

// tmpBase is base cut to tmpBaseMaxBytes, on a UTF-8 boundary.
func tmpBase(base string) string {
	if len(base) <= tmpBaseMaxBytes {
		return base
	}
	n := tmpBaseMaxBytes
	for n > 0 && !utf8.RuneStart(base[n]) {
		n--
	}
	return base[:n]
}

// errTooLarge is writeReaderAtomicIn's refusal of a reader longer than
// its limit.
var errTooLarge = errors.New("larger than the limit")

// mkdirAllIn is os.MkdirAll inside root: each component is made with
// root.Mkdir, so a symlink along the way is followed only within root.
// (os.Root gained MkdirAll in Go 1.25; the module targets 1.24.)
func mkdirAllIn(root *os.Root, rel string) error {
	parts := strings.Split(path.Clean(rel), "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		err := root.Mkdir(p, 0o755)
		if err == nil {
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, serr := root.Stat(p)
		if serr != nil {
			return serr
		}
		if !info.IsDir() {
			return fmt.Errorf("%s: not a directory", p)
		}
	}
	return nil
}

// writeConfined creates abs's parents and atomically writes data to
// it, all through the write base. See openWrite.
func (b *Backend) writeConfined(abs, modelPath string, data []byte) error {
	root, rel, err := b.openWrite(abs, modelPath)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return confinedErr(writeFileAtomicIn(root, rel, data), modelPath)
}
