package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

func getPath(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%v: not a map at %q", keys, k)
		}
		cur = mm[k]
	}
	return cur
}

var owner = InstallOptions{Owner: "someone@example.com"}

func TestFirstUpFormatsAndInstalls(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})

	if !h.vm.lastFormat() {
		t.Fatal("the first boot did not ask the guest to format the data disk")
	}
	st := h.sup.State()
	if !st.DataDiskFormatted || st.Kivali != "0.15.0" || st.VMImage != "0.15.0" || st.Port != h.port {
		t.Fatalf("state %+v", st)
	}
	if st.MemoryMB != 4096 || st.CPUs != 4 {
		t.Fatalf("defaults: %d MiB, %d CPUs", st.MemoryMB, st.CPUs)
	}
	// Windows has no permission bits to check: its mode is the read-only
	// flag alone, and Perm reports 0o666 for a file that can be written.
	if fi, err := os.Stat(filepath.Join(h.dir, "local.json")); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("local.json: %v %v", fi, err)
	}
	if c := h.vm.boots[0]; c.ImageDir != "/vm" || c.DataDisk != h.dataPath() || c.CPUs != 4 || c.MemoryMB != 4096 || c.ConsoleLog == nil {
		t.Fatalf("machine config %+v", c)
	}
	if len(h.vm.created) != 1 || h.vm.created[0] != DataDiskSize {
		t.Fatalf("data disks created %v", h.vm.created)
	}

	file, v := h.manifestValues()
	if file != "kivali-0.15.0.tgz" {
		t.Fatalf("chart file %q", file)
	}
	if got := h.disk()[StaticChartsRel+"/kivali-0.15.0.tgz"]; !strings.Contains(got, "kivali-0.15.0.tgz") {
		t.Fatalf("baked chart not placed: %q", got)
	}
	checks := map[string]any{
		"kivaliEnv":             "dev",
		"devMode":               false,
		"initImage":             "docker.io/rancher/mirrored-library-busybox:1.37.0",
		"image.repository":      "kivali",
		"image.tag":             "v0.15.0",
		"egress.image.tag":      "v0.15.0",
		"secrets.create":        true,
		"secrets.ownerEmails":   "someone@example.com",
		"networkPolicy.enabled": true,
		"networkPolicy.podCIDR": "10.42.0.0/16",
	}
	for k, want := range checks {
		if got := getPath(t, v, strings.Split(k, ".")...); got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	if key, _ := getPath(t, v, "secrets", "sessionKey").(string); len(key) != 64 {
		t.Errorf("session key %q", key)
	}
	if _, ok := v["apiKey"]; ok {
		t.Error("apiKey values set; the org signs in with the CLI's own sign-in only")
	}
}

func TestUpNeedsOwnerOnFirstInstall(t *testing.T) {
	h := newHarness(t)
	err := h.up(UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "--owner") {
		t.Fatalf("up without owner: %v", err)
	}
	if _, ok := h.disk()[ManifestRel]; ok {
		t.Fatal("manifest written without an owner")
	}
}

func TestSecondUpDoesNotFormat(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	_, before := h.manifestValues()
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.mustUp(UpOptions{})
	if h.vm.lastFormat() {
		t.Fatal("the second boot asked for a format")
	}
	_, after := h.manifestValues()
	if getPath(t, before, "secrets", "sessionKey") != getPath(t, after, "secrets", "sessionKey") {
		t.Fatal("second up rewrote the install")
	}
}

// upFailsOnTheRecordedPort runs up on an org whose recorded port cannot
// be bound and checks that up fails with a PortError of that kind, the
// port stays recorded, nothing else was bound, and status carries the
// message for the shell.
func (h *harness) upFailsOnTheRecordedPort(forbidden bool, says string) {
	h.t.Helper()
	err := h.sup.Up(context.Background(), UpOptions{}, h.logf())
	var perr *PortError
	if !errors.As(err, &perr) || perr.Port != h.port || perr.Forbidden != forbidden {
		h.t.Fatalf("up: %v, want a PortError for %d (forbidden %v)", err, h.port, forbidden)
	}
	for _, want := range []string{says, "up --port <port>", h.dir, "Tailscale Serve"} {
		if !strings.Contains(err.Error(), want) {
			h.t.Errorf("message %q lacks %q", err, want)
		}
	}
	if got := h.sup.State().Port; got != h.port {
		h.t.Fatalf("port %d recorded, want %d kept", got, h.port)
	}
	r := h.sup.Status(context.Background())
	if !r.Running || r.Forward != "" || r.ForwardError != err.Error() {
		h.t.Fatalf("status running %v, forward %q, forward_error %q", r.Running, r.Forward, r.ForwardError)
	}
	h.restart()
	if got := h.sup.State().Port; got != h.port {
		h.t.Fatalf("port %d on disk, want %d kept", got, h.port)
	}
}

func TestUpFailsWhenTheRecordedPortIsBusy(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	// Another program now holds the recorded port.
	squatter, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(h.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squatter.Close() }()
	h.restart()
	h.upFailsOnTheRecordedPort(false, "Another program is using port "+strconv.Itoa(h.port))
}

// forbiddenBind is the error of a bind to a port the system will not
// give this program: on Windows a port in an excluded range (WSAEACCES),
// elsewhere a privileged port (EACCES).
func forbiddenBind(port int) error {
	errno := syscall.EACCES
	if runtime.GOOS == "windows" {
		errno = syscall.Errno(10013) // WSAEACCES
	}
	return &net.OpError{Op: "listen", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, Err: os.NewSyscallError("bind", errno)}
}

func TestUpFailsWhenTheRecordedPortIsForbidden(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	// After a reboot, Hyper-V has reserved a range holding the recorded
	// port.
	h.bindErr = func(port int) error {
		if port == h.port {
			return forbiddenBind(port)
		}
		return nil
	}
	h.restart()
	says := "won't let Kivali use port "
	if runtime.GOOS == "windows" {
		says = "Windows has reserved port "
	}
	h.upFailsOnTheRecordedPort(true, says+strconv.Itoa(h.port))
}

