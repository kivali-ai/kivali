package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// agentsFile is what a test puts in the agents file, secrets in the
// clear: writeAgentsFile hashes them.
type agentsFile struct {
	// secrets are the runtimes' own credentials, shells the shells'.
	secrets    map[string]string
	shells     map[string]string
	agentHosts map[string][]string
	exempt     []string
	modelHosts []string
}

// writeAgentsFile writes the agents file core would, in core's own
// shape (store.EgressAgents), hashing each secret.
func writeAgentsFile(t *testing.T, path string, f agentsFile) {
	t.Helper()
	hash := func(secrets map[string]string) map[string]string {
		out := map[string]string{}
		for slug, secret := range secrets {
			sum := sha256.Sum256([]byte(secret))
			out[slug] = hex.EncodeToString(sum[:])
		}
		return out
	}
	file := store.EgressAgents{
		Agents:     hash(f.secrets),
		Shells:     hash(f.shells),
		AgentHosts: f.agentHosts,
		Exempt:     f.exempt,
		ModelHosts: f.modelHosts,
	}
	b, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	// Write and rename into place, as core does.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// writeAgents writes an agents file of runtime credentials and model
// hosts only.
func writeAgents(t *testing.T, path string, secrets map[string]string, modelHosts ...string) {
	t.Helper()
	writeAgentsFile(t, path, agentsFile{secrets: secrets, modelHosts: modelHosts})
}

// verified reports whether secret is one of slug's credentials.
func verified(a *Agents, slug, secret string) bool {
	_, ok := a.Verify(slug, secret)
	return ok
}

func basicAuth(slug, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(slug+":"+secret))
}

// A shell's secret verifies as a shell credential and the runtime's as
// an open one, each for its own slug only. Were the kinds mixed up, a
// shell would get open mode; were the slugs not checked, one agent's
// shell could present another's credential. This fails with the wrong
// kind or with a credential accepted for the wrong slug.
func TestAgentsVerifyTellsTheCredentialsApart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	writeAgentsFile(t, path, agentsFile{
		secrets: map[string]string{"alice": "open-a", "bob": "open-b"},
		shells:  map[string]string{"alice": "shell-a"},
	})
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		slug, secret string
		kind         credKind
		ok           bool
	}{
		{"alice", "open-a", credOpen, true},
		{"alice", "shell-a", credShell, true},
		{"bob", "open-b", credOpen, true},
		{"bob", "shell-a", 0, false},
		{"alice", "open-b", 0, false},
		{"alice", "nope", 0, false},
	} {
		kind, ok := a.Verify(c.slug, c.secret)
		if kind != c.kind || ok != c.ok {
			t.Errorf("Verify(%q, %q) = %v, %v; want %v, %v", c.slug, c.secret, kind, ok, c.kind, c.ok)
		}
	}
}

