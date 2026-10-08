package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// stageLog records the stage of every line an operation logs.
type stageLog struct {
	h      *stageHolder
	stages []string
	lines  []string
}

func newStageLog() (context.Context, *stageLog) {
	ctx, h := withStage(context.Background())
	return ctx, &stageLog{h: h}
}

func (l *stageLog) logf(format string, args ...any) {
	l.stages = append(l.stages, l.h.get())
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// serveLog records the serve log, which the VM's watcher also writes.
type serveLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *serveLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *serveLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// sequence is the stages in order, each run of one stage once.
func (l *stageLog) sequence() []string {
	return slices.Compact(slices.Clone(l.stages))
}

func TestUpStages(t *testing.T) {
	h := newHarness(t)
	ctx, l := newStageLog()
	if err := h.sup.Up(ctx, UpOptions{Install: owner, Port: h.port}, l.logf); err != nil {
		t.Fatal(err)
	}
	if got, want := l.sequence(), []string{StageMakingRoom, StageStarting, StageSettingUp}; !slices.Equal(got, want) {
		t.Fatalf("fresh up stages %v, want %v\n%s", got, want, strings.Join(l.lines, "\n"))
	}
	if l.lines[0] != fmt.Sprintf("creating the data disk %s (%d GiB)", h.dataPath(), DataDiskSize>>30) || l.stages[0] != StageMakingRoom {
		t.Fatalf("first line %q (%s)", l.lines[0], l.stages[0])
	}
	for i, line := range l.lines {
		if strings.HasPrefix(line, "guest agent ") && l.stages[i] != StageStarting {
			t.Fatalf("%q in stage %s", line, l.stages[i])
		}
		if strings.HasPrefix(line, "HelmChart ") && l.stages[i] != StageSettingUp {
			t.Fatalf("%q in stage %s", line, l.stages[i])
		}
	}

	ctx, l = newStageLog()
	if err := h.sup.Down(ctx, l.logf); err != nil {
		t.Fatal(err)
	}
	if got := l.sequence(); !slices.Equal(got, []string{StagePausing}) {
		t.Fatalf("down stages %v", got)
	}
	ctx, l = newStageLog()
	if err := h.sup.Up(ctx, UpOptions{}, l.logf); err != nil {
		t.Fatal(err)
	}
	if got, want := l.sequence(), []string{StageStarting, StageSettingUp}; !slices.Equal(got, want) {
		t.Fatalf("installed up stages %v, want %v\n%s", got, want, strings.Join(l.lines, "\n"))
	}
}

// A prepared up boots and forwards a fresh org without an owner and
// installs nothing; the next up, with the owner, installs into the VM
// already running.
func TestUpPrepareThenInstall(t *testing.T) {
	h := newHarness(t)
	ctx, l := newStageLog()
	if err := h.sup.Up(ctx, UpOptions{Port: h.port, Prepare: true}, l.logf); err != nil {
		t.Fatal(err)
	}
	if got, want := l.sequence(), []string{StageMakingRoom, StageStarting, StageSettingUp}; !slices.Equal(got, want) {
		t.Fatalf("prepare stages %v, want %v", got, want)
	}
	if last := l.lines[len(l.lines)-1]; !strings.Contains(last, "installs once its owner is known") {
		t.Fatalf("last line %q", last)
	}
	if h.sup.State().Kivali != "" {
		t.Fatalf("prepare installed %q", h.sup.State().Kivali)
	}
	ctx, l = newStageLog()
	if err := h.sup.Up(ctx, UpOptions{Install: owner}, l.logf); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(l.lines, "the VM is already running") {
		t.Fatalf("the install booted again:\n%s", strings.Join(l.lines, "\n"))
	}
	if h.sup.State().Kivali == "" {
		t.Fatal("the second up did not install")
	}
}

func TestUpgradeStages(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	ctx, l := newStageLog()
	if err := h.sup.Upgrade(ctx, UpgradeOptions{Feed: feed}, l.logf); err != nil {
		t.Fatal(err)
	}
	want := []string{StageDownloading, StageSnapshot, StageInstalling, StageStarting}
	if got := l.sequence(); !slices.Equal(got, want) {
		t.Fatalf("upgrade stages %v, want %v\n%s", got, want, strings.Join(l.lines, "\n"))
	}

	// A broken release rolls back.
	h.vm.broken["v0.17.0"] = true
	feed = writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.17.0", "v0.17.0")
	ctx, l = newStageLog()
	if err := h.sup.Upgrade(ctx, UpgradeOptions{Feed: feed}, l.logf); err == nil {
		t.Fatal("broken upgrade succeeded")
	}
	got := l.sequence()
	if got[len(got)-1] != StageRollingBack || !slices.Contains(got, StageInstalling) {
		t.Fatalf("rollback stages %v", got)
	}
}

// Over the RPC every log line carries the stage, and the serve log shows
// it in brackets.
func TestRPCEventsCarryTheStage(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	served := &serveLog{}
	h.sup.o.Logf = served.logf
	srv := httptest.NewServer(NewRPCServer(h.sup).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/down", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	logs := 0
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Done {
			if ev.Error != "" || ev.Stage != "" {
				t.Fatalf("final event %+v", ev)
			}
			break
		}
		logs++
		if ev.Stage != StagePausing {
			t.Fatalf("event %+v", ev)
		}
	}
	if logs == 0 {
		t.Fatal("no log lines")
	}
	if !slices.Contains(served.get(), "[pausing] clean shutdown requested through the guest agent") {
		t.Fatalf("serve log %q", served.get())
	}
}

func TestTerminalSessionsAreCounted(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.ptys = make(chan net.Conn, 1)
	h.terminals = make(chan int, 4)
	srv := httptest.NewServer(NewRPCServer(h.sup).Handler())
	defer srv.Close()
	ctx := context.Background()
	if r := h.sup.Status(ctx); r.Terminals != 0 {
		t.Fatalf("terminals before %d", r.Terminals)
	}
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := guestapi.Upgrade(ctx, conn, "/v1/terminal?rows=24&cols=80")
	if err != nil {
		t.Fatal(err)
	}
	guest := <-h.vm.ptys
	if n := <-h.terminals; n != 1 {
		t.Fatalf("count %d on attach", n)
	}
	if r := h.sup.Status(ctx); r.Terminals != 1 {
		t.Fatalf("status terminals %d", r.Terminals)
	}
	_ = s.Close()
	_, _ = io.Copy(io.Discard, guest) // the splice closes the guest end
	if n := <-h.terminals; n != 0 {
		t.Fatalf("count %d after close", n)
	}
	if r := h.sup.Status(ctx); r.Terminals != 0 {
		t.Fatalf("status terminals %d after close", r.Terminals)
	}

	// A terminal that never attaches is never counted.
	h.vm.ptys = nil
	conn, err = net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guestapi.Upgrade(ctx, conn, "/v1/terminal?rows=24&cols=80"); err == nil {
		t.Fatal("terminal attached without a pty")
	}
	if r := h.sup.Status(ctx); r.Terminals != 0 {
		t.Fatalf("failed attach counted: %d", r.Terminals)
	}
}

func TestStatusReportsDiskUsage(t *testing.T) {
	h := newHarness(t)
	if r := h.sup.Status(context.Background()); r.DiskUsedBytes != 0 || r.DiskSizeBytes != 0 {
		t.Fatalf("no disk yet: %d/%d", r.DiskUsedBytes, r.DiskSizeBytes)
	}
	h.mustUp(UpOptions{Install: owner})
	fi, err := os.Stat(h.dataPath())
	if err != nil {
		t.Fatal(err)
	}
	r := h.sup.Status(context.Background())
	if r.DiskSizeBytes != fi.Size() || r.DiskUsedBytes <= 0 {
		t.Fatalf("disk %d/%d, file size %d", r.DiskUsedBytes, r.DiskSizeBytes, fi.Size())
	}
}

func TestPortHintOnlyOnAFreshOrg(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A save before the first up (a check records its outcome) does not
	// count as a recorded port.
	if err := h.sup.update(func(*State) {}); err != nil {
		t.Fatal(err)
	}
	if err := h.sup.Up(ctx, UpOptions{Install: owner, PortHint: h.port}, h.logf()); err != nil {
		t.Fatal(err)
	}
	if got := h.sup.State().Port; got != h.port {
		t.Fatalf("port %d, want the hint %d", got, h.port)
	}
	if err := h.sup.Down(ctx, h.logf()); err != nil {
		t.Fatal(err)
	}
	// A recorded port wins over a later hint.
	other := freePort(t)
	h.restart()
	if err := h.sup.Up(ctx, UpOptions{PortHint: other}, h.logf()); err != nil {
		t.Fatal(err)
	}
	if got := h.sup.State().Port; got != h.port {
		t.Fatalf("port %d after a second hint, want %d", got, h.port)
	}
}

func TestPortHintMovesWhenBusy(t *testing.T) {
	h := newHarness(t)
	squatter, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(h.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = squatter.Close() }()
	if err := h.sup.Up(context.Background(), UpOptions{Install: owner, PortHint: h.port}, h.logf()); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := h.sup.State().Port; got == h.port || got == 0 {
		t.Fatalf("port %d: the busy hint was kept", got)
	}
}

func TestDestroyRemovesExactlyItsFiles(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(h.dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// What a past upgrade, serve and a crash left, and what is not ours.
	ours := []string{"downloads/0.16.0/kivali-0.16.0.tgz", "logs/supervisor.log", ".tmp-local.json-123"}
	keep := []string{"supervisor.sock", "serve.lock", "shell.json", "logs/notes.txt"}
	for _, f := range append(slices.Clone(ours), keep...) {
		write(f)
	}
	if err := os.WriteFile(h.snapPath(), []byte("snap"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, l := newStageLog()
	res, err := h.sup.Destroy(ctx, l.logf)
	if err != nil {
		t.Fatal(err)
	}
	if res.FreedBytes <= 0 {
		t.Fatalf("freed %d", res.FreedBytes)
	}
	if got := l.sequence(); !slices.Equal(got, []string{StageDeleting}) {
		t.Fatalf("stages %v", got)
	}
	if !slices.Contains(l.lines, "deleting the data disk "+h.dataPath()) {
		t.Fatalf("lines %q", l.lines)
	}
	if m, _ := h.sup.current(); m != nil {
		t.Fatal("VM still running")
	}
	gone := append([]string{"local.json", "logs/console.log", "downloads", filepath.Dir(h.dataPath())}, ours...)
	for _, f := range gone {
		p := f
		if !filepath.IsAbs(p) {
			p = filepath.Join(h.dir, f)
		}
		if exists(p) {
			t.Errorf("%s is still there", f)
		}
	}
	for _, f := range keep {
		if !exists(filepath.Join(h.dir, f)) {
			t.Errorf("%s was deleted", f)
		}
	}

	// The org starts over: a fresh disk, formatted, installed again.
	h.vm.created = nil
	h.mustUp(UpOptions{Install: owner})
	if len(h.vm.created) != 1 || !h.vm.lastFormat() {
		t.Fatalf("after destroy: created %v, format %v", h.vm.created, h.vm.lastFormat())
	}
}

// destroy removes a VM that outlives serve through the backend, once,
// after the stop and while the data disk is still there to identify it.
func TestDestroyRemovesTheMachine(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	ctx, l := newStageLog()
	if _, err := h.sup.Destroy(ctx, l.logf); err != nil {
		t.Fatal(err)
	}
	want := []machineRemoval{{configDir: h.dir, allStopped: true, diskThere: true}}
	if got := h.vm.machineRemovals(); !slices.Equal(got, want) {
		t.Fatalf("removals %+v, want %+v", got, want)
	}
	if !slices.Contains(l.lines, "removing the VM") {
		t.Fatalf("lines %q", l.lines)
	}
}

// A serve that never booted the VM (a new process, the VM off) still
// removes it: the VM is the host's, not the process's.
func TestDestroyRemovesAMachineThisServeNeverBooted(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if _, err := h.sup.Destroy(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	if got := h.vm.machineRemovals(); len(got) != 1 || got[0].configDir != h.dir || !got[0].diskThere {
		t.Fatalf("removals %+v", got)
	}
}

// A remover that fails does not stop the destroy: a line says what is
// left, and the files go all the same.
func TestDestroyGoesOnWhenTheMachineRemovalFails(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.mu.Lock()
	h.vm.removeErr = errors.New("broker: the VM could not be removed (500)")
	h.vm.mu.Unlock()
	ctx, l := newStageLog()
	if _, err := h.sup.Destroy(ctx, l.logf); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !slices.Contains(l.lines, "remove the VM: broker: the VM could not be removed (500)") {
		t.Fatalf("lines %q", l.lines)
	}
	if got := h.vm.machineRemovals(); len(got) != 1 {
		t.Fatalf("removals %+v", got)
	}
	for _, p := range []string{h.dataPath(), filepath.Join(h.dir, "local.json")} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
}

func TestDestroyRefusesWithAJournal(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	if err := h.sup.update(func(st *State) {
		st.Upgrade = &Journal{From: "0.15.0", To: "0.16.0", Step: StepApplying, Snapshot: h.snapPath(), SnapshotComplete: true}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := h.sup.Destroy(context.Background(), h.logf())
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("destroy with a journal: %v", err)
	}
	if !exists(h.dataPath()) || !exists(filepath.Join(h.dir, "local.json")) {
		t.Fatal("destroy deleted something while refusing")
	}
	if m, _ := h.sup.current(); m == nil {
		t.Fatal("destroy stopped the VM while refusing")
	}
	if got := h.vm.machineRemovals(); len(got) != 0 {
		t.Fatalf("destroy removed the VM while refusing: %+v", got)
	}
}

func TestSetAddressRecordsTheExternalURL(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	ctx := context.Background()
	for _, bad := range []string{"http://a.example", "https://a.example/auth/callback", "https://u@a.example", "a.example", "https://a.example?x=1"} {
		if err := h.sup.SetAddress(ctx, AddressRequest{ExternalURL: bad}, func(string, ...any) {}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	_, before := h.manifestValues()
	sctx, l := newStageLog()
	if err := h.sup.SetAddress(sctx, AddressRequest{ExternalURL: "https://dana-imac.tailnet.ts.net/"}, l.logf); err != nil {
		t.Fatal(err)
	}
	_, v := h.manifestValues()
	if v["externalURL"] != "https://dana-imac.tailnet.ts.net" {
		t.Fatalf("externalURL = %v", v["externalURL"])
	}
	if getPath(t, before, "secrets", "sessionKey") != getPath(t, v, "secrets", "sessionKey") {
		t.Error("the session key changed")
	}
	if !slices.Contains(l.lines, "external address set to https://dana-imac.tailnet.ts.net") {
		t.Fatalf("lines %q", l.lines)
	}
	// The same address again changes nothing.
	sctx, l = newStageLog()
	if err := h.sup.SetAddress(sctx, AddressRequest{ExternalURL: "https://dana-imac.tailnet.ts.net"}, l.logf); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(l.lines, "the external address is unchanged") {
		t.Fatalf("lines %q", l.lines)
	}
	if err := h.sup.SetAddress(ctx, AddressRequest{}, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if _, v := h.manifestValues(); v["externalURL"] != nil {
		t.Fatalf("externalURL not cleared: %v", v["externalURL"])
	}
}

// The credential is what `claude auth status --json` reports in the
// server container: signed out (exit 1, the same JSON), then each kind
// of sign-in, in the words the desktop shows.
func TestCredentialAsksTheCLI(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	ctx := context.Background()
	st, err := h.sup.Credential(ctx)
	if err != nil || st.SignedIn || st.Email != "" || st.Billing != "" || st.CheckedAt == "" {
		t.Fatalf("signed out %+v, %v", st, err)
	}
	for _, tc := range []struct {
		out            string
		email, billing string
	}{
		{`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","email":"owner@example.com","orgName":"o","subscriptionType":"max"}`, "owner@example.com", "Claude Max"},
		{`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","apiKeySource":"/login managed key","email":"ops@example.com","orgName":"Ex","subscriptionType":null}`, "ops@example.com", "Anthropic Console"},
		{`{"loggedIn":true,"authMethod":"third_party","apiProvider":"bedrock"}`, "", "Amazon Bedrock"},
		{`{"loggedIn":true,"authMethod":"third_party","apiProvider":"vertex"}`, "", "Google Vertex AI"},
	} {
		h.vm.authStatus = tc.out
		st, err := h.sup.Credential(ctx)
		if err != nil || !st.SignedIn || st.Email != tc.email || st.Billing != tc.billing {
			t.Errorf("%s: %+v, %v", tc.out, st, err)
		}
	}
	// Output that is not the CLI's JSON is an error, and is not echoed.
	h.vm.authStatus = "token=SECRET"
	if _, err := h.sup.Credential(ctx); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("garbage: %v", err)
	}
}

func TestCredentialRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var nr *NotReadyError
	if _, err := h.sup.Credential(ctx); !errors.As(err, &nr) {
		t.Fatalf("credential with no VM: %v", err)
	}
	srv := httptest.NewServer(NewRPCServer(h.sup).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/credential")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "not running") {
		t.Fatalf("GET /v1/credential: %s %q", resp.Status, body)
	}

	h.mustUp(UpOptions{Install: owner})
	// Another operation holds the lock: the status still answers.
	h.sup.op <- struct{}{}
	h.sup.mu.Lock()
	h.sup.opName = "upgrade"
	h.sup.mu.Unlock()
	// It never takes the lock; were it to wait, this would hang.
	if _, err := h.sup.Credential(ctx); err != nil {
		t.Fatalf("status during an upgrade: %v", err)
	}
	h.sup.mu.Lock()
	h.sup.opName = ""
	h.sup.mu.Unlock()
	<-h.sup.op
}

// The JSON the shell's wire.rs (desktop_additions_match_the_supervisor)
// reads and sends.
func TestWireShapes(t *testing.T) {
	enc := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := enc(Event{Log: "creating the data disk", Stage: StageMakingRoom}); got != `{"log":"creating the data disk","stage":"making-room"}` {
		t.Errorf("event %s", got)
	}
	r := enc(Report{Terminals: 1, DiskUsedBytes: 3 << 30, DiskSizeBytes: 64 << 30})
	for _, want := range []string{`"terminals":1`, `"disk_used_bytes":3221225472`, `"disk_size_bytes":68719476736`} {
		if !strings.Contains(r, want) {
			t.Errorf("report %s lacks %s", r, want)
		}
	}
	if got := enc(CredentialStatus{SignedIn: true, Email: "owner@example.com", Billing: "Claude Max", CheckedAt: "2026-10-01T12:00:00Z"}); got != `{"signed_in":true,"email":"owner@example.com","billing":"Claude Max","checked_at":"2026-10-01T12:00:00Z"}` {
		t.Errorf("credential %s", got)
	}
	if got := enc(CredentialStatus{CheckedAt: "2026-10-01T12:00:00Z"}); got != `{"signed_in":false,"checked_at":"2026-10-01T12:00:00Z"}` {
		t.Errorf("credential %s", got)
	}
	if got := enc(DestroyResult{FreedBytes: 3 << 30}); got != `{"freed_bytes":3221225472}` {
		t.Errorf("destroy result %s", got)
	}
	if got := enc(HandoffResult{Token: "p.s"}); got != `{"token":"p.s"}` {
		t.Errorf("handoff result %s", got)
	}
	var up UpOptions
	if err := json.Unmarshal([]byte(`{"install":{"env":["A=b"]},"port_hint":18081,"memory_mb":6144,"cpus":2}`), &up); err != nil {
		t.Fatal(err)
	}
	if up.PortHint != 18081 || up.MemoryMB != 6144 || up.CPUs != 2 || !slices.Equal(up.Install.Env, []string{"A=b"}) {
		t.Errorf("up %+v", up)
	}
	var dr DestroyRequest
	if err := json.Unmarshal([]byte(`{"exit":true}`), &dr); err != nil || !dr.Exit {
		t.Errorf("destroy request %+v %v", dr, err)
	}
}