func TestFirstUpTakesAFreePortBelowTheDynamicRange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// The hint is busy and the first fallback port is reserved.
	squatter, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(h.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squatter.Close() }()
	h.bindErr = func(port int) error {
		if port == FirstFallbackPort {
			return forbiddenBind(port)
		}
		return nil
	}
	h.restart()
	if err := h.sup.Up(ctx, UpOptions{Install: owner, PortHint: h.port}, h.logf()); err != nil {
		t.Fatalf("up: %v", err)
	}
	got := h.sup.State().Port
	if got <= FirstFallbackPort || got >= DynamicPortsStart {
		t.Fatalf("port %d recorded, want one in (%d, %d)", got, FirstFallbackPort, DynamicPortsStart)
	}
	if r := h.sup.Status(ctx); r.Forward != net.JoinHostPort("127.0.0.1", strconv.Itoa(got)) || r.ForwardError != "" {
		t.Fatalf("forward %q, forward_error %q", r.Forward, r.ForwardError)
	}
	// The next up uses the chosen port, even with the hint free again.
	if err := h.sup.Down(ctx, h.logf()); err != nil {
		t.Fatal(err)
	}
	_ = squatter.Close()
	h.restart()
	if err := h.sup.Up(ctx, UpOptions{PortHint: h.port}, h.logf()); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if r := h.sup.Status(ctx); h.sup.State().Port != got || r.Forward != net.JoinHostPort("127.0.0.1", strconv.Itoa(got)) {
		t.Fatalf("second up on %q, recorded %d, want %d", r.Forward, h.sup.State().Port, got)
	}
}

func TestUpPortMovesTheOrg(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.mustUp(UpOptions{Install: owner})
	other := freePort(t)
	if err := h.sup.Up(ctx, UpOptions{Port: other}, h.logf()); err != nil {
		t.Fatalf("up --port: %v", err)
	}
	if got := h.sup.State().Port; got != other {
		t.Fatalf("port %d recorded, want %d", got, other)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(other)))
	if err != nil {
		t.Fatalf("forward not listening on the new port: %v", err)
	}
	_ = c.Close()
	h.restart()
	if got := h.sup.State().Port; got != other {
		t.Fatalf("port %d on disk, want %d", got, other)
	}
}

func TestFailedFirstBootRecreatesDataDisk(t *testing.T) {
	h := newHarness(t)
	h.vm.fatalOnBoot = "ready: waited 300s for the node to be Ready; giving up"
	err := h.up(UpOptions{Install: owner})
	if err == nil || !strings.Contains(err.Error(), "KIVALI-VM FATAL: ready: waited 300s") {
		t.Fatalf("up: %v", err)
	}
	if h.sup.State().DataDiskFormatted {
		t.Fatal("data disk marked formatted after a failed first boot")
	}
	// The failed boot left a partly written disk: it must not be
	// booted again, and the format flag must not be retried on it.
	if err := os.WriteFile(h.dataPath(), []byte("partial mkfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.mustUp(UpOptions{Install: owner})
	if !h.vm.lastFormat() || !h.sup.State().DataDiskFormatted {
		t.Fatalf("retry: format %v, state %+v", h.vm.lastFormat(), h.sup.State())
	}
}

func TestMissingStateKeepsExistingDisk(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.dir, "local.json")); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.mustUp(UpOptions{})
	if h.vm.lastFormat() {
		t.Fatal("a data disk with no local.json was reformatted")
	}
	if h.sup.State().Kivali != "0.15.0" {
		t.Fatalf("version not recovered from the manifest: %+v", h.sup.State())
	}
}

func TestDownFallsBackToPoweroffRequest(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.agentDown = true
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	if h.vm.poweroffs != 1 {
		t.Fatalf("%d poweroff requests", h.vm.poweroffs)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("machine still current after down")
	}
	if h.vm.machines[0].running() {
		t.Fatal("VM still running")
	}
}

// consoleClosed reports whether the supervisor has closed the console log
// it gave the machine. A zero-length write meets the closed check first.
func consoleClosed(m *fakeMachine) bool {
	_, err := m.cfg.ConsoleLog.Write(nil)
	return errors.Is(err, os.ErrClosed)
}

