package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
)

// SubagentMaxBatch is the cap on tasks per single batched subagent call.
// Surfaced in the tool description so the model doesn't learn it from
// rejection. Five is a guess — measure and adjust if real workloads
// pile up against the ceiling (agentpod.SubagentMaxBatch, which the
// control client's dispatch timeout is sized from).
const SubagentMaxBatch = agentpod.SubagentMaxBatch

// SubagentToolName is the MCP-side name, and the bare name core sees
// on every event: how a provider spells it on its own wire is the
// driver's concern (internal/claudeagent).
const SubagentToolName = "subagent"

// SubagentStatusToolName / SubagentCancelToolName are the job-control
// companions to `subagent`. They exist because dispatch is
// fire-and-forget: once a call returns a receipt rather than an answer,
// an agent needs some way to ask what is still outstanding and to call
// off work it no longer wants.
//
// There is deliberately no "get result" tool. Results are delivered as
// messages when they land; a getter would invite an agent to sit in a
// polling loop, which is exactly the blocking behaviour async dispatch
// exists to remove.
const (
	SubagentStatusToolName = "subagent_status"
	SubagentCancelToolName = "subagent_cancel"
)

// SubagentToolConfig wires the subagent MCP tool. Backend is the
// service that actually runs the subagents — in production it's a
// controlclient that proxies to the parent web process. Parent is
// the slug of the agent calling subagent.
type SubagentToolConfig struct {
	Backend SubagentBackend
	Parent  string
	// Provider supplies the schema's models and efforts. Required.
	Provider provider.Provider
}

// requireProvider refuses a dispatch tool built without a provider:
// its schema could not name a model, and a wiring mistake should stop
// the MCP server at startup rather than advertise an empty enum.
func requireProvider(constructor string, p provider.Provider) provider.Provider {
	if p == nil {
		panic("mcp." + constructor + ": Provider is nil; wire the driver (claudeagent.New) in the composition root")
	}
	return p
}

// SubagentTool returns the MCP tool definition for `subagent`. The
// handler does NO work itself — input is forwarded as-is to the
// configured Backend, which lives in the parent web process. This
// keeps the MCP subprocess stateless wrt subagents and gives the
// parent a single place to run them, watch them, and broadcast
// progress.
func SubagentTool(cfg SubagentToolConfig) Tool {
	return Tool{
		Name:        SubagentToolName,
		Description: subagentDescription,
		InputSchema: subagentInputSchema(requireProvider("SubagentTool", cfg.Provider)),
		Handler:     subagentHandler(cfg),
	}
}

// subagentHandler is a pure RPC proxy. The web process owns the
// runner, transcript persistence, and chip live-update emission;
// the handler here just shuttles the request and the response.
func subagentHandler(cfg SubagentToolConfig) func(context.Context, json.RawMessage) (*ToolResult, error) {
	return func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
		if cfg.Backend == nil {
			return &ToolResult{IsError: true, Content: []string{"subagent: backend not configured (web control socket unreachable)"}}, nil
		}
		if cfg.Parent == "" {
			return &ToolResult{IsError: true, Content: []string{"subagent: parent slug not configured"}}, nil
		}
		rendered, err := cfg.Backend.RunSubagent(ctx, cfg.Parent, raw)
		if err != nil {
			return &ToolResult{IsError: true, Content: []string{"subagent: " + err.Error()}}, nil
		}
		return &ToolResult{Content: []string{rendered}}, nil
	}
}

