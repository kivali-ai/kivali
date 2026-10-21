package store

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A fresh install's list is exactly the host Kivali needs. A wider seed
// would hand every new team's shells a host anyone can hold an account
// on (GitHub, PyPI) before the owner chose to.
func TestReadEgressAllowlistSeedsDefaults(t *testing.T) {
	s := mustStore(t)
	al, err := s.ReadEgressAllowlist()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []string{"deb.debian.org"}
	if !slices.Equal(al.Patterns, want) {
		t.Errorf("seed = %v, want %v", al.Patterns, want)
	}
	// File should have been written.
	if _, err := os.Stat(filepath.Join(s.Root(), "allowed_egress.yaml")); err != nil {
		t.Errorf("file not created: %v", err)
	}
	again, err := s.ReadEgressAllowlist()
	if err != nil || !slices.Equal(again.Patterns, want) {
		t.Fatalf("re-read: %v, %v, want %v", err, again.Patterns, want)
	}
}

// An existing list keeps its hosts, model providers included: only a
// fresh install gets the seed.
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

// The permanent host is in every list the store writes or reads: a
// write without it (an Org → Network removal), or a file written by
// something else without it (a hand edit, a restored backup), would
// otherwise leave software installs in agents' shells failing.
func TestEgressAllowlistKeepsThePermanentHost(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteEgressAllowlist(EgressAllowlist{Patterns: []string{"example.com"}}); err != nil {
		t.Fatal(err)
	}
	al, err := s.ReadEgressAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"deb.debian.org", "example.com"}; !slices.Equal(al.Patterns, want) {
		t.Errorf("after write = %v, want %v", al.Patterns, want)
	}
	path := filepath.Join(s.Root(), "allowed_egress.yaml")
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "deb.debian.org") {
		t.Errorf("file lacks the permanent host: %v\n%s", err, b)
	}
	if err := os.WriteFile(path, []byte("patterns:\n    - example.org\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	al, err = s.ReadEgressAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"deb.debian.org", "example.org"}; !slices.Equal(al.Patterns, want) {
		t.Errorf("read of a file without it = %v, want %v", al.Patterns, want)
	}
	// The proxy's copy at start comes from that same read, so the file
	// on disk still lacking the host does not reach the proxy.
	sync := filepath.Join(t.TempDir(), "allowlist.yaml")
	s.SetEgressSyncPath(sync)
	if err := s.SyncEgressAllowlistToProxy(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(sync); err != nil || !strings.Contains(string(b), "deb.debian.org") {
		t.Errorf("proxy copy lacks the permanent host: %v\n%s", err, b)
	}
	if !PermanentEgressPattern("deb.debian.org") || PermanentEgressPattern("example.org") {
		t.Error("PermanentEgressPattern names the wrong hosts")
	}
}

// Normalization and the permanent host compose: a write that dedups
// and sorts must not drop the host, and the host must not escape the
// sort order the proxy's YAML diffs rely on.
func TestWriteEgressAllowlistNormalizes(t *testing.T) {
	s := mustStore(t)
	err := s.WriteEgressAllowlist(EgressAllowlist{
		Patterns: []string{"  *.Example.Com", "PyPi.org", "pypi.org", "", "# comment"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	al, _ := s.ReadEgressAllowlist()
	want := []string{"*.example.com", "deb.debian.org", "pypi.org"}
	if !slices.Equal(al.Patterns, want) {
		t.Errorf("normalized = %v, want %v", al.Patterns, want)
	}
}

// An empty write (the owner removed every host they could) must not
// fail and must not leave the list empty: an empty list would stop the
// software installs the permanent host serves.
func TestWriteEgressAllowlistEmptyOK(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteEgressAllowlist(EgressAllowlist{}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	al, _ := s.ReadEgressAllowlist()
	if want := []string{"deb.debian.org"}; !slices.Equal(al.Patterns, want) {
		t.Errorf("patterns = %v, want %v", al.Patterns, want)
	}
}
