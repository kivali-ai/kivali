package githubintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The app's bot user as GitHub names it on what the app opened, and
// other authors.
const (
	appUser      = `{"login":"kivali-acme[bot]","type":"Bot"}`
	personUser   = `{"login":"dana","type":"User"}`
	otherAppUser = `{"login":"other-app[bot]","type":"Bot"}`
)

// appInfoJSON is GET /app's answer for the app the connection keeps.
const appInfoJSON = `{"id":1234,"slug":"kivali-acme","name":"Kivali acme"}`

// appReads is every GET /app the fake took.
func appReads(f *fakeGitHub) []recorded {
	var out []recorded
	for _, q := range f.api() {
		if q.Path == "/app" {
			out = append(out, q)
		}
	}
	return out
}

// pullIntoJSON is an open pull request a person opened from dana/x
// into the branch base.
func pullIntoJSON(n int, base string) string {
	return fmt.Sprintf(`{"number":%d,"title":"t","state":"open","user":%s,"merged_at":null,"head":{"ref":"dana/x","repo":{"full_name":"acme/widgets"}},"base":{"ref":%q},"html_url":"https://github.com/acme/widgets/pull/%d"}`,
		n, personUser, base, n)
}

// issueJSON is an issue GitHub answers, by author and state; a pull
// request, as the issues endpoint serves one, when pull is true.
func issueJSON(n int, user, state string, pull bool) string {
	pr := ""
	if pull {
		pr = fmt.Sprintf(`,"pull_request":{"url":"https://api.github.com/repos/acme/widgets/pulls/%d"}`, n)
	}
	return fmt.Sprintf(`{"number":%d,"title":"t","state":%q,"user":%s,"html_url":"https://github.com/acme/widgets/issues/%d"%s}`, n, state, user, n, pr)
}

// pullJSON is a pull request GitHub answers: its author, state and
// head branch, into main, merged when merged.
func pullJSON(n int, user, state, head string, merged bool) string {
	mergedAt := "null"
	if merged {
		mergedAt = `"2026-10-20T00:00:00Z"`
	}
	return fmt.Sprintf(`{"number":%d,"title":"t","state":%q,"user":%s,"merged_at":%s,"head":{"ref":%q},"base":{"ref":"main"},"html_url":"https://github.com/acme/widgets/pull/%d"}`,
		n, state, user, mergedAt, head, n)
}

// writesMinted is every token the fake minted that may write anything.
func writesMinted(f *fakeGitHub) []string {
	var out []string
	for _, m := range f.mints() {
		var g grant
		if err := json.Unmarshal([]byte(m.Body), &g); err != nil {
			continue
		}
		for name, level := range g.Permissions {
			if level == "write" {
				out = append(out, name)
			}
		}
	}
	return out
}

// writesTaken is every request the fake took that would change
// something on GitHub.
func writesTaken(f *fakeGitHub) []string {
	var out []string
	for _, q := range f.api() {
		if q.Method != "GET" {
			out = append(out, q.Method+" "+q.Path)
		}
	}
	return out
}

// Why this test exists: whether a user GitHub names is the app's bot
// is what every close (the issue's or pull request's author) and every
// branch deletion (the users a commit's author and committer resolve
// to) turns on. Only the app's own bot user counts: the login
// <slug>[bot] of type Bot. A person, another app's bot, a user of
// another type that bears the login, or a login differing in its slug
// must not pass, or an agent could close or delete what someone else
// made. A user of another type
// is never the app and costs no request; a bot that is not the kept
// slug's is checked against GitHub's slug for the app, read once a
// call however many authors it checks.
func TestIsAppBotIsTheAppsBotUserAlone(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	conn, err := readConnection(testConn())
	if err != nil {
		t.Fatal(err)
	}
	c := &call{g: g, conn: conn}
	for _, tc := range []struct {
		user ghUser
		want bool
	}{
		{ghUser{Login: "kivali-acme[bot]", Type: "Bot"}, true},
		{ghUser{Login: "kivali-acme[bot]", Type: "User"}, false},
		{ghUser{Login: "kivali-acme[bot]"}, false},
		{ghUser{Login: "kivali-acme", Type: "Bot"}, false},
		{ghUser{Login: "kivali-acme", Type: "User"}, false},
		{ghUser{Login: "other-app[bot]", Type: "Bot"}, false},
		{ghUser{Login: "kivali-acme-2[bot]", Type: "Bot"}, false},
		{ghUser{Login: "Kivali-Acme[bot]", Type: "Bot"}, false},
		{ghUser{}, false},
	} {
		got, err := c.isAppBot(context.Background(), tc.user)
		if err != nil || got != tc.want {
			t.Errorf("%+v: %v, %v", tc.user, got, err)
		}
	}
	if n := len(appReads(f)); n != 1 {
		t.Errorf("GET /app %d times in one call", n)
	}

	// Neither the app's own bot nor a person asks GitHub.
	c = &call{g: g, conn: conn}
	for _, u := range []ghUser{{Login: "kivali-acme[bot]", Type: "Bot"}, {Login: "dana", Type: "User"}, {Login: "kivali-acme[bot]", Type: "User"}} {
		if _, err := c.isAppBot(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(appReads(f)); n != 1 {
		t.Errorf("GET /app for the kept bot or a person: %d", n)
	}
}

// Why this test exists: the slug the connection keeps is the app's
// name when setup ran, and a renamed app's bot user is GitHub's new
// slug followed by [bot]. Held only to the kept slug, an agent could
// close nothing the renamed app opened; so a bot that is not the kept
// slug's is held to the slug GitHub gives the app now, and only that
// one passes.
func TestIsAppBotKnowsARenamedApp(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, `{"id":1234,"slug":"kivali-acme-renamed","name":"Kivali acme renamed"}`)
	f.route("GET /repos/acme/widgets/issues/7", 200, issueJSON(7, `{"login":"kivali-acme-renamed[bot]","type":"Bot"}`, "open", false))
	f.route("PATCH /repos/acme/widgets/issues/7", 200, issueJSON(7, `{"login":"kivali-acme-renamed[bot]","type":"Bot"}`, "closed", false))
	writer := agent(true, "acme/*")
	if r := g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7}); r.isError {
		t.Fatalf("renamed: %+v", r)
	}
	f.route("GET /repos/acme/widgets/issues/8", 200, issueJSON(8, otherAppUser, "open", false))
	if r := g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 8}); r.refusal == nil {
		t.Errorf("another app's: %+v", r)
	}
	if n := len(appReads(f)); n != 2 {
		t.Errorf("GET /app %d times over two calls", n)
	}
}