// The agents' own patterns and exemptions are read with the
// allowlist's normalization, and fields the proxy does not know are
// ignored. This fails if an upper-case or port-carrying pattern never
// matches, if an empty list or slug is kept, or if an unknown field
// fails the load (which would keep the previous file in force).
func TestAgentsReadsAgentHostsAndExempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	body := `{"agents": {}, "agent_hosts": {"alice": [" API.GitHub.com:443 ", "#x", ""], "bob": [], "": ["x.test"]},
		"exempt": ["carol", ""], "integrations": {"future": true}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatalf("a file with an unknown field: %v", err)
	}
	if got := a.AgentHosts("alice"); len(got) != 1 || got[0] != "api.github.com" {
		t.Fatalf("alice's hosts = %q", got)
	}
	if a.AgentHosts("bob") != nil || a.AgentHosts("") != nil {
		t.Fatal("an empty list or an empty slug was kept")
	}
	if !a.AgentAllowed("alice", "v2.api.github.com:443") || a.AgentAllowed("alice", "github.com:443") || a.AgentAllowed("bob", "api.github.com:443") {
		t.Fatal("AgentAllowed does not follow the pattern language or the slug")
	}
	if !a.Exempt("carol") || a.Exempt("alice") || a.Exempt("") {
		t.Fatal("exempt set wrong")
	}
}

// The watcher picks up a new agent_hosts entry on its next poll, as it
// does the credentials. This fails if Load leaves the patterns out or
// the watcher misses the change.
func TestWatcherReloadsAgentHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	writeAgentsFile(t, path, agentsFile{shells: map[string]string{"alice": "s"}})
	a := &Agents{Path: path}
	w := &Watcher{Path: path}
	if err := w.pollOnce(a); err != nil {
		t.Fatal(err)
	}
	if a.AgentHosts("alice") != nil {
		t.Fatal("hosts before any were added")
	}
	writeAgentsFile(t, path, agentsFile{shells: map[string]string{"alice": "s"}, agentHosts: map[string][]string{"alice": {"own.test"}}, exempt: []string{"bob"}})
	if err := w.pollOnce(a); err != nil {
		t.Fatal(err)
	}
	if got := a.AgentHosts("alice"); len(got) != 1 || got[0] != "own.test" || !a.Exempt("bob") {
		t.Fatalf("after the poll: hosts %q, bob exempt %v", got, a.Exempt("bob"))
	}
}

func TestAgentsLoadAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if verified(a, "alice", "s3cret") || a.IsModelHost("api.anthropic.com:443") {
		t.Fatal("missing file should mean no agents and no model hosts")
	}

	writeAgents(t, path, map[string]string{"alice": "s3cret"}, "API.Anthropic.com")
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	if !verified(a, "alice", "s3cret") {
		t.Error("valid secret refused")
	}
	if verified(a, "alice", "s3cret!") || verified(a, "bob", "s3cret") || verified(a, "", "s3cret") {
		t.Error("wrong credentials accepted")
	}
	for host, want := range map[string]bool{
		"api.anthropic.com":       true,
		"api.anthropic.com:443":   true,
		"API.ANTHROPIC.COM:443":   true,
		"x.api.anthropic.com:443": false,
		"anthropic.com:443":       false,
		"api.anthropic.com.evil":  false,
	} {
		if got := a.IsModelHost(host); got != want {
			t.Errorf("IsModelHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAgentsDropsMalformedHashes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	short := sha256.Sum256([]byte("x"))
	body := `{"agents": {"short": "` + hex.EncodeToString(short[:16]) + `", "nothex": "zz", "UPPER": "` +
		strings.ToUpper(hex.EncodeToString(short[:])) + `"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	if verified(a, "short", "x") || verified(a, "nothex", "x") {
		t.Error("malformed hash matched")
	}
	if !verified(a, "UPPER", "x") {
		t.Error("an upper-case hex hash should still match")
	}
}

func TestAgentsBadJSONKeepsPrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	writeAgents(t, path, map[string]string{"alice": "s3cret"})
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Load(path); err == nil {
		t.Fatal("bad JSON accepted")
	}
	if !verified(a, "alice", "s3cret") {
		t.Error("bad file replaced the good one")
	}
}

// A miss re-reads the file only when it has changed, so misses from a
// client with bad credentials never delay a new agent's first call.
func TestAgentsReloadOnMissOnlyWhenChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	a := &Agents{Path: path}
	_ = a.Load(path)

	// Missing, and still missing: nothing to read.
	before := a.loads.Load()
	if verified(a, "alice", "a") || a.loads.Load() != before {
		t.Fatalf("miss on a still-missing file read it: loads %d -> %d", before, a.loads.Load())
	}

	writeAgents(t, path, map[string]string{"alice": "a"})
	if !verified(a, "alice", "a") {
		t.Fatal("first call after registration refused")
	}
	before = a.loads.Load()
	for i := 0; i < 1000; i++ {
		if verified(a, "mallory", "x") {
			t.Fatal("bad credentials accepted")
		}
	}
	if got := a.loads.Load(); got != before {
		t.Fatalf("misses on an unchanged file read it %d times", got-before)
	}

	writeAgents(t, path, map[string]string{"alice": "a", "bob": "b"})
	if !verified(a, "bob", "b") {
		t.Fatal("first call after a second registration refused")
	}
	// A hit never re-reads: alice still verifies after the file drops her.
	writeAgents(t, path, map[string]string{"bob": "b"})
	before = a.loads.Load()
	if !verified(a, "alice", "a") || a.loads.Load() != before {
		t.Fatal("hit should use the loaded file")
	}
}

func TestParseProxyAuth(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cases := []struct {
		in           string
		slug, secret string
		ok           bool
	}{
		{"Basic " + b64("alice:s3:cret"), "alice", "s3:cret", true},
		{"basic " + b64("alice:x"), "alice", "x", true},
		{"Bearer " + b64("alice:x"), "", "", false},
		{"Basic " + b64("alice"), "", "", false},
		{"Basic " + b64(":x"), "", "", false},
		{"Basic " + b64("alice:"), "", "", false},
		{"Basic not-base64!", "", "", false},
		{"Basic", "", "", false},
	}
	for _, c := range cases {
		slug, secret, ok := parseProxyAuth(c.in)
		if slug != c.slug || secret != c.secret || ok != c.ok {
			t.Errorf("parseProxyAuth(%q) = %q, %q, %v", c.in, slug, secret, ok)
		}
	}
}
