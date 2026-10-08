package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// startStateServer wires a Server with the chat / role / memory
// endpoints on a Unix socket.
func startStateServer(t *testing.T) (path string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "# initial role\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/chat/history", srv.handleAgentpodChatHistory)
	mux.HandleFunc("POST /v1/agent/{slug}/chat/append", srv.handleAgentpodChatAppend)
	mux.HandleFunc("/v1/agent/{slug}/role", srv.handleAgentpodRole)
	mux.HandleFunc("/v1/agent/{slug}/memory", srv.handleAgentpodMemory)
	mux.HandleFunc("POST /v1/agent/{slug}/memory/dispatch", srv.handleAgentpodMemoryDispatch)
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return path, srv, cleanup
}

// TestAgentpodChatHistoryRoundtrip: append a row via the endpoint,
// read the history back, see the row.
func TestAgentpodChatHistoryRoundtrip(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	// Initial history: empty.
	hist, err := c.ReadChatHistory(ctx)
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	if len(hist) != 0 {
		t.Errorf("initial history len = %d, want 0", len(hist))
	}

	// Append a row.
	if err := c.AppendChatMessage(ctx, store.ChatMessage{
		Role:    store.RoleSent,
		Kind:    "direct_chat",
		Content: "hello world",
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	hist, err = c.ReadChatHistory(ctx)
	if err != nil {
		t.Fatalf("ReadChatHistory after append: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history len = %d, want 1", len(hist))
	}
	if hist[0].Content != "hello world" {
		t.Errorf("content = %q, want hello world", hist[0].Content)
	}
	if hist[0].Kind != "direct_chat" {
		t.Errorf("kind = %q, want direct_chat", hist[0].Kind)
	}
}

// TestAgentpodRoleRoundtrip: write + read role.md.
func TestAgentpodRoleRoundtrip(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	got, err := c.ReadRole(ctx)
	if err != nil {
		t.Fatalf("ReadRole: %v", err)
	}
	// Seeded as "# initial role\n" by startStateServer.
	if got != "# initial role\n" {
		t.Errorf("initial role = %q, want %q", got, "# initial role\n")
	}

	if err := c.WriteRole(ctx, "# new role\n\nDo a different thing.\n"); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	got, err = c.ReadRole(ctx)
	if err != nil {
		t.Fatalf("ReadRole after write: %v", err)
	}
	if got != "# new role\n\nDo a different thing.\n" {
		t.Errorf("read after write = %q", got)
	}
}

// TestAgentpodMemoryWriteRead: WriteMemory replaces; ReadMemory
// returns the new bytes.
func TestAgentpodMemoryWriteRead(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	if err := c.WriteMemory(ctx, "first version"); err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	got, err := c.ReadMemory(ctx)
	if err != nil {
		t.Fatalf("ReadMemory: %v", err)
	}
	if got != "first version" {
		t.Errorf("got %q, want first version", got)
	}
}

// TestAgentpodRoleETagHeader: a GET on /role emits an ETag, and a
// subsequent GET with If-None-Match matching that ETag returns 304
// (no body, no fresh JSON). Locks the wire-side cache validation
// shape that internal/agentpod/cache.go relies on.
func TestAgentpodRoleETagHeader(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()

	url := "http://core/v1/agent/alice/role"
	httpClient := unixHTTPClient(t, path)

	req1, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	req1.Header.Set(agentpod.SlugHeader, "alice")
	resp, err := httpClient.Do(req1)
	if err != nil {
		t.Fatalf("GET role: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("first GET missing ETag header")
	}

	// Second GET with If-None-Match — expect 304.
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	req2.Header.Set("If-None-Match", etag)
	req2.Header.Set(agentpod.SlugHeader, "alice")
	resp2, err := httpClient.Do(req2)
	if err != nil {
		t.Fatalf("GET role w/ If-None-Match: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match status = %d, want 304", resp2.StatusCode)
	}

	// After a write, the new GET must return 200 (different ETag).
	if err := writeRoleViaUnixClient(t, httpClient, "alice", "# changed\n"); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	req3, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	req3.Header.Set("If-None-Match", etag)
	req3.Header.Set(agentpod.SlugHeader, "alice")
	resp3, err := httpClient.Do(req3)
	if err != nil {
		t.Fatalf("GET role after write: %v", err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("post-write status = %d, want 200 (etag should differ)", resp3.StatusCode)
	}
	if got := resp3.Header.Get("ETag"); got == etag {
		t.Errorf("post-write ETag unchanged: %q", got)
	}
}

// unixHTTPClient builds an http.Client that dials the supplied
// Unix socket. Mirrors the dialer pattern in agentpod.NewClient
// but drops the typed wrapper so this test can hit raw HTTP.
func unixHTTPClient(t *testing.T, sockPath string) *http.Client {
	t.Helper()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

// writeRoleViaUnixClient is a tiny helper for the ETag test —
// POSTs the standard {"body": ...} envelope at /role.
func writeRoleViaUnixClient(t *testing.T, c *http.Client, slug, body string) error {
	t.Helper()
	envelope, _ := json.Marshal(map[string]string{"body": body})
	url := "http://core/v1/agent/" + slug + "/role"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(envelope))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agentpod.SlugHeader, slug)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return errors.New("write failed: " + resp.Status)
	}
	return nil
}

// TestAgentpodMemoryAppend: AppendMemory adds without overwriting.
func TestAgentpodMemoryAppend(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	if err := c.WriteMemory(ctx, "line one\n"); err != nil {
		t.Fatalf("WriteMemory: %v", err)
	}
	if err := c.AppendMemory(ctx, "line two\n"); err != nil {
		t.Fatalf("AppendMemory: %v", err)
	}
	got, err := c.ReadMemory(ctx)
	if err != nil {
		t.Fatalf("ReadMemory: %v", err)
	}
	// AppendAgentMemory inserts a separator newline before appending
	// when the existing content doesn't end with one — but our seed
	// ends with \n. Just assert both substrings are present.
	if !contains(got, "line one") || !contains(got, "line two") {
		t.Errorf("got %q, want both lines", got)
	}
}

// TestAgentpodMemoryDispatchAppend: dispatch endpoint runs an
// agent_memory_append, the bytes land on disk, ReadMemory sees
// them.
func TestAgentpodMemoryDispatchAppend(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	// Seed memory so append concatenates instead of starting empty.
	if err := c.WriteMemory(ctx, "existing line\n"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	resp, err := c.MemoryDispatch(ctx, agent.AgentMemoryAppendToolName, json.RawMessage(`{"text":"new note"}`))
	if err != nil {
		t.Fatalf("MemoryDispatch: %v", err)
	}
	if resp.IsError {
		t.Fatalf("dispatch returned IsError=true: %s", resp.Body)
	}
	if !contains(resp.Body, "appended") {
		t.Errorf("dispatch body = %q, want it to mention append", resp.Body)
	}
	got, err := c.ReadMemory(ctx)
	if err != nil {
		t.Fatalf("ReadMemory after dispatch: %v", err)
	}
	if !contains(got, "existing line") || !contains(got, "new note") {
		t.Errorf("memory after dispatch = %q, want both lines", got)
	}
}

// TestAgentpodMemoryDispatchStrReplace: str_replace runs through the
// dispatch endpoint and surfaces tool-level errors as IsError.
func TestAgentpodMemoryDispatchStrReplace(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	if err := c.WriteMemory(ctx, "alpha beta gamma\n"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	// Happy path.
	resp, err := c.MemoryDispatch(ctx, agent.AgentMemoryStrReplaceToolName, json.RawMessage(`{"old_str":"beta","new_str":"BETA"}`))
	if err != nil {
		t.Fatalf("MemoryDispatch: %v", err)
	}
	if resp.IsError {
		t.Errorf("dispatch returned IsError=true: %s", resp.Body)
	}
	got, err := c.ReadMemory(ctx)
	if err != nil {
		t.Fatalf("ReadMemory: %v", err)
	}
	if !contains(got, "alpha BETA gamma") {
		t.Errorf("memory after str_replace = %q, want substituted", got)
	}

	// not-found surfaces as IsError, not a transport error.
	resp, err = c.MemoryDispatch(ctx, agent.AgentMemoryStrReplaceToolName, json.RawMessage(`{"old_str":"missing","new_str":"x"}`))
	if err != nil {
		t.Fatalf("MemoryDispatch (not-found): %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError=true for not-found, got %+v", resp)
	}
}

// TestAgentpodMemoryDispatchUnknownTool: an unrecognized tool name
// surfaces as IsError on the dispatch result, not a transport error.
func TestAgentpodMemoryDispatchUnknownTool(t *testing.T) {
	path, _, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	resp, err := c.MemoryDispatch(ctx, "agent_memory_nope", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("MemoryDispatch: %v", err)
	}
	if !resp.IsError {
		t.Errorf("expected IsError=true for unknown tool, got %+v", resp)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