// SubagentStatusTool returns the MCP tool definition for
// `subagent_status`. Like SubagentTool, the handler is a pure proxy —
// core owns the job registry and renders the text, so there is exactly
// one description of what a job's state looks like.
func SubagentStatusTool(cfg SubagentToolConfig) Tool {
	return Tool{
		Name: SubagentStatusToolName,
		Description: `List your background subagent tasks — what is still running, what has finished, and how long each has been going.

Running tasks show their current step and how long since they last did anything. A task that has been silent far longer than its work plausibly takes is worth a look; there is no automatic judgement about this, so use yours.

Finished tasks have already sent you their results as messages — this is a roster, not a way to re-read them. No input parameters.`,
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		Handler: func(ctx context.Context, _ json.RawMessage) (*ToolResult, error) {
			if cfg.Backend == nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_status: backend not configured (web control socket unreachable)"}}, nil
			}
			out, err := cfg.Backend.SubagentStatus(ctx, cfg.Parent)
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_status: " + err.Error()}}, nil
			}
			return &ToolResult{Content: []string{out}}, nil
		},
	}
}

// SubagentCancelTool returns the MCP tool definition for
// `subagent_cancel`.
func SubagentCancelTool(cfg SubagentToolConfig) Tool {
	return Tool{
		Name: SubagentCancelToolName,
		Description: `Stop one of your running background subagent tasks.

Use when a task has become irrelevant (you have the answer another way, the plan changed) or has clearly wedged. A cancelled task sends you no result — you asked it to stop, so there is nothing to report — but anything it already wrote under /files/subagents/<id>/artifacts/private/ stays.

Cancelling is not free: whatever it had done is lost. Prefer letting a slow-but-progressing task finish.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "string", "description": "the task id, as shown by subagent_status or by the dispatch receipt"}
  },
  "required": ["id"]
}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			if cfg.Backend == nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_cancel: backend not configured (web control socket unreachable)"}}, nil
			}
			var in struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_cancel: invalid input: " + err.Error()}}, nil
			}
			out, err := cfg.Backend.SubagentCancel(ctx, cfg.Parent, in.ID)
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_cancel: " + err.Error()}}, nil
			}
			return &ToolResult{Content: []string{out}}, nil
		},
	}
}

func subagentInputSchema(p provider.Provider) json.RawMessage {
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
          "description": {"type": "string", "description": "short label (≤ 8 words) describing this task — shown in the parent's chat as a chip"},
          "prompt": {"type": "string", "description": "the actual instruction the subagent will receive verbatim. Reference your own /files/ paths inline (e.g. /files/project/data.csv) — the subagent inherits a narrowed view of your filesystem"},
          %s,
          %s
        },
        "required": ["description", "prompt"]
      }
    }
  },
  "required": ["tasks"]
}`, SubagentMaxBatch, modelEffortProperties(p,
		"Leave it unset for normal work; go lower for mechanical or lookup tasks (faster, cheaper) and to the highest levels only for genuinely hard analysis/planning where deep reasoning is worth the extra latency and tokens."),
		schemaProperty))
}

// schemaProperty is the `schema` task field, shared by the agent's
// dispatch tool and the sub-lead's: the one place the subset is spelled
// out for the model. webTaskProperties are the fields of a web task,
// shared the same way.
const schemaProperty = `"schema": {"type": "object", "description": "optional: a JSON Schema for the answer as data. An object of properties typed string (maxLength required; or enum; or format date, date-time or uri), number, integer, boolean, array (with items) or nested object. The subagent then finishes through submit_result and you receive JSON {outcome, result, alternatives, blocked_on, files} checked against it."},
          "kind": {"type": "string", "enum": ["general", "web"], "description": "web runs the task in a browser instead of a shell and needs a schema; general is the default"},
          "session": {"type": "string", "description": "web only: open (the default: signed in nowhere, nothing kept between tasks) or a site such as vendor.com: a session locked to that site and its subdomains that keeps its sign-ins between tasks"},
          "start_url": {"type": "string", "description": "web only: the http or https page to start at; needed with files"},
          "files": {"type": "array", "items": {"type": "string"}, "maxItems": 10, "description": "web only: paths under your /files/ the task may upload to a page, by name, once a person approves"}`

