package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: a built-in record outlives the binary that
// carried it (a Kivali that dropped it, a backup restored from one
// that had it). Nothing runs it, so its page must not say Kivali is
// starting it, its Test connection and options must say why they
// cannot run, and the owner must be able to uninstall it; otherwise it
// stays on the team for good.
func TestBuiltinIntegrationNotInThisKivali(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withEcho(t, srv)
	connectEcho(t, srv, "Hello")
	srv.BuiltinIntegrations = builtinintegrations.Registry{}

	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/echo"))
	if page.Summary.State != apitypes.IntegrationStateNotIncluded || !page.Summary.Builtin || !page.CanUninstall || len(page.Requirements) != 0 {
		t.Errorf("page = %+v, can uninstall %v, requirements %v", page.Summary, page.CanUninstall, page.Requirements)
	}
	const why = "Echo is not in this version of Kivali, so nothing runs it; you can uninstall it"
	if e := assertAPIError(t, intPost(t, srv, intBase+"/echo/connection/verify"), http.StatusConflict); e.Error != why || e.Who != whoYou {
		t.Errorf("verify = %+v", e)
	}
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/echo/choices/colors"), http.StatusConflict); e.Error != why {
		t.Errorf("choices = %+v", e)
	}
	if rr := intDelete(t, srv, intBase+"/echo"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := srv.Store.GetIntegration(echotest.Name); err == nil {
		t.Errorf("the record is still there")
	}
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets left: %v", names)
	}
}

// Why this test exists: a backup holds a built-in's record and versions
// as the binary that wrote it had them, or none from a Kivali before
// one shipped. The restore must write this binary's version over them,
// keeping the owner's switch, or the restored team would run, show or
// lack a built-in other than the one this Kivali carries until the
// next restart.
func TestRestoreRegistersBuiltinIntegrations(t *testing.T) {
	src := newTestServer(t)
	withEcho(t, src)
	if rr := intPost(t, src, intBase+"/echo/disable"); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d", rr.Code)
	}
	zipBytes := backupZip(t, src).Body.Bytes()

	dst := newTestServer(t)
	newer := echotest.New()
	newer.Version = "1.1.0"
	dst.BuiltinIntegrations = builtinintegrations.Registry{newer}
	if rr := restoreZip(t, dst, "backup.zip", zipBytes); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	it, err := dst.Store.GetIntegration(echotest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !it.Builtin || it.Enabled || it.CurrentVersion != "1.1.0" {
		t.Errorf("after the restore = %+v", it)
	}
	vs, _ := dst.Store.IntegrationVersions(echotest.Name)
	if len(vs) != 2 || vs[0].State != store.IntegrationVersionApproved || vs[1].State != store.IntegrationVersionSuperseded {
		t.Errorf("versions = %+v", vs)
	}

	// From a Kivali that shipped none, the restore installs it.
	empty := backupZip(t, newTestServer(t)).Body.Bytes()
	dst2 := newTestServer(t)
	dst2.BuiltinIntegrations = builtinintegrations.Registry{echotest.New()}
	if rr := restoreZip(t, dst2, "backup.zip", empty); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	if it, err := dst2.Store.GetIntegration(echotest.Name); err != nil || !it.Builtin || !it.Enabled || it.CurrentVersion != "1.0.0" {
		t.Errorf("after a restore of a backup without it = %+v, %v", it, err)
	}
}

// savingEcho is echo whose Call saves a file through the caller's Save
// (folder, name and bytes from its arguments) and answers with the path,
// naming outside as who wrote it; with the folder "none" it answers
// whether Save was given at all.
type savingEcho struct {
	*echotest.Echo
	outside []string
}

func (e *savingEcho) Call(_ context.Context, caller builtinintegrations.Caller, _ builtinintegrations.Connection, _ string, raw json.RawMessage) (*builtinintegrations.Result, error) {
	var a struct{ Folder, Name, Data string }
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if caller.Save == nil {
		return &builtinintegrations.Result{Content: []string{"no save"}, IsError: true}, nil
	}
	p, err := caller.Save(a.Folder, a.Name, strings.NewReader(a.Data), 16)
	if err != nil {
		return &builtinintegrations.Result{Content: []string{err.Error()}, IsError: true}, nil
	}
	return &builtinintegrations.Result{Content: []string{p}, Audit: "saved " + a.Name, Outside: e.outside}, nil
}

