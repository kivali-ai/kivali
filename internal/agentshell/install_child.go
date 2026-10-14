package agentshell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// InstallTimeout bounds a whole install. The install child enforces
// it, because it is root and can stop apt-get; the daemon, unprivileged
// by then, cannot.
const InstallTimeout = 20 * time.Minute

const reasonNotRoot = "the shell does not run as root, so it cannot install software"

// BecomeUnprivileged is the daemon's start-up privilege step, run
// before it touches HOME, creates its socket or runs anything for the
// agent. It returns the install the daemon started with (nil when
// there is none) and an error the caller must treat as fatal.
//
// Started as root (any of the real, effective or saved uid 0): when
// pkgs is non-empty it starts the install with start, which runs it in
// a child process that stays root, then drops this process to uid:gid
// (DropPrivileges, which verifies the result). It drops also when there
// is nothing to install: the daemon never serves as root.
//
// Started as anyone else: nothing changes. A non-empty pkgs is reported
// as every package failed, because only root can install. Any group ID
// of 0 is an error.
func BecomeUnprivileged(c Creds, uid, gid int, pkgs []string, start func([]string) (*Install, error), logf func(string, ...any)) (*Install, error) {
	ids, err := c.IDs()
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	if !ids.Privileged() {
		if ids.RGID == 0 || ids.EGID == 0 || ids.SGID == 0 {
			return nil, fmt.Errorf("running as %s: refusing to serve with group root", ids)
		}
		if len(pkgs) == 0 {
			return nil, nil
		}
		logf("not root (%s); cannot install %v", ids, pkgs)
		return FailedInstall(pkgs, reasonNotRoot), nil
	}
	var inst *Install
	if len(pkgs) > 0 {
		inst, err = start(pkgs)
		if err != nil {
			logf("start installer: %v", err)
			inst = FailedInstall(pkgs, "the installer could not start: "+err.Error())
		}
	}
	if err := DropPrivileges(c, uid, gid); err != nil {
		return nil, err
	}
	return inst, nil
}

// installResultGrace is how long past InstallTimeout the daemon waits
// for the install child's result. The child stops apt-get at
// InstallTimeout and reports; a child that has not reported by then is
// stuck, and the daemon, which cannot stop a root process, stops
// waiting for it instead.
const installResultGrace = 2 * time.Minute

const reasonInstallTooLong = "the installation did not finish in time"

// StartInstallChild runs `exe install-software <pkgs...>` with env, in
// /, and returns the running Install, finished when the child has
// reported (ReadInstallResult) or, at the latest, InstallTimeout plus a
// grace later on clk, with every package failed. exe is the
// agent-shell binary itself; the child has the caller's credentials,
// so started before the daemon drops privileges it is root. Its stdout
// and stderr (apt-get's output) go to log; its result comes back on a
// pipe passed as InstallResultFD. The packages are arguments, never
// shell text.
func StartInstallChild(exe string, pkgs []string, env []string, log io.Writer, clk clock.Clock) (*Install, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, append([]string{InstallSubcommand}, pkgs...)...)
	cmd.Env = env
	cmd.Dir = "/"
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.ExtraFiles = []*os.File{w} // InstallResultFD in the child
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	_ = w.Close() // the child holds the write end now
	inst := NewInstall(pkgs)
	awaitInstallResult(inst, pkgs, r, cmd.Wait, clk, InstallTimeout+installResultGrace)
	return inst, nil
}

// awaitInstallResult finishes inst with the result read from r, or
// with every package failed once bound has passed on clk without one.
// It returns at once. r is closed either way; wait reaps the child,
// after its result or, when it is stuck, whenever it ends.
func awaitInstallResult(inst *Install, pkgs []string, r io.ReadCloser, wait func() error, clk clock.Clock, bound time.Duration) {
	got := make(chan InstallResult, 1)
	go func() { got <- ReadInstallResult(r, pkgs) }()
	t := clk.NewTimer(bound)
	go func() {
		defer t.Stop()
		select {
		case res := <-got:
			_ = r.Close()
			_ = wait()
			inst.Finish(res)
		case <-t.C():
			inst.Finish(failAll(pkgs, reasonInstallTooLong))
			_ = r.Close()
			_ = wait()
		}
	}()
}

// OpenInstallResult is the install subcommand's end of the result
// pipe, InstallResultFD, made close-on-exec first so that apt-get and
// everything it starts (dpkg, maintainer scripts) do not inherit it:
// only the child's own report reaches the daemon.
func OpenInstallResult() (*os.File, error) {
	if err := closeOnExec(InstallResultFD); err != nil {
		return nil, fmt.Errorf("result pipe: %w", err)
	}
	f := os.NewFile(InstallResultFD, "install-result")
	if f == nil {
		return nil, errors.New("no result pipe")
	}
	return f, nil
}

// InstallEnv is the install child's environment: PATH, the proxy
// variables from env (so apt-get reaches the package servers the way
// everything else in the pod does), a C.UTF-8 locale and HOME=/root, so
// nothing run as root writes into the agent's home on its disk.
func InstallEnv(getenv func(string) string) []string {
	path := getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{"PATH=" + path, "HOME=/root", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// RunInstallSubcommand is `agent-shell install-software <pkgs...>`:
// install pkgs with in, bounded by InstallTimeout, and write the
// InstallResult as JSON to result.
//
// The child is the subreaper of everything it starts, and kills what
// is left before it reports: a service a package's maintainer script
// started, say, would otherwise keep running as root beside the
// agent's shell.
func RunInstallSubcommand(in Installer, pkgs []string, result io.Writer) error {
	if err := BecomeSubreaper(); err != nil && in.Log != nil {
		_, _ = fmt.Fprintf(in.Log, "agent-shell install-software: subreaper: %v\n", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), InstallTimeout)
	defer cancel()
	res := in.Install(ctx, pkgs)
	KillDescendants()
	return json.NewEncoder(result).Encode(res)
}
