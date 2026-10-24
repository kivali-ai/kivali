package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The tests here drive echo's own setup flow (echotest, setup.go) over
// the HTTP API: a Go built-in's Setup, Requirements and Uninstall as
// core dispatches them. The runner's own mechanics, with steps no
// integration's flow takes, are in integration_setup_test.go.

// Why this test exists: this is a Go built-in's whole install, as the
// owner takes it and as core dispatches it: a Form of the declared
// fields (the setup-only key marked secret), the answer reaching Setup;
// a Go whose /connect/start is a page posting the key to the declared
// host with the flow's cookie set; the service's callback, matched by
// its state, handing the query to Setup and landing on the page; a Wait
// checked only while a stream watches it, and done once the service
// says so; what each step kept in the connection's Secret, the key in
// it nowhere; and an audit row per event. A step missing here is a
// setup no built-in can finish.
func TestEchoSetupInstalls(t *testing.T) {
	srv, e, clk := setupServer(t, nil)
	mem := secrets.NewMemory()
	srv.Secrets = mem

	flow := startSetupFlow(t, srv, "echo")
	if first := e.nextAnswer(t); first.Form != nil || first.Return != nil || first.Checked || first.State != nil || first.Leg != 1 ||
		!strings.HasPrefix(first.Callback, loopbackOrigin+"/connect/callback?state=") {
		t.Errorf("first answer = %+v", first)
	}
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if f.ID != flow.ID || f.Flow != "setup" || f.Step.Kind != apitypes.SetupFlowStepKindForm || f.Step.Words != "Name your setup.example account and paste its key." ||
		len(f.Step.Fields) != 2 {
		t.Fatalf("form = %+v", f.Step)
	}
	if acc, key := f.Step.Fields[0], f.Step.Fields[1]; acc != (apitypes.SetupField{Key: "account", Title: "Account", Words: "Your account on setup.example."}) ||
		key.Key != "key" || key.Title != "Key" || !strings.HasPrefix(key.Words, "From the account's key page.") || !key.Secret || !key.SetupOnly {
		t.Errorf("fields = %+v", f.Step.Fields)
	}

	// A blank account is asked for again, and nothing goes anywhere.
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": " ", "key": echoKey}}))
	e.nextAnswer(t)
	if f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm); f.Step.Words != "Name your setup.example account; it cannot be left blank." {
		t.Errorf("blank account = %+v", f.Step)
	}

	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": " acme ", "key": echoKey}}))
	toGo := e.nextAnswer(t)
	if toGo.Form["account"] != "acme" || toGo.Form["key"] != echoKey || toGo.State["at"] != "form" {
		t.Errorf("the form reached setup as %+v", toGo)
	}
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	if f.Step.Kind != apitypes.SetupFlowStepKindGo || textOf(f.Step.Host) != setupHost || f.Step.NoReturn {
		t.Errorf("go = %+v", f.Step)
	}
	rr, cookie := startPage(t, srv, f)
	body := rr.Body.String()
	if rr.Code != http.StatusOK || cookie == nil || !strings.Contains(body, `action="`+echotest.InstallURL+`"`) ||
		!strings.Contains(body, `name="key" value="`+echoKey+`"`) || !strings.Contains(body, `name="redirect_url"`) {
		t.Errorf("start = %d, cookie %v, page %s", rr.Code, cookie, body)
	}
	if conn := srv.setupConnection(context.Background(), "echo"); conn.Fields["account"] != "acme" || conn.Fields["key"] != "" {
		t.Errorf("kept at the Go = %v", conn.Fields)
	}

	// The answer's read of the flow counted as watching it; that has
	// lapsed by the time the owner is back from the service.
	clk.Advance(setupWatchWindow)
	to := echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-77"}})
	if id := landedOnSetup(t, to, "echo"); id != flow.ID {
		t.Errorf("landed on flow %s", id)
	}
	// What the Go kept comes back as the connection; the key, kept
	// nowhere, in State.
	if back := e.nextCall(t); back.Answer.Return.Get("code") != "c0de-77" || back.Answer.State["key"] != echoKey || back.Conn.Fields["account"] != "acme" || back.Conn.ID == "" {
		t.Errorf("the return reached setup as %+v", back)
	}
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	if textOf(f.Step.URL) != echotest.AppURL || textOf(f.Step.Host) != setupHost {
		t.Errorf("wait = %+v", f.Step)
	}

	// Nobody watches: the clock ticks, Setup is not called.
	calls := e.callCount()
	for range 3 {
		clk.BlockUntil(1)
		clk.Advance(setupWaitEvery)
	}
	clk.BlockUntil(1)
	if n := e.callCount(); n != calls {
		t.Errorf("%d checks with nobody watching", n-calls)
	}
	// A stream watches: a check that finds it not yet installed keeps the
	// Wait; one that finds it installed is done.
	if c := echoCheck(t, srv, e, clk); !c.Answer.Checked || c.Conn.Fields["token"] != "tok-c0de-77" || c.Conn.Fields["account"] != "acme" {
		t.Errorf("check = %+v", c)
	}
	if f := crmFlowOf(t, srv, "echo"); f.State != apitypes.SetupFlowStateWait {
		t.Errorf("not yet installed = %+v", f)
	}
	e.SetInstalled(true)
	echoCheck(t, srv, e, clk)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)

	rec, err := srv.Store.GetIntegrationConnection("echo")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := mem.Get(context.Background(), rec.SecretName)
	if data["token"] != "tok-c0de-77" || data["account"] != "acme" || data["key"] != "" || len(rec.Fields) != 0 ||
		!slices.Equal(rec.SecretKeys, []string{"account", "token"}) {
		t.Errorf("kept %v, fields %v, keys %v", data, rec.Fields, rec.SecretKeys)
	}
	if got := strings.Join(setupAudit(t, srv, "echo"), "|"); got != "started|returned|done" {
		t.Errorf("audit = %s", got)
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateInstalled {
		t.Errorf("after setup = %s", page.Summary.State)
	}
	// What setup kept is the connection an ordinary call is given: Test
	// connection answers with the account and the token's start.
	page := orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/connection/verify"))
	if c := page.Connection; c == nil || c.Verified == nil || !*c.Verified || textOf(c.VerifyMessage) != "acme; token tok..." {
		t.Errorf("test connection = %+v", c)
	}
}