// Why this test exists: closing an issue the app opened is what write
// now allows. The issue must be read first with a token that only
// reads, then the comment posted and the issue closed with a token
// narrowed to the repository that may write issues and nothing else,
// in that order (GitHub's own "close with comment" order, so the
// comment is on the issue when it closes). The agent reads the issue's
// address and that it is closed, and the owner's audit row says what
// was closed and the comment posted, or the owner cannot tell from it
// what an agent closed.
func TestCloseIssueTheAppOpened(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/issues/7", 200, issueJSON(7, appUser, "open", false))
	f.route("POST /repos/acme/widgets/issues/7/comments", 201, `{"html_url":"https://github.com/acme/widgets/issues/7#issuecomment-1"}`)
	f.route("PATCH /repos/acme/widgets/issues/7", 200, issueJSON(7, appUser, "closed", false))
	writer := agent(true, "acme/*")
	r := g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7, "comment": "Fixed by #8."})
	if r.isError || r.refusal != nil ||
		r.text != "Closed https://github.com/acme/widgets/issues/7, with the comment https://github.com/acme/widgets/issues/7#issuecomment-1." ||
		r.audit != "closed https://github.com/acme/widgets/issues/7, with the comment https://github.com/acme/widgets/issues/7#issuecomment-1" {
		t.Fatalf("%+v", r)
	}
	api := f.api()
	if len(api) != 3 || api[0].Method != "GET" || api[1].Method != "POST" || api[2].Method != "PATCH" {
		t.Fatalf("requests %+v", api)
	}
	if api[1].Body != `{"body":"Fixed by #8."}` || api[2].Body != `{"state":"closed"}` {
		t.Errorf("bodies %q %q", api[1].Body, api[2].Body)
	}
	wantPerms := []map[string]string{
		{"metadata": "read", "issues": "read", "pull_requests": "read"},
		{"metadata": "read", "issues": "write"},
		{"metadata": "read", "issues": "write"},
	}
	for i, q := range api {
		if q.Grant == nil || !mapsEqual(q.Grant.Permissions, wantPerms[i]) || !slices.Equal(q.Grant.Repositories, []string{"widgets"}) {
			t.Errorf("request %d %s %s: grant %+v", i, q.Method, q.Path, q.Grant)
		}
	}

	// Without a comment, or with a blank one, nothing is posted.
	for _, a := range []map[string]any{{}, {"comment": "  \n"}} {
		before := len(f.api())
		a["repository"], a["number"] = "acme/widgets", 7
		r := g.callT(t, writer, "close_issue", a)
		if r.isError || r.text != "Closed https://github.com/acme/widgets/issues/7." || r.audit != "closed https://github.com/acme/widgets/issues/7" {
			t.Errorf("%v: %+v", a, r)
		}
		if got := f.api()[before:]; len(got) != 2 || got[1].Method != "PATCH" {
			t.Errorf("%v: requests %+v", a, got)
		}
	}
}

// Why this test exists: write lets an agent close only an issue the
// app opened. An issue a person or another app opened must be refused,
// in words that name the cause and send the agent to the owner, with
// nothing written and no token that writes minted; a pull request,
// which GitHub serves from the issues endpoint too, must be refused by
// close_issue rather than closed as an issue; and an issue already
// closed must fail in words rather than read as closed by the agent.
func TestCloseIssueRefusesWhatTheAppDidNotOpen(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	writer := agent(true, "acme/*")
	for _, user := range []string{personUser, otherAppUser, `{"login":"kivali-acme[bot]","type":"User"}`, `null`} {
		f.route("GET /repos/acme/widgets/issues/7", 200, issueJSON(7, user, "open", false))
		r := g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7, "comment": "Closing."})
		if r.refusal == nil || r.text != "Your access lets you close only issues the GitHub App opened; ask the owner." ||
			r.refusal.Reason != "the GitHub App did not open acme/widgets#7" {
			t.Errorf("%s: %+v", user, r)
		}
	}

	f.route("GET /repos/acme/widgets/issues/8", 200, issueJSON(8, appUser, "open", true))
	r := g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 8})
	if r.refusal == nil || r.text != "acme/widgets#8 is a pull request; github_close_pull_request closes it." || r.refusal.Reason != "the number is a pull request" {
		t.Errorf("pull request: %+v", r)
	}

	f.route("GET /repos/acme/widgets/issues/9", 200, issueJSON(9, appUser, "closed", false))
	r = g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 9, "comment": "Closing."})
	if !r.isError || r.refusal != nil || r.text != "Issue acme/widgets#9 is closed already." {
		t.Errorf("closed already: %+v", r)
	}

	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
	if w := writesMinted(f); len(w) != 0 {
		t.Errorf("minted tokens that write %v", w)
	}
}

