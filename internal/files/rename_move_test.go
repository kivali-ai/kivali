package files

import (
	"os"
	"path/filepath"
	"testing"
)

// file_rename falls back to moveFileIn when rename(2) answers EXDEV,
// which it does in the agent pod for a move between /files/artifacts/
// (a mount of its own) and the rest of /files/. A file arrives whole
// and leaves its old name; a directory is refused rather than moved
// by halves.
func TestMoveFileInMovesAFileAndRefusesADirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "artifacts", "private", "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "draft.md"), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if err := moveFileIn(root, "draft.md", "artifacts/private/draft.md"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(base, "artifacts", "private", "draft.md")); err != nil || string(b) != "draft" {
		t.Errorf("moved file = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "draft.md")); !os.IsNotExist(err) {
		t.Errorf("source still there: %v", err)
	}

	if err := moveFileIn(root, "artifacts/private/d", "d"); err == nil {
		t.Error("moving a directory succeeded, want refused")
	}
	if _, err := os.Stat(filepath.Join(base, "artifacts", "private", "d")); err != nil {
		t.Errorf("refused directory move disturbed the source: %v", err)
	}
}
