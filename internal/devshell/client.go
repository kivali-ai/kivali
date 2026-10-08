package devshell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/clock"
)

// SidecarShell implements agent.ShellExecutor by dispatching commands
// to the dev-shell sidecar over its Unix-domain socket. The agent
// runtime container holds no shell-exec state; bash + the dev tooling
// live in the sidecar's image, which alone mounts the agent's /files/.
//
// Slug is informational (logged, not authenticated — the socket lives
// on a pod-local emptyDir, so reachability IS the auth).
type SidecarShell struct {
	SocketPath string
	Slug       string

	// CwdPrefix narrows the effective root for this client. The
	// daemon's --root is /files/ for every agent pod; full-agent
	// callers leave this empty and resolve cwd against /files/
	// directly. Subagent callers set it to e.g. "subagents/<id>" so
	// the user-supplied cwd lands under the subagent's narrowed
	// view (matching LocalShell's per-subagent ScratchRoot).
	CwdPrefix string

	// Clock drives the dial-retry backoff and the startup deadline.
	// Nil means the real clock; tests inject a clock.Fake so the
	// retry behaviour is proved rather than waited out.
	Clock clock.Clock

	// OverlayPrivate, when set, is forwarded as ExecRequest.OverlayPrivate
	// so the daemon runs bash inside `unshare -mUr` with this path
	// bind-mounted over /files/artifacts/private. Subagent callers set
	// it to <overlay-root>/artifacts/private; full-agent callers leave
	// it empty (no isolation needed — full agents own their /files/).
	OverlayPrivate string

	// DialTimeout caps each individual UDS connection attempt. Zero
	// → 5s. The sidecar can take a few seconds to come up on cold
	// pod start; the first Exec call rides the retry loop.
	DialTimeout time.Duration

	// StartupGrace is how long Exec will retry a connection refused /
	// no-such-file before giving up on the socket. Zero → 30s, which
	// covers a slow sidecar image pull.
	StartupGrace time.Duration

	connOnce sync.Once
	conn     *sidecarConn
}

