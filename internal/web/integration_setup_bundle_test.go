package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The tests here drive a bundle's setup, requirements and uninstall
// over MCP, as core sends them through its pod's shim
// (integration_setup_bundle.go): a fake shim answers for the bundle's
// server, so the requests and answers are exactly what a pod carries.

// bundleSetupManifest is crm made a setup bundle: the account and a
// setup-only key asked for, the browser sent to setupHost alone, and
// the token and the account kept, the token put in the server's env.
func bundleSetupManifest() map[string]any {
	m := integrationtest.Manifest("crm", "1.0.0")
	m["user_config"] = map[string]any{}
	m["server"].(map[string]any)["mcp_config"].(map[string]any)["env"] = map[string]any{"TOKEN": "${user_config.token}"}
	m["kivali"].(map[string]any)["connection"] = map[string]any{
		"method": "setup",
		"setup": map[string]any{
			"summary": "Installs the crm app on your " + setupHost + " account.",
			"hosts":   []any{setupHost},
			"fields": map[string]any{
				"account": map[string]any{"words": "Your account."},
				"key":     map[string]any{"words": "From the account's key page.", "secret": true, "setup_only": true},
			},
			"keeps": map[string]any{"token": "The app's token", "account": "The account the app is on"},
		},
	}
	return m
}

// podProgress sends one progress notification of the call it was given
// for, as the bundle's server writes it.
type podProgress func(words string, done, total float64)

// podStep answers the bundle's nth kivali/setup call, from 1: a result,
// an error, or ok false for no answer at all.
type podStep func(n int, p integrationpod.SetupParams, progress podProgress) (result any, rpcErr *integrationpod.RPCError, ok bool)

// setupPod is a setup bundle's server as a fake shim answers for it.
type setupPod struct {
	reg   *integrationRegistry
	name  string
	calls chan integrationpod.SetupParams

	mu        sync.Mutex
	step      podStep
	n         int
	methods   []string
	rows      func() (any, *integrationpod.RPCError)
	uninstall func() *integrationpod.RPCError
}

// stepOf is a kivali/setup result holding step.
func stepOf(step map[string]any) map[string]any { return map[string]any{"step": step} }

func (sp *setupPod) answer(m integrationpod.Message) (any, *integrationpod.RPCError, bool) {
	sp.mu.Lock()
	sp.methods = append(sp.methods, m.Method)
	rows, uninstall := sp.rows, sp.uninstall
	sp.mu.Unlock()
	switch m.Method {
	case "ping":
		return map[string]any{}, nil, true
	case "tools/list":
		return map[string]any{"tools": []any{}}, nil, true
	case integrationpod.MethodSetup:
		var p integrationpod.SetupParams
		_ = json.Unmarshal(m.Params, &p)
		sp.mu.Lock()
		sp.n++
		n, step := sp.n, sp.step
		sp.mu.Unlock()
		sp.calls <- p
		progress := func(words string, done, total float64) {
			tok, _ := json.Marshal(p.Meta.ProgressToken)
			params, _ := json.Marshal(integrationpod.ProgressParams{ProgressToken: tok, Progress: done, Total: &total, Message: words})
			sp.reg.onMessage(context.Background(), sp.name, integrationpod.Message{JSONRPC: "2.0", Method: integrationpod.MethodProgress, Params: params})
		}
		return step(n, p, progress)
	case integrationpod.MethodRequirements:
		if rows == nil {
			return map[string]any{"requirements": []any{}}, nil, true
		}
		res, rpcErr := rows()
		return res, rpcErr, true
	case integrationpod.MethodUninstall:
		if uninstall != nil {
			if rpcErr := uninstall(); rpcErr != nil {
				return nil, rpcErr, true
			}
		}
		return map[string]any{}, nil, true
	}
	return nil, &integrationpod.RPCError{Code: integrationpod.CodeMethodNotFound, Message: "method not found"}, true
}

