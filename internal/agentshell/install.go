package agentshell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/debpkg"
)

// InstallSubcommand is the agent-shell subcommand that installs
// software: `agent-shell install-software <package>...`. The daemon runs
// it as a root child of itself (before it drops to its own unprivileged
// uid) and reads the InstallResult it writes to InstallResultFD.
const InstallSubcommand = "install-software"

// InstallResultFD is the file descriptor the install subcommand writes
// its InstallResult to, as JSON: the write end of a pipe the daemon
// passes as the child's first extra file. apt-get's own output goes to
// the child's stdout and stderr, which the daemon sends to its log.
const InstallResultFD = 3

// InstallResult is what an install did: the packages now installed and
// the ones that are not, each with a short reason.
type InstallResult struct {
	Installed []string         `json:"installed"`
	Failed    []PackageFailure `json:"failed"`
}

// PackageFailure is a package that could not be installed. Error is a
// short reason: the tail of apt-get's error output, capped at
// failureReasonMax bytes, or a plain sentence when apt-get never ran.
type PackageFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// ValidPackageName reports whether name is a Debian package name
// (debpkg.ValidName), the rule store.ValidSoftwareName applies when a
// list is saved and integration.Validate when a manifest declares
// packages. Checked again here because a name reaches apt-get as an
// argument: one that starts with '-' would be an option, and apt-get
// reads a trailing '-' as "remove".
func ValidPackageName(name string) bool { return debpkg.ValidName(name) }

