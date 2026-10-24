package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// setupHost is the host the test setup integration's Go and Wait
// steps may name.
const setupHost = "service.example"

// fakeSetup is a built-in whose connection its own setup flow makes
// (method setup): echo's actions, a setup section of its own, and a
// Setup the test scripts. Each call's Answer is sent on seen.
type fakeSetup struct {
	*echotest.Echo
	setup integration.Setup
	// edit, when set, changes the connection section after setup is
	// put in (a hand-over's endpoints).
	edit   func(c *integration.Connection)
	script func(n int, ctx context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error)

	mu    sync.Mutex
	calls int
	seen  chan builtinintegrations.Answer
}

func (f *fakeSetup) Manifest() *integration.Manifest {
	m := f.Echo.Manifest()
	m.UserConfig = nil
	setup := f.setup
	m.Kivali.Connection = integration.Connection{Method: integration.MethodSetup, Setup: &setup}
	if f.edit != nil {
		f.edit(&m.Kivali.Connection)
	}
	return m
}

func (f *fakeSetup) Setup(ctx context.Context, conn builtinintegrations.Connection, flow string, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	f.seen <- last
	return f.script(n, ctx, conn, last, p)
}

// nextAnswer is the Answer of the setup's next call.
func (f *fakeSetup) nextAnswer(t *testing.T) builtinintegrations.Answer {
	t.Helper()
	select {
	case a := <-f.seen:
		return a
	case <-time.After(failsafeWait):
		t.Fatal("setup was not called")
	}
	return builtinintegrations.Answer{}
}

// callCount is how many times Setup was called.
func (f *fakeSetup) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testSetupDecl is the test integration's kivali.connection.setup.
func testSetupDecl() integration.Setup {
	return integration.Setup{
		Summary: "Makes an app on the service.",
		Hosts:   []string{setupHost},
		Fields: integration.SetupFields{
			{Key: "account", Words: "Your account's name"},
			{Key: "key", Title: "Key", Words: "From the key page", Secret: true, SetupOnly: true},
		},
		Keeps: integration.SetupKeeps{{Key: "app_id", Words: "The app's id"}, {Key: "app_secret", Words: "The app's secret"}},
	}
}

// setupServer is a server on a fake clock with fakeSetup installed as
// the built-in echo, scripted by script.
func setupServer(t *testing.T, script func(n int, ctx context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error)) (*Server, *fakeSetup, *clock.Fake) {
	t.Helper()
	return setupServerWith(t, script, nil)
}

// setupServerWith is setupServer with the fake changed by edit before
// it is installed.
func setupServerWith(t *testing.T, script func(n int, ctx context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error), edit func(fs *fakeSetup)) (*Server, *fakeSetup, *clock.Fake) {
	t.Helper()
	srv := newTestServer(t)
	clk := clock.NewFakeAt(signInAt)
	srv.Clock = clk
	fs := &fakeSetup{Echo: echotest.New(), setup: testSetupDecl(), script: script, seen: make(chan builtinintegrations.Answer, 64)}
	if edit != nil {
		edit(fs)
	}
	srv.BuiltinIntegrations = builtinintegrations.Registry{fs}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	return srv, fs, clk
}

// flowDo serves method path with body (JSON when not nil) through the
// whole server, signed in as the owner, from a page on loopback.
func flowDo(t *testing.T, srv *Server, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(method, loopbackOrigin+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, loopbackOrigin+path, nil)
	}
	return serveSignedIn(t, srv, req, cookies...)
}

// startSetupFlow starts name's setup flow and returns the answer.
func startSetupFlow(t *testing.T, srv *Server, name string) apitypes.SetupFlow {
	t.Helper()
	return orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/"+name+"/setup", apitypes.SetupFlowStart{Flow: "setup"}))
}

// waitFlow waits for name's flow to reach state, watching its stream
// meanwhile, and returns it.
func waitFlow(t *testing.T, srv *Server, name string, state apitypes.SetupFlowState) apitypes.SetupFlow {
	t.Helper()
	run, initial, ch, ok := srv.setupSubscribe(name)
	if !ok {
		t.Fatalf("%s has no setup flow", name)
	}
	defer srv.setupUnsubscribe(run, ch)
	b := initial
	deadline := time.After(failsafeWait)
	for {
		var f apitypes.SetupFlow
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		if f.State == state {
			return f
		}
		select {
		case next, ok := <-ch:
			if !ok {
				t.Fatalf("%s's flow was forgotten while waiting for %s (last %+v)", name, state, f)
			}
			b = next
		case <-deadline:
			t.Fatalf("%s's flow is %s, waited for %s: %+v", name, f.State, state, f)
		}
	}
}

