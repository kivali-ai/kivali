package integration

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/zipsafe"
)

// maxManifestBytes caps manifest.json.
const maxManifestBytes = 1 << 20

// Bundle is a read bundle: its manifest and the files it holds, before
// anything is written.
type Bundle struct {
	Manifest *Manifest
	// Files is every file, with the top folder stripped, in zip order.
	// Entries Validate refuses (a symlink, a path that leaves the
	// bundle) are listed too, so it can name them.
	Files []BundleFile
	// Size is the files' unpacked bytes, as the zip declares them.
	Size int64

	manifestRaw []byte
	zipData     []byte
	// dir is the bundle's directory when it was read from one
	// (ReadDirectory) rather than from a zip.
	dir fs.FS
}

// ManifestBytes is manifest.json byte for byte.
func (b *Bundle) ManifestBytes() []byte { return b.manifestRaw }

// Extract unpacks the bundle into dir: the zip, as the package
// function does, or for a bundle read from a directory, a copy of its
// files, under the same limits and refusals.
func (b *Bundle) Extract(dir string) error {
	if b.dir != nil {
		return b.copyTo(dir)
	}
	return Extract(b.zipData, dir)
}

// ReadBundle reads a bundle's zip: where its root is, every file it
// holds and its manifest. It writes nothing. A zip that is not one, or
// has no manifest.json at its root or inside one top-level folder, or
// whose manifest is not valid JSON, is an error; everything else about
// the bundle is Validate's to say.
func ReadBundle(data []byte) (*Bundle, error) {
	if len(data) == 0 {
		return nil, errors.New("the bundle is empty")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("the bundle is not a zip file")
	}
	prefix, ok := zipsafe.Prefix(zr, ManifestName)
	if !ok || !zipsafe.Contains(zr, prefix, ManifestName) {
		return nil, fmt.Errorf("the bundle has no %s at its root or inside a single top-level folder", ManifestName)
	}
	b := &Bundle{zipData: data}
	for _, f := range zr.File {
		cleaned := path.Clean(f.Name)
		if zipsafe.IsMacOSMetadata(cleaned) || f.FileInfo().IsDir() {
			continue
		}
		if prefix != "" && !strings.HasPrefix(cleaned, prefix) {
			continue
		}
		rel := strings.TrimPrefix(cleaned, prefix)
		if rel == "" || rel == "." {
			continue
		}
		size := int64(f.UncompressedSize64)
		if size < 0 {
			size = MaxBundleBytes + 1
		}
		b.Files = append(b.Files, BundleFile{Path: rel, Size: size, Mode: f.Mode()})
		b.Size += size
		if rel == ManifestName && f.Mode().IsRegular() {
			raw, err := readEntry(f, maxManifestBytes)
			if err != nil {
				return nil, err
			}
			b.manifestRaw = raw
		}
	}
	if b.manifestRaw == nil {
		return nil, fmt.Errorf("the bundle's %s is not a regular file", ManifestName)
	}
	m, err := ParseManifest(b.manifestRaw)
	if err != nil {
		return nil, err
	}
	b.Manifest = m
	return b, nil
}

// readEntry reads one zip entry of at most max bytes.
func readEntry(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("the bundle's %s cannot be read: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, fmt.Errorf("the bundle's %s cannot be read: %w", f.Name, err)
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("the bundle's %s is over %d KiB", f.Name, max>>10)
	}
	return b, nil
}

// bundleLimits are the caps Extract holds a bundle to, whatever its
// zip declares.
var bundleLimits = zipsafe.Limits{Total: MaxBundleBytes, Files: MaxBundleFiles}

// Extract unpacks a bundle's zip into dir, which must be empty or
// absent, stripping a single top-level folder, under the bundle
// limits. It refuses what zipsafe refuses: absolute paths, `..`,
// symlinks and special files. On a refusal dir may hold part of the
// bundle; extract into a staging directory and remove it on error.
func Extract(data []byte, dir string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return errors.New("the bundle is not a zip file")
	}
	prefix, ok := zipsafe.Prefix(zr, ManifestName)
	if !ok || !zipsafe.Contains(zr, prefix, ManifestName) {
		return fmt.Errorf("the bundle has no %s at its root or inside a single top-level folder", ManifestName)
	}
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("extract bundle: %s is not empty", dir)
	}
	return zipsafe.Extract(zr, prefix, dir, bundleLimits, "bundle")
}

// skippedDirs are the directories ReadDirectory leaves out: macOS's
// metadata, as a zip's reading does, and Python's bytecode cache,
// which running the server or its tests writes beside the code.
var skippedDirs = map[string]bool{"__MACOSX": true, "__pycache__": true}

