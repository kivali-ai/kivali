package mcp

import (
	"context"
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

// IntegrationCaller relays one call of an integration action to core,
// which checks the agent's access and answers with the result or the
// refusal. *agentpod.Client implements it.
type IntegrationCaller interface {
	IntegrationCall(ctx context.Context, name string, args json.RawMessage) (*agentpod.IntegrationCallResponse, error)
}

// IntegrationTools are the agent's integration actions as MCP tools,
// one per entry of the list core gave when this server started
// (agentpod.Client.IntegrationTools). Each is a relay: core matches
// the name against the agent's access at the call and runs it, so a
// grant taken away since the list refuses there. A name a toolkit tool
// already has is left out, so a Kivali tool is never shadowed.
func IntegrationTools(c IntegrationCaller, tools []agentpod.IntegrationTool, taken []Tool) []Tool {
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
				resp, err := c.IntegrationCall(ctx, name, args)
				if err != nil {
					return &ToolResult{IsError: true, Content: []string{"The call did not reach Kivali: " + err.Error()}}, nil
				}
				return &ToolResult{Content: resp.Content, IsError: resp.IsError}, nil
			},
		})
	}
	return out
}
