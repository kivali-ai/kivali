package githubintegration

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/gitrepo"
)

const repoJSON = `{"full_name":"acme/widgets","description":"The widgets","default_branch":"main","private":true,"html_url":"https://github.com/acme/widgets","updated_at":"2026-10-01T00:00:00Z"}`

// Why this test exists: the owner gives an agent repositories as
// owner/name or patterns, by the rules the Git integration reads them
// by, and the page and the docs describe those rules once for both. A
// built-in that matched differently would let an agent read or write a
// repository its access does not name, or refuse one it does. Each
// case is held to gitrepo.Match itself; a refused call reaches GitHub
// not even for a token.
func TestAccessHoldsCallsToTheRepositoryPatterns(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets", 200, repoJSON)
	f.route("GET /repos/ACME/Widgets", 200, repoJSON)
	f.route("GET /repos/Acme/Widgets", 200, repoJSON)
	f.route("GET /repos/acme/site.js", 200, repoJSON)
	cases := []struct {
		patterns []string
		repo     string
	}{
		{[]string{"acme/*"}, "acme/widgets"},
		{[]string{"acme/*"}, "ACME/Widgets"},
		{[]string{"acme/*"}, "other/widgets"},
		{[]string{"acme/widgets"}, "acme/widgets2"},
		{[]string{"acme/widgets"}, "Acme/Widgets"},
		{[]string{"*/widgets"}, "acme/widgets"},
		{[]string{"acme/w*s"}, "acme/widgets"},
		{[]string{"acme/w*s"}, "acme/wid"},
		{[]string{"Acme/Widgets.git"}, "acme/widgets"},
		{[]string{"/acme/widgets/"}, "acme/widgets"},
		{[]string{"acme/?idgets"}, "acme/widgets"},
		{[]string{"acme/[w]idgets"}, "acme/widgets"},
		{[]string{`acme/\widgets`}, "acme/widgets"},
		{[]string{"*"}, "acme/widgets"},
		{[]string{"*/*"}, "acme/widgets"},
		{[]string{"acme*"}, "acme/widgets"},
		{[]string{"acme/*.js"}, "acme/site.js"},
		{[]string{"acme/*.js"}, "acme/site.jsx"},
		{[]string{}, "acme/widgets"},
		{[]string{"other/x", "acme/*"}, "acme/widgets"},
	}
	ran := 0
	for _, c := range cases {
		want := gitrepo.Match(c.patterns, strings.ToLower(c.repo))
		r := g.callT(t, agent(true, c.patterns...), "get_repository", map[string]any{"repository": c.repo})
		if want {
			ran++
			if r.isError {
				t.Errorf("%v %s: refused: %s", c.patterns, c.repo, r.text)
			}
		} else if r.refusal == nil || r.text != "Your access to GitHub does not include "+c.repo+"; ask the owner." ||
			r.refusal.Reason != "the access does not include "+c.repo {
			t.Errorf("%v %s: %+v", c.patterns, c.repo, r)
		}
		if got := len(f.api()); got != ran {
			t.Fatalf("%v %s: GitHub took %d calls, want %d", c.patterns, c.repo, got, ran)
		}
	}
	before := len(f.requests())

	// A repository not written as owner/name is refused before any
	// pattern is read: a path of more segments is not a GitHub
	// repository, and one of .. would climb the API's paths.
	for _, repo := range []any{"acme", "acme/widgets/extra", "-R/x", "acme/..", "acme/wid gets", "acme/widgets;id", "acme/widgets\n", "acme/%2e%2e", "", 7, nil} {
		r := g.callT(t, agent(true, "*/*"), "get_repository", map[string]any{"repository": repo})
		if r.refusal == nil || (!strings.Contains(r.text, "is not owner/name") && r.text != "Name the repository as owner/name.") {
			t.Errorf("%q: %+v", repo, r)
		}
	}
	if len(f.requests()) != before {
		t.Errorf("GitHub took a request for a repository that is not owner/name")
	}

	// Spaces and tabs around a repository are a slip a model makes, and
	// the Git integration takes them: the repository is the one inside.
	r := g.callT(t, agent(false, "acme/*"), "get_repository", map[string]any{"repository": " acme/widgets\t"})
	api := f.api()
	if r.isError || api[len(api)-1].Path != "/repos/acme/widgets" {
		t.Errorf("spaces around it: %+v, %s", r, api[len(api)-1].Path)
	}
	want := `{"repository":"acme/widgets","description":"The widgets","default_branch":"main","private":true,"url":"https://github.com/acme/widgets","updated_at":"2026-10-01T00:00:00Z"}`
	if r.text != want {
		t.Errorf("answer %s", r.text)
	}
}

// writeTools are the actions that create something on GitHub.
var writeTools = []string{"create_issue", "comment", "create_pull_request"}