// ReadDirectory reads a bundle from a directory, fsys rooted at the
// bundle's root (an os.Root's FS, so nothing it opens leaves the
// directory): every entry it holds and its manifest. It writes
// nothing, and reads as ReadBundle reads a zip: manifest.json must be
// a regular file at the root and valid JSON, and everything else about
// the bundle is Validate's to say. A symlink or special file is listed
// for Validate to name, never followed; __MACOSX and __pycache__
// directories are left out. The walk stops past MaxBundleFiles
// entries. The bundle's Extract copies the files then, so a bundle is
// stored from a directory as it is from a zip.
func ReadDirectory(fsys fs.FS) (*Bundle, error) {
	b := &Bundle{dir: fsys}
	errFull := errors.New("full")
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if skippedDirs[path.Base(p)] {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var size int64
		if info.Mode().IsRegular() {
			size = info.Size()
		}
		b.Files = append(b.Files, BundleFile{Path: p, Size: size, Mode: info.Mode()})
		b.Size += size
		if len(b.Files) > MaxBundleFiles {
			return errFull
		}
		return nil
	})
	switch {
	case errors.Is(err, errFull):
	case errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("the directory is not there")
	case err != nil:
		return nil, fmt.Errorf("the directory cannot be read: %w", err)
	}
	i := slices.IndexFunc(b.Files, func(f BundleFile) bool { return f.Path == ManifestName })
	switch {
	case i < 0:
		return nil, fmt.Errorf("the bundle has no %s at its root", ManifestName)
	case !b.Files[i].Mode.IsRegular():
		return nil, fmt.Errorf("the bundle's %s is not a regular file", ManifestName)
	case b.Files[i].Size > maxManifestBytes:
		return nil, fmt.Errorf("the bundle's %s is over %d KiB", ManifestName, maxManifestBytes>>10)
	}
	raw, err := fs.ReadFile(fsys, ManifestName)
	if err != nil {
		return nil, fmt.Errorf("the bundle's %s cannot be read: %w", ManifestName, err)
	}
	if len(raw) > maxManifestBytes {
		return nil, fmt.Errorf("the bundle's %s is over %d KiB", ManifestName, maxManifestBytes>>10)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	b.manifestRaw, b.Manifest = raw, m
	return b, nil
}

// copyTo copies the files a directory bundle listed into dir, which
// must be empty or absent, under the bundle limits: a symlink or
// special file, more files than MaxBundleFiles or more bytes than
// MaxBundleBytes, counted as they are written, is refused. The
// manifest is the one read, byte for byte, whatever the directory
// holds now, so the stored bundle's manifest is the one its install
// screen shows. On a refusal dir may hold part of the bundle, as with
// Extract.
func (b *Bundle) copyTo(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("copy bundle: %s is not empty", dir)
	}
	if len(b.Files) > MaxBundleFiles {
		return fmt.Errorf("bundle: more than %d files", MaxBundleFiles)
	}
	total := int64(len(b.manifestRaw))
	if err := os.WriteFile(filepath.Join(dir, ManifestName), b.manifestRaw, 0o644); err != nil {
		return err
	}
	for _, f := range b.Files {
		if f.Path == ManifestName {
			continue
		}
		if !f.Mode.IsRegular() {
			return fmt.Errorf("bundle: %s is not a regular file", f.Path)
		}
		n, err := copyBundleFile(b.dir, f, dir, MaxBundleBytes-total)
		if err != nil {
			return err
		}
		total += n
	}
	return nil
}

// copyBundleFile copies one regular file of at most limit bytes.
func copyBundleFile(fsys fs.FS, f BundleFile, dir string, limit int64) (int64, error) {
	if !fs.ValidPath(f.Path) {
		return 0, fmt.Errorf("bundle: unsafe path %q", f.Path)
	}
	src, err := fsys.Open(f.Path)
	if err != nil {
		return 0, fmt.Errorf("bundle: %s: %w", f.Path, err)
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return 0, fmt.Errorf("bundle: %s: %w", f.Path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("bundle: %s is not a regular file", f.Path)
	}
	target := filepath.Join(dir, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	perm := os.FileMode(0o644)
	if info.Mode().Perm()&0o111 != 0 {
		perm = 0o755
	}
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("bundle: %s: %w", f.Path, err)
	}
	if n > limit {
		return n, fmt.Errorf("bundle: more than %d MiB", MaxBundleBytes>>20)
	}
	return n, nil
}

// WriteZip writes the bundle unpacked in fsys (rooted at the bundle's
// root, so manifest.json is at the top) as a zip to w: every regular
// file, by path, in path order, with its mode; __MACOSX and __pycache__
// directories are left out, as a reading leaves them out. The zip
// reads back with ReadBundle to the same files, so a stored version
// can be passed on and installed elsewhere as it is. A symlink or
// special file is an error: a stored bundle holds none.
func WriteZip(fsys fs.FS, w io.Writer) error {
	zw := zip.NewWriter(w)
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if skippedDirs[path.Base(p)] {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("bundle: %s is not a regular file", p)
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = p
		h.Method = zip.Deflate
		dst, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		src, err := fsys.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, src)
		_ = src.Close()
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}
