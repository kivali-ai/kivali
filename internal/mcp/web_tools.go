// Package mcp (web_tools.go) — the browser toolkit a web subagent runs
// with.
//
// A web subagent has these tools and submit_result, and nothing else.
// Every handler is a relay: it posts the call to core, which checks
// the task's session and state and forwards the command to the task's
// browser, and hands back what core answered. The MCP side formats
// nothing and decides nothing, so a rule about what a web subagent may
// do lives in exactly one place.

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/structured"
)

// WebBackend is what the web tools relay to. In production it posts
// to core over the agent pod's UDS client.
type WebBackend interface {
	WebCommand(ctx context.Context, req agentpod.WebCommandRequest) (*agentpod.WebCommandResponse, error)
}

// WebSubagentDeps wires the web toolkit's MCP server for one task.
type WebSubagentDeps struct {
	Client     *agentpod.Client
	SubagentID string
	// Session is the web session the task runs in; web_eval exists
	// only in the open session.
	Session string
	// ResultSchema is the task's schema. A web task always has one.
	ResultSchema *structured.Schema

	// Web and SubmitResult are where the tools relay to. Nil means
	// core, through Client; tests inject recorders.
	Web          WebBackend
	SubmitResult SubmitResultBackend
}

// WebSubagentToolkit returns the tool bundle a web subagent gets: the
// web_* tools for its session and submit_result. The set matches
// agentpod.WebSubagentTools, which is what the run's --allowedTools
// names.
func WebSubagentToolkit(deps WebSubagentDeps) Toolkit {
	if deps.ResultSchema == nil {
		panic("mcp.WebSubagentToolkit: ResultSchema is nil; a web task always has a schema")
	}
	web := deps.Web
	if web == nil {
		web = NewAgentpodWebDispatcher(deps.Client, deps.SubagentID)
	}
	submit := deps.SubmitResult
	if submit == nil {
		submit = NewAgentpodSubmitResultDispatcher(deps.Client, deps.SubagentID)
	}
	tools := WebTools(web, deps.Session)
	tools = append(tools, SubmitResultTool(SubmitResultToolConfig{Backend: submit, Schema: deps.ResultSchema}))
	return Toolkit{Name: "web", Tools: tools}
}

// NewAgentpodWebDispatcher returns the UDS-backed WebBackend for one
// task.
func NewAgentpodWebDispatcher(c *agentpod.Client, subagentID string) WebBackend {
	return agentpodWebDispatcher{c: c, id: subagentID}
}

type agentpodWebDispatcher struct {
	c  *agentpod.Client
	id string
}

func (d agentpodWebDispatcher) WebCommand(ctx context.Context, req agentpod.WebCommandRequest) (*agentpod.WebCommandResponse, error) {
	return d.c.WebCommand(ctx, d.id, req)
}