// next is the params of the bundle's next kivali/setup call.
func (sp *setupPod) next(t *testing.T) integrationpod.SetupParams {
	t.Helper()
	select {
	case p := <-sp.calls:
		return p
	case <-time.After(failsafeWait):
		t.Fatal("kivali/setup was not sent")
	}
	return integrationpod.SetupParams{}
}

// sent counts the requests of method the server was sent.
func (sp *setupPod) sent(method string) int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	n := 0
	for _, m := range sp.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (sp *setupPod) set(edit func(sp *setupPod)) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	edit(sp)
}

// bundleSetupServer is a server on a fake clock with crm uploaded and
// approved as a setup bundle, its Secrets in memory, and its server
// answered by a setupPod whose steps step decides; the pod's shim is
// connected when attach is true.
func bundleSetupServer(t *testing.T, step podStep, attach bool) (*Server, *setupPod, *fakeShim, *clock.Fake, *secrets.Memory) {
	t.Helper()
	srv := newTestServer(t)
	clk := clock.NewFakeAt(signInAt)
	srv.Clock = clk
	mem := secrets.NewMemory()
	srv.Secrets = mem
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intUploadApprove(t, srv, bundleSetupManifest())
	pod := &setupPod{reg: srv.Integrations, name: "crm", step: step, calls: make(chan integrationpod.SetupParams, 64)}
	var fs *fakeShim
	if attach {
		fs = attachShim(t, srv, "crm", pod.answer)
	}
	return srv, pod, fs, clk, mem
}

// installBundle runs crm's install as one step that keeps the token and
// the account, and waits for it to be done.
func installBundle(t *testing.T, srv *Server, pod *setupPod) {
	t.Helper()
	startSetupFlow(t, srv, "crm")
	pod.next(t)
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateDone)
}

// keepAndDone is a pod whose first call keeps a token and the account
// and is done, and whose later calls (a Fix) are done.
func keepAndDone(n int, _ integrationpod.SetupParams, _ podProgress) (any, *integrationpod.RPCError, bool) {
	if n == 1 {
		return stepOf(map[string]any{"done": true, "keep": map[string]any{"token": "t0k3n-xyz", "account": "acme"}}), nil, true
	}
	return stepOf(map[string]any{"done": true}), nil, true
}

// waitProgress waits for name's flow to show progress, watching its
// stream meanwhile, and returns it.
func waitProgress(t *testing.T, srv *Server, name string) apitypes.SetupFlowProgress {
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
		if f.Progress != nil {
			return *f.Progress
		}
		select {
		case b = <-ch:
		case <-deadline:
			t.Fatalf("%s's flow showed no progress: %+v", name, f)
		}
	}
}

// bundleCheck has crm's Wait checked once, with a stream watching it.
func bundleCheck(t *testing.T, srv *Server, pod *setupPod, clk *clock.Fake) integrationpod.SetupParams {
	t.Helper()
	run, _, ch, ok := srv.setupSubscribe("crm")
	if !ok {
		t.Fatal("crm has no setup flow")
	}
	defer srv.setupUnsubscribe(run, ch)
	// Every call before the check has returned and been applied: a call
	// the pod has answered still holds its deadline timer on the clock
	// until it returns, and that timer would answer BlockUntil before
	// the wait loop arms its own.
	callsEnd(t, run)
	clk.BlockUntil(1)
	clk.Advance(setupWaitEvery)
	return pod.next(t)
}

