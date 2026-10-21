// Package zipsafe extracts an uploaded zip into a directory without
// letting it write anywhere else: no absolute paths, no `..`, no
// symlinks or special files, and caps on the bytes and files written.
// Skills (internal/store) and integration bundles
// (internal/integration) both unpack through it.
package zipsafe

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bounds what Extract writes. A zero field is no cap.
type Limits struct {
	// PerFile caps one file's bytes.
	PerFile int64
	// Total caps the bytes of every file together.
	Total int64
	// Files caps how many files are written.
	Files int
}

// macOSMetadata is the folder macOS's Archive Utility adds beside a
// zip's content; it is never part of what was zipped.
const macOSMetadata = "__MACOSX"

// IsMacOSMetadata reports whether the cleaned zip name sits under
// macOS's metadata folder.
func IsMacOSMetadata(cleaned string) bool {
	return strings.HasPrefix(cleaned, macOSMetadata) || strings.Contains(cleaned, "/"+macOSMetadata+"/")
}

// Prefix is the path prefix to strip from every entry so the zip's
// content becomes the root: "" when manifest sits at the archive root,
// "<folder>/" when every entry (macOS metadata aside) sits in one
// top-level folder. ok is false for any other layout. The folder is
// not checked for manifest; Contains does that.
func Prefix(zr *zip.Reader, manifest string) (prefix string, ok bool) {
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() && path.Clean(f.Name) == manifest {
			return "", true
		}
	}
	top := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimLeft(path.Clean(f.Name), "/")
		if name == "" || name == "." {
			continue
		}
		first := name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			first = name[:i]
		}
		if first == macOSMetadata {
			continue
		}
		top[first] = true
	}
	if len(top) != 1 {
		return "", false
	}
	for k := range top {
		prefix = k + "/"
	}
	return prefix, true
}

// Contains reports whether a regular file named name sits directly
// under prefix.
func Contains(zr *zip.Reader, prefix, name string) bool {
	want := prefix + name
	for _, f := range zr.File {
		if path.Clean(f.Name) == want && !f.FileInfo().IsDir() {
			return true
		}
	}
	return false
}

// Member is the slash-separated path f extracts to under prefix. skip
// is true for an entry Extract leaves out: macOS metadata, an entry
// outside prefix, and the root itself. err refuses an entry that would
// leave the destination (an absolute path or `..`) or is not a regular
// file or directory (a symlink, a device, a pipe or a socket).
func Member(f *zip.File, prefix string) (rel string, skip bool, err error) {
	cleaned := path.Clean(f.Name)
	if IsMacOSMetadata(cleaned) {
		return "", true, nil
	}
	if prefix != "" && !strings.HasPrefix(cleaned, prefix) {
		return "", true, nil
	}
	rel = strings.TrimPrefix(cleaned, prefix)
	if rel == "" || rel == "." {
		return "", true, nil
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") || rel == ".." {
		return "", false, fmt.Errorf("unsafe path %q", f.Name)
	}
	if f.Mode()&(os.ModeSymlink|os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice) != 0 {
		return "", false, fmt.Errorf("%q has unsupported file type", f.Name)
	}
	return rel, false, nil
}

// ErrTooMany is wrapped by Extract's refusal of a zip with more files
// than Limits.Files.
var ErrTooMany = errors.New("too many files")

// Extract writes every entry of zr under prefix into dst, with the
// prefix stripped (Member), creating directories as it goes. A file
// keeps its exec bit and lands 0644 or 0755. Every refusal, prefixed
// with label ("skill zip: ..."), stops the extraction where it is; the
// caller extracts into a staging directory it removes on error.
func Extract(zr *zip.Reader, prefix, dst string, lim Limits, label string) error {
	var total int64
	files := 0
	for _, f := range zr.File {
		rel, skip, err := Member(f, prefix)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if skip {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		// Re-verify the computed path stays under dst even after
		// filepath.Join resolves any `..` that slipped through.
		if !strings.HasPrefix(target, dst+string(filepath.Separator)) && target != dst {
			return fmt.Errorf("%s: unsafe path %q", label, f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		files++
		if lim.Files > 0 && files > lim.Files {
			return fmt.Errorf("%s: more than %d files: %w", label, lim.Files, ErrTooMany)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// Honor the exec bit; ignore writeability flags: everything
		// lands writable by the Kivali process.
		perm := os.FileMode(0o644)
		if f.Mode().Perm()&0o111 != 0 {
			perm = 0o755
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s: open %q: %w", label, f.Name, err)
		}
		var r io.Reader = rc
		switch {
		case lim.PerFile > 0:
			r = io.LimitReader(rc, lim.PerFile+1)
		case lim.Total > 0:
			r = io.LimitReader(rc, lim.Total-total+1)
		}
		buf, err := io.ReadAll(r)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("%s: read %q: %w", label, f.Name, err)
		}
		if lim.PerFile > 0 && int64(len(buf)) > lim.PerFile {
			return fmt.Errorf("%s: %q exceeds %d-byte per-file cap", label, f.Name, lim.PerFile)
		}
		total += int64(len(buf))
		if lim.Total > 0 && total > lim.Total {
			return fmt.Errorf("%s: bundle exceeds %d-byte total cap", label, lim.Total)
		}
		if err := os.WriteFile(target, buf, perm); err != nil {
			return err
		}
	}
	return nil
}