// say prints a line on boot n's console, as the backend does for the guest.
func (h *harness) say(n int, line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.vm.machines[n].cfg.ConsoleLog, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) consoleLog(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "logs", name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func TestDownClosesTheConsoleLogBeforeItReturns(t *testing.T) {
	// With one P, the goroutine bootLocked starts to close the file cannot
	// run while down keeps going, so only a close in down itself passes.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	// No waiting: the next boot renames the file, which Windows refuses
	// while it is open.
	if !consoleClosed(h.vm.machines[0]) {
		t.Fatal("the console log is still open after down returned")
	}
}

func TestEachBootKeepsTheConsoleLogOfTheBootBefore(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.say(0, "first boot")
	// The same serve stops and boots again at once, as an upgrade does; the
	// third boot also replaces a console.log.1 that exists.
	for n, prev := range []string{"first boot", "second boot"} {
		if err := h.sup.Down(context.Background(), h.logf()); err != nil {
			t.Fatal(err)
		}
		h.mustUp(UpOptions{})
		now := []string{"second boot", "third boot"}[n]
		h.say(n+1, now)
		if got := h.consoleLog("console.log.1"); got != prev+"\n" {
			t.Fatalf("boot %d: console.log.1 is %q, want %q", n+2, got, prev+"\n")
		}
		if got := h.consoleLog("console.log"); got != now+"\n" {
			t.Fatalf("boot %d: console.log is %q, want %q", n+2, got, now+"\n")
		}
	}
}

// blockConsoleRotation puts a directory where console.log.1 goes, which
// makes the rename fail on every OS, as an open file does on Windows.
func (h *harness) blockConsoleRotation() string {
	h.t.Helper()
	p := filepath.Join(h.dir, "logs", "console.log.1")
	if err := os.Mkdir(p, 0o700); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func TestConsoleLogThatCannotBeMovedIsKeptNotTruncated(t *testing.T) {
	h := newHarness(t)
	served := &serveLog{}
	h.sup.o.Logf = served.logf
	h.mustUp(UpOptions{Install: owner})
	h.say(0, "first boot")
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	h.blockConsoleRotation()
	h.mustUp(UpOptions{})
	h.say(1, "second boot")

	log := h.consoleLog("console.log")
	first, marker, second := strings.Index(log, "first boot\n"), strings.Index(log, "supervisor: the previous console log could not be moved"), strings.Index(log, "second boot\n")
	if first != 0 || marker < first || second < marker {
		t.Fatalf("console.log is %q: want the first boot, a marker, then the second boot", log)
	}
	if !slices.ContainsFunc(served.get(), func(l string) bool { return strings.Contains(l, "could not be moved to console.log.1") }) {
		t.Fatalf("serve log %q", served.get())
	}
}

func TestConsoleLogRenameIsRetriedBriefly(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.say(0, "first boot")
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	blocker := h.blockConsoleRotation()
	// Whatever held the file lets go after two waits.
	var waits atomic.Int32
	h.sup.o.Clock = stepClock{h.clock, func() {
		if waits.Add(1) == 3 {
			_ = os.Remove(blocker)
		}
	}}
	h.mustUp(UpOptions{})
	if got := h.consoleLog("console.log.1"); got != "first boot\n" {
		t.Fatalf("console.log.1 is %q", got)
	}
	if got := h.consoleLog("console.log"); got != "" {
		t.Fatalf("console.log is %q, want a fresh one", got)
	}
}

// stepClock is the fake clock, calling onAfter at every After.
type stepClock struct {
	*fakeClock
	onAfter func()
}

func (c stepClock) After(d time.Duration) <-chan time.Time {
	c.onAfter()
	return c.fakeClock.After(d)
}

func TestInstallValuesClientAndEnv(t *testing.T) {
	im := Images{Kivali: "ghcr.io/kivali-ai/kivali:v1.0.0", Egress: "ghcr.io/kivali-ai/kivali-egress-proxy:v1.0.0", Busybox: "bb:1"}
	v, err := InstallValues(InstallOptions{Owner: "a@b.c", PublicClientID: "cid", Env: []string{"A=b=c"}}, im, "k")
	if err != nil {
		t.Fatal(err)
	}
	if getPath(t, v, "image", "repository") != "ghcr.io/kivali-ai/kivali" || getPath(t, v, "oauth", "publicClientID") != "cid" {
		t.Fatalf("values: %v", v)
	}
	env := v["extraEnv"].([]any)[0].(map[string]any)
	if env["name"] != "A" || env["value"] != "b=c" {
		t.Fatalf("env %v", env)
	}
	if _, err := InstallValues(InstallOptions{Env: []string{"novalue"}}, im, "k"); err == nil {
		t.Fatal("bad env accepted")
	}
}

// A first install whose values Helm refuses fails on the third run with
// the same helm error, three looks in, and says helm's error: without
// that the person waits ten minutes for an error naming nothing.
func TestSetOverridesAndHelmFailure(t *testing.T) {
	h := newHarness(t)
	h.vm.helmBroken["kivali-0.15.0.tgz"] = true
	o := owner
	o.Set = []string{"secrets.oauthRedirectURL=http://127.0.0.1:1/auth/callback", "networkPolicy.serverIngress.enabled=false"}
	err := h.up(UpOptions{Install: o})
	if err == nil || !strings.Contains(err.Error(), "failed to install the chart 3 times with the same error: Error: INSTALLATION FAILED: duplicate entries .env: duplicate key") {
		t.Fatalf("up: %v", err)
	}
	if h.vm.helmRuns != helmJobFailures {
		t.Fatalf("the install job's pod was looked at %d times; the third failure should end the wait", h.vm.helmRuns)
	}
	_, v := h.manifestValues()
	if getPath(t, v, "secrets", "oauthRedirectURL") != "http://127.0.0.1:1/auth/callback" || getPath(t, v, "networkPolicy", "serverIngress", "enabled") != false {
		t.Fatalf("values %v", v)
	}
	if err := applySets(map[string]any{}, []string{"a..b=1"}); err == nil {
		t.Fatal("bad path accepted")
	}
}

func TestPickImages(t *testing.T) {
	refs := []string{"docker.io/library/kivali:dev", "docker.io/library/kivali-egress-proxy:dev"}
	if _, err := pickImages(refs, false, false); err == nil || !strings.Contains(err.Error(), "kivali-agent-shell:dev") {
		t.Fatalf("missing agent-shell: %v", err)
	}
	refs = append(refs, "docker.io/library/kivali-agent-shell:dev")
	// A chart without browser pods needs no browser image.
	if _, err := pickImages(refs, false, false); err != nil {
		t.Fatalf("three images, no browser wanted: %v", err)
	}
	if _, err := pickImages(refs, true, false); err == nil || !strings.Contains(err.Error(), "kivali-browser:dev") || !strings.Contains(err.Error(), "the 4 Kivali images") {
		t.Fatalf("missing browser: %v", err)
	}
	refs = append(refs, "docker.io/library/kivali-browser:dev", "docker.io/library/kivali:v2")
	if _, err := pickImages(refs, true, false); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("two kivali tags: %v", err)
	}
}

// TestUpgradeToAReleaseWithoutBrowserPods: a release whose chart has no
// browser section ships three images, and upgrading to it succeeds.
func TestUpgradeToAReleaseWithoutBrowserPods(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeReleaseWith(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "0.16.0", "v0.16.0", false, repoKivali, repoEgress, repoShell)
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if st := h.sup.State(); st.Kivali != "0.16.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
}

// TestUpgradeRefusesAReleaseMissingItsBrowserImage: a chart with a
// browser section needs the browser image in the bundle; without it
// the upgrade stops before touching the org.
func TestUpgradeRefusesAReleaseMissingItsBrowserImage(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeReleaseWith(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "0.16.0", "v0.16.0", true, repoKivali, repoEgress, repoShell)
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "kivali-browser:v0.16.0 is missing") {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Upgrade != nil || len(h.vm.boots) != 1 {
		t.Fatalf("a refused release touched the org: %+v, %d boots", h.sup.State(), len(h.vm.boots))
	}
}

func TestUpgradeCommits(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	_, before := h.manifestValues()
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")

	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	st := h.sup.State()
	if st.Kivali != "0.16.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
	if exists(h.snapPath()) {
		t.Fatal("snapshot left behind")
	}
	file, after := h.manifestValues()
	if file != "kivali-0.16.0.tgz" || getPath(t, after, "image", "tag") != "v0.16.0" || getPath(t, after, "egress", "image", "tag") != "v0.16.0" {
		t.Fatalf("manifest %s %v", file, after)
	}
	for _, k := range [][]string{{"secrets", "sessionKey"}, {"secrets", "ownerEmails"}, {"initImage"}} {
		if getPath(t, before, k...) != getPath(t, after, k...) {
			t.Errorf("%v changed across the upgrade", k)
		}
	}
	if m, _ := h.sup.current(); m == nil {
		t.Fatal("VM not running after the upgrade")
	}
	// Two boots before the upgrade? One: install; the upgrade adds one.
	if len(h.vm.boots) != 2 {
		t.Fatalf("%d boots", len(h.vm.boots))
	}
}

// The release a newer desktop brought: its VM image bakes a chart newer
// than the team's, and `upgrade --baked` moves the team to it with
// nothing downloaded (no downloads directory, no feed), through the
// same journal, snapshot and waits as a feed upgrade. Guards the
// first-boot update the desktop runs after it is updated.
func TestUpgradeFromTheBakedRelease(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	_, before := h.manifestValues()
	h.bakeRelease("0.16.0", "v0.16.0")
	if b := h.sup.Status(context.Background()).Baked; b == nil || !b.Newer || b.Version != "0.16.0" {
		t.Fatalf("baked verdict before the upgrade: %+v", b)
	}

	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf()); err != nil {
		t.Fatalf("upgrade --baked: %v", err)
	}
	st := h.sup.State()
	if st.Kivali != "0.16.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
	if exists(h.snapPath()) {
		t.Fatal("snapshot left behind")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "downloads")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a baked upgrade made a downloads directory: %v", err)
	}
	file, after := h.manifestValues()
	if file != "kivali-0.16.0.tgz" || getPath(t, after, "image", "tag") != "v0.16.0" || getPath(t, after, "egress", "image", "tag") != "v0.16.0" {
		t.Fatalf("manifest %s %v", file, after)
	}
	if h.disk()[StaticChartsRel+"/kivali-0.16.0.tgz"] != "chart baked kivali-0.16.0.tgz" {
		t.Fatalf("the chart was not placed from the VM image: %q", h.disk()[StaticChartsRel+"/kivali-0.16.0.tgz"])
	}
	for _, k := range [][]string{{"secrets", "sessionKey"}, {"secrets", "ownerEmails"}, {"initImage"}} {
		if getPath(t, before, k...) != getPath(t, after, k...) {
			t.Errorf("%v changed across the upgrade", k)
		}
	}
	if len(h.vm.boots) != 2 {
		t.Fatalf("%d boots", len(h.vm.boots))
	}
	if b := h.sup.Status(context.Background()).Baked; b == nil || b.Newer || b.Version != "0.16.0" {
		t.Fatalf("baked verdict after the upgrade: %+v", b)
	}
}

