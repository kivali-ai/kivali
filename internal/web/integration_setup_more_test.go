package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// currentRun is name's flow, for a test that waits on its calls.
func currentRun(t *testing.T, srv *Server, name string) *setupRun {
	t.Helper()
	srv.setupRuns.mu.Lock()
	defer srv.setupRuns.mu.Unlock()
	run := srv.setupRuns.currentLocked(name)
	if run == nil {
		t.Fatalf("%s has no setup flow", name)
	}
	return run
}

// callsEnd waits until every call of run has returned, with what it
// keeps or shows, failing the test after failsafeWait.
func callsEnd(t *testing.T, run *setupRun) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		run.calling.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(failsafeWait):
		t.Fatal("a setup call did not return")
	}
}

// Why this test exists: Google and Amazon put the device flow's page
// (verification_uri) on a host none of their endpoints is on. That page
// comes from the manifest's own device endpoint's answer, not from an
// integration's code, so a sign-in must not fail on it for want of a
// host the manifest could never declare; it is still held to https.
func TestDeviceSignInPageOnAnotherHost(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	vendor.Set(func(k *connecttest.Knobs) {
		k.DeviceVerificationURI = "https://www.google.com/device"
		k.DeviceVerificationURIComplete = "https://www.google.com/device?user_code=WDJB-MJHT"
	})
	f := startDeviceSignIn(t, srv)
	if textOf(f.Step.URL) != "https://www.google.com/device?user_code=WDJB-MJHT" || textOf(f.Step.Host) != "www.google.com" {
		t.Errorf("wait = %+v", f.Step)
	}
	pollAfter(t, clk, 5*time.Second)
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateDone {
		t.Errorf("flow = %+v %q", f, textOf(f.Error))
	}

	plain, plainVendor, _ := signInServer(t)
	signInBundle(t, plain, plainVendor)
	saveSignInClient(t, plain)
	plainVendor.Set(func(k *connecttest.Knobs) {
		k.DeviceVerificationURI = "http://device.example/activate"
		k.DeviceVerificationURIComplete = "http://device.example/activate?user_code=WDJB-MJHT"
	})
	startSetupFlow(t, plain, "crm")
	if f := waitFlow(t, plain, "crm", apitypes.SetupFlowStateFailed); !strings.Contains(textOf(f.Error), "is not an https address") {
		t.Errorf("a plain http page: %q", textOf(f.Error))
	}
}

// Why this test exists: the flow cookie goes with any top-level GET a
// page the owner visits makes (SameSite=Lax), so a callback with no
// state could hand an attacker's query to Setup (a manifest code of the
// attacker's account, signing the owner in to it). Only a Go step that
// says its service drops the state may take such a return; for any
// other the flow fails and Setup is not called.
func TestSetupFlowStatelessReturnOnlyWhereTheStepSaysSo(t *testing.T) {
	for _, stateless := range []bool{false, true} {
		srv, fs, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, _ builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
			if n == 1 {
				return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/go", Stateless: stateless}}, nil
			}
			return builtinintegrations.Step{Done: true}, nil
		})
		startSetupFlow(t, srv, "echo")
		fs.nextAnswer(t)
		f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
		_, cookie := startPage(t, srv, f)
		to := callback(t, srv, &url.URL{Path: "/connect/callback", RawQuery: "code=attackers"}, cookie)
		landedOnSetup(t, to, "echo")
		if !stateless {
			if f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed); textOf(f.Error) != errSetupNoState {
				t.Errorf("a stateless return of a step that did not say so: %q", textOf(f.Error))
			}
			callsEnd(t, currentRun(t, srv, "echo"))
			if n := fs.callCount(); n != 1 {
				t.Errorf("Setup was called %d times, want once", n)
			}
			continue
		}
		if a := fs.nextAnswer(t); a.Return.Get("code") != "attackers" {
			t.Errorf("a step that says so: %+v", a)
		}
		waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)
	}
}

