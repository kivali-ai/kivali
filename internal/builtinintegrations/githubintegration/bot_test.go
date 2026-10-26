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

// Why this test exists: a working copy a GitHub clone makes is given
// the app's bot as its git identity, so the commits an agent makes
// there read as the bot's, which is what lets it delete such a branch
// later. The clone's job must name the bot's login and its no-reply
// address exactly as GitHub resolves them (<id>+<login>@...), a fetch
// or a push none; the bot user is read once per connection, with an
// installation token that reads metadata alone, and the agent is told
// in github_clone's description whose identity its commits carry.
func TestACloneIsGivenTheAppsBotAsItsIdentity(t *testing.T) {
	g, f := newTest(t)
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

	tools, err := g.Tools(context.Background(), writer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Name == gitops.Clone && !strings.Contains(tl.Description, "commits you make there are authored as the app's bot unless you change the identity") {
			t.Errorf("clone's description: %q", tl.Description)
		}
	}
}

// Why this test exists: the slug the connection keeps is the app's
// name when setup ran, and GitHub has no bot user by it once the app
// is renamed. Held to it alone, every clone of a renamed app would
// fail; the bot user is read again by the slug GitHub gives the app
// now (GET /app), and the working copy gets that bot's identity.
func TestACloneOfARenamedAppHasTheBotsNewIdentity(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, `{"id":1234,"slug":"kivali-acme-renamed","name":"Kivali acme renamed"}`)
	f.route("GET /users/kivali-acme-renamed[bot]", 200, `{"login":"kivali-acme-renamed[bot]","id":77,"type":"Bot"}`)
	job, res, ref := g.gitCall(t, agent(false, "acme/*"), "clone", `{"url":"acme/widgets","path":"w"}`)
	want := shelljob.GitIdentity{Name: "kivali-acme-renamed[bot]", Email: "77+kivali-acme-renamed[bot]@users.noreply.github.com"}
	if ref != nil || job == nil || job.Identity == nil || *job.Identity != want {
		t.Errorf("clone = %+v, %+v, %+v", job, res, ref)
	}
}

// Why this test exists: without the bot user, the working copy could
// not be given the bot's identity, and the agent's commits would carry
// whatever identity the shell has. GitHub not answering for it, or
// answering a user that is not the app's bot, is an error result in
// words, and no job, so no token for one, goes out.
func TestACloneWithoutTheBotUserIsAnErrorAndNoJob(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"not found":     {404, `{"message":"Not Found"}`, "GitHub answered 404: Not Found"},
		"not a bot":     {200, `{"login":"kivali-acme[bot]","id":99,"type":"User"}`, "GitHub did not answer: its answer for kivali-acme[bot] is not the app's bot user. Try again later."},
		"another login": {200, `{"login":"someone","id":99,"type":"Bot"}`, "GitHub did not answer: its answer for kivali-acme[bot] is not the app's bot user. Try again later."},
	} {
		g, f := newTest(t)
		f.route("GET /users/kivali-acme[bot]", c.status, c.body)
		res, err := g.Call(context.Background(), agent(false, "acme/*"), testConn(), "clone", json.RawMessage(`{"url":"acme/widgets","path":"w"}`))
		if err != nil || res == nil || res.ShellJob != nil || !res.IsError || res.Content[0] != c.want {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
		for _, m := range f.mints() {
			if strings.Contains(m.Body, "contents") {
				t.Errorf("%s: a job's token was minted: %s", name, m.Body)
			}
		}
	}
}
