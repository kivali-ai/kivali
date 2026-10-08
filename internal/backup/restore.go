package backup

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one member of an archive.
type Entry struct {
	// Name is the member's path as stored, slash-separated. A directory
	// may carry a trailing slash.
	Name    string
	Dir     bool
	Mode    fs.FileMode
	ModTime time.Time
	// Size is the declared size of a file, or -1 when the format does
	// not say.
	Size int64
	Open func() (io.ReadCloser, error)
}

// Source yields an archive's entries in order, then io.EOF.
type Source interface {
	Next() (*Entry, error)
}

// Options shape a restore.
type Options struct {
	// MaxTotalBytes, when positive, refuses an archive whose files add
	// up to more than this (ErrNoRoom): the room left on the volume it
	// unpacks to. CheckZip refuses it from the manifest before anything
	// is written; Restore stops writing at it.
	MaxTotalBytes int64
}

// ErrNoRoom is why an archive bigger than Options.MaxTotalBytes is
// refused.
var ErrNoRoom = errors.New("the backup does not fit on the data volume")

// Result says what a restore wrote.
type Result struct {
	Manifest *Manifest
	Files    int
	Dirs     int
	Bytes    int64
}

// errNoManifest refuses an archive without a manifest: what it should
// hold cannot be checked.
var errNoManifest = errors.New("restore: the archive has no manifest, so what it should hold cannot be checked")

// Restore unpacks src into the data directory dst and verifies every
// file it wrote against the archive's manifest: a file missing from the
// archive, one the manifest does not list, one of another size or with
// other content, each fails the restore. So does an archive without a
// manifest. Each file is written in full or the restore fails.
//
// A failure can leave dst partly written; the caller says so, and the
// operator empties it before trying again. File modes and modification
// times are taken from the archive. Every write goes through an os.Root
// on dst, so a symlink already in dst cannot carry one outside it.
func Restore(dst string, src Source, opt Options) (*Result, error) {
	res := &Result{}
	root, err := os.OpenRoot(dst)
	if err != nil {
		return res, fmt.Errorf("restore: %w", err)
	}
	defer func() { _ = root.Close() }()
	var got []File
	seen := map[string]bool{}
	type dirTime struct {
		path string
		mod  time.Time
	}
	var dirTimes []dirTime
	for {
		e, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("restore: read archive: %w", err)
		}
		name := strings.TrimSuffix(e.Name, "/")
		if name == ManifestName && !e.Dir {
			if res.Manifest != nil {
				return res, errors.New("restore: the archive holds two manifests")
			}
			m, err := readManifest(e)
			if err != nil {
				return res, fmt.Errorf("restore: %w", err)
			}
			res.Manifest = m
			continue
		}
		target, err := SafeTarget(dst, name)
		if err != nil {
			return res, fmt.Errorf("restore: archive entry rejected: %w", err)
		}
		rel := filepath.ToSlash(filepath.Clean(name))
		if seen[rel] {
			return res, fmt.Errorf("restore: %s appears twice in the archive", rel)
		}
		seen[rel] = true
		if e.Dir {
			if err := mkdirAllIn(root, rel, dirPerm(e.Mode)); err != nil {
				return res, fmt.Errorf("restore: %w", err)
			}
			if !e.ModTime.IsZero() {
				dirTimes = append(dirTimes, dirTime{target, e.ModTime})
			}
			res.Dirs++
			continue
		}
		var room int64
		if opt.MaxTotalBytes > 0 {
			room = opt.MaxTotalBytes - res.Bytes
			if e.Size > room {
				return res, fmt.Errorf("restore: %s: %w (%d bytes to spare)", rel, ErrNoRoom, opt.MaxTotalBytes)
			}
		}
		f, err := writeFile(root, rel, target, e, room)
		if err != nil {
			return res, fmt.Errorf("restore: %s: %w", rel, err)
		}
		f.Path = rel
		got = append(got, f)
		res.Files++
		res.Bytes += f.Size
	}
	if res.Manifest == nil {
		return res, errNoManifest
	}
	if diff := Diff(res.Manifest.Files, got); len(diff) > 0 {
		return res, fmt.Errorf("restore: the archive does not match its manifest (%d differences): %s", len(diff), summarize(diff))
	}
	// Directory times last: writing into a directory moves its mtime.
	// Deepest first, so setting a child does not move its parent again.
	sort.Slice(dirTimes, func(i, j int) bool { return len(dirTimes[i].path) > len(dirTimes[j].path) })
	for _, d := range dirTimes {
		_ = os.Chtimes(d.path, d.mod, d.mod)
	}
	return res, nil
}

// mkdirAllIn is os.MkdirAll inside root, for the Go versions whose
// os.Root has no MkdirAll. An existing component must be a directory.
func mkdirAllIn(root *os.Root, rel string, perm fs.FileMode) error {
	parts := strings.Split(rel, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		err := root.Mkdir(p, perm)
		if err == nil {
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		fi, lerr := root.Lstat(p)
		if lerr != nil {
			return lerr
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", p)
		}
	}
	return nil
}

func writeFile(root *os.Root, rel, target string, e *Entry, max int64) (File, error) {
	if dir := path.Dir(rel); dir != "." {
		if err := mkdirAllIn(root, dir, 0o755); err != nil {
			return File{}, err
		}
	}
	rc, err := e.Open()
	if err != nil {
		return File{}, err
	}
	defer func() { _ = rc.Close() }()
	out, err := root.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm(e.Mode))
	if err != nil {
		return File{}, err
	}
	var r io.Reader = rc
	if max > 0 {
		r = io.LimitReader(rc, max+1)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), r)
	if err == nil {
		err = out.Chmod(filePerm(e.Mode))
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return File{}, err
	}
	if max > 0 && n > max {
		return File{}, ErrNoRoom
	}
	if e.Size >= 0 && n != e.Size {
		return File{}, fmt.Errorf("wrote %d bytes, the archive declares %d", n, e.Size)
	}
	if !e.ModTime.IsZero() {
		if err := os.Chtimes(target, e.ModTime, e.ModTime); err != nil {
			return File{}, err
		}
	}
	return File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func readManifest(e *Entry) (*Manifest, error) {
	rc, err := e.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return ParseManifest(b)
}

