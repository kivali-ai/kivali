package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/files"
)

// minimalPNG is a 67-byte 1×1 transparent PNG. Used as a tiny image
// fixture so file_view tests don't need to vendor binary assets.
var minimalPNG = func() []byte {
	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNgYAAAAAMAASsJTYQAAAAASUVORK5CYII="
	out, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic(err)
	}
	return out
}()

// endToEndClient simulates a minimal MCP client that speaks JSON-RPC
// to the server over pipes. Used by tests to drive the protocol
// without pulling in a real MCP library.
type endToEndClient struct {
	toServer   *io.PipeWriter // bytes we send to server's stdin
	fromServer *bufferedReader
	nextID     int
	t          *testing.T
}

// bufferedReader reads line-delimited JSON-RPC responses.
type bufferedReader struct {
	r   *io.PipeReader
	buf []byte
}

func (b *bufferedReader) readMessage() ([]byte, error) {
	for {
		for i, c := range b.buf {
			if c == '\n' {
				line := b.buf[:i]
				b.buf = b.buf[i+1:]
				return line, nil
			}
		}
		chunk := make([]byte, 4096)
		n, err := b.r.Read(chunk)
		if n > 0 {
			b.buf = append(b.buf, chunk[:n]...)
		}
		if err != nil {
			return nil, err
		}
	}
}

func (c *endToEndClient) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	c.nextID++
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID,
		"method":  method,
		"params":  json.RawMessage(paramsBytes),
	}
	buf, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	buf = append(buf, '\n')
	if _, err := c.toServer.Write(buf); err != nil {
		t.Fatalf("write to server: %v", err)
	}
	line, err := c.fromServer.readMessage()
	if err != nil {
		t.Fatalf("read from server: %v", err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal response: %v (line=%q)", err, line)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error on %s: %+v", method, resp.Error)
	}
	return resp.Result
}

func startServer(t *testing.T, tools []Tool) *endToEndClient {
	t.Helper()
	// Client writes into clientToServerW; server reads from
	// clientToServerR. Server writes into serverToClientW; client
	// reads from serverToClientR.
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	srv := &Server{
		Tools:      tools,
		ServerName: "test",
		Version:    "0.0",
		Logger:     log.New(io.Discard, "", 0),
	}
	go func() {
		_ = srv.Serve(context.Background(), clientToServerR, serverToClientW)
		_ = serverToClientW.Close()
	}()
	t.Cleanup(func() {
		_ = clientToServerW.Close()
		_ = serverToClientR.Close()
	})
	return &endToEndClient{
		toServer:   clientToServerW,
		fromServer: &bufferedReader{r: serverToClientR},
		t:          t,
	}
}

