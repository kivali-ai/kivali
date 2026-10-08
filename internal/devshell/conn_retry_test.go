package devshell

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/clock"
)

// startDyingDaemon listens on sock and, for every connection, reads
// one whole request and closes the connection without answering: a
// daemon that applied the call and died before it could respond.
// Returns the count of requests it received.
func startDyingDaemon(t *testing.T, sock string) *atomic.Int32 {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var n atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if req, err := http.ReadRequest(bufio.NewReader(c)); err == nil {
				_, _ = io.Copy(io.Discard, req.Body)
				n.Add(1)
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return &n
}

// A call the daemon received is never sent again: it may already have
// been applied. Only a failed dial is retried. The fake clock never
// moves, so a retry would wait on it forever instead of returning.
func TestSidecarDoesNotResendAReceivedCall(t *testing.T) {
	sock := shortSock(t)
	received := startDyingDaemon(t, sock)
	clk := clock.NewFake()

	f := &SidecarFiles{SocketPath: sock, Clock: clk}
	if _, err := f.Dispatch(context.Background(), "file_insert", json.RawMessage(`{"path":"/files/a.md","insert_line":0,"new_str":"x"}`)); err == nil {
		t.Fatal("file call to a daemon that died mid-call: want an error")
	}
	if got := received.Load(); got != 1 {
		t.Errorf("file call received %d times, want 1", got)
	}

	s := &SidecarShell{SocketPath: sock, Clock: clk}
	if _, err := s.Exec(context.Background(), "alice", agent.ShellRequest{Command: "echo >> log", TimeoutSeconds: 5}); err == nil {
		t.Fatal("shell call to a daemon that died mid-call: want an error")
	}
	if got := received.Load(); got != 2 {
		t.Errorf("shell call received %d times in all, want 2", got)
	}
}

// A dense HTML file_create is sent as it came: json.Marshal would
// write each <, > and & as a six-byte escape and push a file the MCP
// server accepted past the daemon's body cap.
func TestSidecarFilesDoesNotHTMLEscapeTheBody(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, filepath.Join(root, "artifacts", "private"))
	html := strings.Repeat("<a>&", 1<<10) // 4 KiB; about 19 KiB escaped
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: root, FilesMaxBodyBytes: 8 << 10})
	defer stop()

	f := &SidecarFiles{SocketPath: sock}
	raw := json.RawMessage(`{"path":"/files/artifacts/private/page.html","file_text":"` + html + `"}`)
	res, err := f.Dispatch(context.Background(), "file_create", raw)
	if err != nil || res.IsError {
		t.Fatalf("create: %+v, %v", res, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "artifacts", "private", "page.html")); err != nil || string(b) != html {
		t.Errorf("page.html = %d bytes, %v; want the %d bytes sent", len(b), err, len(html))
	}
}
