package browserpod

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// maxProfileBytes bounds what an unpacked profile may hold.
const maxProfileBytes = MaxProfileBytes

// profileSkipDirs are directories a saved profile leaves out, at any
// depth: caches Chromium rebuilds, models and components it downloads
// again, and crash reports. Two-element entries match a directory
// under a parent of that name.
var profileSkipDirs = []string{
	"Cache", "Code Cache", "GPUCache", "DawnCache", "DawnGraphiteCache", "DawnWebGPUCache",
	"GrShaderCache", "GraphiteDawnCache", "ShaderCache", "GPUPersistentCache",
	"Service Worker/CacheStorage", "Service Worker/ScriptCache",
	"optimization_guide_model_store", "component_crx_cache", "extensions_crx_cache", "Crashpad",
}

// profileSkipFiles are files a saved profile leaves out: locks and
// logs of the run that wrote it.
var profileSkipFiles = []string{"SingletonLock", "SingletonSocket", "SingletonCookie", "lockfile", "RunningChromeVersion"}

// skipInProfile reports whether rel (slash-separated, relative to
// the profile root) stays out of a saved profile.
func skipInProfile(rel string, dir bool) bool {
	base := path.Base(rel)
	if dir {
		for _, s := range profileSkipDirs {
			if strings.Contains(s, "/") {
				if rel == s || strings.HasSuffix(rel, "/"+s) {
					return true
				}
			} else if base == s {
				return true
			}
		}
		return false
	}
	if strings.HasSuffix(base, ".log") {
		return true
	}
	for _, s := range profileSkipFiles {
		if base == s {
			return true
		}
	}
	return false
}

// packProfile writes dir as a zstd-compressed tar, leaving out caches,
// locks and logs. Symlinks and other special files are left out too.
func packProfile(dir string, w io.Writer) error {
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skipInProfile(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: 0o700, ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			defer func() { _ = f.Close() }()
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			// A file that changed size while packing is written to the
			// size its header announced.
			n, err := io.Copy(tw, io.LimitReader(f, info.Size()))
			if err != nil {
				return err
			}
			if n < info.Size() {
				_, err = io.CopyN(tw, zeroReader{}, info.Size()-n)
			}
			return err
		}
		return nil
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = zw.Close()
		return walkErr
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// unpackProfile extracts a packed profile into dir. Entries that would
// land outside dir, absolute names and links are skipped; the total is
// capped at maxProfileBytes.
func unpackProfile(r io.Reader, dir string) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("browserpod: read profile: %w", err)
		}
		name := h.Name
		if name == "" || strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
			continue
		}
		clean := path.Clean(name)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains("/"+name+"/", "/../") {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(clean))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > maxProfileBytes {
				return fmt.Errorf("browserpod: profile is over %d bytes", int64(maxProfileBytes))
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}
