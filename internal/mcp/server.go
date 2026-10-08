package mcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
)

// Tool registers one callable operation on the MCP server. Name and
// InputSchema are what the client sees via tools/list; Handler is
// invoked on tools/call with the raw arguments JSON. Handlers are
// expected to return user-meaningful output as a ToolResult — they
// should NOT return a non-nil error for tool-level failures (use
// ToolResult.IsError + Content instead); returning a Go error is
// reserved for transport/internal failures that should surface as
// a JSON-RPC error.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// ReadOnly marks tools that do not mutate state on the server or
	// in the outside world. Surfaced to the MCP client as the
	// readOnlyHint annotation; Claude Code's SDK uses it to dispatch
	// such tool calls concurrently within a single assistant turn
	// (vs. the default serial dispatch for unannotated tools). Set
	// true for pure lookups, search, view-only ops; leave false for
	// anything that writes a file, sends a message, spawns a
	// process, or otherwise has side effects.
	ReadOnly bool
	Handler  func(ctx context.Context, arguments json.RawMessage) (*ToolResult, error)
}

// ToolResult is the output of a tool call. Content is one or more
// text blocks; Images, when set, appends image content blocks after
// the text (Claude's vision model reads the pixels). IsError=true
// marks results the model should treat as an error without aborting
// the conversation.
type ToolResult struct {
	Content []string       // text blocks, in order
	Images  []ImageContent // optional image blocks, appended after text
	IsError bool
}

// ImageContent is a single image block carried out of a tool call.
// MIMEType must be one of image/png, image/jpeg, image/gif,
// image/webp (Anthropic's accepted vision set). The server
// base64-encodes Data on the wire; callers pass raw bytes.
type ImageContent struct {
	Data     []byte
	MIMEType string
}

// Server is a stdio MCP server. Attach Tools and/or Resources before
// calling Serve. Logger writes to stderr (never stdout — stdout is
// reserved for the JSON-RPC wire).
type Server struct {
	Tools      []Tool
	Resources  ResourceProvider // optional; nil → resources capability not advertised
	ServerName string
	Version    string
	Logger     *log.Logger

	initMu      sync.Mutex
	initialized bool
}

// ResourceProvider serves binary- or text-content resources to an
// MCP client. List enumerates available resources; Read returns a
// specific resource's bytes. Implementations typically wrap a
// filesystem view (e.g. project_files/).
type ResourceProvider interface {
	List(ctx context.Context) ([]ResourceDescriptor, error)
	Read(ctx context.Context, uri string) (*ResourceContents, error)
}

// ResourceDescriptor is the entry tools/list returns for each
// available resource.
type ResourceDescriptor struct {
	URI         string
	Name        string
	Description string
	MIMEType    string
	Size        int64
}

// ResourceContents is the payload returned from a Read. Exactly one
// of Text or Blob should be set.
type ResourceContents struct {
	URI      string
	MIMEType string
	Text     string // text content for text/* MIME types
	Blob     []byte // raw bytes for binary MIME types; encoded base64 on the wire
}