// setupAudit is name's audit rows of its setup flow, oldest first, as
// "<allowed> <reason>" (ReadIntegrationAuditTail answers newest first).
func setupAudit(t *testing.T, srv *Server, name string) []string {
	t.Helper()
	rows, err := srv.Store.ReadIntegrationAuditTail(name, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r.Action == "setup" {
			if r.Agent != agent.CEOSlug {
				t.Errorf("a setup row under %s", r.Agent)
			}
			out = append(out, strings.TrimSpace(map[bool]string{true: "", false: "refused "}[r.Allowed]+r.Reason))
		}
	}
	return out
}

// startPage follows the Go step of flow f: GET /connect/start, which
// answers with the address it sends the browser to and the flow's
// cookie.
func startPage(t *testing.T, srv *Server, f apitypes.SetupFlow) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	if f.Step == nil || f.Step.StartURL == nil {
		t.Fatalf("flow has no Go step: %+v", f)
	}
	rr := flowDo(t, srv, http.MethodGet, *f.Step.StartURL, nil)
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "kivali_connect" && c.MaxAge > 0 {
			cookie = c
		}
	}
	return rr, cookie
}

// Why this test exists: this is a setup integration's whole flow
// through core, as the owner takes it: a Form of declared fields (a
// secret one marked), the owner's answer reaching Setup as Answer.Form,
// a Go whose /connect/start sends the browser to the service with the
// flow's cookie, the service's callback (matched by the state Setup was
// given in Answer.Callback) handing the whole query to Setup as Return
// and landing on the page with ?setup=<id> and nothing else, a Wait
// checked while the page watches, and Done with Keep written into the
// connection's Secret as the connection's values. Each event is in the
// owner's audit under the flow's name. A step missing here is a setup
// no integration can finish.
func TestSetupFlowFormGoWaitDone(t *testing.T) {
	var goURL string
	srv, fs, clk := setupServer(t, func(n int, _ context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
		switch n {
		case 1:
			return builtinintegrations.Step{Form: &builtinintegrations.Form{Words: "Name the account.", Fields: []string{"account", "key"}}}, nil
		case 2:
			goURL = "https://" + setupHost + "/apps/new?" + url.Values{"redirect": {last.Callback}}.Encode()
			return builtinintegrations.Step{
				Go:    &builtinintegrations.Go{Words: "Create the app.", URL: goURL},
				State: map[string]string{"key": last.Form["key"]},
			}, nil
		case 3:
			p.Stage("Making the app", 1, 2)
			return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Install it.", URL: "https://" + setupHost + "/install"}, State: last.State}, nil
		default:
			if !last.Checked {
				t.Errorf("call %d is not a check: %+v", n, last)
			}
			return builtinintegrations.Step{Done: true, Keep: map[string]string{"app_id": "app-42", "app_secret": "s3cret-value"}}, nil
		}
	})

	flow := startSetupFlow(t, srv, "echo")
	if flow.Flow != "setup" || len(flow.ID) != 32 || !flow.ExpiresAt.Equal(signInAt.Add(setupFlowTTL)) {
		t.Errorf("started = %+v", flow)
	}
	if first := fs.nextAnswer(t); first.Form != nil || first.Return != nil || first.Checked || first.State != nil || !strings.HasPrefix(first.Callback, loopbackOrigin+"/connect/callback?state=") {
		t.Errorf("first answer = %+v", first)
	}
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if f.Step == nil || f.Step.Kind != apitypes.SetupFlowStepKindForm || f.Step.Words != "Name the account." || len(f.Step.Fields) != 2 ||
		f.Step.Fields[0].Title != "Account" || f.Step.Fields[1].Key != "key" || !f.Step.Fields[1].Secret || !f.Step.Fields[1].SetupOnly {
		t.Errorf("form = %+v", f.Step)
	}

	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": " acme ", "key": "pak-123456"}}))
	if a := fs.nextAnswer(t); a.Form["account"] != "acme" || a.Form["key"] != "pak-123456" {
		t.Errorf("the form reached setup as %+v", a.Form)
	}
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	if f.Step.Kind != apitypes.SetupFlowStepKindGo || *f.Step.StartURL != "/connect/start?flow="+f.ID || *f.Step.Host != setupHost || f.Step.NoReturn {
		t.Errorf("go = %+v", f.Step)
	}
	rr, cookie := startPage(t, srv, f)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != goURL || cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("start = %d %q, cookie %+v", rr.Code, rr.Header().Get("Location"), cookie)
	}

	redirect, _ := url.Parse(goURL)
	back, _ := url.Parse(redirect.Query().Get("redirect"))
	q := back.Query()
	q.Set("code", "c0de-from-service")
	q.Set("installation_id", "77")
	back.RawQuery = q.Encode()
	rr = serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, back.String(), nil), cookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/org/integrations/echo?setup="+f.ID {
		t.Fatalf("callback = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if a := fs.nextAnswer(t); a.Return.Get("code") != "c0de-from-service" || a.Return.Get("installation_id") != "77" || a.State["key"] != "pak-123456" {
		t.Errorf("the return reached setup as %+v", a)
	}
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	if f.Step.Kind != apitypes.SetupFlowStepKindWait || *f.Step.URL != "https://"+setupHost+"/install" || f.Step.Words != "Install it." {
		t.Errorf("wait = %+v", f.Step)
	}

	// The page watches; the Wait is checked every three seconds.
	run, _, ch, _ := srv.setupSubscribe("echo")
	clk.BlockUntil(1)
	clk.Advance(setupWaitEvery)
	if a := fs.nextAnswer(t); !a.Checked {
		t.Errorf("check = %+v", a)
	}
	srv.setupUnsubscribe(run, ch)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)

	conn, err := srv.Store.GetIntegrationConnection("echo")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := srv.Secrets.Get(context.Background(), conn.SecretName)
	if data["app_id"] != "app-42" || data["app_secret"] != "s3cret-value" || data["key"] != "" || len(conn.Fields) != 0 {
		t.Errorf("kept %v, fields %v", data, conn.Fields)
	}
	if got := strings.Join(setupAudit(t, srv, "echo"), "|"); got != "started|returned|done" {
		t.Errorf("audit = %s", got)
	}
}

