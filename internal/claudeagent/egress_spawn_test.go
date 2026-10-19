package claudeagent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/provider"
)

// egressFixture is a credential with a known secret, as the agent
// runtime would hand the driver.
func egressFixture(t *testing.T) (secret, credURL string, scrub *egresscred.Scrubber) {
	t.Helper()
	secret = "dGVzdC1zZWNyZXQtZm9yLXRoZS1kcml2ZXItc3Bhd24tdGVzdA"
	credURL = "http://alice:" + secret + "@10.43.0.12:3128"
	return secret, credURL, egresscred.NewScrubber("alice", secret, credURL)
}

func readEnvFile(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the fake CLI did not record its environment: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// envValue is the value the CLI sees for key: the last entry wins, as
// exec does with duplicates.
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

func assertAgentSpawn(t *testing.T, path string, args, env []string, credURL string) {
	t.Helper()
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("%s: args %q lack --strict-mcp-config", path, args)
	}
	for k, want := range map[string]string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"ENABLE_CLAUDEAI_MCP_SERVERS":              "false",
		"HTTP_PROXY":                               credURL,
		"HTTPS_PROXY":                              credURL,
		"http_proxy":                               credURL,
		"https_proxy":                              credURL,
	} {
		if got := envValue(env, k); got != want {
			t.Errorf("%s: %s = %q, want %q", path, k, got, want)
		}
	}
}

// Every CLI the driver starts, the warm runner, the per-call stream and
// a subagent run, gets --strict-mcp-config, the run env and Options.Env
// (the runtime's credentialed proxy), over the bare proxy the process
// itself has.
func TestEverySpawnGetsStrictMCPConfigAndTheRunEnv(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	argsFile := filepath.Join(dir, "args.jsonl")
	t.Setenv("GO_FAKE_CLAUDE_ENV_FILE", envFile)
	t.Setenv("GO_FAKE_CLAUDE_ARGS_FILE", argsFile)
	t.Setenv("HTTPS_PROXY", "http://10.43.0.12:3128")
	_, credURL, scrub := egressFixture(t)

	opts := helperOpts(t, "echo", &memSessionStore{})
	opts.Env = egresscred.ProxyEnv(credURL)
	opts.Scrubber = scrub
	c := New(opts)
	defer func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	}()
	req := provider.CompleteRequest{
		Agent:  "alice",
		Model:  "claude-opus-4-8",
		Effort: "high",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "hi"}}},
		},
	}
	s, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	drainTurn(t, s)
	_ = s.Close()
	assertAgentSpawn(t, "warm runner", readSpawnArgs(t, argsFile)[0], readEnvFile(t, envFile), credURL)

	req.Agent = ""
	s, err = c.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	drainTurn(t, s)
	_ = s.Close()
	assertAgentSpawn(t, "per-call", readSpawnArgs(t, argsFile)[1], readEnvFile(t, envFile), credURL)

	s, err = c.RunSubagent(context.Background(), subagentTestRequest(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	drainTurn(t, s)
	_ = s.Close()
	assertAgentSpawn(t, "subagent", readSpawnArgs(t, argsFile)[2], readEnvFile(t, envFile), credURL)
}

// Without Options.Env the CLI keeps the process's own proxy: the
// server's spawns are unchanged.
func TestSpawnWithoutEnvKeepsTheProcessProxy(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("GO_FAKE_CLAUDE_ENV_FILE", envFile)
	t.Setenv("HTTPS_PROXY", "http://10.43.0.12:3128")
	c := New(helperOpts(t, "echo", nil))
	s, err := c.RunSubagent(context.Background(), subagentTestRequest(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	drainTurn(t, s)
	_ = s.Close()
	if got := envValue(readEnvFile(t, envFile), "HTTPS_PROXY"); got != "http://10.43.0.12:3128" {
		t.Fatalf("HTTPS_PROXY = %q", got)
	}
}

// What the driver writes of a CLI run, its stderr and the mirror of its
// stdin, never holds the credential.
func TestSpawnScrubsStderrAndTheStdinMirror(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	secret, credURL, scrub := egressFixture(t)
	t.Setenv("GO_FAKE_CLAUDE_STDERR", "fetch failed through "+credURL+" (secret "+secret+")")

	opts := helperOpts(t, "echo", nil)
	opts.Scrubber = scrub
	c := New(opts)

	// A subagent run: stderr goes to the caller's writer, the mirror
	// into its run dir.
	runDir := t.TempDir()
	req := subagentTestRequest(runDir)
	req.Prompt = "use " + secret
	var stderr bytes.Buffer
	req.Stderr = &stderr
	s, err := c.RunSubagent(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	drainTurn(t, s)
	_ = s.Close()
	assertScrubbed(t, "subagent stderr", stderr.Bytes())
	mirror, err := os.ReadFile(filepath.Join(runDir, "cli-stdin.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	assertScrubbed(t, "subagent stdin mirror", mirror)

	// The per-call stream: both land in the session dir.
	sr := provider.CompleteRequest{
		Model: "claude-opus-4-8",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "use " + secret}}},
		},
	}
	st, err := c.Stream(context.Background(), sr)
	if err != nil {
		t.Fatal(err)
	}
	drainTurn(t, st)
	_ = st.Close()
	for _, name := range []string{"cli-stderr-latest.log", "cli-stdin-latest.jsonl"} {
		b, err := os.ReadFile(filepath.Join(opts.SessionDir, name))
		if err != nil {
			t.Fatal(err)
		}
		assertScrubbed(t, name, b)
	}
}

// A failed `auth status` quotes the CLI's stderr in its error, which
// the sign-in check logs: the credential is scrubbed from it.
func TestAuthStatusErrorIsScrubbed(t *testing.T) {
	withFakeCLIEnv(t, "echo")
	secret, credURL, scrub := egressFixture(t)
	t.Setenv("GO_FAKE_CLAUDE_AUTH_STDERR", "proxy "+credURL+" refused")
	_, err := authStatus(context.Background(), helperBinary(t), egresscred.ProxyEnv(credURL), scrub)
	if err == nil {
		t.Fatal("a failing status returned no error")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), egresscred.Placeholder) {
		t.Fatalf("error = %q", err)
	}
}

func assertScrubbed(t *testing.T, what string, b []byte) {
	t.Helper()
	secret, _, _ := egressFixture(t)
	if bytes.Contains(b, []byte(secret)) {
		t.Errorf("%s holds the secret: %s", what, b)
	}
	if !bytes.Contains(b, []byte(egresscred.Placeholder)) {
		t.Errorf("%s has no placeholder: %s", what, b)
	}
}