// WebTools returns the web_* tools for a session, each relaying to
// backend.
func WebTools(backend WebBackend, session string) []Tool {
	tools := []Tool{
		webTool(backend, agentpod.WebNavigateTool, webNavigateDescription, `{
  "type": "object",
  "properties": {"url": {"type": "string", "description": "an http or https URL"}},
  "required": ["url"]
}`, false),
		webTool(backend, agentpod.WebSnapshotTool, webSnapshotDescription, `{"type": "object", "properties": {}}`, true),
		webTool(backend, agentpod.WebScreenshotTool, webScreenshotDescription, `{"type": "object", "properties": {}}`, true),
		webTool(backend, agentpod.WebClickTool, `Click the element with this ref from your last snapshot. Waits for a navigation it starts. Returns what was clicked and where the tab is now.`, `{
  "type": "object",
  "properties": {"ref": {"type": "string", "description": "a ref from the last web_snapshot, such as e12"}},
  "required": ["ref"]
}`, false),
		webTool(backend, agentpod.WebTypeTool, `Type text into the element with this ref: the element is focused, its existing text replaced, and the text entered. submit presses Enter after it and waits for a navigation it starts. Type only text your task gave you.`, `{
  "type": "object",
  "properties": {
    "ref": {"type": "string", "description": "a ref from the last web_snapshot"},
    "text": {"type": "string", "description": "the text to enter"},
    "submit": {"type": "boolean", "description": "press Enter after typing"}
  },
  "required": ["ref", "text"]
}`, false),
		webTool(backend, agentpod.WebSelectTool, `Choose an option in a select element by its value or its visible label.`, `{
  "type": "object",
  "properties": {
    "ref": {"type": "string", "description": "the select's ref from the last web_snapshot"},
    "value": {"type": "string", "description": "the option's value or label"}
  },
  "required": ["ref", "value"]
}`, false),
		webTool(backend, agentpod.WebPressTool, `Press a key in the current tab: Enter, Tab, Escape, Backspace, Delete, ArrowUp, ArrowDown, ArrowLeft, ArrowRight, Home, End, PageUp, PageDown, Space, a single character, or a chord such as Control+a. Waits for a navigation it starts.`, `{
  "type": "object",
  "properties": {"key": {"type": "string", "description": "the key or chord"}},
  "required": ["key"]
}`, false),
		webTool(backend, agentpod.WebScrollTool, `Scroll the page, or the element with ref, by viewport heights in a direction. Take a new snapshot afterwards to read what came into view.`, `{
  "type": "object",
  "properties": {
    "ref": {"type": "string", "description": "a scrollable element's ref; omit for the page"},
    "direction": {"type": "string", "enum": ["up", "down", "left", "right"]},
    "amount": {"type": "number", "description": "viewport heights or widths to scroll; 1 when omitted"}
  },
  "required": ["direction"]
}`, false),
		webTool(backend, agentpod.WebBackTool, `Go back one page in the current tab's history.`, `{"type": "object", "properties": {}}`, false),
		webTool(backend, agentpod.WebTabsTool, `Your tabs: list them, switch to one, open a new one (at a URL if given) or close one. You see only your own tabs; other tasks sharing the browser have theirs.`, `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list", "switch", "open", "close"]},
    "tab": {"type": "string", "description": "the tab id (t1, t2, ...) for switch and close; close without it closes the current tab"},
    "url": {"type": "string", "description": "for open: the URL to load in the new tab"}
  },
  "required": ["action"]
}`, false),
		webTool(backend, agentpod.WebWaitTool, `Wait for the page to show text, for a ref to exist, or for the next navigation to finish; up to timeout_ms (30000 at most, the default). Cheaper than snapshotting repeatedly. Returns what the page shows when the wait ends or runs out.`, `{
  "type": "object",
  "properties": {
    "for": {"type": "string", "enum": ["text", "ref", "navigation"]},
    "value": {"type": "string", "description": "the text to wait for, or the ref"},
    "timeout_ms": {"type": "integer", "minimum": 100, "maximum": 30000}
  },
  "required": ["for"]
}`, true),
	}
	if session == agentpod.OpenWebSession {
		tools = append(tools, webTool(backend, agentpod.WebEvalTool, `Run JavaScript in the current page, in an isolated world, and return its JSON value (cut at 8000 characters). For the rare value a snapshot cannot give you. Available in the open session only.`, `{
  "type": "object",
  "properties": {"script": {"type": "string", "description": "an expression or statements; the value of the last expression is returned, promises are awaited"}},
  "required": ["script"]
}`, false))
	}
	tools = append(tools, webTool(backend, agentpod.WebUploadTool, `Attach one of your task's files to a file input. file is one of the names your task was dispatched with; you cannot upload anything else.`, `{
  "type": "object",
  "properties": {
    "ref": {"type": "string", "description": "the file input's ref from the last web_snapshot"},
    "file": {"type": "string", "description": "the file's name, as your task lists it"}
  },
  "required": ["ref", "file"]
}`, false))
	return tools
}

const webNavigateDescription = `Load a URL in your current tab and wait for the page to settle. http and https only. Returns the page's title and its final URL; take a web_snapshot to read the page.`

const webSnapshotDescription = `The current page as text: what a person would see, one element per line, with a ref like [e12] on every element you can act on (links, buttons, fields, options). Hidden text is left out. This is the default way to read a page. Refs belong to the snapshot they came from: take a new one after anything changes the page. A long page is cut at a limit and says so; scroll to read more.`

const webScreenshotDescription = `The viewport as an image. For layout, charts, images and anything a snapshot cannot show; it costs more than a snapshot and has no refs.`

// webTool builds one relaying tool. The command core receives is the
// tool's name without its prefix.
func webTool(backend WebBackend, name, description, schema string, readOnly bool) Tool {
	command := strings.TrimPrefix(name, agentpod.WebToolPrefix)
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: json.RawMessage(schema),
		ReadOnly:    readOnly,
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			if backend == nil {
				return &ToolResult{IsError: true, Content: []string{name + ": backend not configured (core unreachable)"}}, nil
			}
			if len(raw) == 0 {
				raw = json.RawMessage(`{}`)
			}
			if !json.Valid(raw) {
				return &ToolResult{IsError: true, Content: []string{name + ": the input is not valid JSON"}}, nil
			}
			resp, err := backend.WebCommand(ctx, agentpod.WebCommandRequest{Command: command, Args: raw})
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{fmt.Sprintf("%s: %v", name, err)}}, nil
			}
			out := &ToolResult{IsError: resp.IsError, Content: []string{resp.Text}}
			if resp.Image != nil && len(resp.Image.Data) > 0 {
				out.Images = []ImageContent{{Data: resp.Image.Data, MIMEType: resp.Image.MIME}}
			}
			return out, nil
		},
	}
}

// WebCommandName is the browser command a web tool's name stands for,
// and whether the name is a web tool at all.
func WebCommandName(tool string) (string, bool) {
	if !strings.HasPrefix(tool, agentpod.WebToolPrefix) {
		return "", false
	}
	command := strings.TrimPrefix(tool, agentpod.WebToolPrefix)
	return command, browserpod.KnownCommand(command)
}
