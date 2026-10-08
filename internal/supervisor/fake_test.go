package supervisor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// fakeClock never sleeps: After advances the clock and fires at once.
// The supervisor's waits give events already delivered priority over a
// timer, so the fakes deliver everything before the wait starts.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

// The fake guest keeps its "filesystem" (paths relative to the data
// directory) as JSON inside the data disk file itself, so the data disk
// clone, the rollback swap and the format rule act on real content.
type diskFS map[string]string

func readDisk(p string) (diskFS, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	fs := diskFS{}
	if err := json.Unmarshal(b, &fs); err != nil {
		return nil, fmt.Errorf("not a kivali disk: %w", err)
	}
	return fs, nil
}

func writeDisk(p string, fs diskFS) error {
	b, err := json.Marshal(fs)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// blank reports whether the head of the disk is all zeros.
func blank(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 4096)
	n, _ := io.ReadFull(f, b)
	return n == len(b) && bytes.Count(b, []byte{0}) == len(b)
}

type fakeBackend struct {
	mu       sync.Mutex
	boots    []MachineConfig
	machines []*fakeMachine
	// created records the size of every data disk created.
	created []int64
	// poweroffs counts poweroff requests.
	poweroffs int
	// fatalOnBoot makes the next boot print a FATAL and stop.
	fatalOnBoot string
	// world shared by every machine: images in containerd, broken tags.
	imported map[string]bool
	broken   map[string]bool
	baked    []string
	charts   []guestapi.BakedChart
	// agentDown makes Shutdown over the agent fail (poweroff request
	// path).
	agentDown bool
	// slowShutdown makes Shutdown over the agent succeed without the
	// machine stopping: the guest is still shutting down, and the test
	// stops it.
	slowShutdown bool
	// hardStops counts HardStop calls: power cuts on the data disk.
	hardStops int
	// helmFails is the Helm install job's failure count; while set, no
	// deployment exists.
	helmFails int
	// renderSuffix is appended to the server image the "chart" renders
	// (a chart that ignores the image values).
	renderSuffix string
	// fatalAt makes boot number n (1-based) print a FATAL.
	fatalAt map[int]string
	// proxyEntered, when set, makes Proxy signal it and then block
	// until its context ends.
	proxyEntered chan struct{}
	// importEntered/importRelease, when set, make ImportImages signal
	// and then wait for the release.
	importEntered, importRelease chan struct{}
	// org is the org's server Proxy reaches (startOrg): it signs a
	// handoff in, serves a backup and takes a restore.
	org *httptest.Server
	// backupFails makes the org's backup download stop part way.
	backupFails bool
	// restoreRefusal, when set, is the org's 409 answer to a restore.
	restoreRefusal string
	// restored is the archive the org's restore last took.
	restored []byte
	// execEntered, when set (under mu), makes the next Exec signal it
	// and then block until its context ends.
	execEntered chan struct{}
	// manifestWrites counts HelmChart writes: the deployment's
	// generation follows them, as the Helm controller's upgrades would.
	manifestWrites int
	// authStatus is what `claude auth status --json` prints in the
	// server container; empty prints the signed-out answer, exit 1.
	authStatus string
	// claudeSettings is the CLI's settings file in the server container
	// (nil: none); settingsWrites the argv of each write. Under mu.
	claudeSettings *string
	settingsWrites []string
	// probes is what a deployment check for a model prints (exit 1);
	// a model not in it answers OK. probed records each check's argv.
	// Under mu.
	probes map[string]string
	probed []string
	// ptys, when set, receives the guest end of every terminal opened.
	ptys chan net.Conn
	// removals records every RemoveMachine, with what held at that
	// moment; removeErr, when set, is its answer.
	removals  []machineRemoval
	removeErr error
}

// machineRemoval is one RemoveMachine: its config directory, whether
// every machine had stopped by then, and whether the data disk was
// still there (the VM goes before the files).
type machineRemoval struct {
	configDir  string
	allStopped bool
	diskThere  bool
}

