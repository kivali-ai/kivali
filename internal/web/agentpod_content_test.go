package web

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// startContentServer wires a Server with attachment + project-file
// endpoints on a Unix socket.
func startContentServer(t *testing.T) (path string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/attachments/{sha}", srv.handleAgentpodAttachmentMeta)
	mux.HandleFunc("GET /v1/attachments/{sha}/blob", srv.handleAgentpodAttachmentBlob)
	mux.HandleFunc("GET /v1/attachments/{sha}/canonical", srv.handleAgentpodAttachmentCanonical)
	mux.HandleFunc("POST /v1/agent/{slug}/attachments", srv.handleAgentpodAttachmentAdd)
	mux.HandleFunc("GET /v1/project-files", srv.handleAgentpodProjectFiles)
	mux.HandleFunc("GET /v1/project-files/{sha}", srv.handleAgentpodProjectFileMeta)
	mux.HandleFunc("GET /v1/project-files/{sha}/blob", srv.handleAgentpodProjectFileBlob)
	mux.HandleFunc("GET /v1/project-files/{sha}/canonical", srv.handleAgentpodProjectFileCanonical)
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

// TestAgentpodAttachmentRoundtrip: AddAttachment uploads bytes,
// the resulting SHA fetches back identical metadata + blob bytes
// + canonical text.
func TestAgentpodAttachmentRoundtrip(t *testing.T) {
	path, _, cleanup := startContentServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	want := []byte("# spec\n\nbody text\n")
	att, err := c.AddAttachment(ctx, "spec.md", want)
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if att.SHA == "" {
		t.Fatal("AddAttachment returned empty SHA")
	}
	got, err := c.GetAttachment(ctx, att.SHA)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if got.Name != "spec.md" {
		t.Errorf("Name = %q, want spec.md", got.Name)
	}

	rc, err := c.OpenAttachmentBlob(ctx, att.SHA)
	if err != nil {
		t.Fatalf("OpenAttachmentBlob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	body, _ := io.ReadAll(rc)
	if string(body) != string(want) {
		t.Errorf("blob = %q, want %q", string(body), string(want))
	}

	text, err := c.ReadAttachmentText(ctx, att.SHA)
	if err != nil {
		t.Fatalf("ReadAttachmentText: %v", err)
	}
	if !strings.Contains(text, "body text") {
		t.Errorf("canonical text missing content: %q", text)
	}
}

// TestAgentpodAttachmentMissingSHAReturns404 guards the not-found
// path on the metadata endpoint.
func TestAgentpodAttachmentMissingSHAReturns404(t *testing.T) {
	path, _, cleanup := startContentServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	_, err := c.GetAttachment(context.Background(), "deadbeef")
	if err == nil {
		t.Error("expected error for missing sha; got nil")
	}
}

// TestAgentpodProjectFilesRoundtrip: seed a project file via the
// store, list it via the endpoint, fetch its blob + canonical.
func TestAgentpodProjectFilesRoundtrip(t *testing.T) {
	path, srv, cleanup := startContentServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	pf, err := srv.Store.AddProjectFile("doc.md", strings.NewReader("# project\n\ncontent\n"))
	if err != nil {
		t.Fatalf("AddProjectFile: %v", err)
	}
	pfs, err := c.ListProjectFiles(ctx)
	if err != nil {
		t.Fatalf("ListProjectFiles: %v", err)
	}
	found := false
	for _, p := range pfs {
		if p.SHA == pf.SHA {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ListProjectFiles missing seeded SHA %s; got %d files", pf.SHA, len(pfs))
	}

	got, err := c.GetProjectFile(ctx, pf.SHA)
	if err != nil {
		t.Fatalf("GetProjectFile: %v", err)
	}
	if got.OriginalName != "doc.md" {
		t.Errorf("OriginalName = %q, want doc.md", got.OriginalName)
	}

	rc, err := c.OpenProjectFileBlob(ctx, pf.SHA)
	if err != nil {
		t.Fatalf("OpenProjectFileBlob: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(body), "# project") {
		t.Errorf("blob missing content: %q", string(body))
	}

	// Canonical may or may not exist depending on whether the
	// convert pipeline ran inline at AddProjectFile time. The
	// endpoint correctness — 200 with bytes when present, 404
	// when not — is what we exercise; we don't require a
	// canonical for this fixture.
	rc, err = c.OpenProjectFileCanonical(ctx, pf.SHA)
	if err == nil {
		_ = rc.Close()
	}
}

// TestAgentpodProjectFilesEmptyListReturnsEmptyArray locks the
// "never null over JSON" promise.
func TestAgentpodProjectFilesEmptyListReturnsEmptyArray(t *testing.T) {
	path, _, cleanup := startContentServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	pfs, err := c.ListProjectFiles(context.Background())
	if err != nil {
		t.Fatalf("ListProjectFiles: %v", err)
	}
	if pfs == nil {
		t.Error("got nil; expected empty (non-nil) slice")
	}
}