func TestInitializeAndToolsList(t *testing.T) {
	mem := &files.Backend{Root: t.TempDir()}
	c := startServer(t, FilesystemTools(NewLocalFilesDispatcher(mem)))

	initResult := c.call(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"clientInfo":      map[string]string{"name": "test-client", "version": "0"},
	})
	var init initializeResult
	if err := json.Unmarshal(initResult, &init); err != nil {
		t.Fatalf("unmarshal init: %v", err)
	}
	if init.ServerInfo.Name != "test" {
		t.Errorf("server name = %q", init.ServerInfo.Name)
	}
	if init.Capabilities.Tools == nil {
		t.Error("tools capability missing")
	}

	listResult := c.call(t, "tools/list", map[string]any{})
	var list toolsListResult
	if err := json.Unmarshal(listResult, &list); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	names := map[string]bool{}
	byName := map[string]toolDescriptor{}
	for _, td := range list.Tools {
		names[td.Name] = true
		byName[td.Name] = td
	}
	for _, want := range []string{"file_view", "file_create", "file_str_replace", "file_insert", "file_delete", "file_rename", "file_copy"} {
		if !names[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
	// readOnlyHint — Claude Code's SDK uses it to dispatch matching
	// tool calls concurrently within a single assistant turn instead
	// of serially. file_view must carry it; mutating tools must not.
	if a := byName["file_view"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Errorf("file_view annotations = %+v, want readOnlyHint=true", a)
	}
	for _, mut := range []string{"file_create", "file_str_replace", "file_insert", "file_delete", "file_rename", "file_copy"} {
		if a := byName[mut].Annotations; a != nil && a.ReadOnlyHint {
			t.Errorf("%s annotations carry readOnlyHint=true — would let SDK fan out a mutating tool", mut)
		}
	}
}

// TestToolsListAnnotationJSONShape pins the on-the-wire JSON shape
// of the readOnlyHint annotation. Claude Code's SDK looks for the
// exact key `annotations.readOnlyHint`; if either name drifts the
// concurrent-dispatch optimization silently turns off and 3-tool
// fanouts go back to ~3× wall time.
func TestToolsListAnnotationJSONShape(t *testing.T) {
	tools := []Tool{
		{Name: "ro_tool", Description: "a", InputSchema: json.RawMessage(`{"type":"object"}`), ReadOnly: true,
			Handler: func(context.Context, json.RawMessage) (*ToolResult, error) {
				return &ToolResult{Content: []string{"ok"}}, nil
			}},
		{Name: "rw_tool", Description: "b", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(context.Context, json.RawMessage) (*ToolResult, error) {
				return &ToolResult{Content: []string{"ok"}}, nil
			}},
	}
	c := startServer(t, tools)
	_ = c.call(t, "initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]string{"name": "x", "version": "0"}})
	raw := c.call(t, "tools/list", map[string]any{})

	// Decode loosely so the test is keyed on JSON wire shape, not the
	// Go struct (which could rename keys without breaking compile).
	var list struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byName := map[string]map[string]any{}
	for _, td := range list.Tools {
		byName[td["name"].(string)] = td
	}
	ro, ok := byName["ro_tool"]["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("ro_tool missing annotations object: %+v", byName["ro_tool"])
	}
	if ro["readOnlyHint"] != true {
		t.Errorf("ro_tool annotations.readOnlyHint = %v, want true", ro["readOnlyHint"])
	}
	if _, present := byName["rw_tool"]["annotations"]; present {
		t.Errorf("rw_tool should omit annotations entirely; got %+v", byName["rw_tool"]["annotations"])
	}
}

func TestMemoryCreateViewRoundtrip(t *testing.T) {
	root := t.TempDir()
	mem := &files.Backend{Root: root}
	// Pre-provision the writable directory so file_create can land
	// a file under artifacts/private/. Failure is non-fatal — the
	// test just wants the dir to exist.
	_ = files.Sync(files.BootstrapOptions{AgentRoot: filepath.Dir(root)})
	c := startServer(t, FilesystemTools(NewLocalFilesDispatcher(mem)))

	// Write a file.
	writeResult := c.call(t, "tools/call", map[string]any{
		"name": "file_create",
		"arguments": map[string]any{
			"path":      "/files/artifacts/private/draft.md",
			"file_text": "# Draft\n\nBody.\n",
		},
	})
	var write toolsCallResult
	if err := json.Unmarshal(writeResult, &write); err != nil {
		t.Fatalf("unmarshal write: %v", err)
	}
	if write.IsError {
		t.Fatalf("file_create returned error: %+v", write.Content)
	}

	// Read it back.
	readResult := c.call(t, "tools/call", map[string]any{
		"name": "file_view",
		"arguments": map[string]any{
			"path": "/files/artifacts/private/draft.md",
		},
	})
	var read toolsCallResult
	if err := json.Unmarshal(readResult, &read); err != nil {
		t.Fatalf("unmarshal read: %v", err)
	}
	if read.IsError {
		t.Fatalf("file_view returned error: %+v", read.Content)
	}
	if len(read.Content) == 0 || !strings.Contains(read.Content[0].Text, "# Draft") {
		t.Errorf("content = %+v", read.Content)
	}
}

func TestFileViewImageReturnsVisionBlock(t *testing.T) {
	root := t.TempDir()
	// Drop the PNG under artifacts/private/ — a writable subtree, so
	// file_view's path resolution is on a realistic codepath.
	if err := os.MkdirAll(filepath.Join(root, "artifacts", "private"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "private", "shot.png"), minimalPNG, 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}
	mem := &files.Backend{Root: root}
	c := startServer(t, FilesystemTools(NewLocalFilesDispatcher(mem)))

	res := c.call(t, "tools/call", map[string]any{
		"name": "file_view",
		"arguments": map[string]any{
			"path": "/files/artifacts/private/shot.png",
		},
	})
	var got toolsCallResult
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IsError {
		t.Fatalf("file_view returned error: %+v", got.Content)
	}
	// Expect: 1 text header + 1 image block.
	if len(got.Content) != 2 {
		t.Fatalf("expected 2 content items (text header + image), got %d: %+v", len(got.Content), got.Content)
	}
	if got.Content[0].Type != "text" {
		t.Errorf("content[0] type = %q, want text", got.Content[0].Type)
	}
	if !strings.Contains(got.Content[0].Text, "image/png") {
		t.Errorf("content[0] header missing mime: %q", got.Content[0].Text)
	}
	img := got.Content[1]
	if img.Type != "image" {
		t.Fatalf("content[1] type = %q, want image", img.Type)
	}
	if img.MIMEType != "image/png" {
		t.Errorf("image mimeType = %q, want image/png", img.MIMEType)
	}
	decoded, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("decode image data: %v", err)
	}
	if !bytes.Equal(decoded, minimalPNG) {
		t.Errorf("image bytes mismatch: got %d bytes, want %d", len(decoded), len(minimalPNG))
	}
}

func TestFileViewTextStillReturnsTextOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts", "private"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "private", "note.md"), []byte("# Note\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mem := &files.Backend{Root: root}
	c := startServer(t, FilesystemTools(NewLocalFilesDispatcher(mem)))

	res := c.call(t, "tools/call", map[string]any{
		"name":      "file_view",
		"arguments": map[string]any{"path": "/files/artifacts/private/note.md"},
	})
	var got toolsCallResult
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IsError {
		t.Fatalf("file_view returned error: %+v", got.Content)
	}
	for i, ci := range got.Content {
		if ci.Type != "text" {
			t.Errorf("content[%d] type = %q, want text (regression: text path emitted non-text block)", i, ci.Type)
		}
	}
}

