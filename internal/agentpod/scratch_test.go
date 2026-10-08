package agentpod

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func topLevel(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// The agent half starts empty at every pod start, whatever is in it:
// everything the runtime keeps there is regenerable.
func TestPrepareScratchEmptiesAgentHalfEveryStart(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ScratchAgentSubPath, "cache", "path", "ab", "abc", "body"), "planted")
	ro := filepath.Join(root, ScratchAgentSubPath, "locked")
	writeTestFile(t, filepath.Join(ro, "f"), "x")
	writeTestFile(t, filepath.Join(ro, "sealed", "g"), "y")
	if err := os.Chmod(filepath.Join(ro, "sealed"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	// Should the sweep fail, leave the tree removable for TempDir's
	// cleanup.
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})

	if err := PrepareScratch(root, t.Logf); err != nil {
		t.Fatal(err)
	}
	if got := topLevel(t, filepath.Join(root, ScratchAgentSubPath)); len(got) != 0 {
		t.Errorf("agent half = %v, want empty", got)
	}
	// Nothing of the old agent half reaches the shell.
	if got := topLevel(t, filepath.Join(root, ScratchShellSubPath)); len(got) != 0 {
		t.Errorf("shell half = %v, want empty", got)
	}
	// The unwritable directories were opened up on the way down and
	// the old half removed, trash and all.
	want := []string{ScratchAgentSubPath, ScratchShellSubPath}
	sort.Strings(want)
	if got := topLevel(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("volume root = %v, want %v", got, want)
	}
}

// A link under either name is replaced by a real directory (kubelet
// refuses a subPath with a link in it), and what it pointed at is
// untouched.
func TestPrepareScratchReplacesLinksWithRealDirs(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "keep"), "outside")
	for _, name := range []string{ScratchAgentSubPath, ScratchShellSubPath} {
		if err := os.Symlink(outside, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareScratch(root, t.Logf); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ScratchAgentSubPath, ScratchShellSubPath} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !info.IsDir() {
			t.Errorf("%s: %v, %v; want a real directory", name, info, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(b) != "outside" {
		t.Errorf("link target touched: %q, %v", b, err)
	}
}

// The shell half is kept across starts, and the second run is
// idempotent.
func TestPrepareScratchKeepsShellHalf(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ScratchShellSubPath, "home", ".bashrc"), "alias ll='ls -l'")
	for i := 0; i < 2; i++ {
		if err := PrepareScratch(root, t.Logf); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, ScratchShellSubPath, "home", ".bashrc")); err != nil || string(b) != "alias ll='ls -l'" {
		t.Errorf("shell home = %q, %v", b, err)
	}
	want := []string{ScratchAgentSubPath, ScratchShellSubPath}
	sort.Strings(want)
	if got := topLevel(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("volume root = %v, want %v", got, want)
	}
}

// A directory is read in batches: an agent half with more entries than
// one batch is removed whole.
func TestPrepareScratchRemovesMoreThanOneBatch(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < scratchReadBatch+10; i++ {
		writeTestFile(t, filepath.Join(root, ScratchAgentSubPath, fmt.Sprintf("f%05d", i)), "x")
	}
	if err := PrepareScratch(root, t.Logf); err != nil {
		t.Fatal(err)
	}
	if got := topLevel(t, filepath.Join(root, ScratchAgentSubPath)); len(got) != 0 {
		t.Errorf("agent half holds %d entries, want none", len(got))
	}
	if got := len(topLevel(t, root)); got != 2 {
		t.Errorf("volume root holds %d entries, want the two halves", got)
	}
}
