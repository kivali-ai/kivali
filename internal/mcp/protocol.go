// Package mcp is a small stdio-transport Model Context Protocol
// server. It exposes Kivali operations (memory, publish_*, shell)
// to an MCP client — primarily Claude Code / the Claude Agent SDK,
// but usable by any MCP-capable caller.
//
// The protocol wire format is JSON-RPC 2.0 with MCP-specific methods
// (initialize, tools/list, tools/call). We deliberately don't pull
// in an external MCP library: the surface we implement is small and
// keeping it in-tree avoids pinning a fast-moving spec.
package mcp

import (
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/provider"
)

// protocolVersion is the MCP spec version this server advertises.
// Bumped as the spec evolves; clients negotiate and usually accept
// forward-compat.
const protocolVersion = "2025-06-18"

// serverName + serverVersion identify this MCP server to clients. The
// name is the one a run's MCPServers registers it under
// (provider.KivaliMCPServer); how a model provider spells this server's
// tools on its own wire is the driver's business, never this package's.
const (
	defaultServerName    = provider.KivaliMCPServer
	defaultServerVersion = "0.1"
)

// JSON-RPC 2.0 envelope types.

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC 2.0 standard error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// MCP initialize / capability types.

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      implementation     `json:"serverInfo"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type serverCapabilities struct {
	// Tools capability signals that the server exposes tools/list +
	// tools/call. ListChanged isn't supported yet (tool list is
	// fixed at server start).
	Tools *toolsCapability `json:"tools,omitempty"`
	// Resources capability signals that the server exposes
	// resources/list + resources/read. Resources are binary- or
	// text-content items the model can fetch by URI (Kivali serves
	// project files this way so PDFs and images can reach Claude
	// without going through Anthropic's Files API).
	Resources *resourcesCapability `json:"resources,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type resourcesCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
	Subscribe   bool `json:"subscribe,omitempty"`
}

// MCP tools/list and tools/call types.

type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

type toolDescriptor struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`
}

// toolAnnotations carries the MCP tool-annotation hints the host can
// use to optimize execution. Today the only one we set is
// readOnlyHint — Claude Code's SDK will dispatch read-only tool calls
// concurrently within a single assistant turn instead of serially
// (the default for any tool that isn't annotated as read-only).
//
// MCP defines several other hints (destructiveHint, idempotentHint,
// openWorldHint, title) — add fields here as they become useful.
// Pointer-bool semantics matter: omitempty drops the field entirely
// when unset, so a tool that doesn't opt in stays shape-compatible
// with hosts that don't understand the hint.
type toolAnnotations struct {
	ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolsCallResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// contentItem is one piece of a tool result. MCP supports text,
// image, resource, etc. Today Kivali tools produce text and image:
// file_view returns an image content block when the requested path
// is an image (PNG/JPEG/GIF/WebP), so the agent reads the pixels via
// Claude's native vision rather than getting an apologetic "binary
// file" stub. Data is base64 of the raw image bytes; MIMEType is the
// image MIME (image/png, image/jpeg, image/gif, image/webp).
type contentItem struct {
	Type     string `json:"type"` // "text" | "image"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`     // base64 for type=="image"
	MIMEType string `json:"mimeType,omitempty"` // for type=="image"
}

// MCP resources/list and resources/read types.

type resourcesListResult struct {
	Resources []resourceDescriptor `json:"resources"`
}

type resourceDescriptor struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

type resourcesReadParams struct {
	URI string `json:"uri"`
}

type resourcesReadResult struct {
	Contents []resourceContents `json:"contents"`
}

// resourceContents carries a single resource's bytes. Text is the
// raw body for text MIME types. Blob is base64-encoded bytes for
// binaries. Exactly one is set per item (MCP spec).
type resourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}