func TestFileViewMissingImageReturnsToolError(t *testing.T) {
	mem := &files.Backend{Root: t.TempDir()}
	c := startServer(t, FilesystemTools(NewLocalFilesDispatcher(mem)))

	res := c.call(t, "tools/call", map[string]any{
		"name":      "file_view",
		"arguments": map[string]any{"path": "/files/missing.png"},
	})
	var got toolsCallResult
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.IsError {
		t.Fatalf("expected IsError=true for missing image")
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	c := startServer(t, nil)
	// Use raw JSON so we can look at the error envelope rather than
	// call(), which fatal()s on rpc errors.
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "does_not_exist"}
	buf, _ := json.Marshal(req)
	buf = append(buf, '\n')
	if _, err := c.toServer.Write(buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := c.fromServer.readMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp struct {
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Errorf("expected method-not-found error, got %+v", resp.Error)
	}
}

func TestUnknownToolReturnsToolError(t *testing.T) {
	// Unknown tool → MCP-level success with IsError=true, not a
	// JSON-RPC error. This matches MCP spec: transport is fine, the
	// tool call itself failed.
	c := startServer(t, nil)
	result := c.call(t, "tools/call", map[string]any{"name": "nope"})
	var r toolsCallResult
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !r.IsError {
		t.Error("expected IsError=true")
	}
	if len(r.Content) == 0 {
		t.Error("expected error content")
	}
	_ = bytes.NewReader // keep import
}

// TestServeDispatchesToolCallsConcurrently pins the parallel-dispatch
// contract: two tools/call requests arriving back-to-back must run
// their handlers concurrently, not back-to-back.
//
// Both handlers park on per-id release channels and signal entry on
// a shared `entered` channel. The test fires both calls, then waits
// to see BOTH entries before unblocking either. If dispatch were
// serial the second handler would never enter (the first would still
// be parked) and the test would deadlock — caught by the per-receive
// timeout.
//
// Deterministic: no time.Sleep for synchronization; only channels.
// The 5s timeout is a regression backstop, not a correctness signal.
func TestServeDispatchesToolCallsConcurrently(t *testing.T) {
	releases := map[string]chan struct{}{
		"A": make(chan struct{}),
		"B": make(chan struct{}),
	}
	entered := make(chan string, 2)

	blockTool := Tool{
		Name:        "block",
		Description: "test-only: park until the test releases the matching id",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			var in struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return &ToolResult{IsError: true, Content: []string{err.Error()}}, nil
			}
			ch, ok := releases[in.ID]
			if !ok {
				return &ToolResult{IsError: true, Content: []string{"unknown id"}}, nil
			}
			entered <- in.ID
			select {
			case <-ch:
				return &ToolResult{Content: []string{"released:" + in.ID}}, nil
			case <-ctx.Done():
				return &ToolResult{IsError: true, Content: []string{ctx.Err().Error()}}, nil
			}
		},
	}

	c := startServer(t, []Tool{blockTool})

	// Use raw writes (not c.call) — c.call is synchronous (write +
	// read), but we need to fire BOTH writes before reading any
	// response. A serial dispatch would never produce the second
	// response.
	send := func(id int, callID string) {
		req := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      "block",
				"arguments": map[string]any{"id": callID},
			},
		}
		buf, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf = append(buf, '\n')
		if _, err := c.toServer.Write(buf); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	send(1, "A")
	send(2, "B")

	got := map[string]bool{}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(got) < 2 {
		select {
		case id := <-entered:
			got[id] = true
		case <-deadline.C:
			t.Fatalf("did not see both handler entries within 5s; got=%v (regression: dispatch serialized?)", got)
		}
	}

	// Release in REVERSE order so responses land out-of-arrival-order;
	// proves writeMu serializes per-response bytes (responses parse as
	// well-formed JSON, not interleaved garbage).
	close(releases["B"])
	close(releases["A"])

	for i := 0; i < 2; i++ {
		line, err := c.fromServer.readMessage()
		if err != nil {
			t.Fatalf("read response %d: %v", i, err)
		}
		var resp struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("malformed response %d (interleaved bytes?) line=%q err=%v", i, line, err)
		}
		if resp.Error != nil {
			t.Fatalf("rpc error on response %d: %+v", i, resp.Error)
		}
	}
}