// A baked upgrade that fails rolls back like a feed upgrade: the team
// is on its version again, and nothing is left of the attempt.
func TestBakedUpgradeRollsBack(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.broken["v0.16.0"] = true
	h.bakeRelease("0.16.0", "v0.16.0")

	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "still on 0.15.0") {
		t.Fatalf("upgrade: %v", err)
	}
	st := h.sup.State()
	if st.Kivali != "0.15.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
	if exists(h.snapPath()) {
		t.Fatal("snapshot left behind")
	}
	if file, _ := h.manifestValues(); file != "kivali-0.15.0.tgz" {
		t.Fatalf("manifest %s after the rollback", file)
	}
}

// The baked release is applied only between two releases, and only
// forward: a development build on either side, the same version and
// an older VM image are refused before anything is touched, and the
// report's verdict agrees with each refusal. Guards the rule that keeps
// a local build from moving a real team, and a dev team from being
// moved onto a release.
func TestBakedUpgradeRefusals(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	boots := len(h.vm.boots)
	try := func(installed, baked, tag, wantErr string) {
		t.Helper()
		if err := h.update(t, func(st *State) { st.Kivali = installed }); err != nil {
			t.Fatal(err)
		}
		h.bakeRelease(baked, tag)
		if b := h.sup.Status(context.Background()).Baked; b == nil || b.Newer || b.Version != strings.TrimPrefix(baked, "v") {
			t.Errorf("%s with %s baked: verdict %+v, want not newer", installed, baked, b)
		}
		err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf())
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s with %s baked: %v, want %q", installed, baked, err, wantErr)
		}
		if st := h.sup.State(); st.Kivali != installed || st.Upgrade != nil {
			t.Errorf("%s with %s baked: state %+v", installed, baked, st)
		}
	}
	try("0.15.0", "0.15.0", "v0.15.0", "up to date")
	try("0.15.0", "0.14.2", "v0.14.2", "up to date")
	try("0.0.0-dev", "0.16.0", "v0.16.0", "development build")
	try("0.15.0", "0.0.0-dev", "dev", "development build")
	try("0.0.0-dev", "0.0.0-dev", "dev", "development build")
	// A release whose upgrade needs steps by hand (the image's marker)
	// is newer but refused, as the feed's manual_steps release is.
	if err := h.update(t, func(st *State) { st.Kivali = "0.15.0" }); err != nil {
		t.Fatal(err)
	}
	h.bakeRelease("0.16.0", "v0.16.0")
	h.vm.mu.Lock()
	h.vm.manualSteps = true
	h.vm.mu.Unlock()
	if b := h.sup.Status(context.Background()).Baked; b == nil || !b.Newer || !b.ManualSteps {
		t.Errorf("manual steps: verdict %+v, want newer with manual steps", b)
	}
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf()); err == nil || !strings.Contains(err.Error(), "needs steps the app can't do yet") {
		t.Fatalf("manual steps: %v", err)
	}
	h.vm.mu.Lock()
	h.vm.manualSteps = false
	h.vm.mu.Unlock()
	// A development build is refused even with --force; the same
	// release is not.
	if err := h.update(t, func(st *State) { st.Kivali = "0.0.0-dev" }); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true, Force: true}, h.logf()); err == nil || !strings.Contains(err.Error(), "development build") {
		t.Fatalf("dev team, --force: %v", err)
	}
	if len(h.vm.boots) != boots {
		t.Fatalf("a refusal booted the VM (%d boots, had %d)", len(h.vm.boots), boots)
	}
	// No kivali chart baked: no verdict, and a refusal naming it.
	if err := h.update(t, func(st *State) { st.Kivali = "0.15.0" }); err != nil {
		t.Fatal(err)
	}
	h.vm.mu.Lock()
	h.vm.charts = nil
	h.vm.mu.Unlock()
	if b := h.sup.Status(context.Background()).Baked; b != nil {
		t.Errorf("verdict with no baked chart: %+v", b)
	}
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf()); err == nil || !strings.Contains(err.Error(), "bakes 0 kivali charts") {
		t.Fatalf("no baked chart: %v", err)
	}
}

// The VM image's images reach containerd as k3s imports them at boot;
// a baked upgrade waits for them, and when one never arrives it fails
// before the journal, with the team untouched.
func TestBakedUpgradeWaitsForTheImport(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.bakeRelease("0.16.0", "v0.16.0")
	// The boot's import happened before the bake (the fake imports at
	// boot): the new refs are baked but not imported.
	h.vm.mu.Lock()
	for r := range h.vm.imported {
		if strings.HasSuffix(r, ":v0.16.0") {
			delete(h.vm.imported, r)
		}
	}
	h.vm.mu.Unlock()
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Baked: true}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "kivali:v0.16.0 is not in containerd yet") {
		t.Fatalf("upgrade with the images missing: %v", err)
	}
	if st := h.sup.State(); st.Kivali != "0.15.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
	if len(h.vm.boots) != 1 {
		t.Fatalf("%d boots", len(h.vm.boots))
	}
}

