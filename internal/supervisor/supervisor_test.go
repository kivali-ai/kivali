package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestUpTakesAFreePortWhenTheRecordedOneIsBusy(t *testing.T) {
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

	// A port asked for by name is never moved.
	err = h.sup.Up(context.Background(), UpOptions{Port: h.port}, h.logf())
	if !h.sup.o.Host.AddrInUse(err) {
		t.Fatalf("explicit busy port: %v", err)
	}
	// Without one, up takes a free port, records it, and serves there.
	if err := h.sup.Up(context.Background(), UpOptions{}, h.logf()); err != nil {
		t.Fatalf("up: %v", err)
	}
	got := h.sup.State().Port
	if got == h.port || got == 0 {
		t.Fatalf("port %d still recorded", got)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(got)))
	if err != nil {
		t.Fatalf("forward not listening on the chosen port: %v", err)
	}
	_ = c.Close()
	h.restart()
	if h.sup.State().Port != got {
		t.Fatalf("chosen port not persisted: %d", h.sup.State().Port)
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

func TestSetOverridesAndHelmFailure(t *testing.T) {
	h := newHarness(t)
	h.vm.helmFails = 3
	o := owner
	o.Set = []string{"secrets.oauthRedirectURL=http://127.0.0.1:1/auth/callback", "networkPolicy.serverIngress.enabled=false"}
	err := h.up(UpOptions{Install: o})
	if err == nil || !strings.Contains(err.Error(), "failed to install the chart 3 times: Error: INSTALLATION FAILED: duplicate entries .env: duplicate key") {
		t.Fatalf("up: %v", err)
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
	if _, err := pickImages(refs, false); err == nil || !strings.Contains(err.Error(), "kivali-dev-shell:dev") {
		t.Fatalf("missing dev-shell: %v", err)
	}
	refs = append(refs, "docker.io/library/kivali-dev-shell:dev", "docker.io/library/kivali:v2")
	if _, err := pickImages(refs, false); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("two kivali tags: %v", err)
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
	if err := writeTarTo(&archive, map[string]string{"manifest.json": `[{"RepoTags":["kivali:dev","kivali-egress-proxy:dev","kivali-dev-shell:dev"]}]`}); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.LoadImages(context.Background(), &archive, h.logf()); err != nil {
		t.Fatal(err)
	}
	if !h.vm.imported["docker.io/library/kivali-dev-shell:dev"] {
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
	f, err := StartForward(0, func(context.Context) (net.Conn, error) { return a, nil }, func(string, ...any) {})
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
