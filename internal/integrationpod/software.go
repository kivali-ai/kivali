package integrationpod

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integration"
)

// Software is what the shim does as root before anything of the bundle
// runs: install the manifest's packages (kivali.packages), then leave
// root for good. RootSoftware is the pod's; tests fake it.
type Software interface {
	// Install installs pkgs, apt-get's output going to log, and reports
	// each package installed or failed. It returns once the install has
	// finished or been given up on.
	Install(ctx context.Context, pkgs []string, log io.Writer) agentshell.InstallResult
	// LeaveRoot makes the process the images' user for good, with no
	// capability left, and checks that it is.
	LeaveRoot() error
}

// InstallArgs are the arguments that make the shim's binary the install
// child: `kivali integration-shim install-software <pkgs...>`.
var InstallArgs = []string{"integration-shim", agentshell.InstallSubcommand}

// RootSoftware installs with the agent shell's installer, in a root
// child: the shim's own binary run again with InstallArgs, which runs
// apt-get with the packages as arguments, never through a shell, kills
// what a package's scripts left running, and reports on a pipe. The
// child stops apt-get after agentshell.InstallTimeout, and the shim
// gives up on a child that has not reported shortly after, on Clock.
type RootSoftware struct {
	// Exe is the shim's binary, /proc/self/exe.
	Exe string
	// Env is the child's environment (agentshell.InstallEnv): PATH,
	// HOME=/root, a C.UTF-8 locale and the pod's bare proxy variables.
	// apt-get reaches deb.debian.org through the egress proxy with no
	// credential, held to the team's allow list, which always holds it.
	Env   []string
	Clock clock.Clock
	// Creds changes the process's credentials: agentshell.HostCreds.
	Creds agentshell.Creds
}

// Install runs the install child and waits for its result.
func (r RootSoftware) Install(ctx context.Context, pkgs []string, log io.Writer) agentshell.InstallResult {
	inst, err := agentshell.StartInstallCommand(append([]string{r.Exe}, InstallArgs...), pkgs, r.Env, log, r.Clock)
	if err != nil {
		inst = agentshell.FailedInstall(pkgs, "the installer could not start: "+err.Error())
	}
	select {
	case <-inst.Done():
	case <-ctx.Done():
		inst = agentshell.FailedInstall(pkgs, "the integration stopped")
	}
	_, installed, failed := inst.State()
	return agentshell.InstallResult{Installed: installed, Failed: failed}
}

// LeaveRoot drops the process to the images' user (agentshell.LeaveRoot).
func (r RootSoftware) LeaveRoot() error { return agentshell.LeaveRoot(r.Creds, podUID, podUID) }

// prepare is the shim's start before it dials core's events stream, so
// the integration is not running while it lasts. With Software set
// (the shim started as root), the manifest's packages are installed and
// root is left for good; nothing of the bundle has run by then, and the
// server never runs as root. Each step goes to the integration's log:
// "Installing gh, jq", apt-get's output, then "Installed gh, jq" or what
// failed and why. It reports whether the shim may go on; one that may
// not exits, and kubelet starts it again, which tries the install
// again.
func (s *Shim) prepare(ctx context.Context, m *integration.Manifest) bool {
	pkgs := m.Kivali.Packages
	if s.Software == nil {
		if len(pkgs) == 0 {
			return true
		}
		s.logf("not root; cannot install %v", pkgs)
		s.notifyLog(ctx, "error", "Could not install "+strings.Join(pkgs, ", ")+": the integration does not start as root, so it cannot install software. It stops and tries again when it restarts.")
		return false
	}
	if len(pkgs) > 0 {
		names := strings.Join(pkgs, ", ")
		s.logf("installing %s", names)
		s.notifyLog(ctx, "info", "Installing "+names)
		out := &logLines{ctx: ctx, s: s, level: "debug"}
		res := s.Software.Install(ctx, pkgs, out)
		out.flush()
		if ctx.Err() != nil {
			return false
		}
		if len(res.Failed) > 0 {
			parts := make([]string, len(res.Failed))
			for i, f := range res.Failed {
				parts[i] = f.Name + " (" + f.Error + ")"
			}
			s.logf("install failed: %s", strings.Join(parts, "; "))
			s.notifyLog(ctx, "error", "Could not install "+strings.Join(parts, "; ")+". The integration stops and tries again when it restarts.")
			return false
		}
		s.logf("installed %s", names)
		s.notifyLog(ctx, "info", "Installed "+names)
	}
	if err := s.Software.LeaveRoot(); err != nil {
		s.logf("leave root: %v", err)
		s.notifyLog(ctx, "error", "Could not stop running as root, so the integration did not start: "+err.Error())
		return false
	}
	s.logf("left root")
	return true
}

// logLines sends each line written to it to core as a log line of
// level, cut to maxStderrLine bytes; flush sends a last line with no
// newline.
type logLines struct {
	ctx   context.Context
	s     *Shim
	level string

	mu       sync.Mutex
	buf      []byte
	skipping bool
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !l.skipping {
			room := maxStderrLine - len(l.buf)
			if len(chunk) > room {
				l.buf = append(l.buf, chunk[:room]...)
				l.emit()
				l.skipping = true
			} else {
				l.buf = append(l.buf, chunk...)
			}
		}
		if i < 0 {
			break
		}
		if !l.skipping {
			l.emit()
		}
		l.skipping = false
		p = p[i+1:]
	}
	return n, nil
}

// emit sends the buffered line, if it has any text.
func (l *logLines) emit() {
	line := strings.TrimRight(string(l.buf), "\r")
	l.buf = l.buf[:0]
	if strings.TrimSpace(line) != "" {
		l.s.notifyLog(l.ctx, l.level, line)
	}
}

func (l *logLines) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.skipping {
		l.emit()
	}
	l.buf, l.skipping = nil, false
}
