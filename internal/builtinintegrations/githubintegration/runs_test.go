package githubintegration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// runner is a writer whose access also has the run_workflows switch.
func runner(patterns ...string) builtinintegrations.Caller {
	c := agent(true, patterns...)
	c.Access["run_workflows"] = true
	return c
}

// runCalls is one call of each run action, with what GitHub answers it,
// the address it must reach and the audit row it must write.
var runCalls = []struct {
	name   string
	args   map[string]any
	route  string
	status int
	body   string
	audit  string
}{
	{"rerun_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5},
		"POST /repos/acme/widgets/actions/runs/5/rerun", 201, `{}`, "started workflow run 5 of acme/widgets again"},
	{"rerun_job", map[string]any{"repository": "acme/widgets", "job_id": 7},
		"POST /repos/acme/widgets/actions/jobs/7/rerun", 201, `{}`, "started job 7 of acme/widgets again"},
	{"cancel_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5},
		"POST /repos/acme/widgets/actions/runs/5/cancel", 202, `{}`, "asked GitHub to cancel workflow run 5 of acme/widgets"},
	{"dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": "ci.yml", "ref": "main"},
		"POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches", 200,
		`{"workflow_run_id":99,"run_url":"https://api.github.com/repos/acme/widgets/actions/runs/99","html_url":"https://github.com/acme/widgets/actions/runs/99"}`,
		"started the workflow ci.yml of acme/widgets on main as run 99 (https://github.com/acme/widgets/actions/runs/99)"},
}

// Why this test exists: rerunning or starting a workflow runs it with
// the repository's Actions secrets, and a workflow may deploy, so the
// switch is the owner's alone to give. A reader, a writer without the
// switch, the switch without write and a switch that is not true must
// each be refused before GitHub is asked anything, the words naming the
// cause and the owner and no other way round, and must not be listed
// the tools, so that a model is never offered what it cannot do. The
// second request goes through the same check, so an access changed
// between requests holds too.
func TestRunActionsNeedTheSwitchAndWrite(t *testing.T) {
	reader := agent(false, "acme/*")
	switchOnly := agent(false, "acme/*")
	switchOnly.Access["run_workflows"] = true
	for _, c := range []struct {
		name   string
		caller builtinintegrations.Caller
	}{
		{"reader", reader},
		{"writer without the switch", agent(true, "acme/*")},
		{"the switch without write", switchOnly},
		{"a switch that is not true", callerWith(map[string]any{"repositories": []any{"acme/*"}, "write": true, "run_workflows": "true"})},
	} {
		g, f := newTest(t)
		for _, rc := range runCalls {
			r := g.callT(t, c.caller, rc.name, rc.args)
			if r.refusal == nil || r.text != runRefusal || r.refusal.Reason != runReason {
				t.Errorf("%s, %s: %+v", c.name, rc.name, r)
			}
			raw, _ := json.Marshal(rc.args)
			_, err := g.Complete(context.Background(), c.caller, testConn(), rc.name, raw, shelljob.Result{})
			var ref *builtinintegrations.Refusal
			if !errors.As(err, &ref) || ref.Text != runRefusal {
				t.Errorf("%s, %s: second request = %v", c.name, rc.name, err)
			}
		}
		if n := len(f.requests()); n != 0 {
			t.Errorf("%s: GitHub took %d requests", c.name, n)
		}
		tools, err := g.Tools(context.Background(), c.caller)
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range tools {
			for _, rc := range runCalls {
				if tl.Name == rc.name {
					t.Errorf("%s is listed %s", c.name, tl.Name)
				}
			}
		}
	}
}

