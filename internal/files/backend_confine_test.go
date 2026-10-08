package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// podLike lays out a data volume the way the agent pod sees it:
// alice's /files/ (Root), the read-only farm targets it may read
// through, a sibling agent's tree and the Claude credentials it may
// not. Returns the backend and the paths the tests poke at.
type podLike struct {
	b       *Backend
	data    string
	creds   string
	credDir string
	bobFile string
}

func newPodLike(t *testing.T) podLike {
	t.Helper()
	data := t.TempDir()
	root := filepath.Join(data, "agents", "alice", "memory")
	for _, d := range []string{
		filepath.Join(root, "project"),
		filepath.Join(root, "artifacts", "private"),
		filepath.Join(root, "artifacts", "public"),
		filepath.Join(root, SharedWorkspaceDir),
		filepath.Join(data, "project_files", "abc"),
		filepath.Join(data, "claude-home", ".claude"),
		filepath.Join(data, "agents", "bob", "memory", "artifacts", "private"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(data, "project_files", "abc", "plan.txt"), "the plan\n")
	creds := filepath.Join(data, "claude-home", ".claude", ".credentials.json")
	write(creds, "oauth-token")
	bobFile := filepath.Join(data, "agents", "bob", "memory", "artifacts", "private", "secret.txt")
	write(bobFile, "bob's secret")
	// The farm link core makes: absolute, into a read root.
	if err := os.Symlink(filepath.Join(data, "project_files", "abc", "plan.txt"), filepath.Join(root, "project", "plan.txt")); err != nil {
		t.Fatal(err)
	}
	return podLike{
		b: &Backend{
			Root:      root,
			ReadRoots: []string{filepath.Join(data, "project_files")},
		},
		data:    data,
		creds:   creds,
		credDir: filepath.Dir(creds),
		bobFile: bobFile,
	}
}

// plant makes a link at /files/<rel> the way run_shell would.
func (p podLike) plant(t *testing.T, rel, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(p.b.Root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func (p podLike) credsIntact(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(p.creds)
	if err != nil || string(b) != "oauth-token" {
		t.Errorf("credentials changed: %q, %v", b, err)
	}
	entries, _ := os.ReadDir(p.credDir)
	if len(entries) != 1 {
		t.Errorf("credentials dir has %d entries, want 1 (something was written there)", len(entries))
	}
}

// The farm links core makes still work: they resolve into a read root.
func TestBackendReadsFarmLinkIntoReadRoot(t *testing.T) {
	p := newPodLike(t)
	got, err := p.b.View("/files/project/plan.txt", ViewOptions{})
	if err != nil {
		t.Fatalf("View through farm link: %v", err)
	}
	if !strings.Contains(got, "the plan") {
		t.Errorf("View = %q", got)
	}
	if err := p.b.Copy("/files/project/plan.txt", "/files/artifacts/private/plan.txt"); err != nil {
		t.Fatalf("Copy from farm link: %v", err)
	}
	if list, err := p.b.View("/files/project", ViewOptions{}); err != nil || !strings.Contains(list, "plan.txt") {
		t.Errorf("listing project/: %q, %v", list, err)
	}
}

// A link run_shell planted to the credentials or a sibling agent is not
// followed by any read: view, copy, image view, directory listing.
func TestBackendRefusesReadsThroughLinksOutsideItsRoots(t *testing.T) {
	p := newPodLike(t)
	p.plant(t, "artifacts/private/creds.json", p.creds)
	p.plant(t, "artifacts/private/creds.png", p.creds)
	p.plant(t, "artifacts/private/claude", p.credDir)
	rel, err := filepath.Rel(filepath.Join(p.b.Root, "artifacts", "private"), p.bobFile)
	if err != nil {
		t.Fatal(err)
	}
	p.plant(t, "artifacts/private/bob.txt", rel)

	for _, path := range []string{
		"/files/artifacts/private/creds.json",
		"/files/artifacts/private/claude/.credentials.json",
		"/files/artifacts/private/claude",
		"/files/artifacts/private/bob.txt",
	} {
		if got, err := p.b.View(path, ViewOptions{}); err == nil {
			t.Errorf("View %s = %q, want refused", path, got)
		}
	}
	if err := p.b.Copy("/files/artifacts/private/creds.json", "/files/artifacts/private/stolen.json"); err == nil {
		t.Error("Copy of a link to the credentials succeeded")
	}
	if _, err := os.Lstat(filepath.Join(p.b.Root, "artifacts", "private", "stolen.json")); err == nil {
		t.Error("Copy left a destination file behind")
	}
	if img, ok, err := TryImageView(p.b, "/files/artifacts/private/creds.png"); err == nil {
		t.Errorf("TryImageView of a link to the credentials = %v, %v; want an error", img, ok)
	}
	// StrReplace reads before it writes: refused, and the target
	// untouched.
	if err := p.b.StrReplace("/files/artifacts/private/creds.json", "oauth", "x"); err == nil {
		t.Error("StrReplace through a link to the credentials succeeded")
	}
	p.credsIntact(t)
}

// Writes through a symlinked directory cannot land outside the tree.
func TestBackendRefusesWritesThroughSymlinkedDir(t *testing.T) {
	p := newPodLike(t)
	p.plant(t, "artifacts/private/out", p.credDir)
	if err := p.b.Create("/files/artifacts/private/out/x.md", "pwned"); err == nil {
		t.Error("Create through a symlinked dir succeeded")
	}
	if err := p.b.Create("/files/artifacts/private/out/new/x.md", "pwned"); err == nil {
		t.Error("Create (with new parents) through a symlinked dir succeeded")
	}
	if err := p.b.Create("/files/artifacts/private/local.md", "mine"); err != nil {
		t.Fatal(err)
	}
	if err := p.b.Copy("/files/artifacts/private/local.md", "/files/artifacts/private/out/copied.md"); err == nil {
		t.Error("Copy into a symlinked dir succeeded")
	}
	if err := p.b.Rename("/files/artifacts/private/local.md", "/files/artifacts/private/out/moved.md"); err == nil {
		t.Error("Rename into a symlinked dir succeeded")
	}
	if err := p.b.Delete("/files/artifacts/private/out/.credentials.json"); err == nil {
		t.Error("Delete through a symlinked dir succeeded")
	}
	if err := p.b.Rename("/files/artifacts/private/out/.credentials.json", "/files/artifacts/private/got.json"); err == nil {
		t.Error("Rename out of a symlinked dir succeeded")
	}
	p.credsIntact(t)
}

// A write at a path that is itself a link replaces the link; the file
// it pointed at is never written.
func TestBackendWriteReplacesALinkRatherThanWritingThroughIt(t *testing.T) {
	p := newPodLike(t)
	p.plant(t, "artifacts/private/c.json", p.creds)
	if err := p.b.Create("/files/artifacts/private/c.json", "new"); err != nil {
		t.Fatalf("Create over a link: %v", err)
	}
	info, err := os.Lstat(filepath.Join(p.b.Root, "artifacts", "private", "c.json"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("c.json should now be a regular file: %v, %v", info, err)
	}
	p.credsIntact(t)
}

// Links the agent makes inside its own tree keep working, relative or
// absolute (`ln -s /files/background pub` in the pod).
func TestBackendWritesThroughLinksInsideItsOwnTree(t *testing.T) {
	p := newPodLike(t)
	p.plant(t, "artifacts/private/pub", filepath.Join(p.b.Root, SharedWorkspaceDir))
	p.plant(t, "artifacts/private/rel", "../../"+SharedWorkspaceDir)
	for _, path := range []string{"/files/artifacts/private/pub/a.md", "/files/artifacts/private/rel/b.md"} {
		if err := p.b.Create(path, "x"); err != nil {
			t.Errorf("Create %s: %v", path, err)
		}
	}
	for _, name := range []string{"a.md", "b.md"} {
		if _, err := os.Lstat(filepath.Join(p.b.Root, SharedWorkspaceDir, name)); err != nil {
			t.Errorf("%s did not land in background/: %v", name, err)
		}
	}
	if err := p.b.Rename("/files/artifacts/private/pub/a.md", "/files/artifacts/private/moved.md"); err != nil {
		t.Errorf("Rename out through a link: %v", err)
	}
}

// A link that stays inside /files/ but leads into a read-only subtree
// does not make that subtree writable.
func TestBackendRefusesWritesIntoReadOnlySubtreeViaLink(t *testing.T) {
	p := newPodLike(t)
	p.plant(t, "artifacts/private/p", "../../project")
	if err := p.b.Create("/files/artifacts/private/p/new.md", "x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Create into project/ via link: err = %v, want ErrReadOnly", err)
	}
	if err := p.b.Delete("/files/artifacts/private/p/plan.txt"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete of a farm link via link: err = %v, want ErrReadOnly", err)
	}
	if _, err := os.Lstat(filepath.Join(p.b.Root, "project", "new.md")); err == nil {
		t.Error("a file was created in project/")
	}
	if _, err := os.Lstat(filepath.Join(p.b.Root, "project", "plan.txt")); err != nil {
		t.Errorf("farm link was removed: %v", err)
	}
}

// A subagent's overlay links resolve into its parent's tree: reads of
// the parent's farms and writes to the shared background/ both work
// with the parent's /files/ as a WriteRoot, and the credentials stay
// out of reach.
func TestSubagentBackendFollowsOverlayLinksOnly(t *testing.T) {
	p := newPodLike(t)
	if err := os.MkdirAll(filepath.Join(p.b.Root, SharedWorkspaceDir), 0o755); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(p.b.Root, "subagents", "ab12")
	if err := BuildSubagentOverlay(SubagentOverlay{Root: overlay}); err != nil {
		t.Fatal(err)
	}
	sub := &Backend{Root: overlay, ReadRoots: p.b.ReadRoots, WriteRoots: []string{SubagentParentRoot(overlay)}}
	if got, err := sub.View("/files/project/plan.txt", ViewOptions{}); err != nil || !strings.Contains(got, "the plan") {
		t.Errorf("subagent View of project file: %q, %v", got, err)
	}
	if err := sub.Create("/files/background/notes.md", "shared"); err != nil {
		t.Errorf("subagent write to background/: %v", err)
	}
	if err := sub.Create("/files/artifacts/private/own.md", "own"); err != nil {
		t.Errorf("subagent write to its private dir: %v", err)
	}
	if err := os.Symlink(p.credDir, filepath.Join(overlay, "artifacts", "private", "out")); err != nil {
		t.Fatal(err)
	}
	if err := sub.Create("/files/artifacts/private/out/x", "pwned"); err == nil {
		t.Error("subagent Create through a symlinked dir succeeded")
	}
	if _, err := sub.View("/files/artifacts/private/out/.credentials.json", ViewOptions{}); err == nil {
		t.Error("subagent View through a symlinked dir succeeded")
	}
	p.credsIntact(t)
}