// Why this test exists: this is a bundle's whole install, over MCP, as
// the owner takes it: kivali/setup sent with the flow and an Answer
// carrying every part a built-in's Setup is given (the form, the state,
// the return, the check, the leg, the callback and the team's name)
// under the wire's names; its progress notifications shown as the
// flow's stage while the step runs; a Form of the declared fields; a
// Go whose /connect/start posts to the declared host; the callback's
// query reaching the next call; a Wait checked while watched; each Keep
// written into the connection's Secret and restarting the server with
// it; and an audit row per event. A break anywhere is a bundle whose setup cannot
// finish.
func TestBundleSetupInstalls(t *testing.T) {
	release := make(chan struct{})
	wait := map[string]any{"words": "Approve the app there.", "url": "https://" + setupHost + "/apps/crm"}
	srv, pod, fs, clk, mem := bundleSetupServer(t, func(n int, p integrationpod.SetupParams, progress podProgress) (any, *integrationpod.RPCError, bool) {
		switch n {
		case 1:
			return stepOf(map[string]any{"form": map[string]any{"words": "Name the account and paste its key.", "fields": []any{"account", "key"}}, "state": map[string]any{"at": "form"}}), nil, true
		case 2:
			progress("Checking the key", 1, 2)
			<-release
			return stepOf(map[string]any{
				"go":    map[string]any{"words": "Approve the app.", "url": "https://" + setupHost + "/install", "post": map[string]any{"key": p.Answer.Form["key"], "redirect_url": p.Answer.Callback}},
				"keep":  map[string]any{"account": p.Answer.Form["account"]},
				"state": map[string]any{"at": "go", "key": p.Answer.Form["key"]},
			}), nil, true
		case 3:
			return stepOf(map[string]any{"wait": wait, "keep": map[string]any{"token": "tok-" + p.Answer.Return["code"][0]}, "state": map[string]any{"at": "wait"}}), nil, true
		case 4:
			return stepOf(map[string]any{"wait": wait, "state": map[string]any{"at": "wait"}}), nil, true
		}
		return stepOf(map[string]any{"done": true}), nil, true
	}, true)

	if err := srv.Store.WriteCompanyName("Acme"); err != nil {
		t.Fatal(err)
	}
	flow := startSetupFlow(t, srv, "crm")
	first := pod.next(t)
	if first.Flow != "setup" || first.Answer.Leg != 1 || len(first.Answer.State) != 0 || len(first.Answer.Form) != 0 || len(first.Answer.Return) != 0 ||
		first.Answer.Checked || !strings.HasPrefix(first.Answer.Callback, loopbackOrigin+"/connect/callback?state=") || first.Meta == nil || first.Meta.ProgressToken == "" || first.Answer.Team != "Acme" {
		t.Errorf("first call = %+v", first)
	}
	f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
	if f.ID != flow.ID || f.Step.Words != "Name the account and paste its key." || len(f.Step.Fields) != 2 || f.Step.Fields[0].Key != "account" || !f.Step.Fields[1].Secret || !f.Step.Fields[1].SetupOnly {
		t.Errorf("form = %+v", f.Step)
	}

	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": " acme ", "key": "pak-1234567"}}))
	second := pod.next(t)
	if second.Answer.Form["account"] != "acme" || second.Answer.Form["key"] != "pak-1234567" || second.Answer.State["at"] != "form" || second.Answer.Leg != 2 {
		t.Errorf("the form reached the bundle as %+v", second.Answer)
	}
	if got := waitProgress(t, srv, "crm"); got != (apitypes.SetupFlowProgress{Words: "Checking the key", Done: 1, Total: 2}) {
		t.Errorf("progress = %+v", got)
	}
	close(release)
	f = waitFlow(t, srv, "crm", apitypes.SetupFlowStateGo)
	// The Keep restarts the server with what it kept. The next call
	// goes on the same events stream after the restart, and in a pod the
	// shim queues it until the new server has started (Shim.loop), so it
	// reaches the server that holds the kept values; the fake shim here
	// answers at once.
	fs.next(t, integrationpod.EventRestart)
	if conn := srv.setupConnection(context.Background(), "crm"); conn.Fields["account"] != "acme" || conn.Fields["key"] != "" {
		t.Errorf("kept at the Go = %v", conn.Fields)
	}
	rr, cookie := startPage(t, srv, f)
	if body := rr.Body.String(); rr.Code != http.StatusOK || cookie == nil || !strings.Contains(body, `action="https://`+setupHost+`/install"`) || !strings.Contains(body, `name="key" value="pak-1234567"`) {
		t.Fatalf("start = %d, cookie %v, page %s", rr.Code, cookie, body)
	}

	back := mustURL(t, second.Answer.Callback)
	q := back.Query()
	q.Set("code", "c0de-9")
	back.RawQuery = q.Encode()
	if id := landedOnSetup(t, callback(t, srv, back, cookie), "crm"); id != flow.ID {
		t.Errorf("landed on flow %s", id)
	}
	third := pod.next(t)
	if !slices.Equal(third.Answer.Return["code"], []string{"c0de-9"}) || len(third.Answer.Return["state"]) != 1 || third.Answer.State["key"] != "pak-1234567" || third.Answer.Leg != 3 {
		t.Errorf("the return reached the bundle as %+v", third.Answer)
	}
	f = waitFlow(t, srv, "crm", apitypes.SetupFlowStateWait)
	if textOf(f.Step.URL) != "https://"+setupHost+"/apps/crm" || textOf(f.Step.Host) != setupHost {
		t.Errorf("wait = %+v", f.Step)
	}
	if a := bundleCheck(t, srv, pod, clk); !a.Answer.Checked || a.Answer.Leg != 4 {
		t.Errorf("check = %+v", a.Answer)
	}
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateWait)
	bundleCheck(t, srv, pod, clk)
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateDone)

	rec, err := srv.Store.GetIntegrationConnection("crm")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := mem.Get(context.Background(), rec.SecretName)
	if data["token"] != "tok-c0de-9" || data["account"] != "acme" || data["key"] != "" || !slices.Equal(rec.SecretKeys, []string{"account", "token"}) {
		t.Errorf("kept %v, keys %v", data, rec.SecretKeys)
	}
	if got := strings.Join(setupAudit(t, srv, "crm"), "|"); got != "started|returned|done" {
		t.Errorf("audit = %s", got)
	}
	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil || cred.Env["TOKEN"] != "tok-c0de-9" || cred.Fields["account"] != "acme" {
		t.Errorf("the server is started with %+v, %v", cred, rpcErr)
	}
	if page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm")); !installedAndMet(page) {
		t.Errorf("after setup = %s, unmet %v", page.Summary.State, unmetIDs(page))
	}
}

