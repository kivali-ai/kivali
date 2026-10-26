package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// otherGitToken is a second token in these tests: a connection's other
// than the integration's.
const otherGitToken = "glpat-web-git-test-token-4567"

// Why this test exists: an integration has one connection, and the
// installation is its identity. The routes that made, saved, tested,
// signed out and deleted one of several by id must be gone, and a body
// naming a connection's label or an access row's connection must be
// refused as a field Kivali does not read, not quietly taken: a page
// or client still sending them would otherwise believe it had chosen
// a connection.
func TestOneConnectionPerInstallation(t *testing.T) {
	srv := newTestServer(t)
	withGit(t, srv)
	intTeam(t, srv, "dana")
	page := connectGit(t, srv, "git.example.com")
	id := page.Connection.ID
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, intBase + "/git/connections"},
		{http.MethodPut, intBase + "/git/connections/" + id},
		{http.MethodDelete, intBase + "/git/connections/" + id},
		{http.MethodPost, intBase + "/git/connections/" + id + "/verify"},
		{http.MethodPost, intBase + "/git/connections/" + id + "/sign-out"},
	} {
		rr := orgDo(t, srv, "", c.method, c.path, "application/json", []byte(`{"fields":{"host":"gitlab.example.com","token":"x-token-123"}}`))
		if rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d %s", c.method, c.path, rr.Code, rr.Body.String())
		}
	}
	if conns, _ := srv.Store.ListIntegrationConnections("git"); len(conns) != 1 || conns[0].ID != id || conns[0].Fields["host"] != "git.example.com" {
		t.Errorf("connections after the removed routes = %+v", conns)
	}

	// The one route saves the one connection, under its id.
	page = connectGit(t, srv, "gitlab.example.com")
	if page.Connection == nil || page.Connection.ID != id {
		t.Errorf("the second save made another connection: %+v", page.Connection)
	}
	assertAPIError(t, orgDo(t, srv, "", http.MethodPut, intBase+"/git/connection", "application/json", []byte(`{"label":"Company","fields":{"host":"git.example.com"}}`)), http.StatusBadRequest)
	assertAPIError(t, orgDo(t, srv, "", http.MethodPut, intBase+"/git/access/dana", "application/json", []byte(`{"access":{"pull_repositories":["acme/*"]},"connection_id":"`+id+`"}`)), http.StatusBadRequest)

	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)
	body := orgGet(t, srv, intBase+"/git").Body.String()
	tab := orgGet(t, srv, "/api/v1/agents/dana/integrations").Body.String()
	for _, key := range []string{`"connections"`, `"multiple"`, `"label"`, `"connection_id"`, `"connection_label"`} {
		if strings.Contains(body, key) || strings.Contains(tab, key) {
			t.Errorf("%s is on the wire:\npage %s\ntab %s", key, body, tab)
		}
	}
}