// The development-build rule that keeps a local build from moving a real
// team and a dev team from being moved onto a release; the desktop's
// updates::is_dev_version is the same rule, so the two sides must agree
// on every shape here.
func TestIsDevVersion(t *testing.T) {
	for _, v := range []string{"dev", " dev ", "0.0.0-dev", "v0.0.0-dev", "1.3.0-dev.2", "1.3.0-rc.1.dev+build"} {
		if !isDevVersion(v) {
			t.Errorf("isDevVersion(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "1.2.2", "v1.2.2", "0.17.0-rc.2", "devel", "1.2.2-devel", "1.2.2+dev"} {
		if isDevVersion(v) {
			t.Errorf("isDevVersion(%q) = true, want false", v)
		}
	}
}

func TestUpgradeRollsBackABrokenRelease(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	h.vm.broken["v0.16.0-broken"] = true
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0-broken")

	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "still on 0.15.0") || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("upgrade: %v", err)
	}
	st := h.sup.State()
	if st.Kivali != "0.15.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
	if exists(h.snapPath()) {
		t.Fatal("snapshot left behind")
	}
	if got := h.disk()[ManifestRel]; got != before {
		t.Fatalf("manifest not restored:\n%s", got)
	}
	if _, ok := h.disk()[StaticChartsRel+"/kivali-0.16.0.tgz"]; ok {
		t.Fatal("the new chart survived the rollback")
	}
	if m, _ := h.sup.current(); m == nil {
		t.Fatal("VM not running after the rollback")
	}
}

// A release whose chart or values Helm refuses must roll back as soon as
// the install job has failed three times with the same error (three
// looks), with helm's own error. k3s's
// install job restarts its container in place and never counts a failed
// pod, so without this the update waited out the whole rollout timeout
// (about 11 minutes with the rollback) and named only the chart label.
func TestUpgradeRollsBackAChartHelmCannotInstall(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	h.vm.helmBroken["kivali-0.16.0.tgz"] = true
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "still on 0.15.0") || !strings.Contains(err.Error(), "Error: INSTALLATION FAILED: duplicate entries .env: duplicate key") {
		t.Fatalf("upgrade: %v", err)
	}
	// One look per poll: the third failure ends the wait, seconds in.
	if h.vm.helmRuns != helmJobFailures {
		t.Fatalf("the install job's pod was looked at %d times; the third failure should end the wait", h.vm.helmRuns)
	}
	if h.sup.State().Kivali != "0.15.0" || h.disk()[ManifestRel] != before {
		t.Fatalf("not rolled back: %+v", h.sup.State())
	}
}

// failedHelmRuns must count a failed run whichever state the container
// is in, and never an earlier install's finished pod: a miscount either
// lets a broken chart wait out the rollout timeout or rolls back an
// update whose install got past a few transient failures.
func TestFailedHelmRuns(t *testing.T) {
	for _, c := range []struct {
		name, pods string
		failures   int
		previous   bool
	}{
		{"waiting in CrashLoopBackOff after its third failure", `{"items":[{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":[{"name":"helm","restartCount":2,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1}}}]}}]}`, 3, true},
		{"terminated, not yet restarted", `{"items":[{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":[{"name":"helm","restartCount":2,"state":{"terminated":{"exitCode":1,"message":"boom"}},"lastState":{"terminated":{"exitCode":1}}}]}}]}`, 3, false},
		{"running again after two failures", `{"items":[{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":[{"name":"helm","restartCount":2,"state":{"running":{}},"lastState":{"terminated":{"exitCode":1}}}]}}]}`, 2, true},
		{"an earlier install's finished pod", `{"items":[{"metadata":{"name":"p"},"status":{"phase":"Succeeded","containerStatuses":[{"name":"helm","restartCount":5,"state":{"terminated":{"exitCode":0}}}]}}]}`, 0, false},
	} {
		var pl helmPodList
		if err := json.Unmarshal([]byte(c.pods), &pl); err != nil {
			t.Fatal(err)
		}
		var r helmRun
		if runs := failedHelmRuns(pl); len(runs) > 0 {
			r = runs[0]
		}
		if r.failures != c.failures || (r.failures > 0 && r.previous != c.previous) {
			t.Errorf("%s: %+v, want %d failures (previous %v)", c.name, r, c.failures, c.previous)
		}
	}
}

// Helm runs that fail while the cluster is still starting fail each
// with some other error, and the install then goes through. Counting
// them as a broken chart would abort a healthy install or roll back a
// good update on a slow first boot.
func TestHelmInstallGetsPastDifferentTransientErrors(t *testing.T) {
	h := newHarness(t)
	h.vm.helmErrors = []string{
		"Error: Kubernetes cluster unreachable: the server is currently unable to handle the request",
		"Error: failed to create resource: etcdserver: request timed out",
		"Error: INSTALLATION FAILED: Internal error occurred: failed calling webhook",
	}
	if err := h.up(UpOptions{Install: owner}); err != nil {
		t.Fatalf("up: %v", err)
	}
	if h.vm.helmRuns != 3 || h.sup.State().Kivali != "0.15.0" {
		t.Fatalf("%d failed runs, state %+v", h.vm.helmRuns, h.sup.State())
	}
}

// Transient failures before a broken chart's own error neither end the
// wait nor delay its end: it fails on the third run with the chart's
// error, not on the third failed run, and not after the timeout.
func TestHelmFailsOnTheThirdIdenticalError(t *testing.T) {
	h := newHarness(t)
	h.vm.helmBroken["kivali-0.15.0.tgz"] = true
	h.vm.helmErrors = []string{
		"Error: Kubernetes cluster unreachable: the server is currently unable to handle the request",
		"Error: failed to create resource: etcdserver: request timed out",
	}
	err := h.up(UpOptions{Install: owner})
	if err == nil || !strings.Contains(err.Error(), "failed to install the chart 3 times with the same error: Error: INSTALLATION FAILED: duplicate entries .env: duplicate key") {
		t.Fatalf("up: %v", err)
	}
	if h.vm.helmRuns != 5 {
		t.Fatalf("the install job's pod was looked at %d times; the fifth run, the third identical failure, should end the wait", h.vm.helmRuns)
	}
}

func TestUpgradeRollsBackAChartRenderingAnotherImage(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	h.vm.renderSuffix = "-broken"
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), `runs image "kivali:v0.16.0-broken"`) {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Kivali != "0.15.0" || h.disk()[ManifestRel] != before {
		t.Fatalf("not rolled back: %+v", h.sup.State())
	}
}