// Why this test exists: a close that posts its comment and then fails
// to close has written to GitHub. The agent must be told the comment is
// there, or it reads the call as having done nothing and posts the
// comment again when it retries.
func TestACloseThatFailsAfterItsCommentSaysTheCommentIsPosted(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/issues/7", 200, issueJSON(7, appUser, "open", false))
	f.route("POST /repos/acme/widgets/issues/7/comments", 201, `{"html_url":"https://github.com/acme/widgets/issues/7#issuecomment-1"}`)
	f.route("PATCH /repos/acme/widgets/issues/7", 502, `{"message":"Server Error"}`)
	r := g.callT(t, agent(true, "acme/*"), "close_issue", map[string]any{"repository": "acme/widgets", "number": 7, "comment": "Fixed."})
	if !r.isError || r.text != "GitHub answered 502: Server Error. The comment is on GitHub: https://github.com/acme/widgets/issues/7#issuecomment-1." {
		t.Errorf("%+v", r)
	}
}

// Why this test exists: close_pull_request closes, unmerged, only a
// pull request the app opened. GitHub cannot close a merged pull
// request, and an agent told only "GitHub answered 422" would not know
// why, so a merged one fails in words before anything is written. One
// anyone else opened is refused with nothing written and no token that
// writes minted. The comment goes through the issue's comments, as
// comment's does, with the token narrowed to the repository's pull
// requests.
func TestClosePullRequest(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	writer := agent(true, "acme/*")
	f.route("GET /repos/acme/widgets/pulls/5", 200, pullJSON(5, appUser, "closed", "agent/x", true))
	r := g.callT(t, writer, "close_pull_request", map[string]any{"repository": "acme/widgets", "number": 5, "comment": "Closing."})
	if !r.isError || r.refusal != nil || r.text != "Pull request acme/widgets#5 is merged, and a merged pull request cannot be closed." {
		t.Errorf("merged: %+v", r)
	}
	f.route("GET /repos/acme/widgets/pulls/6", 200, pullJSON(6, appUser, "closed", "agent/y", false))
	r = g.callT(t, writer, "close_pull_request", map[string]any{"repository": "acme/widgets", "number": 6})
	if !r.isError || r.refusal != nil || r.text != "Pull request acme/widgets#6 is closed already." {
		t.Errorf("closed: %+v", r)
	}
	for _, user := range []string{personUser, otherAppUser} {
		f.route("GET /repos/acme/widgets/pulls/7", 200, pullJSON(7, user, "open", "dana/x", false))
		r = g.callT(t, writer, "close_pull_request", map[string]any{"repository": "acme/widgets", "number": 7})
		if r.refusal == nil || r.text != "Your access lets you close only pull requests the GitHub App opened; ask the owner." ||
			r.refusal.Reason != "the GitHub App did not open acme/widgets#7" {
			t.Errorf("%s: %+v", user, r)
		}
	}
	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
	if w := writesMinted(f); len(w) != 0 {
		t.Errorf("minted tokens that write %v", w)
	}

	f.route("GET /repos/acme/widgets/pulls/8", 200, pullJSON(8, appUser, "open", "agent/z", false))
	f.route("POST /repos/acme/widgets/issues/8/comments", 201, `{"html_url":"https://github.com/acme/widgets/pull/8#issuecomment-2"}`)
	f.route("PATCH /repos/acme/widgets/pulls/8", 200, pullJSON(8, appUser, "closed", "agent/z", false))
	before := len(f.api())
	r = g.callT(t, writer, "close_pull_request", map[string]any{"repository": "acme/widgets", "number": 8, "comment": "Superseded by #9."})
	if r.isError || r.text != "Closed https://github.com/acme/widgets/pull/8, with the comment https://github.com/acme/widgets/pull/8#issuecomment-2." ||
		r.audit != "closed https://github.com/acme/widgets/pull/8, with the comment https://github.com/acme/widgets/pull/8#issuecomment-2" {
		t.Fatalf("%+v", r)
	}
	api := f.api()[before:]
	if len(api) != 3 || api[1].Path != "/repos/acme/widgets/issues/8/comments" || api[2].Method != "PATCH" || api[2].Body != `{"state":"closed"}` {
		t.Fatalf("requests %+v", api)
	}
	for i, want := range []map[string]string{
		{"metadata": "read", "pull_requests": "read"},
		{"metadata": "read", "pull_requests": "write"},
		{"metadata": "read", "pull_requests": "write"},
	} {
		if q := api[i]; q.Grant == nil || !mapsEqual(q.Grant.Permissions, want) || !slices.Equal(q.Grant.Repositories, []string{"widgets"}) {
			t.Errorf("request %d %s %s: grant %+v", i, q.Method, q.Path, q.Grant)
		}
	}
}

