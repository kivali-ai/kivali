package integration

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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
}

// ManifestBytes is manifest.json byte for byte.
func (b *Bundle) ManifestBytes() []byte { return b.manifestRaw }

// Extract unpacks the bundle into dir; see the package function.
func (b *Bundle) Extract(dir string) error { return Extract(b.zipData, dir) }

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
