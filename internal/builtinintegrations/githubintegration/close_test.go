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

// pullJSON is a pull request GitHub answers: its author, state, head
// branch and the repository that branch is in, merged when merged.
func pullJSON(n int, user, state, head, headRepo string, merged bool) string {
	mergedAt := "null"
	if merged {
		mergedAt = `"2026-10-20T00:00:00Z"`
	}
	repo := "null"
	if headRepo != "" {
		repo = fmt.Sprintf(`{"full_name":%q}`, headRepo)
	}
	return fmt.Sprintf(`{"number":%d,"title":"t","state":%q,"user":%s,"merged_at":%s,"head":{"ref":%q,"repo":%s},"base":{"ref":"main"},"html_url":"https://github.com/acme/widgets/pull/%d"}`,
		n, state, user, mergedAt, head, repo, n)
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

// Why this test exists: "opened by the app" is the whole of the
// same-app rule, and every close and branch deletion turns on it. Only
// the app's own bot user counts: the login <slug>[bot] of type Bot. A
// person, another app's bot, a user of another type that bears the
// login, or a login differing in its slug must not pass, or an agent
// could close or delete what someone else made. A user of another type
// is never the app and costs no request; a bot that is not the kept
// slug's is checked against GitHub's slug for the app, read once a
// call however many authors it checks.
func TestOpenedByAppIsTheAppsBotUserAlone(t *testing.T) {
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
		got, err := c.openedByApp(context.Background(), tc.user)
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
		if _, err := c.openedByApp(context.Background(), u); err != nil {
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
func TestOpenedByAppKnowsARenamedApp(t *testing.T) {
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
	f.route("GET /repos/acme/widgets/pulls/5", 200, pullJSON(5, appUser, "closed", "agent/x", "acme/widgets", true))
	r := g.callT(t, writer, "close_pull_request", map[string]any{"repository": "acme/widgets", "number": 5, "comment": "Closing."})
	if !r.isError || r.refusal != nil || r.text != "Pull request acme/widgets#5 is merged, and a merged pull request cannot be closed." {
		t.Errorf("merged: %+v", r)
	}
	f.route("GET /repos/acme/widgets/pulls/6", 200, pullJSON(6, appUser, "closed", "agent/y", "acme/widgets", false))
	r = g.callT(t, writer, "close_pull_request", map[string]any{"repository": "acme/widgets", "number": 6})
	if !r.isError || r.refusal != nil || r.text != "Pull request acme/widgets#6 is closed already." {
		t.Errorf("closed: %+v", r)
	}
	for _, user := range []string{personUser, otherAppUser} {
		f.route("GET /repos/acme/widgets/pulls/7", 200, pullJSON(7, user, "open", "dana/x", "acme/widgets", false))
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

	f.route("GET /repos/acme/widgets/pulls/8", 200, pullJSON(8, appUser, "open", "agent/z", "acme/widgets", false))
	f.route("POST /repos/acme/widgets/issues/8/comments", 201, `{"html_url":"https://github.com/acme/widgets/pull/8#issuecomment-2"}`)
	f.route("PATCH /repos/acme/widgets/pulls/8", 200, pullJSON(8, appUser, "closed", "agent/z", "acme/widgets", false))
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

// branchRoutes answers acme/widgets (default branch main, owner acme),
// its pull requests from agent/fix-1 with from, and its open ones into
// agent/fix-1 with none.
func branchRoutes(f *fakeGitHub, from ...string) {
	branchRoutesInto(f, nil, from...)
}

// branchRoutesInto is branchRoutes, answering the open pull requests
// into agent/fix-1 (a listing by base) with into.
func branchRoutesInto(f *fakeGitHub, into []string, from ...string) {
	f.route("GET /repos/acme/widgets", 200, `{"full_name":"acme/widgets","default_branch":"main","owner":{"login":"acme","type":"Organization"}}`)
	f.routeFunc("GET /repos/acme/widgets/pulls", func(r recorded) reply {
		list := from
		if r.Query.Get("base") != "" {
			list = into
		}
		return reply{Status: 200, Body: "[" + strings.Join(list, ",") + "]"}
	})
}

// Why this test exists: delete_branch is the one way an agent deletes
// anything on GitHub. It deletes only the head branch, in the
// repository itself, of a closed or merged pull request the app opened,
// never the default branch, and not while any open pull request in the
// repository has it as its head or its base (deleting a base closes or
// retargets the pull requests into it). A branch past any of these must
// be refused with nothing deleted and no token that writes minted: the
// default branch before its pull requests are read; an open pull
// request's head or base, whoever opened it; a branch only people or
// other apps opened pull requests from, or only a fork's branch of the
// same name, or none at all.
func TestDeleteBranchRefusesWhatTheAppsPullRequestsDidNotLeave(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	writer := agent(true, "acme/*")
	branchRoutes(f)
	for _, branch := range []string{"main", "Main"} {
		r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": branch})
		if r.refusal == nil || r.text != branch+" is the default branch of acme/widgets, which your access does not let you delete; ask the owner." ||
			r.refusal.Reason != "the branch is the repository's default branch" {
			t.Errorf("%s: %+v", branch, r)
		}
	}
	for _, q := range f.api() {
		if q.Path != "/repos/acme/widgets" {
			t.Errorf("the default branch's pull requests were read: %s", q.Path)
		}
	}

	merged := pullJSON(3, appUser, "closed", "agent/fix-1", "acme/widgets", true)
	for _, open := range []string{appUser, personUser} {
		branchRoutes(f, pullJSON(4, open, "open", "agent/fix-1", "acme/widgets", false), merged)
		r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
		if r.refusal == nil || r.text != "agent/fix-1 is the head of pull request #4, which is open; a branch is deleted only once no open pull request has it as its head or its base; ask the owner." ||
			r.refusal.Reason != "pull request #4 is open from the branch" {
			t.Errorf("open by %s: %+v", open, r)
		}
	}
	branchRoutesInto(f, []string{pullIntoJSON(12, "agent/fix-1")}, merged)
	r := g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
	if r.refusal == nil || r.text != "agent/fix-1 is the base of pull request #12, which is open; a branch is deleted only once no open pull request has it as its head or its base; ask the owner." ||
		r.refusal.Reason != "pull request #12 is open into the branch" {
		t.Errorf("open into it: %+v", r)
	}
	var into []recorded
	for _, q := range f.api() {
		if q.Path == "/repos/acme/widgets/pulls" && q.Query.Get("base") != "" {
			into = append(into, q)
		}
	}
	if len(into) == 0 || into[len(into)-1].Query.Get("base") != "agent/fix-1" || into[len(into)-1].Query.Get("state") != "open" {
		t.Errorf("pull requests into the branch asked for as %+v", into)
	}

	for name, pulls := range map[string][]string{
		"no pull requests":   nil,
		"a person's":         {pullJSON(3, personUser, "closed", "agent/fix-1", "acme/widgets", true)},
		"another app's":      {pullJSON(3, otherAppUser, "closed", "agent/fix-1", "acme/widgets", false)},
		"a fork's branch":    {pullJSON(3, appUser, "closed", "agent/fix-1", "acme/widgets-fork", true)},
		"a fork GitHub lost": {pullJSON(3, appUser, "closed", "agent/fix-1", "", true)},
		// GitHub filters by head; a pull request it answers from another
		// branch all the same does not count.
		"another branch's": {pullJSON(3, appUser, "closed", "agent/fix-2", "acme/widgets", true)},
		"a person's, and the app's from a fork": {
			pullJSON(3, personUser, "closed", "agent/fix-1", "acme/widgets", true),
			pullJSON(2, appUser, "closed", "agent/fix-1", "acme/other", true),
		},
	} {
		branchRoutes(f, pulls...)
		r = g.callT(t, writer, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
		if r.refusal == nil || r.text != "Your access lets you delete only the branch of a closed or merged pull request the GitHub App opened; ask the owner." ||
			r.refusal.Reason != "no closed pull request the GitHub App opened came from agent/fix-1" {
			t.Errorf("%s: %+v", name, r)
		}
	}

	if w := writesTaken(f); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
	if w := writesMinted(f); len(w) != 0 {
		t.Errorf("minted tokens that write %v", w)
	}
}

// Why this test exists: once the rule holds, delete_branch deletes the
// branch's ref and nothing else, with a token narrowed to the
// repository that may write its contents, minted only then. The pull
// requests must be asked for by the repository owner's head
// (owner:branch) in every state, or a closed one would be missed or
// another owner's branch read as this one, and those into the branch
// by base, open; and the owner's audit row must say what was deleted.
func TestDeleteBranchDeletesTheHeadOfAClosedPullRequestTheAppOpened(t *testing.T) {
	g, f := newTest(t)
	for _, pr := range []string{
		pullJSON(3, appUser, "closed", "agent/fix-1", "acme/widgets", true),
		pullJSON(3, appUser, "closed", "agent/fix-1", "ACME/Widgets", false),
	} {
		// A pull request GitHub answers into another branch all the
		// same does not hold this one.
		branchRoutesInto(f, []string{pullIntoJSON(12, "main")}, pullJSON(2, personUser, "closed", "agent/fix-1", "acme/widgets", false), pr)
		f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
		before := len(f.api())
		r := g.callT(t, agent(true, "acme/*"), "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"})
		if r.isError || r.text != "Deleted the branch agent/fix-1 of acme/widgets." || r.audit != "deleted the branch agent/fix-1 of acme/widgets" {
			t.Fatalf("%+v", r)
		}
		api := f.api()[before:]
		if len(api) != 4 || api[1].Path != "/repos/acme/widgets/pulls" || api[2].Path != "/repos/acme/widgets/pulls" || api[3].Method != "DELETE" {
			t.Fatalf("requests %+v", api)
		}
		if q := api[1].Query; q.Get("head") != "acme:agent/fix-1" || q.Get("state") != "all" {
			t.Errorf("query %v", q)
		}
		if q := api[2].Query; q.Get("base") != "agent/fix-1" || q.Get("state") != "open" || q.Get("head") != "" {
			t.Errorf("query %v", q)
		}
		for i, want := range []map[string]string{
			{"metadata": "read", "pull_requests": "read"},
			{"metadata": "read", "pull_requests": "read"},
			{"metadata": "read", "pull_requests": "read"},
			{"metadata": "read", "contents": "write"},
		} {
			if q := api[i]; q.Grant == nil || !mapsEqual(q.Grant.Permissions, want) || !slices.Equal(q.Grant.Repositories, []string{"widgets"}) {
				t.Errorf("request %d %s %s: grant %+v", i, q.Method, q.Path, q.Grant)
			}
		}
	}

	// A branch GitHub has no longer is GitHub's failure, in its words.
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
// makes rests on seeing them all: an open pull request from the branch
// on the page after the last one read would be missed and the branch
// deleted under it. A listing whose last page read is full is taken as
// cut, and the branch is refused, with nothing deleted and no token
// that writes minted, whichever of the two listings it is.
func TestDeleteBranchRefusesWhenItsPullRequestsCannotAllBeRead(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /app", 200, appInfoJSON)
	closed := func(i int) string { return pullJSON(i, personUser, "closed", "agent/fix-1", "acme/widgets", false) }
	from := []string{fullPage(1, func(i int) string {
		if i == 1 {
			return pullJSON(1, appUser, "closed", "agent/fix-1", "acme/widgets", true)
		}
		return closed(i)
	})}
	for p := 1; p < maxListPages; p++ {
		from = append(from, fullPage(p*100+1, closed))
	}
	from = append(from, "["+pullJSON(5000, personUser, "open", "agent/fix-1", "acme/widgets", false)+"]")
	var into []string
	for p := range maxListPages + 1 {
		into = append(into, fullPage(p*100+1, func(i int) string { return pullIntoJSON(i, "agent/fix-1") }))
	}
	f.route("GET /repos/acme/widgets", 200, `{"full_name":"acme/widgets","default_branch":"main","owner":{"login":"acme"}}`)
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	for name, pages := range map[string][2][]string{
		"from the branch": {from, nil},
		"into the branch": {{"[" + pullJSON(1, appUser, "closed", "agent/fix-1", "acme/widgets", true) + "]"}, into},
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
// The pull requests from the branch must be asked for by GitHub's
// owner, and their head repository held to GitHub's name for the
// repository: by the agent's spelling, a listing by the old owner
// finds none of the branch's pull requests, and an open one would go
// unseen.
func TestDeleteBranchAsksForPullRequestsByGitHubsOwner(t *testing.T) {
	for _, c := range []struct {
		name, arg, full, owner string
	}{
		{"capitals", "ACME/widgets", "acme/widgets", "acme"},
		{"transferred", "acme/widgets", "newco/widgets", "newco"},
	} {
		g, f := newTest(t)
		f.route("GET /repos/"+c.arg, 200, fmt.Sprintf(`{"full_name":%q,"default_branch":"main","owner":{"login":%q,"type":"Organization"}}`, c.full, c.owner))
		f.routeFunc("GET /repos/"+c.arg+"/pulls", func(r recorded) reply {
			if r.Query.Get("base") != "" {
				return reply{Status: 200, Body: "[]"}
			}
			return reply{Status: 200, Body: "[" + pullJSON(3, appUser, "closed", "agent/fix-1", c.full, true) + "]"}
		})
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
// token cannot use) once a call however many authors the call checks;
// a connection that keeps its slug never asks. Where GitHub names no
// slug either, the call is refused as a connection to set up again.
func TestOpenedByAppReadsTheSlugFromGitHubWhenTheConnectionKeepsNone(t *testing.T) {
	g, f := newTest(t)
	conn := testConn()
	delete(conn.Fields, FieldSlug)
	f.route("GET /app", 200, `{"id":1234,"slug":"kivali-acme","name":"Kivali acme"}`)
	f.route("GET /repos/acme/widgets/issues/7", 200, issueJSON(7, appUser, "open", false))
	f.route("PATCH /repos/acme/widgets/issues/7", 200, issueJSON(7, appUser, "closed", false))
	writer := agent(true, "acme/*")
	if r := g.callConn(t, writer, conn, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7}); r.isError {
		t.Fatalf("%+v", r)
	}
	branchRoutes(f,
		pullJSON(2, personUser, "closed", "agent/fix-1", "acme/widgets", false),
		pullJSON(3, otherAppUser, "closed", "agent/fix-1", "acme/widgets", false),
		pullJSON(4, appUser, "closed", "agent/fix-1", "acme/widgets", true))
	f.route("DELETE /repos/acme/widgets/git/refs/heads/agent/fix-1", 204, "")
	if r := g.callConn(t, writer, conn, "delete_branch", map[string]any{"repository": "acme/widgets", "branch": "agent/fix-1"}); r.isError {
		t.Fatalf("%+v", r)
	}
	var apps []recorded
	for _, q := range f.api() {
		if q.Path == "/app" {
			apps = append(apps, q)
		}
	}
	if len(apps) != 2 {
		t.Fatalf("GET /app %d times over two calls", len(apps))
	}
	for _, q := range apps {
		if q.JWT == nil || q.JWT.ISS != testAppID {
			t.Errorf("GET /app without the app's JWT: %+v", q)
		}
	}

	// The connection's slug, when it keeps one, is the one used.
	before := len(f.api())
	if r := g.callT(t, writer, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7}); r.isError {
		t.Fatalf("%+v", r)
	}
	for _, q := range f.api()[before:] {
		if q.Path == "/app" {
			t.Error("a connection that keeps its slug asked GitHub for it")
		}
	}

	f.route("GET /app", 200, `{"id":1234,"name":"Kivali acme"}`)
	r := g.callConn(t, writer, conn, "close_issue", map[string]any{"repository": "acme/widgets", "number": 7})
	if r.refusal == nil || r.text != "GitHub's connection is not set up; ask the owner to set it up again." || r.refusal.Reason != "GitHub names no slug for the app" {
		t.Errorf("no slug: %+v", r)
	}
}
