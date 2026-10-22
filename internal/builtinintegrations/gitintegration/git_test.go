package gitintegration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

const testToken = "ghp_git-integration-test-token"

func testConn(host string) builtinintegrations.Connection {
	return builtinintegrations.Connection{ID: "1a2b3c4d", Fields: map[string]string{"host": host, "token": testToken}}
}

func dana(patterns ...any) builtinintegrations.Caller {
	return builtinintegrations.Caller{Agent: "dana", Access: map[string]any{"repositories": patterns}}
}

// Why this test exists: the manifest is what the Org page draws the
// connection and access forms from and what boot writes to every team.
// It must pass the built-in check, ask for the host, the token (a
// secret) and the test repository in that order, take repositories as
// its one required access field, and list exactly clone and fetch, the
// two actions Call and Tools know.
func TestManifest(t *testing.T) {
	m := New().Manifest()
	if f := integration.ValidateBuiltin(m); len(f) != 0 {
		t.Fatalf("findings: %v", f.Texts())
	}
	var keys []string
	for _, f := range m.UserConfig {
		keys = append(keys, f.Key)
	}
	if strings.Join(keys, ",") != "host,token,test_repository" || !m.UserConfig[1].Sensitive || !m.UserConfig[0].Required || m.UserConfig[2].Required {
		t.Errorf("user_config = %+v", m.UserConfig)
	}
	form := integration.AccessForm(m)
	if form == nil || len(form.Fields) != 1 || form.Fields[0].Key != "repositories" || !form.Fields[0].Required || form.Fields[0].Kind != integration.FieldList {
		t.Errorf("access form = %+v", form)
	}
	tools, _ := New().Tools(context.Background(), builtinintegrations.Caller{})
	if len(tools) != 2 || tools[0].Name != "clone" || tools[1].Name != "fetch" || len(m.Tools) != 2 {
		t.Errorf("tools = %+v, manifest %+v", tools, m.Tools)
	}
	for _, tl := range tools {
		if strings.Contains(tl.Description, "MCP") || !json.Valid(tl.InputSchema) {
			t.Errorf("%s: %q %s", tl.Name, tl.Description, tl.InputSchema)
		}
	}
	if h := New().Hosts(testConn("https://GitHub.com/")); len(h) != 1 || h[0] != "github.com" {
		t.Errorf("hosts = %v", h)
	}
	if h := New().Hosts(testConn("git.acme.io:8443")); len(h) != 1 || h[0] != "git.acme.io" {
		t.Errorf("hosts with a port = %v", h)
	}
	if h := New().Hosts(testConn("not a host")); h != nil {
		t.Errorf("hosts of no host = %v", h)
	}
}

// Why this test exists: the job core hands the agent's shell carries
// the owner's token, so everything in it is core's, not the model's: a
// URL core built from the connection's host and the repository's
// owner/name (case kept, no user info), the access's patterns for the
// daemon's own check, the bound, the token under the user GitHub
// expects, and no proxy credential (core adds the connection's).
func TestCallBuildsTheJob(t *testing.T) {
	g := New()
	res, err := g.Call(context.Background(), dana("acme/*"), testConn("github.com"), "clone",
		json.RawMessage(`{"url":"https://github.com/Acme/Widgets","path":"/files/widgets"}`))
	if err != nil || res.ShellJob == nil || res.ShellJob.Git == nil {
		t.Fatalf("clone = %+v, %v", res, err)
	}
	want := shelljob.GitJob{
		Op: "clone", Path: "/files/widgets", URL: "https://github.com/Acme/Widgets.git", Host: "github.com",
		AllowedRepositories: []string{"acme/*"}, Auth: shelljob.GitAuth{Username: "x-access-token", Password: testToken},
		TimeoutSeconds: 900,
	}
	got, _ := json.Marshal(res.ShellJob.Git)
	if w, _ := json.Marshal(want); string(got) != string(w) {
		t.Errorf("clone job = %s, want %s", got, w)
	}

	res, err = g.Call(context.Background(), dana("acme/widgets"), testConn("github.com"), "fetch",
		json.RawMessage(`{"path":"~/w","refs":["+refs/heads/main:refs/remotes/origin/main"]}`))
	if err != nil || res.ShellJob == nil {
		t.Fatalf("fetch = %+v, %v", res, err)
	}
	if j := res.ShellJob.Git; j.Op != "fetch" || j.URL != "" || j.Path != "~/w" || j.TimeoutSeconds != 600 ||
		len(j.RefSpecs) != 1 || len(j.AllowedRepositories) != 1 || j.AllowedRepositories[0] != "acme/widgets" {
		t.Errorf("fetch job = %+v", j)
	}
	res, err = g.Call(context.Background(), dana("acme/widgets"), testConn("github.com"), "fetch", json.RawMessage(`{"path":"w","url":"acme/widgets"}`))
	if err != nil || res.ShellJob.Git.URL != "https://github.com/acme/widgets.git" {
		t.Errorf("fetch with url = %+v, %v", res, err)
	}
}