// Serve runs the JSON-RPC loop on the given reader/writer until the
// reader hits EOF or ctx cancels. Returns nil on clean shutdown
// (EOF), otherwise the first fatal I/O error.
//
// Concurrency model: each parsed request is dispatched in its own
// goroutine so slow tool handlers (e.g. shell, sleep) don't block
// subsequent requests. writeMu serializes the JSON-RPC response
// writes so per-line bytes don't interleave on the wire.
//
// Notifications (no id) are handled inline — they don't produce a
// response and existing notifications (notifications/initialized)
// only flip a small mutex-guarded flag. Inline handling preserves the
// "initialized arrives before tools/call" invariant a well-behaved
// client expects: the SDK always emits notifications/initialized
// before issuing any tools/call, and we don't want a goroutine race
// to let a tools/call observe initialized=false. (The server doesn't
// reject tools/call on initialized=false today, but keeping the
// notification path strictly ordered preserves that option.)
//
// Serve waits for all in-flight dispatch goroutines to finish before
// returning so the caller can rely on "Serve returned" meaning "no
// more handler activity" — important for tests that close pipes on
// return and for production shutdown ordering.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if s.Logger == nil {
		s.Logger = log.New(io.Discard, "", 0)
	}
	if s.ServerName == "" {
		s.ServerName = defaultServerName
	}
	if s.Version == "" {
		s.Version = defaultServerVersion
	}
	// Serialize writes so we don't interleave response bytes when
	// tools handle concurrently: dispatch runs in goroutines.
	var writeMu sync.Mutex
	write := func(resp *response) {
		writeMu.Lock()
		defer writeMu.Unlock()
		buf, err := json.Marshal(resp)
		if err != nil {
			s.Logger.Printf("marshal response: %v", err)
			return
		}
		buf = append(buf, '\n')
		if _, err := out.Write(buf); err != nil {
			s.Logger.Printf("write response: %v", err)
		}
	}

	// inflight tracks dispatch goroutines so Serve can wait for them
	// at exit. The reader loop is single-threaded; only dispatchers
	// run concurrently.
	var inflight sync.WaitGroup
	defer inflight.Wait()

	scanner := bufio.NewScanner(in)
	// MCP messages can be large (tool inputs carrying file bodies).
	// Default scanner buffer is 64 KB; raise to match the file_*
	// tools' realistic ceiling.
	scanner.Buffer(make([]byte, 0, 128*1024), 16*1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// json.Unmarshal copies into req — RawMessage fields
		// (req.ID, req.Params) own fresh backing arrays after
		// Unmarshal returns, so they don't alias the scanner's
		// internal buffer that gets reused on the next Scan().
		// Safe to hand req off to a goroutine that outlives this
		// iteration.
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			// Malformed line: emit parse-error with null id.
			write(&response{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: codeParseError, Message: err.Error()},
			})
			continue
		}
		if req.JSONRPC != "2.0" {
			write(&response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: codeInvalidRequest, Message: "jsonrpc must be '2.0'"},
			})
			continue
		}
		// Notifications have no id — we don't respond. Handle
		// inline so notifications/initialized's flag flip is
		// strictly ordered relative to subsequent requests on the
		// wire. This is cheap (no I/O, no tool work) and avoids
		// a class of test/prod races where a tools/call dispatched
		// concurrently with the initialized notification might run
		// before the flag flip.
		isNotification := len(req.ID) == 0
		if isNotification {
			s.dispatch(ctx, &req)
			continue
		}
		// Real request: dispatch in a goroutine so slow handlers
		// (sleep, shell) don't block the reader. writeMu serializes
		// the response write.
		//
		// Capture by value: a new req is allocated each loop
		// iteration (so address-of is per-iteration), but copying
		// keeps the goroutine self-contained and removes any need
		// for the reader to think about lifetime. RawMessage fields
		// already own their bytes (see Unmarshal copy note above).
		reqCopy := req
		inflight.Add(1)
		go func() {
			defer inflight.Done()
			resp := s.dispatch(ctx, &reqCopy)
			resp.JSONRPC = "2.0"
			resp.ID = reqCopy.ID
			write(resp)
		}()
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// dispatch routes a parsed JSON-RPC request to the right handler and
// produces a response envelope (no JSONRPC/ID fields — Serve fills
// those in).
func (s *Server) dispatch(ctx context.Context, req *request) *response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "notifications/initialized":
		s.initMu.Lock()
		s.initialized = true
		s.initMu.Unlock()
		return &response{} // no reply for notification, discarded
	case "ping":
		return &response{Result: struct{}{}}
	case "tools/list":
		return s.handleToolsList()
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	case "resources/list":
		return s.handleResourcesList(ctx)
	case "resources/read":
		return s.handleResourcesRead(ctx, req)
	case "shutdown":
		return &response{Result: struct{}{}}
	default:
		return &response{
			Error: &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method},
		}
	}
}