// Why this test exists: a Go may reach its service by a link as well as
// by a POST. With no key to post, echo's Go is a link, and
// /connect/start must send the browser to it with a 303 and the flow's
// cookie, so the callback that comes back is the flow's.
func TestEchoSetupGoByLink(t *testing.T) {
	srv, e, _ := setupServer(t, nil)
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme", "key": ""}}))
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	rr, cookie := startPage(t, srv, f)
	to := mustURL(t, rr.Header().Get("Location"))
	if rr.Code != http.StatusSeeOther || cookie == nil || to.Scheme+"://"+to.Host+to.Path != echotest.InstallURL ||
		!strings.HasPrefix(to.Query().Get("redirect_url"), loopbackOrigin+"/connect/callback?state=") {
		t.Fatalf("start = %d %q, cookie %v", rr.Code, to, cookie)
	}
	landedOnSetup(t, echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-2"}}), "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
}

// Why this test exists: the flow cookie goes with any top-level GET a
// page the owner visits makes, so a callback with no state could hand
// anyone's code to Setup. Echo's Go does not say its service drops the
// state, so such a return must fail the flow without calling Setup,
// and the install keep nothing; the callback with its state is in the
// test above.
func TestEchoSetupReturnWithoutState(t *testing.T) {
	srv, e, _ := setupServer(t, nil)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	startSetupFlow(t, srv, "echo")
	_, cookie := echoGo(t, srv, "acme")
	calls := e.callCount()
	landedOnSetup(t, callback(t, srv, &url.URL{Path: "/connect/callback", RawQuery: "code=someone-elses"}, cookie), "echo")
	if f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed); textOf(f.Error) != errSetupNoState {
		t.Errorf("a return without state: %q", textOf(f.Error))
	}
	callsEnd(t, currentRun(t, srv, "echo"))
	if n := e.callCount(); n != calls {
		t.Errorf("Setup was called %d more times", n-calls)
	}
	srv.setupAwaitDiscard(context.Background(), "echo")
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets left: %v", names)
	}
}

