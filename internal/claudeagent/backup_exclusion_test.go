package claudeagent_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/backup"
	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/files"
)

// A backup never carries credentials. The model provider keeps its
// sign-in in the home directory it names (the Claude CLI's OAuth tokens
// in claude-home/), which every agent pod mounts too; the exclusion list
// names that directory by hand, so this ties the two together: a
// provider that moves its home moves the exclusion with it or fails here.
func TestBackupLeavesOutTheProvidersCredentialHome(t *testing.T) {
	home := claudeagent.New(claudeagent.Options{HomeDir: t.TempDir()}).Credentials().HomeDir()
	if home == "" || !files.ShouldExcludeFromBackup(home) {
		t.Fatalf("the provider keeps credentials in %q, which a backup does not leave out", home)
	}

	root := t.TempDir()
	for rel, body := range map[string]string{
		home + "/.claude/.credentials.json":        `{"claudeAiOauth":{"accessToken":"SECRET-ACCESS","refreshToken":"SECRET-REFRESH"}}`,
		home + "/.claude.json":                     `{"oauthAccount":"SECRET-ACCOUNT"}`,
		home + "/.claude/projects/-/session.jsonl": "SECRET-SESSION",
		"system_instructions.md":                   "# rules\n",
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
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, home) {
			t.Errorf("the manifest lists %s", f.Path)
		}
	}
	// The zip stores member names in the clear, so its raw bytes show
	// any member stored under the home, directories included.
	if bytes.Contains(buf.Bytes(), []byte(home+"/")) {
		t.Errorf("a member under %s/ was stored", home)
	}
}
