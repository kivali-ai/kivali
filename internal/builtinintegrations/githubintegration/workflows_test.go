package githubintegration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// workflowsAgent is a writer whose access also has the workflows
// switch.
func workflowsAgent(patterns ...string) builtinintegrations.Caller {
	c := agent(true, patterns...)
	c.Access["workflows"] = true
	return c
}

// Why this test exists: GitHub refuses a push that changes a workflow
// file unless its token may write workflows, and a workflow runs with
// the repository's Actions secrets. So a push's token asks for
// workflows write only when both the owner's switch allows it and the
// agent says this push changes one: the switch is the access, and the
// agent's word keeps every other push's token without it, so a
// workflow change the agent did not mean to make is refused by GitHub
// instead of landing. A push saying so without the switch is refused
// by name before any token is asked for; the switch without write
// gives nothing, since only a writer pushes.
func TestPushAsksForWorkflowsOnlyWhenSaidAndAllowed(t *testing.T) {
	const contentsOnly = `{"permissions":{"contents":"write","metadata":"read"},"repositories":["widgets"]}`
	const withWorkflows = `{"permissions":{"contents":"write","metadata":"read","workflows":"write"},"repositories":["widgets"]}`
	for _, c := range []struct {
		name   string
		caller builtinintegrations.Caller
		args   string
		mint   string
		audit  string
	}{
		{"switch, not said", workflowsAgent("acme/*"), `{"path":"w","url":"acme/widgets","refs":["agent/x"]}`, contentsOnly,
			"started: acme/widgets, push of refs/heads/agent/x"},
		{"switch, said false", workflowsAgent("acme/*"), `{"path":"w","url":"acme/widgets","refs":["agent/x"],"workflows":false}`, contentsOnly,
			"started: acme/widgets, push of refs/heads/agent/x"},
		{"switch, said", workflowsAgent("acme/*"), `{"path":"w","url":"acme/widgets","refs":["agent/x"],"workflows":true}`, withWorkflows,
			"started: acme/widgets, push of refs/heads/agent/x, changing workflow files"},
		{"no switch, not said", agent(true, "acme/*"), `{"path":"w","url":"acme/widgets","refs":["agent/x"],"workflows":false}`, contentsOnly,
			"started: acme/widgets, push of refs/heads/agent/x"},
	} {
		g, f := newTest(t)
		job, res, ref := g.gitCall(t, c.caller, "push", c.args)
		if ref != nil || job == nil {
			t.Fatalf("%s: push = %+v, %+v", c.name, res, ref)
		}
		if mints := f.mints(); len(mints) != 1 || mints[0].Body != c.mint {
			t.Errorf("%s: mints = %+v", c.name, mints)
		}
		if res.Audit != c.audit {
			t.Errorf("%s: audit = %q", c.name, res.Audit)
		}
	}

	for _, c := range []struct {
		name   string
		caller builtinintegrations.Caller
	}{
		{"no switch", agent(true, "acme/*")},
		{"switch not true", callerWith(map[string]any{"repositories": []any{"acme/*"}, "write": true, "workflows": "true"})},
	} {
		g, f := newTest(t)
		_, _, ref := g.gitCall(t, c.caller, "push", `{"path":"w","url":"acme/widgets","refs":["agent/x"],"workflows":true}`)
		if ref == nil || ref.Text != "The GitHub integration's access for you does not allow pushing changes to workflow files (.github/workflows). Ask the owner if your work needs it." ||
			ref.Reason != "the access does not allow pushing changes to workflow files" {
			t.Errorf("%s: %+v", c.name, ref)
		}
		if n := len(f.requests()); n != 0 {
			t.Errorf("%s: GitHub took %d requests", c.name, n)
		}
	}

	// The switch without write: the push itself is refused, as every
	// write is.
	g, f := newTest(t)
	reader := agent(false, "acme/*")
	reader.Access["workflows"] = true
	if _, _, ref := g.gitCall(t, reader, "push", `{"path":"w","url":"acme/widgets","refs":["agent/x"],"workflows":true}`); ref == nil || ref.Reason != writeReason {
		t.Errorf("reader = %+v", ref)
	}
	// A clone and a fetch never ask for workflows.
	for _, op := range []string{"clone", "fetch"} {
		if _, _, ref := g.gitCall(t, workflowsAgent("acme/*"), op, `{"path":"w","url":"acme/widgets"}`); ref != nil {
			t.Fatalf("%s: %+v", op, ref)
		}
	}
	for _, m := range f.mints() {
		if strings.Contains(m.Body, "workflows") {
			t.Errorf("mint = %s", m.Body)
		}
	}
}