// Why this test exists: the manifest's declarations are the install
// screen's promise: Kivali sends the browser only to declared hosts,
// asks only for declared fields and keeps only declared keys. A step
// outside them (a bug, or a bundle sending the owner to a look-alike
// page) must fail the flow in words that name what it tried, before
// anything of the step is kept. A step that is none or several of the
// kinds fails too.
func TestSetupFlowHoldsStepsToTheDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		step builtinintegrations.Step
		want string
	}{
		{"undeclared field", builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"password"}}}, "Echo's setup asked for password, which its manifest does not declare"},
		{"undeclared host", builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://evil.example/login"}}, "Echo's setup tried to send your browser to evil.example, which its manifest does not declare"},
		{"plain http", builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "http://" + setupHost + "/login"}}, "Echo's setup tried to send your browser to " + setupHost + ", which its manifest does not declare"},
		{"wait page elsewhere", builtinintegrations.Step{Wait: &builtinintegrations.Wait{URL: "https://evil.example/"}}, "Echo's setup named a page on evil.example, which its manifest does not declare"},
		{"undeclared keep", builtinintegrations.Step{Done: true, Keep: map[string]string{"app_id": "a1", "password": "hunter22"}}, "Echo's setup tried to keep password, which its manifest does not declare"},
		{"no step", builtinintegrations.Step{Keep: map[string]string{"app_id": "a1"}}, errSetupNoStep},
		{"two steps", builtinintegrations.Step{Done: true, Wait: &builtinintegrations.Wait{}}, errSetupManySteps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
				return tc.step, nil
			})
			startSetupFlow(t, srv, "echo")
			f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
			if f.Error == nil || *f.Error != tc.want || f.Step != nil {
				t.Errorf("failed = %+v, %q", f, textOf(f.Error))
			}
			if _, err := srv.Store.GetIntegrationConnection("echo"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a refused step kept a connection: %v", err)
			}
			if got := setupAudit(t, srv, "echo"); len(got) != 2 || got[1] != "refused failed: "+tc.want {
				t.Errorf("audit = %q", got)
			}
		})
	}
}

