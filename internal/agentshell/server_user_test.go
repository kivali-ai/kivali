//go:build unix

package agentshell

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ServerConfig.CommandUser has to reach the fork: a uid the test user
// cannot become makes the command fail to start with EPERM. It guards
// against the config field being dropped between the handler and
// RunCommand, which would run every command as the daemon with no
// error. As root the setuid would succeed, so the test does not run.
func TestHandlerExecPassesCommandUserToTheFork(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can become any uid, so the command would start")
	}
	srv := httptest.NewServer(NewHandler(ServerConfig{
		Root:        t.TempDir(),
		CommandUser: &CommandUser{UID: uint32(os.Geteuid() + 1), GID: uint32(os.Getegid())},
	}))
	defer srv.Close()

	body, _ := json.Marshal(ExecRequest{Command: "true", TimeoutSeconds: 5})
	resp, err := http.Post(srv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ExitCode != -1 || !strings.Contains(out.Err, "operation not permitted") {
		t.Errorf("ExitCode = %d, Err = %q; want -1 and %q", out.ExitCode, out.Err, "operation not permitted")
	}
}
