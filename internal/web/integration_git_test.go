package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitintegration"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// gitToken is the Git connection's token in these tests.
const gitToken = "ghp_web-git-test-token-0123"

// withGit gives srv the Git built-in, installed as boot installs it,
// with no pod provisioner: nothing it does may need a pod.
func withGit(t *testing.T, srv *Server) {
	t.Helper()
	srv.BuiltinIntegrations = builtinintegrations.Registry{gitintegration.New()}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func connectGit(t *testing.T, srv *Server, host string) apitypes.IntegrationPage {
	t.Helper()
	return orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/git/connection",
		apitypes.ConnectionWrite{Fields: map[string]string{"host": host, "token": gitToken}}))
}

// gitProxy is the Git connection's scoped proxy credential as the store
// registered it, its secret in the connection's Secret, and its
// connection id.
func gitProxy(t *testing.T, srv *Server) (store.EgressScoped, bool, string, string) {
	t.Helper()
	rec, err := srv.Store.GetIntegrationConnection("git")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := srv.Secrets.Get(context.Background(), rec.SecretName)
	e, ok := srv.Store.EgressScopedCredentials()[builtinintegrations.ProxyUser("git", rec.ID)]
	return e, ok, data[builtinintegrations.ProxySecretKey], rec.ID
}

// fakeGitShell stands for the agent-shell daemon: it records each job
// and answers with res.
type fakeGitShell struct {
	res  *agentshell.GitResponse
	jobs []agentshell.GitRequest
}

func (f *fakeGitShell) Git(_ context.Context, req agentshell.GitRequest) (*agentshell.GitResponse, error) {
	f.jobs = append(f.jobs, req)
	return f.res, nil
}

