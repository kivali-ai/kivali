package devshell

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerExecRoundTrip exercises the full daemon handler — JSON
// in, JSON out, command actually runs. End-to-end smoke for the
// wire contract the SidecarShell client depends on.
func TestHandlerExecRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: tmp}))
	defer srv.Close()

	body, _ := json.Marshal(ExecRequest{Command: "echo hi", TimeoutSeconds: 5})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0; err=%q", out.ExitCode, out.Err)
	}
	if !strings.Contains(out.Stdout, "hi") {
		t.Errorf("Stdout = %q, want contains 'hi'", out.Stdout)
	}
}

// TestHandlerRejectsBadCwd asserts a cwd-escape attempt surfaces in
// the Err field, NOT as a process exit. The model needs to see this
// as a tool_use is_error=true so it knows the request was refused
// rather than the command failed.
func TestHandlerRejectsBadCwd(t *testing.T) {
	tmp := t.TempDir()
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: tmp}))
	defer srv.Close()

	body, _ := json.Marshal(ExecRequest{Command: "echo nope", Cwd: "../../etc"})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out ExecResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Err == "" {
		t.Errorf("Err = empty, want a cwd-escape complaint; resp=%+v", out)
	}
	if out.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for env failure", out.ExitCode)
	}
}

// TestHandlerHealthz confirms the liveness endpoint exists. Without
// it, the k8s liveness probe (when wired) would mark the sidecar
// unhealthy and bounce the pod.
func TestHandlerHealthz(t *testing.T) {
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: t.TempDir()}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestHandlerRejectsEmptyCommand catches the missing-command guard.
// Without it, the daemon would happily run `bash -c ""` and return
// a confusing zero-exit no-output.
func TestHandlerRejectsEmptyCommand(t *testing.T) {
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: t.TempDir()}))
	defer srv.Close()

	body, _ := json.Marshal(ExecRequest{Command: "", TimeoutSeconds: 5})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestHandlerExitCodeNonZero confirms exit codes propagate through
// the wire. The most basic correctness check.
func TestHandlerExitCodeNonZero(t *testing.T) {
	srv := httptest.NewServer(NewHandler(ServerConfig{Root: t.TempDir()}))
	defer srv.Close()

	body, _ := json.Marshal(ExecRequest{Command: "exit 7", TimeoutSeconds: 5})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out ExecResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", out.ExitCode)
	}
}
