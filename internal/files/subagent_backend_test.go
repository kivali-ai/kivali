package files

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSubagentBackendForReadsTheSubagentsOwnView: core reads a
// subagent's files through the subagent's overlay, so its own
// artifacts/private is there, the parent's artifacts/private and
// past-chats are not, the shared workspace is reached through the
// overlay's link, and a link out of the tree is no file.
func TestSubagentBackendForReadsTheSubagentsOwnView(t *testing.T) {
	data := t.TempDir()
	d := Dispatcher{DataDir: data}
	parent := filepath.Join(data, "agents", "alice", StoragePrefix)
	overlay := filepath.Join(parent, "subagents", "sub1")
	for _, dir := range []string{
		filepath.Join(parent, "artifacts", "private"),
		filepath.Join(parent, "past-chats"),
		filepath.Join(data, "claude-home"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := BuildSubagentOverlay(SubagentOverlay{Root: overlay}); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(overlay, "artifacts", "private", "quote.pdf"), "the subagent's")
	write(filepath.Join(parent, "artifacts", "private", "secret.pdf"), "the agent's")
	write(filepath.Join(parent, "past-chats", "2026-01.md"), "a past chat")
	write(filepath.Join(parent, SharedWorkspaceDir, "notes.md"), "shared")
	write(filepath.Join(data, "claude-home", "credentials.json"), "oauth-token")
	if err := os.Symlink(filepath.Join(data, "claude-home", "credentials.json"), filepath.Join(overlay, "artifacts", "private", "leak.pdf")); err != nil {
		t.Fatal(err)
	}

	b := d.SubagentBackendFor("alice", "sub1")
	read := func(p string) (string, error) {
		f, info, err := b.OpenRead(p)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		if !info.Mode().IsRegular() {
			return "", errors.New("not a regular file")
		}
		body := make([]byte, info.Size())
		_, err = f.Read(body)
		return string(body), err
	}
	for path, want := range map[string]string{
		"/files/artifacts/private/quote.pdf":         "the subagent's",
		"/files/" + SharedWorkspaceDir + "/notes.md": "shared",
	} {
		if got, err := read(path); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{
		"/files/artifacts/private/secret.pdf",
		"/files/past-chats/2026-01.md",
		"/files/artifacts/private/leak.pdf",
		"/files/subagents/sub1/artifacts/private/quote.pdf",
	} {
		if got, err := read(path); err == nil {
			t.Errorf("%s = %q, want not found", path, got)
		}
	}
}
