package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The tests here hold a flow's lifetime: what a start while an install
// is under way takes over, and how long a flow is kept: setupFlowTTL after its
// last change or its last Wait check (made only while someone watches),
// and setupFlowMaxAge after its start at most, on the server's clock.

// Why this test exists: an owner watching a Wait that takes longer than
// ten minutes (an organization's admin approving the app, a build) must
// not have the flow forgotten under them while the page checks it every
// three seconds; and a stage the driver reports during a check is not a
// change, so a flow nobody watches is forgotten ten minutes after its
// last change however much its checks would have said. Once nobody
// watches, the ten minutes run from the last check.
func TestSetupFlowWatchedWaitIsKept(t *testing.T) {
	srv, fs, clk := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, _ builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
		if n > 1 {
			p.Stage("Looking at the service", 1, 2)
		}
		return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Waiting."}}, nil
	})
	startSetupFlow(t, srv, "echo")
	fs.nextAnswer(t)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	shown := clk.Now()

	run, _, ch, _ := srv.setupSubscribe("echo")
	checks := int(setupFlowTTL/setupWaitEvery) + 20
	for range checks {
		callsEnd(t, run)
		clk.BlockUntil(1)
		clk.Advance(setupWaitEvery)
		if a := fs.nextAnswer(t); !a.Checked {
			t.Fatalf("not a check: %+v", a)
		}
	}
	callsEnd(t, run)
	if clk.Now().Sub(shown) <= setupFlowTTL {
		t.Fatalf("only %v went by", clk.Now().Sub(shown))
	}
	// Each check moved expires_at and nothing else: no state was sent to
	// the stream for it, or a watched Wait would send one every three
	// seconds.
	select {
	case b := <-ch:
		t.Errorf("a check of the same Wait was streamed: %s", b)
	default:
	}
	f, _, _ := srv.setupPeek("echo")
	if f == nil || f.State != apitypes.SetupFlowStateWait {
		t.Fatalf("a watched Wait was forgotten: %+v", f)
	}
	// The last check was made now.
	if !f.ExpiresAt.Equal(clk.Now().Add(setupFlowTTL)) {
		t.Errorf("expires at %v, now %v", f.ExpiresAt, clk.Now())
	}
	srv.setupRuns.mu.Lock()
	changed := run.changed
	srv.setupRuns.mu.Unlock()
	if !changed.Equal(shown) {
		t.Errorf("the checks' stages changed the flow at %v; it was shown at %v", changed, shown)
	}

	srv.setupUnsubscribe(run, ch)
	clk.Advance(setupFlowTTL)
	if f, _, _ := srv.setupPeek("echo"); f != nil {
		t.Errorf("a Wait nobody watches was kept %v after its last check: %+v", setupFlowTTL, f)
	}
}

// Why this test exists: an owner who starts an install again while one
// is under way (back from the service in another tab, or after leaving
// the page) gets the new flow over the connection the old one made:
// what it kept stays and the new flow's calls read it, nothing of it is
// undone on the service, and the new flow is the install, so if it is
// cancelled, that connection is undone and goes. The docs and the skill
// tell a setup author to expect exactly this.
func TestSetupStartedAgainTakesOverWhatWasKept(t *testing.T) {
	srv, e, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		if last.Form == nil {
			return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
		}
		return builtinintegrations.Step{Keep: map[string]string{"token": "kept-token-1"}, Wait: &builtinintegrations.Wait{Words: "Waiting."}}, nil
	})
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}))
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	made, err := srv.Store.GetIntegrationConnection("echo")
	if err != nil {
		t.Fatal(err)
	}
	for len(e.seen) > 0 {
		<-e.seen
	}

	startSetupFlow(t, srv, "echo")
	first := e.nextCall(t)
	if first.Conn.ID != made.ID || first.Conn.Fields["token"] != "kept-token-1" {
		t.Errorf("the new flow's first call read %+v, want the connection %s with what was kept", first.Conn, made.ID)
	}
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if u := e.callsOf("uninstall"); len(u) != 0 {
		t.Errorf("starting again undid the install: %+v", u)
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateSettingUp {
		t.Errorf("while the new install runs = %s", page.Summary.State)
	}

	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rr.Code)
	}
	srv.setupAwaitDiscard(context.Background(), "echo")
	if _, err := srv.Store.GetIntegrationConnection("echo"); err == nil {
		t.Error("the connection the first install made outlived the second's cancel")
	}
	if u := e.callsOf("uninstall"); len(u) != 1 || u[0].Conn.Fields["token"] != "kept-token-1" {
		t.Errorf("uninstall calls = %+v", u)
	}
}

// Why this test exists: a device sign-in's Wait is kept until core's
// poller ends it, when the code expires, not ten minutes after it was
// shown, and the page says how long the code works from expires_at. A
// code the vendor gives five minutes must read as five minutes there.
func TestDeviceWaitExpiresWithItsCode(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	vendor.Set(func(k *connecttest.Knobs) { k.DeviceExpiresIn = 5 * time.Minute })
	issued := clk.Now()
	f := startDeviceSignIn(t, srv)
	if !f.ExpiresAt.Equal(issued.Add(5 * time.Minute)) {
		t.Errorf("expires at %v, want the code's %v", f.ExpiresAt, issued.Add(5*time.Minute))
	}
}

// Why this test exists: watching keeps a flow, so a tab left open on a
// Wait would keep it, and the connection an install made, for ever.
// setupFlowMaxAge after its start it goes however it is watched, a Wait
// its driver ends itself (the device flow's poller) included; before
// that a watched flow and a polled device Wait stay.
func TestSetupFlowEndsAtItsMaxAge(t *testing.T) {
	start := signInAt
	t.Run("checked", func(t *testing.T) {
		var runs setupRuns
		run := &setupRun{id: "a", state: apitypes.SetupFlowStateWait, started: start, changed: start, touched: start.Add(setupFlowMaxAge - time.Minute), cancel: func() {}}
		runs.runs = map[string]*setupRun{run.id: run}
		if got := run.expiresLocked(); !got.Equal(start.Add(setupFlowMaxAge)) {
			t.Errorf("expires at %v, want the ceiling", got)
		}
		runs.pruneLocked(start.Add(setupFlowMaxAge - time.Second))
		if runs.runs[run.id] == nil {
			t.Fatal("forgotten before its ceiling")
		}
		runs.pruneLocked(start.Add(setupFlowMaxAge))
		if runs.runs[run.id] != nil {
			t.Error("kept past its ceiling")
		}
	})
	t.Run("a Wait its driver ends", func(t *testing.T) {
		var runs setupRuns
		run := &setupRun{id: "b", state: apitypes.SetupFlowStateWait, started: start, changed: start, spec: setupSpec{driver: &signInSetup{}}, cancel: func() {}}
		runs.runs = map[string]*setupRun{run.id: run}
		runs.pruneLocked(start.Add(setupFlowTTL + time.Hour))
		if runs.runs[run.id] == nil {
			t.Fatal("a device Wait was forgotten while its poller runs")
		}
		runs.pruneLocked(start.Add(setupFlowMaxAge))
		if runs.runs[run.id] != nil {
			t.Error("a device Wait was kept past the ceiling")
		}
	})
}
