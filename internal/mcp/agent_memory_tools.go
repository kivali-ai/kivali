package mcp

import (
	"context"
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/agent"
)

// AgentMemoryDispatcher executes one agent_memory_* tool call and
// returns the rendered tool_result body + tool-level error flag.
//
// The production implementation wraps an agentpod.Client: the agent
// pod's MCP subprocess forwards (toolName, raw) over UDS to core,
// where agent.DispatchAgentMemoryTool runs against the authoritative
// FSStore and returns (body, isError) back over the wire.
type AgentMemoryDispatcher interface {
	DispatchAgentMemoryTool(ctx context.Context, name string, raw json.RawMessage) (body string, isError bool, err error)
}

// AgentMemoryTools returns MCP tool definitions for the agent_memory_*
// family: the three verbs on semantic memory (agent_memory.md) and the
// same three on habits (stored as agent_memory_habits.md).
//
// Both documents are inlined into every future system prompt as part
// of the agent identity block. They live outside the /files/ virtual
// filesystem and are reachable only through these dedicated tools.
//
// The definitions are derived from agent.AgentMemoryTools (the
// native-API equivalent) rather than restated, so the name, schema
// and description an agent sees are identical across transports by
// construction; TestAgentMemoryDescriptionsMatchAcrossTransports
// checks it.
func AgentMemoryTools(d AgentMemoryDispatcher) []Tool {
	native := agent.AgentMemoryTools()
	tools := make([]Tool, 0, len(native))
	for _, t := range native {
		tools = append(tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			ReadOnly:    t.Name == agent.AgentMemoryViewToolName || t.Name == agent.AgentHabitsViewToolName,
			Handler:     agentMemoryHandler(d, t.Name),
		})
	}
	return tools
}

// agentMemoryHandler wraps an AgentMemoryDispatcher into the MCP
// handler shape, returning a ToolResult populated from the dispatch.
// Transport errors (non-2xx HTTP, etc.) surface as the MCP handler's
// returned error; tool-level failures (validation, str-replace
// uniqueness, the rotation-only gate on habits) ride on isError +
// body.
func agentMemoryHandler(d AgentMemoryDispatcher, name string) func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
	return func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
		body, isError, err := d.DispatchAgentMemoryTool(ctx, name, raw)
		if err != nil {
			return &ToolResult{IsError: true, Content: []string{name + ": " + err.Error()}}, nil
		}
		return &ToolResult{Content: []string{body}, IsError: isError}, nil
	}
}
