// Package mcp (nested_subagent_tool.go) — the `subagent` tool as a
// SUB-LEAD sees it.
//
// A durable agent's subagent call is fire-and-forget: it returns a
// receipt, the agent ends its turn, and each answer is delivered later
// as a message that wakes it. None of that is available one tier down.
// A subagent is a one-shot `claude -p` whose stdin closes after its
// prompt, so there is no later moment at which anything could reach
// it; if it ended its turn with workers outstanding, their answers
// would have nowhere to go.
//
// So a sub-lead's dispatch BLOCKS and returns the answers themselves.
// Same tool name, same input shape, deliberately opposite contract —
// which is why the description is written separately rather than
// shared and parameterised. The one sentence that matters most to the
// model ("this returns a receipt" vs "this returns answers") is the
// one that would get lost in a template.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
)

// NestedSubagentBackend is what a sub-lead's dispatch proxies to. The
// implementation reaches core over the control socket; core owns the
// depth rule, the concurrency budget, and the jobs themselves.
//
// CallerID is the sub-lead's OWN subagent id. Core uses it to find the
// dispatching job in its registry and derive the new jobs' depth from
// what it recorded there — never from anything in this request. A
// subagent that could state its own depth could state 0.
type NestedSubagentBackend interface {
	RunNestedSubagent(ctx context.Context, callerID string, arguments json.RawMessage) (rendered string, err error)
}

// NestedSubagentToolConfig wires the sub-lead's dispatch tool.
type NestedSubagentToolConfig struct {
	Backend  NestedSubagentBackend
	CallerID string
	// Provider supplies the schema's models and efforts. Required.
	Provider provider.Provider
}

// NestedSubagentTool returns the `subagent` tool definition for a
// subagent that is allowed to delegate. Registered under the same name
// a durable agent uses, so nothing downstream needs to know which tier
// made a call.
func NestedSubagentTool(cfg NestedSubagentToolConfig) Tool {
	return Tool{
		Name:        SubagentToolName,
		Description: nestedSubagentDescription,
		InputSchema: nestedSubagentInputSchema(requireProvider("NestedSubagentTool", cfg.Provider)),
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			if cfg.Backend == nil {
				return &ToolResult{IsError: true, Content: []string{"subagent: backend not configured (web control socket unreachable)"}}, nil
			}
			if cfg.CallerID == "" {
				return &ToolResult{IsError: true, Content: []string{"subagent: caller id not configured"}}, nil
			}
			rendered, err := cfg.Backend.RunNestedSubagent(ctx, cfg.CallerID, raw)
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"subagent: " + err.Error()}}, nil
			}
			return &ToolResult{Content: []string{rendered}}, nil
		},
	}
}

func nestedSubagentInputSchema(p provider.Provider) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "tasks": {
      "type": "array",
      "minItems": 1,
      "maxItems": %d,
      "items": {
        "type": "object",
        "properties": {
          "description": {"type": "string", "description": "short label (≤ 8 words) for this piece of work"},
          "prompt": {"type": "string", "description": "the instruction this worker receives verbatim. It sees only this prompt and the shared /files/background/ — none of your conversation — so name the files and say what output you want where."},
          %s
        },
        "required": ["description", "prompt"]
      }
    }
  },
  "required": ["tasks"]
}`, agentpod.SubagentMaxNestedFanout, modelEffortProperties(p, "Go lower for mechanical or lookup work.")))
}

var nestedSubagentDescription = fmt.Sprintf(`Split your task across up to %d workers running in parallel.

THIS CALL BLOCKS AND RETURNS THEIR ANSWERS. It is not fire-and-forget — do not end your turn expecting results later. You are a one-shot process: anything still running when you finish is abandoned, and nothing can wake you to collect it. Dispatch, wait here, read what comes back, then write your own answer.

Each worker is a fresh model conversation with no view of your conversation. It gets its prompt, the same read-only files you can see, and the SHARED /files/background/ directory. Everything it needs to know has to be in the prompt or in a file you point it at.

Use this when your task has genuinely independent pieces: different sources to check, separate files to process, a draft and an independent review of it. Don't delegate what you could finish in the time it takes to write the instruction, and don't split something whose parts each need the others' answers — that is one task, not several.

Workers cannot delegate further. You are the last tier that can split work.

WHAT COMES BACK IS RAW MATERIAL, NOT YOUR ANSWER. Reconcile it: where two workers disagree, resolve it or say which you trust and why; where one is thin, fill the gap yourself; where one is confidently wrong, say so. Pasting their replies end to end is the one outcome that makes delegating worse than not — your caller delegated to you so that exactly one agent would be accountable for this piece.

tasks[] is 1..%d objects: description (short label), prompt (verbatim instruction), optional model (omit for the default), optional effort (omit for the model's default).`,
	agentpod.SubagentMaxNestedFanout, agentpod.SubagentMaxNestedFanout)