// Why this test exists: a bundle's code comes from someone else, and
// its steps are held to its manifest's declarations exactly as a
// built-in's are: a step that sends the browser to another host, asks
// for another field or keeps another key fails the flow in words naming
// what it tried, before anything of it is kept. So does a step Kivali
// cannot read (a misspelt key would drop part of it) and a JSON-RPC
// error, whose message is the owner's words, scrubbed of the key the
// owner entered.
func TestBundleSetupRefusedSteps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		step   map[string]any
		rpcErr *integrationpod.RPCError
		want   string
	}{
		{"host", map[string]any{"go": map[string]any{"url": "https://evil.example/login"}}, nil, "Test crm's setup tried to send your browser to evil.example, which its manifest does not declare"},
		{"field", map[string]any{"form": map[string]any{"fields": []any{"password"}}}, nil, "Test crm's setup asked for password, which its manifest does not declare"},
		{"keep", map[string]any{"done": true, "keep": map[string]any{"password": "hunter22"}}, nil, "Test crm's setup tried to keep password, which its manifest does not declare"},
		{"unreadable", map[string]any{"go": map[string]any{"url": "https://" + setupHost + "/x", "noreturn": true}}, nil, `Test crm's setup answered with a step Kivali cannot read: step: json: unknown field "noreturn"`},
		{"no step", map[string]any{}, nil, "Test crm's setup gave no next step"},
		{"long address", map[string]any{"go": map[string]any{"url": "https://" + setupHost + "/" + strings.Repeat("a", setupURLMax)}}, nil, "Test crm's setup named an address longer than 4096 characters"},
		{"too much state", map[string]any{"form": map[string]any{"fields": []any{"key"}}, "state": map[string]any{"blob": strings.Repeat("x", setupCarryMax)}}, nil, "Test crm's setup gave more to keep and carry than Kivali holds for a step (64 KiB)"},
		{"too much to post", map[string]any{"go": map[string]any{"url": "https://" + setupHost + "/x", "post": map[string]any{"blob": strings.Repeat("x", setupCarryMax)}}}, nil, "Test crm's setup gave a form to post larger than Kivali sends (64 KiB)"},
		{"error", nil, &integrationpod.RPCError{Code: -32000, Message: "setup.example refused the key pak-1234567"}, "setup.example refused the key " + integrationSecretPlaceholder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, pod, _, _, _ := bundleSetupServer(t, func(n int, _ integrationpod.SetupParams, _ podProgress) (any, *integrationpod.RPCError, bool) {
				if n == 1 {
					return stepOf(map[string]any{"form": map[string]any{"fields": []any{"key"}}}), nil, true
				}
				if tc.rpcErr != nil {
					return nil, tc.rpcErr, true
				}
				return stepOf(tc.step), nil, true
			}, true)
			startSetupFlow(t, srv, "crm")
			waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
			orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"key": "pak-1234567"}}))
			pod.next(t)
			f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateFailed)
			if textOf(f.Error) != tc.want {
				t.Errorf("error = %q, want %q", textOf(f.Error), tc.want)
			}
			if _, err := srv.Store.GetIntegrationConnection("crm"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("a refused step kept a connection: %v", err)
			}
			if got := setupAudit(t, srv, "crm"); len(got) != 2 || got[1] != "refused failed: "+tc.want {
				t.Errorf("audit = %q", got)
			}
		})
	}
}

