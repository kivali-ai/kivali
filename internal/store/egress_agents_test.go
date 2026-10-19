package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/files"
)

func readProxyAgents(t *testing.T, path string) EgressAgents {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the proxy's copy: %v", err)
	}
	var got EgressAgents
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return got
}

func TestEgressAgentCredentialsPersistAndMirror(t *testing.T) {
	s := mustStore(t)
	sync := filepath.Join(t.TempDir(), "egress", "agents.json")
	s.SetEgressAgentsSyncPath(sync)
	a, b := egresscred.Hash("a"), egresscred.Hash("b")

	if err := s.SetEgressAgentCredential("alice", a); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEgressAgentCredential("bob", b); err != nil {
		t.Fatal(err)
	}
	// A new registration replaces the slug's earlier one.
	if err := s.SetEgressAgentCredential("alice", b); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"alice": b, "bob": b}
	got, err := s.EgressAgentCredentials()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("credentials = %v, %v; want %v", got, err, want)
	}
	if p := readProxyAgents(t, sync); !reflect.DeepEqual(p.Agents, want) || p.ModelHosts == nil {
		t.Fatalf("proxy copy = %+v", p)
	}
	// Persisted in DataDir: a new store over the same root reads it.
	again, err := New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.EgressAgentCredentials(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after reopen = %v", got)
	}
	fi, err := os.Stat(filepath.Join(s.Root(), egressAgentsFilename))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("data dir file: %v, %v", fi, err)
	}

	if err := s.DropEgressAgentCredential("alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.DropEgressAgentCredential("nobody"); err != nil {
		t.Fatal(err)
	}
	if p := readProxyAgents(t, sync); !reflect.DeepEqual(p.Agents, map[string]string{"bob": b}) {
		t.Fatalf("after drop, proxy copy = %+v", p)
	}
}

func TestSetEgressAgentCredentialRefusesBadInput(t *testing.T) {
	s := mustStore(t)
	for _, tc := range []struct{ slug, hash string }{
		{"alice", "nope"},
		{"alice", strings.ToUpper(egresscred.Hash("a"))},
		{"", egresscred.Hash("a")},
		{"../x", egresscred.Hash("a")},
	} {
		if err := s.SetEgressAgentCredential(tc.slug, tc.hash); err == nil {
			t.Errorf("SetEgressAgentCredential(%q, %q) accepted", tc.slug, tc.hash)
		}
	}
	if got, _ := s.EgressAgentCredentials(); len(got) != 0 {
		t.Fatalf("credentials = %v", got)
	}
}

func TestEgressModelHostsGoToTheProxyCopyOnly(t *testing.T) {
	s := mustStore(t)
	sync := filepath.Join(t.TempDir(), "agents.json")
	s.SetEgressAgentsSyncPath(sync)
	if err := s.SetEgressModelHosts([]string{" Platform.Claude.com", "api.anthropic.com", "api.anthropic.com"}); err != nil {
		t.Fatal(err)
	}
	p := readProxyAgents(t, sync)
	if !reflect.DeepEqual(p.ModelHosts, []string{"api.anthropic.com", "platform.claude.com"}) {
		t.Fatalf("model hosts = %v", p.ModelHosts)
	}
	if p.Agents == nil || len(p.Agents) != 0 {
		t.Fatalf("agents = %v, want an empty object", p.Agents)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), egressAgentsFilename)); !os.IsNotExist(err) {
		t.Fatalf("model hosts wrote the data dir file: %v", err)
	}
}

// Boot (and a restore) writes what is on disk to a fresh proxy volume.
func TestSyncEgressAgentsToProxyWritesWhatIsOnDisk(t *testing.T) {
	s := mustStore(t)
	if err := s.SetEgressAgentCredential("alice", egresscred.Hash("a")); err != nil {
		t.Fatal(err)
	}
	// Without a sync path nothing is mirrored, and that is not an error.
	if err := s.SyncEgressAgentsToProxy(); err != nil {
		t.Fatal(err)
	}
	sync := filepath.Join(t.TempDir(), "agents.json")
	s.SetEgressAgentsSyncPath(sync)
	if err := s.SyncEgressAgentsToProxy(); err != nil {
		t.Fatal(err)
	}
	if p := readProxyAgents(t, sync); p.Agents["alice"] != egresscred.Hash("a") {
		t.Fatalf("proxy copy = %+v", p)
	}
}

// A corrupt file reads as empty and the next registration writes a
// good one over it.
func TestCorruptEgressAgentsFileIsOverwritten(t *testing.T) {
	s := mustStore(t)
	path := filepath.Join(s.Root(), egressAgentsFilename)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EgressAgentCredentials(); err != nil || len(got) != 0 {
		t.Fatalf("corrupt file read as %v, %v", got, err)
	}
	if err := s.SetEgressAgentCredential("alice", egresscred.Hash("a")); err != nil {
		t.Fatalf("registration over a corrupt file: %v", err)
	}
	again, err := New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.EgressAgentCredentials(); got["alice"] != egresscred.Hash("a") || len(got) != 1 {
		t.Fatalf("after overwrite = %v", got)
	}
}

// A backup leaves the credentials out: they belong to the running
// runtimes, not to the team.
func TestEgressAgentsFileIsLeftOutOfBackups(t *testing.T) {
	if !files.ShouldExcludeFromBackup(egressAgentsFilename) {
		t.Fatalf("a backup carries %s", egressAgentsFilename)
	}
}

func TestArchiveAgentDropsItsEgressCredential(t *testing.T) {
	s := mustStore(t)
	sync := filepath.Join(t.TempDir(), "agents.json")
	s.SetEgressAgentsSyncPath(sync)
	if err := s.CreateAgent(Agent{Slug: "dave", Role: "r"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEgressAgentCredential("dave", egresscred.Hash("d")); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveAgent("dave"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.EgressAgentCredentials(); len(got) != 0 {
		t.Fatalf("credentials after archive = %v", got)
	}
	if p := readProxyAgents(t, sync); len(p.Agents) != 0 {
		t.Fatalf("proxy copy after archive = %+v", p)
	}
}