// Why this test exists: a call that finishes after its flow was
// cancelled, or replaced by a new start, must keep nothing and change
// nothing: a Keep landing then would write a connection the owner
// stopped making, or over the new flow's.
func TestSetupCallAfterCancelKeepsNothing(t *testing.T) {
	for _, how := range []string{"cancel", "replace"} {
		t.Run(how, func(t *testing.T) {
			release := make(chan struct{})
			srv, fs, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, _ builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
				if n == 1 {
					<-release
					return builtinintegrations.Step{Done: true, Keep: map[string]string{"app_id": "late-app-1"}}, nil
				}
				return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
			})
			startSetupFlow(t, srv, "echo")
			fs.nextAnswer(t)
			old := currentRun(t, srv, "echo")
			if how == "cancel" {
				if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil); rr.Code != http.StatusNoContent {
					t.Fatalf("cancel: %d", rr.Code)
				}
			} else {
				startSetupFlow(t, srv, "echo")
				waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
			}
			close(release)
			callsEnd(t, old)
			if _, err := srv.Store.GetIntegrationConnection("echo"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a cancelled call kept a connection: %v", err)
			}
			if how == "replace" {
				if f := crmFlowOf(t, srv, "echo"); f.State != apitypes.SetupFlowStateForm {
					t.Errorf("the new flow = %+v", f)
				}
			} else {
				assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)
			}
		})
	}
}

// crmFlowOf is name's flow as GET .../setup answers it.
func crmFlowOf(t *testing.T, srv *Server, name string) apitypes.SetupFlow {
	t.Helper()
	return orgOK[apitypes.SetupFlow](t, orgGet(t, srv, intBase+"/"+name+"/setup"))
}

// Why this test exists: what a step keeps is the connection's from
// then on, and a later step reads it back as conn.Fields (the app's id
// to install, the key to sign with). Each call is told which it is
// (Answer.Leg), and a Wait's check is given the latest state issued, so
// an address it puts in the service comes back to the flow.
func TestSetupFlowReadsBackWhatItKept(t *testing.T) {
	conns := make(chan builtinintegrations.Connection, 8)
	srv, fs, clk := setupServer(t, func(n int, _ context.Context, conn builtinintegrations.Connection, _ builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		conns <- conn
		switch n {
		case 1:
			return builtinintegrations.Step{Keep: map[string]string{"app_id": "app-42"}, Wait: &builtinintegrations.Wait{Words: "Install it."}}, nil
		case 2:
			return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Install it."}}, nil
		}
		return builtinintegrations.Step{Done: true}, nil
	})
	startSetupFlow(t, srv, "echo")
	first := fs.nextAnswer(t)
	<-conns
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	run, _, ch, _ := srv.setupSubscribe("echo")
	clk.BlockUntil(1)
	clk.Advance(setupWaitEvery)
	check := fs.nextAnswer(t)
	if conn := <-conns; conn.Fields["app_id"] != "app-42" || conn.ID == "" {
		t.Errorf("the check's connection = %+v", conn)
	}
	srv.setupUnsubscribe(run, ch)
	if first.Leg != 1 || check.Leg != 2 || !check.Checked || check.Callback != first.Callback {
		t.Errorf("first %d %q, check %d %q", first.Leg, first.Callback, check.Leg, check.Callback)
	}
}

// Why this test exists: the declared hosts bound where a step may send
// the owner's browser. A declared bare host must not admit another port
// on it (a server the service never runs), user info must not hide a
// host, a host's case or an IPv6 loopback must read as what it is, and
// plain http is never safe off loopback, a POST no more than a link. A
// Wait with no page is fine.
func TestSetupURLAllowed(t *testing.T) {
	hosts := []string{"github.com", "127.0.0.1:8443"}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://github.com/settings/apps/new", true},
		{"https://GitHub.COM/settings", true},
		{"https://github.com:8443/settings", false},
		{"https://github.com:443/settings", false},
		{"https://user:pass@github.com/", false},
		{"https://evil.example@github.com/", false},
		{"http://github.com/settings", false},
		{"https://127.0.0.1:8443/authorize", true},
		{"http://127.0.0.1:8443/authorize", true},
		{"https://127.0.0.1:9000/authorize", false},
		{"http://[::1]:8443/", false},
		{"ftp://github.com/", false},
		{"github.com/settings", false},
		{"", false},
	} {
		if got := setupURLAllowed(tc.raw, hosts); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.raw, got, tc.want)
		}
	}
	if !setupURLAllowed("http://[::1]:8443/", []string{"[::1]:8443"}) {
		t.Error("an IPv6 loopback endpoint")
	}
	spec := setupSpec{hosts: []string{"github.com"}}
	if err := spec.check("Echo", builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "http://github.com/new", Post: url.Values{"manifest": {"{}"}}}}); err == nil {
		t.Error("a POST to a plain http address passed")
	}
	if err := spec.check("Echo", builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Waiting."}}); err != nil {
		t.Errorf("a Wait with no page: %v", err)
	}
}