func TestUpgradeCloneFailureLeavesTheOrg(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.snapshot = func(_, dst string) error {
		_ = os.WriteFile(dst, []byte("partial"), 0o600)
		return errors.New("disk full")
	}
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("upgrade: %v", err)
	}
	if exists(h.snapPath()) || h.sup.State().Upgrade != nil || h.sup.State().Kivali != "0.15.0" {
		t.Fatalf("after a failed clone: %+v", h.sup.State())
	}
	if m, _ := h.sup.current(); m == nil {
		t.Fatal("org not restarted")
	}
}

func TestUpgradeStageFailureTouchesNothing(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	dir := filepath.Join(t.TempDir(), "rel")
	feed := writeRelease(t, dir, "0.16.0", "v0.16.0")
	if err := os.WriteFile(filepath.Join(dir, "kivali-0.16.0.tgz"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Upgrade != nil || len(h.vm.boots) != 1 {
		t.Fatalf("a failed fetch touched the org: %+v, %d boots", h.sup.State(), len(h.vm.boots))
	}
}

func TestUpgradeRefusals(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	same := writeRelease(t, filepath.Join(t.TempDir(), "same"), "0.15.0", "v0.15.0")
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: same}, h.logf()); err == nil || !strings.Contains(err.Error(), "up to date") {
		t.Fatalf("same version: %v", err)
	}
	if err := h.update(t, func(st *State) { st.Upgrade = &Journal{From: "a", To: "b", Step: StepPrepared} }); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: same, Force: true}, h.logf()); err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("open journal: %v", err)
	}
}

