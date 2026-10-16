package agentpod

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestSubagentTurnHandsTheSchemaToTheMCPServer: a structured task's
// schema reaches the subagent's MCP server as a file in the run dir,
// named by --result-schema, so submit_result can show the subagent the
// shape core will check. A free-text task passes no such flag.
func TestSubagentTurnHandsTheSchemaToTheMCPServer(t *testing.T) {
	fc := startFakeCore(t)
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}}}`)
	var seenArgs []string
	var seenSchema []byte
	mc := &provider.MockClient{
		RunSubagentFn: func(_ context.Context, req provider.SubagentRequest) (provider.Stream, error) {
			if len(req.MCPServers) == 1 {
				seenArgs = req.MCPServers[0].Args
				for i, a := range seenArgs {
					if a == "--result-schema" && i+1 < len(seenArgs) {
						seenSchema, _ = os.ReadFile(seenArgs[i+1])
					}
				}
			}
			return provider.NewMockStream(
				[]provider.StreamEvent{{Kind: provider.StreamEnd}},
				&provider.CompleteResponse{StopReason: provider.StopEndTurn},
			), nil
		},
	}
	rt := subagentRuntime(t, fc, mc)
	spec := SubagentSpec{SubagentID: "sub9", UserPrompt: "count them", Tools: SubagentTools(2, true), Schema: schema}
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-schema", 2, spec)); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(rt.ScratchRoot, "subagents", "sub9", resultSchemaFile)
	if i := slices.Index(seenArgs, "--result-schema"); i < 0 || i+1 >= len(seenArgs) || seenArgs[i+1] != wantPath {
		t.Errorf("MCP args = %v, want --result-schema %s", seenArgs, wantPath)
	}
	if string(seenSchema) != string(schema) {
		t.Errorf("schema file held %q, want the spec's schema", seenSchema)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Errorf("schema file still present after the run (stat err %v)", err)
	}

	// No schema, no flag.
	seenArgs = nil
	free := SubagentSpec{SubagentID: "sub10", UserPrompt: "count them", Tools: SubagentTools(2, false)}
	if err := rt.handleSubagentTurn(context.Background(), subagentTurnEvent(t, "t-free", 2, free)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(seenArgs, "--result-schema") {
		t.Errorf("free-text task passed --result-schema: %v", seenArgs)
	}
}

// TestSubagentToolsAndPromptFollowTheSchema: a task with a schema gets
// submit_result and a result contract that names it; a free-text task
// gets neither, so no subagent is shown a tool it cannot use.
func TestSubagentToolsAndPromptFollowTheSchema(t *testing.T) {
	for _, depth := range []int{1, MaxSubagentDepth} {
		if !slices.Contains(SubagentTools(depth, true).Kivali, SubmitResultToolName) {
			t.Errorf("depth %d structured tools lack %s", depth, SubmitResultToolName)
		}
		if slices.Contains(SubagentTools(depth, false).Kivali, SubmitResultToolName) {
			t.Errorf("depth %d free-text tools carry %s", depth, SubmitResultToolName)
		}
	}
	structured := SubagentSystemPrompt("chief-of-staff", "price the renewal", 1, true)
	free := SubagentSystemPrompt("chief-of-staff", "price the renewal", 1, false)
	if !strings.Contains(structured, "`submit_result`") || !strings.Contains(structured, "nothing you write as plain text reaches them") {
		t.Error("structured prompt does not route the answer through submit_result")
	}
	if strings.Contains(structured, "LARGE ANSWERS") || strings.Contains(structured, "final assistant message is what gets returned") {
		t.Error("structured prompt still describes a free-text answer")
	}
	if strings.Contains(free, "submit_result") {
		t.Error("free-text prompt mentions submit_result")
	}
	for name, p := range map[string]string{"structured": structured, "free": free} {
		if !strings.Contains(p, "IF YOU ARE BLOCKED") {
			t.Errorf("%s prompt lacks the blocked paragraph", name)
		}
	}
	// The tail is the only per-task text, for both kinds.
	if !strings.HasSuffix(structured, "for this task: price the renewal.\n") {
		t.Errorf("structured prompt does not end with the per-task tail: %q", structured[len(structured)-80:])
	}
}
