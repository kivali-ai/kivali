package zipsafe

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string]string) *zip.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

// Each limit stops an extraction that passes it, whatever the zip
// declares: a zip bomb's declared sizes are not what its entries
// inflate to, so the caps count the bytes written. Without them a
// crafted upload fills the data volume.
func TestExtractLimits(t *testing.T) {
	zr := zipOf(t, map[string]string{"a": strings.Repeat("x", 100), "b": strings.Repeat("y", 100), "c": "z"})
	cases := map[string]struct {
		lim  Limits
		want string
	}{
		"per file": {Limits{PerFile: 50}, "exceeds 50-byte per-file cap"},
		"total":    {Limits{Total: 150}, "bundle exceeds 150-byte total cap"},
		"files":    {Limits{Files: 2}, "more than 2 files"},
	}
	for name, c := range cases {
		err := Extract(zr, "", t.TempDir(), c.lim, "test zip")
		if err == nil || !strings.HasPrefix(err.Error(), "test zip: ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
		if name == "files" && !errors.Is(err, ErrTooMany) {
			t.Errorf("files: %v is not ErrTooMany", err)
		}
	}
	dst := t.TempDir()
	if err := Extract(zr, "", dst, Limits{PerFile: 100, Total: 201, Files: 3}, "test zip"); err != nil {
		t.Fatalf("within the limits: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "b")); len(b) != 100 {
		t.Errorf("b = %d bytes", len(b))
	}
}

// Prefix finds the root of a zip made flat or from one folder, and
// none for any other layout, ignoring macOS's metadata folder; a wrong
// root would put every file one level off, entry point included.
func TestPrefix(t *testing.T) {
	cases := []struct {
		files  map[string]string
		prefix string
		ok     bool
	}{
		{map[string]string{"m.json": "", "x/y": ""}, "", true},
		{map[string]string{"top/m.json": "", "top/x": "", "__MACOSX/top/._m.json": ""}, "top/", true},
		{map[string]string{"a/m.json": "", "b/x": ""}, "", false},
	}
	for _, c := range cases {
		p, ok := Prefix(zipOf(t, c.files), "m.json")
		if p != c.prefix || ok != c.ok {
			t.Errorf("Prefix(%v) = %q, %v; want %q, %v", c.files, p, ok, c.prefix, c.ok)
		}
	}
}