// Why this test exists: a built-in's call saves what it fetched (an
// email's attachment) into the calling agent's files, and names who
// outside the owner's domain wrote what it returned. Core must give the
// call a Save that writes into that agent's own /files/, under the
// integration's untrusted downloads folder, answering the path the
// agent reads it at, with the bytes as given; a file over the limit,
// or one more into a folder already full, must fail in words naming no
// host path, a full folder named as the agent reads it, so the agent
// knows why the save failed; and the audit row must keep
// the outside writers, blanks dropped and at most MaxOutside of them,
// beside the call's own reason, so the approval card can later read
// them back.
func TestBuiltinCallSavesAndNamesOutside(t *testing.T) {
	srv := newTestServer(t)
	many := []string{"alice@example.org", " ", "bob@example.net"}
	for i := range builtinintegrations.MaxOutside {
		many = append(many, fmt.Sprintf("x%d@example.com", i))
	}
	e := &savingEcho{Echo: echotest.New(), outside: many}
	srv.BuiltinIntegrations = builtinintegrations.Registry{e}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	intTeam(t, srv, "dana")
	connectEcho(t, srv, "Hello")
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")

	resp, err := dana.IntegrationCall(context.Background(), agentpod.IntegrationCallRequest{Name: "echo_echo", Arguments: json.RawMessage(`{"folder":"m1","name":"a b.txt","data":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	const want = "/files/integrations/echo/untrusted-downloads/m1/a-b.txt"
	if resp.IsError || len(resp.Content) != 1 || resp.Content[0] != want {
		t.Fatalf("save = %+v", resp)
	}
	onDisk := filepath.Join(srv.Store.Root(), "agents", "dana", "memory", "integrations", "echo", "untrusted-downloads", "m1", "a-b.txt")
	if b, err := os.ReadFile(onDisk); err != nil || string(b) != "hello" {
		t.Errorf("the file = %q, %v", b, err)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("echo", 1)
	if len(rows) != 1 || rows[0].Reason != "saved a b.txt" || len(rows[0].Outside) != builtinintegrations.MaxOutside ||
		rows[0].Outside[0] != "alice@example.org" || rows[0].Outside[1] != "bob@example.net" {
		t.Errorf("audit = %+v", rows)
	}

	resp, _ = dana.IntegrationCall(context.Background(), agentpod.IntegrationCallRequest{Name: "echo_echo", Arguments: json.RawMessage(`{"folder":"m1","name":"big.bin","data":"12345678901234567"}`)})
	if !resp.IsError || resp.Content[0] != "the file is larger than 16 bytes" {
		t.Errorf("a file over the limit = %+v", resp)
	}

	// A folder already holding the most files a folder takes refuses
	// another in words that name it as the agent reads it.
	full := filepath.Join(srv.Store.Root(), "agents", "dana", "memory", "integrations", "echo", "untrusted-downloads", "m2")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range files.MaxIntegrationFolderFiles {
		if err := os.WriteFile(filepath.Join(full, fmt.Sprintf("f%d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resp, _ = dana.IntegrationCall(context.Background(), agentpod.IntegrationCallRequest{Name: "echo_echo", Arguments: json.RawMessage(`{"folder":"m2","name":"new.txt","data":"y"}`)})
	if !resp.IsError || resp.Content[0] != "/files/integrations/echo/untrusted-downloads/m2 is full, holding the 20 files Kivali saves into one folder" {
		t.Errorf("a full folder = %+v", resp)
	}
}

// hangingEcho is echo whose Call never returns on its own: it says when
// it started and waits for its context to end.
type hangingEcho struct {
	*echotest.Echo
	entered chan struct{}
	stopped chan struct{}
}

func (h *hangingEcho) Call(ctx context.Context, _ builtinintegrations.Caller, _ builtinintegrations.Connection, _ string, _ json.RawMessage) (*builtinintegrations.Result, error) {
	close(h.entered)
	<-ctx.Done()
	close(h.stopped)
	return nil, ctx.Err()
}

// Why this test exists: a built-in's Call runs in core's process. One
// that never returns must not hold the agent's call past the deadline a
// pod's call has: the agent is answered at integrationCallTimeout on
// core's clock, the audit says so, and the function is told to stop.
func TestBuiltinIntegrationCallThatNeverReturns(t *testing.T) {
	srv := newTestServer(t)
	clk := clock.NewFake()
	srv.Clock = clk
	h := &hangingEcho{Echo: echotest.New(), entered: make(chan struct{}), stopped: make(chan struct{})}
	srv.BuiltinIntegrations = builtinintegrations.Registry{h}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	intTeam(t, srv, "dana")
	connectEcho(t, srv, "Hello")
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	dana := agentpod.NewClient(serveCoreUDS(t, srv), "dana")

	type answer struct {
		resp *agentpod.IntegrationCallResponse
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		resp, err := dana.IntegrationCall(context.Background(), agentpod.IntegrationCallRequest{Name: "echo_echo", Arguments: json.RawMessage(`{}`)})
		got <- answer{resp, err}
	}()
	<-h.entered // runBuiltin arms its deadline before the function starts
	clk.Advance(integrationCallTimeout)
	a := <-got
	if a.err != nil {
		t.Fatal(a.err)
	}
	if !a.resp.IsError || len(a.resp.Content) != 1 || a.resp.Content[0] != "The Echo integration did not answer within 60 seconds; try again later or tell the owner." {
		t.Errorf("answer = %+v", a.resp)
	}
	<-h.stopped
	rows, _ := srv.Store.ReadIntegrationAuditTail("echo", 1)
	if len(rows) != 1 || !rows[0].Allowed || rows[0].Reason != "the integration did not answer within 60 seconds" {
		t.Errorf("audit = %+v", rows)
	}
}
