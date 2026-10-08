package agentpod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
)

// LocalShell is the in-pod shell executor. It implements
// agent.ShellExecutor by running `bash -c <command>` directly via
// os/exec inside the agent pod. With the unified /files/ namespace
// (see docs/developers/files-and-publishing.md), bash reads/writes the same physical
// paths file_view sees, so no input staging or output capture is needed.
//
// One LocalShell per agent pod. The Slug field is informational only
// (logged, not used for auth — the pod is single-tenant by k8s
// boundary). ScratchRoot is the absolute path bash starts in by
// default — typically /files/ in production so cwd-relative paths
// resolve under the agent's writable workspace. Tests pin a temp dir.
type LocalShell struct {
	ScratchRoot string
	Slug        string
}

// shell-exec caps: per-stream output buffer and the default/maximum
// timeout in seconds a command may request.
const (
	stdStreamCap   = 256 << 10 // 256 KiB per stdout/stderr buffer
	defaultTimeout = 60
	maxTimeout     = 600
)

// Exec runs req.Command via bash. cwd resolution is relative to
// ScratchRoot; bytes the command writes to /files/... persist on the
// PVC directly (no capture bookend). The slug arg is recorded but not
// used for routing — LocalShell is bound to one slug for its whole
// lifetime.
//
// Returns (*ShellResult, nil) for any outcome the model should see,
// even non-zero exit / timeout — those are normal shell semantics,
// not infrastructure failures. The error return is reserved for
// environment failures (cwd resolution, unable to start the process)
// that warrant a tool_use is_error=true.
func (s *LocalShell) Exec(ctx context.Context, _ string, req agent.ShellRequest) (*agent.ShellResult, error) {
	if s.ScratchRoot == "" {
		return nil, errors.New("agentpod.LocalShell: ScratchRoot is empty")
	}

	cwd, err := s.resolveCwd(req.Cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve cwd %q: %w", req.Cwd, err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cwd %q: %w", cwd, err)
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if req.TimeoutSeconds <= 0 {
		timeout = defaultTimeout * time.Second
	}
	if req.TimeoutSeconds > maxTimeout {
		timeout = maxTimeout * time.Second
	}

	res, runErr := runCommand(ctx, cwd, req.Command, timeout)
	if runErr != nil {
		// Non-exit error (e.g. cmd.Start failed). Surface as Err so
		// the chat-loop sees a tool_use is_error=true.
		res.Err = runErr.Error()
	}
	return &res, nil
}

// SyncSkills satisfies agent.ShellExecutor and does nothing: skills
// reach the shell through the per-agent /files/skills/ symlink farm,
// so no per-call sync is needed.
func (s *LocalShell) SyncSkills(_ context.Context, _ string) error {
	return nil
}

// resolveCwd normalizes req.Cwd to an absolute path under ScratchRoot.
// Empty cwd → ScratchRoot. Absolute paths and any form of `..`
// escape are rejected.
func (s *LocalShell) resolveCwd(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "." {
		return s.ScratchRoot, nil
	}
	if filepath.IsAbs(cwd) {
		return "", errors.New("cwd must be relative to scratch root")
	}
	clean := filepath.Clean(cwd)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", errors.New("cwd must not escape scratch root")
	}
	return filepath.Join(s.ScratchRoot, clean), nil
}

// runCommand invokes `bash -c <command>` in cwd with the given
// timeout: stdout/stderr
// in 256 KiB capped buffers (truncation marker appended when full),
// PATH/HOME/LANG inherited, proxy env vars forwarded.
func runCommand(ctx context.Context, cwd, command string, timeout time.Duration) (agent.ShellResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "bash", "-c", command)
	cmd.Dir = cwd
	var outBuf, errBuf cappedBuffer
	outBuf.cap = stdStreamCap
	errBuf.cap = stdStreamCap
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"PATH=" + pathEnv,
		"HOME=" + cwd,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	res := agent.ShellResult{
		Stdout:     outBuf.String(),
		Stderr:     errBuf.String(),
		DurationMs: dur.Milliseconds(),
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		// Non-exit error (cmd.Start failed, killed by signal without
		// status, etc). Caller surfaces as is_error=true.
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// cappedBuffer accumulates up to cap bytes from a Writer source and
// discards the rest silently. String() emits a truncation marker
// when bytes were dropped so callers know the buffer isn't
// authoritative.
type cappedBuffer struct {
	cap       int
	buf       []byte
	overflown bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(c.buf) < c.cap {
		room := c.cap - len(c.buf)
		if room > len(p) {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
		if room < len(p) {
			c.overflown = true
		}
	} else {
		c.overflown = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if !c.overflown {
		return string(c.buf)
	}
	return string(c.buf) + fmt.Sprintf("\n[... truncated at %d bytes ...]\n", c.cap)
}
