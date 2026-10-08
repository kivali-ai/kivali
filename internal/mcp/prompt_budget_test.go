package mcp

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
)

// Tool descriptions and subagent system prompts are ALWAYS-ON text.
// A tool description rides in the system prompt of every request the
// agent makes, for the life of the agent; a subagent's system prompt is
// paid on every spawn, before the subagent has done anything. Prose
// that is merely nice to have is therefore charged thousands of times.
//
// The budgets below are not a quality bar — nothing here can tell good
// writing from bad. They exist because this text has no natural
// pressure against growth: every individual sentence looks worth
// adding, and the cost shows up somewhere nobody is looking. A failure
// means "justify this", not "you are wrong". Raising a number is a fine
// resolution; doing it without noticing is what this prevents.
//
// Detail that only matters once a job is under way belongs in a skill
// instead — read on demand, paid once, and editable without a deploy.
const (
	// The durable agent's dispatch tool. Read on every turn of every
	// agent in the fleet, whether or not it delegates that day.
	maxSubagentDescChars = 3600
	// A sub-lead's dispatch tool. Present only for subagents that can
	// delegate, but then paid on every one of their turns.
	maxNestedDescChars = 1900
	// Schemas are terser than prose and grow by field, not paragraph;
	// the ceilings are here so an added field is a visible decision.
	//
	// Raised from 1400 to fit the model enum. Listing the deployment's
	// models costs ~43 tokens per use and removes a whole class of
	// runtime failure — an agent guessing an id from its training data,
	// the subagent dying at spawn on a CLI error minutes later. Worth
	// paying for; recorded here so it stays a decision rather than
	// drift.
	maxSubagentSchemaChars = 1500
	maxNestedSchemaChars   = 1300
	// A subagent's whole system prompt. The most expensive of the lot
	// in aggregate: a batch of five spawns pays it five times, and a
	// cheap Haiku lookup pays it before earning anything.
	maxSubagentPromptChars = 4600
	// The publishing pair. On every turn of every agent and subagent;
	// the publish description carries the source/dest rules and the
	// limits, which the model otherwise learns one refusal at a time.
	maxArtifactPublishDescChars   = 1400
	maxArtifactUnpublishDescChars = 400
)

func TestAlwaysOnPromptBudgets(t *testing.T) {
	cases := []struct {
		name string
		text string
		max  int
	}{
		{"subagent tool description", SubagentTool(SubagentToolConfig{Provider: budgetProvider{}}).Description, maxSubagentDescChars},
		{"subagent tool schema", string(SubagentTool(SubagentToolConfig{Provider: budgetProvider{}}).InputSchema), maxSubagentSchemaChars},
		{"nested tool description", NestedSubagentTool(NestedSubagentToolConfig{Provider: budgetProvider{}}).Description, maxNestedDescChars},
		{"nested tool schema", string(NestedSubagentTool(NestedSubagentToolConfig{Provider: budgetProvider{}}).InputSchema), maxNestedSchemaChars},
		{"artifact_publish description", agent.ArtifactPublishTool().Description, maxArtifactPublishDescChars},
		{"artifact_unpublish description", agent.ArtifactUnpublishTool().Description, maxArtifactUnpublishDescChars},
		{
			"subagent system prompt (sub-lead)",
			agentpod.SubagentSystemPrompt("chief-of-staff", "review the draft", 1),
			maxSubagentPromptChars,
		},
		{
			"subagent system prompt (leaf)",
			agentpod.SubagentSystemPrompt("chief-of-staff", "grep the logs", agentpod.MaxSubagentDepth),
			maxSubagentPromptChars,
		},
	}

	for _, c := range cases {
		// Roughly four characters per token — close enough to reason
		// about, and the character count is what the budget pins.
		t.Logf("%-36s %5d chars (~%d tokens)", c.name, len(c.text), len(c.text)/4)
		if len(c.text) > c.max {
			t.Errorf("%s is %d chars, over its %d budget (~%d extra tokens on every use).\n"+
				"Move detail that only matters mid-job into the plan-and-fan-out skill, "+
				"or raise the budget deliberately and say why.",
				c.name, len(c.text), c.max, (len(c.text)-c.max)/4)
		}
	}
}

// budgetProvider is a provider as large as the largest one Kivali
// ships — four models on offer, five efforts each, ids at least as
// long as the real ones — so the schema budgets measure what an agent
// on that provider actually reads, not the two-model mock.
type budgetProvider struct{ provider.MockProvider }

func (budgetProvider) Models() []provider.ModelInfo {
	efforts := []provider.Effort{
		{ID: "low", Label: "Low"}, {ID: "medium", Label: "Medium"}, {ID: "high", Label: "High", Default: true},
		{ID: "xhigh", Label: "Extra high"}, {ID: "max", Label: "Max"},
	}
	var out []provider.ModelInfo
	for _, id := range []string{"provider-small-4-5", "provider-medium-5", "provider-large-5-5", "provider-xlarge-5-1"} {
		out = append(out, provider.ModelInfo{ID: id, Label: id, Current: true, ContextWindow: 1_000_000, Efforts: efforts})
	}
	return out
}

// TestTierPromptsDoNotDivergeInSize is a smell test for duplication.
//
// The two tiers differ by one short paragraph: whether splitting is
// offered. If the sub-lead's prompt grows much larger than the leaf's,
// it usually means the nested tool's own description has been copied
// back into the system prompt — which costs tokens on every spawn and
// gives the two copies somewhere to disagree.
func TestTierPromptsDoNotDivergeInSize(t *testing.T) {
	subLead := agentpod.SubagentSystemPrompt("cos", "review the draft", 1)
	leaf := agentpod.SubagentSystemPrompt("cos", "grep the logs", agentpod.MaxSubagentDepth)

	diff := len(subLead) - len(leaf)
	if diff < 0 {
		diff = -diff
	}
	const maxTierDiff = 600
	if diff > maxTierDiff {
		t.Errorf("tier prompts differ by %d chars (max %d): the delegation frame should say what the "+
			"tool description cannot, not repeat it", diff, maxTierDiff)
	}
}
