package store

import (
	"os"
	"path/filepath"
	"strings"
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
	// The shell's dev tooling hosts are in the seed.
	want := map[string]bool{
		"deb.debian.org": false,
		"*.pypi.org":     false,
		"github.com":     false,
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
	// No model provider host: the shell does not call the model, and a
	// provider's API could carry data out to a planted key's account.
	for _, p := range al.Patterns {
		for _, provider := range []string{"anthropic", "claude.com", "bedrock", "sts.", "aiplatform", "googleapis.com", "azure.com", "microsoftonline", "sso.", "oidc."} {
			if strings.Contains(p, provider) {
				t.Errorf("default seed holds model provider host %q", p)
			}
		}
	}
}

// An existing list keeps its model provider hosts: only a fresh
// install gets the new defaults.
func TestReadEgressAllowlistKeepsAnExistingList(t *testing.T) {
	s := mustStore(t)
	old := []string{"api.anthropic.com", "deb.debian.org", "platform.claude.com"}
	if err := s.WriteEgressAllowlist(EgressAllowlist{Patterns: old}); err != nil {
		t.Fatal(err)
	}
	al, err := s.ReadEgressAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(al.Patterns, ",") != strings.Join(old, ",") {
		t.Fatalf("patterns = %v, want %v", al.Patterns, old)
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
