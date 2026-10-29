package claudeagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeauth"
	"github.com/kivali-ai/kivali/internal/provider"
)

// testSignIn is a signInWatch over a temporary HOME whose status is
// whatever cur holds, counting the checks.
type testSignIn struct {
	t     *testing.T
	home  string
	cur   claudeauth.Status
	err   error
	calls int
	watch *signInWatch
}

func newTestSignIn(t *testing.T, st claudeauth.Status) *testSignIn {
	ts := &testSignIn{t: t, home: t.TempDir(), cur: st}
	ts.watch = &signInWatch{home: ts.home, status: func(context.Context) (claudeauth.Status, error) {
		ts.calls++
		return ts.cur, ts.err
	}}
	ts.write(".credentials.json", `{"claudeAiOauth":{"accessToken":"token-1"}}`)
	return ts
}

// write puts body at HOME/.claude/name.
func (ts *testSignIn) write(name, body string) {
	ts.t.Helper()
	p := filepath.Join(ts.home, ".claude", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		ts.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		ts.t.Fatal(err)
	}
}

// TestSignInFilesStampFollowsContents: a rewrite of the same size that
// keeps the modification time (two writes inside one filesystem
// timestamp tick) moves the stamp; a touch that leaves the contents
// alone does not.
func TestSignInFilesStampFollowsContents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	missing := filepath.Join(dir, "config")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	put := func(body string, mod time.Time) string {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
		return signInFilesStamp(p, missing)
	}

	s1 := put(`{"env":{"A":"1","B":"2"}}`, at)
	s2 := put(`{"env":{"B":"2","A":"1"}}`, at)
	if s1 == s2 {
		t.Fatal("a same-size rewrite with the same modification time kept the stamp")
	}
	if s3 := put(`{"env":{"B":"2","A":"1"}}`, at.Add(time.Hour)); s3 != s2 {
		t.Fatal("a rewrite with the same contents moved the stamp")
	}
	if !strings.HasSuffix(s2, ";-;") {
		t.Fatalf("stamp %q does not mark the missing file", s2)
	}
}

func (ts *testSignIn) key() string { return ts.watch.Key(context.Background()) }

var subscriptionA = claudeauth.Status{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "firstParty",
	Email: "a@example.com", OrgName: "Example", SubscriptionType: "max"}

// TestSignInKeyStableAcrossTokenRefresh: a token refresh rewrites
// .credentials.json for the same account; the key is asked again and
// stays, and without a file change the CLI is not asked at all.
func TestSignInKeyStableAcrossTokenRefresh(t *testing.T) {
	ts := newTestSignIn(t, subscriptionA)
	k1 := ts.key()
	if k1 == "" || ts.calls != 1 {
		t.Fatalf("first key %q after %d checks", k1, ts.calls)
	}
	if ts.key() != k1 || ts.calls != 1 {
		t.Fatalf("unchanged files: key moved or the CLI was asked again (%d checks)", ts.calls)
	}
	ts.write(".credentials.json", `{"claudeAiOauth":{"accessToken":"token-2-refreshed","refreshToken":"r2"}}`)
	if ts.key() != k1 {
		t.Fatal("a token refresh for the same account changed the key")
	}
	if ts.calls != 2 {
		t.Fatalf("a rewritten credentials file should be checked once more; %d checks", ts.calls)
	}
}

// TestSignInKeyChangesWithIdentity: another account, another provider,
// or another settings env block (a Foundry key) is another key.
func TestSignInKeyChangesWithIdentity(t *testing.T) {
	ts := newTestSignIn(t, subscriptionA)
	seen := map[string]string{ts.key(): "subscription a"}
	step := func(name string) {
		t.Helper()
		k := ts.key()
		if prev, ok := seen[k]; ok {
			t.Fatalf("%s has the key of %s", name, prev)
		}
		seen[k] = name
	}

	ts.cur.Email = "b@example.com"
	ts.write(".credentials.json", `{"claudeAiOauth":{"accessToken":"token-b"}}`)
	step("subscription b")

	ts.cur = claudeauth.Status{LoggedIn: true, AuthMethod: "third_party", APIProvider: "foundry"}
	ts.write("settings.json", `{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_RESOURCE":"r","ANTHROPIC_FOUNDRY_API_KEY":"k1"}}`)
	step("foundry k1")

	ts.write("settings.json", `{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_RESOURCE":"r","ANTHROPIC_FOUNDRY_API_KEY":"k2"}}`)
	step("foundry k2")

	// The provider cleared before a new sign-in: signed out.
	ts.cur = claudeauth.Status{AuthMethod: "none", APIProvider: "firstParty"}
	ts.write("settings.json", `{}`)
	step("signed out")
}

// TestSignInKeyKeepsLastOnError: a failed check answers the last key,
// and the next call asks again.
func TestSignInKeyKeepsLastOnError(t *testing.T) {
	ts := newTestSignIn(t, subscriptionA)
	k1 := ts.key()
	ts.err = errors.New("claude auth status: timed out")
	ts.cur.Email = "b@example.com"
	ts.write(".credentials.json", `{"claudeAiOauth":{"accessToken":"token-b"}}`)
	if ts.key() != k1 {
		t.Fatal("a failed check moved the key")
	}
	ts.err = nil
	if k := ts.key(); k == k1 {
		t.Fatal("the check after a failure did not ask again")
	}
	if ts.calls != 3 {
		t.Fatalf("checks = %d, want 3", ts.calls)
	}
}

// TestRegistryRespawnsOnSignInChange: a warm runner survives a token
// refresh and is respawned, at its next turn, once the CLI is signed
// in to another account.
func TestRegistryRespawnsOnSignInChange(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	c := New(helperOpts(t, "echo", &memSessionStore{}))
	reg, err := c.ensureRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	ts := newTestSignIn(t, subscriptionA)
	reg.signIn = ts.watch

	runTurn := func() *runner {
		t.Helper()
		s, err := c.Stream(context.Background(), provider.CompleteRequest{
			Agent: "signer",
			Model: "claude-opus-4-8",
			Messages: []provider.Message{
				{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
			},
		})
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		drainTurn(t, s)
		_ = s.Close()
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return reg.runners["signer"]
	}

	r1 := runTurn()
	ts.write(".credentials.json", `{"claudeAiOauth":{"accessToken":"token-2-refreshed"}}`)
	if r2 := runTurn(); r2 != r1 {
		t.Fatal("a token refresh respawned the runner")
	}
	if r1.isDead() {
		t.Fatal("the runner died on a token refresh")
	}

	ts.cur.Email = "b@example.com"
	ts.write(".credentials.json", `{"claudeAiOauth":{"accessToken":"token-b"}}`)
	r3 := runTurn()
	if r3 == nil || r3 == r1 {
		t.Fatal("a new sign-in did not respawn the runner")
	}
	if !r1.isDead() {
		t.Error("the runner of the old sign-in was not closed")
	}
	if r3.spawnSignIn != ts.key() {
		t.Error("the respawned runner does not carry the new sign-in")
	}
}
