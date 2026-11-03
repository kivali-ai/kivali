package githubintegration

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

const headSHA = "0123456789abcdef0123456789abcdef01234567"

// checkRuns is GitHub's answer listing a commit's check runs: a GitHub
// Actions job that failed and another app's check that passed.
const checkRuns = `{"total_count":2,"check_runs":[` +
	`{"id":701,"name":"build","head_sha":"` + headSHA + `","status":"completed","conclusion":"failure","started_at":"2026-11-03T10:00:00Z","completed_at":"2026-11-03T10:04:00Z",` +
	`"html_url":"https://github.com/acme/widgets/runs/701","app":{"slug":"github-actions","name":"GitHub Actions"},"output":{"title":"build failed","summary":"Process completed with exit code 1.","annotations_count":1}},` +
	`{"id":702,"name":"lint","head_sha":"` + headSHA + `","status":"completed","conclusion":"success","html_url":"https://github.com/acme/widgets/runs/702","app":{"slug":"other-ci","name":"Other CI"},"output":{"title":"","summary":""}}]}`

// mintedFor is the permissions each token minted asks for, in order.
func mintedFor(f *fakeGitHub) []string {
	var out []string
	for _, m := range f.mints() {
		var g grant
		_ = json.Unmarshal([]byte(m.Body), &g)
		names := ""
		for _, k := range []string{"actions", "checks", "contents", "pull_requests", "workflows"} {
			if lvl, ok := g.Permissions[k]; ok {
				names += k + "=" + lvl + " "
			}
		}
		out = append(out, strings.TrimSpace(names)+" "+strings.Join(g.Repositories, ","))
	}
	return out
}

