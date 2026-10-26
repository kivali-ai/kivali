package githubintegration

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitops"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// Why this test exists: a working copy a writer's GitHub clone makes is
// given the app's bot as its git identity, so the commits an agent
// makes there read as the bot's, which is what lets it delete such a
// branch later. The clone's job must name the bot's login and its
// no-reply address exactly as GitHub resolves them (<id>+<login>@...),
// a fetch or a push none; the bot user is read once per connection, by
// the slug GitHub gives the app, with an installation token that reads
// metadata alone; and the writer is told in github_clone's description
// whose identity its commits carry.
func TestACloneIsGivenTheAppsBotAsItsIdentity(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	f.route("GET /users/kivali-acme[bot]", 200, botUserJSON)
	writer := agent(true, "acme/*")
	want := shelljob.GitIdentity{Name: "kivali-acme[bot]", Email: "99+kivali-acme[bot]@users.noreply.github.com"}
	for range 2 {
		job, res, ref := g.gitCall(t, writer, "clone", `{"url":"acme/widgets","path":"w"}`)
		if ref != nil || job == nil || job.Identity == nil || *job.Identity != want {
			t.Fatalf("clone = %+v, %+v, %+v", job, res, ref)
		}
	}
	for name, args := range map[string]string{
		"fetch": `{"path":"w","url":"acme/widgets"}`,
		"push":  `{"path":"w","url":"acme/widgets","refs":["agent/x"]}`,
	} {
		if job, res, ref := g.gitCall(t, writer, name, args); job == nil || job.Identity != nil {
			t.Errorf("%s = %+v, %+v, %+v", name, job, res, ref)
		}
	}
	var users []recorded
	for _, q := range f.api() {
		if q.Path == "/users/kivali-acme[bot]" {
			users = append(users, q)
		}
	}
	if len(users) != 1 || users[0].Grant == nil || !mapsEqual(users[0].Grant.Permissions, map[string]string{"metadata": "read"}) ||
		!slices.Equal(users[0].Grant.Repositories, []string{"widgets"}) {
		t.Errorf("GET /users = %+v", users)
	}
	if apps := appReads(f); len(apps) != 1 || apps[0].JWT == nil {
		t.Errorf("GET /app = %+v", apps)
	}

	for _, write := range []bool{true, false} {
		tools, err := g.Tools(context.Background(), agent(write, "acme/*"))
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range tools {
			if tl.Name == gitops.Clone && strings.Contains(tl.Description, "commits you make there are authored as the app's bot unless you change the identity") != write {
				t.Errorf("write %v: clone's description: %q", write, tl.Description)
			}
		}
	}
}

// Why this test exists: an agent whose access only reads cannot push
// what it commits, so its clone needs no identity, and must not hang
// on reading the bot user: a lookup that fails would fail a read-only
// clone for nothing. Its job names no identity and nothing but the
// job's token is asked of GitHub.
func TestAReadOnlyCloneHasNoIdentity(t *testing.T) {
	g, f := newTest(t)
	job, res, ref := g.gitCall(t, agent(false, "acme/*"), "clone", `{"url":"acme/widgets","path":"w"}`)
	if ref != nil || job == nil || job.Identity != nil {
		t.Fatalf("clone = %+v, %+v, %+v", job, res, ref)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("GitHub took %d requests, want the job's mint", n)
	}
}

// Why this test exists: the slug the connection keeps is the app's
// name when setup ran, and GitHub has no bot user by it once the app
// is renamed. The bot user is read by the slug GitHub gives the app
// now (GET /app), so the working copy gets the renamed bot's identity.
func TestACloneOfARenamedAppHasTheBotsNewIdentity(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, `{"id":1234,"slug":"kivali-acme-renamed","name":"Kivali acme renamed"}`)
	f.route("GET /users/kivali-acme-renamed[bot]", 200, `{"login":"kivali-acme-renamed[bot]","id":77,"type":"Bot"}`)
	job, res, ref := g.gitCall(t, agent(true, "acme/*"), "clone", `{"url":"acme/widgets","path":"w"}`)
	want := shelljob.GitIdentity{Name: "kivali-acme-renamed[bot]", Email: "77+kivali-acme-renamed[bot]@users.noreply.github.com"}
	if ref != nil || job == nil || job.Identity == nil || *job.Identity != want {
		t.Errorf("clone = %+v, %+v, %+v", job, res, ref)
	}
}

// Why this test exists: the identity is a convenience on top of the
// clone, so a bot user GitHub does not give (not found, or a user that
// is not the app's bot) must not fail a writer's clone: the job goes
// out without an identity. The clone's answer must then say its commits
// will not read as the app's, or the agent would expect to delete the
// branch it makes there; a clone the shell gave the identity, and a
// reader's, say nothing of it.
func TestACloneWithoutTheBotUserLandsAndSaysSo(t *testing.T) {
	args := json.RawMessage(`{"url":"acme/widgets","path":"w"}`)
	landed := func(identity bool) shelljob.Result {
		return shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", ConnectionID: "c0ffee01", Identity: identity,
			Updated: []shelljob.GitRefUpdate{{Ref: "refs/remotes/origin/main", New: "1111111aaaa"}}}}
	}
	for name, c := range map[string]struct{ status int }{
		"not found": {404},
		"not a bot": {200},
	} {
		g, f := newTest(t)
		f.route("GET /app", 200, appInfoJSON)
		f.route("GET /users/kivali-acme[bot]", c.status, `{"login":"kivali-acme[bot]","id":99,"type":"User"}`)
		job, res, ref := g.gitCall(t, agent(true, "acme/*"), "clone", string(args))
		if ref != nil || job == nil || job.Identity != nil {
			t.Errorf("%s: clone = %+v, %+v, %+v", name, job, res, ref)
		}
	}

	g, _ := newTest(t)
	for _, c := range []struct {
		write, identity, says bool
	}{
		{true, false, true},
		{true, true, false},
		{false, false, false},
	} {
		res, err := g.Complete(context.Background(), agent(c.write, "acme/*"), testConn(), "clone", args, landed(c.identity))
		if err != nil || res.IsError || !strings.HasPrefix(res.Content[0], "Cloned acme/widgets into w.") ||
			strings.HasSuffix(res.Content[0], noIdentity) != c.says {
			t.Errorf("%+v: %+v, %v", c, res, err)
		}
	}
}

// Why this test exists: a commit GitHub resolved no user for is held to
// the bot's no-reply address, which needs the bot user. GitHub
// answering for it with a user that is not the app's bot is not a
// passing outage: the deletion fails in words that say what is wrong
// and send the agent to the owner, not to try again.
func TestADeletionWithoutTheBotUserFailsInWords(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	f.route("GET /users/kivali-acme[bot]", 200, `{"login":"kivali-acme[bot]","id":99,"type":"User"}`)
	branchRoutes(f, compareJSON(1, commitJSON("aaa", "null", "null", botNoreply, botNoreply)))
	r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
	if !r.isError || r.refusal != nil || r.text != "GitHub's user kivali-acme[bot] is not the GitHub App's bot user; ask the owner." {
		t.Errorf("%+v", r)
	}
	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}
