package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadEgressAllowlistSeedsDefaults(t *testing.T) {
	s := mustStore(t)
	al, err := s.ReadEgressAllowlist()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(al.Patterns) == 0 {
		t.Error("expected default patterns")
	}
	// File should have been written.
	if _, err := os.Stat(filepath.Join(s.Root(), "allowed_egress.yaml")); err != nil {
		t.Errorf("file not created: %v", err)
	}
	// Anthropic hosts MUST be in the seed — without them an agent
	// pod boots with credentials mounted but the egress proxy denies
	// CONNECT to api.anthropic.com / platform.claude.com and the CLI
	// errors "Failed to authenticate" on the first chat-turn. Pin the
	// load-bearing entries explicitly so a future seed reorg can't
	// silently drop them.
	// The same goes for the Bedrock and Vertex endpoints a `claude
	// login` to either bills through, and for Microsoft Foundry's.
	want := map[string]bool{
		"api.anthropic.com":   false,
		"platform.claude.com": false,
		"bedrock-runtime.[a-z][a-z]-*-[0-9].amazonaws.com": false,
		"sts.amazonaws.com":           false,
		"*-aiplatform.googleapis.com": false,
		"aiplatform.googleapis.com":   false,
		"oauth2.googleapis.com":       false,
		"*.services.ai.azure.com":     false,
		"login.microsoftonline.com":   false,
	}
	// The globs survive the YAML round trip.
	again, err := s.ReadEgressAllowlist()
	if err != nil || len(again.Patterns) != len(al.Patterns) {
		t.Fatalf("re-read: %v, %d patterns, want %d", err, len(again.Patterns), len(al.Patterns))
	}
	al = again
	for _, p := range al.Patterns {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for host, present := range want {
		if !present {
			t.Errorf("default seed missing required host %q (full seed: %v)", host, al.Patterns)
		}
	}
}

func TestWriteEgressAllowlistNormalizes(t *testing.T) {
	s := mustStore(t)
	err := s.WriteEgressAllowlist(EgressAllowlist{
		Patterns: []string{"  *.Example.Com", "PyPi.org", "pypi.org", "", "# comment"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	al, _ := s.ReadEgressAllowlist()
	want := []string{"*.example.com", "pypi.org"}
	if len(al.Patterns) != 2 || al.Patterns[0] != want[0] || al.Patterns[1] != want[1] {
		t.Errorf("normalized = %v, want %v", al.Patterns, want)
	}
}

func TestWriteEgressAllowlistEmptyOK(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteEgressAllowlist(EgressAllowlist{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	al, _ := s.ReadEgressAllowlist()
	if len(al.Patterns) != 0 {
		t.Errorf("patterns = %v, want empty", al.Patterns)
	}
}
