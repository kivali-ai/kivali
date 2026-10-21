package integration

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
)

// rawZip writes entries exactly as named, with modes, so a test can
// make the zips a careful tool never would.
func rawZip(t *testing.T, entries []struct {
	name string
	mode fs.FileMode
	body string
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A bundle zipped from its folder ("zip -r hubspot hubspot/") reads
// like one zipped from inside it: the folder is stripped from every
// path, so entry_point resolves, and macOS's metadata folder is not a
// file of the bundle.
func TestReadBundleStripsTheTopFolder(t *testing.T) {
	for _, top := range []string{"", "hubspot"} {
		data := integrationtest.Zip(t, top, integrationtest.Manifest("hubspot", "1.2.0"), map[string]integrationtest.File{
			"README.md": {Body: "# HubSpot\n"},
		})
		if top != "" {
			// Archive Utility's metadata sits beside the folder.
			data = appendEntry(t, data, "__MACOSX/hubspot/._manifest.json", "junk")
		}
		b, err := ReadBundle(data)
		if err != nil {
			t.Fatalf("top %q: %v", top, err)
		}
		var paths []string
		for _, f := range b.Files {
			paths = append(paths, f.Path)
		}
		if got := strings.Join(paths, ","); got != "README.md,manifest.json,server/main.py" {
			t.Errorf("top %q: files = %s", top, got)
		}
		if b.Manifest.Name != "hubspot" || !bytes.HasPrefix(b.ManifestBytes(), []byte("{")) || b.Size == 0 {
			t.Errorf("top %q: bundle = %+v", top, b)
		}
		if f := Validate(b.Manifest, b.Files); len(f) != 0 {
			t.Errorf("top %q: findings %v", top, f.Texts())
		}
	}
}

// appendEntry rewrites a zip with one more stored entry.
func appendEntry(t *testing.T, data []byte, name, body string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		if err := zw.Copy(f); err != nil {
			t.Fatal(err)
		}
	}
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(body))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// What ReadBundle refuses outright is what leaves nothing to check:
// no zip, no manifest, a manifest that is not a file or not JSON. Each
// is the upload refusal's one finding, so each must say what it is.
func TestReadBundleRefusals(t *testing.T) {
	type entry = struct {
		name string
		mode fs.FileMode
		body string
	}
	cases := map[string]struct {
		data []byte
		want string
	}{
		"empty":    {nil, "the bundle is empty"},
		"not zip":  {[]byte("PK not really"), "not a zip file"},
		"no manif": {rawZip(t, []entry{{"server/main.py", 0o644, "x"}, {"README.md", 0o644, "x"}}), "has no manifest.json"},
		"deep":     {rawZip(t, []entry{{"a/b/manifest.json", 0o644, "{}"}, {"c/x", 0o644, "x"}}), "has no manifest.json"},
		"symlink":  {rawZip(t, []entry{{"manifest.json", fs.ModeSymlink | 0o777, "/etc/passwd"}}), "is not a regular file"},
		"bad json": {rawZip(t, []entry{{"manifest.json", 0o644, "{nope"}}), "manifest.json is not valid"},
		"too big":  {rawZip(t, []entry{{"manifest.json", 0o644, `{"description":"` + strings.Repeat("x", maxManifestBytes) + `"}`}}), "is over 1024 KiB"},
	}
	for name, c := range cases {
		if _, err := ReadBundle(c.data); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// Extraction is where a hostile bundle would write outside its
// directory. A symlink, a `..` path and an absolute path are each
// refused, and nothing lands outside the destination.
func TestExtractRefusesWhatWouldEscape(t *testing.T) {
	type entry = struct {
		name string
		mode fs.FileMode
		body string
	}
	manifest := entry{"manifest.json", 0o644, "{}"}
	cases := map[string][]entry{
		"symlink":  {manifest, {"server/link", fs.ModeSymlink | 0o777, "../../outside"}},
		"dotdot":   {manifest, {"../outside", 0o644, "x"}},
		"deeper":   {manifest, {"server/../../outside", 0o644, "x"}},
		"absolute": {manifest, {"/tmp/outside-kivali-test", 0o644, "x"}},
		"fifo":     {manifest, {"server/fifo", fs.ModeNamedPipe | 0o644, ""}},
	}
	for name, entries := range cases {
		parent := t.TempDir()
		dst := filepath.Join(parent, "bundle")
		err := Extract(rawZip(t, entries), dst)
		if err == nil {
			t.Errorf("%s: extracted", name)
		}
		if _, err := os.Stat(filepath.Join(parent, "outside")); err == nil {
			t.Errorf("%s: wrote outside the destination", name)
		}
	}
	if _, err := os.Stat("/tmp/outside-kivali-test"); err == nil {
		t.Error("absolute path written")
	}
}

// A bundle extracts with its top folder stripped and its exec bits
// kept, into an empty directory only: a version's bundle directory is
// never merged into one that already holds files.
func TestExtractStripsTopFolderKeepsExecBit(t *testing.T) {
	data := integrationtest.Zip(t, "hubspot", integrationtest.Manifest("hubspot", "1.2.0"), map[string]integrationtest.File{
		"bin/run.sh": {Body: "#!/bin/sh\n", Mode: 0o755},
	})
	dst := filepath.Join(t.TempDir(), "bundle")
	if err := Extract(data, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "server", "main.py")); err != nil || string(b) != integrationtest.MainPy {
		t.Errorf("server/main.py = %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "bin", "run.sh")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh mode = %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "hubspot")); err == nil {
		t.Error("top folder kept")
	}
	if err := Extract(data, dst); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Errorf("second extract into the same dir: %v", err)
	}
}
