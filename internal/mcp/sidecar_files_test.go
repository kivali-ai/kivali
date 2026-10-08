package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/devshell"
)

// startDevShell serves the dev-shell daemon's handler on a UDS rooted
// at root and returns the socket path. /tmp keeps the path under
// darwin's sun_path limit.
func startDevShell(t *testing.T, root string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcp-ds-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: devshell.NewHandler(devshell.ServerConfig{Root: root}), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return sock
}

// TestSubagentToolkitFilesRunInDevShell: the subagent toolkit's file_*
// tools go wherever its Files dispatcher sends them; with the sidecar
// dispatcher that is the daemon, rooted at the subagent's view. A
// file_view of an image comes back as a vision block.
func TestSubagentToolkitFilesRunInDevShell(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "subagents", "abc", "artifacts", "private")
	if err := os.MkdirAll(private, 0o755); err != nil {
		t.Fatal(err)
	}
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNgYAAAAAMAASsJTYQAAAAASUVORK5CYII=")
	if err := os.WriteFile(filepath.Join(private, "shot.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	sock := startDevShell(t, root)

	tk := SubagentToolkit(SubagentDeps{
		Client:     newTestClient("alice"),
		SubagentID: "abc",
		Files:      NewSidecarFilesDispatcher(&devshell.SidecarFiles{SocketPath: sock, Root: "subagents/abc"}),
	})
	tools := map[string]Tool{}
	for _, tl := range tk.Tools {
		tools[tl.Name] = tl
	}

	res, err := tools["file_create"].Handler(context.Background(),
		json.RawMessage(`{"path":"/files/artifacts/private/out.md","file_text":"written in the dev-shell"}`))
	if err != nil || res.IsError {
		t.Fatalf("file_create: %+v, %v", res, err)
	}
	if got, err := os.ReadFile(filepath.Join(private, "out.md")); err != nil || string(got) != "written in the dev-shell" {
		t.Errorf("out.md = %q, %v", got, err)
	}

	res, err = tools["file_view"].Handler(context.Background(), json.RawMessage(`{"path":"/files/artifacts/private/shot.png"}`))
	if err != nil || res.IsError {
		t.Fatalf("file_view: %+v, %v", res, err)
	}
	if len(res.Images) != 1 || res.Images[0].MIMEType != "image/png" || !bytes.Equal(res.Images[0].Data, png) {
		t.Errorf("images = %+v, want the PNG", res.Images)
	}
	if len(res.Content) != 1 || !strings.Contains(res.Content[0], "[image:") {
		t.Errorf("content = %v, want the image header", res.Content)
	}
}

// recordingFiles is a FilesDispatcher that notes the tools it was asked
// to run.
type recordingFiles struct{ calls []string }

func (r *recordingFiles) DispatchFilesTool(_ context.Context, tool string, _ json.RawMessage) (string, bool, *FilesImage, error) {
	r.calls = append(r.calls, tool)
	return "ok", false, nil, nil
}

// TestFullAgentToolkitUsesFilesDispatcher: every file_* tool in the
// full-agent toolkit goes through the dispatcher it was given, so the
// pod's wiring alone decides where they execute.
func TestFullAgentToolkitUsesFilesDispatcher(t *testing.T) {
	rec := &recordingFiles{}
	tk := FullAgentToolkit(FullAgentDeps{Client: newTestClient("alice"), Files: rec})
	var want []string
	for _, tl := range tk.Tools {
		if !strings.HasPrefix(tl.Name, "file_") {
			continue
		}
		want = append(want, tl.Name)
		if _, err := tl.Handler(context.Background(), json.RawMessage(`{}`)); err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
	}
	if len(want) != 7 || strings.Join(rec.calls, ",") != strings.Join(want, ",") {
		t.Errorf("dispatched %v, want %v", rec.calls, want)
	}
}