// Why this test exists: an agent that pushed needs to know whether CI
// passed. Any agent whose access includes the repository, writer or
// not, may list a commit's checks by a ref or by a pull request (its
// head commit), with a token narrowed to the repository and to reading
// checks (and the pull request, for its head). Each check says whether
// GitHub Actions ran it, since only then is its id a job whose log
// github_get_job_log reads; and the answer counts the outcomes. A
// repository outside the access, and a call naming neither or both
// commits, are refused before GitHub is asked anything.
func TestListChecksByRefOrPullRequest(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/commits/main/check-runs", 200, checkRuns)
	f.route("GET /repos/acme/widgets/pulls/7", 200, `{"head":{"sha":"`+headSHA+`"}}`)
	f.route("GET /repos/acme/widgets/commits/"+headSHA+"/check-runs", 200, checkRuns)
	reader := agent(false, "acme/*")

	r := g.callT(t, reader, "list_checks", map[string]any{"repository": "acme/widgets", "ref": "main"})
	if r.isError {
		t.Fatalf("by ref = %+v", r)
	}
	var out struct {
		Commit string `json:"commit"`
		Total  int    `json:"total"`
		Tally  string `json:"tally"`
		Checks []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			App        string `json:"app"`
			Conclusion string `json:"conclusion"`
			Summary    string `json:"summary"`
			ActionsJob bool   `json:"actions_job"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(r.text), &out); err != nil {
		t.Fatalf("%v: %s", err, r.text)
	}
	if out.Commit != headSHA || out.Total != 2 || out.Tally != "1 failed, 1 succeeded" || len(out.Checks) != 2 ||
		out.Checks[0].ID != 701 || !out.Checks[0].ActionsJob || out.Checks[0].Summary != "Process completed with exit code 1." ||
		out.Checks[1].App != "other-ci" || out.Checks[1].ActionsJob {
		t.Errorf("checks = %s", r.text)
	}
	if q := f.api()[0].Query; q.Get("filter") != "latest" || q.Get("per_page") != "100" {
		t.Errorf("query = %v", q)
	}
	if got := mintedFor(f); len(got) != 1 || got[0] != "checks=read widgets" {
		t.Errorf("mints = %v", got)
	}

	r = g.callT(t, reader, "list_checks", map[string]any{"repository": "acme/widgets", "pull_request": 7})
	if r.isError || !strings.Contains(r.text, `"commit":"`+headSHA+`"`) {
		t.Errorf("by pull request = %+v", r)
	}
	if got := mintedFor(f); len(got) != 2 || got[1] != "pull_requests=read widgets" {
		t.Errorf("mints = %v", got)
	}

	before := len(f.requests())
	for _, c := range []struct {
		args map[string]any
		text string
	}{
		{map[string]any{"repository": "acme/widgets"}, "Give ref (a branch, a tag or a commit id) or pull_request (a number)."},
		{map[string]any{"repository": "acme/widgets", "ref": "main", "pull_request": 7}, "Give ref or pull_request, not both."},
		{map[string]any{"repository": "acme/widgets", "ref": "main..x"}, "ref is a branch, a tag or a commit id."},
		{map[string]any{"repository": "other/widgets", "ref": "main"}, "Your access to GitHub does not include other/widgets; ask the owner."},
	} {
		if r := g.callT(t, reader, "list_checks", c.args); r.refusal == nil || r.text != c.text {
			t.Errorf("%v = %+v", c.args, r)
		}
	}
	if n := len(f.requests()); n != before {
		t.Errorf("refused calls made %d requests", n-before)
	}
}

// Why this test exists: a check another CI ran has no log GitHub keeps;
// what it reported is its output and annotations, which the agent
// reads to find what failed and where. Annotations are asked for only
// when the check has some.
func TestGetCheckRunReadsItsOutputAndAnnotations(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/check-runs/701", 200, `{"id":701,"name":"build","head_sha":"`+headSHA+`","status":"completed","conclusion":"failure",`+
		`"html_url":"https://github.com/acme/widgets/runs/701","app":{"slug":"github-actions"},"output":{"title":"build failed","summary":"1 error","text":"details","annotations_count":1}}`)
	f.route("GET /repos/acme/widgets/check-runs/701/annotations", 200, `[{"path":"src/a.go","start_line":3,"end_line":3,"annotation_level":"failure","title":"vet","message":"unused variable x"}]`)
	r := g.callT(t, agent(false, "acme/*"), "get_check_run", map[string]any{"repository": "acme/widgets", "check_run_id": 701})
	if r.isError || !strings.Contains(r.text, `"annotations":[{"path":"src/a.go","start_line":3,"end_line":3,"level":"failure","title":"vet","message":"unused variable x"}]`) ||
		!strings.Contains(r.text, `"text":"details"`) || !strings.Contains(r.text, `"actions_job":true`) {
		t.Errorf("check = %+v", r)
	}
	f.route("GET /repos/acme/widgets/check-runs/702", 200, `{"id":702,"name":"lint","status":"completed","conclusion":"success","output":{"annotations_count":0}}`)
	before := len(f.api())
	if r := g.callT(t, agent(false, "acme/*"), "get_check_run", map[string]any{"repository": "acme/widgets", "check_run_id": 702}); r.isError || !strings.Contains(r.text, `"annotations":[]`) {
		t.Errorf("no annotations = %+v", r)
	}
	if n := len(f.api()) - before; n != 1 {
		t.Errorf("GitHub took %d requests for a check without annotations", n)
	}
}

// Why this test exists: an agent finds the run its push started by
// branch or commit, and narrows to failures. The filters reach GitHub
// as its query names them, a commit must be a full id, a status one
// GitHub knows, and the token reads Actions alone.
func TestListWorkflowRuns(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/actions/runs", 200, `{"total_count":1,"workflow_runs":[{"id":900,"name":"CI","display_title":"Fix the build\nmore","head_branch":"agent/x","head_sha":"`+headSHA+`",`+
		`"event":"push","status":"completed","conclusion":"failure","run_number":12,"run_attempt":1,"html_url":"https://github.com/acme/widgets/actions/runs/900"}]}`)
	r := g.callT(t, agent(false, "acme/*"), "list_workflow_runs", map[string]any{"repository": "acme/widgets", "branch": "agent/x", "commit": headSHA, "status": "failure", "limit": 5})
	if r.isError || !strings.Contains(r.text, `"id":900,"workflow":"CI","title":"Fix the build"`) || !strings.Contains(r.text, `"conclusion":"failure"`) {
		t.Errorf("runs = %+v", r)
	}
	want := url.Values{"branch": {"agent/x"}, "head_sha": {headSHA}, "status": {"failure"}, "per_page": {"5"}}
	if q := f.api()[0].Query; q.Encode() != want.Encode() {
		t.Errorf("query = %v", q)
	}
	if got := mintedFor(f); len(got) != 1 || got[0] != "actions=read widgets" {
		t.Errorf("mints = %v", got)
	}
	for _, c := range []struct {
		args map[string]any
		text string
	}{
		{map[string]any{"repository": "acme/widgets", "commit": "0123abc"}, "commit is a commit's full id, in lower-case hex."},
		{map[string]any{"repository": "acme/widgets", "status": "broken"}, "status is one of " + strings.Join(runStatuses, ", ") + "."},
	} {
		if r := g.callT(t, agent(false, "acme/*"), "list_workflow_runs", c.args); r.refusal == nil || r.text != c.text {
			t.Errorf("%v = %+v", c.args, r)
		}
	}
}

// Why this test exists: a run's jobs name the job ids
// github_get_job_log takes and the step that failed. The jobs are the
// latest attempt's, so a rerun's do not mix with the first's.
func TestGetWorkflowRunListsItsJobsAndSteps(t *testing.T) {
	g, f := newTest(t)
	f.route("GET /repos/acme/widgets/actions/runs/900", 200, `{"id":900,"name":"CI","head_sha":"`+headSHA+`","status":"completed","conclusion":"failure","run_attempt":2}`)
	f.route("GET /repos/acme/widgets/actions/runs/900/jobs", 200, `{"total_count":1,"jobs":[{"id":701,"name":"build","status":"completed","conclusion":"failure",`+
		`"steps":[{"number":1,"name":"checkout","status":"completed","conclusion":"success"},{"number":2,"name":"make","status":"completed","conclusion":"failure"}]}]}`)
	r := g.callT(t, agent(false, "acme/*"), "get_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 900})
	if r.isError || !strings.Contains(r.text, `"attempt":2`) ||
		!strings.Contains(r.text, `"jobs":[{"id":701,"name":"build","status":"completed","conclusion":"failure","url":"","steps":[{"number":1,"name":"checkout","status":"completed","conclusion":"success"},{"number":2,"name":"make","status":"completed","conclusion":"failure"}]}]`) {
		t.Errorf("run = %+v", r)
	}
	if q := f.api()[1].Query; q.Get("filter") != "latest" {
		t.Errorf("jobs query = %v", q)
	}
}

// Why this test exists: GitHub answers a job's log with a redirect to
// a host that keeps it, at an address signed for a minute. Kivali must
// follow that redirect itself, with none of its credentials (the
// installation token is GitHub's API's alone), and answer with the
// lines GitHub marked as errors and the log's end, timestamps gone,
// since that is where a failure shows. A job GitHub has no log for is
// told in words that say why that may be.
func TestGetJobLogFollowsTheRedirectWithNoCredential(t *testing.T) {
	g, f := newTest(t)
	f.routeFunc("GET /repos/acme/widgets/actions/jobs/701/logs", func(recorded) reply {
		return reply{Status: 303, Header: map[string]string{"Location": f.srv.URL + "/blob/log?sig=s1gned"}}
	})
	// The log starts with a byte order mark, as GitHub's do.
	log := string(rune(0xFEFF)) + "2026-11-03T10:00:00.1234567Z ##[group]Run make\n" +
		"2026-11-03T10:00:01.0000000Z compiling\n" +
		"2026-11-03T10:00:02.0000000Z ##[error]a.go:3: unused variable x\n" +
		"2026-11-03T10:00:03.0000000Z make: *** [build] Error 1\n" +
		"2026-11-03T10:00:04.0000000Z ##[error]Process completed with exit code 2.\n"
	f.route("GET /blob/log", 200, log)
	r := g.callT(t, agent(false, "acme/*"), "get_job_log", map[string]any{"repository": "acme/widgets", "job_id": 701, "lines": 2})
	want := "Job 701's log has 5 lines.\n\n" +
		"Lines marked as errors (2):\nline 3: ##[error]a.go:3: unused variable x\nline 5: ##[error]Process completed with exit code 2.\n\n" +
		"Lines 4 to 5:\nmake: *** [build] Error 1\n##[error]Process completed with exit code 2."
	if r.isError || r.text != want {
		t.Errorf("log = %q", r.text)
	}
	var blob *recorded
	for _, rq := range f.requests() {
		if rq.Path == "/blob/log" {
			blob = &rq
		}
	}
	if blob == nil || blob.Bearer != "" || blob.Header.Get("Authorization") != "" || blob.Query.Get("sig") != "s1gned" {
		t.Errorf("blob request = %+v", blob)
	}
	if got := mintedFor(f); len(got) != 1 || got[0] != "actions=read widgets" {
		t.Errorf("mints = %v", got)
	}

	f.route("GET /repos/acme/widgets/actions/jobs/702/logs", 404, `{"message":"Not Found"}`)
	if r := g.callT(t, agent(false, "acme/*"), "get_job_log", map[string]any{"repository": "acme/widgets", "job_id": 702}); !r.isError ||
		r.text != "GitHub has no log of job 702 in acme/widgets (HTTP 404): the job may still be running, its log may be past GitHub's retention, or it is not a job of this repository." {
		t.Errorf("no log = %+v", r)
	}
	f.routeFunc("GET /repos/acme/widgets/actions/jobs/703/logs", func(recorded) reply {
		return reply{Status: 302, Header: map[string]string{"Location": "http://logs.example.com/703"}}
	})
	if r := g.callT(t, agent(false, "acme/*"), "get_job_log", map[string]any{"repository": "acme/widgets", "job_id": 703}); !r.isError ||
		r.text != "GitHub keeps the log on logs.example.com, which is not a host Kivali reads logs from; tell the owner." {
		t.Errorf("elsewhere = %+v", r)
	}
}

// Why this test exists: the log's address comes from GitHub's
// redirect, and Kivali fetches it from the server. It must be https on
// a host the manifest names (kivali.hosts lists them to the owner), each
// pattern read as kivali.hosts reads it: the host or a name under it, *
// within one label. A pattern must not match a host that only contains
// it, nor one under another domain.
func TestLogURLHoldsTheRedirectToTheLogHosts(t *testing.T) {
	base, _ := url.Parse(DefaultAPIBase)
	for loc, ok := range map[string]bool{
		"https://productionresultssa18.blob.core.windows.net/actions-results/x?sig=1": true,
		"https://pipelines.actions.githubusercontent.com/x":                           true,
		"https://actions.githubusercontent.com/x":                                     true,
		"http://productionresultssa18.blob.core.windows.net/x":                        false,
		"https://evil.blob.core.windows.net/x":                                        false,
		"https://productionresultssa1.x.blob.core.windows.net/x":                      false,
		"https://productionresultssa1.blob.core.windows.net.evil.com/x":               false,
		"https://actions.githubusercontent.com.evil.com/x":                            false,
		"https://evilactions.githubusercontent.com/x":                                 false,
		"https://user@productionresultssa1.blob.core.windows.net/x":                   false,
		"/relative/on/the/api":                                                        false,
		"":                                                                            false,
	} {
		if _, err := logURL(base, loc); (err == nil) != ok {
			t.Errorf("%q: %v", loc, err)
		}
	}
}

// Why this test exists: a job's log can run to hundreds of thousands of
// lines. The answer must keep the end (where a failure shows), count
// every line, keep at most the first error lines, and fit an answer
// whole, so the last line is never the part cut off.
func TestReadLogKeepsTheEndWithinAnAnswer(t *testing.T) {
	var b strings.Builder
	const total = 100000
	for i := 1; i <= total; i++ {
		if i%1000 == 0 {
			fmt.Fprintf(&b, "2026-11-03T10:00:00Z ##[error]failure %d\n", i)
		} else {
			fmt.Fprintf(&b, "2026-11-03T10:00:00Z line %d %s\n", i, strings.Repeat("x", 80))
		}
	}
	got, err := readLog(strings.NewReader(b.String()), 9, logLinesMost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Job 9's log has 100000 lines.\n\nLines marked as errors (20, the first ones):\nline 1000: ##[error]failure 1000\n") ||
		!strings.HasSuffix(got, "##[error]failure 100000") || utf8.RuneCountInString(got) > answerClip {
		t.Errorf("log: %d characters, starts %q, ends %q", utf8.RuneCountInString(got), got[:200], got[len(got)-100:])
	}
	if strings.Contains(got, "2026-11-03T") {
		t.Error("a timestamp is left")
	}
	if got, _ := readLog(strings.NewReader(""), 9, 10); got != "Job 9's log is empty." {
		t.Errorf("empty = %q", got)
	}
}