// Why this test exists: the switch adds actions and no repositories. A
// call for a repository the access does not name must be refused before
// any token is minted, or a token that may run workflows would exist for
// a repository the owner never chose.
func TestRunActionsStayInTheAccess(t *testing.T) {
	g, f := newTest(t)
	for _, rc := range runCalls {
		r := g.callT(t, runner("acme/gadgets"), rc.name, rc.args)
		if r.refusal == nil || r.text != "Your access to GitHub does not include acme/widgets; ask the owner." {
			t.Errorf("%s: %+v", rc.name, r)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("GitHub took %d requests", n)
	}
}

// Why this test exists: each run action must reach its address with a
// token narrowed to the one repository and to actions write alone, so a
// leaked or misused token reaches nothing else, and must leave an audit
// row naming what it did, since the owner reads Recent calls to see what
// ran with their secrets.
func TestRunActionsTokenAddressAndAudit(t *testing.T) {
	for _, rc := range runCalls {
		g, f := newTest(t)
		f.route(rc.route, rc.status, rc.body)
		r := g.callT(t, runner("acme/*"), rc.name, rc.args)
		if r.isError || r.refusal != nil || r.audit != rc.audit {
			t.Errorf("%s: %+v", rc.name, r)
		}
		if want := strings.ToUpper(rc.audit[:1]) + rc.audit[1:] + "."; r.text != want {
			t.Errorf("%s: text %q, want %q", rc.name, r.text, want)
		}
		api := f.api()
		if len(api) != 1 || api[0].Method+" "+api[0].Path != rc.route {
			t.Fatalf("%s: requests %+v", rc.name, api)
		}
		if gr := api[0].Grant; gr == nil || !mapsEqual(gr.Permissions, map[string]string{"metadata": "read", "actions": "write"}) || !slices.Equal(gr.Repositories, []string{"widgets"}) {
			t.Errorf("%s: grant %+v", rc.name, api[0].Grant)
		}
	}
}

// Why this test exists: needed() asks for actions write, so an app made
// earlier reads unmet until the owner's Fix, and everything else must
// keep working until then. Were any read of Actions to ask for write,
// GitHub would mint it no token, and an owner who has not approved run
// control would lose runs, logs and artifacts as well. The fake refuses
// any token past actions read, as such an installation does.
func TestReadsOfActionsWorkWithoutActionsWrite(t *testing.T) {
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"list_workflows", map[string]any{"repository": "acme/widgets"}},
		{"list_workflow_runs", map[string]any{"repository": "acme/widgets"}},
		{"get_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5}},
		{"get_job_log", map[string]any{"repository": "acme/widgets", "job_id": 7}},
		{"save_job_log", map[string]any{"repository": "acme/widgets", "job_id": 7}},
		{"list_artifacts", map[string]any{"repository": "acme/widgets"}},
		{"download_artifact", map[string]any{"repository": "acme/widgets", "artifact_id": 3}},
	} {
		g, f := newTest(t)
		f.installed = map[string]string{"metadata": "read", "actions": "read"}
		raw, _ := json.Marshal(c.args)
		res, err := g.Call(context.Background(), agent(false, "acme/*"), testConn(), c.name, raw)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(res.Content) > 0 && strings.Contains(res.Content[0], "a token") {
			t.Errorf("%s: %q", c.name, res.Content[0])
		}
		mints := f.mints()
		if len(mints) == 0 {
			t.Errorf("%s minted no token", c.name)
		}
		for _, m := range mints {
			if !strings.Contains(m.Body, `"actions":"read"`) {
				t.Errorf("%s: mint %s", c.name, m.Body)
			}
		}
	}
}

// Why this test exists: an app that has not approved actions write gets
// no token at all for a run action, and GitHub's 422 alone reads as a
// bad call. The agent must be told it is the owner's to approve on
// GitHub, and nothing must reach the run's address.
func TestRunActionsOnAnAppWithoutActionsWrite(t *testing.T) {
	const want = "GitHub refused the GitHub App a token that may run workflows in acme/widgets (HTTP 422): The permissions requested are not granted to this installation. The app's installation may lack the Actions write permission; ask the owner to approve it on GitHub."
	for _, rc := range runCalls {
		g, f := newTest(t)
		f.installed = map[string]string{"metadata": "read", "actions": "read"}
		r := g.callT(t, runner("acme/*"), rc.name, rc.args)
		if !r.isError || r.refusal != nil || r.text != want {
			t.Errorf("%s: %+v", rc.name, r)
		}
		if n := len(f.api()); n != 0 {
			t.Errorf("%s: GitHub took %d requests past the token", rc.name, n)
		}
	}
}