// Why this test exists: a Wait is checked only while someone watches
// it, so a flow left in a closed tab does not call the service every
// three seconds for ten minutes. A read of the flow's state counts as
// watching for a few seconds (the page's polling fallback), a stream
// for as long as it is open. A check that finds the same Wait is no
// change, so a Wait nobody finishes is forgotten after ten minutes.
func TestSetupFlowWaitChecksOnlyWhileWatched(t *testing.T) {
	srv, fs, clk := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Waiting."}}, nil
	})
	startSetupFlow(t, srv, "echo")
	fs.nextAnswer(t)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)

	// Nobody watches: three ticks, no check.
	for range 3 {
		clk.BlockUntil(1)
		clk.Advance(setupWaitEvery)
	}
	clk.BlockUntil(1)
	if n := fs.callCount(); n != 1 {
		t.Errorf("%d calls with nobody watching", n)
	}

	// Ten minutes with the same Wait: forgotten.
	clk.Advance(setupFlowTTL)
	assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)

	// A read counts as watching for setupWatchWindow.
	srv, fs, clk = setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Waiting."}}, nil
	})
	startSetupFlow(t, srv, "echo")
	fs.nextAnswer(t)
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	clk.BlockUntil(1)
	orgOK[apitypes.SetupFlow](t, orgGet(t, srv, intBase+"/echo/setup"))
	clk.Advance(setupWaitEvery)
	if a := fs.nextAnswer(t); !a.Checked {
		t.Errorf("check = %+v", a)
	}
	run := &setupRun{polled: signInAt}
	if !run.watchedLocked(signInAt.Add(setupWatchWindow-time.Second)) || run.watchedLocked(signInAt.Add(setupWatchWindow)) {
		t.Errorf("a read watches for %v", setupWatchWindow)
	}
}

// Why this test exists: setup handles keys and codes, and its error
// words, progress and steps are shown on the page, logged and audited.
// Every kept value, every secret the owner entered and every value of
// the flow's State six characters or longer must be replaced before
// any of them is shown or kept, whole, and only then cut, so no piece
// of a secret is left.
func TestSetupFlowScrubsItsSecrets(t *testing.T) {
	srv, fs, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		switch n {
		case 1:
			return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"key"}}}, nil
		case 2:
			return builtinintegrations.Step{
				Keep:  map[string]string{"app_secret": "kept-secret-1"},
				State: map[string]string{"code": "state-code-22", "short": "ab"},
				Form:  &builtinintegrations.Form{Words: "Key pak-entered-9 again", Fields: []string{"account"}},
			}, nil
		}
		return builtinintegrations.Step{}, errors.New("failed with pak-entered-9, kept-secret-1, state-code-22 and ab " + strings.Repeat("x", 300))
	})
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"key": "pak-entered-9"}}))
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if f.Step.Words != "Key [secret] again" {
		t.Errorf("form words = %q", f.Step.Words)
	}
	flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}})
	fs.nextAnswer(t)
	fs.nextAnswer(t)
	fs.nextAnswer(t)
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
	want := clipRunes("failed with [secret], [secret], [secret] and ab "+strings.Repeat("x", 300), signInWordsRunes)
	if f.Error == nil || *f.Error != want {
		t.Errorf("error = %q", textOf(f.Error))
	}
	for _, row := range setupAudit(t, srv, "echo") {
		for _, secret := range []string{"pak-entered-9", "kept-secret-1", "state-code-22"} {
			if strings.Contains(row, secret) {
				t.Errorf("the audit holds %q: %s", secret, row)
			}
		}
	}
}

// Why this test exists: Setup runs off the request, and nothing it does
// may hang the flow or the server: a call that does not finish within
// five minutes on the server's clock fails the flow in words that say
// so, and one that panics fails it rather than taking the server down.
// Its stages show as the flow's progress while it runs, with the count
// a real progress bar needs.
func TestSetupFlowCallsAreBounded(t *testing.T) {
	t.Run("progress and timeout", func(t *testing.T) {
		staged := make(chan struct{})
		srv, _, clk := setupServer(t, func(_ int, ctx context.Context, _ builtinintegrations.Connection, _ builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
			p.Stage("Building the app", 2, 5)
			close(staged)
			<-ctx.Done()
			return builtinintegrations.Step{}, ctx.Err()
		})
		startSetupFlow(t, srv, "echo")
		<-staged
		f := orgOK[apitypes.SetupFlow](t, orgGet(t, srv, intBase+"/echo/setup"))
		if f.State != apitypes.SetupFlowStateRunning || f.Progress == nil || *f.Progress != (apitypes.SetupFlowProgress{Words: "Building the app", Done: 2, Total: 5}) {
			t.Errorf("running = %+v", f)
		}
		clk.BlockUntil(1)
		clk.Advance(setupCallTimeout)
		if f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed); textOf(f.Error) != errSetupTimeout {
			t.Errorf("after the deadline: %q", textOf(f.Error))
		}
	})
	t.Run("panic", func(t *testing.T) {
		srv, _, _ := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
			panic("boom")
		})
		startSetupFlow(t, srv, "echo")
		if f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed); !strings.Contains(textOf(f.Error), "boom") {
			t.Errorf("after a panic: %q", textOf(f.Error))
		}
	})
}

