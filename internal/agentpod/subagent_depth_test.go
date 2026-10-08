package agentpod

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestSubagentToolsByDepth pins the rule that decides whether a
// subagent can delegate. A leaf must not be handed the tool: a model
// shown a tool it will be refused for using will try it, and the
// refusal arrives as a tool error in the middle of real work.
func TestSubagentToolsByDepth(t *testing.T) {
	subLead := SubagentTools(1)
	if !slices.Contains(subLead.Kivali, "subagent") {
		t.Errorf("depth 1 tools = %v, want the subagent tool present", subLead.Kivali)
	}

	leaf := SubagentTools(MaxSubagentDepth)
	if slices.Contains(leaf.Kivali, "subagent") {
		t.Errorf("depth %d tools = %v, want no subagent tool at the last tier", MaxSubagentDepth, leaf.Kivali)
	}

	// Whatever the tiers do differently, they get the same working
	// tools — a worker is not a lesser agent, just a terminal one.
	for _, want := range []string{"file_view", "run_shell"} {
		if !slices.Contains(leaf.Kivali, want) {
			t.Errorf("leaf tools missing %s: %v", want, leaf.Kivali)
		}
	}
	if !slices.Contains(leaf.Builtins, provider.BuiltinWebSearch) || !slices.Contains(leaf.Builtins, provider.BuiltinWebFetch) {
		t.Errorf("leaf builtins = %v, want web_fetch and web_search", leaf.Builtins)
	}
	// Policy speaks in bare names; the provider's spelling is the
	// driver's. A prefixed name here would be spelled twice.
	for _, n := range append(subLead.Kivali, leaf.Kivali...) {
		if strings.Contains(n, "__") {
			t.Errorf("tool %q carries a provider prefix; SubagentTools names tools bare", n)
		}
	}
}

// TestSubagentPromptMatchesItsTier checks the two things this prompt
// alone decides: whether splitting is offered, and whether the subagent
// is told it is terminal.
//
// The operational contract of nested dispatch — that it blocks, that
// abandoning workers is possible — is deliberately NOT asserted here.
// It lives in the tool's own description, which is in context whenever
// this paragraph is, and stating it in both places costs tokens on
// every spawn and gives the copies somewhere to disagree.
// TestNestedToolStatesTheBlockingContract in internal/mcp pins it.
func TestSubagentPromptMatchesItsTier(t *testing.T) {
	subLead := SubagentSystemPrompt("chief-of-staff", "review the draft", 1)
	if !strings.Contains(subLead, "DELEGATING") {
		t.Error("sub-lead prompt does not offer splitting")
	}
	if !strings.Contains(subLead, "/files/background/plan.md") {
		t.Error("sub-lead prompt does not point at the shared plan")
	}

	leaf := SubagentSystemPrompt("chief-of-staff", "grep the logs", MaxSubagentDepth)
	if strings.Contains(leaf, "DELEGATING") {
		t.Error("leaf prompt offers delegation it cannot do")
	}
	if !strings.Contains(leaf, "last tier") {
		t.Error("leaf prompt does not tell the worker it is terminal")
	}

	// Every tier is told it cannot ask anyone anything, because every
	// tier is one-shot. A worker that does not know this waits for a
	// clarification that can never arrive, or guesses silently.
	for name, p := range map[string]string{"sub-lead": subLead, "leaf": leaf} {
		if !strings.Contains(p, "IF YOU ARE BLOCKED") {
			t.Errorf("%s prompt does not tell it what to do when it cannot proceed", name)
		}
		if !strings.Contains(p, "do not edit it") {
			t.Errorf("%s prompt does not warn it off editing the shared plan", name)
		}
	}
}

// TestSubagentSpecDepthDefaultsToTierOne covers the rolling-deploy
// case: a spec serialised before depth existed decodes with Depth 0,
// and 0 must read as tier 1. Read literally it would be "shallower
// than the agent's own subagents", which CanDelegate would happily
// treat as allowed to delegate — turning old leaves into sub-leads
// mid-deploy.
func TestSubagentSpecDepthDefaultsToTierOne(t *testing.T) {
	var spec SubagentSpec
	if err := json.Unmarshal([]byte(`{"subagent_id":"abc","user_prompt":"hi"}`), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := spec.EffectiveDepth(); got != 1 {
		t.Errorf("EffectiveDepth() on a depth-less spec = %d, want 1", got)
	}
	if !CanDelegate(spec.EffectiveDepth()) {
		t.Error("tier 1 should be able to delegate")
	}
	if CanDelegate(MaxSubagentDepth) {
		t.Error("the last tier must not be able to delegate")
	}
}
