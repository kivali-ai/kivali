package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// gitLabToken is the second Git connection's token in these tests.
const gitLabToken = "glpat-web-git-test-token-4567"

// createdPage decodes a 201 answer, failing on any other status.
func createdPage(t *testing.T, rr *httptest.ResponseRecorder) apitypes.IntegrationPage {
	t.Helper()
	if rr.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201 (body %s)", rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.IntegrationPage](t, rr)
}

// useFakeClock gives srv a clock tick advances, so connections made
// one after another are ordered oldest first by their creation time.
func useFakeClock(srv *Server) { srv.Clock = clock.NewFake() }

func tick(srv *Server) { srv.Clock.(*clock.Fake).Advance(time.Minute) }

// addConnection posts a new connection of name.
func addConnection(t *testing.T, srv *Server, name, label string, fields map[string]string) apitypes.IntegrationPage {
	t.Helper()
	return createdPage(t, orgJSON(t, srv, http.MethodPost, intBase+"/"+name+"/connections", apitypes.ConnectionWrite{Label: label, Fields: fields}))
}

// twoGitConnections connects Git to github.com (labelled by its host)
// and to a company GitLab, and returns the page and both connections.
func twoGitConnections(t *testing.T, srv *Server) (apitypes.IntegrationPage, apitypes.ConnectionState, apitypes.ConnectionState) {
	t.Helper()
	addConnection(t, srv, "git", "", map[string]string{"host": "github.com", "token": gitToken})
	tick(srv)
	page := addConnection(t, srv, "git", "Company GitLab", map[string]string{"host": "gitlab.example.com", "token": gitLabToken})
	if len(page.Connections) != 2 {
		t.Fatalf("connections = %+v", page.Connections)
	}
	return page, page.Connections[0], page.Connections[1]
}

func connectionValue(c apitypes.ConnectionState, key string) string {
	for _, f := range c.Fields {
		if f.Key == key && f.Value != nil {
			return *f.Value
		}
	}
	return ""
}

func putAccessFor(t *testing.T, srv *Server, name, slug, access, connID string) *httptest.ResponseRecorder {
	t.Helper()
	return orgJSON(t, srv, http.MethodPut, intBase+"/"+name+"/access/"+slug, apitypes.AccessWrite{Access: []byte(access), ConnectionID: connID})
}

