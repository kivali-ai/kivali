package devshell

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
)

// TestSidecarShellRoundTrip stands up the real handler on a UDS in
// a tempdir and exercises the full client→server path. End-to-end
// proof that ShellRequest → ExecRequest → exec → ExecResponse →
// ShellResult preserves every field the model relies on.
func TestSidecarShellRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: tmp})
	defer stop()

	c := &SidecarShell{SocketPath: sock, Slug: "alice"}
	res, err := c.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "echo hi",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hi\n" {
		t.Errorf("Stdout = %q, want \"hi\\n\"", res.Stdout)
	}
}

// TestSidecarShellCwdPrefix proves the CwdPrefix narrows the
// effective root the same way LocalShell's per-subagent ScratchRoot
// did. With prefix "subagents/abc" + user cwd "private", the daemon
// resolves <root>/subagents/abc/private — bash actually lands there
// AND can write a file we can verify from the test harness.
func TestSidecarShellCwdPrefix(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "subagents/abc/private"), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: tmp})
	defer stop()

	c := &SidecarShell{SocketPath: sock, CwdPrefix: "subagents/abc"}
	res, err := c.Exec(context.Background(), "parent", agent.ShellRequest{
		// Write a marker file. Asserting via filesystem state side-steps
		// the macOS /var → /private/var symlink mismatch that pwd output
		// would trigger.
		Command:        "echo here > marker.txt",
		Cwd:            "private",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, err=%q", res.ExitCode, res.Err)
	}
	wrote, rerr := os.ReadFile(filepath.Join(tmp, "subagents/abc/private/marker.txt"))
	if rerr != nil {
		t.Fatalf("marker file: %v", rerr)
	}
	if strings.TrimSpace(string(wrote)) != "here" {
		t.Errorf("marker contents = %q, want 'here'", strings.TrimSpace(string(wrote)))
	}
}

// TestSidecarShellStartupRetry asserts the client waits for the
// sidecar's socket to appear instead of failing on first dial. With
// a fresh agent pod, the agent runtime can race the dev-shell
// container's startup; the retry loop must absorb that.
func TestSidecarShellStartupRetry(t *testing.T) {
	tmp := t.TempDir()
	sock := shortSock(t)

	// Client points at a socket that doesn't exist yet. After a
	// short delay we stand the daemon up. Exec must succeed.
	c := &SidecarShell{
		SocketPath:   sock,
		StartupGrace: 2 * time.Second,
	}

	stopCh := make(chan func(), 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		stopCh <- startTestDaemon(t, sock, ServerConfig{Root: tmp})
	}()
	defer func() {
		select {
		case stop := <-stopCh:
			stop()
		case <-time.After(3 * time.Second):
			// Daemon never came up — the test failed for that reason.
		}
	}()

	res, err := c.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "echo waited",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("Exec after retry: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "waited\n" {
		t.Errorf("res = %+v, want exit=0 stdout=\"waited\\n\"", res)
	}
}

// TestSidecarShellGivesUpAfterGrace asserts the retry loop has a
// finite cap. Without it, an Exec against a permanently-down
// sidecar would hang the run_shell tool until the per-command
// timeout — leaking minutes of model time on a configuration error.
func TestSidecarShellGivesUpAfterGrace(t *testing.T) {
	c := &SidecarShell{
		SocketPath:   "/tmp/nope-this-socket-doesnt-exist.sock",
		StartupGrace: 200 * time.Millisecond,
	}
	start := time.Now()
	_, err := c.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "echo never",
		TimeoutSeconds: 5,
	})
	dur := time.Since(start)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Grace + a healthy slack for the final attempt and backoff.
	if dur > 5*time.Second {
		t.Errorf("Exec hung for %v, want under 5s — retry loop should give up after StartupGrace", dur)
	}
}

// TestSidecarShellPersistentHomeAcrossCalls is the headline UX
// behaviour: a file dropped in $HOME from one Exec is visible to
// the next, even though each call is a fresh bash subprocess. This
// is what powers "pip install --user persists" and ".bashrc edits
// stick" — bash subprocesses are stateless, the FILESYSTEM is the
// state. Without this, every call would start with an empty $HOME
// and pyenv / pip --user would be useless.
func TestSidecarShellPersistentHomeAcrossCalls(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "h")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BASH_ENV", "") // not relevant for this test
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: root})
	defer stop()

	c := &SidecarShell{SocketPath: sock, Slug: "alice"}

	// Drop a marker into $HOME from one call.
	if _, err := c.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "echo persistent > $HOME/marker.txt",
		TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("first Exec: %v", err)
	}

	// Read it back from a SEPARATE Exec — a different bash subprocess.
	res, err := c.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "cat $HOME/marker.txt",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("second Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "persistent" {
		t.Errorf("$HOME/marker.txt = %q, want 'persistent' (HOME state should persist across calls)", res.Stdout)
	}
}