// Why this test exists: a bundle's step is someone else's text, and its
// server may write lines of 16 MiB. The page shows a step's words cut
// to 200 runes, scrubbed first so no piece of a secret is left by the
// cut, and a Wait's code cut to 64 and shown as given: the step chose to
// show it, for the owner to enter on the service's page, and a code
// that happens to equal a value Kivali scrubs elsewhere (one the owner
// typed, as here) must not read as [secret] there.
func TestBundleSetupStepWordsAreBounded(t *testing.T) {
	srv, pod, _, _, _ := bundleSetupServer(t, func(n int, p integrationpod.SetupParams, _ podProgress) (any, *integrationpod.RPCError, bool) {
		if n == 1 {
			return stepOf(map[string]any{"form": map[string]any{"fields": []any{"key"}}}), nil, true
		}
		return stepOf(map[string]any{"wait": map[string]any{
			"words": p.Answer.Form["key"] + strings.Repeat(" more", 300),
			"code":  p.Answer.Form["key"] + strings.Repeat("C", 100),
		}}), nil, true
	}, true)
	startSetupFlow(t, srv, "crm")
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"key": "pak-1234567"}}))
	pod.next(t)
	pod.next(t)
	f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateWait)
	words, code := f.Step.Words, textOf(f.Step.Code)
	if len([]rune(words)) != signInWordsRunes || !strings.HasPrefix(words, integrationSecretPlaceholder+" more") {
		t.Errorf("words = %d runes: %.40q", len([]rune(words)), words)
	}
	if len([]rune(code)) != setupCodeRunes || !strings.HasPrefix(code, "pak-1234567CCC") {
		t.Errorf("code = %q", code)
	}
}

// Why this test exists: a bundle's server that never answers a setup
// step must not leave the flow running for ever: the step fails at the
// runner's deadline, on the server's clock, in the runner's words, as a
// built-in's would.
func TestBundleSetupStepTimesOut(t *testing.T) {
	srv, pod, _, clk, _ := bundleSetupServer(t, func(int, integrationpod.SetupParams, podProgress) (any, *integrationpod.RPCError, bool) {
		return nil, nil, false
	}, true)
	startSetupFlow(t, srv, "crm")
	pod.next(t)
	clk.BlockUntil(1)
	clk.Advance(setupCallTimeout)
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateFailed); textOf(f.Error) != errSetupTimeout {
		t.Errorf("after the deadline: %q", textOf(f.Error))
	}
}