// Exec satisfies agent.ShellExecutor. The slug arg is recorded but
// not used for routing — SidecarShell is bound to one slug for its
// whole lifetime.
func (s *SidecarShell) Exec(ctx context.Context, _ string, req agent.ShellRequest) (*agent.ShellResult, error) {
	if s.SocketPath == "" {
		return nil, errors.New("devshell.SidecarShell: SocketPath is empty")
	}
	cwd := joinCwd(s.CwdPrefix, req.Cwd)
	body, err := marshalRequest(ExecRequest{
		Command:        req.Command,
		Cwd:            cwd,
		TimeoutSeconds: req.TimeoutSeconds,
		OverlayPrivate: s.OverlayPrivate,
	})
	if err != nil {
		return nil, fmt.Errorf("devshell: marshal ExecRequest: %w", err)
	}

	// The client-side context must outlast the daemon's per-command
	// timeout — otherwise we'd cancel the dial before the daemon
	// could surface a TimedOut response. Add a generous buffer above
	// MaxTimeout so a long-running command can finish naturally.
	clientTimeout := TimeoutFor(req.TimeoutSeconds) + 30*time.Second
	cctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(cctx, http.MethodPost,
		"http://dev-shell/v1/exec", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("devshell: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := s.connection().do(hreq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("devshell: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("devshell: decode ExecResponse: %w", err)
	}
	return &agent.ShellResult{
		ExitCode:   out.ExitCode,
		Stdout:     out.Stdout,
		Stderr:     out.Stderr,
		DurationMs: out.DurationMs,
		TimedOut:   out.TimedOut,
		Err:        out.Err,
	}, nil
}

// Packages satisfies agent.ShellPackageLister: the apt packages the
// dev-shell image has installed, from GET /v1/packages. An empty list
// means the image records none. The call rides the same dial retry as
// Exec, so a caller that must not wait out the startup grace bounds ctx.
func (s *SidecarShell) Packages(ctx context.Context) ([]string, error) {
	if s.SocketPath == "" {
		return nil, errors.New("devshell.SidecarShell: SocketPath is empty")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dev-shell/v1/packages", nil)
	if err != nil {
		return nil, fmt.Errorf("devshell: build request: %w", err)
	}
	resp, err := s.connection().do(hreq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("devshell: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out PackagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("devshell: decode PackagesResponse: %w", err)
	}
	return out.Packages, nil
}

// SyncSkills satisfies agent.ShellExecutor. Skills are surfaced to bash
// via the per-agent /files/skills/ symlink farm; no per-call sync is
// needed. Kept on the interface for parity with LocalShell.
func (s *SidecarShell) SyncSkills(_ context.Context, _ string) error { return nil }

// joinCwd combines a static CwdPrefix (e.g. "subagents/<id>") with a
// per-request user cwd. Result is always a path RELATIVE to the
// daemon's --root (/files/) so the daemon's ResolveCwd does the
// final escape-checking. Empty prefix is the full-agent case;
// empty user cwd is "start at the prefix root."
func joinCwd(prefix, userCwd string) string {
	prefix = strings.Trim(prefix, "/")
	userCwd = strings.TrimSpace(userCwd)
	switch {
	case prefix == "" && (userCwd == "" || userCwd == "."):
		return ""
	case prefix == "":
		return userCwd
	case userCwd == "" || userCwd == ".":
		return prefix
	default:
		return prefix + "/" + userCwd
	}
}

// connection returns the shell's daemon connection, built once from
// its fields. Lazily, because SidecarShell is constructed as a
// literal.
func (s *SidecarShell) connection() *sidecarConn {
	s.connOnce.Do(func() {
		s.conn = newSidecarConn(s.SocketPath, s.Clock, s.DialTimeout, s.StartupGrace)
	})
	return s.conn
}

// SidecarFiles runs file_* tool calls in the dev-shell container
// through the daemon's POST /v1/files. The daemon builds the Backend
// from its own --root, --agent and Root below, so the calls run with
// exactly the shell's mounts and privileges; this side only names the
// tool, the arguments and the view.
type SidecarFiles struct {
	SocketPath string

	// Root selects the view, relative to the daemon's --root: empty
	// for the agent itself, "subagents/<id>" for a subagent (see
	// FilesRequest.Root).
	Root string

	// Clock, DialTimeout and StartupGrace are as on SidecarShell.
	Clock        clock.Clock
	DialTimeout  time.Duration
	StartupGrace time.Duration

	connOnce sync.Once
	conn     *sidecarConn
}

// filesCallTimeout bounds one file_* round trip. A call is one
// filesystem operation on a local mount; the bound only keeps a wedged
// daemon from holding a tool call forever.
const filesCallTimeout = 2 * time.Minute

// Dispatch runs one file_* call. A tool-level failure comes back as
// IsError on the response; an error return means the call did not run
// (the daemon was unreachable or refused the request) or its arguments
// were malformed.
func (f *SidecarFiles) Dispatch(ctx context.Context, tool string, input json.RawMessage) (*FilesResponse, error) {
	if f.SocketPath == "" {
		return nil, errors.New("devshell.SidecarFiles: SocketPath is empty")
	}
	body, err := marshalRequest(FilesRequest{Tool: tool, Input: input, Root: f.Root})
	if err != nil {
		return nil, fmt.Errorf("devshell: marshal FilesRequest: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, filesCallTimeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(cctx, http.MethodPost,
		"http://dev-shell/v1/files", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("devshell: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")

	f.connOnce.Do(func() {
		f.conn = newSidecarConn(f.SocketPath, f.Clock, f.DialTimeout, f.StartupGrace)
	})
	// Retry during the startup grace like run_shell: a fresh pod's
	// first tool call may well be a file_view.
	resp, err := f.conn.do(hreq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("devshell: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out FilesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("devshell: decode FilesResponse: %w", err)
	}
	if out.Err != "" {
		return nil, errors.New(out.Err)
	}
	return &out, nil
}

// marshalRequest encodes a request body without HTML escaping.
// json.Marshal writes each <, > and & as a six-byte \u escape, inside a
// RawMessage too, so a dense HTML or XML file_create the MCP server
// accepted could grow past FilesMaxBodyBytes on the way here and be
// refused with a 413. Nothing reading these bodies is a browser.
func marshalRequest(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
