package files

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// publishedReadOnlyBackend is a Backend over a tree with both published
// mount points, a private workspace and one core-managed farm.
func publishedReadOnlyBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"artifacts/public", "artifacts/shared/peer", "artifacts/private", "project"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "private", "draft.md"), []byte("draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Backend{Root: root}
}

// An agent that writes a published tree the way it writes its workspace
// is told how files get there, and the error is still ErrReadOnly to
// every caller that checks for it.
func TestWritingAPublishedTreeNamesArtifactPublish(t *testing.T) {
	b := publishedReadOnlyBackend(t)
	cases := map[string]error{
		"create":      b.Create("/files/artifacts/public/spec.md", "x"),
		"str_replace": b.StrReplace("/files/artifacts/public/spec.md", "a", "b"),
		"insert":      b.Insert("/files/artifacts/shared/peer/x.md", 0, "x"),
		"delete":      b.Delete("/files/artifacts/shared/peer/x.md"),
		"copy":        b.Copy("/files/artifacts/private/draft.md", "/files/artifacts/public/draft.md"),
		"rename":      b.Rename("/files/artifacts/private/draft.md", "/files/artifacts/public/draft.md"),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrPublishedReadOnly) || !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s: err = %v, want ErrPublishedReadOnly wrapping ErrReadOnly", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "artifact_publish") {
			t.Errorf("%s: %q does not name artifact_publish", name, err)
		}
	}
}

// A write that reaches a published tree only through a link in the
// workspace gets the same guidance from the real-path check.
func TestWritingThroughALinkIntoAPublishedTreeNamesArtifactPublish(t *testing.T) {
	b := publishedReadOnlyBackend(t)
	if err := os.Symlink("../public", filepath.Join(b.Root, "artifacts", "private", "pub")); err != nil {
		t.Fatal(err)
	}
	err := b.Create("/files/artifacts/private/pub/spec.md", "x")
	if !errors.Is(err, ErrPublishedReadOnly) {
		t.Fatalf("Create through a link: err = %v, want ErrPublishedReadOnly", err)
	}
}

// The other read-only trees keep the plain error: artifact_publish is
// no way into them.
func TestWritingACoreManagedFarmKeepsThePlainError(t *testing.T) {
	b := publishedReadOnlyBackend(t)
	err := b.Create("/files/project/x.md", "x")
	if !errors.Is(err, ErrReadOnly) || errors.Is(err, ErrPublishedReadOnly) {
		t.Fatalf("Create in project/: err = %v, want plain ErrReadOnly", err)
	}
}

// The text the model receives from file_create carries the guidance.
func TestFileCreateResultNamesArtifactPublish(t *testing.T) {
	b := publishedReadOnlyBackend(t)
	raw, err := json.Marshal(map[string]string{"path": "/files/artifacts/public/spec.md", "file_text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	body, isError, err := Dispatch(b, ToolCreate, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !isError || !strings.Contains(body, "artifact_publish") {
		t.Fatalf("file_create result = %q (isError %v), want a tool error naming artifact_publish", body, isError)
	}
}
