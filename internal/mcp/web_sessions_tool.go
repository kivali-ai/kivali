package mcp

import (
	"context"
	"encoding/json"
	"strings"
)

// The calling agent's tools over its web sessions. Like subagent_status
// and subagent_cancel they are proxies: core owns the sessions and
// renders the text, so a session is described in one place.
const (
	WebSessionsToolName     = "subagent_web_sessions"
	WebSessionResetToolName = "subagent_web_session_reset"
)

// WebSessionsTool returns the MCP tool definition for
// `subagent_web_sessions`.
func WebSessionsTool(cfg SubagentToolConfig) Tool {
	return Tool{
		Name: WebSessionsToolName,
		Description: `List your web sessions: the sites your web tasks can be signed in to, when each was last used, and whether its browser is running now. Never what is in them.

A web task names a session: open (signed in nowhere, nothing kept between tasks) or a site such as vendor.com, which keeps its sign-ins between tasks and is locked to that site. A site session exists once a web task has named it. No input parameters.`,
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		ReadOnly:    true,
		Handler: func(ctx context.Context, _ json.RawMessage) (*ToolResult, error) {
			if cfg.Backend == nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_web_sessions: backend not configured (web control socket unreachable)"}}, nil
			}
			out, err := cfg.Backend.WebSessions(ctx, cfg.Parent)
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_web_sessions: " + err.Error()}}, nil
			}
			return &ToolResult{Content: []string{out}}, nil
		},
	}
}

// WebSessionResetTool returns the MCP tool definition for
// `subagent_web_session_reset`.
func WebSessionResetTool(cfg SubagentToolConfig) Tool {
	return Tool{
		Name: WebSessionResetToolName,
		Description: `Forget one of your web sessions: its browser stops, any web task running in it is cancelled, and its saved sign-ins are deleted. The next web task that names the session starts it afresh, signed in nowhere.

Use it to sign a site out, or to start over when a session has gone wrong. Signing in again costs a person's time, so do not reset a session that still works.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "session": {"type": "string", "description": "the session's name, as subagent_web_sessions lists it"}
  },
  "required": ["session"]
}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			if cfg.Backend == nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_web_session_reset: backend not configured (web control socket unreachable)"}}, nil
			}
			var in struct {
				Session string `json:"session"`
			}
			if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Session) == "" {
				return &ToolResult{IsError: true, Content: []string{"subagent_web_session_reset: session is required"}}, nil
			}
			out, err := cfg.Backend.WebSessionReset(ctx, cfg.Parent, strings.TrimSpace(in.Session))
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"subagent_web_session_reset: " + err.Error()}}, nil
			}
			return &ToolResult{Content: []string{out}}, nil
		},
	}
}
