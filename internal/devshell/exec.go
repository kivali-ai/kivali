package devshell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// shell-exec caps. The same values as agentpod.LocalShell's, so a
// command behaves the same whichever executor runs it.
const (
	StdStreamCap   = 256 << 10 // 256 KiB per stdout/stderr buffer
	DefaultTimeout = 60        // seconds
	MaxTimeout     = 600       // seconds
)

// overlayBindScript is the bash script `unshare -mUr` runs when a
// subagent shell call requests overlay isolation. It bind-mounts
// $KIVALI_OVERLAY_PRIVATE over $KIVALI_OVERLAY_TARGET inside a
// fresh mount namespace, then exec's the user's command via
// `bash -c "$KIVALI_USER_CMD"` so arbitrary shell metacharacters
// in the command don't have to round-trip through another quoting
// layer.
//
// `mount --make-rprivate /` is required up front: without it,
// kernel-default shared mount propagation could leak the bind-mount
// back into the parent namespace (and unmount on namespace teardown
// could rip a still-in-use parent mount). The bind-mount itself
// requires CAP_SYS_ADMIN, which `-r` (map-root-user inside the user
// namespace) provides without privileges on the dev-shell sidecar's
// real UID.
const overlayBindScript = `set -e
mount --make-rprivate / 2>/tmp/.kivali-overlay-err || {
  echo "subagent isolation: make-rprivate failed: $(cat /tmp/.kivali-overlay-err 2>/dev/null)" >&2
  exit 250
}
mount --bind "$KIVALI_OVERLAY_SRC" "$KIVALI_OVERLAY_DST" 2>/tmp/.kivali-overlay-err || {
  echo "subagent isolation: bind-mount $KIVALI_OVERLAY_SRC -> $KIVALI_OVERLAY_DST failed: $(cat /tmp/.kivali-overlay-err 2>/dev/null)" >&2
  exit 251
}
exec bash -c "$KIVALI_USER_CMD"
`

// RunCommand invokes `bash -c <command>` in cwd with the given timeout.
// Stateless: each call is a fresh subprocess. Persistent state (env
// exports, pip --user installs, pyenv shims) survives across calls
// via the on-disk HOME — the daemon points HOME at the per-agent
// PVC, and BASH_ENV at a shim file every bash sources at startup.
//
// When overlayPrivate is non-empty (subagent caller), bash runs inside
// `unshare -mUr` with overlayPrivate bind-mounted over
// <RootDefault>/artifacts/private — so the subagent's view of
// /files/artifacts/private is its own overlay, not the parent's. See
// ExecRequest.OverlayPrivate for the rationale.
//
// Returns an ExecResponse populated for any outcome the model should
// see — non-zero exit + timeouts are normal shell semantics, not
// errors. The error return is reserved for environment failures
// (cmd.Start fail, signal-without-status) that warrant tool_use
// is_error=true upstream.
//
// stdout/stderr are each captured in a 256 KiB buffer; when full the
// remainder is dropped silently and a truncation marker is appended
// so the consumer knows the buffer isn't authoritative.
func RunCommand(ctx context.Context, cwd, command, overlaySrc, overlayDst string, timeout time.Duration) (ExecResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := bashEnv()
	var cmd *exec.Cmd
	if overlaySrc == "" {
		cmd = exec.CommandContext(cctx, "bash", "-c", command)
	} else {
		// The wrapper script reads its inputs from env vars so the
		// user's command (with arbitrary quoting / shell metacharacters)
		// never has to be re-escaped through another shell layer. The
		// caller is responsible for picking the destination path —
		// production passes <Root>/artifacts/private (the path the
		// file_* tools resolve under the overlay); tests
		// pin a writable temp dir.
		cmd = exec.CommandContext(cctx, "unshare", "-mUr", "--", "bash", "-c", overlayBindScript)
		env = append(env,
			"KIVALI_OVERLAY_SRC="+overlaySrc,
			"KIVALI_OVERLAY_DST="+overlayDst,
			"KIVALI_USER_CMD="+command,
		)
	}
	cmd.Dir = cwd
	var outBuf, errBuf cappedBuffer
	outBuf.cap = StdStreamCap
	errBuf.cap = StdStreamCap
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Env = env

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	res := ExecResponse{
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
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// bashEnv builds the env every bash subprocess inherits. PATH, HOME,
// LANG, BASH_ENV, and proxy vars come from the daemon's own
// environment (set via Dockerfile ENV + k8s pod env).
//
// HOME is the load-bearing one: agents' pip --user installs, pyenv
// state, and ~/.bashrc all live under $HOME. The Dockerfile sets
// HOME to /scratch/home; /scratch is a per-agent PVC so the dir
// survives pod restart + chat rotation. BASH_ENV points at a shim
// file bash sources at the start of every non-interactive shell —
// this is the hook agents use to make their environment changes
// stick (export new vars, prepend to PATH, source pyenv init).
func bashEnv() []string {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"PATH=" + pathEnv,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	if h := os.Getenv("HOME"); h != "" {
		env = append(env, "HOME="+h)
	}
	if be := os.Getenv("BASH_ENV"); be != "" {
		env = append(env, "BASH_ENV="+be)
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// ResolveCwd normalises req.Cwd to an absolute path under root. Empty
// or "." → root. Absolute paths and any form of `..` escape are
// rejected. The caller is responsible for MkdirAll on the result.
func ResolveCwd(root, cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == "." {
		return root, nil
	}
	if filepath.IsAbs(cwd) {
		return "", errors.New("cwd must be relative to root")
	}
	clean := filepath.Clean(cwd)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", errors.New("cwd must not escape root")
	}
	return filepath.Join(root, clean), nil
}

// cappedBuffer accumulates up to cap bytes from a Writer source and
// discards the rest silently. String() emits a truncation marker when
// bytes were dropped so the consumer knows the buffer isn't full.
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

// TimeoutFor clamps a request's TimeoutSeconds into the supported
// range and returns the resulting time.Duration.
func TimeoutFor(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultTimeout * time.Second
	}
	if seconds > MaxTimeout {
		return MaxTimeout * time.Second
	}
	return time.Duration(seconds) * time.Second
}
