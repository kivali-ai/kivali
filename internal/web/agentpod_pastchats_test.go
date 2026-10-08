package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// startPastChatsServer wires the past-chats list + read endpoints
// on a Unix socket.
func startPastChatsServer(t *testing.T) (sockPath string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/past-chats", srv.handleAgentpodPastChatsList)
	mux.HandleFunc("GET /v1/agent/{slug}/past-chats/{ts}", srv.handleAgentpodPastChatsRead)
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sockPath)
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
	return sockPath, srv, cleanup
}

// TestAgentpodPastChatsListEmpty: a fresh agent with no archived
// chats returns an empty (non-nil) slice — distinguishable from a
// transport error.
func TestAgentpodPastChatsListEmpty(t *testing.T) {
	sockPath, _, cleanup := startPastChatsServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	got, err := c.ListArchivedChats(ctx)
	if err != nil {
		t.Fatalf("ListArchivedChats: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d archives, want 0", len(got))
	}
}

// TestAgentpodPastChatsReadRoundtrip: write a chat row, archive it,
// then list + read it back via the UDS endpoints.
func TestAgentpodPastChatsReadRoundtrip(t *testing.T) {
	sockPath, srv, cleanup := startPastChatsServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleSent, Kind: "direct_chat", Content: "the body to find",
	}); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	ts, err := srv.Store.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}

	chats, err := c.ListArchivedChats(ctx)
	if err != nil {
		t.Fatalf("ListArchivedChats: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("ListArchivedChats len = %d, want 1", len(chats))
	}
	if chats[0].Timestamp != ts {
		t.Errorf("Timestamp = %q, want %q", chats[0].Timestamp, ts)
	}

	hist, err := c.ReadArchivedChat(ctx, ts)
	if err != nil {
		t.Fatalf("ReadArchivedChat: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("hist len = %d, want 1", len(hist))
	}
	if hist[0].Content != "the body to find" {
		t.Errorf("Content = %q", hist[0].Content)
	}
}

// TestAgentpodPastChatsReadNotFound: an unknown timestamp returns
// a transport error (404), not an empty slice.
func TestAgentpodPastChatsReadNotFound(t *testing.T) {
	sockPath, _, cleanup := startPastChatsServer(t)
	defer cleanup()
	c := agentpod.NewClient(sockPath, "alice")
	ctx := context.Background()

	_, err := c.ReadArchivedChat(ctx, "1900-01-01T00-00-00Z")
	if err == nil {
		t.Fatal("expected error for unknown ts, got success")
	}
}