// Why this test exists: Git is the integration that takes several
// connections, GitHub and a company GitLab side by side. Each must be
// stored under its own id with its own label and fields: a label left
// empty takes the host, a label another connection has (ignoring case)
// is a 409, and the page lists every connection, oldest first, each
// with its own values, while Connection is the first, which the
// one-connection page reads. Saving one by id must change
// that one alone and keep its token when the token is left out.
func TestGitTakesSeveralConnections(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withGit(t, srv)
	page, hub, lab := twoGitConnections(t, srv)
	if !page.Multiple || page.Summary.Status != apitypes.IntegrationStatusConnected {
		t.Errorf("page = multiple %v, status %s", page.Multiple, page.Summary.Status)
	}
	if hub.Label != "github.com" || lab.Label != "Company GitLab" || hub.ID == lab.ID {
		t.Errorf("labels = %q (%s), %q (%s)", hub.Label, hub.ID, lab.Label, lab.ID)
	}
	if connectionValue(hub, "host") != "github.com" || connectionValue(lab, "host") != "gitlab.example.com" {
		t.Errorf("hosts = %q, %q", connectionValue(hub, "host"), connectionValue(lab, "host"))
	}
	if page.Connection == nil || page.Connection.ID != hub.ID {
		t.Errorf("connection = %+v, want the first", page.Connection)
	}
	if strings.Contains(orgGet(t, srv, intBase+"/git").Body.String(), gitLabToken) {
		t.Error("the page carries a token")
	}
	if got, _ := mem.Get(context.Background(), secrets.ConnectionSecretName("git", lab.ID)); got["token"] != gitLabToken {
		t.Errorf("the second connection's Secret = %v", got)
	}

	dup := orgJSON(t, srv, http.MethodPost, intBase+"/git/connections", apitypes.ConnectionWrite{Label: "company gitlab", Fields: map[string]string{"host": "gitlab.com", "token": "x-token-123"}})
	if e := assertAPIError(t, dup, http.StatusConflict); e.Error != "Git already has a connection called Company GitLab" {
		t.Errorf("a label taken = %+v", e)
	}
	sameHost := orgJSON(t, srv, http.MethodPost, intBase+"/git/connections", apitypes.ConnectionWrite{Fields: map[string]string{"host": "https://github.com/", "token": "x-token-123"}})
	if e := assertAPIError(t, sameHost, http.StatusConflict); e.Error != "Git already has a connection called github.com" {
		t.Errorf("a second github.com without a label = %+v", e)
	}

	// By id: the second one's host and label change, its token stays.
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/git/connections/"+lab.ID, apitypes.ConnectionWrite{Label: "GitLab", Fields: map[string]string{"host": "gitlab.example.org"}})
	page = orgOK[apitypes.IntegrationPage](t, rr)
	if c := page.Connections[1]; c.ID != lab.ID || c.Label != "GitLab" || connectionValue(c, "host") != "gitlab.example.org" {
		t.Errorf("after the put = %+v", c)
	}
	if c := page.Connections[0]; c.Label != "github.com" || connectionValue(c, "host") != "github.com" {
		t.Errorf("the other connection changed: %+v", c)
	}
	if got, _ := mem.Get(context.Background(), secrets.ConnectionSecretName("git", lab.ID)); got["token"] != gitLabToken {
		t.Errorf("the token after a put without it = %v", got)
	}
	// An empty label keeps the stored one; another's label is a 409.
	page = orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/git/connections/"+lab.ID, apitypes.ConnectionWrite{Fields: map[string]string{"host": "gitlab.example.org"}}))
	if page.Connections[1].Label != "GitLab" {
		t.Errorf("label after a put without one = %q", page.Connections[1].Label)
	}
	assertAPIError(t, orgJSON(t, srv, http.MethodPut, intBase+"/git/connections/"+lab.ID, apitypes.ConnectionWrite{Label: "GITHUB.COM", Fields: map[string]string{"host": "gitlab.example.org"}}), http.StatusConflict)
	assertAPIError(t, orgJSON(t, srv, http.MethodPut, intBase+"/git/connections/nope", apitypes.ConnectionWrite{Fields: map[string]string{"host": "x.com"}}), http.StatusNotFound)
	assertAPIError(t, intPost(t, srv, intBase+"/git/connections/nope/verify"), http.StatusNotFound)
	assertAPIError(t, intDelete(t, srv, intBase+"/git/connections/nope"), http.StatusNotFound)
}

// Why this test exists: an integration whose built-in gives no label
// must be given one when it takes several connections, or the page and
// the access dialog would offer two connections with no name to choose
// between.
func TestSeveralConnectionsNeedALabel(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	withMultipleEcho(t, srv)
	rr := orgJSON(t, srv, http.MethodPost, intBase+"/echo/connections", apitypes.ConnectionWrite{Fields: map[string]string{"token": echoToken}})
	if e := assertAPIError(t, rr, http.StatusBadRequest); e.Error != "the connection needs a label" {
		t.Errorf("no label = %+v", e)
	}
	rr = orgJSON(t, srv, http.MethodPost, intBase+"/echo/connections", apitypes.ConnectionWrite{Label: strings.Repeat("x", connectionLabelMax+1), Fields: map[string]string{"token": echoToken}})
	assertAPIError(t, rr, http.StatusBadRequest)
}

