package agentshell

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/clock"
)

func TestInstallNoticeIsTakenOnce(t *testing.T) {
	inst := NewInstall([]string{"jq", "nosuch"})
	if n := inst.TakeNotice(); n != "" {
		t.Errorf("notice while running = %q", n)
	}
	inst.Finish(InstallResult{Installed: []string{"jq"}, Failed: []PackageFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}}})
	inst.Finish(InstallResult{Installed: []string{"jq", "nosuch"}}) // only the first counts
	n := inst.TakeNotice()
	if !strings.Contains(n, "nosuch (Unable to locate package nosuch)") {
		t.Errorf("notice = %q", n)
	}
	if again := inst.TakeNotice(); again != "" {
		t.Errorf("second notice = %q, want none", again)
	}

	ok := NewInstall([]string{"jq"})
	ok.Finish(InstallResult{Installed: []string{"jq"}})
	if n := ok.TakeNotice(); n != "" {
		t.Errorf("notice with nothing failed = %q", n)
	}

	var none *Install
	if !none.Finished() || none.TakeNotice() != "" || none.Requested() != nil {
		t.Error("a nil Install is not a finished, empty one")
	}
}

func TestFailedInstall(t *testing.T) {
	inst := FailedInstall([]string{"a1", "b2"}, reasonNotRoot)
	installing, installed, failed := inst.State()
	if installing != nil || len(installed) != 0 {
		t.Errorf("installing = %v, installed = %v", installing, installed)
	}
	if want := []PackageFailure{{Name: "a1", Error: reasonNotRoot}, {Name: "b2", Error: reasonNotRoot}}; !reflect.DeepEqual(failed, want) {
		t.Errorf("failed = %v, want %v", failed, want)
	}
}

// installHandler is the daemon's handler with a running install of jq
// and nosuch, on a fake clock.
func installHandler(t *testing.T) (*httptest.Server, *Install, *clock.Fake) {
	t.Helper()
	inst := NewInstall([]string{"jq", "nosuch"})
	clk := clock.NewFake()
	srv := httptest.NewServer(NewHandler(ServerConfig{
		Root:     t.TempDir(),
		Packages: []string{"bash", "zip"},
		Install:  inst,
		Clock:    clk,
	}))
	t.Cleanup(srv.Close)
	return srv, inst, clk
}

func postExec(t *testing.T, url, command string) ExecResponse {
	t.Helper()
	body, _ := json.Marshal(ExecRequest{Command: command, TimeoutSeconds: 5})
	resp, err := http.Post(url+"/v1/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Errorf("POST /v1/exec: %v", err)
		return ExecResponse{}
	}
	defer func() { _ = resp.Body.Close() }()
	var out ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Errorf("decode: %v", err)
	}
	return out
}

func getPackages(t *testing.T, url string) PackagesResponse {
	t.Helper()
	resp, err := http.Get(url + "/v1/packages")
	if err != nil {
		t.Errorf("GET /v1/packages: %v", err)
		return PackagesResponse{}
	}
	defer func() { _ = resp.Body.Close() }()
	var out PackagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Errorf("decode: %v", err)
	}
	return out
}

// A command waits for the install, then runs; the first response after
// an install with failures carries the notice, the next does not.
func TestExecWaitsForTheInstallAndCarriesTheNoticeOnce(t *testing.T) {
	srv, inst, clk := installHandler(t)

	got := make(chan ExecResponse, 1)
	go func() { got <- postExec(t, srv.URL, "echo ran") }()
	clk.BlockUntil(1) // the handler is waiting on the install
	select {
	case r := <-got:
		t.Fatalf("command answered before the install finished: %+v", r)
	default:
	}
	inst.Finish(InstallResult{Installed: []string{"jq"}, Failed: []PackageFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}}})
	first := <-got
	if first.ExitCode != 0 || !strings.Contains(first.Stdout, "ran") {
		t.Errorf("first = %+v, want the command to have run", first)
	}
	if !strings.Contains(first.Notice, "nosuch (Unable to locate package nosuch)") {
		t.Errorf("first notice = %q", first.Notice)
	}
	if second := postExec(t, srv.URL, "echo again"); second.Notice != "" {
		t.Errorf("second notice = %q, want none", second.Notice)
	}
}

// An install that outlasts the exec wait gets a plain answer, before
// the client would give up.
func TestExecAnswersWhileStillInstalling(t *testing.T) {
	srv, _, clk := installHandler(t)
	got := make(chan ExecResponse, 1)
	go func() { got <- postExec(t, srv.URL, "echo ran") }()
	clk.BlockUntil(1)
	clk.Advance(DefaultExecInstallWait)
	r := <-got
	if r.ExitCode != -1 || !strings.Contains(r.Err, "still installing software (jq, nosuch)") || strings.Contains(r.Stdout, "ran") {
		t.Errorf("response = %+v, want a still-installing error and no command run", r)
	}
}