// Why this test exists: failed_only is the difference between running
// the failed jobs and running everything again, deployments included.
// It must choose the address, and a value that is not a boolean must be
// refused rather than read as either.
func TestFailedOnlyChoosesTheAddress(t *testing.T) {
	g, f := newTest(t)
	f.route("POST /repos/acme/widgets/actions/runs/5/rerun", 201, `{}`)
	f.route("POST /repos/acme/widgets/actions/runs/5/rerun-failed-jobs", 201, `{}`)
	r := g.callT(t, runner("acme/*"), "rerun_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5, "failed_only": true})
	if r.isError || r.audit != "started the failed jobs of workflow run 5 of acme/widgets again" {
		t.Errorf("failed only: %+v", r)
	}
	if r := g.callT(t, runner("acme/*"), "rerun_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5, "failed_only": false}); r.isError {
		t.Errorf("failed_only false: %+v", r)
	}
	api := f.api()
	if len(api) != 2 || !strings.HasSuffix(api[0].Path, "/rerun-failed-jobs") || !strings.HasSuffix(api[1].Path, "/rerun") {
		t.Errorf("requests %+v", api)
	}
	if r := g.callT(t, runner("acme/*"), "rerun_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5, "failed_only": "yes"}); r.refusal == nil || r.text != "failed_only is true or false." {
		t.Errorf("failed_only text: %+v", r)
	}
}

// Why this test exists: the body is what GitHub starts the workflow
// with. It must carry the ref and the inputs with their types, and no
// inputs key when there are none. The answer must carry the new run's id
// and address, or the agent cannot follow what it started; a file name
// must stay one escaped segment of the address.
func TestDispatchBodyAndAnswer(t *testing.T) {
	g, f := newTest(t)
	const answer = `{"workflow_run_id":99,"html_url":"https://github.com/acme/widgets/actions/runs/99"}`
	f.route("POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches", 200, answer)
	f.route("POST /repos/acme/widgets/actions/workflows/123/dispatches", 200, answer)
	f.route("POST /repos/acme/widgets/actions/workflows/my flow.yml/dispatches", 200, answer)

	r := g.callT(t, runner("acme/*"), "dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": "ci.yml", "ref": "release/1.2",
		"inputs": map[string]any{"env": "staging", "count": 3, "dry": true}})
	if r.isError || !strings.Contains(r.text, "run 99") || !strings.Contains(r.text, "https://github.com/acme/widgets/actions/runs/99") {
		t.Fatalf("answer = %+v", r)
	}
	if got := f.api()[0].Body; got != `{"inputs":{"count":3,"dry":true,"env":"staging"},"ref":"release/1.2","return_run_details":true}` {
		t.Errorf("body = %s", got)
	}

	if r := g.callT(t, runner("acme/*"), "dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": 123, "ref": "main", "inputs": map[string]any{}}); r.isError {
		t.Fatalf("by id: %+v", r)
	}
	if got := f.api()[1].Body; got != `{"ref":"main","return_run_details":true}` {
		t.Errorf("body without inputs = %s", got)
	}

	if r := g.callT(t, runner("acme/*"), "dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": "my flow.yml", "ref": "main"}); r.isError {
		t.Fatalf("escaped: %+v", r)
	}
	if got := f.api()[2].Escaped; got != "/repos/acme/widgets/actions/workflows/my%20flow.yml/dispatches" {
		t.Errorf("escaped path = %s", got)
	}
}