func TestValidVersion(t *testing.T) {
	for _, v := range []string{"1.0.0", "v1.0.0", "1.0.0-rc.1", "v1.0.0-dev.12", "1.0.0+build.5", "10.20.30-a-b.c+d"} {
		if !validVersion(v) {
			t.Errorf("validVersion(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "latest", "1.0", "1.0.0.0", "1.0.0-", "1.0.0-x/../../escape", `1.0.0-x\..\escape`, "../1.0.0", "1.0.0 ", "1.0.0-rc 1"} {
		if validVersion(v) {
			t.Errorf("validVersion(%q) = true, want false", v)
		}
	}
}

// A feed whose version would name a directory outside downloads is
// refused before anything is downloaded, even with a chart that agrees
// with it.
func TestUpgradeRefusesAFeedVersionThatIsNotAVersion(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	for _, v := range []string{"0.16.0-x/../../escape", `0.16.0-x\..\..\escape`, "latest"} {
		feed := writeReleaseChart(t, filepath.Join(t.TempDir(), "rel"), v, v, "v0.16.0")
		err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed, Force: true}, h.logf())
		if err == nil || !strings.Contains(err.Error(), "is not vMAJOR.MINOR.PATCH") {
			t.Fatalf("version %q: %v", v, err)
		}
		res, err := h.sup.Check(context.Background(), feed, h.logf())
		if err != nil || res.Status != CheckFailed {
			t.Fatalf("check %q: %+v %v", v, res, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("a directory outside downloads: %v", err)
	}
}

// stage keeps a release's downloads under downloads whatever reached it.
func TestStageRefusesADirectoryOutsideDownloads(t *testing.T) {
	h := newHarness(t)
	rel := Release{Version: "0.16.0-x/../../escape", Images: map[string]Asset{h.sup.platform(): {Name: "images.tar.zst"}}}
	_, err := h.sup.stage(context.Background(), nil, "", rel, h.logf())
	if err == nil || !strings.Contains(err.Error(), "does not name a directory under") {
		t.Fatalf("stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("a directory outside downloads: %v", err)
	}
}

func (h *harness) update(t *testing.T, fn func(*State)) error {
	t.Helper()
	return h.sup.update(fn)
}

// crashAt leaves the org as a crash at a journal step would: the VM
// stopped, the journal on disk, and serve gone.
func (h *harness) crashAt(j Journal) {
	h.t.Helper()
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		h.t.Fatal(err)
	}
	if err := h.update(h.t, func(st *State) { st.Upgrade = &j }); err != nil {
		h.t.Fatal(err)
	}
	h.restart()
}

func TestRecoverCrashDuringClone(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	snap := h.snapPath()
	if err := os.WriteFile(snap, []byte("partial clone"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.crashAt(Journal{From: "0.15.0", To: "0.16.0", Step: StepSnapshotting, Snapshot: snap})
	h.mustUp(UpOptions{})
	if exists(snap) || h.sup.State().Upgrade != nil || h.sup.State().Kivali != "0.15.0" {
		t.Fatalf("after recovery: snapshot %v, state %+v", exists(snap), h.sup.State())
	}
	if h.disk()[ManifestRel] != before {
		t.Fatal("data disk changed")
	}
}

func TestRecoverUncommittedRollsBack(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	snap := h.snapPath()
	if err := host.Default().Snapshot(h.dataPath(), snap); err != nil {
		t.Fatal(err)
	}
	// The new release had started writing to the disk.
	fs := h.disk()
	fs[ManifestRel] = "half-applied"
	if err := writeDisk(h.dataPath(), fs); err != nil {
		t.Fatal(err)
	}
	h.crashAt(Journal{From: "0.15.0", To: "0.16.0", Step: StepApplying, Snapshot: snap, SnapshotComplete: true})
	h.mustUp(UpOptions{})
	if exists(snap) || h.sup.State().Upgrade != nil || h.sup.State().Kivali != "0.15.0" {
		t.Fatalf("after recovery: snapshot %v, state %+v", exists(snap), h.sup.State())
	}
	if h.disk()[ManifestRel] != before {
		t.Fatalf("data disk not rolled back: %q", h.disk()[ManifestRel])
	}
}

func TestRecoverCommittedCleansUp(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	snap := h.snapPath()
	if err := os.WriteFile(snap, []byte("old disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.crashAt(Journal{From: "0.15.0", To: "0.16.0", Step: StepCommitted, Snapshot: snap, SnapshotComplete: true})
	h.mustUp(UpOptions{})
	if exists(snap) || h.sup.State().Upgrade != nil || h.sup.State().Kivali != "0.16.0" {
		t.Fatalf("after recovery: snapshot %v, state %+v", exists(snap), h.sup.State())
	}
}

func TestCheck(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	ctx := context.Background()
	newer := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	res, err := h.sup.Check(ctx, newer, h.logf())
	if err != nil || res.Status != CheckUpgrade || res.Latest != "0.16.0" {
		t.Fatalf("check: %+v %v", res, err)
	}
	st := h.sup.State()
	if st.LastCheck == nil || !st.LastCheckOK || st.Latest == nil || st.Latest.Latest != "0.16.0" {
		t.Fatalf("state %+v", st)
	}

	res, err = h.sup.Check(ctx, filepath.Join(t.TempDir(), "missing.json"), h.logf())
	if err != nil || res.Status != CheckFailed || h.sup.State().LastCheckOK {
		t.Fatalf("failed check: %+v %v", res, err)
	}

	var rel Release
	b, _ := os.ReadFile(newer)
	_ = json.Unmarshal(b, &rel)
	for _, tc := range []struct {
		edit func(*Release)
		want string
	}{
		{func(r *Release) { r.MinDesktopVersion = "0.17.0" }, CheckAppTooOld},
		{func(r *Release) { r.ManualSteps = true }, CheckManualSteps},
		{func(r *Release) { r.Version = "v0.15.0"; r.MinDesktopVersion = "0.14.0" }, CheckUpToDate},
	} {
		r := rel
		tc.edit(&r)
		got := h.sup.evaluate(r, "0.15.0")
		if got.Status != tc.want {
			t.Errorf("evaluate: %s, want %s (%s)", got.Status, tc.want, got.Message)
		}
		if got.MinDesktopVersion != r.MinDesktopVersion {
			t.Errorf("evaluate: min_desktop_version %q, want %q", got.MinDesktopVersion, r.MinDesktopVersion)
		}
	}

	// Through Check and the wire: the minimum is passed on even when the
	// verdict is up to date, and the supervisor's own verdict is kept.
	for _, tc := range []struct{ version, min, want string }{
		{"0.15.0", "0.16.0", CheckUpToDate},
		{"0.16.0", "0.15.0", CheckUpgrade},
		{"0.16.0", "0.16.0", CheckAppTooOld}, // this supervisor is 0.15.0
	} {
		r := rel
		r.Version, r.MinDesktopVersion = tc.version, tc.min
		b, _ := json.Marshal(r)
		feed := filepath.Join(t.TempDir(), "release.json")
		if err := os.WriteFile(feed, b, 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := h.sup.Check(ctx, feed, h.logf())
		if err != nil || res.Status != tc.want || res.MinDesktopVersion != tc.min {
			t.Fatalf("check %s: %+v %v", tc.version, res, err)
		}
		wire, _ := json.Marshal(res)
		if !strings.Contains(string(wire), `"min_desktop_version":"`+tc.min+`"`) {
			t.Fatalf("wire %s", wire)
		}
		if h.sup.State().Latest.MinDesktopVersion != tc.min {
			t.Fatalf("local.json latest %+v", h.sup.State().Latest)
		}
	}
}

func TestLoadImagesAndBackup(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	// The archive is whatever the caller hands over (a docker-archive
	// tar here); the guest imports it as it is.
	var archive bytes.Buffer
	if err := writeTarTo(&archive, map[string]string{"manifest.json": `[{"RepoTags":["kivali:dev","kivali-egress-proxy:dev","kivali-agent-shell:dev","kivali-browser:dev"]}]`}); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.LoadImages(context.Background(), &archive, h.logf()); err != nil {
		t.Fatal(err)
	}
	if !h.vm.imported["docker.io/library/kivali-agent-shell:dev"] || !h.vm.imported["docker.io/library/kivali-browser:dev"] {
		t.Fatalf("imported %v", h.vm.imported)
	}
	// restart alone, for images built straight into the VM.
	if err := h.sup.Restart(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	// The backup is the app's own download, signed in through the
	// handoff, over the guest's proxy.
	h.vm.startOrg(t)
	var buf bytes.Buffer
	if err := h.sup.Backup(context.Background(), &buf, h.logf()); err != nil || buf.String() != "PK-BACKUP-ARCHIVE" {
		t.Fatalf("backup %q %v", buf.String(), err)
	}
}

// A restore uploads the zip to the app's own restore, signed in as the
// owner; the app's refusal (an org that is not fresh) comes back as is.
func TestRestoreGoesThroughTheApp(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.startOrg(t)
	in := filepath.Join(h.dir, "org.zip")
	if err := os.WriteFile(in, []byte("PK-THE-ORG"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.RestoreFromFile(context.Background(), in, h.logf()); err != nil {
		t.Fatal(err)
	}
	if string(h.vm.restored) != "PK-THE-ORG" {
		t.Fatalf("the org took %q", h.vm.restored)
	}
	h.vm.restoreRefusal = "restore is only available on a fresh deployment"
	if err := h.sup.RestoreFromFile(context.Background(), in, h.logf()); err == nil || !strings.Contains(err.Error(), "fresh deployment") {
		t.Fatalf("refused restore: %v", err)
	}
	if err := h.sup.RestoreFromFile(context.Background(), "/etc/hosts", h.logf()); err == nil {
		t.Fatal("restored from outside the config and home directories")
	}
}

func TestScanConsole(t *testing.T) {
	in := "boot noise\r\nKIVALI-VM: boot: done\nKIVALI-VM READY\nKIVALI-VM FATAL: ready: x\n" + CleanLine + "\n"
	events := make(chan ConsoleEvent, 8)
	var log bytes.Buffer
	ScanConsole(strings.NewReader(in), &log, events)
	close(events)
	var kinds []EventKind
	for ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	if len(kinds) != 3 || kinds[0] != EventReady || kinds[1] != EventFatal || kinds[2] != EventClean {
		t.Fatalf("events %v", kinds)
	}
	if strings.Count(log.String(), "\n") != 5 || strings.Contains(log.String(), "\r") {
		t.Fatalf("log %q", log.String())
	}
}

// A console line longer than the scanner's limit (binary noise on a
// serial port) is passed on in pieces and the scan goes on to the end:
// stopping would leave the guest's console writes blocked, and on
// Hyper-V would end Done with the VM still running.
func TestScanConsoleSurvivesAnOverlongLine(t *testing.T) {
	in := strings.Repeat("x", 3*maxConsoleLine+17) + "\n" + ReadyLine + "\n" + CleanLine + "\n"
	events := make(chan ConsoleEvent, 8)
	r := strings.NewReader(in)
	ScanConsole(r, io.Discard, events)
	if r.Len() != 0 {
		t.Fatalf("ScanConsole returned with %d bytes unread", r.Len())
	}
	close(events)
	var kinds []EventKind
	for ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	if len(kinds) != 2 || kinds[0] != EventReady || kinds[1] != EventClean {
		t.Fatalf("events %v, want READY then the clean line", kinds)
	}
}

func TestForwarder(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(b, buf)
		_, _ = b.Write(append([]byte("re:"), buf...))
		_ = b.Close()
	}()
	f, err := StartForward(0, func(context.Context) (net.Conn, error) { return a, nil }, &fakeClock{}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !strings.HasPrefix(f.Addr(), "127.0.0.1:") {
		t.Fatalf("bound %s", f.Addr())
	}
	c, err := net.Dial("tcp", f.Addr())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("hello"))
	got, _ := io.ReadAll(c)
	if string(got) != "re:hello" {
		t.Fatalf("got %q", got)
	}
}

// failingListener fails its first fails accepts, as a real listener
// does when the descriptor it just accepted is closed before Go sets
// it non-blocking, then hands out the connections sent on conns.
type failingListener struct {
	fails  int
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.fails > 0 {
		l.fails--
		return nil, errors.New("accept tcp 127.0.0.1:8080: setnonblock: bad file descriptor")
	}
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *failingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *failingListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// TestForwarderRetriesFailedAccept guards against an org that stops
// answering while its VM and server are healthy. On a macOS team
// (2026-10-21) one accept failed with "setnonblock: bad file
// descriptor" and the forward's accept loop returned, so every request
// from the app waited forever until the supervisor restarted. A failed
// accept is now logged and retried, with a delay that doubles to a
// cap, and the next connection is forwarded.
func TestForwarderRetriesFailedAccept(t *testing.T) {
	ln := &failingListener{fails: 10, conns: make(chan net.Conn), closed: make(chan struct{})}
	up, guest := net.Pipe()
	go func() {
		buf := make([]byte, 5)
		_, _ = io.ReadFull(guest, buf)
		_, _ = guest.Write(append([]byte("re:"), buf...))
		_ = guest.Close()
	}()
	var mu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	f := serveForward(ln, func(context.Context) (net.Conn, error) { return up, nil }, &fakeClock{}, logf)
	defer f.Close()
	stopped := make(chan struct{})
	go func() { f.wg.Wait(); close(stopped) }()
	client, browser := net.Pipe()
	select {
	case ln.conns <- browser: // accepted only once the forward has retried past every failure
	case <-stopped:
		t.Fatal("the forward stopped accepting after a failed accept")
	}
	_, _ = client.Write([]byte("hello"))
	got, _ := io.ReadAll(client)
	if string(got) != "re:hello" {
		t.Fatalf("got %q after the failed accepts", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"5ms", "10ms", "20ms", "40ms", "80ms", "160ms", "320ms", "640ms", "1s", "1s"}
	if len(logs) != len(want) {
		t.Fatalf("logged %q, want one line per failed accept", logs)
	}
	for i, d := range want {
		if !strings.HasPrefix(logs[i], "forward: accept: ") || !strings.HasSuffix(logs[i], "; retrying in "+d) {
			t.Fatalf("log line %d = %q, want the accept error and a retry in %s", i, logs[i], d)
		}
	}
}

func TestRPC(t *testing.T) {
	h := newHarness(t)
	hst := h.sup.o.Host
	dir := t.TempDir()
	ln, err := hst.Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(hst.Endpoint(dir)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode %v %v", fi, err)
		}
	}
	if second, err := hst.Listen(dir); err == nil {
		_ = second.Close()
		t.Fatal("second serve on a live endpoint")
	}
	rpc := NewRPCServer(h.sup)
	srv := &http.Server{Handler: rpc.Handler()}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	c := &Client{Host: hst, Dir: dir}
	if !c.Ping(context.Background()) || c.Endpoint() != hst.Endpoint(dir) {
		t.Fatalf("client of %s does not reach serve", c.Endpoint())
	}
	ctx := context.Background()

	var logs []string
	res, err := c.Op(ctx, "up", UpOptions{Install: owner, Port: h.port}, func(l string) { logs = append(logs, l) })
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(res, &r); err != nil || !r.Running || r.State.Kivali != "0.15.0" || r.Guest == nil {
		t.Fatalf("report %+v %v", r, err)
	}
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, "\n"), ReadyLine) {
		t.Fatalf("logs %v", logs)
	}
	if _, err := c.Op(ctx, "install", owner, nil); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatalf("second install: %v", err)
	}
	h.vm.startOrg(t)
	out := filepath.Join(h.dir, "backups", "org.zip")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err = c.Op(ctx, "backup", BackupRequest{Path: out}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var br BackupResult
	_ = json.Unmarshal(res, &br)
	if b, err := os.ReadFile(out); err != nil || string(b) != "PK-BACKUP-ARCHIVE" || br.Bytes != int64(len(b)) {
		t.Fatalf("backup %q %v %+v", b, err, br)
	}
	for _, bad := range []string{"/etc/kivali.zip", "relative.zip", filepath.Join(h.dir, "..", "escape.zip"), filepath.Join(filepath.VolumeName(h.dir)+string(filepath.Separator), "kivali.zip")} {
		if _, err := c.Op(ctx, "backup", BackupRequest{Path: bad}, nil); err == nil {
			t.Errorf("backup to %s accepted", bad)
		}
	}
	// The backup restores through the same RPC (the fake org takes it).
	if _, err := c.Op(ctx, "restore", RestoreRequest{Path: out}, nil); err != nil || string(h.vm.restored) != "PK-BACKUP-ARCHIVE" {
		t.Fatalf("restore: %v, the org took %q", err, h.vm.restored)
	}
	if _, err := c.Op(ctx, "down", DownRequest{Exit: true}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rpc.Quit():
	default:
		t.Fatal("down --exit did not ask serve to quit")
	}
	if c := h.vm.compacted(); len(c) != 0 {
		t.Fatalf("down --exit compacted the data disk: %+v", c)
	}
	st, err := c.Status(ctx)
	if err != nil || st.Running {
		t.Fatalf("status after down: %+v %v", st, err)
	}
}

func TestGuestStateFatalFailsBoot(t *testing.T) {
	// The console said READY but the agent reports FATAL (a check that
	// failed after the marker): the boot fails with the agent's reason.
	h := newHarness(t)
	h.sup.o.GuestFor = func(m Machine) Guest { return fatalGuest{fakeGuest{m.(*fakeMachine)}} }
	err := h.up(UpOptions{Install: owner})
	if err == nil || !strings.Contains(err.Error(), "node gone") {
		t.Fatalf("up: %v", err)
	}
}

type fatalGuest struct{ fakeGuest }

func (g fatalGuest) Status(ctx context.Context) (guestapi.Status, error) {
	st, err := g.fakeGuest.Status(ctx)
	st.State, st.Fatal = guestapi.StateFatal, "node gone"
	return st, err
}
