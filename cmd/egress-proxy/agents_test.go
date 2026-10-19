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
)

// writeAgents writes the agents file core would, hashing each secret.
func writeAgents(t *testing.T, path string, secrets map[string]string, modelHosts ...string) {
	t.Helper()
	file := struct {
		Agents     map[string]string `json:"agents"`
		ModelHosts []string          `json:"model_hosts"`
	}{Agents: map[string]string{}, ModelHosts: modelHosts}
	for slug, secret := range secrets {
		sum := sha256.Sum256([]byte(secret))
		file.Agents[slug] = hex.EncodeToString(sum[:])
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

func basicAuth(slug, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(slug+":"+secret))
}

func TestAgentsLoadAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if a.Verify("alice", "s3cret") || a.IsModelHost("api.anthropic.com:443") {
		t.Fatal("missing file should mean no agents and no model hosts")
	}

	writeAgents(t, path, map[string]string{"alice": "s3cret"}, "API.Anthropic.com")
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	if !a.Verify("alice", "s3cret") {
		t.Error("valid secret refused")
	}
	if a.Verify("alice", "s3cret!") || a.Verify("bob", "s3cret") || a.Verify("", "s3cret") {
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
	if a.Verify("short", "x") || a.Verify("nothex", "x") {
		t.Error("malformed hash matched")
	}
	if !a.Verify("UPPER", "x") {
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
	if !a.Verify("alice", "s3cret") {
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
	if a.Verify("alice", "a") || a.loads.Load() != before {
		t.Fatalf("miss on a still-missing file read it: loads %d -> %d", before, a.loads.Load())
	}

	writeAgents(t, path, map[string]string{"alice": "a"})
	if !a.Verify("alice", "a") {
		t.Fatal("first call after registration refused")
	}
	before = a.loads.Load()
	for i := 0; i < 1000; i++ {
		if a.Verify("mallory", "x") {
			t.Fatal("bad credentials accepted")
		}
	}
	if got := a.loads.Load(); got != before {
		t.Fatalf("misses on an unchanged file read it %d times", got-before)
	}

	writeAgents(t, path, map[string]string{"alice": "a", "bob": "b"})
	if !a.Verify("bob", "b") {
		t.Fatal("first call after a second registration refused")
	}
	// A hit never re-reads: alice still verifies after the file drops her.
	writeAgents(t, path, map[string]string{"bob": "b"})
	before = a.loads.Load()
	if !a.Verify("alice", "a") || a.loads.Load() != before {
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
