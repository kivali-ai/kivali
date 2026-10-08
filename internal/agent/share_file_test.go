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

// newTestStore returns an FSStore rooted at t.TempDir(). Tests own
// the lifecycle.
func newTestStore(t *testing.T) *store.FSStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

// TestResolveShareFilePathArtifactsPrivate: a nested path under
// /files/artifacts/private/ resolves to a content-addressed SHA via
// AddAttachment.
func TestResolveShareFilePathArtifactsPrivate(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Plant a file in the agent's writable artifacts/private/ tree.
	agentRoot := filepath.Join(s.Root(), "agents", "alice")
	memRoot := files.StorageRoot(agentRoot)
	relPath := "artifacts/private/HDP_v1_0_0_RC6.tar.gz"
	abs := filepath.Join(memRoot, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("tarball-bytes-here"), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveShareFilePath(context.Background(), s, "alice", "/files/"+relPath)
	if err != nil {
		t.Fatalf("ResolveShareFilePath: %v", err)
	}
	if resolved.SHA == "" {
		t.Error("expected SHA, got empty")
	}
	if resolved.Name != "HDP_v1_0_0_RC6.tar.gz" {
		t.Errorf("name = %q, want HDP_v1_0_0_RC6.tar.gz (basename of path)", resolved.Name)
	}
	// SHA must be retrievable via the store — the resolver content-
	// addressed it via AddAttachment.
	if _, err := s.GetAttachment(resolved.SHA); err != nil {
		t.Errorf("attachment %s not in store after share_file path: %v", resolved.SHA, err)
	}
}

// TestResolveShareFilePathArtifactsPublic: /files/artifacts/public/<p>
// is the agent's published tree, public/<slug>/<p> on the data volume
// (the pod mounts it there), and shares from there — never from the
// empty mount point in the agent's own tree, and never from a peer's.
func TestResolveShareFilePathArtifactsPublic(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	abs := filepath.Join(files.PublishedDir(s.Root(), "alice"), "specs", "spec.md")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("# spec\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	peer := filepath.Join(files.PublishedDir(s.Root(), "bob"), "b.md")
	if err := os.MkdirAll(filepath.Dir(peer), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peer, []byte("bob's"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveShareFilePath(context.Background(), s, "alice", "/files/artifacts/public/specs/spec.md")
	if err != nil {
		t.Fatalf("ResolveShareFilePath: %v", err)
	}
	if resolved.Name != "spec.md" {
		t.Errorf("name = %q, want spec.md", resolved.Name)
	}
	att, err := s.GetAttachment(resolved.SHA)
	if err != nil {
		t.Errorf("attachment %s missing: %v", resolved.SHA, err)
	}
	if att.Size != int64(len("# spec\n")) {
		t.Errorf("shared %d bytes, want the published file's", att.Size)
	}
	for _, p := range []string{"/files/artifacts/public/../bob/b.md", "/files/artifacts/public/missing.md"} {
		if _, err := ResolveShareFilePath(context.Background(), s, "alice", p); err == nil {
			t.Errorf("ResolveShareFilePath(%s) succeeded", p)
		}
	}
}

// TestResolveShareFilePathArtifactsSharedRejected proves an agent
// can't share another agent's published artifact through this path
// — artifacts/shared/ is a read-only mirror, not the agent's own
// content. Allowing it would let agents re-attribute peer files.
func TestResolveShareFilePathArtifactsSharedRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	_, err := ResolveShareFilePath(context.Background(), s, "alice", "/files/artifacts/shared/bob/foo.md")
	if err == nil {
		t.Error("expected rejection of /files/artifacts/shared/ path; got success")
	}
}

// TestResolveShareFilePathTraversalRejected proves the resolver
// honors files.Backend.Resolve's `..` escape protection. A crafted
// path like /files/artifacts/private/../../../etc/passwd must not
// reach AddAttachment.
func TestResolveShareFilePathTraversalRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	_, err := ResolveShareFilePath(context.Background(), s, "alice", "/files/artifacts/private/../../../etc/passwd")
	if err == nil {
		t.Error("expected rejection of traversal path; got success")
	}
}

// TestResolveShareFilePathMissingFile exercises the "you said path X
// but X doesn't exist" failure mode. The error should mention the
// path so the agent can correct it.
func TestResolveShareFilePathMissingFile(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	_, err := ResolveShareFilePath(context.Background(), s, "alice", "/files/artifacts/private/nope.md")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "nope.md") {
		t.Errorf("error %q should mention nope.md so agent can correct", err.Error())
	}
}

// TestResolveShareFilePathIdempotent — re-sharing the same bytes
// returns the same SHA. AddAttachment is content-addressed, so the
// second call no-ops at the bytes level.
func TestResolveShareFilePathIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	memRoot := files.StorageRoot(filepath.Join(s.Root(), "agents", "alice"))
	abs := filepath.Join(memRoot, "artifacts/private/x.txt")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("identical bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved1, err := ResolveShareFilePath(context.Background(), s, "alice", "/files/artifacts/private/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	resolved2, err := ResolveShareFilePath(context.Background(), s, "alice", "/files/artifacts/private/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if resolved1.SHA != resolved2.SHA {
		t.Errorf("sha1=%q sha2=%q; identical bytes should produce identical SHA", resolved1.SHA, resolved2.SHA)
	}
}

// TestResolveShareFilePathProjectAndAttachmentsStillFlat asserts the
// flat-path subtrees haven't changed: project/ and attachments/
// reject nested paths so a name with slashes is an unambiguous
// "this is wrong" rather than silently accepted.
func TestResolveShareFilePathProjectAndAttachmentsStillFlat(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "R", ReportsTo: "ceo"}, "seed"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	for _, p := range []string{
		"/files/project/sub/file.md",
		"/files/attachments/sub/file.md",
	} {
		_, err := ResolveShareFilePath(context.Background(), s, "alice", p)
		if err == nil {
			t.Errorf("%s: expected rejection of nested path; got success", p)
		} else if !strings.Contains(err.Error(), "nested") {
			t.Errorf("%s: error should mention 'nested'; got %q", p, err.Error())
		}
	}
}