// legacyGitWithTwo is Git as a Kivali that took several connections
// left it: hub (github.com, labelled GitHub, the oldest) and lab
// (gitlab.example.com, no label), each with its Secret, and the access
// rows of dana (hub) and lee and kim (lab), each naming its connection.
func legacyGitWithTwo(t *testing.T, srv *Server, mem *secrets.Memory) (hub, lab string) {
	t.Helper()
	withGit(t, srv)
	intTeam(t, srv, "dana", "lee", "kim")
	dir := filepath.Join(srv.Store.Root(), store.IntegrationsDirName, "git")
	write := func(rel string, v any) {
		t.Helper()
		b, err := yaml.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i, c := range []struct{ id, label, host, token string }{
		{"a1b2c3d4", "GitHub", "github.com", gitToken},
		{"e5f6a7b8", "", "gitlab.example.com", otherGitToken},
	} {
		secret := secrets.ConnectionSecretName("git", c.id)
		if err := mem.Put(context.Background(), secret, map[string]string{"token": c.token, builtinintegrations.ProxySecretKey: "proxy-secret-" + c.id + "-0123456789abcdef"}); err != nil {
			t.Fatal(err)
		}
		rec := map[string]any{"id": c.id, "created_at": created.Add(time.Duration(i) * time.Hour), "fields": map[string]string{"host": c.host},
			"secret_keys": []string{"token"}, "secret_name": secret}
		if c.label != "" {
			rec["label"] = c.label
		}
		write("connections/"+c.id+".yaml", rec)
	}
	for slug, id := range map[string]string{"dana": "a1b2c3d4", "lee": "e5f6a7b8", "kim": "e5f6a7b8"} {
		write("access/"+slug+".yaml", map[string]any{"slug": slug, "connection_id": id, "set_at": created, "set_by": "ceo",
			"access": map[string]any{"pull_repositories": []any{"acme/*"}}})
	}
	return "a1b2c3d4", "e5f6a7b8"
}

// Why this test exists: after the boot that gives Git one connection,
// what the owner and the agents see must follow it end to end. The
// page shows the connection kept, with each agent's row valid or
// saying which connection it was set for; the agent whose access was
// set aside reads it on its tab and is refused with the reason; the
// others' calls go through the kept connection's host, token and proxy
// credential; the set-aside connection's proxy credential no longer
// reaches its host; and setting the access again gives it back.
func TestGitAfterOneConnectionMigration(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	hub, lab := legacyGitWithTwo(t, srv, mem)
	// An earlier boot registered both connections' credentials.
	for _, id := range []string{hub, lab} {
		if err := srv.Store.SetEgressScopedCredential(builtinintegrations.ProxyUser("git", id), egresscred.Hash("proxy-secret-"+id+"-0123456789abcdef"), []string{"example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.Store.MigrateIntegrationConnections(); err != nil {
		t.Fatal(err)
	}
	srv.RegisterBuiltinEgress(context.Background())

	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/git"))
	if page.Connection == nil || page.Connection.ID != lab || !connectedAndMet(page) {
		t.Fatalf("page = %+v, unmet %v", page.Connection, unmetIDs(page))
	}
	rows := map[string]apitypes.AccessRow{}
	for _, r := range page.Access {
		rows[r.Agent.Slug] = r
	}
	const reason = "set for the connection GitHub, which Git (Basic) no longer has"
	if rows["lee"].Invalid != nil || rows["kim"].Invalid != nil || rows["dana"].Invalid == nil || *rows["dana"].Invalid != reason {
		t.Errorf("rows = %+v", rows)
	}
	tab := orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/dana/integrations"))
	if len(tab.Connected) != 1 || tab.Connected[0].Usable || tab.Connected[0].Invalid == nil || *tab.Connected[0].Invalid != reason {
		t.Errorf("dana's tab = %+v", tab.Connected)
	}
	creds := srv.Store.EgressScopedCredentials()
	if _, ok := creds[builtinintegrations.ProxyUser("git", hub)]; ok {
		t.Error("the connection set aside keeps its proxy credential")
	}
	if c, ok := creds[builtinintegrations.ProxyUser("git", lab)]; !ok || len(c.Hosts) != 1 || c.Hosts[0] != "gitlab.example.com" {
		t.Errorf("the kept connection's credential = %+v, %v", c, ok)
	}
	if names := mem.Names(); len(names) != 2 {
		t.Errorf("secrets after the migration = %v; the one set aside is kept", names)
	}

	ctx := context.Background()
	sock := serveCoreUDS(t, srv)
	args := json.RawMessage(`{"url":"https://gitlab.example.com/acme/tools","path":"/files/w"}`)
	refused, err := agentpod.NewClient(sock, "dana").IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args})
	if err != nil || !refused.IsError || !strings.Contains(strings.Join(refused.Content, " "), reason) {
		t.Errorf("dana's call = %+v, %v", refused, err)
	}
	lee := agentpod.NewClient(sock, "lee")
	list, err := lee.IntegrationTools(ctx)
	if err != nil || len(list.Tools) == 0 {
		t.Fatalf("lee's actions = %+v, %v", list, err)
	}
	shell := &fakeGitShell{res: &agentshell.GitResponse{Repository: "acme/tools", DurationMs: 10}}
	if _, err := mcp.IntegrationTools(lee, shell, list.Tools, nil)[0].Handler(ctx, args); err != nil || len(shell.jobs) != 1 {
		t.Fatalf("lee's clone: %v, jobs %d", err, len(shell.jobs))
	}
	if j := shell.jobs[0]; j.Host != "gitlab.example.com" || j.Auth.Password != otherGitToken || j.Proxy.Username != builtinintegrations.ProxyUser("git", lab) || j.ConnectionID != lab {
		t.Errorf("lee's job = %+v", j)
	}

	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)
	tab = orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/dana/integrations"))
	if len(tab.Connected) != 1 || !tab.Connected[0].Usable || tab.Connected[0].Invalid != nil {
		t.Errorf("dana's tab after setting the access again = %+v", tab.Connected)
	}

	// Disconnect forgets the connection set aside with the connection.
	if rr := intDelete(t, srv, intBase+"/git/connection"); rr.Code != http.StatusOK {
		t.Fatalf("disconnect = %d %s", rr.Code, rr.Body.String())
	}
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets after disconnect = %v", names)
	}
	if retired, _ := srv.Store.RetiredIntegrationConnections("git"); len(retired) != 0 {
		t.Errorf("set aside after disconnect = %+v", retired)
	}
}