func (s *Server) handleInitialize(req *request) *response {
	// We accept any protocol version the client sends — MCP
	// clients typically negotiate down if we advertise something
	// they don't understand. Echo our supported version.
	caps := serverCapabilities{
		Tools: &toolsCapability{},
	}
	if s.Resources != nil {
		caps.Resources = &resourcesCapability{}
	}
	return &response{
		Result: initializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    caps,
			ServerInfo: implementation{
				Name:    s.ServerName,
				Version: s.Version,
			},
		},
	}
}

func (s *Server) handleToolsList() *response {
	descs := make([]toolDescriptor, 0, len(s.Tools))
	for _, t := range s.Tools {
		d := toolDescriptor{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
		if t.ReadOnly {
			d.Annotations = &toolAnnotations{ReadOnlyHint: true}
		}
		descs = append(descs, d)
	}
	return &response{Result: toolsListResult{Tools: descs}}
}

func (s *Server) handleToolsCall(ctx context.Context, req *request) *response {
	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &response{Error: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
	}
	var tool *Tool
	for i := range s.Tools {
		if s.Tools[i].Name == params.Name {
			tool = &s.Tools[i]
			break
		}
	}
	if tool == nil {
		// Tool-unknown surfaces as a tool-level error (isError=true),
		// not a JSON-RPC error, so the model sees it as a call that
		// failed with a message rather than a transport failure.
		return &response{
			Result: toolsCallResult{
				IsError: true,
				Content: []contentItem{{Type: "text", Text: fmt.Sprintf("unknown tool: %q", params.Name)}},
			},
		}
	}
	args := params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result, err := tool.Handler(ctx, args)
	if err != nil {
		return &response{Error: &rpcError{Code: codeInternalError, Message: err.Error()}}
	}
	if result == nil {
		result = &ToolResult{Content: []string{""}}
	}
	items := make([]contentItem, 0, len(result.Content)+len(result.Images))
	for _, txt := range result.Content {
		items = append(items, contentItem{Type: "text", Text: txt})
	}
	for _, img := range result.Images {
		items = append(items, contentItem{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(img.Data),
			MIMEType: img.MIMEType,
		})
	}
	return &response{Result: toolsCallResult{Content: items, IsError: result.IsError}}
}

func (s *Server) handleResourcesList(ctx context.Context) *response {
	if s.Resources == nil {
		return &response{Result: resourcesListResult{Resources: []resourceDescriptor{}}}
	}
	descs, err := s.Resources.List(ctx)
	if err != nil {
		return &response{Error: &rpcError{Code: codeInternalError, Message: err.Error()}}
	}
	out := make([]resourceDescriptor, 0, len(descs))
	for _, d := range descs {
		out = append(out, resourceDescriptor(d))
	}
	return &response{Result: resourcesListResult{Resources: out}}
}

func (s *Server) handleResourcesRead(ctx context.Context, req *request) *response {
	if s.Resources == nil {
		return &response{Error: &rpcError{Code: codeMethodNotFound, Message: "resources capability not enabled"}}
	}
	var params resourcesReadParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &response{Error: &rpcError{Code: codeInvalidParams, Message: err.Error()}}
	}
	if params.URI == "" {
		return &response{Error: &rpcError{Code: codeInvalidParams, Message: "uri is required"}}
	}
	rc, err := s.Resources.Read(ctx, params.URI)
	if err != nil {
		return &response{Error: &rpcError{Code: codeInternalError, Message: err.Error()}}
	}
	if rc == nil {
		return &response{Result: resourcesReadResult{Contents: []resourceContents{}}}
	}
	item := resourceContents{URI: rc.URI, MIMEType: rc.MIMEType}
	if rc.URI == "" {
		item.URI = params.URI
	}
	if len(rc.Blob) > 0 {
		item.Blob = base64.StdEncoding.EncodeToString(rc.Blob)
	} else {
		item.Text = rc.Text
	}
	return &response{Result: resourcesReadResult{Contents: []resourceContents{item}}}
}
