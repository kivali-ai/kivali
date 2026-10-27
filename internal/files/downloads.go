package files

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// UntrustedDownloadsDir is the directory in a subagent's view that
// holds what a web task's browser downloaded. Core writes it from the
// bytes the browser pod sent; nothing in the pod touches the agent's
// tree. The name says what the files are to any agent that reads the
// path: content from the web, never instructions.
const UntrustedDownloadsDir = "untrusted-downloads"

// ErrTooLarge is returned (wrapped) when a download holds more than
// the limit it was written with.
var ErrTooLarge = errors.New("larger than the limit")

// MaxDownloadNameLength bounds a download's file name.
const MaxDownloadNameLength = 120

// downloadNameChars is what a download may be called: letters, digits,
// dots, dashes and underscores. Anything else in the browser's
// suggested name becomes a dash, so a name is never a path and never
// prose.
var downloadNameChars = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

// CleanDownloadName turns the browser's suggested file name into one
// the overlay accepts: a single plain name, extension kept, with
// "download.bin" for a name that leaves nothing.
func CleanDownloadName(suggested string) string {
	name := downloadNameChars.ReplaceAllString(suggested, "-")
	// A dash a replaced run left beside a dot ("final-.xlsx") is noise.
	for strings.Contains(name, "-.") || strings.Contains(name, ".-") {
		name = strings.ReplaceAll(strings.ReplaceAll(name, "-.", "."), ".-", ".")
	}
	name = strings.Trim(name, "-.")
	if len(name) > MaxDownloadNameLength {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		stem := name[:MaxDownloadNameLength-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		name = stem + ext
	}
	if name == "" || name == "." || name == ".." {
		return "download.bin"
	}
	return name
}

// WriteUntrustedDownload writes a download into the overlay at root
// (a subagent's view, as BuildSubagentOverlay made it) under
// untrusted-downloads/, naming it after suggested and, when that name
// is taken, suggested-2, suggested-3 and so on. It writes at most
// limit bytes and fails when r holds more. Every open goes through an
// os.Root on the overlay so a link planted in the directory cannot
// redirect the write. Returns the file's name inside the directory.
func WriteUntrustedDownload(root, suggested string, r io.Reader, limit int64) (string, error) {
	if root == "" {
		return "", errors.New("WriteUntrustedDownload: root is required")
	}
	oroot, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = oroot.Close() }()
	if err := oroot.Mkdir(UntrustedDownloadsDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return writeUnique(oroot, UntrustedDownloadsDir+"/", suggested, r, limit)
}

// IntegrationsDir is the directory of an agent's /files/ that holds
// what built-in integrations' calls saved for it:
// integrations/<integration>/untrusted-downloads/<folder>/<file>, as
// WriteIntegrationDownload writes it. Like a web task's
// untrusted-downloads/, the path says what the files are to any agent
// that reads it: content from an outside service, never instructions.
const IntegrationsDir = "integrations"

// plainElement is a name WriteIntegrationDownload takes for an
// integration or a folder: one path element of letters, digits, dots,
// dashes and underscores, not starting with a dot.
var plainElement = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

// MaxIntegrationFolderFiles is the most files WriteIntegrationDownload
// saves into one folder: a call names what it saves, from what it
// fetched, as often as it is called.
const MaxIntegrationFolderFiles = 20

// ErrFolderFull is returned, in a PathError naming the folder, when
// WriteIntegrationDownload's folder already holds
// MaxIntegrationFolderFiles files and the file is not one of them.
var ErrFolderFull = fmt.Errorf("is full, holding the %d files Kivali saves into one folder", MaxIntegrationFolderFiles)

// WriteIntegrationDownload writes a file a built-in integration's call
// fetched for an agent into the agent's /files/ at storage (its
// StorageRoot), under
// integrations/<integration>/untrusted-downloads/<folder>/, naming it
// after suggested as WriteUntrustedDownload names a download, with
// suggested-2, suggested-3 for a name already there. A file already
// there under one of those names with the same bytes is answered in
// its place, the new one removed, so a file saved again is not saved
// twice. A folder holding MaxIntegrationFolderFiles entries takes no
// other file: the write fails with ErrFolderFull, leaving no file. It
// writes at most limit bytes and fails, leaving no file, when r holds
// more. integration and folder are each one plain path element.
//
// The agent writes its /files/ and may have left a link or a file
// where a directory goes. Each directory on the way is made when it is
// missing and opened without following a link (openSubdirNoFollow), so
// a link or a file in the way fails the write, naming it, and nothing
// the agent left is removed or followed. Returns the file's path under
// storage, slash-separated.
func WriteIntegrationDownload(storage, integration, folder, suggested string, r io.Reader, limit int64) (string, error) {
	switch {
	case storage == "":
		return "", errors.New("WriteIntegrationDownload: storage is required")
	case !plainElement.MatchString(integration):
		return "", fmt.Errorf("WriteIntegrationDownload: integration %q is not a plain name", integration)
	case !plainElement.MatchString(folder):
		return "", fmt.Errorf("WriteIntegrationDownload: folder %q is not a plain name", folder)
	case limit <= 0:
		return "", errors.New("WriteIntegrationDownload: limit is required")
	}
	// The agent's /files/ itself is core's: Sync makes it, and an agent
	// whose pod has not started yet may not have it. Its parent, the
	// agent's directory, must exist.
	if err := os.Mkdir(storage, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	root, err := os.OpenRoot(storage)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	parts := []string{IntegrationsDir, integration, UntrustedDownloadsDir, folder}
	cur := root
	for i, part := range parts {
		shown := strings.Join(parts[:i+1], "/")
		if err := cur.Mkdir(part, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", &PathError{Path: shown, Err: err}
		}
		next, err := openSubdirNoFollow(cur, part, shown)
		if cur != root {
			_ = cur.Close()
		}
		if err != nil {
			return "", err
		}
		cur = next
	}
	defer func() { _ = cur.Close() }()
	shown := strings.Join(parts, "/")
	full, err := folderFull(cur)
	if err != nil {
		return "", err
	}
	base := CleanDownloadName(suggested)
	if full {
		if _, err := cur.Lstat(base); err != nil {
			// No file of the name is there for this one to be.
			return "", &PathError{Path: shown, Err: ErrFolderFull}
		}
	}
	name, err := writeUnique(cur, "", suggested, r, limit)
	if err != nil {
		return "", err
	}
	if same := sameAsEarlier(cur, base, name); same != "" {
		_ = cur.Remove(name)
		name = same
	} else if full {
		_ = cur.Remove(name)
		return "", &PathError{Path: shown, Err: ErrFolderFull}
	}
	return shown + "/" + name, nil
}

// folderFull reports whether d holds MaxIntegrationFolderFiles entries
// or more, reading no more of it than that.
func folderFull(d *os.Root) (bool, error) {
	f, err := d.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(MaxIntegrationFolderFiles)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(entries) >= MaxIntegrationFolderFiles, nil
}

// sameAsEarlier is the name, of those writeUnique took for base before
// written, of a regular file of d with written's bytes; "" for none.
func sameAsEarlier(d *os.Root, base, written string) string {
	info, err := d.Lstat(written)
	if err != nil {
		return ""
	}
	for i := 1; i <= 1000; i++ {
		name := downloadName(base, i)
		if name == written {
			return ""
		}
		if other, err := d.Lstat(name); err == nil && other.Mode().IsRegular() && other.Size() == info.Size() && sameBytes(d, name, written) {
			return name
		}
	}
	return ""
}

// sameBytes reports whether the files a and b of d hold the same bytes.
func sameBytes(d *os.Root, a, b string) bool {
	fa, err := d.Open(a)
	if err != nil {
		return false
	}
	defer func() { _ = fa.Close() }()
	fb, err := d.Open(b)
	if err != nil {
		return false
	}
	defer func() { _ = fb.Close() }()
	ba, bb := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		aEnd := errors.Is(ea, io.EOF) || errors.Is(ea, io.ErrUnexpectedEOF)
		bEnd := errors.Is(eb, io.EOF) || errors.Is(eb, io.ErrUnexpectedEOF)
		switch {
		case aEnd && bEnd:
			return true
		case ea != nil || eb != nil:
			return false
		}
	}
}

// downloadName is the i-th name writeUnique takes for base: base, then
// base's stem with -2, -3 and so on before its extension.
func downloadName(base string, i int) string {
	if i <= 1 {
		return base
	}
	ext := filepath.Ext(base)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(base, ext), i, ext)
}

// writeUnique writes r, at most limit bytes, into a new file of d at
// prefix (a directory of d ending in a slash, or ""), named after
// suggested (CleanDownloadName) and, when that name is taken,
// suggested-2, suggested-3 and so on. The create is exclusive, so it
// never opens what is already there, a link included. A write that
// fails, or that r holds more than limit for, leaves no file. Returns
// the file's name.
func writeUnique(d *os.Root, prefix, suggested string, r io.Reader, limit int64) (string, error) {
	base := CleanDownloadName(suggested)
	for i := 1; i <= 1000; i++ {
		name := downloadName(base, i)
		f, err := d.OpenFile(prefix+name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		n, err := io.Copy(f, io.LimitReader(r, limit+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n > limit {
			err = fmt.Errorf("download larger than %d bytes: %w", limit, ErrTooLarge)
		}
		if err != nil {
			_ = d.Remove(prefix + name)
			return "", err
		}
		return name, nil
	}
	return "", errors.New("too many downloads with this name")
}