// Why this test exists: an integration that takes one connection must
// keep taking one. A second POST .../connections would give a bundle's
// agents a connection its server never starts with; it is a 409 in the
// integration's name, and the first is created as the singular route
// would create it.
func TestOneConnectionIntegrationRefusesASecond(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	intInstalled(t, srv, "crm", "1.0.0")
	page := addConnection(t, srv, "crm", "", map[string]string{"token": "s3cret-crm"})
	if page.Multiple || len(page.Connections) != 1 || page.Connection == nil || page.Connection.ID != page.Connections[0].ID || page.Connection.Label != "" {
		t.Errorf("first = %+v", page)
	}
	rr := orgJSON(t, srv, http.MethodPost, intBase+"/crm/connections", apitypes.ConnectionWrite{Fields: map[string]string{"token": "another"}})
	if e := assertAPIError(t, rr, http.StatusConflict); e.Error != "Test crm takes one connection" {
		t.Errorf("a second = %+v", e)
	}
	// The singular route still edits the one.
	page = orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"portal": "acme"}}))
	if len(page.Connections) != 1 || connectionValue(page.Connections[0], "portal") != "acme" {
		t.Errorf("after the singular put = %+v", page.Connections)
	}
}

// Why this test exists: with two connections an access row must name
// the one the agent's calls use. Saved without one, a row would be
// set for a connection the owner never chose, so it is a 400, as is an
// id that names no connection; with one connection the id may be left
// out and is the one. The page's rows and the agent's tab must say
// which connection each agent uses, or the owner cannot tell GitHub
// access from GitLab access.
func TestAccessNamesAConnection(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	withGit(t, srv)
	intTeam(t, srv, "dana", "lee")
	connectGit(t, srv, "github.com")
	// One connection: the id may be left out.
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)

	tick(srv)
	page := addConnection(t, srv, "git", "Company GitLab", map[string]string{"host": "gitlab.example.com", "token": gitLabToken})
	hub, lab := page.Connections[0], page.Connections[1]
	if e := assertAPIError(t, putAccessFor(t, srv, "git", "lee", `{"pull_repositories":["eng/*"]}`, ""), http.StatusBadRequest); e.Error != "choose a connection" {
		t.Errorf("no connection named = %+v", e)
	}
	assertAPIError(t, putAccessFor(t, srv, "git", "lee", `{"pull_repositories":["eng/*"]}`, "nope"), http.StatusBadRequest)
	page = orgOK[apitypes.IntegrationPage](t, putAccessFor(t, srv, "git", "lee", `{"pull_repositories":["eng/*"]}`, lab.ID))
	rows := map[string]apitypes.AccessRow{}
	for _, r := range page.Access {
		rows[r.Agent.Slug] = r
	}
	if rows["dana"].ConnectionID != hub.ID || rows["dana"].ConnectionLabel != "github.com" || rows["dana"].Invalid != nil ||
		rows["lee"].ConnectionID != lab.ID || rows["lee"].ConnectionLabel != "Company GitLab" || rows["lee"].Invalid != nil {
		t.Errorf("rows = %+v", rows)
	}
	tab := orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/lee/integrations"))
	if len(tab.Connected) != 1 || tab.Connected[0].ConnectionLabel != "Company GitLab" {
		t.Errorf("lee's tab = %+v", tab.Connected)
	}
}