// Why this test exists: a dispatch that succeeded has started a workflow
// that may deploy. Read as a failure, the agent would start it again, so
// an answer with no run in it must still say it started. A request
// GitHub did not answer may have started it as well, and must not read
// as one to try again.
func TestDispatchNeverReadsAsWorthRepeating(t *testing.T) {
	g, f := newTest(t)
	f.route("POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches", 204, ``)
	call := map[string]any{"repository": "acme/widgets", "workflow": "ci.yml", "ref": "main"}
	r := g.callT(t, runner("acme/*"), "dispatch_workflow", call)
	if r.isError || r.text != "Started the workflow ci.yml of acme/widgets on main; GitHub named no run, and github_list_workflow_runs lists it." {
		t.Errorf("no run named: %+v", r)
	}
	f.routeFunc("POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches", func(recorded) reply {
		panic(http.ErrAbortHandler)
	})
	r = g.callT(t, runner("acme/*"), "dispatch_workflow", call)
	if !r.isError || r.text != "GitHub did not answer, so the workflow ci.yml of acme/widgets on main may have started; github_list_workflow_runs shows whether it did." {
		t.Errorf("no answer: %+v", r)
	}
}

// Why this test exists: inputs come from a model, and may have been
// written by whoever wrote what it read. Too many, too long, or of a
// shape GitHub does not take must be refused before any token is minted,
// and exactly the limits must pass.
func TestDispatchInputsLimits(t *testing.T) {
	many := func(n int) map[string]any {
		m := map[string]any{}
		for i := range n {
			m["k"+strings.Repeat("x", i)] = "v"
		}
		return m
	}
	long := map[string]any{}
	for i := range 7 {
		long["k"+strings.Repeat("x", i)] = strings.Repeat("v", inputValueMost)
	}
	for _, c := range []struct {
		name   string
		inputs any
		want   string
	}{
		{"not an object", "a=b", "inputs is an object of names and values."},
		{"too many", many(26), "inputs holds at most 25 values."},
		{"a null value", map[string]any{"a": nil}, "Each of inputs is a string, a number or a boolean."},
		{"a list", map[string]any{"a": []any{"b"}}, "Each of inputs is a string, a number or a boolean."},
		{"an object", map[string]any{"a": map[string]any{"b": "c"}}, "Each of inputs is a string, a number or a boolean."},
		{"a long value", map[string]any{"a": strings.Repeat("v", inputValueMost+1)}, "Each of inputs is at most 10000 characters."},
		{"too long in all", long, "inputs is longer than 60000 characters."},
		{"a blank name", map[string]any{" ": "b"}, "An input's name is one line of at most 100 characters."},
		{"a name with a line break", map[string]any{"a\nb": "c"}, "An input's name is one line of at most 100 characters."},
		{"a long name", map[string]any{strings.Repeat("n", inputNameMost+1): "c"}, "An input's name is one line of at most 100 characters."},
	} {
		g, f := newTest(t)
		r := g.callT(t, runner("acme/*"), "dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": "ci.yml", "ref": "main", "inputs": c.inputs})
		if r.refusal == nil || r.text != c.want {
			t.Errorf("%s: %+v", c.name, r)
		}
		if n := len(f.requests()); n != 0 {
			t.Errorf("%s: GitHub took %d requests", c.name, n)
		}
	}
	g, f := newTest(t)
	f.route("POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches", 200, `{"workflow_run_id":1}`)
	if r := g.callT(t, runner("acme/*"), "dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": "ci.yml", "ref": "main",
		"inputs": many(25)}); r.isError {
		t.Errorf("25 inputs: %+v", r)
	}
	if r := g.callT(t, runner("acme/*"), "dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": "ci.yml", "ref": "main",
		"inputs": map[string]any{"a": strings.Repeat("v", inputValueMost)}}); r.isError {
		t.Errorf("a value at the most: %+v", r)
	}
}