// gitRows is the Git integration's audit, newest first.
func gitRows(t *testing.T, srv *Server) []store.IntegrationAuditRow {
	t.Helper()
	rows, err := srv.Store.ReadIntegrationAuditTail("git", 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Why this test exists: a clone through the Git integration crosses
// core.sock twice with the job in the agent's shell between, on the
// real client and MCP tools. The job must carry core's URL, the token
// and the connection's own proxy credential (the one the store
// registered); only the second answer may reach the model; the call
// must leave two audit rows, one when the job is handed out naming the
// repository, one when it ends naming the refs; and core must check the agent's access again on the second request,
// so access taken away while the job ran refuses it.
func TestGitCallRelaysTheShellJob(t *testing.T) {
	srv := newTestServer(t)
	withGit(t, srv)
	intTeam(t, srv, "dana", "lee")
	connectGit(t, srv, "github.com")
	putIntegrationAccess(t, srv, "git", "dana", `{"repositories":["acme/*"]}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()

	list, err := dana.IntegrationTools(ctx)
	if err != nil || len(list.Tools) != 2 || list.Tools[0].Name != "git_clone" || list.Tools[1].Name != "git_fetch" {
		t.Fatalf("dana's actions = %+v, %v", list, err)
	}
	shell := &fakeGitShell{res: &agentshell.GitResponse{
		Repository: "acme/widgets", DurationMs: 1500,
		Updated: []agentshell.GitRefUpdate{{Ref: "refs/remotes/origin/main", New: "1111111aaaa"}},
	}}
	tools := mcp.IntegrationTools(dana, shell, list.Tools, nil)
	clone := tools[0]
	args := json.RawMessage(`{"url":"https://github.com/acme/widgets","path":"/files/widgets"}`)
	res, err := clone.Handler(ctx, args)
	if err != nil || res.IsError || !strings.Contains(res.Content[0], "Cloned acme/widgets into /files/widgets.") {
		t.Fatalf("clone = %+v, %v", res, err)
	}
	proxy, registered, secret, connID := gitProxy(t, srv)
	if len(shell.jobs) != 1 {
		t.Fatalf("jobs = %d", len(shell.jobs))
	}
	j := shell.jobs[0]
	if j.Op != "clone" || j.URL != "https://github.com/acme/widgets.git" || j.Host != "github.com" || j.Path != "/files/widgets" ||
		j.Auth != (agentshell.GitAuth{Username: "x-access-token", Password: gitToken}) ||
		j.Proxy != (agentshell.GitProxy{Username: builtinintegrations.ProxyUser("git", connID), Password: secret}) ||
		!slices.Equal(j.AllowedRepositories, []string{"acme/*"}) {
		t.Errorf("job = %+v", j)
	}
	if secret == "" || !registered || proxy.Hash != egresscred.Hash(secret) || !slices.Equal(proxy.Hosts, []string{"github.com"}) {
		t.Errorf("proxy credential = %+v registered %v", proxy, registered)
	}
	rows := gitRows(t, srv)
	if len(rows) != 2 || rows[0].Agent != "dana" || rows[0].Action != "clone" || !rows[0].Allowed ||
		rows[0].Reason != "acme/widgets: updated refs/remotes/origin/main" || rows[0].DurationMS < 1500 ||
		rows[1].Action != "clone" || !rows[1].Allowed || rows[1].Reason != "started: acme/widgets" {
		t.Errorf("audit = %+v", rows)
	}

	// The first request alone: the job, no answer for the model yet,
	// and the row that says it started, so a second request that never
	// comes still leaves the owner a row.
	first, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args})
	if err != nil || first.ShellJob == nil || first.ShellJob.Git == nil || len(first.Content) != 0 || first.IsError {
		t.Fatalf("first request = %+v, %v", first, err)
	}
	if rows := gitRows(t, srv); len(rows) != 3 || rows[0].Reason != "started: acme/widgets" {
		t.Errorf("after the first request alone: %+v", rows)
	}
	// The shell's words quoting the credentials are scrubbed for the
	// model and the audit.
	leak := &agentpod.IntegrationShellJobResult{Git: &agentpod.GitResult{Error: "denied " + gitToken + " via " + secret}}
	second, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args, ShellJobResult: leak})
	if err != nil || !second.IsError || strings.Contains(second.Content[0], gitToken) || strings.Contains(second.Content[0], secret) ||
		!strings.Contains(second.Content[0], integrationSecretPlaceholder) {
		t.Errorf("second request = %+v, %v", second, err)
	}
	rows = gitRows(t, srv)
	if last := rows[0]; len(rows) != 4 || strings.Contains(last.Reason, gitToken) || strings.Contains(last.Reason, secret) {
		t.Errorf("audit = %+v", rows)
	}

	// Access taken away while the job ran: the second request refuses.
	if rr := intDelete(t, srv, intBase+"/git/access/dana"); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	done := &agentpod.IntegrationShellJobResult{Git: &agentpod.GitResult{Repository: "acme/widgets"}}
	second, _ = dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: args, ShellJobResult: done})
	if !second.IsError || second.Content[0] != "You have no access to the Git integration. Ask the owner if your work needs it." {
		t.Errorf("after access went = %+v", second)
	}
	rows = gitRows(t, srv)
	if last := rows[0]; last.Allowed {
		t.Errorf("audit = %+v", last)
	}
}

// Why this test exists: a call core refuses must be answered at once,
// before any job carries the token to the agent's pod, and audited as
// not allowed with the cause; an agent whose MCP server has no shell is
// told it has none to run git in, and the owner's audit says nothing
// ran.
func TestGitCallRefusalsAndNoShell(t *testing.T) {
	srv := newTestServer(t)
	withGit(t, srv)
	intTeam(t, srv, "dana")
	connectGit(t, srv, "github.com")
	putIntegrationAccess(t, srv, "git", "dana", `{"repositories":["acme/widgets"]}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()
	list, _ := dana.IntegrationTools(ctx)
	shell := &fakeGitShell{res: &agentshell.GitResponse{}}
	tools := mcp.IntegrationTools(dana, shell, list.Tools, nil)

	res, _ := tools[0].Handler(ctx, json.RawMessage(`{"url":"acme/gadgets","path":"g"}`))
	if !res.IsError || res.Content[0] != "The Git integration's access for you does not include acme/gadgets. Ask the owner if your work needs it." {
		t.Errorf("outside the access = %+v", res)
	}
	res, _ = tools[0].Handler(ctx, json.RawMessage(`{"url":"https://gitlab.com/acme/widgets","path":"g"}`))
	if !res.IsError || !strings.Contains(res.Content[0], "connection is for github.com") {
		t.Errorf("another host = %+v", res)
	}
	if len(shell.jobs) != 0 {
		t.Errorf("a refused call ran a job: %+v", shell.jobs)
	}
	rows := gitRows(t, srv)
	if len(rows) != 2 || rows[1].Allowed || rows[1].Reason != "the access does not include acme/gadgets" || rows[0].Allowed {
		t.Errorf("audit = %+v", rows)
	}

	noShell := mcp.IntegrationTools(dana, nil, list.Tools, nil)
	res, _ = noShell[0].Handler(ctx, json.RawMessage(`{"url":"acme/widgets","path":"w"}`))
	if !res.IsError || res.Content[0] != "You have no shell to run git in, so nothing was cloned." {
		t.Errorf("no shell = %+v", res)
	}
	rows = gitRows(t, srv)
	if last := rows[0]; !last.Allowed || !strings.HasPrefix(last.Reason, "acme/widgets: nothing ran") {
		t.Errorf("audit = %+v", last)
	}
}

// Why this test exists: a shell job's result is accepted only for a
// call whose integration asks for jobs. One sent with a call of an
// integration that never asks (a built-in without jobs, as a bundle
// is) must be refused and audited, not read as that call's answer.
func TestShellJobResultForACallThatAskedForNone(t *testing.T) {
	srv := newTestServer(t)
	withEcho(t, srv)
	intTeam(t, srv, "dana")
	connectEcho(t, srv, "Hello")
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	resp := srv.callAgentIntegrationJob(context.Background(), "dana", "echo_echo", json.RawMessage(`{}`),
		&agentpod.IntegrationShellJobResult{Git: &agentpod.GitResult{Repository: "acme/widgets"}})
	if !resp.IsError || !strings.Contains(resp.Content[0], "asked your shell for nothing") {
		t.Errorf("answer = %+v", resp)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("echo", 10)
	if len(rows) != 1 || rows[0].Allowed || rows[0].Reason != reasonNoShellJob {
		t.Errorf("audit = %+v", rows)
	}
}

// Why this test exists: the Git connection's proxy credential is what
// lets a clone through the egress proxy to the connection's host, and
// only there. It must be minted with the first save and kept in the
// connection's Secret (never among the fields the owner is shown or
// the built-in is given), stay the same across saves, follow the host
// when it changes, go when the connection is deleted or the
// integration switched off, come back when it is switched on, and be
// registered again at boot from the Secret, which the boot reconcile
// of integration pods must leave alone.
func TestGitConnectionProxyCredential(t *testing.T) {
	srv := newTestServer(t)
	withGit(t, srv)
	ctx := context.Background()

	page := connectGit(t, srv, "github.com")
	first, ok, secret, connID := gitProxy(t, srv)
	if !ok || secret == "" || first.Hash != egresscred.Hash(secret) || !slices.Equal(first.Hosts, []string{"github.com"}) {
		t.Fatalf("after the first save: %+v %v", first, ok)
	}
	for _, f := range page.ConnectionFields {
		if f.Key == builtinintegrations.ProxySecretKey {
			t.Errorf("the owner is shown the proxy credential's key: %+v", f)
		}
	}
	if rec, _ := srv.Store.GetIntegrationConnection("git"); !slices.Equal(rec.SecretKeys, []string{"token"}) {
		t.Errorf("secret keys = %v", rec.SecretKeys)
	}
	if conn, _, _, err := srv.builtinConnection(ctx, "git"); err != nil || conn.Fields[builtinintegrations.ProxySecretKey] != "" || conn.Fields["token"] != gitToken {
		t.Errorf("the built-in is given %+v, %v", conn.Fields, err)
	}

	connectGit(t, srv, "github.com")
	if again, _, s, _ := gitProxy(t, srv); s != secret || again.Hash != first.Hash {
		t.Error("a second save changed the credential")
	}
	connectGit(t, srv, "https://gitlab.com/")
	if moved, _, _, _ := gitProxy(t, srv); moved.Hash != first.Hash || !slices.Equal(moved.Hosts, []string{"gitlab.com"}) {
		t.Errorf("after the host changed: %+v", moved)
	}

	intPost(t, srv, intBase+"/git/disable")
	if _, ok, _, _ := gitProxy(t, srv); ok {
		t.Error("registered while switched off")
	}
	intPost(t, srv, intBase+"/git/enable")
	if back, ok, _, _ := gitProxy(t, srv); !ok || back.Hash != first.Hash {
		t.Error("not registered again when switched on")
	}

	// Boot: the proxy's file holds nothing (as after a restore); the
	// Secret does.
	if err := srv.Store.DropEgressScopedCredential(builtinintegrations.ProxyUser("git", connID)); err != nil {
		t.Fatal(err)
	}
	srv.RegisterBuiltinEgress(ctx)
	if booted, ok, _, _ := gitProxy(t, srv); !ok || booted.Hash != first.Hash {
		t.Error("not registered again at boot")
	}
	if err := srv.Integrations.reconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _, _ := gitProxy(t, srv); !ok {
		t.Error("the boot reconcile dropped the connection's credential")
	}
	// Switched off across a restart: boot drops it.
	if err := srv.Store.SetIntegrationEnabled("git", false); err != nil {
		t.Fatal(err)
	}
	srv.RegisterBuiltinEgress(ctx)
	if _, ok, _, _ := gitProxy(t, srv); ok {
		t.Error("registered at boot while switched off")
	}
	intPost(t, srv, intBase+"/git/enable")

	if rr := intDelete(t, srv, intBase+"/git/connection"); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if _, ok := srv.Store.EgressScopedCredentials()[builtinintegrations.ProxyUser("git", connID)]; ok {
		t.Error("the deleted connection's credential is registered")
	}
}

// Why this test exists: a built-in that holds its connection and its
// access to more than the forms do must refuse what it cannot use when
// the owner saves it, as a 400 in words for the owner, and store
// nothing: a host with a path saved would register a proxy credential
// that reaches nothing and refuse every agent's call later, and an
// access entry that is no pattern would grant nothing without saying
// so. What it can use must save as before.
func TestGitConnectionAndAccessChecksAtSave(t *testing.T) {
	srv := newTestServer(t)
	withGit(t, srv)
	intTeam(t, srv, "dana")

	rr := orgJSON(t, srv, http.MethodPut, intBase+"/git/connection",
		apitypes.ConnectionWrite{Fields: map[string]string{"host": "github.com/acme", "token": gitToken}})
	if e := assertAPIError(t, rr, http.StatusBadRequest); e.Error != "Host is a host name such as github.com, without https:// or a path" {
		t.Errorf("a host with a path = %+v", e)
	}
	if _, err := srv.Store.GetIntegrationConnection("git"); err == nil {
		t.Error("the refused connection was stored")
	}
	if len(srv.Store.EgressScopedCredentials()) != 0 {
		t.Error("the refused connection registered a proxy credential")
	}

	connectGit(t, srv, "https://github.com/")
	rr = orgJSON(t, srv, http.MethodPut, intBase+"/git/access/dana", apitypes.AccessWrite{Access: []byte(`{"repositories":["acme/*","acme/[x"]}`)})
	if e := assertAPIError(t, rr, http.StatusBadRequest); e.Error != "acme/[x is not a repository or a pattern" {
		t.Errorf("a bad pattern = %+v", e)
	}
	if rows, _ := srv.Store.ListIntegrationAccess("git"); len(rows) != 0 {
		t.Errorf("the refused access was stored: %+v", rows)
	}
	putIntegrationAccess(t, srv, "git", "dana", `{"repositories":["acme/*","acme/widgets"]}`)
	if rows, _ := srv.Store.ListIntegrationAccess("git"); len(rows) != 1 || rows[0].Invalid != "" {
		t.Errorf("good access = %+v", rows)
	}
}