// Why this test exists: every way a call can be wrong must be refused
// in core, before any job carries the token out: a URL on another host
// (even one that only differs by a port), with user info or a query,
// SSH, a repository the access does not include, an access that names
// none, a connection without a usable host or token, arguments the
// action does not take. Each is a Refusal, so core audits it as not
// allowed, and its words name the cause and the owner only.
func TestCallRefusals(t *testing.T) {
	g := New()
	clone := func(args string) json.RawMessage { return json.RawMessage(args) }
	for name, c := range map[string]struct {
		caller builtinintegrations.Caller
		conn   builtinintegrations.Connection
		action string
		args   json.RawMessage
		words  string
	}{
		"another host":      {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"https://gitlab.com/acme/widgets","path":"w"}`), "is for github.com"},
		"another port":      {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"https://github.com:8443/acme/widgets","path":"w"}`), "is for github.com"},
		"user info":         {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"https://me:pw@github.com/acme/widgets","path":"w"}`), "user name or password"},
		"a query":           {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"https://github.com/acme/widgets?x=1","path":"w"}`), "query"},
		"ssh":               {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"git@github.com:acme/widgets.git","path":"w"}`), "neither an HTTPS URL"},
		"plain http":        {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"http://github.com/acme/widgets","path":"w"}`), "HTTPS"},
		"outside":           {dana("acme/widgets"), testConn("github.com"), "clone", clone(`{"url":"acme/gadgets","path":"w"}`), "does not include acme/gadgets. Ask the owner"},
		"fetch outside":     {dana("acme/widgets"), testConn("github.com"), "fetch", clone(`{"url":"other/x","path":"w"}`), "does not include other/x"},
		"no repositories":   {builtinintegrations.Caller{Access: map[string]any{}}, testConn("github.com"), "clone", clone(`{"url":"acme/widgets","path":"w"}`), "names no repositories"},
		"no host":           {dana("acme/*"), testConn("https://github.com/acme"), "clone", clone(`{"url":"acme/widgets","path":"w"}`), "no host Kivali can use"},
		"no token":          {dana("acme/*"), builtinintegrations.Connection{Fields: map[string]string{"host": "github.com"}}, "clone", clone(`{"url":"acme/widgets","path":"w"}`), "no token"},
		"no path":           {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"acme/widgets"}`), "needs url and path"},
		"fetch no path":     {dana("acme/*"), testConn("github.com"), "fetch", clone(`{}`), "needs path"},
		"an unknown field":  {dana("acme/*"), testConn("github.com"), "clone", clone(`{"url":"acme/widgets","path":"w","force":true}`), "takes url and path"},
		"refs not strings":  {dana("acme/*"), testConn("github.com"), "fetch", clone(`{"path":"w","refs":[1]}`), "list of strings"},
		"an unknown action": {dana("acme/*"), testConn("github.com"), "push", clone(`{}`), "no action push"},
	} {
		res, err := g.Call(context.Background(), c.caller, c.conn, c.action, c.args)
		var ref *builtinintegrations.Refusal
		if !errors.As(err, &ref) || res != nil {
			t.Errorf("%s: = %+v, %v; want a refusal", name, res, err)
			continue
		}
		if !strings.Contains(ref.Text, c.words) || ref.Reason == "" || strings.Contains(ref.Text, testToken) {
			t.Errorf("%s: refusal %+v, want words %q", name, ref, c.words)
		}
	}
}

// Why this test exists: Complete is what the agent and the owner's
// audit read of a clone or a fetch. A success must say where the
// working copy is and what moved, and the audit must name the
// repository and the refs; a fetch without a url names the repository
// the shell reported; a skipped job, a timeout and the host's error
// are errors in their own words, the error cut short; and an access
// taken away between the two requests refuses the second.
func TestComplete(t *testing.T) {
	g := New()
	ctx := context.Background()
	cloneArgs := json.RawMessage(`{"url":"acme/widgets","path":"/files/widgets"}`)
	ok := shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", Updated: []shelljob.GitRefUpdate{
		{Ref: "refs/heads/main", New: "1111111aaaa"},
		{Ref: "refs/remotes/origin/main", New: "1111111aaaa"},
	}}}
	res, err := g.Complete(ctx, dana("acme/*"), testConn("github.com"), "clone", cloneArgs, ok)
	if err != nil || res.IsError || !strings.Contains(res.Content[0], "Cloned acme/widgets into /files/widgets. Its origin is https://github.com/acme/widgets.git") ||
		!strings.Contains(res.Content[0], "refs/remotes/origin/main new at 1111111") || res.Audit != "acme/widgets: updated refs/heads/main, refs/remotes/origin/main" {
		t.Errorf("clone = %+v, %v", res, err)
	}

	fetchArgs := json.RawMessage(`{"path":"/files/widgets"}`)
	moved := shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", Updated: []shelljob.GitRefUpdate{{Ref: "refs/remotes/origin/main", Old: "1111111aaaa", New: "2222222bbbb"}}}}
	res, _ = g.Complete(ctx, dana("acme/*"), testConn("github.com"), "fetch", fetchArgs, moved)
	if res.IsError || !strings.Contains(res.Content[0], "Fetched acme/widgets into /files/widgets.") || !strings.Contains(res.Content[0], "refs/remotes/origin/main 1111111..2222222") ||
		res.Audit != "acme/widgets: updated refs/remotes/origin/main" {
		t.Errorf("fetch = %+v", res)
	}
	res, _ = g.Complete(ctx, dana("acme/*"), testConn("github.com"), "fetch", fetchArgs, shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets"}})
	if res.IsError || !strings.Contains(res.Content[0], "up to date") || res.Audit != "acme/widgets: up to date" {
		t.Errorf("up to date = %+v", res)
	}

	long := strings.Repeat("x", 5000)
	for name, c := range map[string]struct {
		result shelljob.Result
		words  string
		audit  string
	}{
		"skipped":   {shelljob.Result{Skipped: "the agent has no shell to run git in"}, "You have no shell to run git in, so nothing was cloned.", "acme/widgets: nothing ran: the agent has no shell to run git in"},
		"no result": {shelljob.Result{}, "gave back no result", "acme/widgets: the shell gave back no result"},
		"timed out": {shelljob.Result{Git: &shelljob.GitResult{TimedOut: true, Error: "did not finish within 15m0s"}}, "did not finish in time", "acme/widgets: did not finish in time"},
		"failed":    {shelljob.Result{Git: &shelljob.GitResult{Error: "clone: authentication required"}}, "The clone of acme/widgets did not go through: clone: authentication required", "acme/widgets: clone: authentication required"},
		"long":      {shelljob.Result{Git: &shelljob.GitResult{Error: long}}, "did not go through", ""},
	} {
		res, err := g.Complete(ctx, dana("acme/*"), testConn("github.com"), "clone", cloneArgs, c.result)
		if err != nil || !res.IsError || !strings.Contains(res.Content[0], c.words) || (c.audit != "" && res.Audit != c.audit) {
			t.Errorf("%s = %+v, %v", name, res, err)
		}
		if len(res.Content[0]) > 1200 {
			t.Errorf("%s: the answer is %d bytes", name, len(res.Content[0]))
		}
	}

	var ref *builtinintegrations.Refusal
	if _, err := g.Complete(ctx, dana("other/*"), testConn("github.com"), "clone", cloneArgs, ok); !errors.As(err, &ref) {
		t.Errorf("access taken away = %v", err)
	}
}

// gitCLI is the git binary; a test that needs it is skipped where
// there is none.
func gitCLI(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	return p
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitCLI(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Why this test exists: Test connection with a repository named lists
// it from core with the token, over the smart HTTP protocol, and the
// owner reads the branch count; a token the host refuses must fail
// with the host's words, never quoting the token back.
func TestVerifyListsTheRepository(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "f")
	runGit(t, work, "commit", "-q", "-m", "one")
	runGit(t, work, "branch", "dev")
	bare := filepath.Join(root, "acme", "widgets.git")
	runGit(t, "", "init", "-q", "--bare", "-b", "main", bare)
	runGit(t, work, "push", "-q", bare, "main", "dev")
	backend := cgi.Handler{Path: gitCLI(t), Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "REMOTE_USER=x"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "x-access-token" || p != testToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "bad credentials "+p, http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	g := &Git{InsecureHTTP: true}
	conn := testConn(u.Host)
	conn.Fields["test_repository"] = "acme/widgets"
	msg, err := g.Verify(context.Background(), conn)
	if err != nil || msg != "Connected. 2 branches in acme/widgets." {
		t.Fatalf("verify = %q, %v", msg, err)
	}
	conn.Fields["token"] = testToken + "-wrong"
	_, err = g.Verify(context.Background(), conn)
	if err == nil || !strings.Contains(err.Error(), "did not list acme/widgets") || strings.Contains(err.Error(), testToken) {
		t.Errorf("a wrong token = %v", err)
	}
}

// Why this test exists: Test connection without a repository asks the
// host's own account endpoint who the token is: GitHub with a bearer
// token, GitLab with PRIVATE-TOKEN. The owner reads the account's name,
// or the host's words on a refusal with the token taken out; any other
// host has no such endpoint, and the owner is told to name a
// repository.
func TestVerifyAccountEndpoints(t *testing.T) {
	var seen http.Header
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		switch {
		case r.URL.Path == "/gh/user" && r.Header.Get("Authorization") == "Bearer "+testToken:
			_, _ = w.Write([]byte(`{"login":"octo"}`))
		case r.URL.Path == "/gl/user" && r.Header.Get("PRIVATE-TOKEN") == testToken:
			_, _ = w.Write([]byte(`{"username":"tanuki"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials for ` + r.Header.Get("Authorization") + r.Header.Get("PRIVATE-TOKEN") + `"}`))
		}
	}))
	defer api.Close()
	g := &Git{HTTPClient: api.Client(), GitHubAPI: api.URL + "/gh", GitLabAPI: api.URL + "/gl"}
	if msg, err := g.Verify(context.Background(), testConn("github.com")); err != nil || msg != "Connected to github.com as octo." {
		t.Errorf("github = %q, %v (headers %v)", msg, err, seen)
	}
	if msg, err := g.Verify(context.Background(), testConn("gitlab.com")); err != nil || msg != "Connected to gitlab.com as tanuki." {
		t.Errorf("gitlab = %q, %v", msg, err)
	}
	wrong := testConn("github.com")
	wrong.Fields["token"] = testToken + "-wrong"
	_, err := g.Verify(context.Background(), wrong)
	if err == nil || !strings.Contains(err.Error(), "Bad credentials") || strings.Contains(err.Error(), testToken) {
		t.Errorf("a refused token = %v", err)
	}
	if _, err := g.Verify(context.Background(), testConn("git.acme.io")); err == nil ||
		err.Error() != "Name a repository for Test connection: this host has no account endpoint Kivali knows." {
		t.Errorf("another host = %v", err)
	}
}
