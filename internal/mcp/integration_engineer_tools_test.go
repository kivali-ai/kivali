package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
)

// engineerRecorder answers the integration_* relay as core would: a
// first request with a smoke test to run, a second with the answer.
type engineerRecorder struct {
	reqs []agentpod.IntegrationEngineerRequest
	err  error
}

func (r *engineerRecorder) IntegrationEngineerDispatch(_ context.Context, req agentpod.IntegrationEngineerRequest) (*agentpod.IntegrationEngineerResponse, error) {
	r.reqs = append(r.reqs, req)
	if r.err != nil {
		return nil, r.err
	}
	if req.Tool == IntegrationValidateToolName && req.SmokeResult == nil {
		return &agentpod.IntegrationEngineerResponse{Smoke: &agentpod.IntegrationSmoke{Command: "check", Cwd: "artifacts/private/integrations/echo", TimeoutSeconds: 30}}, nil
	}
	return &agentpod.IntegrationEngineerResponse{Body: "answered"}, nil
}

// shellRecorder is a shell that records what it was asked to run.
type shellRecorder struct {
	got []agent.ShellRequest
}

func (s *shellRecorder) Exec(_ context.Context, slug string, req agent.ShellRequest) (*agent.ShellResult, error) {
	s.got = append(s.got, req)
	return &agent.ShellResult{Stdout: "printed", ExitCode: 0}, nil
}
func (s *shellRecorder) SyncSkills(context.Context, string) error { return nil }

func toolNamed(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	return Tool{}
}

// Why this test exists: the smoke test is the one part of a validate
// or a publish that core cannot do, because the engineer's shell is in
// the engineer's pod. The MCP server runs exactly what core asked, in
// the directory core named, and sends the same call again with what the
// shell printed; a server with no shell says so instead of running
// anything. A relay that dropped the second request would answer the
// model with an empty body; one that ran the command elsewhere would
// check the wrong bundle.
func TestIntegrationEngineerToolsRunTheSmokeTestCoreAsksFor(t *testing.T) {
	core := &engineerRecorder{}
	shell := &shellRecorder{}
	validate := toolNamed(IntegrationEngineerTools(core, shell, "integrations-engineer"), IntegrationValidateToolName)
	res, err := validate.Handler(context.Background(), json.RawMessage(`{"path":"/files/x"}`))
	if err != nil || res.IsError || res.Content[0] != "answered" {
		t.Fatalf("validate = %+v, %v", res, err)
	}
	if len(shell.got) != 1 || shell.got[0] != (agent.ShellRequest{Command: "check", Cwd: "artifacts/private/integrations/echo", TimeoutSeconds: 30}) {
		t.Errorf("shell ran %+v", shell.got)
	}
	if len(core.reqs) != 2 || core.reqs[1].SmokeResult == nil || core.reqs[1].SmokeResult.Stdout != "printed" || string(core.reqs[1].Input) != `{"path":"/files/x"}` {
		t.Errorf("requests = %+v", core.reqs)
	}

	core = &engineerRecorder{}
	validate = toolNamed(IntegrationEngineerTools(core, nil, "integrations-engineer"), IntegrationValidateToolName)
	if _, err := validate.Handler(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(core.reqs) != 2 || core.reqs[1].SmokeResult == nil || core.reqs[1].SmokeResult.Skipped != smokeSkippedNoShell {
		t.Errorf("no shell: requests = %+v", core.reqs)
	}

	// A list is one request; a core that cannot be reached is an error
	// the model reads.
	core = &engineerRecorder{}
	list := toolNamed(IntegrationEngineerTools(core, shell, "integrations-engineer"), IntegrationListToolName)
	if res, _ := list.Handler(context.Background(), nil); res.Content[0] != "answered" || len(core.reqs) != 1 || !list.ReadOnly {
		t.Errorf("list = %+v, %d requests", res, len(core.reqs))
	}
	core.err = errors.New("dial: no socket")
	if res, _ := list.Handler(context.Background(), nil); !res.IsError || res.Content[0] != "The call did not reach Kivali: dial: no socket" {
		t.Errorf("unreachable = %+v", res)
	}
}
