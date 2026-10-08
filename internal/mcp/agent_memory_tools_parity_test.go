package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
)

// nopAgentMemoryDispatcher satisfies AgentMemoryDispatcher for tests
// that only inspect the tool definitions, never invoke a handler.
type nopAgentMemoryDispatcher struct{}

func (nopAgentMemoryDispatcher) DispatchAgentMemoryTool(context.Context, string, json.RawMessage) (string, bool, error) {
	return "", false, nil
}

// TestAgentMemoryDescriptionsMatchAcrossTransports is the agent_memory_*
// twin of TestStateToolDescriptionsMatchAcrossTransports.
//
// The family is declared twice — agent.AgentMemoryTools() for the
// native-API path and AgentMemoryTools() here for the SDK/MCP path —
// and mcp/agent_memory_tools.go claims the two are "kept verbatim
// aligned ... so model behavior is identical across transports". That
// claim was untrue and unenforced: agent_memory_str_replace's
// byte-exactness paragraph (copy characters from a prior
// agent_memory_view rather than retyping them) existed only on the
// native side, so agents on the SDK transport — the one pods actually
// run — got the ErrAgentMemoryStrFoldOnly error without ever having
// been given the guidance that prevents it.
func TestAgentMemoryDescriptionsMatchAcrossTransports(t *testing.T) {
	mcpSide := map[string]string{}
	for _, tool := range AgentMemoryTools(nopAgentMemoryDispatcher{}) {
		mcpSide[tool.Name] = tool.Description
	}

	shared := 0
	for _, native := range agent.AgentMemoryTools() {
		want, ok := mcpSide[native.Name]
		if !ok {
			t.Errorf("tool %q exists on the native path but not the MCP path", native.Name)
			continue
		}
		shared++
		if native.Description != want {
			t.Errorf("tool %q describes itself differently per transport.\nnative (internal/agent/agent_memory_tool.go):\n%s\n\nMCP (internal/mcp/agent_memory_tools.go):\n%s",
				native.Name, native.Description, want)
		}
	}
	// Guard the guard: an empty or shrunken family would let the loop
	// above pass while comparing nothing. Three verbs on each of two
	// documents (agent_memory.md, agent_memory_habits.md).
	if shared != 6 {
		t.Errorf("expected all 6 agent_memory_* tools to be compared, got %d", shared)
	}
}