// The fake VM, like Hyper-V's, outlives serve, so destroy removes it.
var _ MachineRemover = (*fakeBackend)(nil)

func (h *fakeBackend) RemoveMachine(configDir string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	stopped := true
	for _, m := range h.machines {
		stopped = stopped && !m.running()
	}
	_, err := os.Stat(h.DataDiskPath(configDir))
	h.removals = append(h.removals, machineRemoval{configDir: configDir, allStopped: stopped, diskThere: err == nil})
	return h.removeErr
}

func (h *fakeBackend) machineRemovals() []machineRemoval {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]machineRemoval(nil), h.removals...)
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		imported: map[string]bool{},
		broken:   map[string]bool{},
		baked: []string{
			"docker.io/library/kivali:v0.15.0",
			"docker.io/library/kivali-egress-proxy:v0.15.0",
			"docker.io/library/kivali-dev-shell:v0.15.0",
			"docker.io/rancher/mirrored-library-busybox:1.37.0",
			"docker.io/rancher/mirrored-pause:3.10.2",
		},
		charts: []guestapi.BakedChart{{Path: "/usr/share/kivali/charts/kivali-0.15.0.tgz", Name: "kivali", Version: "0.15.0", AppVersion: "v0.15.0"}},
	}
}

// fakeArch is the fake image's architecture: deliberately not the macOS
// backend's, so a hard-coded arm64 anywhere in the common code fails.
const fakeArch = "amd64"

func (h *fakeBackend) Image(string) (ImageInfo, error) {
	return ImageInfo{Version: "0.15.0", Arch: fakeArch}, nil
}

// DataDiskPath puts the disk in a subdirectory, as the WSL2 backend
// does, so the common code cannot assume the config directory holds it.
func (h *fakeBackend) DataDiskPath(configDir string) string {
	return filepath.Join(configDir, "disk", "data.disk")
}

// CreateDataDisk records the size and makes a small all-zero file: the
// fake guest reads only its head, and a real 64 GiB file is not sparse
// on every test machine.
func (h *fakeBackend) CreateDataDisk(p string, size int64) error {
	h.mu.Lock()
	h.created = append(h.created, size)
	h.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(p, make([]byte, 64<<10), 0o600)
}

func (h *fakeBackend) NewMachine(c MachineConfig) (Machine, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.boots = append(h.boots, c)
	m := &fakeMachine{vm: h, cfg: c, events: make(chan ConsoleEvent, 16), done: make(chan struct{})}
	h.machines = append(h.machines, m)
	return m, nil
}

// lastFormat reports whether the last boot asked the guest to format.
func (h *fakeBackend) lastFormat() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.boots[len(h.boots)-1].FormatData
}

type fakeMachine struct {
	vm       *fakeBackend
	cfg      MachineConfig
	events   chan ConsoleEvent
	done     chan struct{}
	doneOnce sync.Once
	mu       sync.Mutex
	fs       diskFS
}

func (m *fakeMachine) stop(clean bool) {
	m.doneOnce.Do(func() {
		if clean {
			m.events <- ConsoleEvent{Kind: EventClean, Line: CleanLine}
		}
		close(m.done)
	})
}

// Start follows the guest's boot rules synchronously: format only when
// asked and the disk is blank; otherwise the disk must already be ours.
func (m *fakeMachine) Start() error {
	m.vm.mu.Lock()
	fatal := m.vm.fatalOnBoot
	m.vm.fatalOnBoot = ""
	if f, ok := m.vm.fatalAt[len(m.vm.boots)]; ok {
		fatal = f
	}
	m.vm.mu.Unlock()
	failBoot := func(reason string) error {
		m.events <- ConsoleEvent{Kind: EventFatal, Line: FatalPrefix + ": " + reason}
		m.stop(false)
		return nil
	}
	if m.cfg.FormatData {
		if !blank(m.cfg.DataDisk) {
			return failBoot("boot: data disk is not blank; not formatting")
		}
		if err := writeDisk(m.cfg.DataDisk, diskFS{}); err != nil {
			return err
		}
	}
	fs, err := readDisk(m.cfg.DataDisk)
	if err != nil {
		return failBoot("boot: " + err.Error())
	}
	m.fs = fs
	if fatal != "" {
		return failBoot(fatal)
	}
	m.events <- ConsoleEvent{Kind: EventReady, Line: ReadyLine}
	return nil
}

