package gitintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// dana is an agent that may pull the repositories patterns name and
// push to none.
func dana(patterns ...any) builtinintegrations.Caller {
	return builtinintegrations.Caller{Agent: "dana", Access: map[string]any{"pull_repositories": patterns}}
}

// Why this test exists: the manifest is what the Org page draws the
// connection and access forms from and what boot writes to every team.
// It must pass the built-in check, ask for the host and the token (a
// secret) in that order, take the repositories it may pull and the
// ones it may push to as two lists, neither required (CheckAccess asks
// for one), followed by the refs it may push and the force switch
// marked dangerous, list exactly clone, fetch and push, the actions
// Call and Tools know, push marked as needing a repository to push to,
// and name no kivali.verify, so that Kivali's server never reaches a
// git host itself.
func TestManifest(t *testing.T) {
	m := New().Manifest()
	if f := integration.ValidateBuiltin(m); len(f) != 0 {
		t.Fatalf("findings: %v", f.Texts())
	}
	var keys []string
	for _, f := range m.UserConfig {
		keys = append(keys, f.Key)
	}
	if strings.Join(keys, ",") != "host,token" || !m.UserConfig[1].Sensitive || !m.UserConfig[0].Required || !m.UserConfig[1].Required {
		t.Errorf("user_config = %+v", m.UserConfig)
	}
	if m.Kivali.Verify != "" {
		t.Errorf("kivali.verify = %q", m.Kivali.Verify)
	}
	form := integration.AccessForm(m)
	if form == nil || len(form.Fields) != 4 {
		t.Fatalf("access form = %+v", form)
	}
	want := []struct {
		key      string
		kind     integration.FieldKind
		required bool
		danger   bool
	}{
		{"pull_repositories", integration.FieldList, false, false},
		{"push_repositories", integration.FieldList, false, false},
		{"push_refs", integration.FieldList, false, false},
		{"force", integration.FieldSwitch, false, true},
	}
	for i, w := range want {
		f := form.Fields[i]
		if f.Key != w.key || f.Kind != w.kind || f.Required != w.required || f.Danger != w.danger || f.Title == "" || f.Description == "" {
			t.Errorf("access field %d = %+v, want %+v", i, f, w)
		}
	}
	if m.Version != "2.0.0" {
		t.Errorf("version = %s", m.Version)
	}
	tools, _ := New().Tools(context.Background(), builtinintegrations.Caller{})
	if len(tools) != 3 || tools[0].Name != "clone" || tools[1].Name != "fetch" || tools[2].Name != "push" || len(m.Tools) != 3 || m.Tools[2].Name != "push" {
		t.Errorf("tools = %+v, manifest %+v", tools, m.Tools)
	}
	// The page groups push under the repositories it may push to, and
	// clone and fetch under any access; push unmarked would read as an
	// action any access gives.
	if m.Tools[0].Access != "" || m.Tools[1].Access != "" || m.Tools[2].Access != "push_repositories" {
		t.Errorf("tools' access = %+v", m.Tools)
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
// expects, the connection it runs through (which the second request
// reads back), and no proxy credential (core adds the connection's).
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
		TimeoutSeconds: 900, ConnectionID: "1a2b3c4d",
	}
	got, _ := json.Marshal(res.ShellJob.Git)
	if w, _ := json.Marshal(want); string(got) != string(w) {
		t.Errorf("clone job = %s, want %s", got, w)
	}
	// The row core writes as it hands the job out names the repository.
	if res.Audit != "started: Acme/Widgets" {
		t.Errorf("started row = %q", res.Audit)
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
	if res.Audit != "started: the working copy's origin at ~/w" {
		t.Errorf("started row of a fetch from origin = %q", res.Audit)
	}
	res, err = g.Call(context.Background(), dana("acme/widgets"), testConn("github.com"), "fetch", json.RawMessage(`{"path":"w","url":"acme/widgets"}`))
	if err != nil || res.ShellJob.Git.URL != "https://github.com/acme/widgets.git" {
		t.Errorf("fetch with url = %+v, %v", res, err)
	}
}

// pusher is an agent that may push agent/* and every tag to acme/*,
// and force when force is set.
func pusher(force bool) builtinintegrations.Caller {
	return builtinintegrations.Caller{Agent: "dana", Access: map[string]any{
		"push_repositories": []any{"acme/*"}, "push_refs": []any{"agent/*", "tags/*"}, "force": force,
	}}
}

// pullOnly is the access a with the repositories it may push to moved
// to the ones it may only pull.
func pullOnly(a map[string]any) {
	a["pull_repositories"] = a["push_repositories"]
	delete(a, "push_repositories")
}

// Why this test exists: push is granted per repository. An agent that
// may pull acme/* and push only to acme/widgets must clone and fetch
// any of acme/*, push to acme/widgets, and be refused a push to
// another of acme/* in words that say it may pull that one but not
// push to it. The job of a push must carry only the repositories it may
// push to, since a push without a url reaches whatever the working
// copy's origin names and the daemon holds that to the job's list; a
// clone's job carries both lists.
func TestPushIsPerRepository(t *testing.T) {
	g := New()
	ctx := context.Background()
	caller := builtinintegrations.Caller{Agent: "dana", Access: map[string]any{
		"pull_repositories": []any{"acme/*"}, "push_repositories": []any{"acme/widgets"}, "push_refs": []any{"agent/*"},
	}}
	for _, repo := range []string{"acme/gadgets", "acme/widgets"} {
		res, err := g.Call(ctx, caller, testConn("github.com"), "clone", json.RawMessage(`{"url":"`+repo+`","path":"w"}`))
		if err != nil || strings.Join(res.ShellJob.Git.AllowedRepositories, " ") != "acme/* acme/widgets" {
			t.Errorf("clone %s = %+v, %v", repo, res, err)
		}
	}
	res, err := g.Call(ctx, caller, testConn("github.com"), "push", json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["agent/x"]}`))
	if err != nil || strings.Join(res.ShellJob.Git.AllowedRepositories, " ") != "acme/widgets" {
		t.Errorf("push to acme/widgets = %+v, %v", res, err)
	}
	res, err = g.Call(ctx, caller, testConn("github.com"), "push", json.RawMessage(`{"path":"w","refs":["agent/x"]}`))
	if err != nil || res.ShellJob.Git.URL != "" || strings.Join(res.ShellJob.Git.AllowedRepositories, " ") != "acme/widgets" {
		t.Errorf("push to the working copy's origin = %+v, %v", res, err)
	}
	_, err = g.Call(ctx, caller, testConn("github.com"), "push", json.RawMessage(`{"path":"w","url":"acme/gadgets","refs":["agent/x"]}`))
	var ref *builtinintegrations.Refusal
	if !errors.As(err, &ref) || ref.Reason != "the access does not allow pushing to acme/gadgets" ||
		ref.Text != "The Git integration's access for you lets you pull acme/gadgets but not push to it. Ask the owner if your work needs it." {
		t.Errorf("push to acme/gadgets = %v", err)
	}
}

// Why this test exists: a push's job is what the daemon writes to the
// owner's repository with, so core must hand it the refs normalised
// (a bare name a branch, tags/ a tag, a bare ref pushing itself), +
// only on what the call forces, Force only when the call forces, the
// access's push_refs normalised for the daemon's own check, and the
// push's bound.
func TestCallBuildsThePushJob(t *testing.T) {
	g := New()
	res, err := g.Call(context.Background(), pusher(false), testConn("github.com"), "push",
		json.RawMessage(`{"path":"~/w","refs":["agent/x","tags/v1.2.0","HEAD:refs/heads/agent/y"]}`))
	if err != nil || res.ShellJob == nil || res.ShellJob.Git == nil {
		t.Fatalf("push = %+v, %v", res, err)
	}
	want := shelljob.GitJob{
		Op: "push", Path: "~/w", Host: "github.com",
		RefSpecs:            []string{"refs/heads/agent/x:refs/heads/agent/x", "refs/tags/v1.2.0:refs/tags/v1.2.0", "HEAD:refs/heads/agent/y"},
		AllowedRepositories: []string{"acme/*"}, AllowedRefs: []string{"refs/heads/agent/*", "refs/tags/*"},
		Auth: shelljob.GitAuth{Username: "x-access-token", Password: testToken}, TimeoutSeconds: 600, ConnectionID: "1a2b3c4d",
	}
	got, _ := json.Marshal(res.ShellJob.Git)
	if w, _ := json.Marshal(want); string(got) != string(w) {
		t.Errorf("push job = %s, want %s", got, w)
	}

	res, err = g.Call(context.Background(), pusher(true), testConn("github.com"), "push",
		json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["agent/x","+HEAD:agent/y"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if j := res.ShellJob.Git; !j.Force || j.URL != "https://github.com/acme/widgets.git" ||
		strings.Join(j.RefSpecs, " ") != "refs/heads/agent/x:refs/heads/agent/x +HEAD:refs/heads/agent/y" {
		t.Errorf("a push with one forced ref = %+v", j)
	}
	res, _ = g.Call(context.Background(), pusher(true), testConn("github.com"), "push", json.RawMessage(`{"path":"w","refs":["agent/x"],"force":true}`))
	if j := res.ShellJob.Git; !j.Force || j.RefSpecs[0] != "+refs/heads/agent/x:refs/heads/agent/x" {
		t.Errorf("force: true = %+v", j)
	}
}

// Why this test exists: each way a push can reach past what the owner
// granted must be refused in core, before any job carries the token:
// no repository to push to (or a push list that is not a list), a
// destination outside push_refs (a bare v1 is a branch, not the tag
// tags/* allows), push_refs that do not read or are empty, a force
// without the force switch (by force or by +), a deletion, a ref that
// does not read, a destination twice, and a repository outside the
// access. Each names the cause and the owner only, and is a Refusal so
// core audits it as not allowed.
func TestPushRefusals(t *testing.T) {
	g := New()
	with := func(edit func(map[string]any)) builtinintegrations.Caller {
		c := pusher(false)
		edit(c.Access)
		return c
	}
	for name, c := range map[string]struct {
		caller builtinintegrations.Caller
		args   string
		words  string
		reason string
	}{
		"pull only":      {with(pullOnly), `{"path":"w","refs":["agent/x"]}`, "does not allow pushing. Ask the owner", "the access does not allow pushing"},
		"push empty":     {with(func(a map[string]any) { a["push_repositories"] = []any{}; a["pull_repositories"] = []any{"acme/*"} }), `{"path":"w","refs":["agent/x"]}`, "does not allow pushing", "the access does not allow pushing"},
		"push not list":  {with(func(a map[string]any) { a["push_repositories"] = "acme/*" }), `{"path":"w","refs":["agent/x"]}`, "names no repositories Kivali can read", "the access names no repositories"},
		"push missing":   {dana("acme/*"), `{"path":"w","refs":["agent/x"]}`, "does not allow pushing", "the access does not allow pushing"},
		"pull only, url": {with(pullOnly), `{"path":"w","url":"acme/widgets","refs":["agent/x"]}`, "does not allow pushing", "the access does not allow pushing"},
		"outside refs":   {pusher(false), `{"path":"w","refs":["main"]}`, "does not include refs/heads/main. Ask the owner", "the access does not include refs/heads/main"},
		"a bare tag":     {pusher(false), `{"path":"w","refs":["v1"]}`, "does not include refs/heads/v1", "the access does not include refs/heads/v1"},
		"a refspec dst":  {pusher(false), `{"path":"w","refs":["HEAD:refs/heads/main"]}`, "does not include refs/heads/main", "the access does not include refs/heads/main"},
		"no push_refs":   {with(func(a map[string]any) { delete(a, "push_refs") }), `{"path":"w","refs":["agent/x"]}`, "does not include refs/heads/agent/x", "the access does not include refs/heads/agent/x"},
		"bad push_refs":  {with(func(a map[string]any) { a["push_refs"] = []any{"agent/*", "refs/notes/*"} }), `{"path":"w","refs":["agent/x"]}`, "names branches or tags Kivali cannot read", "the access's branches and tags do not read"},
		"force off":      {pusher(false), `{"path":"w","refs":["agent/x"],"force":true}`, "does not allow force-pushing. Ask the owner", "the access does not allow force-pushing"},
		"a + force off":  {pusher(false), `{"path":"w","refs":["+agent/x"]}`, "does not allow force-pushing", "the access does not allow force-pushing"},
		"a deletion":     {pusher(true), `{"path":"w","refs":[":refs/heads/agent/x"]}`, "Deleting a remote ref is not something the Git integration does.", "the call deletes a remote ref"},
		"does not read":  {pusher(false), `{"path":"w","refs":["agent/x..y"]}`, "git_push cannot read", "a ref does not read"},
		"twice":          {pusher(false), `{"path":"w","refs":["agent/x","HEAD:agent/x"]}`, "names refs/heads/agent/x twice", "a destination is named twice"},
		"no refs":        {pusher(false), `{"path":"w","refs":[]}`, "needs path and refs", "the arguments lack path or refs"},
		"force not bool": {pusher(false), `{"path":"w","refs":["agent/x"],"force":"yes"}`, "git_push takes path and refs", "the arguments do not read"},
		"outside repos":  {pusher(false), `{"path":"w","url":"other/x","refs":["agent/x"]}`, "does not include other/x", "the access does not include other/x"},
	} {
		res, err := g.Call(context.Background(), c.caller, testConn("github.com"), "push", json.RawMessage(c.args))
		var ref *builtinintegrations.Refusal
		if !errors.As(err, &ref) || res != nil {
			t.Errorf("%s: = %+v, %v; want a refusal", name, res, err)
			continue
		}
		if !strings.Contains(ref.Text, c.words) || ref.Reason != c.reason {
			t.Errorf("%s: refusal %+v, want words %q and reason %q", name, ref, c.words, c.reason)
		}
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
		"an unknown action": {dana("acme/*"), testConn("github.com"), "pull", clone(`{}`), "no action pull"},
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
	// The daemon lists only its first refs of a large clone; the answer
	// and the audit count every ref that moved.
	many := shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", UpdatedTotal: 150000}}
	for i := range 200 {
		many.Git.Updated = append(many.Git.Updated, shelljob.GitRefUpdate{Ref: fmt.Sprintf("refs/tags/v%d", i), New: "3333333cccc"})
	}
	res, _ = g.Complete(ctx, dana("acme/*"), testConn("github.com"), "clone", cloneArgs, many)
	if res.IsError || !strings.Contains(res.Content[0], "Updated (150000):") || !strings.Contains(res.Content[0], "... and 149950 more") ||
		!strings.HasSuffix(res.Audit, " and 149990 more") || len(res.Content[0]) > 8<<10 {
		t.Errorf("a large clone = %q, audit %q", res.Content[0][:min(300, len(res.Content[0]))], res.Audit)
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

// Why this test exists: a push's answer tells the agent what landed on
// the owner's repository, and its audit row is the owner's record of
// what was written with their token. A success must name each ref,
// new or moved, and the repository; a forced push must say so in the
// audit; nothing to push and a host's refusal must say which refs; and
// push or force taken away while the job ran must refuse the second
// request.
func TestCompletePush(t *testing.T) {
	g := New()
	ctx := context.Background()
	args := json.RawMessage(`{"path":"/files/w","url":"acme/widgets","refs":["agent/x","tags/v1"]}`)
	done := shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", Updated: []shelljob.GitRefUpdate{
		{Ref: "refs/heads/agent/x", New: "1111111aaaa"},
		{Ref: "refs/tags/v1", Old: "2222222bbbb", New: "3333333cccc"},
	}}}
	res, err := g.Complete(ctx, pusher(false), testConn("github.com"), "push", args, done)
	if err != nil || res.IsError || !strings.HasPrefix(res.Content[0], "Pushed refs/heads/agent/x (new) and refs/tags/v1 to acme/widgets.") ||
		!strings.Contains(res.Content[0], "refs/heads/agent/x new at 1111111") || !strings.Contains(res.Content[0], "refs/tags/v1 2222222..3333333") ||
		res.Audit != "acme/widgets: pushed refs/heads/agent/x, refs/tags/v1" {
		t.Errorf("push = %+v, %v", res, err)
	}

	forced := json.RawMessage(`{"path":"/files/w","refs":["agent/x"],"force":true}`)
	moved := shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", Updated: []shelljob.GitRefUpdate{{Ref: "refs/heads/agent/x", Old: "1111111aaaa", New: "4444444dddd"}}}}
	res, _ = g.Complete(ctx, pusher(true), testConn("github.com"), "push", forced, moved)
	if res.IsError || !strings.HasPrefix(res.Content[0], "Force-pushed refs/heads/agent/x to acme/widgets.") || res.Audit != "acme/widgets: force-pushed refs/heads/agent/x" {
		t.Errorf("force push = %+v", res)
	}

	res, _ = g.Complete(ctx, pusher(false), testConn("github.com"), "push", args, shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets"}})
	if res.IsError || res.Content[0] != "Nothing to push: acme/widgets already has refs/heads/agent/x and refs/tags/v1 as the working copy does." ||
		res.Audit != "acme/widgets: up to date, refs/heads/agent/x, refs/tags/v1" {
		t.Errorf("up to date = %+v", res)
	}

	rejected := shelljob.Result{Git: &shelljob.GitResult{Error: "push: non-fast-forward update: refs/heads/agent/x"}}
	res, _ = g.Complete(ctx, pusher(false), testConn("github.com"), "push", args, rejected)
	if !res.IsError || res.Content[0] != "The push of acme/widgets did not go through: push: non-fast-forward update: refs/heads/agent/x" ||
		res.Audit != "acme/widgets: push of refs/heads/agent/x, refs/tags/v1: push: non-fast-forward update: refs/heads/agent/x" {
		t.Errorf("rejected = %+v", res)
	}
	res, _ = g.Complete(ctx, pusher(true), testConn("github.com"), "push", forced, rejected)
	if !res.IsError || res.Audit != "the working copy's origin: force push of refs/heads/agent/x: push: non-fast-forward update: refs/heads/agent/x" {
		t.Errorf("rejected force push = %+v", res)
	}

	// Access taken away while the job ran, and nothing landed: refused.
	var ref *builtinintegrations.Refusal
	if _, err := g.Complete(ctx, pusher(false), testConn("github.com"), "push", forced, rejected); !errors.As(err, &ref) || ref.Reason != "the access does not allow force-pushing" {
		t.Errorf("force taken away, nothing landed = %v", err)
	}
	off := pusher(false)
	pullOnly(off.Access)
	if _, err := g.Complete(ctx, off, testConn("github.com"), "push", args, shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets"}}); !errors.As(err, &ref) || ref.Reason != "the access does not allow pushing" {
		t.Errorf("push taken away, nothing landed = %v", err)
	}
}

// Why this test exists: the owner can take a repository off the ones an
// agent may push to, or narrow push_refs, while a push runs. The remote
// is written all the same, so the second request must not be audited
// as refused: the row must be allowed and name what landed (only the
// refs the call asked for, never one the shell's report adds), and the
// agent must read what landed and that its access changed.
func TestCompletePushWhenTheAccessChangedDuringIt(t *testing.T) {
	g := New()
	off := pusher(false)
	pullOnly(off.Access)
	args := json.RawMessage(`{"path":"/files/w","refs":["agent/x"]}`)
	landed := shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", Updated: []shelljob.GitRefUpdate{
		{Ref: "refs/heads/agent/x", New: "1111111aaaa"},
		{Ref: "refs/heads/main", Old: "2222222bbbb", New: "3333333cccc"},
	}}}
	res, err := g.Complete(context.Background(), off, testConn("github.com"), "push", args, landed)
	if err != nil || res.IsError {
		t.Fatalf("landed under a changed access = %+v, %v", res, err)
	}
	if res.Audit != "pushed refs/heads/agent/x to acme/widgets; the access changed during the push" ||
		!strings.HasPrefix(res.Content[0], "Pushed refs/heads/agent/x (new) to acme/widgets. Your access to the Git integration changed during the push") ||
		strings.Contains(res.Content[0], "refs/heads/main") {
		t.Errorf("landed under a changed access = %+v", res)
	}
}

// Why this test exists: the result is the agent's pod's own report. A
// push's answer and audit must name only the destinations core asked
// for, so a report that adds a ref cannot write a row saying it was
// pushed; and when the daemon cut its list short (UpdatedTotal above
// the refs listed), the count must still say how many moved.
func TestPushedNamesOnlyWhatWasAsked(t *testing.T) {
	g := New()
	ctx := context.Background()
	args := json.RawMessage(`{"path":"/files/w","url":"acme/widgets","refs":["agent/x"]}`)
	extra := shelljob.Result{Git: &shelljob.GitResult{Updated: []shelljob.GitRefUpdate{
		{Ref: "refs/heads/main", New: "3333333cccc"},
		{Ref: "refs/heads/agent/x", New: "1111111aaaa"},
	}}}
	res, _ := g.Complete(ctx, pusher(false), testConn("github.com"), "push", args, extra)
	if res.Audit != "acme/widgets: pushed refs/heads/agent/x" || strings.Contains(res.Content[0], "main") {
		t.Errorf("a report with a ref not asked for = %+v", res)
	}
	only := shelljob.Result{Git: &shelljob.GitResult{Updated: []shelljob.GitRefUpdate{{Ref: "refs/heads/main", New: "3333333cccc"}}}}
	res, _ = g.Complete(ctx, pusher(false), testConn("github.com"), "push", args, only)
	if !strings.HasPrefix(res.Content[0], "Nothing to push") {
		t.Errorf("a report of only refs not asked for = %+v", res)
	}

	many := json.RawMessage(`{"path":"/files/w","url":"acme/widgets","refs":["agent/a","agent/b","agent/c"]}`)
	cut := shelljob.Result{Git: &shelljob.GitResult{UpdatedTotal: 3, Updated: []shelljob.GitRefUpdate{{Ref: "refs/heads/agent/a", New: "1111111aaaa"}}}}
	res, _ = g.Complete(ctx, pusher(false), testConn("github.com"), "push", many, cut)
	if !strings.HasPrefix(res.Content[0], "Pushed refs/heads/agent/a (new) and 2 more to acme/widgets.") ||
		res.Audit != "acme/widgets: pushed refs/heads/agent/a and 2 more" {
		t.Errorf("a cut list = %+v", res)
	}
}

// Why this test exists: core writes an audit row as it hands a job
// out, so a push whose second request never comes is still on record.
// For a push that row must name the destinations and whether it
// forces, from core's own reading of the refs (the shell has not run
// yet, and its report is the agent's pod's).
func TestPushStartedRow(t *testing.T) {
	g := New()
	res, err := g.Call(context.Background(), pusher(true), testConn("github.com"), "push",
		json.RawMessage(`{"path":"w","url":"acme/widgets","refs":["agent/x","+HEAD:tags/v2"]}`))
	if err != nil || res.Audit != "started: acme/widgets, force push of refs/heads/agent/x, refs/tags/v2" {
		t.Errorf("started = %+v, %v", res, err)
	}
	res, _ = g.Call(context.Background(), pusher(false), testConn("github.com"), "push", json.RawMessage(`{"path":"w","refs":["agent/x"]}`))
	if res.Audit != "started: the working copy's origin at w, push of refs/heads/agent/x" {
		t.Errorf("started without a url = %q", res.Audit)
	}
}

// Why this test exists: a connection is checked when it is saved, so a
// host the integration could never reach is a 400 the owner reads at
// once rather than every agent's call refused later; and an access
// entry that is not a repository or a pattern is refused the same way,
// rather than stored and granting nothing.
func TestCheckConnectionAndAccess(t *testing.T) {
	g := New()
	for _, ok := range []string{"github.com", "https://GitLab.com/", "git.acme.io:8443"} {
		if err := g.CheckConnection(testConn(ok)); err != nil {
			t.Errorf("%q = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "github.com/acme", "user@github.com", "http://github.com"} {
		if err := g.CheckConnection(testConn(bad)); err == nil || err.Error() != "Host is a host name such as github.com, without https:// or a path" {
			t.Errorf("%q = %v", bad, err)
		}
	}
	if err := g.CheckAccess(map[string]any{"pull_repositories": []any{"acme/widgets", "acme/*"}}); err != nil {
		t.Errorf("good access = %v", err)
	}
	if err := g.CheckAccess(map[string]any{"pull_repositories": []any{"acme/*", "acme/[x"}}); err == nil || err.Error() != "acme/[x is not a repository or a pattern" {
		t.Errorf("a bad pattern = %v", err)
	}
}