// Why this test exists: workflow and ref go into the address and the
// body of a request that runs code. A workflow that climbs out of its
// path segment could reach another endpoint with a token that writes,
// and a ref that does not read as a branch or a tag must never be sent.
func TestDispatchWorkflowAndRefValidation(t *testing.T) {
	g, f := newTest(t)
	const notOne = "workflow is a workflow's id or its file's name, such as ci.yml."
	for _, c := range []struct {
		workflow any
		ref      any
		want     string
	}{
		{"../../../issues.yml", "main", notOne},
		{"a/b.yml", "main", notOne},
		{`a\b.yml`, "main", notOne},
		{"ci.yml\n", "main", notOne},
		{"ci.txt", "main", notOne},
		{-1, "main", notOne},
		{1.5, "main", notOne},
		{true, "main", notOne},
		{strings.Repeat("a", workflowFileMost) + ".yml", "main", notOne},
		{"", "main", "Give the workflow: its id or its file's name, such as ci.yml."},
		{"ci.yml", "", "ref is a branch or a tag."},
		{"ci.yml", "a..b", "ref is a branch or a tag."},
		{"ci.yml", "-x", "ref is a branch or a tag."},
		{"ci.yml", "owner:main", "ref is a branch or a tag."},
		{"ci.yml", 7, "ref is a branch or a tag."},
	} {
		r := g.callT(t, runner("acme/*"), "dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": c.workflow, "ref": c.ref})
		if r.refusal == nil || r.text != c.want {
			t.Errorf("%v %v: %+v", c.workflow, c.ref, r)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("GitHub took %d requests", n)
	}
}

// Why this test exists: GitHub's words for a run still in progress, a
// finished run or a workflow without a trigger must reach the agent with
// what it can do next, and not as the generic 403, which sends it to the
// owner over a permission that is not the cause. A rate limit is not
// such a refusal, and must keep saying to try again later.
func TestRunActionsSayWhatGitHubRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		args   map[string]any
		route  string
		status int
		body   string
		want   string
	}{
		{"rerun_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5}, "POST /repos/acme/widgets/actions/runs/5/rerun", 403, `{"message":"This workflow is already running"}`,
			"GitHub did not rerun workflow run 5 of acme/widgets (HTTP 403): This workflow is already running. A run still in progress cannot be rerun; github_get_workflow_run reads its status. For anything else, ask the owner."},
		{"rerun_job", map[string]any{"repository": "acme/widgets", "job_id": 7}, "POST /repos/acme/widgets/actions/jobs/7/rerun", 403, `{"message":"Not finished."}`,
			"GitHub did not rerun job 7 of acme/widgets (HTTP 403): Not finished. A run still in progress cannot be rerun; github_get_workflow_run reads its status. For anything else, ask the owner."},
		{"cancel_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5}, "POST /repos/acme/widgets/actions/runs/5/cancel", 409, `{"message":"Cannot cancel a workflow run that is completed."}`,
			"GitHub did not cancel workflow run 5 of acme/widgets (HTTP 409): Cannot cancel a workflow run that is completed. A run that has finished cannot be cancelled; github_get_workflow_run reads its status. For anything else, ask the owner."},
		{"dispatch_workflow", map[string]any{"repository": "acme/widgets", "workflow": "ci.yml", "ref": "main"}, "POST /repos/acme/widgets/actions/workflows/ci.yml/dispatches", 422, `{"message":"Workflow does not have 'workflow_dispatch' trigger"}`,
			"GitHub did not start the workflow ci.yml of acme/widgets on main (HTTP 422): Workflow does not have 'workflow_dispatch' trigger. The workflow's file on that branch or tag must have a workflow_dispatch trigger, and the inputs must be ones it declares. For anything else, ask the owner."},
		{"rerun_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5}, "POST /repos/acme/widgets/actions/runs/5/rerun", 403, `{"message":"API rate limit exceeded"}`,
			"GitHub is limiting the GitHub App's requests for now (HTTP 403): API rate limit exceeded. Try again later."},
		{"rerun_workflow_run", map[string]any{"repository": "acme/widgets", "run_id": 5}, "POST /repos/acme/widgets/actions/runs/5/rerun", 404, `{"message":"Not Found"}`,
			"GitHub answered 404: Not Found"},
	} {
		g, f := newTest(t)
		f.route(c.route, c.status, c.body)
		r := g.callT(t, runner("acme/*"), c.name, c.args)
		if !r.isError || r.refusal != nil || r.text != c.want {
			t.Errorf("%s %d: %+v", c.name, c.status, r)
		}
	}
}