func (m *fakeMachine) Events() <-chan ConsoleEvent { return m.events }
func (m *fakeMachine) Done() <-chan struct{}       { return m.done }
func (m *fakeMachine) DialAgent(context.Context) (net.Conn, error) {
	return nil, errors.New("fake machine: no agent connection")
}

func (m *fakeMachine) RequestPoweroff() error {
	m.vm.mu.Lock()
	m.vm.poweroffs++
	m.vm.mu.Unlock()
	m.stop(true)
	return nil
}

func (m *fakeMachine) HardStop() error {
	m.vm.mu.Lock()
	m.vm.hardStops++
	m.vm.mu.Unlock()
	m.stop(false)
	return nil
}

func (m *fakeMachine) running() bool {
	select {
	case <-m.done:
		return false
	default:
		return true
	}
}

// fakeGuest is the guest agent of one fake machine.
type fakeGuest struct{ m *fakeMachine }

func (g fakeGuest) Status(context.Context) (guestapi.Status, error) {
	if !g.m.running() {
		return guestapi.Status{}, errors.New("fake guest: VM stopped")
	}
	return guestapi.Status{
		BootID: "boot", State: guestapi.StateReady, NodeReady: true,
		Versions:    guestapi.Versions{Agent: "test", VMImage: "0.15.0"},
		BakedImages: g.m.vm.baked, BakedCharts: g.m.vm.charts,
	}, nil
}

func (g fakeGuest) file(rel string) (string, bool) {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	v, ok := g.m.fs[rel]
	return v, ok
}

func (g fakeGuest) setFile(rel, content string) error {
	if rel == ManifestRel {
		g.m.vm.mu.Lock()
		g.m.vm.manifestWrites++
		g.m.vm.mu.Unlock()
	}
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	g.m.fs[rel] = content
	return writeDisk(g.m.cfg.DataDisk, g.m.fs)
}

func (g fakeGuest) WriteFile(_ context.Context, rel string, _ os.FileMode, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if strings.HasSuffix(rel, ".tgz") {
		meta, err := guestapi.ChartInfo(bytes.NewReader(b))
		if err != nil {
			return err
		}
		return g.setFile(rel, "chart "+meta.Version)
	}
	return g.setFile(rel, string(b))
}

func (g fakeGuest) ImportImages(_ context.Context, r io.Reader) (guestapi.ImportResult, error) {
	if g.m.vm.importEntered != nil {
		close(g.m.vm.importEntered)
		<-g.m.vm.importRelease
	}
	refs, err := guestapi.ImageRefs(r, "bundle.tar")
	if err != nil {
		return guestapi.ImportResult{ExitCode: 1, Output: err.Error()}, nil
	}
	g.m.vm.mu.Lock()
	for _, ref := range refs {
		g.m.vm.imported[ref] = true
	}
	g.m.vm.mu.Unlock()
	return guestapi.ImportResult{Output: strings.Join(refs, "\n")}, nil
}

