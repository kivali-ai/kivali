//go:build unix

package claudeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// fakeCLI writes a `claude` stand-in that prints out for `auth status
// --json` and exits with code, and returns its path.
func fakeCLI(t *testing.T, out string, code int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		`[ "$1 $2 $3" = "auth status --json" ] || { echo "unexpected: $*" >&2; exit 2; }` + "\n" +
		`cat "$(dirname "$0")/status.json"` + "\n" +
		"exit " + string(rune('0'+code)) + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestCredentialsStatusAsksTheCLI: the status is what `claude auth
// status --json` reports, signed out (exit 1) as well as signed in; the
// home directory agent pods mount keeps its name.
func TestCredentialsStatusAsksTheCLI(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		code      int
		want      provider.CredentialStatus
	}{
		{"signed out", `{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`, 1,
			provider.CredentialStatus{}},
		{"subscription", `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"owner@example.com","orgName":"Example","subscriptionType":"max"}`, 0,
			provider.CredentialStatus{Present: true, Who: "owner@example.com", Billing: "Claude Max"}},
		{"bedrock", `{"loggedIn":true,"authMethod":"third_party","apiProvider":"bedrock"}`, 0,
			provider.CredentialStatus{Present: true, Billing: "Amazon Bedrock"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := New(Options{ClaudeBinary: fakeCLI(t, tc.out, tc.code)}).Credentials()
			if creds.HomeDir() != "claude-home" {
				t.Fatalf("HomeDir = %q", creds.HomeDir())
			}
			st, err := creds.Status(context.Background())
			if err != nil || st != tc.want {
				t.Fatalf("Status = %+v, %v; want %+v", st, err, tc.want)
			}
		})
	}
}

// TestCredentialsStatusErrors: output that is not the CLI's JSON, and a
// CLI that is not there, are errors rather than "signed out".
func TestCredentialsStatusErrors(t *testing.T) {
	creds := New(Options{ClaudeBinary: fakeCLI(t, "error: unknown command auth", 1)}).Credentials()
	if _, err := creds.Status(context.Background()); err == nil || !strings.Contains(err.Error(), "claude auth status") {
		t.Fatalf("garbage output: %v", err)
	}
	creds = New(Options{ClaudeBinary: filepath.Join(t.TempDir(), "missing")}).Credentials()
	if _, err := creds.Status(context.Background()); err == nil {
		t.Fatal("missing CLI: no error")
	}
}
