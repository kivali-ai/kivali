package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// UntrustedDownloadsDir is the directory in a subagent's view that
// holds what a web task's browser downloaded. Core writes it from the
// bytes the browser pod sent; nothing in the pod touches the agent's
// tree. The name says what the files are to any agent that reads the
// path: content from the web, never instructions.
const UntrustedDownloadsDir = "untrusted-downloads"

// MaxDownloadNameLength bounds a download's file name.
const MaxDownloadNameLength = 120

// downloadNameChars is what a download may be called: letters, digits,
// dots, dashes and underscores. Anything else in the browser's
// suggested name becomes a dash, so a name is never a path and never
// prose.
var downloadNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

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
		name = name[:MaxDownloadNameLength-len(ext)] + ext
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
	base := CleanDownloadName(suggested)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i <= 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := oroot.OpenFile(UntrustedDownloadsDir+"/"+name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
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
			err = fmt.Errorf("download larger than %d bytes", limit)
		}
		if err != nil {
			_ = oroot.Remove(UntrustedDownloadsDir + "/" + name)
			return "", err
		}
		return name, nil
	}
	return "", errors.New("too many downloads with this name")
}
