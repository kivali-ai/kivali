package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The pass runs on core over trees the agents write. A link anywhere
// on the way to a file — a linked subdirectory under public/, or
// public/ itself replaced with a link — is never followed, so no node,
// snapshot or version can come from outside the owner's own tree.
func TestRefreshGraphNeverFollowsLinkedDirectories(t *testing.T) {
	s := graphStore(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.md"), []byte("---\nid: secret\n---\nnot yours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "blob.bin"), []byte("binary secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	// vp: a linked subdirectory inside a real public/.
	writePublic(t, s, "vp", "real.md", "---\nid: real\n---\nx\n")
	if err := os.Symlink(outside, filepath.Join(publicDir(s, "vp"), "sub")); err != nil {
		t.Fatal(err)
	}
	// cs: public/ itself is a link.
	if err := os.RemoveAll(publicDir(s, "cs")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(publicDir(s, "cs")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, publicDir(s, "cs")); err != nil {
		t.Fatal(err)
	}

	ix, _, err := scanGraph(s)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, id := range []string{"vp/secret", "vp/sub/secret", "vp/sub/blob.bin", "cs/secret", "cs/blob.bin"} {
		if _, ok := ix.Get(id); ok {
			t.Errorf("%s must not be indexed", id)
		}
	}
	if _, ok := ix.Get("vp/real"); !ok {
		t.Error("the real file should still be indexed")
	}
	if got := len(versionLines(t, s)); got != 1 {
		t.Errorf("only the real file should be versioned, got %d lines", got)
	}
}
