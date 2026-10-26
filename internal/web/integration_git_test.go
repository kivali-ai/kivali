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
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// gitToken is the Git connection's token in these tests.
const gitToken = "ghp_web-git-test-token-0123"

// withGit gives srv the Git (Basic) built-in, installed as boot installs it,
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

// gitRows is the Git (Basic) integration's audit, newest first.
func gitRows(t *testing.T, srv *Server) []store.IntegrationAuditRow {
	t.Helper()
	rows, err := srv.Store.ReadIntegrationAuditTail("git", 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Why this test exists: a clone through the Git (Basic) integration crosses
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
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()

	// Dana may only pull, so she is not given push.
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
	if !second.IsError || second.Content[0] != "You have no access to the Git (Basic) integration. Ask the owner if your work needs it." {
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
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/widgets"]}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()
	list, _ := dana.IntegrationTools(ctx)
	shell := &fakeGitShell{res: &agentshell.GitResponse{}}
	tools := mcp.IntegrationTools(dana, shell, list.Tools, nil)

	res, _ := tools[0].Handler(ctx, json.RawMessage(`{"url":"acme/gadgets","path":"g"}`))
	if !res.IsError || res.Content[0] != "The Git (Basic) integration's access for you does not include acme/gadgets. Ask the owner if your work needs it." {
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
	if conn, _, _, err := srv.builtinConnection(ctx, "git", ""); err != nil || conn.Fields[builtinintegrations.ProxySecretKey] != "" || conn.Fields["token"] != gitToken {
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
// access entry that is no pattern, or an access that names no
// repository to pull or push to (the form requires neither list),
// would grant nothing without saying so. What it can use must save as
// before.
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
	rr = orgJSON(t, srv, http.MethodPut, intBase+"/git/access/dana", apitypes.AccessWrite{Access: []byte(`{"push_repositories":["acme/*","acme/[x"]}`)})
	if e := assertAPIError(t, rr, http.StatusBadRequest); e.Error != "acme/[x is not a repository or a pattern" {
		t.Errorf("a bad pattern = %+v", e)
	}
	rr = orgJSON(t, srv, http.MethodPut, intBase+"/git/access/dana", apitypes.AccessWrite{Access: []byte(`{"pull_repositories":[],"push_refs":["agent/*"]}`)})
	if e := assertAPIError(t, rr, http.StatusBadRequest); e.Error != "Name a repository it may pull or push to" {
		t.Errorf("no repository = %+v", e)
	}
	if rows, _ := srv.Store.ListIntegrationAccess("git"); len(rows) != 0 {
		t.Errorf("the refused access was stored: %+v", rows)
	}
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"],"push_repositories":["acme/widgets"]}`)
	if rows, _ := srv.Store.ListIntegrationAccess("git"); len(rows) != 1 || rows[0].Invalid != "" {
		t.Errorf("good access = %+v", rows)
	}
}

// Why this test exists: a push crosses core.sock twice like a clone,
// and is the one Git call that writes to the owner's repository. Its
// job must carry the refs core normalised, the access's push_refs and
// only the repositories the agent may push to (not the ones it may
// only pull) for the daemon's own check, with no force the call did
// not ask for; the call must leave one audit row whose action is push,
// naming the repository and the refs; a push outside push_refs, or to
// a repository the agent may only pull, must be refused and audited
// before any job runs; and the Org page must read the access as one
// line, with the force switch marked dangerous on the wire.
func TestGitPushRelaysTheShellJob(t *testing.T) {
	srv := newTestServer(t)
	withGit(t, srv)
	intTeam(t, srv, "dana")
	connectGit(t, srv, "github.com")
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"],"push_repositories":["acme/widgets"],"push_refs":["agent/*"],"force":true}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()
	list, err := dana.IntegrationTools(ctx)
	if err != nil || len(list.Tools) != 3 || list.Tools[2].Name != "git_push" {
		t.Fatalf("dana's actions = %+v, %v", list, err)
	}
	shell := &fakeGitShell{res: &agentshell.GitResponse{
		Repository: "acme/widgets", DurationMs: 900,
		Updated: []agentshell.GitRefUpdate{{Ref: "refs/heads/agent/x", New: "1111111aaaa"}},
	}}
	push := mcp.IntegrationTools(dana, shell, list.Tools, nil)[2]
	res, err := push.Handler(ctx, json.RawMessage(`{"path":"/files/widgets","refs":["agent/x"]}`))
	if err != nil || res.IsError || !strings.HasPrefix(res.Content[0], "Pushed refs/heads/agent/x (new) to acme/widgets.") {
		t.Fatalf("push = %+v, %v", res, err)
	}
	if len(shell.jobs) != 1 {
		t.Fatalf("jobs = %d", len(shell.jobs))
	}
	if j := shell.jobs[0]; j.Op != "push" || j.URL != "" || j.Force || j.TimeoutSeconds != 600 ||
		!slices.Equal(j.RefSpecs, []string{"refs/heads/agent/x:refs/heads/agent/x"}) ||
		!slices.Equal(j.AllowedRefs, []string{"refs/heads/agent/*"}) || !slices.Equal(j.AllowedRepositories, []string{"acme/widgets"}) {
		t.Errorf("job = %+v", j)
	}
	// Two rows: the start, naming what core read the push to write, and
	// the end, backdated to the job's start.
	rows := gitRows(t, srv)
	if len(rows) != 2 || rows[0].Action != "push" || !rows[0].Allowed || rows[0].Reason != "acme/widgets: pushed refs/heads/agent/x" ||
		rows[1].Reason != "started: the working copy's origin at /files/widgets, push of refs/heads/agent/x" {
		t.Errorf("audit = %+v", rows)
	}

	res, _ = push.Handler(ctx, json.RawMessage(`{"path":"/files/widgets","refs":["HEAD:main"]}`))
	if !res.IsError || res.Content[0] != "The Git (Basic) integration's access for you does not include refs/heads/main. Ask the owner if your work needs it." {
		t.Errorf("outside push_refs = %+v", res)
	}
	if len(shell.jobs) != 1 {
		t.Error("a refused push ran a job")
	}
	if rows := gitRows(t, srv); len(rows) != 3 || rows[0].Action != "push" || rows[0].Allowed || rows[0].Reason != "the access does not include refs/heads/main" {
		t.Errorf("audit = %+v", rows)
	}
	res, _ = push.Handler(ctx, json.RawMessage(`{"path":"/files/gadgets","url":"acme/gadgets","refs":["agent/x"]}`))
	if !res.IsError || res.Content[0] != "The Git (Basic) integration's access for you lets you pull acme/gadgets but not push to it. Ask the owner if your work needs it." {
		t.Errorf("a repository it may only pull = %+v", res)
	}
	if len(shell.jobs) != 1 {
		t.Error("a refused push ran a job")
	}
	if rows := gitRows(t, srv); len(rows) != 4 || rows[0].Allowed || rows[0].Reason != "the access does not allow pushing to acme/gadgets" {
		t.Errorf("audit = %+v", rows)
	}

	// push_refs the daemon could not read are refused at save, naming
	// the entry, and nothing is stored.
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/git/access/dana",
		apitypes.AccessWrite{Access: []byte(`{"push_repositories":["acme/*"],"push_refs":["agent/*","agent[^x]*"]}`)})
	if e := assertAPIError(t, rr, http.StatusBadRequest); e.Error != "agent[^x]* is not a branch, a tag or a pattern" {
		t.Errorf("bad push_refs = %+v", e)
	}

	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/git"))
	if len(page.Access) != 1 || page.Access[0].Summary != "Pull: acme/* · Push: acme/widgets · Branches and tags: agent/* · May force-push" {
		t.Errorf("access rows = %+v", page.Access)
	}
	if f := page.AccessForm; f == nil || len(f.Fields) != 4 || f.Fields[3].Key != "force" || !f.Fields[3].Danger || f.Fields[1].Danger {
		t.Errorf("access form = %+v", f)
	}
}

// Why this test exists: Git 2.0.0 and later grant push per repository,
// where 1.1.0 had one list of repositories and a push switch over all
// of it. A team that set an agent's access under 1.1.0 boots a binary
// carrying a later version: the store supersedes 1.1.0, and that
// access must turn invalid and grant nothing (no clone, and above all
// no push to every repository in the old list) until the owner sets it
// again, after which the agent works as the new access says.
func TestGitAccessFrom1xIsSetAgain(t *testing.T) {
	srv := newTestServer(t)
	srv.BuiltinIntegrations = builtinintegrations.Registry{gitintegration.New()}
	withIntegrationRegistry(srv, nil)
	old := *gitintegration.New().Manifest()
	old.Version = "1.1.0"
	old.Kivali.Access = &integration.Access{Schema: json.RawMessage(`{"type":"object","properties":{` +
		`"repositories":{"type":"array","items":{"type":"string"},"title":"Repositories"},` +
		`"push":{"type":"boolean","title":"May push"},` +
		`"push_refs":{"type":"array","items":{"type":"string"},"title":"Branches and tags it may push"},` +
		`"force":{"type":"boolean","title":"May force-push","x-kivali-danger":true}},` +
		`"required":["repositories"]}`)}
	boot := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	if err := srv.Store.InstallBuiltinIntegrations([]*integration.Manifest{&old}, boot); err != nil {
		t.Fatal(err)
	}
	intTeam(t, srv, "dana")
	connectGit(t, srv, "github.com")
	// Written as 1.1.0's binary saved it: this binary's check refuses
	// the old fields at save.
	if err := srv.Store.SetIntegrationAccess("git", store.IntegrationAccess{
		Slug: "dana", SetAt: boot, SetBy: "ceo",
		Access: map[string]any{"repositories": []any{"acme/*"}, "push": true, "push_refs": []any{"agent/*"}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), boot.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	vs, _ := srv.Store.IntegrationVersions("git")
	if len(vs) != 2 || vs[0].Version != gitintegration.New().Manifest().Version || vs[0].State != store.IntegrationVersionApproved || vs[1].State != store.IntegrationVersionSuperseded {
		t.Fatalf("versions = %+v", vs)
	}
	rows, err := srv.Store.ListIntegrationAccess("git")
	if err != nil || len(rows) != 1 || rows[0].Invalid == "" {
		t.Fatalf("access after the upgrade = %+v, %v", rows, err)
	}
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	ctx := context.Background()
	for name, args := range map[string]string{"git_clone": `{"url":"acme/widgets","path":"w"}`, "git_push": `{"path":"w","refs":["agent/x"]}`} {
		res, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: name, Arguments: json.RawMessage(args)})
		if err != nil || res.ShellJob != nil || !res.IsError || !strings.Contains(res.Content[0], "no longer applies") {
			t.Errorf("%s under the old access = %+v, %v", name, res, err)
		}
	}

	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)
	res, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: json.RawMessage(`{"url":"acme/widgets","path":"w"}`)})
	if err != nil || res.ShellJob == nil {
		t.Errorf("clone once set again = %+v, %v", res, err)
	}
}

// Why this test exists: 2.1.0 renames Git to Git (Basic) and keeps its
// access form. A team on 2.0.0 that boots this binary must read the new
// name on the integration's record (the list, the page and every
// refusal name it from there) while its agents' access stays valid and
// keeps working: a version bump read as a changed form would take every
// agent's Git access away until the owner set each again.
func TestGitAccessFrom2xKeepsWorking(t *testing.T) {
	srv := newTestServer(t)
	srv.BuiltinIntegrations = builtinintegrations.Registry{gitintegration.New()}
	withIntegrationRegistry(srv, nil)
	old := *gitintegration.New().Manifest()
	old.Version, old.DisplayName = "2.0.0", "Git"
	boot := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	if err := srv.Store.InstallBuiltinIntegrations([]*integration.Manifest{&old}, boot); err != nil {
		t.Fatal(err)
	}
	intTeam(t, srv, "dana")
	connectGit(t, srv, "github.com")
	putIntegrationAccess(t, srv, "git", "dana", `{"pull_repositories":["acme/*"]}`)

	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), boot.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	vs, _ := srv.Store.IntegrationVersions("git")
	if len(vs) != 2 || vs[0].Version != "2.1.0" || vs[0].State != store.IntegrationVersionApproved || vs[1].Version != "2.0.0" || vs[1].State != store.IntegrationVersionSuperseded {
		t.Fatalf("versions = %+v", vs)
	}
	if it, _ := srv.Store.GetIntegration("git"); it.DisplayName != "Git (Basic)" {
		t.Errorf("display name = %q", it.DisplayName)
	}
	if rows, err := srv.Store.ListIntegrationAccess("git"); err != nil || len(rows) != 1 || rows[0].Invalid != "" {
		t.Fatalf("access after the upgrade = %+v, %v", rows, err)
	}
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")
	res, err := dana.IntegrationCall(context.Background(), agentpod.IntegrationCallRequest{Name: "git_clone", Arguments: json.RawMessage(`{"url":"acme/gadgets","path":"w"}`)})
	if err != nil || res.ShellJob == nil {
		t.Errorf("clone after the upgrade = %+v, %v", res, err)
	}
}

// Why this test exists: Git 1.1.0 had a Test connection and a
// "Repository for Test connection" field; later versions have neither.
// A team that saved a connection with that field and tested it boots a
// binary without them. Its page must not draw the field, offer Test
// connection or show the old result, and saving the connection again
// must succeed and drop the field's value rather than refuse a key the
// form no longer has.
func TestGitConnectionFromBeforeTheTestWasDropped(t *testing.T) {
	srv := newTestServer(t)
	srv.BuiltinIntegrations = builtinintegrations.Registry{gitintegration.New()}
	withIntegrationRegistry(srv, nil)
	old := *gitintegration.New().Manifest()
	old.Version = "1.1.0"
	old.UserConfig = append(slices.Clone(old.UserConfig), integration.UserConfigField{Key: "test_repository", Type: "string", Title: "Repository for Test connection"})
	old.Kivali.Verify = "ls_remote"
	boot := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	if err := srv.Store.InstallBuiltinIntegrations([]*integration.Manifest{&old}, boot); err != nil {
		t.Fatal(err)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/git/connection",
		apitypes.ConnectionWrite{Fields: map[string]string{"host": "github.com", "token": gitToken, "test_repository": "acme/widgets"}}))
	conn, err := srv.Store.GetIntegrationConnectionByID("git", page.Connection.ID)
	if err != nil || conn.Fields["test_repository"] != "acme/widgets" {
		t.Fatalf("connection under 1.1.0 = %+v, %v", conn, err)
	}
	verified := true
	conn.VerifiedAt, conn.Verified, conn.VerifyMessage = &boot, &verified, "Connected to github.com as octo."
	if err := srv.Store.PutIntegrationConnection("git", conn); err != nil {
		t.Fatal(err)
	}

	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), boot.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/git"))
	if page.ConnectionTest {
		t.Error("connection_test after the upgrade")
	}
	var keys []string
	for _, f := range page.Connection.Fields {
		keys = append(keys, f.Key)
	}
	if strings.Join(keys, ",") != "host,token" {
		t.Errorf("fields after the upgrade = %v", keys)
	}
	if c := page.Connection; c.Verified != nil || c.VerifiedAt != nil || c.VerifyMessage != nil {
		t.Errorf("the old result reached the page: %+v", c)
	}
	orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/git/connection",
		apitypes.ConnectionWrite{Fields: map[string]string{"host": "github.com"}}))
	conn, err = srv.Store.GetIntegrationConnectionByID("git", conn.ID)
	if _, kept := conn.Fields["test_repository"]; err != nil || kept {
		t.Errorf("connection after a save = %+v, %v", conn, err)
	}
}