// Why this test exists: the setup routes refuse what cannot run, in
// words the page shows: an integration switched off or with no approved
// version, a flow name it does not have (a Fix's, until those exist), a
// form answer while the flow asks for none or with a field the form
// does not ask for, and a read, answer or cancel with no flow. Cancel
// ends the flow for good, audited; so does switching the integration
// off.
func TestSetupRoutesRefusals(t *testing.T) {
	srv, _, _ := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
	})
	assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)
	assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{}), http.StatusNotFound)
	assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil), http.StatusNotFound)
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup", apitypes.SetupFlowStart{Flow: "permissions"}), http.StatusConflict); e.Error != "permissions is not a flow Echo has" {
		t.Errorf("a fix = %+v", e)
	}

	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"key": "x"}}), http.StatusBadRequest); e.Error != "key is not a field this step asks for" {
		t.Errorf("an undeclared answer = %+v", e)
	}
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil); rr.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)

	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/disable"))
	assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup", apitypes.SetupFlowStart{Flow: "setup"}), http.StatusConflict); e.Error != "switch Echo on first" {
		t.Errorf("switched off = %+v", e)
	}
	if got := strings.Join(setupAudit(t, srv, "echo"), "|"); got != "started|cancelled|started|cancelled" {
		t.Errorf("audit = %s", got)
	}

	if rr := intUpload(t, srv, intBundle(t, "plain", "1.0.0")); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/plain/setup", apitypes.SetupFlowStart{Flow: "setup"}), http.StatusConflict); e.Error != "approve a version of Test plain first" {
		t.Errorf("no approved version = %+v", e)
	}
}

// Why this test exists: a Go may send the browser away and back more
// than once (make an app, then install it), and a service may come back
// with a state an earlier step gave it (an address fixed when the app
// was made), or with no state at all, in the browser that left. Each
// return must reach Setup; a return to another browser (no cookie, or
// a cookie of another flow) must end the flow as started elsewhere.
func TestSetupFlowLeavesAndReturnsMoreThanOnce(t *testing.T) {
	var firstCallback string
	srv, fs, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		if n == 1 {
			firstCallback = last.Callback
		}
		if n <= 3 {
			return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/leg"}}, nil
		}
		return builtinintegrations.Step{Done: true}, nil
	})
	startSetupFlow(t, srv, "echo")
	fs.nextAnswer(t)

	// The second leg comes back with the first leg's state.
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	_, cookie := startPage(t, srv, f)
	back, _ := url.Parse(firstCallback)
	if rr := serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, back.String(), nil), cookie); rr.Header().Get("Location") != "/org/integrations/echo?setup="+f.ID {
		t.Fatalf("first return = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	fs.nextAnswer(t)
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	_, cookie = startPage(t, srv, f)
	if rr := serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, back.String(), nil), cookie); rr.Code != http.StatusSeeOther {
		t.Fatalf("second return = %d", rr.Code)
	}
	fs.nextAnswer(t)

	// The third comes back with no state, matched by the cookie.
	f = waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	_, cookie = startPage(t, srv, f)
	if rr := serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, loopbackOrigin+"/connect/callback?installation_id=9", nil), cookie); rr.Header().Get("Location") != "/org/integrations/echo?setup="+f.ID {
		t.Fatalf("stateless return = %q", rr.Header().Get("Location"))
	}
	if a := fs.nextAnswer(t); a.Return.Get("installation_id") != "9" {
		t.Errorf("the stateless return reached setup as %+v", a.Return)
	}
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)

	// Another browser's return: the flow ends as started elsewhere.
	srv2, _, _ := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/leg"}}, nil
	})
	startSetupFlow(t, srv2, "echo")
	f = waitFlow(t, srv2, "echo", apitypes.SetupFlowStateGo)
	startPage(t, srv2, f)
	srv2.setupRuns.mu.Lock()
	state := srv2.setupRuns.runs[f.ID].legs[0]
	srv2.setupRuns.mu.Unlock()
	serveSignedIn(t, srv2, httptest.NewRequest(http.MethodGet, loopbackOrigin+"/connect/callback?state="+state, nil))
	if f := waitFlow(t, srv2, "echo", apitypes.SetupFlowStateFailed); textOf(f.Error) != errSignInStartAgain {
		t.Errorf("a return without the cookie: %q", textOf(f.Error))
	}
}

