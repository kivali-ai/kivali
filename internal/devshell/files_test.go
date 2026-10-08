package devshell

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalPNG is a 1x1 PNG, enough for file_view's image short-circuit.
const minimalPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNgYAAAAAMAASsJTYQAAAAASUVORK5CYII="

// postFiles sends one FilesRequest to the handler and returns the
// status and, on a 200, the decoded response.
func postFiles(t *testing.T, srv *httptest.Server, req FilesRequest) (int, FilesResponse) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return postFilesRaw(t, srv, body)
}

func postFilesRaw(t *testing.T, srv *httptest.Server, body []byte) (int, FilesResponse) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/files", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out FilesResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, out
}

func input(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return b
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHandlerFilesCreateAndView: the parent's calls (empty root) run
// against the daemon's --root, and what file_create wrote is on disk
// there and comes back from file_view.
func TestHandlerFilesCreateAndView(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: root}))
	defer srv.Close()

	status, res := postFiles(t, srv, FilesRequest{Tool: "file_create", Input: input(t, map[string]any{
		"path": "/files/artifacts/private/note.md", "file_text": "# hello\nfrom the daemon\n",
	})})
	if status != http.StatusOK || res.IsError || res.Err != "" {
		t.Fatalf("create: status=%d res=%+v", status, res)
	}
	got, err := os.ReadFile(filepath.Join(root, "artifacts", "private", "note.md"))
	if err != nil || string(got) != "# hello\nfrom the daemon\n" {
		t.Fatalf("on disk = %q, %v", got, err)
	}

	status, res = postFiles(t, srv, FilesRequest{Tool: "file_view", Input: input(t, map[string]any{
		"path": "/files/artifacts/private/note.md",
	})})
	if status != http.StatusOK || res.IsError {
		t.Fatalf("view: status=%d res=%+v", status, res)
	}
	if !strings.Contains(res.Body, "from the daemon") || res.Image != nil {
		t.Errorf("view = %+v, want the text and no image", res)
	}
}

// TestHandlerFilesSubagentRoot: a "subagents/<id>" root runs against
// the view core built there. The subagent's own artifacts/private/ is
// a real directory in the view; background/ is a relative link into
// the parent's tree, which the daemon admits as a WriteRoot.
func TestHandlerFilesSubagentRoot(t *testing.T) {
	root := t.TempDir()
	view := filepath.Join(root, "subagents", "abc")
	mkdirs(t, filepath.Join(view, "artifacts", "private"), filepath.Join(root, "background"), filepath.Join(root, "artifacts", "private"))
	if err := os.Symlink("../../background", filepath.Join(view, "background")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: root}))
	defer srv.Close()

	status, res := postFiles(t, srv, FilesRequest{Root: "subagents/abc", Tool: "file_create", Input: input(t, map[string]any{
		"path": "/files/artifacts/private/out.md", "file_text": "mine",
	})})
	if status != http.StatusOK || res.IsError {
		t.Fatalf("create private: status=%d res=%+v", status, res)
	}
	if _, err := os.Stat(filepath.Join(view, "artifacts", "private", "out.md")); err != nil {
		t.Errorf("subagent write did not land in its view: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts", "private", "out.md")); err == nil {
		t.Error("subagent write landed in the parent's artifacts/private")
	}

	status, res = postFiles(t, srv, FilesRequest{Root: "subagents/abc", Tool: "file_create", Input: input(t, map[string]any{
		"path": "/files/background/part.md", "file_text": "shared",
	})})
	if status != http.StatusOK || res.IsError {
		t.Fatalf("create background: status=%d res=%+v", status, res)
	}
	if got, err := os.ReadFile(filepath.Join(root, "background", "part.md")); err != nil || string(got) != "shared" {
		t.Errorf("background write = %q, %v; want it in the parent's background/", got, err)
	}
}

// TestHandlerFilesRejectsBadRoot: the root is "" or "subagents/<id>"
// and nothing else, so no request can aim the tools at an arbitrary
// directory.
func TestHandlerFilesRejectsBadRoot(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: root}))
	defer srv.Close()
	view := input(t, map[string]any{"path": "/files"})
	for _, bad := range []string{"/etc", "..", "../x", "subagents", "subagents/", "subagents/..", "subagents/.", "subagents/a/b", "subagents/../..", "other/abc", "artifacts"} {
		status, _ := postFiles(t, srv, FilesRequest{Root: bad, Tool: "file_view", Input: view})
		if status != http.StatusBadRequest {
			t.Errorf("root %q: status = %d, want 400", bad, status)
		}
	}
}

// TestHandlerFilesRejectsUnknownTool: only the file_* tools run here.
func TestHandlerFilesRejectsUnknownTool(t *testing.T) {
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: t.TempDir()}))
	defer srv.Close()
	status, _ := postFiles(t, srv, FilesRequest{Tool: "run_shell", Input: json.RawMessage(`{}`)})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestHandlerFilesImageView: file_view of an image returns its bytes