// Why this test exists: installing a bundle that is switched off
// switches it on, and its pod takes a while to start. The first step
// must wait for the server rather than fail at once (the owner would
// see every first install of a switched-off bundle fail), and must
// still fail, saying why, when the server does not come up.
func TestBundleSetupWaitsForItsServer(t *testing.T) {
	srv, pod, _, clk, _ := bundleSetupServer(t, func(int, integrationpod.SetupParams, podProgress) (any, *integrationpod.RPCError, bool) {
		return stepOf(map[string]any{"form": map[string]any{"fields": []any{"account"}}}), nil, true
	}, false)
	startSetupFlow(t, srv, "crm")
	// The call's deadline and the wait for the server.
	clk.BlockUntil(2)
	attachShim(t, srv, "crm", pod.answer)
	pod.next(t)
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)

	srv, _, _, clk, _ = bundleSetupServer(t, nil, false)
	startSetupFlow(t, srv, "crm")
	clk.BlockUntil(2)
	clk.Advance(bundleStartWait)
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateFailed); textOf(f.Error) != "Test crm is not running, so its setup cannot go on; its log says why. Start the setup again once it runs" {
		t.Errorf("never running: %q", textOf(f.Error))
	}
}

// Why this test exists: a setup bundle's own rows are its health, read
// from kivali/requirements after Kivali's, scrubbed of the connection's
// secrets, each unmet one a Needs-you row, and a row's Fix runs its
// flow through kivali/setup. A server that cannot give them is one
// unmet row saying why; a server not running is not asked (the running
// row says so) rather than holding the page up until the request times
// out.
func TestBundleRequirementsAndFix(t *testing.T) {
	srv, pod, fs, _, _ := bundleSetupServer(t, keepAndDone, true)
	installBundle(t, srv, pod)
	pod.set(func(sp *setupPod) {
		sp.rows = func() (any, *integrationpod.RPCError) {
			return map[string]any{"requirements": []any{
				map[string]any{"id": "token", "title": "Token", "words": "The token works.", "met": true},
				map[string]any{"id": "scopes", "title": "Scopes", "words": "t0k3n-xyz lacks deals.write; paste a new key.", "met": false, "fix": "rescope"},
			}}, nil
		}
	})
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if got := requirementIDs(page); !slices.Equal(got, []string{"version", "connection", "running", "token", "scopes"}) {
		t.Errorf("rows = %v", got)
	}
	if r, _ := requirementOf(page, "scopes"); r.Met || r.Fix != "rescope" || r.Words != integrationSecretPlaceholder+" lacks deals.write; paste a new key." {
		t.Errorf("scopes = %+v", r)
	}
	if _, ok := requirementNeeds(t, srv)["integration_requirement:crm/scopes"]; !ok {
		t.Error("the unmet row is not a Needs-you row")
	}

	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup", apitypes.SetupFlowStart{Flow: "rescope"}))
	if p := pod.next(t); p.Flow != "rescope" || p.Answer.Leg != 1 {
		t.Errorf("the Fix's call = %+v", p)
	}
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateDone)

	pod.set(func(sp *setupPod) {
		sp.rows = func() (any, *integrationpod.RPCError) {
			return nil, &integrationpod.RPCError{Code: -32000, Message: "the service did not answer"}
		}
	})
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if r, ok := requirementOf(page, reqChecks); !ok || r.Met || r.Words != "Test crm could not check its requirements: the service did not answer." {
		t.Errorf("checks = %+v", r)
	}

	srv.Integrations.disconnect(fs.link)
	asked := pod.sent(integrationpod.MethodRequirements)
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if got := unmetIDs(page); !slices.Equal(got, []string{"running"}) || pod.sent(integrationpod.MethodRequirements) != asked {
		t.Errorf("not running: unmet %v, asked %d more times", got, pod.sent(integrationpod.MethodRequirements)-asked)
	}
	// Its Fix has no rows to come from and no server to run in: the
	// owner reads why, not that the flow is not one it has.
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup", apitypes.SetupFlowStart{Flow: "rescope"}), http.StatusConflict); e.Error != errBundleNotRunning("Test crm") {
		t.Errorf("a Fix while not running = %+v", e)
	}
}

