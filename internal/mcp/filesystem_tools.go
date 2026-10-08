package mcp

import (
	"context"
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/devshell"
	"github.com/kivali-ai/kivali/internal/files"
)

// FilesDispatcher executes one file_* tool call. The body / isError /
// image triple maps onto MCP's ToolResult shape — body becomes a
// text content block, isError flags tool-level failure, and a non-nil
// image becomes a vision content block (file_view of an image).
//
// Two implementations, both in this file:
//
//   - NewSidecarFilesDispatcher — what an agent pod uses. The call
//     executes in the dev-shell container through its daemon, which
//     runs files.Run there against a Backend it builds itself.
//   - NewLocalFilesDispatcher — files.Run in-process against a
//     *files.Backend. Tests, and the no-sidecar fallback.
type FilesDispatcher interface {
	DispatchFilesTool(ctx context.Context, tool string, raw json.RawMessage) (body string, isError bool, image *FilesImage, err error)
}

// FilesImage carries a vision content block returned from a file_view
// of an image attachment / project file. Distinct from
// mcp.ImageContent only because filesystem_tools.go's dispatcher
// abstraction lives in this file and we want the interface to be
// independent of the wider Tool plumbing.
type FilesImage struct {
	Data []byte
	MIME string
}

// FilesystemTools returns MCP tool definitions for Kivali's
// filesystem (file_*) tool family, dispatched through d. The
// operation semantics mirror Anthropic's memory_20250818 family
// (view / create / str_replace / insert / delete / rename), but
// the surface tool names are file_* so agents don't
// conflate this filesystem with the agent_memory_* tools that
// edit the curated agent_memory.md summary.
//
// All tools take a Kivali-style /files/<path> argument and
// delegate to the dispatcher, which enforces per-agent rooting,
// read-only subtrees, and the writable-area quota.
func FilesystemTools(d FilesDispatcher) []Tool {
	return []Tool{
		{
			Name:        files.ToolView,
			Description: "Read a file (or list a directory) under /files/ (e.g. '/files/project/biz-plan.md', '/files/artifacts/private/draft.md', '/files/attachments/screenshot.png'). Text files are prefixed with [file: N lines, B bytes ...]; narrow large reads with offset+limit, grep (Go regex; line-numbered matches with optional grep_context), or view_range — every full read replays on every subsequent API call until chat rotation. Images (PNG/JPEG/GIF/WebP) come back as a vision content block (you read the pixels directly); offset/limit/grep/view_range don't apply. See the handbook's virtual filesystem section for the directory tree.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"path under /files/ to read or list"},"offset":{"type":"integer","minimum":0,"description":"optional 1-indexed line to start reading from (0 = from line 1)"},"limit":{"type":"integer","minimum":0,"description":"optional max lines to return starting at offset (0 = to end)"},"grep":{"type":"string","description":"optional Go regex; only matching lines are returned, prefixed with line numbers"},"grep_context":{"type":"integer","minimum":0,"description":"optional context lines around each grep match (default 0)"},"view_range":{"type":"array","items":{"type":"integer"},"minItems":2,"maxItems":2,"description":"optional [start, end] 1-indexed inclusive line range"}},"required":["path"]}`),
			ReadOnly:    true,
			Handler:     filesHandler(d, files.ToolView),
		},
		{
			Name:        files.ToolCreate,
			Description: "Create or overwrite a file in your workspace: /files/artifacts/private/, /files/background/ (shared with your subagents) or anywhere else under /files/ outside the read-only subtrees. Those (project, past-chats, episodes, skills, attachments, artifacts/public, artifacts/shared) are read-only to this tool and to run_shell alike; artifact_publish is the only way a file gets into artifacts/public, and nothing is visible to other agents until it does. A markdown file with YAML front matter, once published, is a knowledge-graph node (see graph_query); the publish reply says what the index made of it.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"file_text":{"type":"string","description":"full contents of the file"}},"required":["path","file_text"]}`),
			Handler:     filesHandler(d, files.ToolCreate),
		},
		{
			Name:        files.ToolStrReplace,
			Description: "Replace one occurrence of old_str with new_str in a file. Fails if old_str is absent or appears more than once.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_str":{"type":"string"},"new_str":{"type":"string"}},"required":["path","old_str","new_str"]}`),
			Handler:     filesHandler(d, files.ToolStrReplace),
		},
		{
			Name:        files.ToolInsert,
			Description: "Insert text after a 1-indexed line (0 to prepend).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"insert_line":{"type":"integer"},"insert_text":{"type":"string"}},"required":["path","insert_line","insert_text"]}`),
			Handler:     filesHandler(d, files.ToolInsert),
		},
		{
			Name:        files.ToolDelete,
			Description: "Delete a file in your workspace. A directory cannot be removed with this tool (run_shell can). A published file is removed with artifact_unpublish.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			Handler:     filesHandler(d, files.ToolDelete),
		},
		{
			Name:        files.ToolRename,
			Description: "Rename/move a file. Source and destination must both sit in your workspace (outside the read-only subtrees). A directory moved into or out of /files/artifacts/ needs run_shell's mv. A subagent cannot move a file between its artifacts/private/ and background/; file_copy it, or use mv; to move a published file, publish it at the new path and unpublish the old.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"old_path":{"type":"string"},"new_path":{"type":"string"}},"required":["old_path","new_path"]}`),
			Handler:     filesHandler(d, files.ToolRename),
		},
		{
			Name:        files.ToolCopy,
			Description: "Copy a file. Source can be any readable path (including read-only subtrees like project/, attachments/, skills/, artifacts/shared/); destination must sit in your workspace (e.g. artifacts/private/ or background/). Existing destinations are overwritten.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"src_path":{"type":"string"},"dest_path":{"type":"string"}},"required":["src_path","dest_path"]}`),
			Handler:     filesHandler(d, files.ToolCopy),
		},
	}
}