// Why this test exists: an agent whose access does not allow writing
// must not get through by calling a write action by name, and its
// call must not even mint a token, which could write.
func TestWriteActionsNeedWrite(t *testing.T) {
	g, f := newTest(t)
	for _, access := range []map[string]any{
		{"repositories": []any{"acme/*"}, "write": false},
		{"repositories": []any{"acme/*"}},
		{"repositories": []any{"acme/*"}, "write": "true"},
	} {
		for _, name := range writeTools {
			r := g.callT(t, callerWith(access), name, map[string]any{"repository": "acme/widgets", "title": "t", "number": 1, "body": "b", "head": "h", "base": "main"})
			if r.refusal == nil || r.text != "Your access to GitHub does not include creating issues, comments or pull requests; ask the owner." {
				t.Errorf("%v %s: %+v", access, name, r)
			}
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("refused calls made %d requests", n)
	}
}

// Why this test exists: GitHub answers 404 for a repository or an
// issue that does not exist and 422 for a request it will not take
// (a pull request that already exists). The agent must read GitHub's
// reason, marked as an error, or it reports a failed call as done; the
// answer is an error result, not a refusal, since the call reached
// GitHub.
func TestGitHubFailureIsAnErrorWithItsReason(t *testing.T) {
	g, f := newTest(t)
	r := g.callT(t, agent(false, "acme/*"), "list_issues", map[string]any{"repository": "acme/nope"})
	if !r.isError || r.refusal != nil || r.text != "GitHub answered 404: Not Found" {
		t.Errorf("404: %+v", r)
	}
	f.route("POST /repos/acme/widgets/pulls", 422, `{"message":"Validation Failed","errors":[{"resource":"PullRequest","code":"custom","message":"A pull request already exists for acme:agent/x."}]}`)
	r = g.callT(t, agent(true, "acme/*"), "create_pull_request", map[string]any{"repository": "acme/widgets", "title": "t", "head": "agent/x", "base": "main"})
	if !r.isError || r.text != "GitHub answered 422: Validation Failed: A pull request already exists for acme:agent/x." {
		t.Errorf("422: %+v", r)
	}
}

// Why this test exists: GitHub answers 401 when the app's credentials
// no longer work and 403 when the app lacks a permission or is not
// installed on the repository. Neither is the agent's to fix, so its
// words end by telling it to ask the owner; a 403 that is GitHub's rate
// limit says to try again later instead, since the owner cannot mend
// that.
func TestGitHubRefusalsTellTheAgentToAskTheOwner(t *testing.T) {
	g, f := newTest(t)
	for _, c := range []struct {
		status int
		body   string
		header map[string]string
		want   string
	}{
		{401, `{"message":"Bad credentials"}`, nil, "GitHub refused the GitHub App's credentials (HTTP 401): Bad credentials. Ask the owner."},
		{403, `{"message":"Resource not accessible by integration"}`, nil, "GitHub refused this (HTTP 403): Resource not accessible by integration. The GitHub App may lack the permission, or not be installed on the repository; ask the owner."},
		{403, `{"message":"API rate limit exceeded for installation."}`, map[string]string{"X-RateLimit-Remaining": "0"}, "GitHub is limiting the GitHub App's requests for now (HTTP 403): API rate limit exceeded for installation. Try again later."},
		// A secondary limit comes without the headers; its words name it.
		{403, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`, nil, "GitHub is limiting the GitHub App's requests for now (HTTP 403): You have exceeded a secondary rate limit. Please wait a few minutes before you try again. Try again later."},
		{429, `{"message":"Too many requests"}`, nil, "GitHub is limiting the GitHub App's requests for now (HTTP 429): Too many requests. Try again later."},
	} {
		f.routeFunc("GET /repos/acme/widgets/branches", func(recorded) reply { return reply{Status: c.status, Body: c.body, Header: c.header} })
		r := g.callT(t, agent(false, "acme/*"), "list_branches", map[string]any{"repository": "acme/widgets"})
		if !r.isError || r.text != c.want {
			t.Errorf("%d: %+v", c.status, r)
		}
	}

	// The token itself refused: the app's key, the installation, or a
	// repository the installation does not reach.
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"message":"A JSON web token could not be decoded"}`, "GitHub refused the GitHub App's key (HTTP 401): A JSON web token could not be decoded. Ask the owner."},
		{404, `{"message":"Not Found"}`, "GitHub has no installation of the GitHub App that Kivali keeps (HTTP 404): Not Found. Ask the owner."},
		{422, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`, "GitHub refused the GitHub App a token for acme/widgets (HTTP 422): There is at least one repository that does not exist or is not accessible to the parent installation. The app may not be installed on it, or lack the permission; ask the owner."},
	} {
		f.mu.Lock()
		f.mintReply = &reply{Status: c.status, Body: c.body}
		f.mu.Unlock()
		// A built-in of its own, whose cache holds no token yet.
		g := &GitHub{APIBase: f.srv.URL, Clock: f.clk}
		r := g.callT(t, agent(false, "acme/*"), "list_branches", map[string]any{"repository": "acme/widgets", "limit": 3})
		if !r.isError || r.text != c.want {
			t.Errorf("token %d: %+v", c.status, r)
		}
	}
}

// Why this test exists: an issue's thread and a pull request's diff
// can run to megabytes, and an answer that size fills the agent's
// context with one call. The answer keeps the latest 20 comments (read
// from the last pages, since GitHub lists them oldest first), the first
// 20,000 characters of a body and 50,000 of a diff, and says when it
// cut something, so the agent knows there is more.
func TestLongAnswersAreClipped(t *testing.T) {
	g, f := newTest(t)
	const total = 125
	f.route("GET /repos/acme/widgets/issues/7", 200, fmt.Sprintf(`{"number":7,"title":"Long","state":"open","body":%q,"comments":%d,"user":{"login":"u"},"labels":[{"name":"bug"}],"html_url":"https://github.com/acme/widgets/issues/7"}`, strings.Repeat("x", 30000), total))
	f.routeFunc("GET /repos/acme/widgets/issues/7/comments", func(r recorded) reply {
		page, _ := strconv.Atoi(r.Query.Get("page"))
		var out []map[string]any
		for i := (page-1)*100 + 1; i <= min(page*100, total); i++ {
			body := fmt.Sprintf("comment %d", i)
			if i == total {
				body = strings.Repeat("y", 25000)
			}
			out = append(out, map[string]any{"user": map[string]any{"login": "u"}, "body": body})
		}
		b, _ := json.Marshal(out)
		return reply{Status: 200, Body: string(b)}
	})
	f.route("GET /repos/acme/widgets/pulls/8", 200, `{"number":8,"title":"Big","state":"open","body":"short","head":{"ref":"agent/x"},"base":{"ref":"main"}}`)
	reader := agent(false, "acme/*")

	r := g.callT(t, reader, "get_issue", map[string]any{"repository": "acme/widgets", "number": 7})
	if r.isError {
		t.Fatal(r.text)
	}
	var issue struct {
		Body     string `json:"body"`
		Labels   []string
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(r.text), &issue); err != nil {
		t.Fatal(err)
	}
	if len(issue.Body) != 20000 || len(issue.Comments) != 20 || issue.Comments[0].Body != "comment 106" || len(issue.Comments[19].Body) != 20000 {
		t.Errorf("body %d, comments %d, first %q", len(issue.Body), len(issue.Comments), issue.Comments[0].Body)
	}
	for _, w := range []string{"The body is cut to its first 20000 characters.", "Only the latest 20 of its 125 comments are here.", "A comment's body is cut to its first 20000 characters."} {
		if !strings.Contains(issue.Note, w) {
			t.Errorf("note %q lacks %q", issue.Note, w)
		}
	}
	var pages []string
	for _, q := range f.api() {
		if strings.HasSuffix(q.Path, "/comments") {
			pages = append(pages, q.Query.Get("page"))
		}
	}
	if !slices.Equal(pages, []string{"2"}) {
		t.Errorf("comment pages read %v", pages)
	}

	f.routeFunc("GET /repos/acme/widgets/pulls/8", func(r recorded) reply {
		if r.Header.Get("Accept") == mediaDiff {
			return reply{Status: 200, Body: strings.Repeat("d", 60000)}
		}
		return reply{Status: 200, Body: `{"number":8,"title":"Big","state":"open","body":"short","head":{"ref":"agent/x"},"base":{"ref":"main"}}`}
	})
	r = g.callT(t, reader, "get_pull_request", map[string]any{"repository": "acme/widgets", "number": 8, "diff": true})
	var pr struct {
		Body string `json:"body"`
		Diff string `json:"diff"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(r.text), &pr); err != nil {
		t.Fatal(err, r.text)
	}
	if pr.Body != "short" || len(pr.Diff) != 50000 || !strings.Contains(pr.Note, "diff is cut to its first 50000 characters") {
		t.Errorf("body %q, diff %d, note %q", pr.Body, len(pr.Diff), pr.Note)
	}
	// Without diff, the diff is not asked for.
	before := len(f.api())
	if r = g.callT(t, reader, "get_pull_request", map[string]any{"repository": "acme/widgets", "number": 8}); r.isError || strings.Contains(r.text, `"diff"`) {
		t.Errorf("%+v", r)
	}
	if got := len(f.api()) - before; got != 1 {
		t.Errorf("%d requests without diff", got)
	}
	if r = g.callT(t, reader, "get_pull_request", map[string]any{"repository": "acme/widgets", "number": 8, "diff": "yes"}); r.refusal == nil || r.text != "diff is true or false." {
		t.Errorf("%+v", r)
	}
}

// Why this test exists: an issue and a pull request share their
// numbers on GitHub. The built-in asks GitHub which the number is
// first, with a token that only reads, then writes the comment with a
// token narrowed to issues or to pull requests, whichever it is, so a
// comment on an issue never holds pull request write and the reverse.
func TestCommentFindsWhetherTheNumberIsAPullRequest(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/issues/7", 200, `{"number":7,"pull_request":{"url":"https://api.github.com/repos/acme/widgets/pulls/7"}}`)
	f.route("GET /repos/acme/widgets/issues/8", 200, `{"number":8,"pull_request":null}`)
	f.route("POST /repos/acme/widgets/issues/7/comments", 201, `{"html_url":"https://github.com/acme/widgets/pull/7#issuecomment-1"}`)
	f.route("POST /repos/acme/widgets/issues/8/comments", 201, `{"html_url":"https://github.com/acme/widgets/issues/8#issuecomment-2"}`)
	writer := agent(true, "acme/widgets")
	if r := g.callT(t, writer, "comment", map[string]any{"repository": "acme/widgets", "number": 7, "body": "--looks good"}); r.isError || r.text != "https://github.com/acme/widgets/pull/7#issuecomment-1" {
		t.Errorf("pull request: %+v", r)
	}
	if r := g.callT(t, writer, "comment", map[string]any{"repository": "acme/widgets", "number": 8, "body": "On it."}); r.isError || r.text != "https://github.com/acme/widgets/issues/8#issuecomment-2" || r.audit != "created https://github.com/acme/widgets/issues/8#issuecomment-2" {
		t.Errorf("issue: %+v", r)
	}
	api := f.api()
	if len(api) != 4 {
		t.Fatalf("requests %+v", api)
	}
	wantPerms := []map[string]string{
		{"metadata": "read", "issues": "read", "pull_requests": "read"},
		{"metadata": "read", "pull_requests": "write"},
		{"metadata": "read", "issues": "read", "pull_requests": "read"},
		{"metadata": "read", "issues": "write"},
	}
	for i, q := range api {
		if q.Grant == nil || !mapsEqual(q.Grant.Permissions, wantPerms[i]) || !slices.Equal(q.Grant.Repositories, []string{"widgets"}) {
			t.Errorf("request %d %s %s: grant %+v", i, q.Method, q.Path, q.Grant)
		}
	}
	if api[1].Body != `{"body":"--looks good"}` || api[3].Body != `{"body":"On it."}` {
		t.Errorf("bodies %q %q", api[1].Body, api[3].Body)
	}
	if r := g.callT(t, writer, "comment", map[string]any{"repository": "acme/widgets", "number": 8, "body": "  "}); r.refusal == nil || r.text != "Give the body." {
		t.Errorf("blank body: %+v", r)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Why this test exists: create_pull_request is the one action that
// names branches, and the one an agent pairs with a Git push. It opens
// the pull request from a head branch already on GitHub, moving no
// commits, and a branch that is not a branch name (or names another
// owner's fork) is refused before GitHub is reached.
func TestCreatePullRequest(t *testing.T) {
	g, f := newTest(t)
	f.route("POST /repos/acme/widgets/pulls", 201, `{"number":9,"html_url":"https://github.com/acme/widgets/pull/9"}`)
	writer := agent(true, "acme/*")
	r := g.callT(t, writer, "create_pull_request", map[string]any{
		"repository": "acme/widgets", "title": "Fix the build", "head": "agent/fix-1", "base": "main", "body": "Fixes #3.", "draft": true,
	})
	if r.isError || r.text != "https://github.com/acme/widgets/pull/9" || r.audit != "created https://github.com/acme/widgets/pull/9" {
		t.Fatalf("%+v", r)
	}
	api := f.api()
	var body map[string]any
	if len(api) != 1 || json.Unmarshal([]byte(api[0].Body), &body) != nil {
		t.Fatalf("requests %+v", api)
	}
	if body["title"] != "Fix the build" || body["head"] != "agent/fix-1" || body["base"] != "main" || body["body"] != "Fixes #3." || body["draft"] != true {
		t.Errorf("body %v", body)
	}
	if g := api[0].Grant; g == nil || !mapsEqual(g.Permissions, map[string]string{"metadata": "read", "pull_requests": "write"}) {
		t.Errorf("grant %+v", g)
	}
	for _, head := range []any{"-x", "a..b", "a b", "owner:branch", "refs/heads/", "main\n", ".hidden", "x.lock", "", 3} {
		r := g.callT(t, writer, "create_pull_request", map[string]any{"repository": "acme/widgets", "title": "t", "head": head, "base": "main"})
		if r.refusal == nil || !strings.Contains(r.text, "branch") {
			t.Errorf("head %q: %+v", head, r)
		}
	}
	for _, title := range []any{"two\nlines", strings.Repeat("t", 257), "", 5} {
		r := g.callT(t, writer, "create_pull_request", map[string]any{"repository": "acme/widgets", "title": title, "head": "x", "base": "main"})
		if r.refusal == nil {
			t.Errorf("title %q: %+v", title, r)
		}
	}
	if len(f.api()) != 1 {
		t.Error("GitHub was reached for a refused call")
	}
}

// Why this test exists: list_repositories is how an agent learns which
// repositories it may use. It reads every page of what the app's
// installation reaches and keeps only what the patterns match, so an
// agent given acme/w* is not shown acme/api, which its calls would be
// refused; its token reaches the installation but may only read
// metadata.
func TestListRepositoriesKeepsToThePatterns(t *testing.T) {
	g, f := newTest(t)
	f.routeFunc("GET /installation/repositories", func(r recorded) reply {
		var repos []map[string]any
		switch r.Query.Get("page") {
		case "1":
			repos = append(repos,
				map[string]any{"full_name": "acme/widgets", "private": true, "updated_at": "2026-10-01T00:00:00Z", "description": "The widgets\nand more"},
				map[string]any{"full_name": "acme/web", "private": false, "updated_at": "2026-10-02T00:00:00Z"},
				map[string]any{"full_name": "acme/api", "private": true, "updated_at": "2026-10-03T00:00:00Z"})
			for i := len(repos); i < 100; i++ {
				repos = append(repos, map[string]any{"full_name": fmt.Sprintf("acme/x%d", i)})
			}
		case "2":
			repos = append(repos, map[string]any{"full_name": "Other/Tool", "private": false, "updated_at": "2026-10-04T00:00:00Z"},
				map[string]any{"full_name": "other/x", "private": false})
		}
		b, _ := json.Marshal(map[string]any{"total_count": 102, "repositories": repos})
		return reply{Status: 200, Body: string(b)}
	})
	r := g.callT(t, agent(false, "acme/w*", "other/tool", "*/everything"), "list_repositories", nil)
	want := "acme/widgets (private, updated 2026-10-01T00:00:00Z): The widgets\nacme/web (public, updated 2026-10-02T00:00:00Z)\nOther/Tool (public, updated 2026-10-04T00:00:00Z)"
	if r.isError || r.text != want {
		t.Errorf("%v\n%s", r.isError, r.text)
	}
	api := f.api()
	if len(api) != 2 || api[0].Query.Get("per_page") != "100" || api[1].Query.Get("page") != "2" {
		t.Errorf("requests %+v", api)
	}
	if gr := api[0].Grant; gr == nil || len(gr.Repositories) != 0 || !mapsEqual(gr.Permissions, map[string]string{"metadata": "read"}) {
		t.Errorf("grant %+v", gr)
	}
	if r := g.callT(t, agent(false, "nobody/*"), "list_repositories", nil); r.text != "No repositories found." {
		t.Errorf("none: %+v", r)
	}
}

// Why this test exists: list_issues' search runs as a GitHub search
// with repo:<the repository> added, and GitHub ORs repo: qualifiers
// and honours org:, user: and owner:, so a search naming another place
// would hand an agent the issues of any repository the app can see,
// past its access. Such a search is refused before GitHub is reached,
// a label cannot break out of its quotes to add one, and whatever
// GitHub answers, only the repository's own issues reach the agent:
// never a pull request, which the issues endpoint lists too.
func TestListIssuesStaysInTheRepository(t *testing.T) {
	g, f := newTest(t)
	reader := agent(false, "acme/widgets")
	for _, search := range []string{"repo:acme/secret", "crash org:acme", "-repo:acme/widgets repo:x/y", "(repo:x/y)", `"user:someone"`, "OWNER:acme", "bug\tRepo:x/y"} {
		r := g.callT(t, reader, "list_issues", map[string]any{"repository": "acme/widgets", "search": search})
		if r.refusal == nil || r.text != "search cannot name repositories, organizations or users; the repository argument does that." {
			t.Errorf("%q: %+v", search, r)
		}
	}
	if r := g.callT(t, reader, "list_issues", map[string]any{"repository": "acme/widgets", "search": "crash", "labels": []any{`x" repo:acme/secret "`}}); r.refusal == nil {
		t.Errorf("quoted label: %+v", r)
	}
	if r := g.callT(t, reader, "list_issues", map[string]any{"repository": "acme/widgets", "search": "two\nlines"}); r.refusal == nil || r.text != "search is one line." {
		t.Errorf("two lines: %+v", r)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests for refused searches", n)
	}

	items := `[` +
		`{"number":1,"title":"Ours","html_url":"https://github.com/acme/widgets/issues/1","labels":["bug"]},` +
		`{"number":2,"title":"Ours, capitals","html_url":"https://github.com/ACME/Widgets/issues/2"},` +
		`{"number":3,"title":"Theirs","html_url":"https://github.com/acme/secret/issues/3"},` +
		`{"number":4,"title":"Prefix","html_url":"https://github.com/acme/widgets-private/issues/4"},` +
		`{"number":5,"title":"A pull request","html_url":"https://github.com/acme/widgets/pull/5","pull_request":{}}]`
	f.route("GET /search/issues", 200, `{"total_count":5,"items":`+items+`}`)
	r := g.callT(t, reader, "list_issues", map[string]any{"repository": "acme/widgets", "search": "crash in:title", "labels": []any{"needs triage"}, "state": "closed"})
	if r.isError {
		t.Fatal(r.text)
	}
	if want := `[{"number":1,"title":"Ours","state":"","author":"","labels":["bug"],"updated_at":"","url":"https://github.com/acme/widgets/issues/1"},` +
		`{"number":2,"title":"Ours, capitals","state":"","author":"","labels":[],"updated_at":"","url":"https://github.com/ACME/Widgets/issues/2"}]`; r.text != want {
		t.Errorf("kept %s", r.text)
	}
	api := f.api()
	if len(api) != 1 || api[0].Query.Get("q") != `repo:acme/widgets is:issue is:closed label:"needs triage" crash in:title` || api[0].Query.Get("per_page") != "30" {
		t.Errorf("search %+v", api)
	}
	if gr := api[0].Grant; gr == nil || !mapsEqual(gr.Permissions, map[string]string{"metadata": "read", "issues": "read"}) || !slices.Equal(gr.Repositories, []string{"widgets"}) {
		t.Errorf("grant %+v", gr)
	}

	// Without a search, the repository's issues endpoint, which lists
	// pull requests too.
	f.route("GET /repos/acme/widgets/issues", 200, items)
	r = g.callT(t, reader, "list_issues", map[string]any{"repository": "acme/widgets", "labels": []any{"bug", "ui"}, "limit": 1})
	if r.isError || !strings.HasPrefix(r.text, `[{"number":1,`) || strings.Contains(r.text, `"number":2`) {
		t.Errorf("list %+v", r)
	}
	q := f.api()[1].Query
	if q.Get("state") != "open" || q.Get("labels") != "bug,ui" {
		t.Errorf("query %v", q)
	}
	r = g.callT(t, reader, "list_issues", map[string]any{"repository": "acme/widgets", "limit": 50})
	if strings.Contains(r.text, `"number":5`) || strings.Contains(r.text, `"number":3`) {
		t.Errorf("list kept a pull request or another repository's issue: %s", r.text)
	}
	for _, bad := range []map[string]any{{"state": "merged"}, {"limit": 0}, {"limit": 2.5}, {"limit": true}, {"labels": "bug"}, {"labels": []any{"a,b"}}} {
		bad["repository"] = "acme/widgets"
		if r := g.callT(t, reader, "list_issues", bad); r.refusal == nil {
			t.Errorf("%v: %+v", bad, r)
		}
	}
}

// Why this test exists: an answer GitHub gives can be any size, and
// one that fills the agent's context in a call leaves it nothing to
// work with. Every answer is cut at 60,000 characters with a note,
// whatever the action's own limits.
func TestEveryAnswerIsBounded(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets", 200, fmt.Sprintf(`{"full_name":"acme/widgets","description":%q}`, strings.Repeat("r", 70000)))
	r := g.callT(t, agent(false, "acme/*"), "get_repository", map[string]any{"repository": "acme/widgets"})
	note := "\n[The answer is cut to its first 60000 characters.]"
	if r.isError || len([]rune(r.text)) != 60000+len(note) || !strings.HasSuffix(r.text, note) {
		t.Errorf("%v, %d characters", r.isError, len(r.text))
	}
}

// Why this test exists: list_branches and compare call GitHub's REST
// API with paths the built-in builds from the agent's arguments. A path
// built wrong reads another resource or none; these pin both, and what
// each answer keeps (no patches).
func TestBranchesAndCompareCallTheirAPIPaths(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/branches", 200, `[{"name":"main","commit":{"sha":"aaa"}},{"name":"agent/fix-1","commit":{"sha":"bbb"}}]`)
	f.route("GET /repos/acme/widgets/compare/main...agent/fix-1", 200, `{"status":"ahead","ahead_by":1,"behind_by":0,`+
		`"commits":[{"sha":"bbb","commit":{"message":"Fix the build\n\nLonger text","author":{"name":"Dana"}},"author":{"login":"dana"}},`+
		`{"sha":"ccc","commit":{"message":"Unlinked","author":{"name":"Someone"}},"author":null}],`+
		`"files":[{"filename":"a.go","status":"modified","additions":3,"deletions":1,"patch":"@@ a patch"}]}`)
	reader := agent(false, "acme/*")
	if r := g.callT(t, reader, "list_branches", map[string]any{"repository": "acme/widgets", "limit": 5}); r.isError || r.text != "main aaa\nagent/fix-1 bbb" {
		t.Errorf("branches: %+v", r)
	}
	r := g.callT(t, reader, "compare", map[string]any{"repository": "acme/widgets", "base": "main", "head": "agent/fix-1"})
	want := `{"status":"ahead","ahead_by":1,"behind_by":0,"commits":[{"sha":"bbb","title":"Fix the build","author":"dana"},{"sha":"ccc","title":"Unlinked","author":"Someone"}],"files":[{"name":"a.go","status":"modified","additions":3,"deletions":1}]}`
	if r.isError || r.text != want {
		t.Errorf("compare: %+v", r)
	}
	api := f.api()
	if len(api) != 2 || api[0].Path != "/repos/acme/widgets/branches" || api[0].Query.Get("per_page") != "5" || api[1].Path != "/repos/acme/widgets/compare/main...agent/fix-1" {
		t.Errorf("requests %+v", api)
	}
	for _, q := range api {
		if q.Grant == nil || !mapsEqual(q.Grant.Permissions, map[string]string{"metadata": "read", "contents": "read"}) {
			t.Errorf("%s: grant %+v", q.Path, q.Grant)
		}
	}
	if r := g.callT(t, reader, "compare", map[string]any{"repository": "acme/widgets", "base": "main", "head": "../../x"}); r.refusal == nil {
		t.Errorf("climbing head: %+v", r)
	}
}

// Why this test exists: GitHub's pull requests endpoint has no merged
// state; merged is a closed pull request with merged_at. A list asked
// for merged must ask GitHub for closed ones and keep only the merged,
// and every list names a merged one as merged rather than closed.
func TestListPullRequestsReadsMergedFromClosed(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/pulls", 200, `[`+
		`{"number":1,"state":"closed","merged_at":"2026-10-01T00:00:00Z","head":{"ref":"a"},"base":{"ref":"main"},"user":{"login":"dana"}},`+
		`{"number":2,"state":"closed","merged_at":null,"head":{"ref":"b"},"base":{"ref":"main"}}]`)
	reader := agent(false, "acme/*")
	r := g.callT(t, reader, "list_pull_requests", map[string]any{"repository": "acme/widgets", "state": "merged", "base": "main"})
	if r.isError || r.text != `[{"number":1,"title":"","state":"merged","author":"dana","head":"a","base":"main","draft":false,"updated_at":"","url":""}]` {
		t.Errorf("merged: %+v", r)
	}
	q := f.api()[0].Query
	if q.Get("state") != "closed" || q.Get("base") != "main" {
		t.Errorf("query %v", q)
	}
	r = g.callT(t, reader, "list_pull_requests", map[string]any{"repository": "acme/widgets", "state": "closed"})
	if !strings.Contains(r.text, `"number":1,"title":"","state":"merged"`) || !strings.Contains(r.text, `"number":2,"title":"","state":"closed"`) {
		t.Errorf("closed: %s", r.text)
	}
	if gr := f.api()[0].Grant; gr == nil || !mapsEqual(gr.Permissions, map[string]string{"metadata": "read", "pull_requests": "read"}) {
		t.Errorf("grant %+v", gr)
	}
}

// Why this test exists: create_issue writes as the app; its title is
// one line of at most 256 characters, its labels are names, and its
// token may write issues and nothing else. The owner's audit row names
// the issue made (as it does for comment and create_pull_request), or
// the owner cannot tell from the audit what an agent created.
func TestCreateIssue(t *testing.T) {
	g, f := newTest(t)
	f.route("POST /repos/acme/widgets/issues", 201, `{"number":10,"html_url":"https://github.com/acme/widgets/issues/10"}`)
	writer := agent(true, "acme/*")
	r := g.callT(t, writer, "create_issue", map[string]any{"repository": "acme/widgets", "title": "Crash on start", "body": "Steps.", "labels": []any{"bug"}})
	if r.isError || r.text != "https://github.com/acme/widgets/issues/10" || r.audit != "created https://github.com/acme/widgets/issues/10" {
		t.Fatalf("%+v", r)
	}
	api := f.api()
	if len(api) != 1 || api[0].Body != `{"body":"Steps.","labels":["bug"],"title":"Crash on start"}` {
		t.Errorf("requests %+v", api)
	}
	if gr := api[0].Grant; gr == nil || !mapsEqual(gr.Permissions, map[string]string{"metadata": "read", "issues": "write"}) {
		t.Errorf("grant %+v", gr)
	}
	if r := g.callT(t, writer, "create_issue", map[string]any{"repository": "acme/widgets", "title": "x", "body": strings.Repeat("b", 60001)}); r.refusal == nil || r.text != "body is longer than 60000 characters." {
		t.Errorf("long body: %+v", r)
	}
}

func callerWith(access map[string]any) builtinintegrations.Caller {
	return builtinintegrations.Caller{Agent: "dana", Access: access}
}

// commentsRoute answers an issue's comments as GitHub lists them:
// oldest first, 100 a page, "comment <i>" for each of total.
func commentsRoute(total int) func(recorded) reply {
	return func(r recorded) reply {
		page, _ := strconv.Atoi(r.Query.Get("page"))
		out := []map[string]any{}
		for i := (page-1)*100 + 1; i <= min(page*100, total); i++ {
			out = append(out, map[string]any{"user": map[string]any{"login": "u"}, "body": fmt.Sprintf("comment %d", i)})
		}
		b, _ := json.Marshal(out)
		return reply{Status: 200, Body: string(b)}
	}
}

// Why this test exists: GitHub lists an issue's comments oldest first,
// so the latest 20 straddle two pages when the last holds fewer than
// 20. The page before the last is read too, so the agent is given the
// latest 20 in order and not only the few on the last page.
func TestGetIssueReadsThePageBeforeTheLast(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/issues/7", 200, `{"number":7,"comments":105}`)
	f.routeFunc("GET /repos/acme/widgets/issues/7/comments", commentsRoute(105))
	r := g.callT(t, agent(false, "acme/*"), "get_issue", map[string]any{"repository": "acme/widgets", "number": 7})
	var issue struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(r.text), &issue); err != nil {
		t.Fatal(err, r.text)
	}
	if len(issue.Comments) != 20 || issue.Comments[0].Body != "comment 86" || issue.Comments[19].Body != "comment 105" {
		t.Errorf("comments %+v", issue.Comments)
	}
	if !strings.Contains(issue.Note, "latest 20 of its 105 comments") {
		t.Errorf("note %q", issue.Note)
	}
	var pages []string
	for _, q := range f.api() {
		if strings.HasSuffix(q.Path, "/comments") {
			pages = append(pages, q.Query.Get("page"))
		}
	}
	if !slices.Equal(pages, []string{"2", "1"}) {
		t.Errorf("pages read %v", pages)
	}
}

// pageRoute answers a listing page by page: page n is pages[n-1], and
// a page past them is empty.
func pageRoute(pages ...string) func(recorded) reply {
	return func(r recorded) reply {
		n, _ := strconv.Atoi(r.Query.Get("page"))
		if n < 1 || n > len(pages) {
			return reply{Status: 200, Body: `[]`}
		}
		return reply{Status: 200, Body: pages[n-1]}
	}
}

// fullPage is 100 items made by item(i), for i from first.
func fullPage(first int, item func(i int) string) string {
	parts := make([]string, 100)
	for k := range parts {
		parts[k] = item(first + k)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Why this test exists: list_issues leaves out the pull requests the
// issues endpoint lists, and list_pull_requests' merged keeps only the
// merged of the closed. A page full of what is left out must not leave
// the agent with fewer than its limit when more pages hold what it
// asked for; and a listing reads at most 10 pages, so a repository of
// thousands of pull requests never turns one call into hundreds of
// requests.
func TestListsReadFurtherPagesToFillTheirLimit(t *testing.T) {
	g, f := newTest(t)
	reader := agent(false, "acme/*")
	pull := func(i int) string {
		return fmt.Sprintf(`{"number":%d,"html_url":"https://github.com/acme/widgets/pull/%d","pull_request":{}}`, i, i)
	}
	issue := func(i int) string {
		return fmt.Sprintf(`{"number":%d,"html_url":"https://github.com/acme/widgets/issues/%d"}`, i, i)
	}
	f.routeFunc("GET /repos/acme/widgets/issues", pageRoute(fullPage(1, pull), fullPage(101, issue)))
	r := g.callT(t, reader, "list_issues", map[string]any{"repository": "acme/widgets", "limit": 5})
	var got []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(r.text), &got); err != nil || len(got) != 5 || got[0].Number != 101 || got[4].Number != 105 {
		t.Errorf("issues %s", r.text)
	}
	if n := len(f.api()); n != 2 {
		t.Errorf("%d pages read, want 2", n)
	}

	closed := func(i int) string {
		return fmt.Sprintf(`{"number":%d,"state":"closed","merged_at":null}`, i)
	}
	merged := func(i int) string {
		return fmt.Sprintf(`{"number":%d,"state":"closed","merged_at":"2026-10-01T00:00:00Z"}`, i)
	}
	f.routeFunc("GET /repos/acme/widgets/pulls", pageRoute(fullPage(1, closed), fullPage(101, merged)))
	r = g.callT(t, reader, "list_pull_requests", map[string]any{"repository": "acme/widgets", "state": "merged", "limit": 3})
	if err := json.Unmarshal([]byte(r.text), &got); err != nil || len(got) != 3 || got[0].Number != 101 {
		t.Errorf("merged %s", r.text)
	}

	// Nothing but closed, unmerged pull requests: ten pages, then an
	// empty answer.
	var many []string
	for p := range 20 {
		many = append(many, fullPage(p*100+1, closed))
	}
	f.routeFunc("GET /repos/acme/widgets/pulls", pageRoute(many...))
	before := len(f.api())
	r = g.callT(t, reader, "list_pull_requests", map[string]any{"repository": "acme/widgets", "state": "merged"})
	if r.isError || r.text != "[]" {
		t.Errorf("none merged: %+v", r)
	}
	if n := len(f.api()) - before; n != maxListPages {
		t.Errorf("%d pages read, want %d", n, maxListPages)
	}
}

// Why this test exists: GitHub answers 406 for a diff past its own
// size limits. The agent asked for the pull request as well, so it is
// given the pull request with a note that GitHub gives no diff, rather
// than a failed call it cannot get around.
func TestATooLargeDiffAnswersThePullRequest(t *testing.T) {
	g, f := newTest(t)
	f.routeFunc("GET /repos/acme/widgets/pulls/8", func(r recorded) reply {
		if r.Header.Get("Accept") == mediaDiff {
			return reply{Status: 406, Body: `{"message":"Sorry, the diff exceeded the maximum number of lines (20000)."}`}
		}
		return reply{Status: 200, Body: `{"number":8,"title":"Huge","state":"open"}`}
	})
	r := g.callT(t, agent(false, "acme/*"), "get_pull_request", map[string]any{"repository": "acme/widgets", "number": 8, "diff": true})
	if r.isError || strings.Contains(r.text, `"diff"`) || !strings.Contains(r.text, `"title":"Huge"`) ||
		!strings.Contains(r.text, "GitHub gives no diff for this pull request (Sorry, the diff exceeded the maximum number of lines (20000)); compare lists every file changed.") {
		t.Errorf("%+v", r)
	}
}
