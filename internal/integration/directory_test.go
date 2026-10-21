package integration

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTree writes files (path to body) under a new temp directory and
// returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// openTree is dir as an os.Root's FS, the way core opens an agent's
// directory.
func openTree(t *testing.T, dir string) fs.FS {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root.FS()
}

func echoManifestBytes(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "echo", ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Why this test exists: the echo bundle is the fixture every slice's
// end-to-end path runs (the shim's second server, the engineer's
// publish), so it must pass the check with nothing to say. A finding
// here means the fixture drifted from the contract the check holds
// bundles to, and every test built on it would be testing a bundle the
// owner's install screen would warn about.
func TestEchoBundlePassesTheCheck(t *testing.T) {
	b, err := ReadDirectory(openTree(t, filepath.Join("testdata", "echo")))
	if err != nil {
		t.Fatal(err)
	}
	if f := Validate(b.Manifest, b.Files); len(f) != 0 {
		t.Errorf("findings = %v", f.Texts())
	}
	if b.Manifest.Name != "echo" || b.Manifest.Version != "1.0.0" || b.Manifest.Kivali.Verify != "whoami" {
		t.Errorf("manifest = %+v", b.Manifest)
	}
}

// Why this test exists: a directory is read by the rules a zip is read
// by, so what an agent builds and what the owner uploads are checked
// alike. A symlink is listed, never followed, so the check refuses it
// by name rather than packing what it points at (on core, anything on
// the data volume); macOS's metadata and Python's bytecode cache are
// left out, so running the server's tests does not change the bundle.
// A directory with no manifest, or one that is a link, is not a bundle.
func TestReadDirectoryReadsAsAZipIsRead(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"manifest.json":                        echoManifestBytes(t),
		"server/main.py":                       "print('hi')\n",
		"server/__pycache__/main.cpython.pyc":  "bytecode",
		"__MACOSX/._manifest.json":             "junk",
		"vendor/lib/__pycache__/x.cpython.pyc": "bytecode",
		"vendor/lib/x.py":                      "x = 1\n",
	})
	outside := writeTree(t, map[string]string{"secret": "s3cret"})
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "server", "token")); err != nil {
		t.Fatal(err)
	}
	b, err := ReadDirectory(openTree(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range b.Files {
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	if strings.Join(paths, ",") != "manifest.json,server/main.py,server/token,vendor/lib/x.py" {
		t.Errorf("files = %v", paths)
	}
	f := Validate(b.Manifest, b.Files)
	if !f.HasErrors() || !slices.Contains(f.Texts(), "bundle: server/token is a symlink") {
		t.Errorf("findings = %v", f.Texts())
	}
	if err := b.Extract(filepath.Join(t.TempDir(), "out")); err == nil || !strings.Contains(err.Error(), "server/token is not a regular file") {
		t.Errorf("extract of a bundle with a link = %v", err)
	}

	for name, files := range map[string]map[string]string{
		"no manifest": {"server/main.py": "x"},
		"bad json":    {"manifest.json": "{"},
	} {
		if _, err := ReadDirectory(openTree(t, writeTree(t, files))); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	linked := writeTree(t, map[string]string{"server/main.py": "x"})
	if err := os.Symlink(filepath.Join(dir, "manifest.json"), filepath.Join(linked, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDirectory(openTree(t, linked)); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a linked manifest = %v", err)
	}
}

// Why this test exists: a bundle read from a directory is stored by
// copying its files, and the manifest stored in the bundle must be the
// one the check read and the install screen shows. If the copy re-read
// manifest.json, an agent could change it between the check and the
// copy, and the pod would start a server from a manifest the owner
// never saw.
func TestDirectoryBundleExtractKeepsTheManifestItRead(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"manifest.json":  echoManifestBytes(t),
		"server/main.py": "print('hi')\n",
	})
	if err := os.Chmod(filepath.Join(dir, "server", "main.py"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := ReadDirectory(openTree(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(`{"name":"changed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "bundle")
	if err := b.Extract(out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(out, ManifestName))
	if string(got) != string(b.ManifestBytes()) {
		t.Errorf("stored manifest = %s", got)
	}
	info, err := os.Stat(filepath.Join(out, "server", "main.py"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("main.py = %v, %v: the exec bit is kept, as Extract keeps it", info, err)
	}
	if err := b.Extract(out); err == nil {
		t.Error("extract into a directory that is not empty")
	}
}