// filesHandler wraps FilesDispatcher.DispatchFilesTool into the MCP
// handler shape. Image returns become a vision content block; text
// returns become a single text content block. Tool-level errors ride
// on isError; transport errors surface as the handler's error return.
func filesHandler(d FilesDispatcher, name string) func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
	return func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
		body, isError, img, err := d.DispatchFilesTool(ctx, name, raw)
		if err != nil {
			return nil, err
		}
		out := &ToolResult{Content: []string{body}, IsError: isError}
		if img != nil {
			out.Images = []ImageContent{{Data: img.Data, MIMEType: img.MIME}}
		}
		return out, nil
	}
}

// NewLocalFilesDispatcher returns the in-process dispatcher backed by
// a files.Backend: the file_* tools run in the calling process. An
// agent pod never does this (its agent container mounts no /files);
// tests and the no-sidecar fallback do. A nil Backend gives a nil
// dispatcher, which the toolkits refuse.
func NewLocalFilesDispatcher(b *files.Backend) FilesDispatcher {
	if b == nil {
		return nil
	}
	return localFilesDispatcher{b: b}
}

type localFilesDispatcher struct {
	b *files.Backend
}

func (d localFilesDispatcher) DispatchFilesTool(_ context.Context, name string, raw json.RawMessage) (string, bool, *FilesImage, error) {
	body, isError, img, err := files.Run(d.b, name, raw)
	if err != nil {
		return "", false, nil, err
	}
	if img != nil {
		return body, isError, &FilesImage{Data: img.Data, MIME: img.MIME}, nil
	}
	return body, isError, nil, nil
}

// NewSidecarFilesDispatcher returns the dispatcher an agent pod's MCP
// server uses: every file_* call executes in the dev-shell container
// through its daemon, with the shell's mounts and privileges. The
// agent container itself mounts no /files. A nil client gives a nil
// dispatcher, which the toolkits refuse.
func NewSidecarFilesDispatcher(c *devshell.SidecarFiles) FilesDispatcher {
	if c == nil {
		return nil
	}
	return sidecarFilesDispatcher{c: c}
}

type sidecarFilesDispatcher struct {
	c *devshell.SidecarFiles
}

func (d sidecarFilesDispatcher) DispatchFilesTool(ctx context.Context, name string, raw json.RawMessage) (string, bool, *FilesImage, error) {
	res, err := d.c.Dispatch(ctx, name, raw)
	if err != nil {
		return "", false, nil, err
	}
	if res.Image != nil {
		return res.Body, res.IsError, &FilesImage{Data: res.Image.Data, MIME: res.Image.MIME}, nil
	}
	return res.Body, res.IsError, nil, nil
}