// Why this test exists: an agent's call must reach the connection its
// access names, not the oldest. If dispatch read the oldest, an agent
// given the company GitLab would be handed the GitHub token and host,
// and its clone would go to the wrong host with the wrong secret. Each
// job carries its own connection's host, token and scoped proxy
// credential.
func TestGitCallUsesTheConnectionItsAccessNames(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	withGit(t, srv)
	intTeam(t, srv, "dana", "lee")
	_, hub, lab := twoGitConnections(t, srv)
	if rr := putAccessFor(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`, hub.ID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if rr := putAccessFor(t, srv, "git", "lee", `{"pull_repositories":["eng/*"]}`, lab.ID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	ctx := context.Background()
	sock := serveCoreUDS(t, srv)
	creds := srv.Store.EgressScopedCredentials()
	for _, c := range []struct {
		slug, url, host, token, connID string
	}{
		{"dana", "https://github.com/acme/widgets", "github.com", gitToken, hub.ID},
		{"lee", "https://gitlab.example.com/eng/tools", "gitlab.example.com", gitLabToken, lab.ID},
	} {
		client := agentpod.NewClient(sock, c.slug)
		list, err := client.IntegrationTools(ctx)
		if err != nil || len(list.Tools) == 0 {
			t.Fatalf("%s's actions = %+v, %v", c.slug, list, err)
		}
		shell := &fakeGitShell{res: &agentshell.GitResponse{Repository: "x/y", DurationMs: 10}}
		clone := mcp.IntegrationTools(client, shell, list.Tools, nil)[0]
		if _, err := clone.Handler(ctx, json.RawMessage(`{"url":"`+c.url+`","path":"/files/w"}`)); err != nil {
			t.Fatalf("%s's clone: %v", c.slug, err)
		}
		if len(shell.jobs) != 1 {
			t.Fatalf("%s's jobs = %d", c.slug, len(shell.jobs))
		}
		j := shell.jobs[0]
		proxy, ok := creds[builtinintegrations.ProxyUser("git", c.connID)]
		if j.Host != c.host || j.Auth.Password != c.token || j.Proxy.Username != builtinintegrations.ProxyUser("git", c.connID) ||
			!ok || proxy.Hash != egresscred.Hash(j.Proxy.Password) || !slices.Equal(proxy.Hosts, []string{c.host}) {
			t.Errorf("%s's job = %+v (proxy %+v %v)", c.slug, j, proxy, ok)
		}
	}
}

// Why this test exists: each connection has a scoped proxy credential
// of its own, admitted to its own host. Every path that registers or
// drops credentials must cover every connection: the switch, boot,
// and deleting one connection, which must drop that connection's
// credential, Secret and access rows only. Dropping all of them would
// cut off the agents on the other host; dropping none would leave a
// deleted connection's credential reaching its host.
func TestGitConnectionsCredentialsAndDeleteByID(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withGit(t, srv)
	intTeam(t, srv, "dana", "lee")
	_, hub, lab := twoGitConnections(t, srv)
	hubUser, labUser := builtinintegrations.ProxyUser("git", hub.ID), builtinintegrations.ProxyUser("git", lab.ID)
	registered := func() (bool, bool) {
		c := srv.Store.EgressScopedCredentials()
		_, a := c[hubUser]
		_, b := c[labUser]
		return a, b
	}
	if a, b := registered(); !a || !b {
		t.Fatalf("registered after the saves = %v, %v", a, b)
	}
	if got := srv.Store.EgressScopedCredentials()[labUser]; !slices.Equal(got.Hosts, []string{"gitlab.example.com"}) {
		t.Errorf("the second connection's hosts = %v", got.Hosts)
	}

	intPost(t, srv, intBase+"/git/disable")
	if a, b := registered(); a || b {
		t.Errorf("registered while off = %v, %v", a, b)
	}
	intPost(t, srv, intBase+"/git/enable")
	if a, b := registered(); !a || !b {
		t.Errorf("registered after on = %v, %v", a, b)
	}
	for _, u := range []string{hubUser, labUser} {
		if err := srv.Store.DropEgressScopedCredential(u); err != nil {
			t.Fatal(err)
		}
	}
	srv.RegisterBuiltinEgress(context.Background())
	if a, b := registered(); !a || !b {
		t.Errorf("registered after boot = %v, %v", a, b)
	}

	if rr := putAccessFor(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`, hub.ID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if rr := putAccessFor(t, srv, "git", "lee", `{"pull_repositories":["eng/*"]}`, lab.ID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	page := orgOK[apitypes.IntegrationPage](t, intDelete(t, srv, intBase+"/git/connections/"+lab.ID))
	if len(page.Connections) != 1 || page.Connections[0].ID != hub.ID {
		t.Errorf("connections after the delete = %+v", page.Connections)
	}
	if len(page.Access) != 1 || page.Access[0].Agent.Slug != "dana" || page.Access[0].Invalid != nil {
		t.Errorf("access after the delete = %+v", page.Access)
	}
	if a, b := registered(); !a || b {
		t.Errorf("registered after deleting the second = %v, %v", a, b)
	}
	if names := mem.Names(); !slices.Equal(names, []string{secrets.ConnectionSecretName("git", hub.ID)}) {
		t.Errorf("secrets after deleting the second = %v", names)
	}
}

// withMultipleEcho gives srv the echo built-in taking several
// connections.
func withMultipleEcho(t *testing.T, srv *Server) *echotest.Echo {
	t.Helper()
	echo := echotest.New()
	echo.Multiple = true
	srv.BuiltinIntegrations = builtinintegrations.Registry{echo}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	return echo
}

// Why this test exists: Test connection by id must test that
// connection, keep the result on that connection alone, and scrub that
// connection's secrets from what it keeps. Scrubbing with the first
// connection's secrets only would keep the second's token in the
// page, the record and the audit.
func TestVerifyByIDTestsThatConnection(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	echo := withMultipleEcho(t, srv)
	const otherToken = "s3cret-other-echo-token"
	addConnection(t, srv, "echo", "First", map[string]string{"token": echoToken, "greeting": "Hello"})
	tick(srv)
	page := addConnection(t, srv, "echo", "Second", map[string]string{"token": otherToken, "greeting": echotest.FailGreeting})
	first, second := page.Connections[0], page.Connections[1]

	rr := intPost(t, srv, intBase+"/echo/connections/"+second.ID+"/verify")
	if strings.Contains(rr.Body.String(), otherToken) {
		t.Fatal("the answer carries the second connection's token")
	}
	page = orgOK[apitypes.IntegrationPage](t, rr)
	calls := echo.Calls()
	if len(calls) != 1 || calls[0].Action != "verify" || calls[0].Conn.ID != second.ID || calls[0].Conn.Fields["token"] != otherToken {
		t.Errorf("Verify was given %+v", calls)
	}
	if c := page.Connections[1]; c.Verified == nil || *c.Verified || c.VerifyMessage == nil || *c.VerifyMessage != "the token [secret] was refused" {
		t.Errorf("second after its test = %+v", c)
	}
	if c := page.Connections[0]; c.ID != first.ID || c.Verified != nil {
		t.Errorf("first after the second's test = %+v", c)
	}
	audit := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/echo/audit"))
	if len(audit.Rows) != 1 || audit.Rows[0].Reason != "the token [secret] was refused" {
		t.Errorf("audit = %+v", audit.Rows)
	}
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/connections/"+first.ID+"/verify"))
	if c := page.Connections[0]; c.Verified == nil || !*c.Verified || c.VerifyMessage == nil || *c.VerifyMessage != "Hello; token s3c..." {
		t.Errorf("first after its test = %+v", c)
	}
	// The options a field lists come through the connection named.
	orgOK[apitypes.AccessChoices](t, orgGet(t, srv, intBase+"/echo/choices/colors?connection="+second.ID))
	if calls := echo.Calls(); calls[len(calls)-1].Action != "choices" || calls[len(calls)-1].Conn.ID != second.ID {
		t.Errorf("Choices was given %+v", calls[len(calls)-1])
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/echo/choices/colors?connection=nope"), http.StatusNotFound)
}

// Why this test exists: with several connections the integration is
// connected while any one of them has all its secrets. Reading only the
// first would call it not connected when the first lost its Secret and
// the second still works, and agents on the second would look cut off.
func TestStatusWithOneOfTwoConnected(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withMultipleEcho(t, srv)
	addConnection(t, srv, "echo", "First", map[string]string{"token": echoToken})
	tick(srv)
	page := addConnection(t, srv, "echo", "Second", map[string]string{"token": "s3cret-other-echo-token"})
	first, second := page.Connections[0], page.Connections[1]
	_ = mem.Delete(context.Background(), secrets.ConnectionSecretName("echo", first.ID))
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/echo"))
	if page.Summary.Status != apitypes.IntegrationStatusConnected || !page.Connections[0].ReconnectNeeded || page.Connections[1].ReconnectNeeded {
		t.Errorf("one of two = %s %+v", page.Summary.Status, page.Connections)
	}
	_ = mem.Delete(context.Background(), secrets.ConnectionSecretName("echo", second.ID))
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/echo"))
	if page.Summary.Status != apitypes.IntegrationStatusNotConnected {
		t.Errorf("none of two = %s", page.Summary.Status)
	}
	if _, err := srv.Store.GetIntegrationConnectionByID("echo", second.ID); err != nil {
		t.Errorf("the record went with its Secret: %v", err)
	}
}
