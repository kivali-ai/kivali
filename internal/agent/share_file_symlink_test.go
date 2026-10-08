package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// share_file ingests on core, which mounts every agent's tree. An agent
// that plants a link in its own artifacts/ — here alice, pointing at
// bob's private draft with a relative path that stays inside the data
// volume — must not get bob's bytes into a chat.
func TestResolveShareFilePathRefusesSymlinkToSiblingAgent(t *testing.T) {
	s := newTestStore(t)
	for _, slug := range []string{"alice", "bob"} {
		if err := s.CreateAgent(store.Agent{Slug: slug, Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
			t.Fatalf("CreateAgent %s: %v", slug, err)
		}
	}
	aliceRoot := files.StorageRoot(filepath.Join(s.Root(), "agents", "alice"))
	bobRoot := files.StorageRoot(filepath.Join(s.Root(), "agents", "bob"))
	secret := filepath.Join(bobRoot, "artifacts", "private", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("bob's private draft"), 0o644); err != nil {
		t.Fatal(err)
	}
	alicePriv := filepath.Join(aliceRoot, "artifacts", "private")
	if err := os.MkdirAll(alicePriv, 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(alicePriv, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(alicePriv, "x.txt")); err != nil {
		t.Fatal(err)
	}
	// And a symlinked directory on the way, for good measure.
	if err := os.Symlink(filepath.Dir(secret), filepath.Join(alicePriv, "bobdir")); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/files/artifacts/private/x.txt", "/files/artifacts/private/bobdir/secret.txt"} {
		got, err := ResolveShareFilePath(context.Background(), s, "alice", p)
		if err == nil {
			t.Errorf("%s: shared %+v, want refused", p, got)
			continue
		}
		if !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%s: error %q should say it is a symbolic link", p, err)
		}
	}
}
