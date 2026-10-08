package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// body_path can name /files/attachments/<file> on core, as it can in
// the pod: the farm link points into the org-wide blob store, and the
// read is served from the agent's own hardlink dir instead. A link to
// a SHA the agent has not seen (not in its hardlink dir) is not found,
// though the blob exists.
func TestResolveBodyPathReadsAttachmentsFromTheAgentsOwnDir(t *testing.T) {
	data := t.TempDir()
	d := Dispatcher{DataDir: data}
	root := filepath.Join(data, "agents", "alice", StoragePrefix)
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(data, "attachments", "aaa", "canonical.txt"), "alice's upload")
	write(filepath.Join(data, "attachments", "bbb", "canonical.txt"), "bob's upload")
	// Sync's per-agent hardlink dir holds only what alice has seen.
	write(filepath.Join(data, "agents", "alice", "attachments", "aaa", "canonical.txt"), "alice's upload")
	if err := os.MkdirAll(filepath.Join(root, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	for at, sha := range map[string]string{"report.txt": "aaa", "bob.txt": "bbb"} {
		if err := os.Symlink(filepath.Join(data, "attachments", sha, "canonical.txt"), filepath.Join(root, "attachments", at)); err != nil {
			t.Fatal(err)
		}
	}

	resolve := d.ResolveBodyPathFor("alice")
	if got, err := resolve("/files/attachments/report.txt"); err != nil || got != "alice's upload" {
		t.Errorf("report.txt = %q, %v; want alice's upload", got, err)
	}
	if got, err := resolve("/files/attachments/bob.txt"); err == nil {
		t.Errorf("bob.txt = %q, want not found", got)
	}
}

// A link to a file outside every read root reads exactly as a link to
// nothing: core mounts the whole volume, and a different answer would
// tell the agent which paths on it exist.
func TestBackendOutsideRootsReadsAsNotFound(t *testing.T) {
	data := t.TempDir()
	d := Dispatcher{DataDir: data}
	root := filepath.Join(data, "agents", "alice", StoragePrefix)
	if err := os.MkdirAll(filepath.Join(root, "artifacts", "private"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "claude-home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "claude-home", "exists"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for at, target := range map[string]string{
		"there.md":   filepath.Join(data, "claude-home", "exists"),
		"missing.md": filepath.Join(data, "claude-home", "missing"),
	} {
		if err := os.Symlink(target, filepath.Join(root, "artifacts", "private", at)); err != nil {
			t.Fatal(err)
		}
	}
	b := d.BackendFor("alice")
	var msgs []string
	for _, name := range []string{"there.md", "missing.md"} {
		abs, err := b.Resolve("/files/artifacts/private/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = b.readFile(abs, name)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
		_, err = d.ResolveBodyPathFor("alice")("/files/artifacts/private/" + name)
		msgs = append(msgs, strings.ReplaceAll(err.Error(), name, "X"))
	}
	if msgs[0] != msgs[1] {
		t.Errorf("existing and missing targets answer differently: %q vs %q", msgs[0], msgs[1])
	}
}

// Core runs this backend for publish_*'s body_path (and the files
// dispatch endpoint) with the whole data volume mounted. A link the
// agent planted to the credentials or a sibling agent's tree is
// refused; the project farm, which points into project_files/, is not.
func TestDispatcherBackendConfinedOnCore(t *testing.T) {
	data := t.TempDir()
	d := Dispatcher{DataDir: data}
	root := filepath.Join(data, "agents", "alice", StoragePrefix)
	for _, dir := range []string{
		filepath.Join(root, "artifacts", "private"),
		filepath.Join(root, "project"),
		filepath.Join(data, "project_files", "abc"),
		filepath.Join(data, "claude-home", ".claude"),
		filepath.Join(data, "agents", "bob", StoragePrefix, "artifacts", "private"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(data, "project_files", "abc", "plan.txt"), "the plan")
	write(filepath.Join(data, "claude-home", ".claude", ".credentials.json"), "oauth-token")
	write(filepath.Join(data, "agents", "bob", StoragePrefix, "artifacts", "private", "d.md"), "bob's draft")
	write(filepath.Join(root, "artifacts", "private", "draft.md"), "my draft")
	link := func(target, at string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(at))); err != nil {
			t.Fatal(err)
		}
	}
	link(filepath.Join(data, "project_files", "abc", "plan.txt"), "project/plan.txt")
	link(filepath.Join(data, "claude-home", ".claude", ".credentials.json"), "artifacts/private/c.md")
	link("../../../../bob/"+StoragePrefix+"/artifacts/private/d.md", "artifacts/private/b.md")

	resolve := d.ResolveBodyPathFor("alice")
	for path, want := range map[string]string{
		"/files/artifacts/private/draft.md": "my draft",
		"/files/project/plan.txt":           "the plan",
	} {
		got, err := resolve(path)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"/files/artifacts/private/c.md", "/files/artifacts/private/b.md"} {
		if got, err := resolve(path); err == nil {
			t.Errorf("%s = %q, want refused", path, got)
		}
		got, isErr, err := d.Dispatch("alice", ToolView, []byte(`{"path":"`+path+`"}`))
		if err != nil || !isErr || strings.Contains(got, "oauth-token") || strings.Contains(got, "bob's draft") {
			t.Errorf("dispatch view %s = %q (isErr=%v, err=%v), want a refusal", path, got, isErr, err)
		}
	}
}
