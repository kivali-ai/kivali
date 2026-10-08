package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every write lands in one rename: a reader (the agent's next
// file_view, or the knowledge-graph pass hashing public/ at a peer's
// wake) sees the old bytes or the new bytes, never a truncated file.
// The temp file is a dotfile beside the target and does not outlive
// the write.
func TestWritesAreAtomicAndLeaveNoTempFiles(t *testing.T) {
	b := newBackend(t)
	dir := filepath.Join(b.Root, "notes")
	noTemp := func(when string) {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Errorf("%s: temp file %s left behind", when, e.Name())
			}
		}
	}
	read := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "a.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if err := b.Create("/files/notes/a.md", "one\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	noTemp("create")
	if err := b.Create("/files/notes/a.md", "replaced\n"); err != nil {
		t.Fatal(err)
	}
	noTemp("overwrite")
	if got := read(); got != "replaced\n" {
		t.Fatalf("after overwrite: %q", got)
	}
	if err := b.StrReplace("/files/notes/a.md", "replaced", "edited"); err != nil {
		t.Fatal(err)
	}
	noTemp("str_replace")
	if err := b.Insert("/files/notes/a.md", 0, "first"); err != nil {
		t.Fatal(err)
	}
	noTemp("insert")
	if got := read(); got != "first\nedited\n" {
		t.Fatalf("after edits: %q", got)
	}
	if err := b.Copy("/files/notes/a.md", "/files/notes/b.md"); err != nil {
		t.Fatal(err)
	}
	noTemp("copy")
	info, err := os.Stat(filepath.Join(dir, "b.md"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("copied file mode = %v, want 0644", info.Mode().Perm())
	}
}