// Why this test exists: a service that takes a POST (GitHub's app
// manifest flow) is reached by a page whose form posts itself. The page
// must post exactly the step's values, escaped, to the step's address,
// load nothing, run only its own script, post nowhere else, send no
// referrer and never be cached; a browser that runs no scripts gets a
// button.
func TestSetupFlowGoByPost(t *testing.T) {
	srv, _, _ := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/settings/apps/new?state=s1", Post: url.Values{"manifest": {`{"name":"a\"<b>"}`}}}}, nil
	})
	startSetupFlow(t, srv, "echo")
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	rr, cookie := startPage(t, srv, f)
	body := rr.Body.String()
	csp := rr.Header().Get("Content-Security-Policy")
	if rr.Code != http.StatusOK || cookie == nil || rr.Header().Get("Cache-Control") != "no-store" || rr.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("start = %d, headers %v", rr.Code, rr.Header())
	}
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "form-action https://"+setupHost) || !strings.Contains(csp, "script-src 'sha256-") {
		t.Errorf("CSP = %q", csp)
	}
	if !strings.Contains(body, `action="https://service.example/settings/apps/new?state=s1"`) ||
		!strings.Contains(body, `name="manifest" value="{&#34;name&#34;:&#34;a\&#34;&lt;b&gt;&#34;}"`) ||
		!strings.Contains(body, `<button type="submit">Continue</button>`) || strings.Contains(body, "src=") {
		t.Errorf("page = %s", body)
	}
}

// Why this test exists: a Go with NoReturn (a settings page the owner
// acts on) is followed at once by Setup with Answer.Went as the browser
// leaves, so the flow can go on to a Wait; it sets no cookie, since
// nothing comes back, and leaves another flow's cookie alone.
func TestSetupFlowGoWithoutReturn(t *testing.T) {
	srv, fs, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		if n == 1 {
			return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/settings", NoReturn: true}}, nil
		}
		return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: "Save the settings."}}, nil
	})
	startSetupFlow(t, srv, "echo")
	fs.nextAnswer(t)
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	if !f.Step.NoReturn {
		t.Errorf("go = %+v", f.Step)
	}
	rr, cookie := startPage(t, srv, f)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "https://"+setupHost+"/settings" || cookie != nil {
		t.Errorf("start = %d %q, cookie %v", rr.Code, rr.Header().Get("Location"), cookie)
	}
	if a := fs.nextAnswer(t); !a.Went || a.Return != nil {
		t.Errorf("after the Go: %+v", a)
	}
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
}

// Why this test exists: the page follows its flow over a stream, which
// must give the state at once and each change after, and end when the
// flow is forgotten so the page reads it again; a stream with no flow
// is a 404.
func TestSetupFlowStream(t *testing.T) {
	release := make(chan struct{})
	srv, _, _ := setupServer(t, func(n int, _ context.Context, _ builtinintegrations.Connection, _ builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		<-release
		return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	get := func() *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+intBase+"/echo/setup/stream", nil)
		req.AddCookie(authCookie(t, srv))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := get(); resp.StatusCode != http.StatusNotFound {
		t.Errorf("no flow: %d", resp.StatusCode)
	}
	startSetupFlow(t, srv, "echo")
	resp := get()
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type %q", resp.Header.Get("Content-Type"))
	}
	events := make(chan string, 8)
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := resp.Body.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.Index(acc, "\n\n")
				if i < 0 {
					break
				}
				events <- acc[:i]
				acc = acc[i+2:]
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()
	next := func() string {
		select {
		case e, ok := <-events:
			if !ok {
				return ""
			}
			return e
		case <-time.After(failsafeWait):
			t.Fatal("no event")
		}
		return ""
	}
	if e := next(); e != "retry: 1500" {
		t.Errorf("first = %q", e)
	}
	if e := next(); !strings.HasPrefix(e, "event: flow\ndata: ") || !strings.Contains(e, `"state":"running"`) {
		t.Errorf("initial = %q", e)
	}
	close(release)
	if e := next(); !strings.Contains(e, `"state":"form"`) {
		t.Errorf("change = %q", e)
	}
	srv.setupCancel("echo")
	if e := next(); e != "" {
		t.Errorf("after the flow was forgotten: %q", e)
	}
}

// Why this test exists: Kivali Desktop sends a Go step to the system
// browser through the handoff with next=<flow id>. The handoff must
// sign that browser in, set the flow's cookie there and send it to
// /connect/start (not straight to the service), so a Go by POST works
// there too and the callback, which comes to that browser, passes the
// cookie check. A next naming no flow on a Go step, or a handoff at
// another address than the flow's callback, signs nothing in.
func TestSetupFlowThroughTheDesktopHandoff(t *testing.T) {
	srv, _, _ := setupServer(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Go: &builtinintegrations.Go{URL: "https://" + setupHost + "/go"}}, nil
	})
	srv.Handoff = &auth.Handoff{Codec: srv.AuthMW.Codec, Allowlist: auth.NewAllowlist(testOwnerEmail), Now: srv.clk().Now}
	srv.wireRoutes()
	startSetupFlow(t, srv, "echo")
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)

	rr := serveHandoff(t, srv, "127.0.0.1:8080", handoffToken(t, srv), f.ID)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/connect/start?flow="+f.ID || cookieOf(rr, "kivali_connect") == nil {
		t.Errorf("handoff = %d %q %v", rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
	assertHandoffRefused(t, serveHandoff(t, srv, "localhost:8080", handoffToken(t, srv), f.ID), "another address")
	assertHandoffRefused(t, serveHandoff(t, srv, "127.0.0.1:8080", handoffToken(t, srv), strings.Repeat("0", 32)), "no flow")
}

// Why this test exists: a setup that makes an app and hands over to
// core's sign-in (setup.then: browser) must go on, in the same flow,
// to the standard browser sign-in with the client id and secret setup
// kept in the connection's Secret, not the record's fields a pasted
// client id is in: otherwise the app setup made could never sign in,
// and the owner would be asked for an id and secret they never saw.
func TestSetupFlowHandsOverToASignIn(t *testing.T) {
	clk := clock.NewFakeAt(signInAt)
	vendor := connecttest.New(t, clk)
	srv, _, _ := setupServerWith(t, func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Done: true, Keep: map[string]string{"client_id": connecttest.ClientID, "client_secret": connecttest.ClientSecret}}, nil
	}, func(fs *fakeSetup) {
		fs.setup.Then = integration.MethodBrowser
		fs.setup.Keeps = append(fs.setup.Keeps, integration.SetupKeep{Key: "client_id", Words: "The app's client id"}, integration.SetupKeep{Key: "client_secret", Words: "The app's client secret"})
		ep := vendor.Endpoints()
		fs.edit = func(c *integration.Connection) {
			c.AuthorizationEndpoint, c.TokenEndpoint = ep.Authorization, ep.Token
			c.CredentialKeys = map[string]string{"access_token": "TOKEN"}
		}
	})
	srv.Clock = clk
	srv.Connect = &connect.Engine{Clock: clk, HTTP: vendor.Server.Client()}

	startSetupFlow(t, srv, "echo")
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	rr, cookie := startPage(t, srv, f)
	authorize, _ := url.Parse(rr.Header().Get("Location"))
	if authorize.Query().Get("client_id") != connecttest.ClientID {
		t.Fatalf("the sign-in went to %s", authorize)
	}
	landedOnSetup(t, callback(t, srv, vendorApprove(t, authorize), cookie), "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)
	rec, err := srv.Store.GetIntegrationConnection("echo")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := srv.Secrets.Get(context.Background(), rec.SecretName)
	if rec.SignIn == nil || rec.SignIn.Method != integration.MethodBrowser || !vendor.Valid(data[connect.SecretKeyAccessToken]) || data["client_id"] != connecttest.ClientID {
		t.Errorf("after the hand-over: %+v, %v", rec.SignIn, data)
	}
	if got := strings.Join(setupAudit(t, srv, "echo"), "|"); got != "started|returned|done" {
		t.Errorf("audit = %s", got)
	}
}