// Why this test exists: each unmet row of an integration's own is a
// Needs-you row, and a bundle's server is someone else's code: one that
// answered hundreds of unmet rows would bury everything else on Home.
// Kivali keeps the first 20.
func TestBundleRequirementsAreCapped(t *testing.T) {
	srv, pod, _, _, _ := bundleSetupServer(t, keepAndDone, true)
	installBundle(t, srv, pod)
	pod.set(func(sp *setupPod) {
		sp.rows = func() (any, *integrationpod.RPCError) {
			var rows []any
			for i := range 25 {
				rows = append(rows, map[string]any{"id": fmt.Sprintf("r%d", i), "title": "Row", "words": "Not met.", "met": false})
			}
			return map[string]any{"requirements": rows}, nil
		}
	})
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	ids := requirementIDs(page)
	if !slices.Contains(ids, "r19") || slices.Contains(ids, "r20") || len(requirementNeeds(t, srv)) != requirementRowsMax {
		t.Errorf("rows = %v, needs %d", ids, len(requirementNeeds(t, srv)))
	}
}

// Why this test exists: a server that does not implement a setup
// request, or stops while it is being asked, must fail in words the
// owner can act on, not in the protocol's own ("method not found") or
// after the five minutes a step may take.
func TestBundleServerFailuresInWords(t *testing.T) {
	srv, pod, _, _, _ := bundleSetupServer(t, func(int, integrationpod.SetupParams, podProgress) (any, *integrationpod.RPCError, bool) {
		return nil, &integrationpod.RPCError{Code: integrationpod.CodeMethodNotFound, Message: "method not found: kivali/setup"}, true
	}, true)
	startSetupFlow(t, srv, "crm")
	pod.next(t)
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateFailed); textOf(f.Error) != "Test crm's server does not answer kivali/setup; its author must add it" {
		t.Errorf("not implemented: %q", textOf(f.Error))
	}

	var fs *fakeShim
	srv, pod, fs, _, _ = bundleSetupServer(t, func(int, integrationpod.SetupParams, podProgress) (any, *integrationpod.RPCError, bool) {
		return nil, nil, false
	}, true)
	startSetupFlow(t, srv, "crm")
	pod.next(t)
	srv.Integrations.disconnect(fs.link)
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateFailed); textOf(f.Error) != "Test crm stopped before it answered" {
		t.Errorf("stopped mid-step: %q", textOf(f.Error))
	}

	srv, pod, _, _, _ = bundleSetupServer(t, keepAndDone, true)
	installBundle(t, srv, pod)
	pod.set(func(sp *setupPod) {
		sp.uninstall = func() *integrationpod.RPCError {
			return &integrationpod.RPCError{Code: integrationpod.CodeMethodNotFound, Message: "method not found"}
		}
	})
	if e := assertAPIError(t, intDelete(t, srv, intBase+"/crm"), http.StatusBadGateway); e.Error != "Test crm could not undo its installation: Test crm's server does not answer kivali/uninstall; its author must add it; nothing was changed" {
		t.Errorf("uninstall not implemented = %+v", e)
	}
}