// Why this test exists: the browser's routes must not act on a flow
// that is not where they expect it: /connect/start of a flow on a Form
// returns to its page without sending the browser anywhere or setting
// a cookie, one of no flow returns to the list, and an answer while the
// flow asks for no form is a 409 that changes nothing.
func TestSetupRoutesOnTheWrongStep(t *testing.T) {
	srv, fs, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, _ builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		if n == 1 {
			return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
		}
		return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Waiting."}}, nil
	})
	f := startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	rr := flowDo(t, srv, http.MethodGet, connectStartPath+"?flow="+f.ID, nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/org/integrations/echo?setup="+f.ID || len(rr.Result().Cookies()) != 0 {
		t.Errorf("a flow on its Form: %d %q %v", rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
	rr = flowDo(t, srv, http.MethodGet, connectStartPath+"?flow="+strings.Repeat("a", 32), nil)
	startAgain(t, mustURL(t, rr.Header().Get("Location")), "/org/integrations")

	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}))
	fs.nextAnswer(t)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "x"}}), http.StatusConflict); e.Error != "the setup is not asking for a form now" {
		t.Errorf("an answer on a Wait = %+v", e)
	}
	if n := fs.callCount(); n != 2 {
		t.Errorf("%d calls", n)
	}
}

// mustURL is raw parsed.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Why this test exists: a Go with NoReturn has nothing coming back, so
// the Desktop handoff for it must set no flow cookie, which would end
// another flow on a Go step in that browser for nothing.
func TestSetupHandoffForAGoWithoutReturn(t *testing.T) {
	srv, _, _ := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/settings", NoReturn: true}}, nil
	})
	srv.Handoff = desktopHandoff(srv)
	srv.wireRoutes()
	f := startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	rr := serveHandoff(t, srv, "127.0.0.1:8080", handoffToken(t, srv), f.ID)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/connect/start?flow="+f.ID || cookieOf(rr, "kivali_connect") != nil {
		t.Errorf("handoff = %d %q %v", rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
}

// Why this test exists: a setup that makes an app and hands over to a
// device sign-in (setup.then: device) must go on to show the vendor's
// code with the client id setup kept, and finish when the owner
// approves, as a device sign-in connection does.
func TestSetupFlowHandsOverToADeviceSignIn(t *testing.T) {
	clk := clock.NewFakeAt(signInAt)
	vendor := connecttest.New(t, clk)
	srv, _, _ := setupServerWith(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Done: true, Keep: map[string]string{"client_id": connecttest.ClientID, "client_secret": connecttest.ClientSecret}}, nil
	}, func(fs *fakeSetup) {
		fs.setup.Then = integration.MethodDevice
		fs.setup.Keeps = append(fs.setup.Keeps, integration.SetupKeep{Key: "client_id", Words: "The app's client id"}, integration.SetupKeep{Key: "client_secret", Words: "The app's client secret"})
		ep := vendor.Endpoints()
		fs.edit = func(c *integration.Connection) {
			c.DeviceEndpoint, c.TokenEndpoint = ep.Device, ep.Token
			c.CredentialKeys = map[string]string{"access_token": "TOKEN"}
		}
	})
	srv.Clock = clk
	srv.Connect = &connect.Engine{Clock: clk, HTTP: vendor.Server.Client()}
	startSetupFlow(t, srv, "echo")
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	if textOf(f.Step.Code) != "WDJB-MJHT" {
		t.Errorf("wait = %+v", f.Step)
	}
	if start := vendor.Requests()[0]; start.Form.Get("client_id") != connecttest.ClientID {
		t.Errorf("the device start sent %v", start.Form)
	}
	pollAfter(t, clk, 5*time.Second)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)
	rec, err := srv.Store.GetIntegrationConnection("echo")
	if err != nil || rec.SignIn == nil || rec.SignIn.Method != integration.MethodDevice {
		t.Errorf("after the hand-over: %+v %v", rec.SignIn, err)
	}
}