// The package list waits for the install up to its bound, then names
// what is still installing; once finished it lists the installed
// packages beside the image's, and the failures.
func TestPackagesReportsTheInstall(t *testing.T) {
	srv, inst, clk := installHandler(t)

	got := make(chan PackagesResponse, 1)
	go func() { got <- getPackages(t, srv.URL) }()
	clk.BlockUntil(1)
	clk.Advance(DefaultPackagesInstallWait - time.Nanosecond)
	select {
	case r := <-got:
		t.Fatalf("answered before its bound: %+v", r)
	default:
	}
	clk.Advance(time.Nanosecond)
	r := <-got
	if want := []string{"bash", "zip"}; !reflect.DeepEqual(r.Packages, want) {
		t.Errorf("Packages = %v, want %v", r.Packages, want)
	}
	if want := []string{"jq", "nosuch"}; !reflect.DeepEqual(r.Installing, want) {
		t.Errorf("Installing = %v, want %v", r.Installing, want)
	}

	go func() { got <- getPackages(t, srv.URL) }()
	clk.BlockUntil(1)
	inst.Finish(InstallResult{Installed: []string{"jq"}, Failed: []PackageFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}}})
	r = <-got
	if want := []string{"bash", "jq", "zip"}; !reflect.DeepEqual(r.Packages, want) {
		t.Errorf("Packages = %v, want %v", r.Packages, want)
	}
	if r.Installing != nil {
		t.Errorf("Installing = %v, want none", r.Installing)
	}
	if want := []PackageFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}}; !reflect.DeepEqual(r.Failed, want) {
		t.Errorf("Failed = %v, want %v", r.Failed, want)
	}
	// The package list never consumes the exec notice.
	if n := inst.TakeNotice(); n == "" {
		t.Error("GET /v1/packages took the notice")
	}
}

// The file tools never wait for an install.
func TestFilesDoesNotWaitForTheInstall(t *testing.T) {
	srv, _, _ := installHandler(t)
	body, _ := json.Marshal(FilesRequest{Tool: "file_view", Input: json.RawMessage(`{"path":"/files/"}`)})
	resp, err := http.Post(srv.URL+"/v1/files", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/files: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

// The wire: the client reads installing and failed as the daemon
// writes them.
func TestSidecarShellPackagesReadsTheInstall(t *testing.T) {
	sock := shortSock(t)
	inst := FailedInstall([]string{"nosuch"}, "Unable to locate package nosuch")
	stop := startTestDaemon(t, sock, ServerConfig{Root: t.TempDir(), Packages: []string{"bash"}, Install: inst})
	defer stop()
	sw, err := (&SidecarShell{SocketPath: sock}).Packages(t.Context())
	if err != nil {
		t.Fatalf("Packages: %v", err)
	}
	if len(sw.Failed) != 1 || sw.Failed[0].Name != "nosuch" || sw.Failed[0].Error != "Unable to locate package nosuch" {
		t.Errorf("Failed = %+v", sw.Failed)
	}
	if !reflect.DeepEqual(sw.Packages, []string{"bash"}) {
		t.Errorf("Packages = %v", sw.Packages)
	}
}

// The wire: the client carries the notice into the ShellResult.
func TestSidecarShellExecCarriesTheNotice(t *testing.T) {
	sock := shortSock(t)
	inst := FailedInstall([]string{"nosuch"}, "Unable to locate package nosuch")
	stop := startTestDaemon(t, sock, ServerConfig{Root: t.TempDir(), Install: inst})
	defer stop()
	res, err := (&SidecarShell{SocketPath: sock}).Exec(t.Context(), "alice", agent.ShellRequest{Command: "true", TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Notice, "nosuch (Unable to locate package nosuch)") {
		t.Errorf("Notice = %q", res.Notice)
	}
}

// execDirect runs one POST /v1/exec through h in this goroutine, with
// ctx as the request's context.
func execDirect(t *testing.T, h http.Handler, ctx context.Context, req ExecRequest) ExecResponse {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/exec", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out ExecResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v (body %q)", err, w.Body.String())
	}
	return out
}

// The notice is the agent's own: a subagent's command (OverlayPrivate
// set) leaves it for the agent's next command.
func TestExecNoticeSkipsSubagentCommands(t *testing.T) {
	root := t.TempDir()
	inst := FailedInstall([]string{"nosuch"}, "Unable to locate package nosuch")
	h := NewHandler(ServerConfig{Root: root, Install: inst, Clock: clock.NewFake()})
	sub := execDirect(t, h, t.Context(), ExecRequest{Command: "true", TimeoutSeconds: 5, OverlayPrivate: filepath.Join(root, "subagents", "s1")})
	if sub.Notice != "" {
		t.Errorf("subagent notice = %q, want none", sub.Notice)
	}
	own := execDirect(t, h, t.Context(), ExecRequest{Command: "true", TimeoutSeconds: 5})
	if !strings.Contains(own.Notice, "nosuch") {
		t.Errorf("agent notice = %q, want the failure", own.Notice)
	}
}

// A caller that has gone away does not use the notice up: nobody would
// read it.
func TestExecNoticeSkipsAGoneCaller(t *testing.T) {
	inst := FailedInstall([]string{"nosuch"}, "Unable to locate package nosuch")
	h := NewHandler(ServerConfig{Root: t.TempDir(), Install: inst, Clock: clock.NewFake()})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if gone := execDirect(t, h, ctx, ExecRequest{Command: "true", TimeoutSeconds: 5}); gone.Notice != "" {
		t.Errorf("notice to a gone caller = %q", gone.Notice)
	}
	if n := inst.TakeNotice(); !strings.Contains(n, "nosuch") {
		t.Errorf("notice after a gone caller = %q, want it still there", n)
	}
}
