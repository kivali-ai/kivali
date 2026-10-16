package browserpod

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// browserProcess is a running Chromium with its CDP connection.
type browserProcess interface {
	Conn() cdpConn
	// Done is closed when the process has exited.
	Done() <-chan struct{}
	// Detail says why the process ended, or what it last printed.
	Detail() string
	// SandboxOff reports whether Chromium runs with --no-sandbox.
	SandboxOff() bool
	// Close asks Chromium to exit (Browser.close), waits for it, and
	// kills it after closeGrace.
	Close(ctx context.Context) error
	// Kill ends the process at once.
	Kill()
}

// chromeOptions configures one launch.
type chromeOptions struct {
	Binary  string
	Dir     string
	Headful bool
	Logger  *log.Logger
	Clock   clock.Clock
}

const (
	// closeGrace is how long Browser.close may take before the
	// process is killed.
	closeGrace = 10 * time.Second
	// sandboxWindow is how soon an exit must follow the launch to be
	// read as a sandbox failure worth a relaunch.
	sandboxWindow = 3 * time.Second
	// readyTimeout bounds the wait for Chromium's first CDP answer.
	readyTimeout = 30 * time.Second
	// debianChromium is the real binary behind Debian's chromium
	// wrapper script, which adds --disable-dev-shm-usage when
	// /dev/shm is small; the pod's /dev/shm is sized for Chromium, so
	// the pod starts the binary itself.
	debianChromium = "/usr/lib/chromium/chromium"
)

// chromiumBinary is the binary to execute for the configured one.
func chromiumBinary(configured string) string {
	if configured == "" {
		configured = "chromium"
	}
	if configured == "chromium" || configured == "/usr/bin/chromium" {
		if fi, err := os.Stat(debianChromium); err == nil && !fi.IsDir() {
			return debianChromium
		}
	}
	return configured
}

// chromeArgs are Chromium's flags for a launch.
func chromeArgs(o chromeOptions, noSandbox bool) []string {
	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + filepath.Join(o.Dir, "profile"),
		"--password-store=basic",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-sync",
		"--disable-component-update",
		"--disable-features=Translate,MediaRouter",
		"--window-size=1280,800",
		"--lang=en-US",
	}
	if runtime.GOOS == "darwin" {
		// Keeps Chrome on a Mac from asking for the login keychain.
		args = append(args, "--use-mock-keychain")
	}
	if !o.Headful {
		args = append(args, "--headless=new")
	}
	if noSandbox {
		args = append(args, "--no-sandbox")
	}
	return append(args, "about:blank")
}

// isSandboxFailure reports whether Chromium's output says its sandbox
// could not start.
func isSandboxFailure(stderr string) bool {
	for _, s := range []string{"No usable sandbox", "setuid sandbox", "Operation not permitted"} {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

// launchChrome starts Chromium and waits until it answers
// Browser.getVersion. A launch that dies within sandboxWindow
// complaining about its sandbox is retried once with --no-sandbox.
func launchChrome(ctx context.Context, o chromeOptions) (browserProcess, error) {
	if err := os.MkdirAll(filepath.Join(o.Dir, "profile"), 0o700); err != nil {
		return nil, err
	}
	p, err := startChrome(ctx, o, false)
	if err == nil {
		return p, nil
	}
	var early *earlyExit
	if errors.As(err, &early) && isSandboxFailure(early.stderr) {
		o.Logger.Printf("chromium: the sandbox is unavailable here (%s); relaunching with --no-sandbox, so pages run without Chromium's sandbox", firstLine(early.stderr, "No usable sandbox"))
		return startChrome(ctx, o, true)
	}
	return nil, err
}

// earlyExit is a launch that ended within sandboxWindow.
type earlyExit struct {
	stderr string
	err    error
}

func (e *earlyExit) Error() string {
	return fmt.Sprintf("chromium exited at start: %v: %s", e.err, strings.TrimSpace(e.stderr))
}

func firstLine(s, containing string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, containing) {
			return strings.TrimSpace(l)
		}
	}
	return "sandbox error"
}

type chromeProc struct {
	cmd       *exec.Cmd
	cdp       *pipeCDP
	stderr    *ringBuffer
	done      chan struct{}
	waitErr   error
	noSandbox bool
	clock     clock.Clock
	killOnce  sync.Once
}

