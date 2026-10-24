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

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: a label the owner never gave is Git's host,
// derived on every read. Stored at save, it would stay "github.com"
// after the owner pointed the connection at another host, and the
// card, the access table and the agent's tab would name the old host.
// A label sent equal to the derived one is the derived one, and a host
// longer than a label may be is cut to fit, never refused.
func TestDerivedLabelFollowsTheHost(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	withGit(t, srv)
	page := addConnection(t, srv, "git", "", map[string]string{"host": "github.com", "token": gitToken})
	id := page.Connections[0].ID
	if rec, _ := srv.Store.GetIntegrationConnectionByID("git", id); rec.Label != "" || page.Connections[0].Label != "github.com" {
		t.Fatalf("stored %q, shown %q", rec.Label, page.Connections[0].Label)
	}
	page = orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/git/connections/"+id, apitypes.ConnectionWrite{Fields: map[string]string{"host": "github.example.com"}}))
	if got := page.Connections[0].Label; got != "github.example.com" {
		t.Errorf("label after the host changed = %q", got)
	}

	tick(srv)
	page = addConnection(t, srv, "git", "gitlab.com", map[string]string{"host": "gitlab.com", "token": gitLabToken})
	lab := page.Connections[1].ID
	if rec, _ := srv.Store.GetIntegrationConnectionByID("git", lab); rec.Label != "" {
		t.Errorf("a label sent equal to the host was stored as %q", rec.Label)
	}

	tick(srv)
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 20) + ".example.com"
	page = addConnection(t, srv, "git", "", map[string]string{"host": long, "token": "x-token-123456"})
	if got := page.Connections[2].Label; got != long[:connectionLabelMax] {
		t.Errorf("a long host's label = %q", got)
	}
}

// Why this test exists: an agent on the second of two connections
// works only while that connection has its secrets. Its Integrations
// tab reading usable from the integration alone would say so through
// the other connection while every one of its calls fails.
func TestAgentTabUsableIsTheConnections(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withMultipleEcho(t, srv)
	intTeam(t, srv, "dana", "lee")
	addConnection(t, srv, "echo", "First", map[string]string{"token": echoToken})
	tick(srv)
	page := addConnection(t, srv, "echo", "Second", map[string]string{"token": "s3cret-other-echo-token"})
	first, second := page.Connections[0], page.Connections[1]
	putAccessFor(t, srv, "echo", "dana", `{}`, first.ID)
	putAccessFor(t, srv, "echo", "lee", `{}`, second.ID)
	_ = mem.Delete(context.Background(), secrets.ConnectionSecretName("echo", first.ID))
	tab := func(slug string) bool {
		t.Helper()
		got := orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/"+slug+"/integrations"))
		if len(got.Connected) != 1 {
			t.Fatalf("%s's tab = %+v", slug, got)
		}
		return got.Connected[0].Usable
	}
	if tab("dana") {
		t.Error("dana, on the connection that lost its secrets, reads usable")
	}
	if !tab("lee") {
		t.Error("lee, on the one that has them, reads not usable")
	}
}

