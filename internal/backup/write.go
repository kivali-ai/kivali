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
	"path/filepath"
	"time"

	"github.com/kivali-ai/kivali/internal/files"
)

// WriteZip writes a backup of the data directory root to w as a zip,
// the manifest last: the one backup format, the file the app downloads
// and restores. It fails, rather than leaving a file out, on anything it
// cannot read; the one exception is a file deleted between the walk
// listing it and the copy opening it, which a running server does (turn
// markers, temp files), and which the manifest then does not list
// either. A caller streaming it over HTTP must abort the response on an
// error, so the download fails instead of ending as a valid-looking zip
// with files missing.
func WriteZip(root string, w io.Writer, version string) (*Manifest, error) {
	zw := zip.NewWriter(w)
	m, err := write(root, &zipSink{zw: zw}, version)
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("backup: close zip: %w", err)
	}
	return m, nil
}

type sink interface {
	dir(rel string, info fs.FileInfo) error
	// file writes exactly info.Size() bytes read from r.
	file(rel string, info fs.FileInfo, r io.Reader) error
	manifest(body []byte, at time.Time) error
}

func write(root string, s sink, version string) (*Manifest, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("backup: %s is not a directory", root)
	}
	m := &Manifest{Format: ManifestFormat, Created: time.Now().UTC(), Version: version}
	err = Walk(root, func(rel string, info fs.FileInfo) error {
		if info.IsDir() {
			return s.dir(rel, info)
		}
		// No-follow open: the walk saw a regular file, but agents write
		// their own trees (links included) while it runs, and a link
		// swapped in since — at the file or any directory above it —
		// would otherwise carry the org's Claude credentials, which the
		// walk deliberately leaves out, into the archive under an
		// agent's path. What is no longer a regular file is skipped, as
		// a deleted one is.
		f, err := files.OpenNoFollow(root, rel)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, files.ErrSymlink) || errors.Is(err, files.ErrNotRegular) || errors.Is(err, files.ErrNotDir) || errors.Is(err, files.ErrRaced) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		defer func() { _ = f.Close() }()
		// Size the copy from the open file, not the walk: every store
		// write is a temp file renamed into place, so by now the path
		// can name a newer file, which the walk's size would cut short.
		// The descriptor pins this file; only an append can still grow
		// it, and cutting there keeps a whole prefix of an append-only
		// log (chat, usage).
		if info, err = f.Stat(); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		h := sha256.New()
		c := &counter{}
		r := io.TeeReader(io.LimitReader(f, info.Size()), io.MultiWriter(h, c))
		if err := s.file(rel, info, r); err != nil {
			return fmt.Errorf("backup: %s: %w", rel, err)
		}
		if c.n != info.Size() {
			return fmt.Errorf("backup: %s: read %d bytes, want %d (the file shrank while being copied)", rel, c.n, info.Size())
		}
		m.Files = append(m.Files, File{Path: rel, Size: c.n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.manifest(m.Marshal(), m.Created); err != nil {
		return nil, fmt.Errorf("backup: manifest: %w", err)
	}
	return m, nil
}

// Walk visits, in lexical order, every directory and regular file under
// root that a backup carries, with its slash-separated path relative to
// root. Left out: what files.ShouldExcludeFromBackup names (Claude
// sign-in and session logs, debug dumps, temp files), the per-agent
// hardlink mirrors a boot re-links, anything neither a directory nor a
// regular file (sockets, symlinks, which a boot recreates), and a file
// named ManifestName. An entry deleted while the walk runs is skipped;
// any other error stops it.
func Walk(root string, visit func(rel string, info fs.FileInfo) error) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if p != root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("backup: %w", err)
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if files.ShouldExcludeFromBackup(rel) || files.IsHardlinkMirrorPath(rel) || rel == ManifestName {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		return visit(rel, info)
	})
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

type zipSink struct{ zw *zip.Writer }

func (z *zipSink) dir(rel string, info fs.FileInfo) error {
	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	hdr.Name = rel + "/"
	_, err = z.zw.CreateHeader(hdr)
	return err
}

func (z *zipSink) file(rel string, info fs.FileInfo, r io.Reader) error {
	hdr, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	hdr.Name = rel
	hdr.Method = zip.Deflate
	w, err := z.zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	return err
}

func (z *zipSink) manifest(body []byte, at time.Time) error {
	w, err := z.zw.CreateHeader(&zip.FileHeader{Name: ManifestName, Method: zip.Deflate, Modified: at})
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
