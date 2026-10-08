package mcp

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/files"
)

// TestReportSubagentStartupCost measures what a subagent pays before it
// has done anything: its system prompt plus every tool definition its
// toolkit registers. Diagnostic — run with -v.
//
// This is the number that matters for the cheapest tasks. A subagent
// dispatched to run one grep and report a count does perhaps two API
// calls; everything below is charged on both of them, so for that class
// of task the framing IS the cost.
func TestReportSubagentStartupCost(t *testing.T) {
	tk := SubagentToolkit(SubagentDeps{
		SubagentID: "s1",
		Files:      NewLocalFilesDispatcher(&files.Backend{Root: t.TempDir()}),
	})

	toolChars := 0
	for _, tool := range tk.Tools {
		toolChars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	prompt := agentpod.SubagentSystemPrompt("chief-of-staff", "count severity-1 incidents", agentpod.MaxSubagentDepth)

	t.Logf("leaf subagent toolkit: %d tools, %d chars (~%d tokens)", len(tk.Tools), toolChars, toolChars/4)
	t.Logf("leaf system prompt:              %d chars (~%d tokens)", len(prompt), len(prompt)/4)
	t.Logf("TOTAL before any work:           %d chars (~%d tokens)", toolChars+len(prompt), (toolChars+len(prompt))/4)
}

// TestSubagentPromptPrefixIsStableAcrossSpawns guards the property that
// lets the static half of the prompt be cached.
//
// Prompt caching keys on a shared leading prefix. Anything that varies
// per spawn — the task label most of all — invalidates every token
// after it, so putting the variable part first means the ~1000 static
// tokens below it can never be reused between subagents. Two spawns of
// the same tier must therefore share a long identical prefix.
func TestSubagentPromptPrefixIsStableAcrossSpawns(t *testing.T) {
	a := agentpod.SubagentSystemPrompt("chief-of-staff", "count severity-1 incidents", agentpod.MaxSubagentDepth)
	b := agentpod.SubagentSystemPrompt("chief-of-staff", "summarise the renewal terms", agentpod.MaxSubagentDepth)

	shared := 0
	for shared < len(a) && shared < len(b) && a[shared] == b[shared] {
		shared++
	}
	t.Logf("shared prefix between two spawns: %d of %d chars (~%d tokens cacheable)", shared, len(a), shared/4)

	// The static body is the bulk of the prompt; if the differing part
	// sits at the front, almost none of it is shared.
	if shared < len(a)/2 {
		t.Errorf("only %d of %d chars are a shared prefix — the per-task text is early in the prompt, "+
			"so the static remainder cannot be cached across spawns. Move what varies to the end.",
			shared, len(a))
	}
}
