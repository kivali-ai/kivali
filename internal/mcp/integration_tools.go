package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/agentshell"
)

// IntegrationCaller relays one call of an integration action to core,
// which checks the agent's access and answers with the result, the
// refusal, or a job for the agent's shell. *agentpod.Client implements
// it.
type IntegrationCaller interface {
	IntegrationCall(ctx context.Context, req agentpod.IntegrationCallRequest) (*agentpod.IntegrationCallResponse, error)
}

// IntegrationShell runs the shell jobs core hands an integration call:
// *agentshell.SidecarShell, the agent-shell daemon.
type IntegrationShell interface {
	Git(ctx context.Context, req agentshell.GitRequest) (*agentshell.GitResponse, error)
}

// shellJobSkippedNoShell is why a shell job did not run for an agent
// whose MCP server has no shell.
const shellJobSkippedNoShell = "the agent has no shell to run git in"

// shellJobUnknown is why a shell job of a kind this server does not
// know did not run.
const shellJobUnknown = "the agent's shell does not know this job"

// IntegrationTools are the agent's integration actions as MCP tools,
// one per entry of the list core gave when this server started
// (agentpod.Client.IntegrationTools). Each is a relay: core matches
// the name against the agent's access at the call and runs it, so a
// grant taken away since the list refuses there. A name a toolkit tool
// already has is left out, so a Kivali tool is never shadowed.
//
// A call core answers with a shell job takes a second request: the job
// runs in shell (nil, an agent with no shell, sends that it was
// skipped) and the call goes to core again with the same name and
// arguments and the job's result, and core's second answer is the
// call's. The job carries credentials: nothing here logs it or puts it
// in an answer.
func IntegrationTools(c IntegrationCaller, shell IntegrationShell, tools []agentpod.IntegrationTool, taken []Tool) []Tool {
	have := make(map[string]bool, len(taken))
	for _, t := range taken {
		have[t.Name] = true
	}
	out := make([]Tool, 0, len(tools))
	for _, it := range tools {
		if it.Name == "" || have[it.Name] {
			continue
		}
		have[it.Name] = true
		name := it.Name
		out = append(out, Tool{
			Name:        name,
			Description: it.Description,
			InputSchema: it.InputSchema,
			// An action may change the outside service.
			ReadOnly: false,
			Handler: func(ctx context.Context, args json.RawMessage) (*ToolResult, error) {
				req := agentpod.IntegrationCallRequest{Name: name, Arguments: args}
				resp, err := c.IntegrationCall(ctx, req)
				if err == nil && resp.ShellJob != nil {
					req.ShellJobResult = runShellJob(ctx, shell, *resp.ShellJob)
					resp, err = c.IntegrationCall(ctx, req)
				}
				if err != nil {
					return &ToolResult{IsError: true, Content: []string{"The call did not reach Kivali: " + err.Error()}}, nil
				}
				if resp.ShellJob != nil {
					return &ToolResult{IsError: true, Content: []string{"Kivali asked the shell for a second job in one call, which it does not run; tell the owner."}}, nil
				}
				return &ToolResult{Content: resp.Content, IsError: resp.IsError}, nil
			},
		})
	}
	return out
}

// runShellJob runs job in shell and is what core reads back. An error
// reaching the shell is the result's error, scrubbed of the job's
// credentials.
func runShellJob(ctx context.Context, shell IntegrationShell, job agentpod.IntegrationShellJob) *agentpod.IntegrationShellJobResult {
	if job.Git == nil {
		return &agentpod.IntegrationShellJobResult{Skipped: shellJobUnknown}
	}
	if shell == nil {
		return &agentpod.IntegrationShellJobResult{Skipped: shellJobSkippedNoShell}
	}
	g := job.Git
	req := agentshell.GitRequest{
		Op:                  g.Op,
		Path:                g.Path,
		URL:                 g.URL,
		RefSpecs:            g.RefSpecs,
		Force:               g.Force,
		AllowedRepositories: g.AllowedRepositories,
		AllowedRefs:         g.AllowedRefs,
		Host:                g.Host,
		Auth:                agentshell.GitAuth{Username: g.Auth.Username, Password: g.Auth.Password},
		Proxy:               agentshell.GitProxy{Username: g.Proxy.Username, Password: g.Proxy.Password},
		TimeoutSeconds:      g.TimeoutSeconds,
	}
	res, err := shell.Git(ctx, req)
	if err != nil {
		msg := agentshell.GitScrubber(req, "")(err.Error())
		if len(msg) > agentshell.GitErrorMaxBytes {
			msg = strings.ToValidUTF8(msg[:agentshell.GitErrorMaxBytes], "")
		}
		return &agentpod.IntegrationShellJobResult{Git: &agentpod.GitResult{Error: msg}}
	}
	out := &agentpod.GitResult{
		Repository:   res.Repository,
		UpdatedTotal: res.UpdatedTotal,
		Error:        res.Error,
		DurationMs:   res.DurationMs,
		TimedOut:     res.TimedOut,
	}
	for _, u := range res.Updated {
		out.Updated = append(out.Updated, agentpod.GitRefUpdate{Ref: u.Ref, Old: u.Old, New: u.New})
	}
	return &agentpod.IntegrationShellJobResult{Git: out}
}
