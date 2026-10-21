package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

func networkStore(t *testing.T, slugs ...string) (*FSStore, string) {
	t.Helper()
	s := mustStore(t)
	sync := filepath.Join(t.TempDir(), "egress", "agents.json")
	s.SetEgressAgentsSyncPath(sync)
	for _, slug := range slugs {
		if err := s.CreateAgent(Agent{Slug: slug, Role: slug}, ""); err != nil {
			t.Fatal(err)
		}
	}
	return s, sync
}

// An agent's own hosts and its exemption are saved in agent.yaml and
// reach the proxy's agents file at once. Without the mirror, a host the
// owner adds would not let the agent's shell through until something
// else rewrote the file; this fails with a proxy copy that lacks the
// host or the exemption, or with agent.yaml that lost a field.
func TestAgentNetworkSettingsRoundTripToTheProxy(t *testing.T) {
	s, sync := networkStore(t, "alice", "bob")

	got, err := s.AddAgentNetworkHost("alice", " API.GitHub.com ")
	if err != nil || !reflect.DeepEqual(got, []string{"api.github.com"}) {
		t.Fatalf("add = %v, %v", got, err)
	}
	if got, err = s.AddAgentNetworkHost("alice", "*.pypi.org"); err != nil || !reflect.DeepEqual(got, []string{"*.pypi.org", "api.github.com"}) {
		t.Fatalf("second add = %v, %v", got, err)
	}
	if err := s.SetAgentNetworkExempt("bob", true); err != nil {
		t.Fatal(err)
	}

	a, err := s.GetAgent("alice")
	if err != nil || !reflect.DeepEqual(a.NetworkHosts, []string{"*.pypi.org", "api.github.com"}) || a.NetworkExempt {
		t.Fatalf("alice = %+v, %v", a, err)
	}
	if b, _ := s.GetAgent("bob"); !b.NetworkExempt || b.NetworkHosts != nil {
		t.Fatalf("bob = %+v", b)
	}
	p := readProxyAgents(t, sync)
	if want := map[string][]string{"alice": {"*.pypi.org", "api.github.com"}}; !reflect.DeepEqual(p.AgentHosts, want) {
		t.Fatalf("agent_hosts = %v, want %v", p.AgentHosts, want)
	}
	if !reflect.DeepEqual(p.Exempt, []string{"bob"}) {
		t.Fatalf("exempt = %v", p.Exempt)
	}

	if got, err = s.RemoveAgentNetworkHost("alice", "API.github.com"); err != nil || !reflect.DeepEqual(got, []string{"*.pypi.org"}) {
		t.Fatalf("remove = %v, %v", got, err)
	}
	if got, err = s.RemoveAgentNetworkHost("alice", "*.pypi.org"); err != nil || len(got) != 0 {
		t.Fatalf("remove the last = %v, %v", got, err)
	}
	if err := s.SetAgentNetworkExempt("bob", false); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAgent("alice"); a.NetworkHosts != nil {
		t.Fatalf("an emptied list stays in agent.yaml as %#v", a.NetworkHosts)
	}
	p = readProxyAgents(t, sync)
	if p.AgentHosts == nil || len(p.AgentHosts) != 0 || p.Exempt == nil || len(p.Exempt) != 0 {
		t.Fatalf("after clearing, proxy copy = %+v; want an empty object and an empty list", p)
	}
}

