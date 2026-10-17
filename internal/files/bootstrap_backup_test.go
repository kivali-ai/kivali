package files_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/backup"
	"github.com/kivali-ai/kivali/internal/files"
)

// An agent's browser sessions hold its site sign-ins: the saved profile
// and the cookie snapshot of each (internal/store/browser.go). They are
// credentials, and a backup leaves them out as it leaves out the Claude
// sign-in, for an active agent and an archived one alike. The agent's
// own files beside them stay in.
func TestShouldExcludeFromBackupLeavesOutBrowserSessions(t *testing.T) {
	for _, rel := range []string{
		"agents/alice/browser",
		"agents/alice/browser/vendor.com/profile.tar.zst",
		"agents/alice/browser/vendor.com/cookies.json",
		"agents/alice/browser/open/session.json",
		"agents/_archived/bob/browser/vendor.com/profile.tar.zst",
	} {
		if !files.ShouldExcludeFromBackup(rel) {
			t.Errorf("%s is in the backup; it holds a sign-in", rel)
		}
	}
	for _, rel := range []string{
		"agents/alice/chat.jsonl",
		"agents/alice/agent.yaml",
		"agents/alice/memory/browser",
		"agents/alice/memory/browser/notes.md",
		"agents/_archived/bob/agent.yaml",
		"agents/_archived",
		"browser",
		"project/browser/readme.md",
	} {
		if files.ShouldExcludeFromBackup(rel) {
			t.Errorf("%s was left out of the backup", rel)
		}
	}
}

// The zip a backup writes carries no member under an agent's browser
// directory, stored names included.
func TestBackupZipLeavesOutBrowserSessions(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		"agents/alice/agent.yaml":                         "slug: alice\n",
		"agents/alice/browser/vendor.com/session.json":    `{"name":"vendor.com"}`,
		"agents/alice/browser/vendor.com/profile.tar.zst": "SECRET-PROFILE",
		"agents/alice/browser/vendor.com/cookies.json":    `[{"name":"sid","value":"SECRET-COOKIE"}]`,
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	m, err := backup.WriteZip(root, &buf, "test")
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, f := range m.Files {
		kept = append(kept, f.Path)
		if strings.Contains(f.Path, "/browser/") {
			t.Errorf("the manifest lists %s", f.Path)
		}
	}
	if len(kept) != 1 || kept[0] != "agents/alice/agent.yaml" {
		t.Errorf("manifest = %v, want only the agent's record", kept)
	}
	if bytes.Contains(buf.Bytes(), []byte("agents/alice/browser")) || bytes.Contains(buf.Bytes(), []byte("SECRET-")) {
		t.Error("a browser session member was stored")
	}
}