// ParseInstallList splits the daemon's --install value (comma-separated
// package names) into a sorted, de-duplicated list. Blanks are dropped.
// Invalid names are kept: Installer.Install reports each as failed, so
// the agent learns of it rather than the name vanishing.
func ParseInstallList(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// failureReasonMax caps a PackageFailure's Error.
const failureReasonMax = 300

// stderrTailMax is how much of one apt-get run's stderr is kept to
// build a failure reason from.
const stderrTailMax = 16 << 10

const reasonInvalidName = "not a Debian package name"

const reasonNoSuchPackage = "no package has this name"

// Installer installs Debian packages with apt-get. Packages are passed
// to apt-get as arguments, never through a shell.
type Installer struct {
	// AptGet is the apt-get binary: a path or a name looked up on PATH.
	// Empty means "apt-get". Tests point it at a script.
	AptGet string
	// AptCache is the apt-cache binary, used to check that each name
	// is exactly a name in the package lists. Empty means "apt-cache".
	AptCache string
	// Env is apt-get's environment. DEBIAN_FRONTEND=noninteractive is
	// added to it.
	Env []string
	// Log receives apt-get's stdout and stderr as they are written.
	// Nil discards them (the stderr tail is kept either way).
	Log io.Writer
	// OutputGrace is how long a run waits, once apt-get has exited,
	// for its output to close: a process a maintainer script left
	// running may hold it open. Zero means 10s.
	OutputGrace time.Duration
	// ProxyWait is how long Install waits for the proxy in Env to
	// accept a connection before it runs apt-get update. Zero means
	// 2 minutes.
	ProxyWait time.Duration
	// Dial opens the connections of that wait. Nil means a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Clock paces that wait. Nil means the system clock.
	Clock clock.Clock
}

// Install runs `apt-get update`, keeps the names that are exactly a
// name in the package lists (lookup), then installs them in one
// `apt-get install`. If that fails it installs each package on its
// own, to learn which failed and why. Every requested name ends up in
// exactly one of Installed and Failed.
//
// The lookup is what makes each name mean only itself: apt-get reads
// an argument that matches no package name as a pattern, so "lib.."
// would install whatever it matches.
//
// The update waits first for the proxy in Env to accept a connection
// (awaitProxy): the install runs when the pod starts, which at the
// server's boot can be before the proxy is reachable.
//
// An update that fails does not stop the install: the lists already
// on disk may have the package. Its reason goes with every failure,
// as does the reason apt-get update gives, while still succeeding,
// for a package server it could not reach. A proxy wait that ran out
// goes before either.
func (in Installer) Install(ctx context.Context, pkgs []string) InstallResult {
	res := InstallResult{Installed: []string{}, Failed: []PackageFailure{}}
	var valid []string
	for _, p := range pkgs {
		if ValidPackageName(p) {
			valid = append(valid, p)
		} else {
			res.Failed = append(res.Failed, PackageFailure{Name: p, Error: reasonInvalidName})
		}
	}
	if len(valid) == 0 {
		return res
	}
	proxyFailure := in.awaitProxy(ctx)
	updateStderr, err := in.run(ctx, "update")
	updateFailure := updateFetchFailure(updateStderr)
	if err != nil {
		updateFailure = failureReason(updateStderr, err)
	}
	if updateFailure != "" && proxyFailure != "" {
		updateFailure = proxyFailure + "; " + updateFailure
	}
	fail := func(name, reason string) {
		if updateFailure != "" {
			reason += "; the package lists could not be updated: " + updateFailure
		}
		res.Failed = append(res.Failed, PackageFailure{Name: name, Error: capReason(reason)})
	}
	var known []string
	for _, p := range valid {
		switch ok, err := in.lookup(ctx, p); {
		case err != nil:
			fail(p, "could not look the package up: "+err.Error())
		case !ok:
			fail(p, reasonNoSuchPackage)
		default:
			known = append(known, p)
		}
	}
	if len(known) == 0 {
		return res
	}
	installArgs := []string{"install", "-y", "-q", "--no-install-recommends"}
	if _, err := in.run(ctx, append(installArgs, known...)...); err == nil {
		res.Installed = append(res.Installed, known...)
		return res
	}
	for _, p := range known {
		if stderr, err := in.run(ctx, append(append([]string{}, installArgs...), p)...); err != nil {
			fail(p, failureReason(stderr, err))
		} else {
			res.Installed = append(res.Installed, p)
		}
	}
	return res
}

// proxyWaitDefault is Installer.ProxyWait when zero.
const proxyWaitDefault = 2 * time.Minute

// proxyDialTimeout bounds one attempt of the proxy wait.
const proxyDialTimeout = 5 * time.Second

// proxyRetryInterval is the pause between attempts of the proxy wait.
const proxyRetryInterval = time.Second

// awaitProxy waits until the proxy in Env accepts a TCP connection,
// trying every proxyRetryInterval for ProxyWait, and returns "" when
// it did, or when Env names no proxy. Otherwise it returns a reason,
// also written to Log. The wait has its own bound so a proxy that
// never answers still leaves the update and the install their time.
func (in Installer) awaitProxy(ctx context.Context) string {
	addr := proxyHostPort(in.Env)
	if addr == "" {
		return ""
	}
	clk := in.Clock
	if clk == nil {
		clk = clock.New()
	}
	dial := in.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	bound := in.ProxyWait
	if bound <= 0 {
		bound = proxyWaitDefault
	}
	start := clk.Now()
	for {
		dctx, cancel := context.WithTimeout(ctx, proxyDialTimeout)
		conn, err := dial(dctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = conn.Close()
			return ""
		}
		reason := fmt.Sprintf("the egress proxy at %s accepted no connection in %s: %v", addr, bound, err)
		if ctx.Err() == nil && clock.Since(clk, start) < bound {
			t := clk.NewTimer(proxyRetryInterval)
			select {
			case <-t.C():
				continue
			case <-ctx.Done():
				t.Stop()
			}
		}
		if in.Log != nil {
			_, _ = fmt.Fprintf(in.Log, "agent-shell install-software: %s\n", reason)
		}
		return reason
	}
}

// proxyHostPort is the "host:port" of the proxy the environment names:
// the first of https_proxy, HTTPS_PROXY, http_proxy and HTTP_PROXY
// that is set to a URL with a host, or "" when none is. A URL with no
// port gets its scheme's.
func proxyHostPort(env []string) string {
	for _, k := range []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY"} {
		for _, kv := range env {
			v, ok := strings.CutPrefix(kv, k+"=")
			if !ok || v == "" {
				continue
			}
			u, err := url.Parse(v)
			if err != nil || u.Host == "" {
				continue
			}
			if u.Port() != "" {
				return u.Host
			}
			port := "80"
			if u.Scheme == "https" {
				port = "443"
			}
			return net.JoinHostPort(u.Hostname(), port)
		}
	}
	return ""
}

// lookup reports whether name is exactly a name in the package lists.
// `apt-cache pkgnames --all-names <name>` prints every name that
// starts with name, one per line, without reading it as a pattern;
// --all-names includes virtual packages, which apt-get installs by
// name when one package provides them.
func (in Installer) lookup(ctx context.Context, name string) (bool, error) {
	bin := in.AptCache
	if bin == "" {
		bin = "apt-cache"
	}
	cmd := exec.CommandContext(ctx, bin, "pkgnames", "--all-names", name)
	cmd.Env = append([]string{}, in.Env...)
	cmd.Dir = "/"
	var out bytes.Buffer
	tail := &tailBuffer{max: stderrTailMax}
	cmd.Stdout = &limitWriter{w: &out, n: lookupOutputMax}
	cmd.Stderr = tail
	cmd.WaitDelay = in.outputGrace()
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("timed out: %w", ctx.Err())
		}
		return false, errors.New(failureReason(tail.String(), err))
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(l) == name {
			return true, nil
		}
	}
	return false, nil
}

