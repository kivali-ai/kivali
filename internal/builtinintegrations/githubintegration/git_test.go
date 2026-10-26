package githubintegration

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// gitCall is the first request of the clone, fetch or push name: its
// job and the Result, or the refusal.
func (g *GitHub) gitCall(t *testing.T, caller builtinintegrations.Caller, name, args string) (*shelljob.GitJob, *builtinintegrations.Result, *builtinintegrations.Refusal) {
	t.Helper()
	res, err := g.Call(context.Background(), caller, testConn(), name, json.RawMessage(args))
	if ref, ok := err.(*builtinintegrations.Refusal); ok {
		return nil, nil, ref
	}
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.ShellJob == nil || res.ShellJob.Git == nil {
		return nil, res, nil
	}
	return res.ShellJob.Git, res, nil
}

// Why this test exists: a clone is the first time a credential of the
// owner's GitHub App leaves Kivali for an agent's shell. It must be an
// installation token minted for that call, narrowed to the one
// repository and to reading its contents, presented as GitHub takes it
// (x-access-token), and the job must hold the shell to that repository
// alone (the row's patterns, on github.com), leave the proxy for core
// to fill, with a credential admitted to github.com alone (Hosts), and
// grant no push. The token must be in nothing but the job:
// a token in the audit or the answer would reach the owner's log and
// the model. The second request puts the result in words and mints
// nothing.
func TestCloneMintsATokenForTheRepositoryAlone(t *testing.T) {
	g, f := newTest(t)
	dana := agent(false, "acme/*", "tools/cli")
	job, res, ref := g.gitCall(t, dana, "clone", `{"url":"https://github.com/Acme/Widgets","path":"/files/widgets"}`)
	if ref != nil || job == nil {
		t.Fatalf("clone = %+v, %+v", res, ref)
	}
	mints := f.mints()
	if len(mints) != 1 || mints[0].JWT == nil || mints[0].Body != `{"permissions":{"contents":"read","metadata":"read"},"repositories":["widgets"]}` {
		t.Fatalf("mints = %+v", mints)
	}
	tok := "ghs_fakeinstallationtoken0001"
	if job.Op != "clone" || job.URL != "https://github.com/Acme/Widgets.git" || job.Host != "github.com" || job.Path != "/files/widgets" ||
		job.Auth != (shelljob.GitAuth{Username: "x-access-token", Password: tok}) || job.Proxy != (shelljob.GitProxy{}) ||
		!slices.Equal(job.AllowedRepositories, []string{"acme/*", "tools/cli"}) ||
		job.AnyBranchOrTag || job.Force || len(job.AllowedRefs) != 0 || job.ConnectionID != "c0ffee01" {
		t.Errorf("job = %+v", job)
	}
	if res.Audit != "started: Acme/Widgets" || len(res.Content) != 0 || strings.Contains(res.Audit, tok) {
		t.Errorf("result = %+v", res)
	}
	if hosts := g.Hosts(testConn()); !slices.Equal(hosts, []string{"github.com"}) {
		t.Errorf("hosts = %v", hosts)
	}

	done, err := g.Complete(context.Background(), dana, testConn(), "clone", json.RawMessage(`{"url":"https://github.com/Acme/Widgets","path":"/files/widgets"}`),
		shelljob.Result{Git: &shelljob.GitResult{Repository: "Acme/Widgets", ConnectionID: "c0ffee01",
			Updated: []shelljob.GitRefUpdate{{Ref: "refs/remotes/origin/main", New: "1111111aaaa"}}}})
	if err != nil || done.IsError || !strings.HasPrefix(done.Content[0], "Cloned Acme/Widgets into /files/widgets.") ||
		done.Audit != "Acme/Widgets: updated refs/remotes/origin/main" || strings.Contains(done.Content[0]+done.Audit, tok) {
		t.Errorf("complete = %+v, %v", done, err)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("GitHub took %d requests, want the one mint", n)
	}
}