// TestSidecarShellBashEnvSourced proves BASH_ENV is auto-sourced
// before every command, so an export in the shim file shows up to
// the next command. This is the hook agents use to make env
// changes durable.
func TestSidecarShellBashEnvSourced(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	bashEnv := filepath.Join(home, ".shell_env")
	if err := os.WriteFile(bashEnv, []byte("export TEST_FROM_BASHENV=hello-from-shim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BASH_ENV", bashEnv)
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: root})
	defer stop()

	c := &SidecarShell{SocketPath: sock, Slug: "alice"}
	res, err := c.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "echo $TEST_FROM_BASHENV",
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hello-from-shim" {
		t.Errorf("$TEST_FROM_BASHENV = %q, want hello-from-shim (BASH_ENV should be sourced)", res.Stdout)
	}
}

// TestSidecarShellConcurrentRequests proves fan-out works: N
// concurrent Execs to the daemon run as parallel processes. Without
// this, a parent agent fanning out 4 subagents that each call
// run_shell would queue, even though each is its own bash
// invocation.
func TestSidecarShellConcurrentRequests(t *testing.T) {
	root := t.TempDir()
	sock := shortSock(t)
	stop := startTestDaemon(t, sock, ServerConfig{Root: root})
	defer stop()
	c := &SidecarShell{SocketPath: sock, Slug: "alice"}

	const n = 4
	const sleepFor = 500 * time.Millisecond
	errs := make(chan error, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.Exec(ctx, "alice", agent.ShellRequest{
				Command:        "sleep 0.5",
				TimeoutSeconds: 3,
			})
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	dur := time.Since(start)
	// Serial would be 4×500ms = 2s; parallel should be ~500ms.
	// Generous slack for HTTP overhead.
	if dur > 1500*time.Millisecond {
		t.Errorf("4 concurrent calls took %v — should run in parallel (~%v), not serial (~2s)", dur, sleepFor)
	}
}

// TestJoinCwd pins the prefix-merging logic. Empty prefix is the
// full-agent shape (passes user cwd through); empty user cwd lands
// at the prefix root; both empty stays empty so the daemon resolves
// to its --root.
func TestJoinCwd(t *testing.T) {
	cases := []struct {
		prefix string
		user   string
		want   string
	}{
		{"", "", ""},
		{"", "artifacts/private", "artifacts/private"},
		{"subagents/abc", "", "subagents/abc"},
		{"subagents/abc", ".", "subagents/abc"},
		{"subagents/abc", "private", "subagents/abc/private"},
		// Surrounding slashes on prefix get cleaned (manifest builders
		// might compose paths with leading or trailing slashes).
		{"/subagents/abc/", "private", "subagents/abc/private"},
	}
	for _, tc := range cases {
		got := joinCwd(tc.prefix, tc.user)
		if got != tc.want {
			t.Errorf("joinCwd(%q, %q) = %q, want %q", tc.prefix, tc.user, got, tc.want)
		}
	}
}

// shortSock returns a UDS path under /tmp short enough to satisfy
// macOS's sun_path 104-char limit. t.TempDir() on darwin returns a
// path under /var/folders/... that already burns ~80 chars, so the
// socket file overflows. Production uses /run/kivali-dev-shell/sock —
// always short — so the limit isn't a runtime concern.
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ds")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// startTestDaemon binds a real UDS listener at sock and serves the
// production handler. Returns a stop func that closes the listener
// and removes the socket. Used by the round-trip tests so the wire
// path matches production exactly.
func startTestDaemon(t *testing.T, sock string, cfg ServerConfig) func() {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: NewHandler(cfg), ReadHeaderTimeout: 5 * time.Second}
	var done atomic.Bool
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
		done.Store(true)
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.Remove(sock)
		// Avoid leaking goroutines into the next test.
		for i := 0; i < 50 && !done.Load(); i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
}