func (g fakeGuest) Proxy(ctx context.Context, _ int) (net.Conn, error) {
	g.m.vm.mu.Lock()
	entered := g.m.vm.proxyEntered
	g.m.vm.mu.Unlock()
	if entered != nil {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	g.m.vm.mu.Lock()
	org := g.m.vm.org
	g.m.vm.mu.Unlock()
	if org == nil {
		return nil, errors.New("fake guest: no proxy")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", org.Listener.Addr().String())
}

// startOrg stands up the org's server behind Proxy: the handoff takes
// any token and sets a session cookie; backup and restore need it, and
// the same-origin marking the app's API checks.
func (v *fakeBackend) startOrg(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/handoff", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") == "" || r.Host != "127.0.0.1" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "kivali_session", Value: "s", Path: "/"})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if c, err := r.Cookie("kivali_session"); err != nil || c.Value != "s" || r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("POST /api/v1/org/backup", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = io.WriteString(w, "PK-BACKUP-ARCHIVE")
		v.mu.Lock()
		fails := v.backupFails
		v.mu.Unlock()
		if fails {
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	})
	mux.HandleFunc("POST /api/v1/org/restore", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		f, _, err := r.FormFile("archive")
		if err != nil {
			http.Error(w, `{"error":"no backup was uploaded"}`, http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(f)
		v.mu.Lock()
		refusal := v.restoreRefusal
		if refusal == "" {
			v.restored = b
		}
		v.mu.Unlock()
		if refusal != "" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": refusal, "who": "no one"})
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	v.mu.Lock()
	v.org = httptest.NewServer(mux)
	v.mu.Unlock()
	t.Cleanup(v.org.Close)
}

func (g fakeGuest) OpenExec(context.Context, guestapi.ExecSpec) (net.Conn, error) {
	return nil, errors.New("fake guest: no exec stream")
}

func (g fakeGuest) OpenPTY(context.Context, guestapi.ExecSpec) (net.Conn, error) {
	if g.m.vm.ptys == nil {
		return nil, errors.New("fake guest: no pty")
	}
	host, guest := net.Pipe()
	g.m.vm.ptys <- guest
	return host, nil
}

func (g fakeGuest) Shutdown(context.Context) error {
	if g.m.vm.agentDown {
		return errors.New("fake guest: agent unreachable")
	}
	if g.m.vm.slowShutdown {
		return nil
	}
	g.m.stop(true)
	return nil
}

// Exec understands the handful of commands the supervisor runs.
func (g fakeGuest) Exec(ctx context.Context, spec guestapi.ExecSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	g.m.vm.mu.Lock()
	gate := g.m.vm.execEntered
	g.m.vm.execEntered = nil
	g.m.vm.mu.Unlock()
	if gate != nil {
		close(gate)
		<-ctx.Done()
		return -1, ctx.Err()
	}
	if !g.m.running() {
		return -1, errors.New("fake guest: VM stopped")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	a := spec.Argv
	rel := func(p string) string { return strings.TrimPrefix(p, guestapi.DataDir+"/") }
	switch {
	case a[0] == "sh" && strings.Contains(a[2], "cat"):
		v, ok := g.file(rel(a[4]))
		if !ok {
			return 3, nil
		}
		_, _ = io.WriteString(stdout, v)
		return 0, nil
	case a[0] == "cp":
		return 0, g.setFile(rel(a[2]), "chart baked "+path.Base(a[1]))
	case strings.Join(a[:3], " ") == "k3s ctr -n":
		g.m.vm.mu.Lock()
		var refs []string
		for r := range g.m.vm.imported {
			refs = append(refs, r)
		}
		g.m.vm.mu.Unlock()
		sort.Strings(refs)
		_, _ = io.WriteString(stdout, strings.Join(refs, "\n")+"\n")
		return 0, nil
	case a[0] == "k3s" && a[1] == "kubectl":
		return g.kubectl(a[2:], stdin, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "fake guest: unknown command %q", a)
	return 127, nil
}

func (g fakeGuest) kubectl(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	joined := strings.Join(args, " ")
	vm := g.m.vm
	switch {
	case slices.Contains(args, readSettingsScript):
		vm.mu.Lock()
		defer vm.mu.Unlock()
		if vm.claudeSettings == nil {
			return 3, nil
		}
		_, _ = io.WriteString(stdout, *vm.claudeSettings)
		return 0, nil
	case slices.Contains(args, writeSettingsScript):
		b, err := io.ReadAll(stdin)
		if err != nil {
			return -1, err
		}
		vm.mu.Lock()
		defer vm.mu.Unlock()
		s := string(b)
		vm.claudeSettings = &s
		vm.settingsWrites = append(vm.settingsWrites, joined)
		return 0, nil
	case strings.Contains(joined, "exec") && strings.Contains(joined, " claude -p "):
		model := args[slices.Index(args, "--model")+1]
		vm.mu.Lock()
		defer vm.mu.Unlock()
		vm.probed = append(vm.probed, joined)
		if out, ok := vm.probes[model]; ok {
			_, _ = io.WriteString(stdout, out)
			return 1, nil
		}
		_, _ = io.WriteString(stdout, `{"type":"result","is_error":false,"result":"OK"}`)
		return 0, nil
	case strings.Contains(joined, "get job helm-install-kivali"):
		_, _ = fmt.Fprintf(stdout, "%d", g.m.vm.helmFails)
		return 0, nil
	case strings.Contains(joined, "logs job/helm-install-kivali"):
		_, _ = io.WriteString(stdout, "+ helm install kivali\nError: INSTALLATION FAILED: duplicate entries\n  .env: duplicate key\n+ exit\n")
		return 0, nil
	case strings.Contains(joined, "get deployment kivali"):
		manifest, ok := g.file(ManifestRel)
		if !ok || g.m.vm.helmFails > 0 {
			_, _ = io.WriteString(stderr, `Error from server (NotFound): deployments.apps "kivali" not found`)
			return 1, nil
		}
		file, vals, err := ParseManifest([]byte(manifest))
		if err != nil {
			return -1, err
		}
		image := vals["image"].(map[string]any)
		ref := fmt.Sprintf("%s:%s", image["repository"], image["tag"])
		if !strings.Contains(file, "0.15.0") {
			ref += g.m.vm.renderSuffix
		}
		ready := int32(1)
		if g.m.vm.broken[fmt.Sprint(image["tag"])] {
			ready = 0
		}
		g.m.vm.mu.Lock()
		gen := 1 + g.m.vm.manifestWrites
		g.m.vm.mu.Unlock()
		d := map[string]any{
			"metadata": map[string]any{"generation": gen, "labels": map[string]string{"helm.sh/chart": strings.TrimSuffix(file, ".tgz")}},
			"spec": map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{
				"containers": []map[string]string{{"name": "egress-proxy", "image": "x"}, {"name": "kivali", "image": ref}},
			}}},
			"status": map[string]any{"observedGeneration": gen, "replicas": 1, "updatedReplicas": 1, "readyReplicas": ready, "availableReplicas": ready},
		}
		return 0, json.NewEncoder(stdout).Encode(d)
	case strings.Contains(joined, "get pods -l app=kivali"):
		manifest, _ := g.file(ManifestRel)
		_, vals, _ := ParseManifest([]byte(manifest))
		var statuses []map[string]any
		if image, ok := vals["image"].(map[string]any); ok && g.m.vm.broken[fmt.Sprint(image["tag"])] {
			statuses = append(statuses, map[string]any{"name": "kivali", "state": map[string]any{
				"waiting": map[string]string{"reason": "ImagePullBackOff", "message": "pull access denied"}}})
		}
		return 0, json.NewEncoder(stdout).Encode(map[string]any{"items": []any{map[string]any{
			"metadata": map[string]string{"name": "kivali-1"},
			"status":   map[string]any{"containerStatuses": statuses},
		}}})
	case strings.Contains(joined, "delete pod"), strings.Contains(joined, "rollout restart"):
		return 0, nil
	case strings.Contains(joined, "exec") && strings.HasSuffix(joined, "claude auth status --json"):
		if g.m.vm.authStatus == "" {
			_, _ = io.WriteString(stdout, `{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`)
			return 1, nil
		}
		_, _ = io.WriteString(stdout, g.m.vm.authStatus)
		return 0, nil
	}
	_, _ = fmt.Fprintf(stderr, "fake kubectl: unknown %q", joined)
	return 1, nil
}

type okReadyz struct{}

func (okReadyz) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path != "/readyz" {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
}

// harness is one config directory with the fakes around a Supervisor.
type harness struct {
	t   *testing.T
	dir string
	// home is the supervisor's Options.Home: a directory of its own, so
	// that a path outside both it and dir is outside "home" whatever the
	// OS keeps its temp directories under (on Windows they are inside the
	// real home directory).
	home  string
	vm    *fakeBackend
	clock *fakeClock
	// snapshot replaces the host's Snapshot when set.
	snapshot func(src, dst string) error
	sup      *Supervisor
	port     int

	// terminals, when set, receives the terminal count at every change.
	terminals chan int

	mu sync.Mutex
	// renamed and freeDirs record the host's Rename targets and
	// FreeBytes directories.
	renamed, freeDirs []string
	// renameErr, when set, is asked about every Rename first; an error
	// fails it.
	renameErr func(oldpath, newpath string) error
}

// testHost is the real host of the OS running the tests, with Snapshot
// replaceable.
type testHost struct {
	Host
	h *harness
}

func (t testHost) Snapshot(src, dst string) error {
	if t.h.snapshot != nil {
		return t.h.snapshot(src, dst)
	}
	return t.Host.Snapshot(src, dst)
}

// Rename and FreeBytes record their calls on the harness.
func (t testHost) Rename(oldpath, newpath string) error {
	t.h.mu.Lock()
	t.h.renamed = append(t.h.renamed, newpath)
	fail := t.h.renameErr
	t.h.mu.Unlock()
	if fail != nil {
		if err := fail(oldpath, newpath); err != nil {
			return err
		}
	}
	return t.Host.Rename(oldpath, newpath)
}

func (t testHost) FreeBytes(dir string) (uint64, error) {
	t.h.mu.Lock()
	t.h.freeDirs = append(t.h.freeDirs, dir)
	t.h.mu.Unlock()
	return t.Host.FreeBytes(dir)
}

func (h *harness) dataPath() string { return h.vm.DataDiskPath(h.dir) }
func (h *harness) snapPath() string { return h.dataPath() + SnapshotSuffix }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), home: t.TempDir(), vm: newFakeBackend(), clock: &fakeClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, port: freePort(t)}
	h.restart()
	// Registered after t.TempDir, so it runs before the directories are
	// removed.
	t.Cleanup(h.stopMachines)
	return h
}

