package mcp

import (
	"context"
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/agent"
)

// ShareFileEntry is one file in a share_file batch: required Path,
// optional Name override (defaults to the path basename). Mirrors
// the input-schema items shape; carried verbatim through the
// dispatcher to the server-side resolver.
type ShareFileEntry struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

// ShareFileDispatcher executes one share_file invocation, returning
// the rendered tool_result body + tool-level error flag.
//
// The production implementation wraps an agentpod.Client: the agent
// pod's MCP subprocess forwards the structured request over UDS to
// core, where ResolveShareFilePath runs against each entry and a
// single chat row gets appended carrying every resolved attachment.
type ShareFileDispatcher interface {
	DispatchShareFile(ctx context.Context, files []ShareFileEntry, caption string) (body string, isError bool, err error)
}

// ShareFileTool returns the MCP share_file tool definition, or nil
// when the dispatcher is missing.
func ShareFileTool(d ShareFileDispatcher) *Tool {
	if d == nil {
		return nil
	}
	def := agent.ShareFileTool()
	t := Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: def.InputSchema,
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			var in struct {
				Files   []ShareFileEntry `json:"files"`
				Caption string           `json:"caption"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return &ToolResult{IsError: true, Content: []string{"invalid share_file input: " + err.Error()}}, nil
			}
			if len(in.Files) == 0 {
				return &ToolResult{IsError: true, Content: []string{"share_file: files[] is required and must contain at least one entry"}}, nil
			}
			body, isErr, err := d.DispatchShareFile(ctx, in.Files, in.Caption)
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"share_file: " + err.Error()}}, nil
			}
			return &ToolResult{IsError: isErr, Content: []string{body}}, nil
		},
	}
	return &t
}