// Why this test exists: an app made before Kivali asked for workflows
// lacks it until the owner approves it, and GitHub mints no token at
// all for a permission the installation lacks. A push that says it
// changes a workflow file must then say that in words the agent can
// act on (ask the owner to approve the permission), not GitHub's bare
// 422.
func TestPushWithWorkflowsTheInstallationLacks(t *testing.T) {
	g, f := newTest(t)
	f.mintReply = &reply{Status: 422, Body: `{"message":"The permissions requested are not granted to this installation."}`}
	job, res, ref := g.gitCall(t, workflowsAgent("acme/*"), "push", `{"path":"w","url":"acme/widgets","refs":["agent/x"],"workflows":true}`)
	if ref != nil || job != nil || res == nil || !res.IsError ||
		res.Content[0] != "GitHub refused the GitHub App a token that may change workflow files in acme/widgets (HTTP 422): The permissions requested are not granted to this installation. The app's installation may lack the workflows permission; ask the owner to approve it on GitHub." {
		t.Errorf("push = %+v, %+v", res, ref)
	}
}

// Why this test exists: GitHub refuses a whole push when one of its
// commits changes a workflow file and the token may not write
// workflows. The agent must be told what to do next: push again saying
// so when its access allows it, ask the owner for the switch when it
// does not, or ask the owner to approve the app's permission when it
// said so and was still refused. Any other failed push reads as it
// did.
func TestCompleteSaysWhatToDoAboutAWorkflowRefusal(t *testing.T) {
	const refused = "push: command error on refs/heads/main: refusing to allow a GitHub App to create or update workflow `.github/workflows/ci.yml` without `workflows` permission"
	const said = `{"path":"w","url":"acme/widgets","refs":["main"],"workflows":true}`
	const notSaid = `{"path":"w","url":"acme/widgets","refs":["main"]}`
	g, _ := newTest(t)
	for _, c := range []struct {
		name   string
		caller builtinintegrations.Caller
		args   string
		err    string
		ends   string
	}{
		{"allowed, not said", workflowsAgent("acme/*"), notSaid, refused, "The commits change a workflow file (.github/workflows): push again with workflows set to true."},
		{"not allowed", agent(true, "acme/*"), notSaid, refused, "The commits change a workflow file (.github/workflows), and your access to GitHub does not include changing workflows; ask the owner if your work needs it."},
		{"said, still refused", workflowsAgent("acme/*"), said, refused, "GitHub refused the GitHub App's token the workflows permission; ask the owner to approve it for the app's installation on GitHub."},
		{"another failure", workflowsAgent("acme/*"), notSaid, "push: non-fast-forward update: refs/heads/main", "push: non-fast-forward update: refs/heads/main"},
	} {
		res, err := g.Complete(context.Background(), c.caller, testConn(), "push", json.RawMessage(c.args),
			shelljob.Result{Git: &shelljob.GitResult{Repository: "acme/widgets", ConnectionID: "c0ffee01", Error: c.err}})
		if err != nil || res == nil || !res.IsError || !strings.HasPrefix(res.Content[0], "The push of acme/widgets did not go through: ") ||
			!strings.HasSuffix(res.Content[0], c.ends) {
			t.Errorf("%s: %+v, %v", c.name, res, err)
		}
	}
}

// Why this test exists: an agent learns that it may say a push changes
// workflow files only from push's schema and description. An agent
// whose access has the switch must see workflows and the one sentence
// that says when to set it; any other agent must see neither, so it is
// never told of a permission it does not have.
func TestPushListsWorkflowsOnlyWithTheSwitch(t *testing.T) {
	g := New()
	for _, c := range []struct {
		name   string
		caller builtinintegrations.Caller
		want   bool
	}{
		{"switch", workflowsAgent("acme/*"), true},
		{"no switch", agent(true, "acme/*"), false},
	} {
		tools, err := g.Tools(context.Background(), c.caller)
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range tools {
			if tl.Name != "push" {
				continue
			}
			var s struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(tl.InputSchema, &s); err != nil {
				t.Fatal(err)
			}
			_, listed := s.Properties["workflows"]
			said := strings.Contains(tl.Description, "Set workflows to true when the commits you push add, change or delete a file under .github/workflows/")
			if listed != c.want || said != c.want {
				t.Errorf("%s: schema %v, description %v: %s", c.name, listed, said, tl.Description)
			}
		}
	}
}