// The bot's no-reply address, its user id 99, and GET
// /users/kivali-acme[bot]'s answer.
const (
	botNoreply  = "99+kivali-acme[bot]@users.noreply.github.com"
	botUserJSON = `{"login":"kivali-acme[bot]","id":99,"type":"Bot"}`
)

// commitJSON is one commit of a comparison: the users GitHub resolved
// its author and committer to (JSON, or null), and the emails git
// records.
func commitJSON(sha, author, committer, authorEmail, committerEmail string) string {
	return fmt.Sprintf(`{"sha":%q,"commit":{"author":{"name":"n","email":%q},"committer":{"name":"n","email":%q}},"author":%s,"committer":%s}`,
		sha, authorEmail, committerEmail, author, committer)
}

// botCommit is a commit the bot authored and committed, as GitHub
// resolves it.
func botCommit(sha string) string { return commitJSON(sha, appUser, appUser, botNoreply, botNoreply) }

// compareJSON is a comparison counting total commits and listing these.
func compareJSON(total int, commits ...string) string {
	return fmt.Sprintf(`{"status":"ahead","ahead_by":%d,"total_commits":%d,"commits":[%s],"files":[]}`, total, total, strings.Join(commits, ","))
}

// The commits the branch agent/fix-1 and the default branch point at.
const (
	tipSHA  = "1111111111111111111111111111111111111111"
	baseSHA = "2222222222222222222222222222222222222222"
)

// branchJSON is GET /repos/{r}/branches/{name}'s answer.
func branchJSON(name, sha string) string {
	return fmt.Sprintf(`{"name":%q,"commit":{"sha":%q},"protected":false}`, name, sha)
}

// branchRoutesOn answers acme/widgets (default branch def, owner acme),
// the branch agent/fix-1 at tipSHA and def at baseSHA, the open pull
// requests from agent/fix-1 (a listing by head) with from and into it
// (by base) with into, and the comparison of the two commits, and of
// nothing by name, with compare.
func branchRoutesOn(f *fakeGitHub, def string, from, into []string, compare string) {
	f.route("GET /repos/acme/widgets", 200, fmt.Sprintf(`{"full_name":"acme/widgets","default_branch":%q,"owner":{"login":"acme","type":"Organization"}}`, def))
	f.route("GET /repos/acme/widgets/branches/agent/fix-1", 200, branchJSON("agent/fix-1", tipSHA))
	f.route("GET /repos/acme/widgets/branches/"+def, 200, branchJSON(def, baseSHA))
	f.routeFunc("GET /repos/acme/widgets/pulls", func(r recorded) reply {
		list := from
		if r.Query.Get("base") != "" {
			list = into
		}
		return reply{Status: 200, Body: "[" + strings.Join(list, ",") + "]"}
	})
	f.route("GET /repos/acme/widgets/compare/"+baseSHA+"..."+tipSHA, 200, compare)
}

// branchRoutesWith is branchRoutesOn with the default branch main.
func branchRoutesWith(f *fakeGitHub, from, into []string, compare string) {
	branchRoutesOn(f, "main", from, into, compare)
}

// branchRoutes is branchRoutesWith with no open pull requests.
func branchRoutes(f *fakeGitHub, compare string) {
	branchRoutesWith(f, nil, nil, compare)
}

// Why this test exists: whether the app's bot made a commit is what a
// branch deletion turns on, by what git records. A commit counts only
// when both its author and its committer are the bot: each resolved by
// GitHub to the bot user (type Bot, its login), or, where GitHub
// resolved no user, the bot's no-reply address exactly. A person's
// commit, one the bot authored and someone else committed (a merge on
// GitHub's site is committed by web-flow) or the reverse, another app's
// bot's, a user that bears the bot's login but is no Bot, or an
// unresolved email that is not the address, must not count, or an
// agent could delete someone else's work. An address not known must
// match nothing, not even an empty email.
func TestCommitByBot(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	conn, err := readConnection(testConn())
	if err != nil {
		t.Fatal(err)
	}
	c := &call{g: g, conn: conn}
	bot := &ghUser{Login: "kivali-acme[bot]", Type: "Bot"}
	person := &ghUser{Login: "dana", Type: "User"}
	webFlow := &ghUser{Login: "web-flow", Type: "User"}
	other := &ghUser{Login: "other-app[bot]", Type: "Bot"}
	posing := &ghUser{Login: "kivali-acme[bot]", Type: "User"}
	commit := func(author, committer *ghUser, authorEmail, committerEmail string) ghCommit {
		var cm ghCommit
		cm.Author, cm.Committer = author, committer
		cm.Commit.Author.Email, cm.Commit.Committer.Email = authorEmail, committerEmail
		return cm
	}
	for _, tc := range []struct {
		name    string
		cm      ghCommit
		noreply string
		want    bool
	}{
		{"the bot's, resolved", commit(bot, bot, "", ""), "", true},
		{"the bot's, by its address", commit(nil, nil, botNoreply, botNoreply), botNoreply, true},
		{"resolved author, committer at the address", commit(bot, nil, "", botNoreply), botNoreply, true},
		{"a user with no login is unresolved", commit(&ghUser{Type: "Bot"}, bot, botNoreply, ""), botNoreply, true},
		{"a person's", commit(person, person, "dana@example.com", "dana@example.com"), botNoreply, false},
		{"authored by the bot, committed by a person", commit(bot, person, botNoreply, "dana@example.com"), botNoreply, false},
		{"committed on GitHub's site", commit(bot, webFlow, botNoreply, "noreply@github.com"), botNoreply, false},
		{"authored by a person, committed by the bot", commit(person, bot, "dana@example.com", botNoreply), botNoreply, false},
		{"another app's bot", commit(other, other, "", ""), botNoreply, false},
		{"the bot's login on a user that is no Bot", commit(posing, bot, "", ""), botNoreply, false},
		{"unresolved, another address", commit(nil, nil, "dana@example.com", botNoreply), botNoreply, false},
		{"unresolved, the address in other capitals", commit(nil, nil, strings.ToUpper(botNoreply), botNoreply), botNoreply, false},
		{"unresolved, the address not known", commit(nil, nil, "", ""), "", false},
		{"unresolved, the address not known, an address given", commit(nil, nil, botNoreply, botNoreply), "", false},
	} {
		got, err := c.commitByBot(context.Background(), tc.cm, tc.noreply)
		if err != nil || got != tc.want {
			t.Errorf("%s: %v, %v", tc.name, got, err)
		}
	}
}

