package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/agentshell"
)

// recordingCaller answers each call with the next of resps (the last
// again once they run out) or err, and records what it was asked.
type recordingCaller struct {
	resps []*agentpod.IntegrationCallResponse
	err   error
	reqs  []agentpod.IntegrationCallRequest
}

func (c *recordingCaller) IntegrationCall(_ context.Context, req agentpod.IntegrationCallRequest) (*agentpod.IntegrationCallResponse, error) {
	c.reqs = append(c.reqs, req)
	if c.err != nil {
		return nil, c.err
	}
	i := min(len(c.reqs), len(c.resps)) - 1
	return c.resps[i], nil
}

// recordingShell runs every git job with res or err and records it.
type recordingShell struct {
	res  *agentshell.GitResponse
	err  error
	jobs []agentshell.GitRequest
}

func (s *recordingShell) Git(_ context.Context, req agentshell.GitRequest) (*agentshell.GitResponse, error) {
	s.jobs = append(s.jobs, req)
	return s.res, s.err
}

// Why this test exists: an integration action's handler is a relay,
// and the model reads what it returns. Core's answer must reach the
// model as it is (its text and its error flag), and a call that never
// reached core must say so as a tool error rather than fail the MCP
// request, which the CLI would show the model as a broken server. A
// name a Kivali tool already has must not be registered over it.
func TestIntegrationToolsRelayToCore(t *testing.T) {
	c := &recordingCaller{resps: []*agentpod.IntegrationCallResponse{{Content: []string{"posted", "and logged"}}}}
	listed := []agentpod.IntegrationTool{
		{Name: "crm_create_post", Description: "Create a post.", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "share_file", Description: "Not this one."},
	}
	tools := IntegrationTools(c, nil, listed, []Tool{{Name: "share_file"}})
	if len(tools) != 1 || tools[0].Name != "crm_create_post" || tools[0].ReadOnly || string(tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools = %+v", tools)
	}
	res, err := tools[0].Handler(context.Background(), json.RawMessage(`{"title":"x"}`))
	if err != nil || res.IsError || len(res.Content) != 2 || res.Content[1] != "and logged" {
		t.Errorf("result = %+v, %v", res, err)
	}
	if len(c.reqs) != 1 || c.reqs[0].Name != "crm_create_post" || string(c.reqs[0].Arguments) != `{"title":"x"}` || c.reqs[0].ShellJobResult != nil {
		t.Errorf("relayed %+v", c.reqs)
	}

	c.resps = []*agentpod.IntegrationCallResponse{{Content: []string{"You have no access to the CRM integration."}, IsError: true}}
	if res, _ := tools[0].Handler(context.Background(), nil); !res.IsError || res.Content[0] != "You have no access to the CRM integration." {
		t.Errorf("refusal = %+v", res)
	}

	c.resps, c.err = nil, errors.New("dial unix: no such file")
	res, err = tools[0].Handler(context.Background(), nil)
	if err != nil || !res.IsError || res.Content[0] != "The call did not reach Kivali: dial unix: no such file" {
		t.Errorf("transport error = %+v, %v", res, err)
	}
}