// Why this test exists: cancelling an install under way ends it for
// good and keeps nothing, even after it kept the account and the token:
// the owner stopped making the connection, so it must not read as
// installed or hold secrets nobody finished.
func TestEchoSetupCancelled(t *testing.T) {
	srv, e, _ := setupServer(t, nil)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	startSetupFlow(t, srv, "echo")
	_, cookie := echoGo(t, srv, "acme")
	echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-3"}})
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	if len(mem.Names()) != 1 {
		t.Fatalf("secrets at the Wait: %v", mem.Names())
	}
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", rr.Code)
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)
	srv.setupAwaitDiscard(context.Background(), "echo")
	if _, err := srv.Store.GetIntegrationConnection("echo"); !errors.Is(err, store.ErrNotFound) || len(mem.Names()) != 0 {
		t.Errorf("after cancel: %v, secrets %v", err, mem.Names())
	}
	if got := strings.Join(setupAudit(t, srv, "echo"), "|"); got != "started|returned|cancelled" {
		t.Errorf("audit = %s", got)
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable {
		t.Errorf("after cancel = %s", page.Summary.State)
	}
}

// Why this test exists: a built-in's setup that fails says why, and its
// words reach the page and the audit. They quote the token setup kept
// and the key the owner entered, which must both be scrubbed there, and
// the account setup kept, which its manifest declares not secret and
// which must read as it is, not as [secret]; and the failed install
// keeps nothing.
func TestEchoSetupFails(t *testing.T) {
	srv, e, clk := setupServer(t, nil)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	e.FailSetup(true)
	startSetupFlow(t, srv, "echo")
	_, cookie := echoGo(t, srv, "acme-widgets")
	echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-4"}})
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	echoCheck(t, srv, e, clk)
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
	want := "setup.example dropped the app of acme-widgets with token " + integrationSecretPlaceholder + " and key " + integrationSecretPlaceholder
	if textOf(f.Error) != want {
		t.Errorf("error = %q", textOf(f.Error))
	}
	if got := strings.Join(setupAudit(t, srv, "echo"), "|"); got != "started|returned|refused failed: "+want {
		t.Errorf("audit = %s", got)
	}
	srv.setupAwaitDiscard(context.Background(), "echo")
	if _, err := srv.Store.GetIntegrationConnection("echo"); !errors.Is(err, store.ErrNotFound) || len(mem.Names()) != 0 {
		t.Errorf("after the failure: %v, secrets %v", err, mem.Names())
	}
}

// countingPuts is a Secrets store that counts its writes.
type countingPuts struct {
	secrets.Store
	n atomic.Int32
}

func (c *countingPuts) Put(ctx context.Context, name string, data map[string]string) error {
	c.n.Add(1)
	return c.Store.Put(ctx, name, data)
}

// Why this test exists: core holds a built-in's own steps to its
// manifest's declarations as it does a bundle's: a bug in compiled code
// must no more send the browser to another host, ask for another field
// or keep another key than a bundle can. Each fails the flow in words
// naming what it tried, before anything of the step is kept.
func TestEchoSetupStraysFailTheFlow(t *testing.T) {
	for _, tc := range []struct {
		stray echotest.Stray
		want  string
	}{
		{echotest.StrayField, "Echo's setup asked for password, which its manifest does not declare"},
		{echotest.StrayHost, "Echo's setup tried to send your browser to " + echotest.OtherHost + ", which its manifest does not declare"},
		{echotest.StrayKeep, "Echo's setup tried to keep password, which its manifest does not declare"},
	} {
		t.Run(string(tc.stray), func(t *testing.T) {
			srv, e, _ := setupServer(t, nil)
			puts := &countingPuts{Store: secrets.NewMemory()}
			srv.Secrets = puts
			e.SetStray(tc.stray)
			startSetupFlow(t, srv, "echo")
			if tc.stray != echotest.StrayField {
				waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
				orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme", "key": echoKey}}))
			}
			if f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed); textOf(f.Error) != tc.want {
				t.Errorf("error = %q", textOf(f.Error))
			}
			if _, err := srv.Store.GetIntegrationConnection("echo"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a stray step kept a connection: %v", err)
			}
			// Refused before the Keep, not kept and then forgotten.
			srv.setupAwaitDiscard(context.Background(), "echo")
			if n := puts.n.Load(); n != 0 {
				t.Errorf("a stray step wrote the Secret %d times", n)
			}
		})
	}
}