// Why this test exists: delete_branch is the one way an agent deletes
// anything on GitHub. It deletes a branch only when every commit the
// branch has beyond the default branch is the app's bot's, or it has
// none; never the default branch, and not while any open pull request
// in the repository has it as its head or its base (deleting a base
// closes or retargets the pull requests into it). A branch past any of
// these must be refused with nothing deleted and no token that writes
// minted: the default branch before anything else is read; an open
// pull request's head or base, whoever opened it; a person's commit on
// the branch; a comparison that lists fewer commits than it counts; and
// one GitHub cannot make, such as for a branch with no history in
// common with the default branch.
func TestDeleteBranchRefusesABranchNotAllTheBots(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	writer := agent(true, "acme/*")
	del := func() callResult {
		return g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
	}
	branchRoutes(f, compareJSON(0))
	for _, branch := range []string{"main", "Main"} {
		r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": branch})
		if r.refusal == nil || r.text != branch+" is the default branch of acme/widgets, which your access does not let you delete; ask the owner." ||
			r.refusal.Reason != "the branch is the repository's default branch" {
			t.Errorf("%s: %+v", branch, r)
		}
	}
	for _, q := range f.api() {
		if q.Path != "/repos/acme/widgets" {
			t.Errorf("the default branch's pull requests or commits were read: %s", q.Path)
		}
	}

	for _, open := range []string{appUser, personUser} {
		branchRoutesWith(f, []string{pullJSON(4, open, "open", "agent/fix-1", false)}, nil, compareJSON(0))
		r := del()
		if r.refusal == nil || r.text != "agent/fix-1 is the head of pull request #4, which is open; a branch is deleted only once no open pull request has it as its head or its base; ask the owner." ||
			r.refusal.Reason != "pull request #4 is open from the branch" {
			t.Errorf("open by %s: %+v", open, r)
		}
	}
	branchRoutesWith(f, nil, []string{pullIntoJSON(12, "agent/fix-1")}, compareJSON(0))
	r := del()
	if r.refusal == nil || r.text != "agent/fix-1 is the base of pull request #12, which is open; a branch is deleted only once no open pull request has it as its head or its base; ask the owner." ||
		r.refusal.Reason != "pull request #12 is open into the branch" {
		t.Errorf("open into it: %+v", r)
	}

	branchRoutes(f, compareJSON(2, botCommit("aaa"), commitJSON("bbb111222333444", personUser, personUser, "dana@example.com", "dana@example.com")))
	r = del()
	if r.refusal == nil || r.text != "agent/fix-1 has a commit beyond main that the GitHub App's bot did not both author and commit (bbb111222333); your access lets you delete only a branch whose own commits are all the bot's; ask the owner." ||
		r.refusal.Reason != "commit bbb111222333 of the branch is not the app's bot's" {
		t.Errorf("a person's commit: %+v", r)
	}

	var many []string
	for i := range 250 {
		many = append(many, botCommit(fmt.Sprintf("c%d", i)))
	}
	branchRoutes(f, fmt.Sprintf(`{"status":"ahead","ahead_by":300,"total_commits":300,"commits":[%s]}`, strings.Join(many, ",")))
	r = del()
	if r.refusal == nil || r.text != "agent/fix-1 has too many commits beyond main to check (300), so it is not deleted; ask the owner." ||
		r.refusal.Reason != "the branch has too many commits to check" {
		t.Errorf("cut: %+v", r)
	}

	branchRoutes(f, compareJSON(0))
	f.route("GET /repos/acme/widgets/compare/"+baseSHA+"..."+tipSHA, 404, `{"message":"No common ancestor between main and agent/fix-1."}`)
	r = del()
	if r.refusal == nil || r.text != "GitHub cannot compare agent/fix-1 with main (HTTP 404: No common ancestor between main and agent/fix-1), so it is not deleted; ask the owner." ||
		r.refusal.Reason != "the branch cannot be compared with the default branch" {
		t.Errorf("no common history: %+v", r)
	}

	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
	if w := writesMinted(f); len(w) != 0 {
		t.Errorf("minted tokens that write %v", w)
	}
}