// filePerm and dirPerm keep an archive's modes, but always let the
// server read and write what it restored, and enter its directories: a
// zip re-packed by a tool that stores no Unix modes reads back without
// an exec bit, and its directories would lock the restore out.
func filePerm(m fs.FileMode) fs.FileMode {
	if p := m.Perm(); p != 0 {
		return p | 0o600
	}
	return 0o644
}

func dirPerm(m fs.FileMode) fs.FileMode {
	if p := m.Perm(); p != 0 {
		return p | 0o700
	}
	return 0o755
}

// SafeTarget resolves an archive member's name to a path under root,
// refusing an empty name, an absolute one, and any that climbs out of
// root.
func SafeTarget(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty entry name")
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute or rooted path: %s", name)
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes data dir: %s", name)
	}
	target := filepath.Join(root, cleaned)
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes data dir: %s", name)
	}
	return target, nil
}

// ---- sources ----

// OpenArchive opens a backup zip and checks it with CheckZip, so
// nothing is written from one that would fail. The returned func
// closes it.
func OpenArchive(path string, opt Options) (Source, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	var magic [4]byte
	if n, _ := io.ReadFull(f, magic[:]); n != 4 || string(magic[:]) != "PK\x03\x04" {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s is not a backup: a backup is the .zip the app downloads", path)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if err := CheckZip(zr, opt); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return NewZipSource(zr), func() { _ = f.Close() }, nil
}

// ZipSource reads a zip as written by WriteZip.
type ZipSource struct {
	files []*zip.File
	i     int
}

// NewZipSource reads zr's members in the order they are stored.
func NewZipSource(zr *zip.Reader) *ZipSource { return &ZipSource{files: zr.File} }

// Next implements Source.
func (z *ZipSource) Next() (*Entry, error) {
	if z.i >= len(z.files) {
		return nil, io.EOF
	}
	f := z.files[z.i]
	z.i++
	e := &Entry{Name: f.Name, Mode: f.Mode().Perm(), ModTime: f.Modified, Size: int64(f.UncompressedSize64)}
	if strings.HasSuffix(f.Name, "/") || f.Mode().IsDir() {
		e.Dir = true
		return e, nil
	}
	if !f.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a file or a directory", f.Name)
	}
	e.Open = func() (io.ReadCloser, error) { return f.Open() }
	return e, nil
}

// CheckZip checks everything about a zip before a byte is written:
// every name is safe, not refused and stored once, the files fit in
// MaxTotalBytes, the members are exactly the files the manifest lists, and
// every member reads back whole with the manifest's content. That last
// pass decompresses the archive once more than the restore itself
// does, and is what keeps a damaged zip from failing half way through
// and leaving a deployment half restored. Restore still verifies the
// content as it writes.
func CheckZip(zr *zip.Reader, opt Options) error {
	var manifest *Manifest
	var listed []File
	members := map[string]*zip.File{}
	seen := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == ManifestName {
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("restore: manifest: %w", err)
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return fmt.Errorf("restore: manifest: %w", err)
			}
			if manifest, err = ParseManifest(b); err != nil {
				return fmt.Errorf("restore: %w", err)
			}
			continue
		}
		if _, err := SafeTarget(".", name); err != nil {
			return fmt.Errorf("restore: archive entry rejected: %w", err)
		}
		rel := filepath.ToSlash(filepath.Clean(name))
		if seen[rel] {
			return fmt.Errorf("restore: %s appears twice in the archive", rel)
		}
		seen[rel] = true
		if strings.HasSuffix(f.Name, "/") || f.Mode().IsDir() {
			continue
		}
		listed = append(listed, File{Path: rel, Size: int64(f.UncompressedSize64)})
		members[rel] = f
	}
	if manifest == nil {
		return errNoManifest
	}
	// Names and sizes first: cheap, and they name what is wrong.
	want := make([]File, len(manifest.Files))
	var total int64
	for i, f := range manifest.Files {
		want[i] = File{Path: f.Path, Size: f.Size}
		total += f.Size
	}
	if opt.MaxTotalBytes > 0 && total > opt.MaxTotalBytes {
		return fmt.Errorf("restore: %w: it unpacks to %d bytes, and the data volume has %d to spare", ErrNoRoom, total, opt.MaxTotalBytes)
	}
	if diff := Diff(want, listed); len(diff) > 0 {
		return fmt.Errorf("restore: the archive does not match its manifest (%d differences): %s", len(diff), summarize(diff))
	}
	for _, mf := range manifest.Files {
		got, err := hashZipMember(members[mf.Path])
		if err != nil {
			return fmt.Errorf("restore: %s: %w", mf.Path, err)
		}
		if got != mf.SHA256 {
			return fmt.Errorf("restore: the archive does not match its manifest: content differs: %s", mf.Path)
		}
	}
	return nil
}

// hashZipMember reads a member whole, which also has the zip reader
// check its CRC, and returns its SHA-256.
func hashZipMember(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