// The proxy matches the list as written, so the store must keep it
// clean: a duplicate in any case or spacing is refused rather than
// stored twice, and the list stays sorted. This fails if a duplicate
// is accepted or the order follows the adds.
func TestAgentNetworkHostsSortedAndDeduplicated(t *testing.T) {
	s, _ := networkStore(t, "alice")
	for _, h := range []string{"zeta.example", "alpha.example", "mid.example"} {
		if _, err := s.AddAgentNetworkHost("alice", h); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.AddAgentNetworkHost("alice", " ALPHA.example")
	if !errors.Is(err, ErrNetworkHostListed) {
		t.Fatalf("duplicate add err = %v, want ErrNetworkHostListed", err)
	}
	a, _ := s.GetAgent("alice")
	if want := []string{"alpha.example", "mid.example", "zeta.example"}; !reflect.DeepEqual(a.NetworkHosts, want) {
		t.Fatalf("list = %v, want %v", a.NetworkHosts, want)
	}
	if _, err := s.RemoveAgentNetworkHost("alice", "nope.example"); !errors.Is(err, ErrNetworkHostNotListed) {
		t.Fatalf("remove of an unlisted host err = %v", err)
	}
}

// A host the proxy cannot match (a URL, a port, a comment, user info)
// is refused before anything is written, so it never sits on a list
// looking like it allows something. This fails if any of them is
// saved.
func TestAgentNetworkHostRefusesInvalidHosts(t *testing.T) {
	s, _ := networkStore(t, "alice")
	for _, h := range []string{"", "  ", "https://example.com", "example.com/x", "example.com:443", "#comment", "user@example.com", "a b", "x?y"} {
		if _, err := s.AddAgentNetworkHost("alice", h); !errors.Is(err, ErrInvalidEgressHost) {
			t.Errorf("AddAgentNetworkHost(%q) err = %v, want ErrInvalidEgressHost", h, err)
		}
	}
	if a, _ := s.GetAgent("alice"); a.NetworkHosts != nil {
		t.Fatalf("a refused host was saved: %v", a.NetworkHosts)
	}
}

// Only an active agent has network settings: an archived or unknown
// one is ErrNotFound for every setter. Without the check a setter could
// recreate agents/<slug>/agent.yaml for an offboarded agent.
func TestAgentNetworkSettersNeedAnActiveAgent(t *testing.T) {
	s, _ := networkStore(t, "dave")
	if err := s.ArchiveAgent("dave"); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"dave", "nobody"} {
		if _, err := s.AddAgentNetworkHost(slug, "example.com"); !errors.Is(err, ErrNotFound) {
			t.Errorf("add for %s err = %v", slug, err)
		}
		if _, err := s.RemoveAgentNetworkHost(slug, "example.com"); !errors.Is(err, ErrNotFound) {
			t.Errorf("remove for %s err = %v", slug, err)
		}
		if err := s.SetAgentNetworkExempt(slug, true); !errors.Is(err, ErrNotFound) {
			t.Errorf("exempt for %s err = %v", slug, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "agents", "dave")); !os.IsNotExist(err) {
		t.Fatalf("a setter recreated the archived agent's directory: %v", err)
	}
}

// The proxy-facing file carries both credentials and each active
// agent's settings, and an invalid host saved by hand in agent.yaml
// never reaches the proxy. This fails if a field is missing from the
// sync file or a hand-written junk entry is passed on.
func TestEgressAgentsSyncFileShape(t *testing.T) {
	s, sync := networkStore(t, "alice", "bob", "carol")
	if err := s.SetEgressAgentCredential("alice", egresscred.Hash("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEgressShellCredential("alice", egresscred.Hash("a-shell")); err != nil {
		t.Fatal(err)
	}
	if err := s.updateAgent("carol", func(a *Agent) error {
		a.NetworkHosts = []string{"https://bad.example/x", "Good.example"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentNetworkExempt("bob", true); err != nil {
		t.Fatal(err)
	}
	p := readProxyAgents(t, sync)
	want := EgressAgents{
		Agents:     map[string]string{"alice": egresscred.Hash("a")},
		Shells:     map[string]string{"alice": egresscred.Hash("a-shell")},
		AgentHosts: map[string][]string{"carol": {"good.example"}},
		Exempt:     []string{"bob"},
		ModelHosts: []string{},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("proxy copy = %+v, want %+v", p, want)
	}
	// The data dir file holds hashes only, and stays private.
	b, err := os.ReadFile(filepath.Join(s.Root(), egressAgentsFilename))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"good.example", "exempt", "agent_hosts"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("data dir file holds %q: %s", leak, b)
		}
	}
	if fi, err := os.Stat(filepath.Join(s.Root(), egressAgentsFilename)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("data dir file: %v, %v", fi, err)
	}
}

// The shell credential is a credential like the agent's own: it is
// kept across a reopen, replaced by a new registration, dropped by an
// empty one, and refused when malformed. This fails if any of those
// leaves a stale or bad hash in place.
func TestEgressShellCredentialLifecycle(t *testing.T) {
	s, sync := networkStore(t)
	if err := s.SetEgressShellCredential("alice", egresscred.Hash("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEgressShellCredential("alice", egresscred.Hash("two")); err != nil {
		t.Fatal(err)
	}
	again, err := New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.EgressShellCredentials(); !reflect.DeepEqual(got, map[string]string{"alice": egresscred.Hash("two")}) {
		t.Fatalf("after reopen = %v", got)
	}
	for _, bad := range []string{"nope", "ABC"} {
		if err := s.SetEgressShellCredential("alice", bad); err == nil {
			t.Errorf("SetEgressShellCredential(%q) accepted", bad)
		}
	}
	if err := s.SetEgressShellCredential("../x", egresscred.Hash("x")); err == nil {
		t.Error("a bad slug was accepted")
	}
	if err := s.SetEgressShellCredential("alice", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.EgressShellCredentials(); len(got) != 0 {
		t.Fatalf("after an empty registration = %v", got)
	}
	if p := readProxyAgents(t, sync); p.Shells == nil || len(p.Shells) != 0 {
		t.Fatalf("proxy copy shells = %v, want an empty object", p.Shells)
	}
}

// Offboarding takes both credentials and the agent's settings out of
// the proxy's file. Without it, the shell credential an archived
// agent's pod still holds would keep its hosts, or its exemption,
// until the next unrelated write.
func TestArchiveAgentDropsItsShellCredentialAndSettings(t *testing.T) {
	s, sync := networkStore(t, "dave")
	if err := s.SetEgressAgentCredential("dave", egresscred.Hash("d")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEgressShellCredential("dave", egresscred.Hash("d-shell")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAgentNetworkHost("dave", "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentNetworkExempt("dave", true); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveAgent("dave"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.EgressShellCredentials(); len(got) != 0 {
		t.Fatalf("shell credentials after archive = %v", got)
	}
	p := readProxyAgents(t, sync)
	if len(p.Agents) != 0 || len(p.Shells) != 0 || len(p.AgentHosts) != 0 || len(p.Exempt) != 0 {
		t.Fatalf("proxy copy after archive = %+v", p)
	}
}

// An agent with settings but no registered credential is still taken
// out of the proxy's file when it is offboarded: the drop writes the
// file even with nothing to drop. This fails with the exemption left
// in the file.
func TestArchiveAgentWithoutCredentialsStillLeavesTheProxyFile(t *testing.T) {
	s, sync := networkStore(t, "erin")
	if err := s.SetAgentNetworkExempt("erin", true); err != nil {
		t.Fatal(err)
	}
	if err := s.ArchiveAgent("erin"); err != nil {
		t.Fatal(err)
	}
	if p := readProxyAgents(t, sync); len(p.Exempt) != 0 {
		t.Fatalf("exempt after archive = %v", p.Exempt)
	}
}

// One agent.yaml that does not parse leaves only that agent out of the
// proxy's settings; the others, and every credential, still reach it.
// Without this a single bad file would stop every registration from
// reaching the proxy.
func TestEgressSyncSurvivesAnUnreadableAgent(t *testing.T) {
	s, sync := networkStore(t, "alice", "bob")
	if err := s.SetAgentNetworkExempt("alice", true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root(), "agents", "bob", "agent.yaml"), []byte("network_hosts: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEgressAgentCredential("bob", egresscred.Hash("b")); err != nil {
		t.Fatalf("registration with an unreadable agent.yaml present: %v", err)
	}
	p := readProxyAgents(t, sync)
	if p.Agents["bob"] != egresscred.Hash("b") || !reflect.DeepEqual(p.Exempt, []string{"alice"}) {
		t.Fatalf("proxy copy = %+v", p)
	}
}