// and MIME for a vision block, with the header line as body.
func TestHandlerFilesImageView(t *testing.T) {
	root := t.TempDir()
	png, _ := base64.StdEncoding.DecodeString(minimalPNG)
	mkdirs(t, filepath.Join(root, "artifacts", "private"))
	if err := os.WriteFile(filepath.Join(root, "artifacts", "private", "shot.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: root}))
	defer srv.Close()

	status, res := postFiles(t, srv, FilesRequest{Tool: "file_view", Input: input(t, map[string]any{
		"path": "/files/artifacts/private/shot.png",
	})})
	if status != http.StatusOK || res.IsError {
		t.Fatalf("view: status=%d res=%+v", status, res)
	}
	if res.Image == nil || res.Image.MIME != "image/png" || !bytes.Equal(res.Image.Data, png) {
		t.Fatalf("image = %+v, want the PNG bytes", res.Image)
	}
	if !strings.Contains(res.Body, "[image:") {
		t.Errorf("body = %q, want the image header", res.Body)
	}
}

// TestHandlerFilesLargeBody: a file_create far past /v1/exec's 64 KiB
// cap goes through, and a body past the files cap is refused as too
// large rather than truncated.
func TestHandlerFilesLargeBody(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: root}))
	defer srv.Close()

	big := strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB
	status, res := postFiles(t, srv, FilesRequest{Tool: "file_create", Input: input(t, map[string]any{
		"path": "/files/artifacts/private/big.txt", "file_text": big,
	})})
	if status != http.StatusOK || res.IsError {
		t.Fatalf("1 MiB create: status=%d res=%+v", status, res)
	}
	if info, err := os.Stat(filepath.Join(root, "artifacts", "private", "big.txt")); err != nil || info.Size() != int64(len(big)) {
		t.Errorf("big.txt = %v, %v; want %d bytes", info, err, len(big))
	}

	small := httptest.NewServer(NewHandler(ServerConfig{Root: root, FilesMaxBodyBytes: 1 << 10}))
	defer small.Close()
	body, _ := json.Marshal(FilesRequest{Tool: "file_create", Input: input(t, map[string]any{
		"path": "/files/artifacts/private/too-big.txt", "file_text": strings.Repeat("x", 2<<10),
	})})
	if status, _ := postFilesRaw(t, small, body); status != http.StatusRequestEntityTooLarge {
		t.Errorf("over-cap body: status = %d, want 413", status)
	}
}

// TestHandlerFilesReadOnlyAndConfinement: the daemon's Backend keeps
// the file_* rules — writes under a core-managed subtree are refused,
// a farm link into a ReadRoot reads, and a link anywhere else does
// not.
func TestHandlerFilesReadOnlyAndConfinement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "files")
	readRoot := filepath.Join(base, "project_files")
	outside := filepath.Join(base, "secret")
	mkdirs(t, filepath.Join(root, "project"), filepath.Join(root, "artifacts", "private"), readRoot, outside)
	if err := os.WriteFile(filepath.Join(readRoot, "plan.md"), []byte("the plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "creds"), []byte("hunter2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(readRoot, "plan.md"), filepath.Join(root, "project", "plan.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "creds"), filepath.Join(root, "artifacts", "private", "creds")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: root, ReadRoots: []string{readRoot}}))
	defer srv.Close()

	_, res := postFiles(t, srv, FilesRequest{Tool: "file_create", Input: input(t, map[string]any{
		"path": "/files/project/new.md", "file_text": "nope",
	})})
	if !res.IsError {
		t.Errorf("create under project/: %+v, want a read-only error", res)
	}

	_, res = postFiles(t, srv, FilesRequest{Tool: "file_view", Input: input(t, map[string]any{"path": "/files/project/plan.md"})})
	if res.IsError || !strings.Contains(res.Body, "the plan") {
		t.Errorf("view of a farm link into a ReadRoot = %+v, want the plan", res)
	}

	_, res = postFiles(t, srv, FilesRequest{Tool: "file_view", Input: input(t, map[string]any{"path": "/files/artifacts/private/creds"})})
	if !res.IsError || strings.Contains(res.Body, "hunter2") {
		t.Errorf("view of a link out of the allowed roots = %+v, want a refusal", res)
	}
}

// TestSidecarFilesRoundTrip drives SidecarFiles against the real
// handler on a UDS: text, image, the subagent root, a malformed call
// (an error, as an in-process dispatcher returns) and a refused root
// (an error naming the HTTP status).
func TestSidecarFilesRoundTrip(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, filepath.Join(root, "subagents", "abc", "artifacts", "private"))
	png, _ := base64.StdEncoding.DecodeString(minimalPNG)
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: root})
	defer stop()
	ctx := context.Background()

	parent := &SidecarFiles{SocketPath: sock}
	res, err := parent.Dispatch(ctx, "file_create", input(t, map[string]any{
		"path": "/files/artifacts/private/note.md", "file_text": "from the parent",
	}))
	if err != nil || res.IsError {
		t.Fatalf("create: %+v, %v", res, err)
	}
	res, err = parent.Dispatch(ctx, "file_view", input(t, map[string]any{"path": "/files/artifacts/private/note.md"}))
	if err != nil || res.IsError || !strings.Contains(res.Body, "from the parent") {
		t.Fatalf("text view: %+v, %v", res, err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "private", "shot.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = parent.Dispatch(ctx, "file_view", input(t, map[string]any{"path": "/files/artifacts/private/shot.png"}))
	if err != nil || res.Image == nil || !bytes.Equal(res.Image.Data, png) {
		t.Fatalf("image view: %+v, %v", res, err)
	}

	sub := &SidecarFiles{SocketPath: sock, Root: "subagents/abc"}
	if res, err = sub.Dispatch(ctx, "file_create", input(t, map[string]any{
		"path": "/files/artifacts/private/sub.md", "file_text": "from the subagent",
	})); err != nil || res.IsError {
		t.Fatalf("subagent create: %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "subagents", "abc", "artifacts", "private", "sub.md")); err != nil {
		t.Errorf("subagent write not in its view: %v", err)
	}

	if _, err := parent.Dispatch(ctx, "file_view", json.RawMessage(`{"path": 7}`)); err == nil {
		t.Error("malformed arguments: want an error")
	}

	bad := &SidecarFiles{SocketPath: sock, Root: "../etc"}
	if _, err := bad.Dispatch(ctx, "file_view", input(t, map[string]any{"path": "/files"})); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("bad root: err = %v, want HTTP 400", err)
	}
}
