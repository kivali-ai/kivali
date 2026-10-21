package agentshell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/egresscred"
)

const testSecret = "c2hlbGwtbmV2ZXItc2Vlcy10aGlzLXNlY3JldC12YWx1ZQ"

// bash gets the daemon's proxy variables without user info.
func TestBashEnvHasTheBareProxyURL(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://alice:"+testSecret+"@10.43.0.12:3128")
	t.Setenv("HTTP_PROXY", "http://10.43.0.12:3128")
	env := strings.Join(bashEnv(""), "\n") + "\n"
	if strings.Contains(env, testSecret) {
		t.Fatalf("bash env holds the secret:\n%s", env)
	}
	for _, want := range []string{"HTTPS_PROXY=http://10.43.0.12:3128\n", "HTTP_PROXY=http://10.43.0.12:3128\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("bash env lacks %q:\n%s", want, env)
		}
	}
}

// A command or file the model wrote never hands the agent's egress
// proxy credential to the shell: the client scrubs it before the
// daemon sees the request.
func TestSidecarClientsScrubTheCredential(t *testing.T) {
	root := t.TempDir()
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: root})
	defer stop()
	credURL := "http://alice:" + testSecret + "@10.43.0.12:3128"
	scrub := egresscred.NewScrubber("alice", testSecret, credURL)

	sh := &SidecarShell{SocketPath: sock, Slug: "alice", Scrubber: scrub}
	res, err := sh.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "echo " + testSecret + " " + credURL,
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, testSecret) || !strings.Contains(res.Stdout, egresscred.Placeholder) {
		t.Fatalf("the shell ran a command holding the secret: stdout %q", res.Stdout)
	}

	files := &SidecarFiles{SocketPath: sock, Scrubber: scrub}
	in, _ := json.Marshal(map[string]any{"path": "/files/artifacts/private/k.txt", "file_text": "key " + testSecret})
	if _, err := files.Dispatch(context.Background(), "file_create", in); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "artifacts", "private", "k.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), testSecret) {
		t.Fatalf("the shell wrote the secret: %q", got)
	}
}