// Why this test exists: a git clone runs in two requests to core with
// the job between them in the agent's shell. The job must reach the
// daemon field for field (the URL, the access's patterns, the token,
// the proxy credential, the bound, the connection), the second request
// must carry the same name and arguments with what the shell gave back,
// ref for ref, with the connection the daemon echoed,
// and only core's second answer may reach the model: never the job,
// which holds the token.
func TestIntegrationToolsRunTheShellJob(t *testing.T) {
	job := &agentpod.GitJob{
		Op: "clone", Path: "/files/widgets", URL: "https://github.com/acme/widgets.git", Host: "github.com",
		AllowedRepositories: []string{"acme/*"}, RefSpecs: []string{"+refs/heads/main:refs/remotes/origin/main"}, Force: true,
		AllowedRefs:    []string{"refs/heads/agent/*"},
		Auth:           agentpod.GitAuth{Username: "x-access-token", Password: "ghp_secret_token"},
		Proxy:          agentpod.GitProxy{Username: "integration-git.1a2b3c4d", Password: "proxy-secret-value"},
		TimeoutSeconds: 900,
		ConnectionID:   "1a2b3c4d",
	}
	c := &recordingCaller{resps: []*agentpod.IntegrationCallResponse{
		{ShellJob: &agentpod.IntegrationShellJob{Git: job}},
		{Content: []string{"Cloned acme/widgets."}},
	}}
	shell := &recordingShell{res: &agentshell.GitResponse{
		Repository: "acme/widgets", DurationMs: 1200, UpdatedTotal: 901,
		ConnectionID: "1a2b3c4d",
		Updated:      []agentshell.GitRefUpdate{{Ref: "refs/remotes/origin/main", New: "abc"}},
	}}
	tools := IntegrationTools(c, shell, []agentpod.IntegrationTool{{Name: "git_clone"}}, nil)
	args := json.RawMessage(`{"url":"acme/widgets","path":"/files/widgets"}`)
	res, err := tools[0].Handler(context.Background(), args)
	if err != nil || res.IsError || len(res.Content) != 1 || res.Content[0] != "Cloned acme/widgets." {
		t.Fatalf("result = %+v, %v", res, err)
	}
	want := agentshell.GitRequest{
		Op: job.Op, Path: job.Path, URL: job.URL, RefSpecs: job.RefSpecs, Force: true,
		AllowedRepositories: job.AllowedRepositories, AllowedRefs: job.AllowedRefs, Host: job.Host,
		Auth:           agentshell.GitAuth{Username: "x-access-token", Password: "ghp_secret_token"},
		Proxy:          agentshell.GitProxy{Username: "integration-git.1a2b3c4d", Password: "proxy-secret-value"},
		TimeoutSeconds: 900,
		ConnectionID:   "1a2b3c4d",
	}
	if len(shell.jobs) != 1 {
		t.Fatalf("jobs = %+v", shell.jobs)
	}
	got, _ := json.Marshal(shell.jobs[0])
	if wantJSON, _ := json.Marshal(want); string(got) != string(wantJSON) {
		t.Errorf("the daemon got %s, want %s", got, wantJSON)
	}
	if len(c.reqs) != 2 || c.reqs[1].Name != "git_clone" || string(c.reqs[1].Arguments) != string(args) || c.reqs[1].ShellJobResult == nil {
		t.Fatalf("requests = %+v", c.reqs)
	}
	r := c.reqs[1].ShellJobResult.Git
	if r == nil || r.Repository != "acme/widgets" || r.ConnectionID != "1a2b3c4d" || r.DurationMs != 1200 || r.UpdatedTotal != 901 || len(r.Updated) != 1 || r.Updated[0] != (agentpod.GitRefUpdate{Ref: "refs/remotes/origin/main", New: "abc"}) {
		t.Errorf("result sent = %+v", r)
	}

	// The daemon out of reach: the error goes to core, scrubbed of the
	// job's credentials.
	c.reqs = nil
	shell.res, shell.err = nil, errors.New("agentshell: HTTP 400: bad ghp_secret_token proxy-secret-value")
	if _, err := tools[0].Handler(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	e := c.reqs[1].ShellJobResult.Git.Error
	if got := c.reqs[1].ShellJobResult.Git.ConnectionID; got != "1a2b3c4d" {
		t.Errorf("an unreached daemon's result names connection %q", got)
	}
	if strings.Contains(e, "ghp_secret_token") || strings.Contains(e, "proxy-secret-value") || !strings.Contains(e, "HTTP 400") || !strings.Contains(e, agentshell.GitTokenPlaceholder) {
		t.Errorf("error sent = %q", e)
	}

	// No shell (a local tree): skipped, and nothing ran.
	c.reqs, shell.jobs = nil, nil
	tools = IntegrationTools(c, nil, []agentpod.IntegrationTool{{Name: "git_clone"}}, nil)
	if _, err := tools[0].Handler(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if s := c.reqs[1].ShellJobResult; s.Skipped != shellJobSkippedNoShell || s.Git != nil {
		t.Errorf("no shell = %+v", s)
	}

	// Core asking for a second job is not run, and never shown.
	c.reqs = nil
	c.resps = []*agentpod.IntegrationCallResponse{{ShellJob: &agentpod.IntegrationShellJob{Git: job}}}
	tools = IntegrationTools(c, shell, []agentpod.IntegrationTool{{Name: "git_clone"}}, nil)
	res, _ = tools[0].Handler(context.Background(), args)
	if !res.IsError || len(c.reqs) != 2 || strings.Contains(strings.Join(res.Content, " "), "ghp_secret_token") {
		t.Errorf("a second job = %+v after %d requests", res, len(c.reqs))
	}
}

// Why this test exists: runShellJob copies the job core built into the
// request the daemon reads, field by field. A field added to one shape
// and not copied (AllowedRefs, say) would reach the daemon empty, and
// the daemon's own check of the job would refuse every push or, for a
// field that loosens nothing when empty, silently drop what core
// decided. Both shapes must keep the same JSON fields.
func TestGitJobAndGitRequestKeepTheSameFields(t *testing.T) {
	fields := func(v any) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			out = append(out, rt.Field(i).Name+" "+rt.Field(i).Tag.Get("json"))
		}
		return out
	}
	if job, req := fields(agentpod.GitJob{}), fields(agentshell.GitRequest{}); !slices.Equal(job, req) {
		t.Errorf("GitJob %v\nGitRequest %v", job, req)
	}
}