// Why this test exists: a shell job carries the id of the connection it
// ran through, and Git can be uninstalled and installed again while it
// runs. The second request then finds another connection, whose host
// is not the one the job reached: checking the result against it would
// refuse a clone that already happened. It is told as it ran, the
// audit says the connection changed, and the job's connection's token
// is scrubbed from both when the migration set that connection aside
// (a job handed out before the upgrade), or the audit says only the
// current connection's secrets were scrubbed.
func TestGitJobTellsWhatRanThroughItsConnection(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withGit(t, srv)
	intTeam(t, srv, "dana")
	before := connectGit(t, srv, "git.example.com").Connection.ID
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()
	args := json.RawMessage(`{"url":"https://git.example.com/acme/widgets","path":"/files/widgets"}`)
	first, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args})
	if err != nil || first.ShellJob == nil || first.ShellJob.Git == nil || first.ShellJob.Git.ConnectionID != before {
		t.Fatalf("first request = %+v, %v", first, err)
	}
	if rr := intDelete(t, srv, intBase+"/git/connection"); rr.Code != http.StatusOK {
		t.Fatalf("disconnect = %d %s", rr.Code, rr.Body.String())
	}
	if after := connectGit(t, srv, "git.example.com").Connection.ID; after == before {
		t.Fatal("installing again kept the connection's id")
	}
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)

	second, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args, ShellJobResult: &agentpod.IntegrationShellJobResult{
		Git: &agentpod.GitResult{Repository: "acme/widgets", ConnectionID: before},
	}})
	text := strings.Join(second.Content, " ")
	if err != nil || second.IsError || !strings.HasPrefix(text, "Cloned acme/widgets.") || !strings.Contains(text, "The Git (Basic) connection changed during the clone") {
		t.Fatalf("second request = %+v, %v", second, err)
	}
	if row := gitRows(t, srv)[0]; !row.Allowed || !strings.HasSuffix(row.Reason, "; the connection changed during the clone (it ran through a connection that is no longer there, so only the current connection's secrets were scrubbed)") {
		t.Errorf("audit = %+v", row)
	}

	// A job handed out before the upgrade, through a connection the
	// migration then set aside: its token is scrubbed too.
	const retiredID = "5a5a5a5a"
	if err := mem.Put(ctx, secrets.ConnectionSecretName("git", retiredID), map[string]string{"token": otherGitToken}); err != nil {
		t.Fatal(err)
	}
	retired, err := yaml.Marshal(map[string]any{"id": retiredID, "label": "GitLab", "fields": map[string]string{"host": "gitlab.example.com"},
		"secret_keys": []string{"token"}, "secret_name": secrets.ConnectionSecretName("git", retiredID)})
	if err != nil {
		t.Fatal(err)
	}
	retiredDir := filepath.Join(srv.Store.Root(), store.IntegrationsDirName, "git", "connections-retired")
	if err := os.MkdirAll(retiredDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retiredDir, retiredID+".yaml"), retired, 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args, ShellJobResult: &agentpod.IntegrationShellJobResult{
		Git: &agentpod.GitResult{Repository: "acme/widgets", Error: "the host said " + otherGitToken + " was refused", ConnectionID: retiredID},
	}})
	if err != nil || !third.IsError || strings.Contains(strings.Join(third.Content, " "), otherGitToken) {
		t.Fatalf("third request = %+v, %v", third, err)
	}
	if row := gitRows(t, srv)[0]; strings.Contains(row.Reason, otherGitToken) || !strings.HasSuffix(row.Reason, "; the connection changed during the clone (it ran through a connection set aside)") {
		t.Errorf("audit = %+v", row)
	}
}