// Why this test exists: a bare name in a comparison is resolved by git's
// rules, a tag before a branch, and github_push may create any tag. A
// tag named like the branch (or like the default branch) and pointing
// at the default branch's commit would compare as nothing beyond it, a
// fully merged branch, and the branch, with someone's commits on it,
// would be deleted. Both ends must be the branches' own commits, read
// from the branches endpoint, which names branches alone, and compared
// by commit; no comparison by name is asked for.
func TestDeleteBranchComparesTheBranchNotATagOfItsName(t *testing.T) {
	g, f := newTest(t)
	branchRoutes(f, compareJSON(1, commitJSON("ddd444555666777", personUser, personUser, "dana@example.com", "dana@example.com")))
	// What GitHub would answer by name, a tag agent/fix-1 at main's
	// commit taking the branch's place.
	f.route("GET /repos/acme/widgets/compare/main...agent/fix-1", 200, compareJSON(0))
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
	if r.refusal == nil || r.refusal.Reason != "commit ddd444555666 of the branch is not the app's bot's" {
		t.Errorf("%+v", r)
	}
	for _, q := range f.api() {
		if q.Path == "/repos/acme/widgets/compare/main...agent/fix-1" {
			t.Error("the branch was compared by name")
		}
	}
	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}

// Why this test exists: the repository's default branch is whatever
// GitHub names, not main. It must be the one refused, and the one the
// branch is compared with; a branch named main in such a repository is
// an ordinary branch.
func TestDeleteBranchHoldsToTheRepositorysOwnDefaultBranch(t *testing.T) {
	g, f := newTest(t)
	branchRoutesOn(f, "trunk", nil, nil, compareJSON(0))
	writer := agent(true, "acme/*")
	r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "trunk"})
	if r.refusal == nil || r.refusal.Reason != "the branch is the repository's default branch" {
		t.Errorf("trunk: %+v", r)
	}
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	if r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"}); r.isError {
		t.Fatalf("agent/fix-1: %+v", r)
	}
	var tips []string
	for _, q := range f.api() {
		if strings.HasPrefix(q.Path, "/repos/acme/widgets/branches/") {
			tips = append(tips, strings.TrimPrefix(q.Path, "/repos/acme/widgets/branches/"))
		}
	}
	if want := []string{"agent/fix-1", "trunk", "agent/fix-1"}; !slices.Equal(tips, want) {
		t.Errorf("branches read %v, want %v", tips, want)
	}
}

// Why this test exists: only a branch can be deleted, by a name git
// takes for one. HEAD, a segment git refuses, a name the repository has
// no branch by (a tag of that name included) and a name GitHub answers
// for with another branch (a renamed one) are not deleted; and a branch
// that moves between its comparison and the deletion is not deleted
// either, since the commit compared is no longer its tip.
func TestDeleteBranchDeletesOnlyTheBranchItChecked(t *testing.T) {
	g, f := newTest(t)
	writer := agent(true, "acme/*")
	for _, branch := range []string{"HEAD", "agent/.x", "agent.lock/x"} {
		if r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": branch}); r.refusal == nil || r.text != "branch is not a branch name." {
			t.Errorf("%s: %+v", branch, r)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("GitHub took %d requests for names git refuses", n)
	}

	branchRoutes(f, compareJSON(0))
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	for name, answer := range map[string]reply{
		"no such branch": {Status: 404, Body: `{"message":"Branch not found"}`},
		"renamed":        {Status: 200, Body: branchJSON("agent/fix-2", tipSHA)},
	} {
		f.routeFunc("GET /repos/acme/widgets/branches/agent/fix-1", func(recorded) reply { return answer })
		r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
		if !r.isError || r.refusal != nil || r.text != "acme/widgets has no branch named agent/fix-1." {
			t.Errorf("%s: %+v", name, r)
		}
	}

	reads := 0
	f.routeFunc("GET /repos/acme/widgets/branches/agent/fix-1", func(recorded) reply {
		reads++
		if reads == 1 {
			return reply{Status: 200, Body: branchJSON("agent/fix-1", tipSHA)}
		}
		return reply{Status: 200, Body: branchJSON("agent/fix-1", "3333333333333333333333333333333333333333")}
	})
	r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
	if !r.isError || r.text != "agent/fix-1 changed while it was checked, so it is not deleted; try again." || reads != 2 {
		t.Errorf("moved: %+v, %d reads", r, reads)
	}
	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}