// Why this test exists: Uninstall asks a setup bundle to undo its
// installation on the service before Kivali forgets anything; its error
// is the owner's to read, scrubbed, and leaves everything as it was, as
// a built-in's does. A bundle switched on whose server is not running
// is refused, since uninstalling it then would leave its app on the
// service by accident; one switched off has no server to ask, and is
// uninstalled all the same: switching a bundle off is how one whose
// server fails can still be removed.
func TestBundleUninstall(t *testing.T) {
	srv, pod, _, _, mem := bundleSetupServer(t, keepAndDone, true)
	installBundle(t, srv, pod)
	pod.set(func(sp *setupPod) {
		sp.uninstall = func() *integrationpod.RPCError {
			return &integrationpod.RPCError{Code: -32000, Message: "setup.example kept the app of token t0k3n-xyz"}
		}
	})
	apiErr := assertAPIError(t, intDelete(t, srv, intBase+"/crm"), http.StatusBadGateway)
	if apiErr.Error != "Test crm could not undo its installation: setup.example kept the app of token "+integrationSecretPlaceholder+"; nothing was changed" {
		t.Errorf("refusal = %+v", apiErr)
	}
	if page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm")); page.Summary.State != apitypes.IntegrationStateInstalled || len(mem.Names()) != 1 {
		t.Errorf("after the refusal = %s, secrets %v", page.Summary.State, mem.Names())
	}
	pod.set(func(sp *setupPod) { sp.uninstall = nil })
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if n := pod.sent(integrationpod.MethodUninstall); n != 2 || len(mem.Names()) != 0 {
		t.Errorf("kivali/uninstall sent %d times, secrets %v", n, mem.Names())
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/crm"), http.StatusNotFound)

	// Switched on and not running: the server may be about to run, so
	// the uninstall waits for it, and nothing changes.
	var fs *fakeShim
	srv, pod, fs, _, mem = bundleSetupServer(t, keepAndDone, true)
	installBundle(t, srv, pod)
	srv.Integrations.disconnect(fs.link)
	apiErr = assertAPIError(t, intDelete(t, srv, intBase+"/crm"), http.StatusConflict)
	if apiErr.Error != "Test crm is not running, so it cannot undo its installation on the service; switch it off to uninstall it without that" || len(mem.Names()) != 1 {
		t.Errorf("not running = %+v, secrets %v", apiErr, mem.Names())
	}

	// Switched off: no server to ask, so the service's side is left as
	// it is, and the rest goes.
	srv, pod, _, _, mem = bundleSetupServer(t, keepAndDone, true)
	installBundle(t, srv, pod)
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/disable"))
	if err := srv.Integrations.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall while off: %d %s", rr.Code, rr.Body.String())
	}
	if n := pod.sent(integrationpod.MethodUninstall); n != 0 || len(mem.Names()) != 0 {
		t.Errorf("off: kivali/uninstall sent %d times, secrets %v", n, mem.Names())
	}
}

// Why this test exists: a progress notification reaches only the
// request whose token it names, from the pod that request went to, and
// only while the request waits: a pod naming another integration's
// token, a notification Kivali cannot read, or a late one after its
// step answered, must not move a flow's progress bar.
func TestProgressReachesOnlyItsRequest(t *testing.T) {
	srv := newTestServer(t)
	reg, _ := withIntegrationRegistry(srv, newFakeIntegrationProv())
	var got []string
	token, stop := reg.watchProgress("crm", func(p integrationpod.ProgressParams) { got = append(got, p.Message) })
	note := func(name, token, words string) {
		tok, _ := json.Marshal(token)
		params, _ := json.Marshal(integrationpod.ProgressParams{ProgressToken: tok, Progress: 1, Message: words})
		if reply := reg.onMessage(context.Background(), name, integrationpod.Message{JSONRPC: "2.0", Method: integrationpod.MethodProgress, Params: params}); reply != nil {
			t.Errorf("a notification was answered: %s", reply)
		}
	}
	note("crm", token, "mine")
	note("other", token, "another pod's")
	note("crm", "kivali-progress-99", "another request's")
	// A token that is not text, and a progress that is not a number,
	// are not ones Kivali reads.
	for _, raw := range []string{`{"progressToken": 7, "progress": 1, "message": "a number"}`, `{"progressToken": "` + token + `", "progress": "half", "message": "not a number"}`} {
		reg.onMessage(context.Background(), "crm", integrationpod.Message{JSONRPC: "2.0", Method: integrationpod.MethodProgress, Params: json.RawMessage(raw)})
	}
	stop()
	note("crm", token, "late")
	if !slices.Equal(got, []string{"mine"}) {
		t.Errorf("delivered %q", got)
	}
}