// stopMachines hard-stops every VM the fake backend booted, so that a test
// may end with the VM running, and checks that the supervisor then closes
// the console log it opened for it. The supervisor closes it on a goroutine
// of its own once the machine is done, and Windows will not delete an open
// file, so without the stop TempDir's cleanup fails on logs/console.log.
func (h *harness) stopMachines() {
	h.vm.mu.Lock()
	machines := append([]*fakeMachine(nil), h.vm.machines...)
	h.vm.mu.Unlock()
	for i, m := range machines {
		_ = m.HardStop()
		<-m.Done()
		// A zero-length write meets the closed check before anything else.
		closed := func() bool {
			_, err := m.cfg.ConsoleLog.Write(nil)
			return errors.Is(err, os.ErrClosed)
		}
		for deadline := time.Now().Add(5 * time.Second); !closed() && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		if !closed() {
			h.t.Errorf("the console log of VM %d is still open after it stopped", i+1)
			if c, ok := m.cfg.ConsoleLog.(io.Closer); ok {
				_ = c.Close()
			}
		}
	}
}

// restart simulates a new serve process on the same config directory.
func (h *harness) restart() {
	h.t.Helper()
	if h.sup != nil {
		h.sup.mu.Lock()
		if h.sup.fwd != nil {
			h.sup.fwd.Close()
		}
		h.sup.mu.Unlock()
	}
	sup, err := New(Options{
		ConfigDir: h.dir,
		Home:      h.home,
		VMDir:     "/vm",
		Backend:   h.vm,
		Host:      testHost{Host: host.Default(), h: h},
		GuestFor:  func(m Machine) Guest { return fakeGuest{m.(*fakeMachine)} },
		Clock:     h.clock,
		HTTP:      &http.Client{Transport: okReadyz{}},
		Version:   "0.15.0",
		Models:    testModels,
		terminalsChanged: func(n int) {
			if h.terminals != nil {
				h.terminals <- n
			}
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.sup = sup
	h.t.Cleanup(func() {
		sup.mu.Lock()
		if sup.fwd != nil {
			sup.fwd.Close()
		}
		sup.mu.Unlock()
	})
}

func (h *harness) logf() Logf {
	return func(format string, args ...any) { h.t.Logf(format, args...) }
}

func (h *harness) up(o UpOptions) error {
	if o.Port == 0 {
		o.Port = h.port
	}
	return h.sup.Up(context.Background(), o, h.logf())
}

func (h *harness) mustUp(o UpOptions) {
	h.t.Helper()
	if err := h.up(o); err != nil {
		h.t.Fatalf("up: %v", err)
	}
}

func (h *harness) disk() diskFS {
	h.t.Helper()
	fs, err := readDisk(h.dataPath())
	if err != nil {
		h.t.Fatal(err)
	}
	return fs
}

func (h *harness) manifestValues() (string, map[string]any) {
	h.t.Helper()
	file, vals, err := ParseManifest([]byte(h.disk()[ManifestRel]))
	if err != nil {
		h.t.Fatal(err)
	}
	return file, vals
}

func writeTarTo(w io.Writer, files map[string]string) error {
	tw := tar.NewWriter(w)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return err
		}
	}
	return tw.Close()
}

// writeRelease writes release.json, a chart and an image bundle with
// the three Kivali images at tag into dir and returns the feed path.
func writeRelease(t *testing.T, dir, version, tag string) string {
	t.Helper()
	return writeReleaseChart(t, dir, version, version, tag)
}

// writeReleaseChart is writeRelease with a chart whose Chart.yaml
// version may differ from release.json's.
func writeReleaseChart(t *testing.T, dir, version, chartVersion, tag string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var chart bytes.Buffer
	z := gzip.NewWriter(&chart)
	if err := writeTarTo(z, map[string]string{"kivali/Chart.yaml": "apiVersion: v2\nname: kivali\nversion: " + chartVersion + "\nappVersion: v" + chartVersion + "\n"}); err != nil {
		t.Fatal(err)
	}
	_ = z.Close()
	var bundle bytes.Buffer
	if err := writeTarTo(&bundle, map[string]string{"manifest.json": fmt.Sprintf(`[{"RepoTags":["kivali:%[1]s","kivali-egress-proxy:%[1]s","kivali-dev-shell:%[1]s"]}]`, tag)}); err != nil {
		t.Fatal(err)
	}
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	chartName := "kivali-" + version + ".tgz"
	bundleName := "kivali-images-linux-" + fakeArch + ".tar"
	for name, b := range map[string][]byte{chartName: chart.Bytes(), bundleName: bundle.Bytes()} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rel := Release{
		Version: version,
		Chart:   Asset{Name: chartName, SHA256: sum(chart.Bytes())},
		Images:  map[string]Asset{"linux/" + fakeArch: {Name: bundleName, SHA256: sum(bundle.Bytes())}},
	}
	b, _ := json.Marshal(rel)
	feed := filepath.Join(dir, "release.json")
	if err := os.WriteFile(feed, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return feed
}
