package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// networkServer is a test server with two agents and the proxy's
// agents file at the returned path.
func networkServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "alice", Role: "Writer", ReportsTo: "ceo"},
		{Slug: "bob", Role: "Analyst", ReportsTo: "ceo"},
	} {
		if err := srv.Store.CreateAgent(a, "role"); err != nil {
			t.Fatal(err)
		}
	}
	sync := filepath.Join(t.TempDir(), "agents.json")
	srv.Store.SetEgressAgentsSyncPath(sync)
	return srv, sync
}

func proxyAgentsFile(t *testing.T, path string) store.EgressAgents {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f store.EgressAgents
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// The agent's own list is read, added to and removed from through the
// API, every answer carries the team's list beside it, and each write
// reaches the proxy's agents file. This fails if an answer drops the
// team's list or the permanent mark, if the own list is not normalized
// and sorted, or if a write stops short of the proxy.
func TestAPIAgentNetworkHosts(t *testing.T) {
	srv, sync := networkServer(t)
	n := orgOK[apitypes.AgentNetwork](t, orgGet(t, srv, "/api/v1/agents/alice/network"))
	if n.Own == nil || len(n.Own) != 0 || n.Exempt {
		t.Fatalf("initial = %#v", n)
	}
	if len(n.Team) != 1 || n.Team[0] != (apitypes.NetworkHost{Host: "deb.debian.org", Permanent: true}) {
		t.Fatalf("team = %#v", n.Team)
	}

	if n = orgOK[apitypes.AgentNetwork](t, orgJSON(t, srv, http.MethodPost, "/api/v1/agents/alice/network", apitypes.NetworkAdd{Host: " API.GitHub.com "})); !reflect.DeepEqual(n.Own, []string{"api.github.com"}) {
		t.Fatalf("after the first add = %v", n.Own)
	}
	n = orgOK[apitypes.AgentNetwork](t, orgJSON(t, srv, http.MethodPost, "/api/v1/agents/alice/network", apitypes.NetworkAdd{Host: "*.pypi.org"}))
	if want := []string{"*.pypi.org", "api.github.com"}; !reflect.DeepEqual(n.Own, want) || len(n.Team) != 1 {
		t.Fatalf("after adds = %#v, want own %v", n, want)
	}
	if got := proxyAgentsFile(t, sync).AgentHosts["alice"]; !reflect.DeepEqual(got, []string{"*.pypi.org", "api.github.com"}) {
		t.Fatalf("proxy's agent_hosts for alice = %v", got)
	}
	// bob's list is his own.
	if b := orgOK[apitypes.AgentNetwork](t, orgGet(t, srv, "/api/v1/agents/bob/network")); len(b.Own) != 0 {
		t.Fatalf("bob's own list = %v", b.Own)
	}

	n = orgOK[apitypes.AgentNetwork](t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/agents/alice/network/api.github.com", "", nil))
	if want := []string{"*.pypi.org"}; !reflect.DeepEqual(n.Own, want) {
		t.Fatalf("after remove = %v, want %v", n.Own, want)
	}
	if got := proxyAgentsFile(t, sync).AgentHosts["alice"]; !reflect.DeepEqual(got, []string{"*.pypi.org"}) {
		t.Fatalf("proxy's agent_hosts after remove = %v", got)
	}
}

// Each refusal has its own status, says what happened and who can fix
// it, and changes nothing. This fails with the wrong status, an error
// body without both parts, or a refused host saved.
func TestAPIAgentNetworkRefusals(t *testing.T) {
	srv, _ := networkServer(t)
	add := func(slug, host string) int {
		return orgJSON(t, srv, http.MethodPost, "/api/v1/agents/"+slug+"/network", apitypes.NetworkAdd{Host: host}).Code
	}
	orgOK[apitypes.AgentNetwork](t, orgJSON(t, srv, http.MethodPost, "/api/v1/agents/alice/network", apitypes.NetworkAdd{Host: "example.com"}))

	rr := orgJSON(t, srv, http.MethodPost, "/api/v1/agents/alice/network", apitypes.NetworkAdd{Host: "https://example.org/x"})
	if e := assertAPIError(t, rr, http.StatusBadRequest); !strings.Contains(e.Error, "https://example.org/x") {
		t.Errorf("400 does not name the host: %+v", e)
	}
	rr = orgJSON(t, srv, http.MethodPost, "/api/v1/agents/alice/network", apitypes.NetworkAdd{Host: "EXAMPLE.com"})
	if e := assertAPIError(t, rr, http.StatusConflict); !strings.Contains(e.Error, "example.com") {
		t.Errorf("409 does not name the host: %+v", e)
	}
	assertAPIError(t, orgDo(t, srv, "", http.MethodDelete, "/api/v1/agents/alice/network/other.example", "", nil), http.StatusNotFound)
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/agents/alice/network", "application/json", []byte("{")), http.StatusBadRequest)
	if a, _ := srv.Store.GetAgent("alice"); !reflect.DeepEqual(a.NetworkHosts, []string{"example.com"}) {
		t.Fatalf("after the refusals alice's list = %v", a.NetworkHosts)
	}

	if code := add("nobody", "x.example"); code != http.StatusNotFound {
		t.Errorf("unknown agent add: %d", code)
	}
	if code := add("ceo", "x.example"); code != http.StatusBadRequest {
		t.Errorf("the owner add: %d", code)
	}
}

// An offboarded agent has no network settings to show or change: every
// route answers 404, as for an agent that never existed. Without it a
// write could put an archived agent back in the proxy's file.
func TestAPIAgentNetworkArchivedAgentIsNotFound(t *testing.T) {
	srv, _ := networkServer(t)
	if err := srv.Store.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"bob", "nobody"} {
		base := "/api/v1/agents/" + slug + "/network"
		assertAPIError(t, orgGet(t, srv, base), http.StatusNotFound)
		assertAPIError(t, orgJSON(t, srv, http.MethodPost, base, apitypes.NetworkAdd{Host: "x.example"}), http.StatusNotFound)
		assertAPIError(t, orgDo(t, srv, "", http.MethodDelete, base+"/x.example", "", nil), http.StatusNotFound)
		assertAPIError(t, orgJSON(t, srv, http.MethodPost, base+"/exempt", apitypes.AgentNetworkExempt{Exempt: true}), http.StatusNotFound)
	}
}

// The switch turns the agent's limits off and back on, and everywhere
// that shows it agrees: the agent's settings, the proxy's file, Org,
// Network's list of exempt agents (sorted by name, never null) and the
// Team page's summaries. This fails if any of them misses the change
// or lags behind it.
func TestAPIAgentNetworkExemptRoundTrip(t *testing.T) {
	srv, sync := networkServer(t)
	exempt := func(slug string, on bool) apitypes.AgentNetwork {
		return orgOK[apitypes.AgentNetwork](t, orgJSON(t, srv, http.MethodPost, "/api/v1/agents/"+slug+"/network/exempt", apitypes.AgentNetworkExempt{Exempt: on}))
	}
	if n := exempt("alice", true); !n.Exempt || len(n.Team) != 1 {
		t.Fatalf("after turning limits off = %#v", n)
	}
	exempt("bob", true)
	if n := orgOK[apitypes.AgentNetwork](t, orgGet(t, srv, "/api/v1/agents/alice/network")); !n.Exempt {
		t.Fatal("GET does not show the exemption")
	}
	if got := proxyAgentsFile(t, sync).Exempt; !reflect.DeepEqual(got, []string{"alice", "bob"}) {
		t.Fatalf("proxy's exempt = %v", got)
	}
	org := orgOK[apitypes.Network](t, orgGet(t, srv, "/api/v1/org/network"))
	want := []apitypes.NetworkExemptAgent{{Slug: "bob", Name: "Analyst"}, {Slug: "alice", Name: "Writer"}}
	if !reflect.DeepEqual(org.Exempt, want) {
		t.Fatalf("org network exempt = %#v, want %#v", org.Exempt, want)
	}
	summaries := decodeAPI[apitypes.AgentsResponse](t, apiDo(t, srv, http.MethodGet, "/api/v1/agents", "", nil))
	for _, a := range summaries.Agents {
		if (a.Slug == "alice" || a.Slug == "bob") && !a.NetworkExempt {
			t.Errorf("summary for %s lacks the exemption", a.Slug)
		}
	}

	if n := exempt("alice", false); n.Exempt {
		t.Fatal("turning limits back on did not stick")
	}
	exempt("bob", false)
	if got := proxyAgentsFile(t, sync).Exempt; len(got) != 0 {
		t.Fatalf("proxy's exempt after turning limits on = %v", got)
	}
	rr := orgGet(t, srv, "/api/v1/org/network")
	if !strings.Contains(rr.Body.String(), `"exempt":[]`) {
		t.Fatalf("org network with no exempt agents = %s, want an empty list", rr.Body.String())
	}
	summaries = decodeAPI[apitypes.AgentsResponse](t, apiDo(t, srv, http.MethodGet, "/api/v1/agents", "", nil))
	for _, a := range summaries.Agents {
		if a.NetworkExempt {
			t.Errorf("summary for %s still shows the exemption", a.Slug)
		}
	}
}

// An archived agent's summary carries the setting it was offboarded
// with, and Org, Network lists active agents only. This fails if the
// archived summary drops the field or an offboarded agent is listed as
// having no limits.
func TestAPIArchivedAgentExemption(t *testing.T) {
	srv, _ := networkServer(t)
	if err := srv.Store.SetAgentNetworkExempt("bob", true); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	if org := orgOK[apitypes.Network](t, orgGet(t, srv, "/api/v1/org/network")); len(org.Exempt) != 0 {
		t.Fatalf("org network lists %v", org.Exempt)
	}
	summaries := decodeAPI[apitypes.AgentsResponse](t, apiDo(t, srv, http.MethodGet, "/api/v1/agents", "", nil))
	if len(summaries.Archived) != 1 || !summaries.Archived[0].NetworkExempt {
		t.Fatalf("archived = %+v", summaries.Archived)
	}
}
