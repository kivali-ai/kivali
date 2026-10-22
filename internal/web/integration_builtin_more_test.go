package web

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Why this test exists: a built-in record outlives the binary that
// carried it (a Kivali that dropped it, a backup restored from one
// that had it). Nothing runs it, so its page must not say Kivali is
// starting it, its Test connection and options must say why they
// cannot run, and the owner must be able to remove it; otherwise it
// stays on the team for good.
func TestBuiltinIntegrationNotInThisKivali(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withEcho(t, srv)
	connectEcho(t, srv, "Hello")
	srv.BuiltinIntegrations = builtinintegrations.Registry{}

	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/echo"))
	if page.Summary.Status != apitypes.IntegrationStatusNotIncluded || !page.Summary.Builtin || !page.CanRemove {
		t.Errorf("page = %+v, can remove %v", page.Summary, page.CanRemove)
	}
	const why = "Echo is not in this version of Kivali, so nothing runs it; you can remove it"
	if e := assertAPIError(t, intPost(t, srv, intBase+"/echo/connection/verify"), http.StatusConflict); e.Error != why || e.Who != whoYou {
		t.Errorf("verify = %+v", e)
	}
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/echo/choices/colors"), http.StatusConflict); e.Error != why {
		t.Errorf("choices = %+v", e)
	}
	if rr := intDelete(t, srv, intBase+"/echo"); rr.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
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
		resp, err := dana.IntegrationCall(context.Background(), "echo_echo", json.RawMessage(`{}`))
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