// Why this test exists: once the rule holds, delete_branch deletes the
// branch's ref and nothing else, with a token narrowed to the
// repository that may write its contents, minted only then. A fully
// merged branch (no commits beyond the default branch) and one of the
// bot's commits, as GitHub resolves them or by the bot's no-reply
// address where it resolves no user, each qualify. The branch's commit
// must be read as a branch, the pull requests asked for open, by the
// repository owner's head (owner:branch) and by base, the two commits
// compared, and the branch read again before it is deleted, every read
// with a token that only reads; the bot's user, for its address, is
// read once per connection with an installation token (the app's JWT
// reaches only /app); and the owner's audit row must say what was
// deleted.
func TestDeleteBranchDeletesABranchOfTheBotsCommits(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	f.route("GET /users/kivali-acme[bot]", 200, botUserJSON)
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	read := map[string]string{"metadata": "read", "contents": "read", "pull_requests": "read"}
	paths := []string{
		"GET /repos/acme/widgets", "GET /repos/acme/widgets/branches/agent/fix-1", "GET /repos/acme/widgets/pulls", "GET /repos/acme/widgets/pulls",
		"GET /repos/acme/widgets/branches/main", "GET /repos/acme/widgets/compare/" + baseSHA + "..." + tipSHA, "GET /repos/acme/widgets/branches/agent/fix-1",
		"DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1",
	}
	for _, c := range []struct {
		name    string
		compare string
		users   int
	}{
		{"fully merged", compareJSON(0), 0},
		{"the bot's commits", compareJSON(2, botCommit("aaa"), botCommit("bbb")), 0},
		{"the bot's address", compareJSON(2, commitJSON("aaa", "null", "null", botNoreply, botNoreply), botCommit("bbb")), 1},
		{"the bot's address again", compareJSON(1, commitJSON("ccc", "null", appUser, botNoreply, "")), 0},
	} {
		// A pull request GitHub answers into another branch all the
		// same does not hold this one.
		branchRoutesWith(f, nil, []string{pullIntoJSON(12, "main")}, c.compare)
		before := len(f.api())
		r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
		if r.isError || r.text != "Deleted the branch agent/fix-1 of acme/widgets." || r.audit != "deleted the branch agent/fix-1 of acme/widgets" {
			t.Fatalf("%s: %+v", c.name, r)
		}
		var users, rest []recorded
		for _, q := range f.api()[before:] {
			switch q.Path {
			case "/users/kivali-acme[bot]":
				users = append(users, q)
			case "/app":
			default:
				rest = append(rest, q)
			}
		}
		if len(users) != c.users {
			t.Errorf("%s: GET /users %d times, want %d", c.name, len(users), c.users)
		}
		for _, q := range users {
			if q.Grant == nil || !mapsEqual(q.Grant.Permissions, map[string]string{"metadata": "read"}) || !slices.Equal(q.Grant.Repositories, []string{"widgets"}) {
				t.Errorf("%s: GET /users with %+v", c.name, q.Grant)
			}
		}
		var got []string
		for _, q := range rest {
			got = append(got, q.Method+" "+q.Path)
		}
		if !slices.Equal(got, paths) {
			t.Fatalf("%s: requests %v", c.name, got)
		}
		if q := rest[2].Query; q.Get("head") != "acme:agent/fix-1" || q.Get("state") != "open" {
			t.Errorf("%s: query %v", c.name, q)
		}
		if q := rest[3].Query; q.Get("base") != "agent/fix-1" || q.Get("state") != "open" || q.Get("head") != "" {
			t.Errorf("%s: query %v", c.name, q)
		}
		for i, q := range rest {
			want := read
			if q.Method == "DELETE" {
				want = map[string]string{"metadata": "read", "contents": "write"}
			}
			if q.Grant == nil || !mapsEqual(q.Grant.Permissions, want) || !slices.Equal(q.Grant.Repositories, []string{"widgets"}) {
				t.Errorf("%s: request %d %s %s: grant %+v", c.name, i, q.Method, q.Path, q.Grant)
			}
		}
	}

	// A branch GitHub has no longer is GitHub's failure, in its words.
	branchRoutes(f, compareJSON(0))
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 422, `{"message":"Reference does not exist"}`)
	r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
	if !r.isError || r.text != "GitHub answered 422: Reference does not exist" {
		t.Errorf("gone: %+v", r)
	}
	for _, branch := range []any{"", "a..b", "-x", "refs/heads/", 3} {
		if r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": "acme/widgets", "branch": branch}); r.refusal == nil {
			t.Errorf("branch %v: %+v", branch, r)
		}
	}
}

// Why this test exists: a listing reads at most maxListPages pages, and
// a pull request past them is never seen. Every check delete_branch
// makes rests on seeing them all: an open pull request from or into the
// branch on the page after the last one read would be missed and the
// branch deleted under it. A listing whose last page read is full is
// taken as cut, and the branch is refused, with nothing deleted and no
// token that writes minted, whichever of the two listings it is.
func TestDeleteBranchRefusesWhenItsPullRequestsCannotAllBeRead(t *testing.T) {
	g, f := newTest(t)
	full := func(item func(i int) string) []string {
		var pages []string
		for p := range maxListPages {
			pages = append(pages, fullPage(p*100+1, item))
		}
		return pages
	}
	// GitHub's filter aside, every pull request on the full pages is
	// another branch's; the one past them is this branch's.
	from := append(full(func(i int) string { return pullJSON(i, personUser, "open", "dana/other", false) }),
		"["+pullJSON(5000, personUser, "open", "agent/fix-1", false)+"]")
	into := append(full(func(i int) string { return pullIntoJSON(i, "main") }), "["+pullIntoJSON(5000, "agent/fix-1")+"]")
	branchRoutes(f, compareJSON(0))
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	for name, pages := range map[string][2][]string{
		"from the branch": {from, nil},
		"into the branch": {nil, into},
	} {
		f.routeFunc("GET /repos/acme/widgets/pulls", func(r recorded) reply {
			if r.Query.Get("base") != "" {
				return pageRoute(pages[1]...)(r)
			}
			return pageRoute(pages[0]...)(r)
		})
		before := len(f.api())
		r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
		if r.refusal == nil || r.text != "The pull requests of agent/fix-1 in acme/widgets could not all be read, so it is not deleted; ask the owner." ||
			r.refusal.Reason != "the branch's pull requests could not all be read" {
			t.Errorf("%s: %+v", name, r)
		}
		read := 0
		for _, q := range f.api()[before:] {
			if q.Path == "/repos/acme/widgets/pulls" {
				read++
			}
		}
		want := maxListPages
		if name == "into the branch" {
			want++
		}
		if read != want {
			t.Errorf("%s: %d pages read, want %d", name, read, want)
		}
	}
	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
	if w := writesMinted(f); len(w) != 0 {
		t.Errorf("minted tokens that write %v", w)
	}
}