// Why this test exists: an integration that takes one connection uses
// only the oldest. A second record (a leftover, or one written by
// hand) that has its secrets must not make the connection requirement
// read met while the connection agents use needs its secrets again.
func TestOneConnectionRequirementIsTheOldest(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	intConnected(t, srv, "crm")
	attachShim(t, srv, "crm", toolTexts(nil))
	oldest, err := srv.Store.GetIntegrationConnection("crm")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.PutIntegrationConnection("crm", store.IntegrationConnection{ID: "later", CreatedAt: oldest.CreatedAt.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_ = mem.Delete(context.Background(), oldest.SecretName)
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if row, _ := requirementOf(page, "connection"); row.Met {
		t.Errorf("connection = %+v, want not met", row)
	}
}

// Why this test exists: the one-connection page sends no connection
// id, and a page read before the connection was replaced may send the
// old one. That is a stale page, not a choice to make: a 409 that says
// the connection is gone, so the owner reloads, never "choose a
// connection" on a page that offers none.
func TestOneConnectionAccessWithAStaleID(t *testing.T) {
	srv := newTestServer(t)
	intTeam(t, srv, "dana")
	intConnected(t, srv, "crm")
	if e := assertAPIError(t, putAccessFor(t, srv, "crm", "dana", `{"blogs":["M"]}`, "stale"), http.StatusConflict); e.Error != errConnectionGone {
		t.Errorf("a stale id = %+v", e)
	}
	if rr := putAccessFor(t, srv, "crm", "dana", `{"blogs":["M"]}`, ""); rr.Code != http.StatusOK {
		t.Errorf("no id = %d %s", rr.Code, rr.Body.String())
	}
}

// Why this test exists: the owner can move an agent's access to
// another connection while the agent's clone runs. The second request
// then finds the access naming the new connection, whose host is not
// the one the job reached: checking the result against it would refuse
// a clone that already happened. It is told as it ran, the audit says
// the access changed and names the connection it ran through, and the
// job's connection's token is scrubbed from both, though the access no
// longer names it. A job connection deleted since still answers, and
// the audit says only the current connection's secrets were scrubbed.
func TestGitJobTellsWhatRanThroughItsConnection(t *testing.T) {
	srv := newTestServer(t)
	useFakeClock(srv)
	withGit(t, srv)
	intTeam(t, srv, "dana")
	_, hub, lab := twoGitConnections(t, srv)
	if rr := putAccessFor(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`, hub.ID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()
	args := json.RawMessage(`{"url":"https://github.com/acme/widgets","path":"/files/widgets"}`)
	first, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args})
	if err != nil || first.ShellJob == nil || first.ShellJob.Git == nil || first.ShellJob.Git.ConnectionID != hub.ID {
		t.Fatalf("first request = %+v, %v", first, err)
	}
	if rr := putAccessFor(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`, lab.ID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	second, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args, ShellJobResult: &agentpod.IntegrationShellJobResult{
		Git: &agentpod.GitResult{Repository: "acme/widgets", Error: "the host said " + gitToken + " was refused", ConnectionID: hub.ID},
	}})
	text := strings.Join(second.Content, " ")
	if err != nil || !second.IsError || !strings.Contains(text, "changed to another connection during the clone") || strings.Contains(text, gitToken) {
		t.Fatalf("second request = %+v, %v", second, err)
	}
	row := gitRows(t, srv)[0]
	if !row.Allowed || strings.Contains(row.Reason, gitToken) ||
		!strings.HasSuffix(row.Reason, "; the access changed during the clone (it ran through the connection github.com)") {
		t.Errorf("audit = %+v", row)
	}

	if rr := intDelete(t, srv, intBase+"/git/connections/"+hub.ID); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	gone, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args, ShellJobResult: &agentpod.IntegrationShellJobResult{
		Git: &agentpod.GitResult{Repository: "acme/widgets", ConnectionID: hub.ID},
	}})
	if err != nil || gone.IsError || !strings.HasPrefix(strings.Join(gone.Content, " "), "Cloned acme/widgets.") {
		t.Fatalf("after the job's connection went = %+v, %v", gone, err)
	}
	if row := gitRows(t, srv)[0]; !strings.HasSuffix(row.Reason, "(it ran through a connection that is no longer there, so only the current connection's secrets were scrubbed)") {
		t.Errorf("audit after the job's connection went = %+v", row)
	}
}

// Why this test exists: a built-in's Test connection is scrubbed with
// the secrets of the connection it called with. With none (they could
// not be read), scrubbing with another connection's secrets would keep
// this connection's token in the page, the record and the audit: the
// answer is held back instead.
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

// Why this test exists: deleting one connection deletes its Secret
// first, which holds its proxy credential's secret. If the store's
// step then failed and the credential stayed registered, the proxy
// would go on admitting a credential nothing can renew or drop through
// the page; it is dropped as soon as the Secret is gone.
func TestDeleteByIDDropsTheCredentialWhenTheStoreFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory without write permission")
	}
	srv := newTestServer(t)
	useFakeClock(srv)
	withGit(t, srv)
	_, hub, lab := twoGitConnections(t, srv)
	dir := filepath.Join(srv.Store.Root(), store.IntegrationsDirName, "git", "connections")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	assertAPIError(t, intDelete(t, srv, intBase+"/git/connections/"+lab.ID), http.StatusInternalServerError)
	creds := srv.Store.EgressScopedCredentials()
	if _, ok := creds[builtinintegrations.ProxyUser("git", lab.ID)]; ok {
		t.Error("the credential of a connection whose Secret is gone stayed registered")
	}
	if _, ok := creds[builtinintegrations.ProxyUser("git", hub.ID)]; !ok {
		t.Error("the other connection's credential went too")
	}
}