// Why this test exists: a migration that could not finish leaves
// several records under connections/, and an access row no longer says
// which it was set for. If the oldest served every call, an agent whose
// access was set for one host would reach another with its token. The
// page must still open (the owner can see it and uninstall it), every
// row and the connection requirement must say why nothing works, and
// an agent's call must be refused.
func TestSeveralConnectionRecordsGrantNothing(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	hub, _ := legacyGitWithTwo(t, srv, mem)
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/git"))
	const several = "Git (Basic) holds more than one connection record and Kivali could not bring it to one; see the log"
	if page.Connection == nil || page.Connection.ID != hub || len(page.Access) != 3 {
		t.Fatalf("page = %+v, access %+v", page.Connection, page.Access)
	}
	for _, r := range page.Access {
		if r.Invalid == nil || *r.Invalid != several {
			t.Errorf("%s's row = %+v", r.Agent.Slug, r.Invalid)
		}
	}
	if row, _ := requirementOf(page, "connection"); row.Met || row.Fix != "" || !strings.HasPrefix(row.Words, "It holds more than one connection record") {
		t.Errorf("connection row = %+v", row)
	}
	resp := callAs(t, srv, "lee", "git_clone", `{"url":"https://gitlab.example.com/acme/tools","path":"/files/w"}`)
	if !resp.IsError || !strings.Contains(strings.Join(resp.Content, " "), several) {
		t.Errorf("lee's call = %+v", resp)
	}
	if rr := intDelete(t, srv, intBase+"/git/connection"); rr.Code != http.StatusOK {
		t.Fatalf("disconnect = %d %s", rr.Code, rr.Body.String())
	}
	if conns, _ := srv.Store.ListIntegrationConnections("git"); len(conns) != 0 || len(mem.Names()) != 0 {
		t.Errorf("after disconnect: records %+v, secrets %v", conns, mem.Names())
	}
}

// Why this test exists: a built-in's Test connection is scrubbed with
// the secrets of the connection it called with. With none (they could
// not be read), scrubbing with nothing would keep the connection's
// token in the page, the record and the audit: the answer is held back
// instead.
func TestBuiltinOwnerCallWithNoScrubberIsHeldBack(t *testing.T) {
	srv := newTestServer(t)
	echo := withEcho(t, srv)
	v := &integrationView{it: store.Integration{Name: "echo"}, builtin: echo}
	res, failed, err := srv.callIntegrationTool(context.Background(), v, "whoami", func(context.Context) (json.RawMessage, *strings.Replacer, error) {
		return json.RawMessage(`{"content":[{"type":"text","text":"token ` + echoToken + `"}],"isError":true}`), nil, nil
	})
	if err != nil || len(res.Content) != 0 || failed != "Kivali could not check the integration's answer for secrets, so it held the answer back" {
		t.Errorf("result = %+v, failed %q, %v", res, failed, err)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("echo", 1); len(rows) != 1 || strings.Contains(rows[0].Reason, echoToken) {
		t.Errorf("audit = %+v", rows)
	}
}