// Why this test exists: pushing is what the write switch adds, and a
// push's token can write the repository. Without write a push must be
// refused by name before any token is asked for; with it, the token
// may write contents, the job may write any branch or tag and the
// repositories it reaches are the row's. A forced push (force, or a +)
// and a deletion are things GitHub never does here: each refused in
// words that name no other way, and with no token minted.
func TestPushNeedsWriteAndNeverForcesOrDeletes(t *testing.T) {
	g, f := newTest(t)
	args := `{"path":"w","url":"acme/widgets","refs":["agent/x","tags/v1"]}`
	for _, access := range []map[string]any{
		{"repositories": []any{"acme/*"}},
		{"repositories": []any{"acme/*"}, "write": false},
		{"repositories": []any{"acme/*"}, "write": "true"},
	} {
		_, _, ref := g.gitCall(t, callerWith(access), "push", args)
		if ref == nil || ref.Text != "Your access to GitHub does not include opening issues and pull requests, commenting or pushing; ask the owner." ||
			ref.Reason != "the access does not include opening issues and pull requests, commenting or pushing" {
			t.Errorf("%v: %+v", access, ref)
		}
	}
	writer := agent(true, "acme/*")
	for _, c := range []struct{ args, text, reason string }{
		{`{"path":"w","url":"acme/widgets","refs":["agent/x"],"force":true}`, "Force-pushing is not something the GitHub integration does.", "the call forces a push"},
		{`{"path":"w","url":"acme/widgets","refs":["+agent/x"]}`, "Force-pushing is not something the GitHub integration does.", "the call forces a push"},
		{`{"path":"w","url":"acme/widgets","refs":[":agent/x"]}`, "Deleting a remote ref is not something the GitHub integration does.", "the call deletes a remote ref"},
	} {
		_, _, ref := g.gitCall(t, writer, "push", c.args)
		if ref == nil || ref.Text != c.text || ref.Reason != c.reason {
			t.Errorf("%s: %+v", c.args, ref)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("refused pushes made %d requests", n)
	}

	job, res, ref := g.gitCall(t, writer, "push", args)
	if ref != nil || job == nil {
		t.Fatalf("push = %+v, %+v", res, ref)
	}
	if mints := f.mints(); len(mints) != 1 || mints[0].Body != `{"permissions":{"contents":"write","metadata":"read"},"repositories":["widgets"]}` {
		t.Errorf("mints = %+v", mints)
	}
	if job.Op != "push" || job.URL != "https://github.com/acme/widgets.git" || !job.AnyBranchOrTag || len(job.AllowedRefs) != 0 || job.Force ||
		!slices.Equal(job.AllowedRepositories, []string{"acme/*"}) || !slices.Equal(job.RefSpecs, []string{"refs/heads/agent/x:refs/heads/agent/x", "refs/tags/v1:refs/tags/v1"}) ||
		job.Auth.Username != "x-access-token" || job.Auth.Password == "" {
		t.Errorf("job = %+v", job)
	}
	if res.Audit != "started: acme/widgets, push of refs/heads/agent/x, refs/tags/v1" {
		t.Errorf("audit = %q", res.Audit)
	}
}

// Why this test exists: a job's token is narrowed to the repository
// the call names, so the daemon cannot be left to read the working
// copy's origin: a fetch or a push without a url must be refused, in
// words saying to name the repository, before any token is minted. A
// path GitHub cannot have (owner/group/name) is refused too rather than
// sent to GitHub for a token.
func TestFetchAndPushNameTheRepository(t *testing.T) {
	g, f := newTest(t)
	writer := agent(true, "*/*", "acme/*/*")
	for name, args := range map[string]string{
		"fetch": `{"path":"w"}`,
		"push":  `{"path":"w","refs":["agent/x"]}`,
	} {
		_, _, ref := g.gitCall(t, writer, name, args)
		if ref == nil || ref.Text != "github_"+name+" needs url: name the repository, as its HTTPS URL on github.com or owner/name." || ref.Reason != "the arguments lack url" {
			t.Errorf("%s without a url = %+v", name, ref)
		}
	}
	_, _, ref := g.gitCall(t, writer, "clone", `{"url":"acme/tools/cli","path":"c"}`)
	if ref == nil || ref.Text != "A GitHub repository is owner/name, and acme/tools/cli is not one." || ref.Reason != "the repository is not owner/name" {
		t.Errorf("three segments = %+v", ref)
	}
	_, _, ref = g.gitCall(t, writer, "clone", `{"url":"https://gitlab.com/acme/widgets","path":"c"}`)
	if ref == nil || ref.Text != "The GitHub integration's connection is for github.com; give a repository's URL on it, or owner/name." {
		t.Errorf("another host = %+v", ref)
	}
	_, _, ref = g.gitCall(t, agent(false, "acme/widgets"), "fetch", `{"path":"w","url":"acme/gadgets"}`)
	if ref == nil || ref.Text != "The GitHub integration's access for you does not include acme/gadgets. Ask the owner if your work needs it." {
		t.Errorf("outside the access = %+v", ref)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("refused calls made %d requests", n)
	}
}

// Why this test exists: the REST actions' tokens are cached for the
// hour, but a job's token goes to the agent's shell, so it must be
// minted for that job alone. Handed one from the cache, two jobs (or a
// job and a REST call) would share a token; put into the cache, a
// token already in the agent's hands would go on serving Kivali's own
// calls. Two clones of one repository mint two tokens, and a REST call
// of the same repository and permissions after them mints its own.
func TestAJobsTokenIsNeverCached(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/branches", 200, `[]`)
	dana := agent(false, "acme/*")
	clone := `{"url":"acme/widgets","path":"w"}`
	first, _, _ := g.gitCall(t, dana, "clone", clone)
	second, _, _ := g.gitCall(t, dana, "clone", clone)
	if first == nil || second == nil || first.Auth.Password == second.Auth.Password {
		t.Fatalf("jobs = %+v, %+v", first, second)
	}
	if r := g.callT(t, dana, "list_branches", map[string]any{"repository": "acme/widgets"}); r.isError {
		t.Fatal(r.text)
	}
	if third, _, _ := g.gitCall(t, dana, "clone", clone); third == nil || third.Auth.Password == first.Auth.Password {
		t.Errorf("third job = %+v", third)
	}
	mints := f.mints()
	if len(mints) != 4 {
		t.Fatalf("%d mints, want 4", len(mints))
	}
	api := f.api()
	if bearer := api[len(api)-1].Bearer; bearer == first.Auth.Password || bearer == second.Auth.Password {
		t.Error("the REST call used a job's token")
	}
	if len(g.tokens) != 1 {
		t.Errorf("cache = %v", g.tokens)
	}
}

// Why this test exists: a token GitHub refuses (an app not installed on
// the repository, or lacking contents: write after the update) is
// GitHub's answer, not Kivali's refusal: the agent reads why, as an
// error result, and no job goes out without a token. A connection
// setup has not finished mints nothing.
func TestAJobWithoutATokenIsAnErrorAndNoJob(t *testing.T) {
	g, f := newTest(t)
	f.mintReply = &reply{Status: 422, Body: `{"message":"The permissions requested are not granted to this installation."}`}
	job, res, ref := g.gitCall(t, agent(true, "acme/*"), "push", `{"path":"w","url":"acme/widgets","refs":["agent/x"]}`)
	if job != nil || ref != nil || !res.IsError || res.Content[0] !=
		"GitHub refused the GitHub App a token for acme/widgets (HTTP 422): The permissions requested are not granted to this installation. The app may not be installed on it, or lack the permission; ask the owner." {
		t.Errorf("refused a token = %+v %+v %+v", job, res, ref)
	}

	conn := testConn()
	delete(conn.Fields, FieldInstallationID)
	before := len(f.requests())
	res2, err := g.Call(context.Background(), agent(false, "acme/*"), conn, "clone", json.RawMessage(`{"url":"acme/widgets","path":"w"}`))
	ref2, ok := err.(*builtinintegrations.Refusal)
	if res2 != nil || !ok || ref2.Text != notSetUp || ref2.Reason != "the connection is incomplete" || len(f.requests()) != before {
		t.Errorf("not set up = %+v %v", res2, err)
	}
}

// Why this test exists: the write switch can be turned off while a
// push runs. The second request must not mint anything, and since the
// push may have landed, a result that says refs moved is told and
// audited as landed under the old access, as Git (Basic)'s is, rather
// than refused.
func TestCompleteOfAPushAfterWriteWasTurnedOff(t *testing.T) {
	g, f := newTest(t)
	args := json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["agent/x"]}`)
	res, err := g.Complete(context.Background(), agent(false, "acme/*"), testConn(), "push", args,
		shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", ConnectionID: "c0ffee01",
			Updated: []shelljob.GitRefUpdate{{Ref: "refs/heads/agent/x", New: "2222222bbbb"}}}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0], "Your access to the GitHub integration changed during the push") ||
		res.Audit != "pushed refs/heads/agent/x to acme/widgets; the access changed during the push" {
		t.Errorf("complete = %+v, %v", res, err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("Complete made %d requests", n)
	}
}
