package mcp

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestStateToolDescriptionsMatchAcrossTransports pins the two
// hand-maintained copies of every state-tool description to each other.
//
// The same tools are declared twice: internal/agent/state_tools.go
// builds the native-API definitions BuildRequest puts in req.Tools, and
// this package builds the MCP definitions the SDK transport serves. Both
// are live, both reach a model, and nothing but this test stops one from
// being edited while the other is not — which would mean an agent's
// instructions for the same tool depend on which transport it happens to
// be running under. agent/state_tools.go already asks for them to be
// "kept verbatim aligned"; this makes the ask enforceable.
//
// Only shared names are compared. The native list also carries
// search_past_chats and list_project_files, whose MCP twins live in
// mcp/search_tools.go and mcp/project_files_tool.go.
func TestStateToolDescriptionsMatchAcrossTransports(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	d := fakeStateDispatcher{deps: StateDispatchDeps{Store: s, Slug: "alice", IsChiefOfStaff: true}}

	mcpSide := map[string]string{}
	for _, tool := range StateTools(d, true) {
		mcpSide[tool.Name] = tool.Description
	}

	shared := 0
	for _, native := range agent.StateTools(true) {
		want, ok := mcpSide[native.Name]
		if !ok {
			continue
		}
		shared++
		if native.Description != want {
			t.Errorf("tool %q describes itself differently per transport.\nnative (internal/agent/state_tools.go):\n%s\n\nMCP (internal/mcp/state_tools.go):\n%s",
				native.Name, native.Description, want)
		}
	}
	// Guard the guard: if the lists stop overlapping, the loop above
	// passes while comparing nothing.
	if shared < 3 {
		t.Errorf("expected at least the 3 shared state tools to be compared, got %d", shared)
	}
}
