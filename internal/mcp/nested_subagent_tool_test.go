package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestNestedToolStatesTheBlockingContract guards the sentences that
// keep nesting from silently losing work.
//
// A sub-lead that assumed the durable agent's fire-and-forget contract
// would dispatch, end its turn, and exit — abandoning every worker,
// with no error anywhere for anyone to see. This description is the
// only thing standing between that model and that mistake, which is
// why it is asserted rather than left to read well.
//
// It lives here rather than in the system prompt on purpose: the
// description is in context whenever the tool is, and saying it twice
// costs tokens on every spawn and gives the copies room to diverge.
func TestNestedToolStatesTheBlockingContract(t *testing.T) {
	desc := NestedSubagentTool(NestedSubagentToolConfig{Provider: provider.MockProvider{}}).Description

	if !strings.Contains(desc, "BLOCKS") {
		t.Error("does not say the call blocks")
	}
	if !strings.Contains(desc, "one-shot process") {
		t.Error("does not explain why it cannot dispatch and end its turn")
	}
	if !strings.Contains(desc, "abandoned") {
		t.Error("does not say what happens to work still running when it finishes")
	}
	// A worker sees its prompt and the shared workspace, nothing else.
	// A sub-lead that assumes otherwise writes prompts referring to
	// conversation the worker cannot see.
	if !strings.Contains(desc, "no view of your conversation") {
		t.Error("does not warn that workers cannot see the sub-lead's context")
	}
	// The failure that makes delegating worse than not delegating.
	if !strings.Contains(desc, "RAW MATERIAL, NOT YOUR ANSWER") {
		t.Error("does not frame worker replies as raw material")
	}
	if !strings.Contains(desc, "Workers cannot delegate further") {
		t.Error("does not say this is the last tier that can split work")
	}
}

// TestNestedToolOffersModelAndEffort keeps the cost lever available one
// tier down. A sub-lead's workers are usually the narrowest, most
// mechanical tasks in a job — exactly where a cheap model pays — so
// dropping these from the schema would quietly push every nested worker
// onto the default.
func TestNestedToolOffersModelAndEffort(t *testing.T) {
	schema := string(NestedSubagentTool(NestedSubagentToolConfig{Provider: provider.MockProvider{}}).InputSchema)
	for _, want := range []string{`"model"`, `"effort"`, provider.MockModelSmall} {
		if !strings.Contains(schema, want) {
			t.Errorf("nested tool schema is missing %s", want)
		}
	}
}

// TestSubagentToolkitGatesDelegationOnTheBackend pins the structural
// half of the depth rule: a leaf's MCP server does not implement
// `subagent` at all, so no --allowedTools mistake can hand one out.
func TestSubagentToolkitGatesDelegationOnTheBackend(t *testing.T) {
	has := func(tk Toolkit, name string) bool {
		for _, tool := range tk.Tools {
			if tool.Name == name {
				return true
			}
		}
		return false
	}

	leaf := SubagentToolkit(SubagentDeps{SubagentID: "s1", Files: nopFilesDispatcher{}})
	if has(leaf, SubagentToolName) {
		t.Error("a leaf toolkit exposes the subagent tool")
	}

	subLead := SubagentToolkit(SubagentDeps{SubagentID: "s1", Files: nopFilesDispatcher{}, NestedSubagent: stubNested{}, Provider: provider.MockProvider{}})
	if !has(subLead, SubagentToolName) {
		t.Error("a sub-lead toolkit is missing the subagent tool")
	}
	// Job control is deliberately absent at every tier: a sub-lead's
	// dispatch blocks, so it never holds outstanding work it could ask
	// about or call off.
	for _, n := range []string{SubagentStatusToolName, SubagentCancelToolName} {
		if has(subLead, n) {
			t.Errorf("sub-lead toolkit exposes %s, which can only ever answer \"still waiting\"", n)
		}
	}
}

type stubNested struct{}

func (stubNested) RunNestedSubagent(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}
