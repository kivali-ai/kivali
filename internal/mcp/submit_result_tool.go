// Package mcp (submit_result_tool.go) — how a subagent with a schema
// finishes.
//
// A free-text subagent's answer is its final message. A task that
// carries a schema has no such channel: its caller asked for data in a
// shape it wrote, and core checks every submission against that shape
// before anything reaches the caller. This tool is the only way such
// an answer gets out. It is registered only when the task has a
// schema, and it relays: the MCP side formats nothing and validates
// nothing, so there is exactly one description of what a valid result
// is (internal/structured, run by core).

package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/structured"
)

// SubmitResultToolName is the subagent layer's name for the tool, the
// bare name core sees on every event.
const SubmitResultToolName = agentpod.SubmitResultToolName

// SubmitResultBackend is what submit_result relays to. In production
// it posts to core over the agent pod's UDS client; core validates
// against the task's schema and either accepts the result as the
// job's answer or returns the errors as a tool error.
type SubmitResultBackend interface {
	SubmitResult(ctx context.Context, submission json.RawMessage) (body string, isError bool, err error)
}

// SubmitResultToolConfig wires the tool for one task.
type SubmitResultToolConfig struct {
	Backend SubmitResultBackend
	// Schema is the task's parsed schema; the tool's input schema is
	// built from it so the subagent sees the exact shape to fill.
	Schema *structured.Schema
}

// SubmitResultTool returns the `submit_result` tool definition for a
// task with a schema.
func SubmitResultTool(cfg SubmitResultToolConfig) Tool {
	if cfg.Schema == nil {
		panic("mcp.SubmitResultTool: Schema is nil; register the tool only for a task with a schema")
	}
	return Tool{
		Name:        SubmitResultToolName,
		Description: submitResultDescription,
		InputSchema: structured.ToolInputSchema(cfg.Schema),
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			if cfg.Backend == nil {
				return &ToolResult{IsError: true, Content: []string{"submit_result: backend not configured (core unreachable)"}}, nil
			}
			if !json.Valid(raw) {
				return &ToolResult{IsError: true, Content: []string{"submit_result: the input is not valid JSON"}}, nil
			}
			body, isErr, err := cfg.Backend.SubmitResult(ctx, raw)
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{fmt.Sprintf("submit_result: %v", err)}}, nil
			}
			return &ToolResult{IsError: isErr, Content: []string{body}}, nil
		},
	}
}

const submitResultDescription = `Finish this task by handing your caller the answer in the shape they asked for. This is the only way your answer reaches them: your plain text does not.

Fill result from what you verified, in the schema this tool's input shows (leave out a field you could not fill rather than inventing it), and choose the outcome honestly:
  - found: result is what was asked for.
  - partial: result is what was asked for, with fields you could not fill left out.
  - not_found: you looked and there is no such thing.
  - alternative: not the thing asked for, but alternatives (up to 5) that fit the same schema; say how each differs.
  - blocked: you could not proceed; say why in blocked_on.

files lists files this task made, by the absolute path you see them at: under /files/artifacts/private/ or /files/background/. Text longer than a field allows goes in such a file. A web task has no files of its own: its downloads, under /files/untrusted-downloads/, are the only paths it may name, and they are added to its result for it.

An invalid call comes back with what to fix. The third invalid call ends the task with no answer. The first accepted result is final, so call this once, when you are done, then end your turn.`

// NewAgentpodSubmitResultDispatcher returns the UDS-backed
// SubmitResultBackend for one task: it posts to core, which owns the
// job and its schema.
func NewAgentpodSubmitResultDispatcher(c *agentpod.Client, subagentID string) SubmitResultBackend {
	return agentpodSubmitResultDispatcher{c: c, id: subagentID}
}

type agentpodSubmitResultDispatcher struct {
	c  *agentpod.Client
	id string
}

func (d agentpodSubmitResultDispatcher) SubmitResult(ctx context.Context, submission json.RawMessage) (string, bool, error) {
	resp, err := d.c.SubmitSubagentResult(ctx, d.id, submission)
	if err != nil {
		return "", false, err
	}
	return resp.Body, resp.IsError, nil
}