var subagentDescription = fmt.Sprintf(`Dispatch focused subagent(s) to run IN THE BACKGROUND — a way to spend tokens without growing your own context. See Kivali OS §Delegating with subagent for when to use it.

THIS CALL RETURNS IMMEDIATELY WITH A RECEIPT, NOT AN ANSWER. The tasks are still running when you read the result. Each one's answer arrives later as a separate message, and you will be woken for it if you have finished your turn. So:
  - Do NOT re-dispatch a task because the receipt contained no answer.
  - Do NOT try to wait for results inside this turn — you cannot, and a "check status" loop just burns tokens. Say what you have dispatched, do any work that does not depend on the results, and end your turn.
  - Track them with subagent_status; call off work you no longer want with subagent_cancel.

A subagent runs as a fresh model conversation with a minimal system prompt and a narrowed view of YOUR /files/:
  - /files/artifacts/private/  — its own scratch + deliverables (real dir; persists for you to read after).
  - /files/artifacts/public/   — your published artifacts (read-only; it can artifact_publish into it).
  - /files/project/            — your project files (read-only).
  - /files/skills/             — same skills you have (read-only).
  - /files/attachments/        — your attachments by name (read-only).
  - /files/background/              — your shared workspace, WRITABLE by you and by every subagent you dispatch. Same directory for all of them.
  - run_shell and file_* share this filesystem; bash and file_view see the same paths.

/files/background/ is shared with every subagent you dispatch: point prompts at files there, and collect their output from there rather than from returned messages. For work worth planning, see Kivali OS §Breaking work down.

It does NOT have access to: your chat history, past chats, the org chart, publish_*, agent_memory_*, or share_file.

A subagent can split its own task further, once. So a task that is itself a small project should be dispatched WHOLE rather than pre-broken into pieces you would then have to reassemble — that subagent will break it down, wait for its workers, and hand you back one reconciled answer.

Hand the subagent specific files by referencing them in the prompt (the paths resolve under its overlay). After it finishes, anything it wrote to /files/artifacts/private/ is reachable in YOUR /files/subagents/<id>/artifacts/private/.

tasks[] is 1..%d objects: description (chip label), prompt (verbatim instruction), optional model (see the schema; omit for the default), optional effort (reasoning depth; omit for the model's default), optional schema (an answer as data; see below), optional kind (web for a browser; see below).

Effort is the model's default when omitted — right for most delegated work, so usually omit it. Lower levels answer faster and cost less; higher levels think longer. The levels each model takes are in the effort field's description.

STRUCTURED ANSWERS: give a task a schema when its answer is data rather than reading — a price, a shortlist, a pass/fail with reasons. You then receive JSON {outcome: found|partial|not_found|alternative|blocked, result, alternatives, blocked_on, files}, checked by core against your schema, and none of the subagent's prose. Keep fields few and short; long text belongs in a file the result names.

WEB TASKS: kind "web" runs the task in a browser instead of a shell: it reads pages as text, clicks, types and downloads; it has no files, shell or messages. It needs a schema, because pages are untrusted: its answer is data, never a page's text. Optional start_url and files. With files it needs a start_url: a person approves uploading them to that site before it starts (denied: blocked, upload_denied); it stays there and puts them only on its pages. Every web task has a budget on what it sends out (2 KB an action, 8 KB a task, 32 KB a day); an action over it is refused. Session open (the default) is signed in nowhere and keeps nothing, so a sign-in wall ends the task blocked. A site session (session: vendor.com) is locked to that site and keeps its sign-ins; when a page there needs a person (sign-in, second factor, CAPTCHA, consent) the task pauses until they have done it in the browser window, and nothing reaches you until its result. Downloads land in /files/subagents/<id>/untrusted-downloads/, listed in the result's files. Web tasks are expensive (every page costs tokens; a browser starts): use one when WebFetch and WebSearch cannot do the job. Signing in is very expensive (a person's time): use a site session only when the task needs what is behind the sign-in, and reuse one that exists (see subagent_web_sessions).

This call returns one line per task with its id. Each task's answer arrives later as its own message — its verbatim final text, or the JSON for a task with a schema — with a pointer to its artifacts dir. Intermediate steps are NOT in your context — see the transcript link in the chip.`, SubagentMaxBatch)