func startChrome(ctx context.Context, o chromeOptions, noSandbox bool) (*chromeProc, error) {
	bin := chromiumBinary(o.Binary)
	args := chromeArgs(o, noSandbox)
	name := bin
	if o.Headful {
		if xvfb, err := exec.LookPath("xvfb-run"); err == nil {
			name = xvfb
			args = append([]string{"-a", "-s", "-screen 0 1280x800x24", bin}, args...)
		}
	}
	// Chromium reads CDP from fd 3 and writes it to fd 4.
	toChromeR, toChromeW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromChromeR, fromChromeW, err := os.Pipe()
	if err != nil {
		_ = toChromeR.Close()
		_ = toChromeW.Close()
		return nil, err
	}
	cmd := exec.Command(name, args...)
	cmd.ExtraFiles = []*os.File{toChromeR, fromChromeW}
	ownProcessGroup(cmd)
	ring := newRingBuffer(16 << 10)
	cmd.Stderr = ring
	cmd.Stdout = ring
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{toChromeR, toChromeW, fromChromeR, fromChromeW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	_ = toChromeR.Close()
	_ = fromChromeW.Close()
	o.Logger.Printf("chromium: started %s (pid %d)", bin, cmd.Process.Pid)

	p := &chromeProc{
		cmd:       cmd,
		cdp:       newPipeCDP(fromChromeR, toChromeW),
		stderr:    ring,
		done:      make(chan struct{}),
		noSandbox: noSandbox,
		clock:     o.Clock,
	}
	started := o.Clock.Now()
	go func() {
		p.waitErr = cmd.Wait()
		_ = fromChromeR.Close()
		close(p.done)
	}()

	readyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := o.Clock.NewTimer(readyTimeout)
	defer timer.Stop()
	ready := make(chan error, 1)
	go func() {
		_, err := p.cdp.Call(readyCtx, "", "Browser.getVersion", nil)
		ready <- err
	}()
	select {
	case err := <-ready:
		if err == nil {
			return p, nil
		}
		// The pipe closing usually means the process is exiting.
		select {
		case <-p.done:
		case <-timer.C():
			p.Kill()
			<-p.done
		}
	case <-p.done:
	case <-timer.C():
		p.Kill()
		<-p.done
		return nil, fmt.Errorf("chromium did not answer within %s: %s", readyTimeout, p.Detail())
	case <-ctx.Done():
		p.Kill()
		<-p.done
		return nil, ctx.Err()
	}
	if clock.Since(o.Clock, started) <= sandboxWindow {
		return nil, &earlyExit{stderr: ring.String(), err: p.waitErr}
	}
	return nil, fmt.Errorf("chromium exited: %s", p.Detail())
}

func (p *chromeProc) Conn() cdpConn         { return p.cdp }
func (p *chromeProc) Done() <-chan struct{} { return p.done }
func (p *chromeProc) SandboxOff() bool      { return p.noSandbox }

func (p *chromeProc) Detail() string {
	tail := strings.TrimSpace(p.stderr.String())
	if len(tail) > 600 {
		tail = tail[len(tail)-600:]
		if j := strings.IndexByte(tail, '\n'); j >= 0 {
			tail = tail[j+1:]
		}
	}
	select {
	case <-p.done:
		if p.waitErr != nil {
			return fmt.Sprintf("%v; %s", p.waitErr, tail)
		}
		return "exited; " + tail
	default:
		return tail
	}
}

func (p *chromeProc) Close(ctx context.Context) error {
	callCtx, cancel := context.WithCancel(ctx)
	go func() {
		_, _ = p.cdp.Call(callCtx, "", "Browser.close", nil)
	}()
	defer cancel()
	timer := p.clock.NewTimer(closeGrace)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C():
	case <-ctx.Done():
	}
	p.Kill()
	<-p.done
	return errors.New("chromium did not exit in time and was killed")
}

func (p *chromeProc) Kill() {
	select {
	case <-p.done:
		return
	default:
	}
	p.killOnce.Do(func() {
		killProcessGroup(p.cmd)
	})
}

// ringBuffer keeps the last n bytes written to it.
type ringBuffer struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

func newRingBuffer(n int) *ringBuffer { return &ringBuffer{n: n} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.n {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.n:]...)
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