// Why this test exists: GitHub names a repository's owner as it is, in
// its own capitals, and for a transferred repository the new owner
// (GitHub answers the old name with the repository where it is now).
// The open pull requests from the branch must be asked for by GitHub's
// owner: by the agent's spelling, a listing by the old owner finds
// none of the branch's pull requests, and an open one would go unseen.
func TestDeleteBranchAsksForPullRequestsByGitHubsOwner(t *testing.T) {
	for _, c := range []struct {
		name, arg, full, owner string
	}{
		{"capitals", "ACME/widgets", "acme/widgets", "acme"},
		{"transferred", "acme/widgets", "newco/widgets", "newco"},
	} {
		g, f := newTest(t)
		f.route("GET /repos/"+c.arg, 200, fmt.Sprintf(`{"full_name":%q,"default_branch":"main","owner":{"login":%q,"type":"Organization"}}`, c.full, c.owner))
		f.route("GET /repos/"+c.arg+"/branches/agent/fix-1", 200, branchJSON("agent/fix-1", tipSHA))
		f.route("GET /repos/"+c.arg+"/branches/main", 200, branchJSON("main", baseSHA))
		f.route("GET /repos/"+c.arg+"/pulls", 200, "[]")
		f.route("GET /repos/"+c.arg+"/compare/"+baseSHA+"..."+tipSHA, 200, compareJSON(0))
		f.route("DELETE /repos/"+c.arg+"/git/refs/heads/agent/fix-1", 204, "")
		r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": c.arg, "branch": "agent/fix-1"})
		if r.isError {
			t.Errorf("%s: %+v", c.name, r)
		}
		var heads []string
		for _, q := range f.api() {
			if h := q.Query.Get("head"); h != "" {
				heads = append(heads, h)
			}
		}
		if want := []string{c.owner + ":agent/fix-1"}; !slices.Equal(heads, want) {
			t.Errorf("%s: head %v, want %v", c.name, heads, want)
		}
	}
}

// Why this test exists: a connection set up before the app's slug was
// kept holds none, and without it no author can be told to be the
// app's, so every close and deletion would be refused. Its slug is read
// from GitHub (GET /app, with the app's JWT, which an installation
// token cannot use) once a call however many users the call checks; a
// connection that keeps its slug never asks for the bot it keeps.
// Where GitHub names no slug either, the call is refused as a
// connection to set up again.
func TestIsAppBotReadsTheSlugFromGitHubWhenTheConnectionKeepsNone(t *testing.T) {
	g, f := newTest(t)
	conn := testConn()
	delete(conn.Fields, FieldSlug)
	f.route("GET /app", 200, appInfoJSON)
	f.route("GET /repos/acme/widgets/issues/7", 200, issueJSON(7, appUser, "open", false))
	f.route("PATCH /repos/acme/widgets/issues/7", 200, issueJSON(7, appUser, "closed", false))
	writer := agent(true, "acme/*")
	if r := g.callConn(t, writer, conn, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7}); r.isError {
		t.Fatalf("%+v", r)
	}
	branchRoutes(f, compareJSON(3, botCommit("aaa"), botCommit("bbb"), botCommit("ccc")))
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	if r := g.callConn(t, writer, conn, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"}); r.isError {
		t.Fatalf("%+v", r)
	}
	apps := appReads(f)
	if len(apps) != 2 {
		t.Fatalf("GET /app %d times over two calls", len(apps))
	}
	for _, q := range apps {
		if q.JWT == nil || q.JWT.ISS != testAppID {
			t.Errorf("GET /app without the app's JWT: %+v", q)
		}
	}

	// The connection's slug, when it keeps one, is the one used.
	before := len(appReads(f))
	if r := g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7}); r.isError {
		t.Fatalf("%+v", r)
	}
	if len(appReads(f)) != before {
		t.Error("a connection that keeps its slug asked GitHub for it")
	}

	f.route("GET /app", 200, `{"id":1234,"name":"Kivali acme"}`)
	r := g.callConn(t, writer, conn, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7})
	if r.refusal == nil || r.text != "GitHub's connection is not set up; ask the owner to set it up again." || r.refusal.Reason != "GitHub names no slug for the app" {
		t.Errorf("no slug: %+v", r)
	}
}