// lookupOutputMax caps what one lookup reads: the names starting with
// a valid package name.
const lookupOutputMax = 4 << 20

// limitWriter passes on the first n bytes written to it and drops the
// rest, so a command writing more is not stopped by an error.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		if _, err := l.w.Write(p[:k]); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (in Installer) outputGrace() time.Duration {
	if in.OutputGrace <= 0 {
		return 10 * time.Second
	}
	return in.OutputGrace
}

// updateFetchFailure is the reason in apt-get update's first "Failed
// to fetch" warning ("W: Failed to fetch <url>  <reason>"), or "".
func updateFetchFailure(stderr string) string {
	for _, l := range strings.Split(stderr, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(l), "W: Failed to fetch ")
		if !ok {
			continue
		}
		if _, reason, ok := strings.Cut(rest, "  "); ok && strings.TrimSpace(reason) != "" {
			return strings.TrimSpace(reason)
		}
		return rest
	}
	return ""
}

// run runs apt-get with args and returns its stderr's tail.
func (in Installer) run(ctx context.Context, args ...string) (string, error) {
	bin := in.AptGet
	if bin == "" {
		bin = "apt-get"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(append([]string{}, in.Env...), "DEBIAN_FRONTEND=noninteractive")
	cmd.Dir = "/"
	log := in.Log
	if log == nil {
		log = io.Discard
	}
	tail := &tailBuffer{max: stderrTailMax}
	cmd.Stdout = log
	cmd.Stderr = io.MultiWriter(log, tail)
	cmd.WaitDelay = in.outputGrace()
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		// apt-get succeeded; something it started still held the
		// output open.
		err = nil
	}
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("timed out: %w", ctx.Err())
	}
	return tail.String(), err
}

// failureReason makes a short reason from apt-get's stderr: its "E:"
// lines when it printed any, else its last lines, else the error.
func failureReason(stderr string, err error) string {
	var errLines, all []string
	for _, l := range strings.Split(stderr, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		all = append(all, l)
		if strings.HasPrefix(l, "E: ") {
			errLines = append(errLines, strings.TrimPrefix(l, "E: "))
		}
	}
	lines := errLines
	if len(lines) == 0 && len(all) > 0 {
		lines = all[max(0, len(all)-2):]
	}
	if len(lines) == 0 {
		if err == nil {
			return "apt-get failed"
		}
		return err.Error()
	}
	return capReason(strings.Join(lines, "; "))
}

// capReason cuts s to failureReasonMax bytes on a rune boundary.
func capReason(s string) string {
	if len(s) <= failureReasonMax {
		return s
	}
	cut := failureReasonMax - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte{}, t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// installResultMax caps how much of the install child's result the
// daemon reads.
const installResultMax = 1 << 20

// ReadInstallResult reads the install child's result from r and
// accounts for every requested package: a name the child did not
// report, or a result that could not be read, is a failure with a
// plain reason. Names the child reports that were not requested are
// ignored.
func ReadInstallResult(r io.Reader, requested []string) InstallResult {
	var got InstallResult
	err := json.NewDecoder(io.LimitReader(r, installResultMax)).Decode(&got)
	if err != nil {
		return failAll(requested, "the installer stopped without reporting a result")
	}
	want := map[string]bool{}
	for _, p := range requested {
		want[p] = true
	}
	res := InstallResult{Installed: []string{}, Failed: []PackageFailure{}}
	seen := map[string]bool{}
	for _, p := range got.Installed {
		if want[p] && !seen[p] {
			seen[p] = true
			res.Installed = append(res.Installed, p)
		}
	}
	for _, f := range got.Failed {
		if want[f.Name] && !seen[f.Name] {
			seen[f.Name] = true
			res.Failed = append(res.Failed, PackageFailure{Name: f.Name, Error: capReason(f.Error)})
		}
	}
	for _, p := range requested {
		if !seen[p] {
			res.Failed = append(res.Failed, PackageFailure{Name: p, Error: "the installer did not report on it"})
		}
	}
	return res
}

// failAll is an InstallResult with every package failed for reason.
func failAll(pkgs []string, reason string) InstallResult {
	res := InstallResult{Installed: []string{}, Failed: []PackageFailure{}}
	for _, p := range pkgs {
		res.Failed = append(res.Failed, PackageFailure{Name: p, Error: reason})
	}
	return res
}