// desktopHandoff is the Desktop handoff for srv, on its clock.
func desktopHandoff(srv *Server) *auth.Handoff {
	return &auth.Handoff{Codec: srv.AuthMW.Codec, Allowlist: auth.NewAllowlist(testOwnerEmail), Now: srv.clk().Now}
}

// Why this test exists: what a service sends back (a manifest code, an
// installation id) is a secret of the flow's, and a driver that echoes
// it in its error must not put it on the page or in the audit. The
// service's own words of a refusal (error_description) stay readable.
// Progress words are scrubbed whole before they are cut, so no piece of
// a secret astride the cut is left.
func TestSetupFlowScrubsWhatCameBack(t *testing.T) {
	staged := make(chan struct{})
	srv, fs, _ := setupServer(t, func(n int, ctx context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
		switch n {
		case 1:
			return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/go"}, State: map[string]string{"key": "state-secret-77"}}, nil
		case 2:
			p.Stage(strings.Repeat("y", signInWordsRunes-5)+"state-secret-77", 1, 2)
			close(staged)
			<-ctx.Done()
			return builtinintegrations.Step{}, ctx.Err()
		}
		return builtinintegrations.Step{}, errors.New("GitHub refused " + last.Return.Get("code") + ": " + last.Return.Get("error_description"))
	})
	startSetupFlow(t, srv, "echo")
	fs.nextAnswer(t)
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	_, cookie := startPage(t, srv, f)
	srv.setupRuns.mu.Lock()
	legs := append([]string(nil), srv.setupRuns.runs[f.ID].legs...)
	srv.setupRuns.mu.Unlock()
	back := &url.URL{Path: "/connect/callback", RawQuery: url.Values{"state": {legs[0]}, "code": {"c0de-secret-9"}, "error_description": {"the code expired"}}.Encode()}
	landedOnSetup(t, callback(t, srv, back, cookie), "echo")
	<-staged
	got := crmFlowOf(t, srv, "echo")
	if got.Progress == nil || strings.Contains(got.Progress.Words, "state-secr") || len([]rune(got.Progress.Words)) > signInWordsRunes {
		t.Errorf("progress = %+v", got.Progress)
	}
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rr.Code)
	}

	srv, fs, _ = setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		if n == 1 {
			return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/go"}}, nil
		}
		return builtinintegrations.Step{}, errors.New("GitHub refused " + last.Return.Get("code") + ": " + last.Return.Get("error_description"))
	})
	startSetupFlow(t, srv, "echo")
	fs.nextAnswer(t)
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	_, cookie = startPage(t, srv, f)
	srv.setupRuns.mu.Lock()
	legs = append([]string(nil), srv.setupRuns.runs[f.ID].legs...)
	srv.setupRuns.mu.Unlock()
	back = &url.URL{Path: "/connect/callback", RawQuery: url.Values{"state": {legs[0]}, "code": {"c0de-secret-9"}, "error_description": {"the code expired"}}.Encode()}
	callback(t, srv, back, cookie)
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
	if textOf(f.Error) != "GitHub refused [secret]: the code expired" {
		t.Errorf("error = %q", textOf(f.Error))
	}
	for _, row := range setupAudit(t, srv, "echo") {
		if strings.Contains(row, "c0de-secret-9") {
			t.Errorf("audit = %s", row)
		}
	}
}

// Why this test exists: a registered client (client: registered) is
// one this version of Kivali does not register itself as. The start is
// refused in words that say so, rather than started and failed, which
// would leave started and failed rows in the audit for nothing.
func TestSetupFlowRefusesARegisteredClient(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	browserBundle(t, srv, vendor, func(c *integration.Connection) {
		c.Client, c.Discover = integration.ClientRegistered, vendor.URL+"/mcp"
	})
	e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup", apitypes.SetupFlowStart{Flow: "setup"}), http.StatusConflict)
	if e.Error != errSignInNotRegistered {
		t.Errorf("a registered client = %+v", e)
	}
	if got := setupAudit(t, srv, "crm"); len(got) != 0 {
		t.Errorf("audit = %q", got)
	}
}
