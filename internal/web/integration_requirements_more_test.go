package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// failingDeletes is a Secrets store whose next deletes fail, as many as
// left says.
type failingDeletes struct {
	secrets.Store
	left atomic.Int32
}

func (f *failingDeletes) Delete(ctx context.Context, name string) error {
	if f.left.Add(-1) >= 0 {
		return errors.New("the cluster refused the delete")
	}
	return f.Store.Delete(ctx, name)
}

// keepThenFail is a setup script whose first call keeps an account and
// asks for a Form, and whose answer fails.
func keepThenFail(_ int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
	if last.Form == nil {
		return builtinintegrations.Step{Keep: map[string]string{"account": "half-made-app"}, Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
	}
	return builtinintegrations.Step{}, errors.New("the service refused")
}

// keepThenDone is a setup script of one Form, whose answer it keeps as
// the account and a token, then Done: an install in one step, for a
// test about what follows it.
func keepThenDone(_ int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
	if last.Form == nil {
		return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
	}
	return builtinintegrations.Step{Done: true, Keep: map[string]string{"account": last.Form["account"], "token": "s3cret-token"}}, nil
}

// answerEcho answers echo's Form with account.
func answerEcho(t *testing.T, srv *Server, account string) {
	t.Helper()
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": account}}); rr.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
	}
}

// Why this test exists: once a built-in's Uninstall has undone the
// installation on the service, a Secret Kivali cannot delete must not
// make the owner read "nothing was changed" (the service's side is
// gone), nor leave a record whose next Uninstall can never finish. The
// answer says the service's side was undone, the connection stays
// named so its Secret is not lost, and Uninstall again (the built-in
// treating the gone installation as done) completes it.
func TestDisconnectFinishesAfterASecretWouldNotGo(t *testing.T) {
	srv, e, _ := healthServer(t)
	mem := &failingDeletes{Store: secrets.NewMemory()}
	srv.Secrets = mem
	installEcho(t, srv, e, "acme")
	mem.left.Store(1)
	apiErr := assertAPIError(t, intDelete(t, srv, intBase+"/echo/connection"), http.StatusInternalServerError)
	if apiErr.Error != "Echo's installation was undone on the service, but Kivali could not remove its connection; disconnect it again to finish" {
		t.Errorf("first disconnect = %+v", apiErr)
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateConnected || page.Connection == nil {
		t.Fatalf("after the failed delete = %s, connections %v", page.Summary.State, page.Connection)
	}
	if rr := intDelete(t, srv, intBase+"/echo/connection"); rr.Code != http.StatusOK {
		t.Fatalf("second disconnect: %d %s", rr.Code, rr.Body.String())
	}
	calls := len(e.callsOf("uninstall"))
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateInstalled || page.Connection != nil || calls != 2 {
		t.Errorf("after the second = %s, connections %v, Uninstall called %d times", page.Summary.State, page.Connection, calls)
	}
	if got := auditActions(t, srv, "echo", disconnectAction); len(got) != 2 || got[1] != "" {
		t.Errorf("audit = %v", got)
	}
}

// Why this test exists: a second start of the install flow while the
// first has kept (made) the connection takes the install over. The
// connection must not be forgotten as the first ends, since the second
// carries on with it; the second's failure forgets it, and its success
// keeps it. Forgetting it at the takeover would leave the second flow
// keeping into a record that is gone.
func TestInstallTakeoverKeepsTheMadeConnection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second echotest.SetupScript
		end    apitypes.SetupFlowState
		conns  int
	}{
		{"the second fails", keepThenFail, apitypes.SetupFlowStateFailed, 0},
		{"the second is done", keepThenDone, apitypes.SetupFlowStateDone, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, e, _ := healthServer(t)
			e.ScriptSetup(keepThenFail)
			startSetupFlow(t, srv, "echo")
			waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
			e.ScriptSetup(tc.second)
			startSetupFlow(t, srv, "echo")
			waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
			srv.setupAwaitDiscard(context.Background(), "echo")
			if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateInstalled || page.Connection == nil {
				t.Fatalf("after the takeover = %s, connections %v", page.Summary.State, page.Connection)
			}
			answerEcho(t, srv, "acme")
			waitFlow(t, srv, "echo", tc.end)
			srv.setupAwaitDiscard(context.Background(), "echo")
			if page := echoPage(t, srv); (page.Connection != nil) != (tc.conns == 1) {
				t.Errorf("after the second ends = %s, connections %v", page.Summary.State, page.Connection)
			}
		})
	}
}

// Why this test exists: Disconnect while a connect has made its
// connection is the owner abandoning the install: the flow ends, the
// made connection and its Secret go, and the built-in is available,
// with nothing left for the flow's own discard to find.
func TestDisconnectWhileConnecting(t *testing.T) {
	srv, e, _ := healthServer(t)
	e.ScriptSetup(keepThenFail)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if rr := intDelete(t, srv, intBase+"/echo/connection"); rr.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	srv.setupAwaitDiscard(context.Background(), "echo")
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateInstalled || page.Connection != nil || page.Flow != nil {
		t.Errorf("after disconnect = %s, connections %v, flow %+v", page.Summary.State, page.Connection, page.Flow)
	}
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets left: %v", names)
	}
}

// Why this test exists: a failed install's connection may start going
// after a new start waited for it and before the start takes the lock.
// A flow started then would keep its values into the record the
// discard is about to delete, and end Done with no connection. The
// start must be refused while a discard is under way, and go through
// once it is over.
func TestSetupStartRefusedWhileAFailedInstallIsUndone(t *testing.T) {
	srv, e, _ := healthServer(t)
	e.ScriptSetup(keepThenFail)
	release := make(chan struct{})
	srv.setupRuns.discard = func(name, id string, keepRecord bool) {
		<-release
		srv.setupDiscard(name, id, keepRecord)
	}
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	answerEcho(t, srv, "acme")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)

	// A request whose wait for the discard is already over: its context
	// ended, as for a discard that started after the wait.
	b, _ := json.Marshal(apitypes.SetupFlowStart{Flow: "setup"})
	req := httptest.NewRequest(http.MethodPost, loopbackOrigin+intBase+"/echo/setup", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	if e := assertAPIError(t, serveSignedIn(t, srv, req.WithContext(ctx)), http.StatusConflict); e.Error != errSetupUndoing {
		t.Errorf("start while undoing = %+v", e)
	}
	close(release)
	srv.setupAwaitDiscard(context.Background(), "echo")
	e.ScriptSetup(nil)
	installEcho(t, srv, e, "acme")
	if page := echoPage(t, srv); !connectedAndMet(page) || page.Connection == nil {
		t.Errorf("installed after the undo = %s, connections %v", page.Summary.State, page.Connection)
	}
}

// Why this test exists: the owner's own save of the connection while an
// install runs makes that connection theirs. Forgetting it when the
// install then ends (cancelled by the save of a new client id, or
// cancelled later) would delete what the owner just entered.
func TestOwnersSaveKeepsAnInstallsConnection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clientID string
		cancel   bool
	}{
		{"a new client id cancels the sign-in", "another-client-id", false},
		{"the same app, cancelled later", connecttest.ClientID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, vendor, _ := signInServer(t)
			signInBundle(t, srv, vendor)
			startSetupFlow(t, srv, "crm")
			waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
			fields := map[string]string{"token": "pasted-field-value", "client_id": connecttest.ClientID, "client_secret": connecttest.ClientSecret}
			if rr := flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: fields}); rr.Code != http.StatusOK {
				t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
			}
			waitFlow(t, srv, "crm", apitypes.SetupFlowStateWait)
			if rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "pasted-field-value", "client_id": tc.clientID}}); rr.Code != http.StatusOK {
				t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
			}
			if tc.cancel {
				if rr := flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/cancel", nil); rr.Code != http.StatusNoContent {
					t.Fatalf("cancel: %d", rr.Code)
				}
			}
			srv.setupAwaitDiscard(context.Background(), "crm")
			rec, err := srv.Store.GetIntegrationConnection("crm")
			if err != nil || rec.Fields["client_id"] != tc.clientID {
				t.Errorf("the owner's saved connection = %+v, %v", rec, err)
			}
		})
	}
}

// tokenHealth is the token built-in echo with requirements of its own,
// whose Fix names a flow its token connection has no setup to run.
type tokenHealth struct {
	*echotest.Echo
}

func (tokenHealth) Requirements(context.Context, builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
	return []builtinintegrations.Requirement{
		{ID: "scopes", Title: "Scopes", Fix: "rescope"},
		{ID: "bad-fix", Title: "Bad", Fix: "not a flow"},
	}, nil
}

// Why this test exists: an integration's Fix is a flow name, and so is
// the owner's request, which comes back in the 409's words and the
// audit. A row whose Fix is not a flow's name is left out; a request
// for one is answered without its words echoed; and a Fix a token
// built-in names says its setup runs no flows of its own rather than
// that it has no such flow.
func TestFixNamesAreFlowNames(t *testing.T) {
	srv := newTestServer(t)
	srv.BuiltinIntegrations = builtinintegrations.Registry{tokenHealth{echotest.New()}}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	connectEcho(t, srv, "Hello")
	page := echoPage(t, srv)
	if _, ok := requirementOf(page, "bad-fix"); ok {
		t.Errorf("a row whose fix is not a flow's name = %v", requirementIDs(page))
	}
	start := func(flow string) *httptest.ResponseRecorder {
		return flowDo(t, srv, http.MethodPost, intBase+"/echo/setup", apitypes.SetupFlowStart{Flow: flow})
	}
	if e := assertAPIError(t, start("<b>call 555-0100</b>"), http.StatusConflict); e.Error != "that is not a flow Echo has" {
		t.Errorf("a flow that is not a name = %+v", e)
	}
	if e := assertAPIError(t, start("rescope"), http.StatusConflict); e.Error != "Echo's setup does not run its own flows" {
		t.Errorf("a token built-in's fix = %+v", e)
	}
}

// renamed is echo under another name, for a team with two.
type renamed struct {
	*echotest.Echo
	name string
}

func (r renamed) Manifest() *integration.Manifest {
	m := r.Echo.Manifest()
	m.Name, m.DisplayName = r.name, r.name
	return m
}

// installNamed runs name's install flow, scripted by keepThenDone, to
// its end.
func installNamed(t *testing.T, srv *Server, name string) {
	t.Helper()
	startSetupFlow(t, srv, name)
	waitFlow(t, srv, name, apitypes.SetupFlowStateForm)
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/"+name+"/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}); rr.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
	}
	waitFlow(t, srv, name, apitypes.SetupFlowStateDone)
}

// Why this test exists: the refresh asks every connected built-in for
// its own requirements, each with a five-second deadline. Asked one
// after another, a team with several slow integrations would hold a
// pass five seconds for each; asked at once, a pass waits five seconds
// at most. Both deadlines armed together, before either passes, is
// what shows they run at once. The list then shows what the pass
// found without asking, and an hour after the pass ended the next one
// asks again, so what the list shows cannot go stale for good.
func TestRefreshAsksEveryIntegrationAtOnce(t *testing.T) {
	srv, e, clk := healthServer(t)
	e.ScriptSetup(keepThenDone)
	other := echotest.New()
	other.SetupFlow = true
	other.ScriptSetup(keepThenDone)
	srv.BuiltinIntegrations = builtinintegrations.Registry{e.Echo, renamed{other, "second"}}
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	installNamed(t, srv, "echo")
	installNamed(t, srv, "second")
	// Each ask says it began, then waits for its deadline.
	called := make(chan struct{}, 4)
	slow := func(ctx context.Context, _ builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		called <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	e.ScriptRequirements(slow)
	other.ScriptRequirements(slow)
	bothAsked := func(when string) {
		t.Helper()
		for i := 0; i < 2; i++ {
			select {
			case <-called:
			case <-time.After(failsafeWait):
				t.Fatalf("%s, only %d of the two were asked", when, i)
			}
		}
	}
	stop := srv.StartRequirementsRefresh()
	t.Cleanup(stop)
	clk.BlockUntil(2) // both deadlines, armed together
	bothAsked("at the start")
	clk.Advance(requirementsTimeout)
	clk.BlockUntil(1) // the hour's timer: the pass is over
	list := orgOK[apitypes.Integrations](t, orgGet(t, srv, intBase))
	if len(list.Integrations) != 2 {
		t.Fatalf("list = %+v", list)
	}
	for _, sum := range list.Integrations {
		if sum.State != apitypes.IntegrationStateConnected || sum.Needs != 1 {
			t.Errorf("%s = %+v", sum.Name, sum)
		}
	}
	select {
	case <-called:
		t.Fatal("the list asked")
	default:
	}
	clk.Advance(requirementsRefreshEvery)
	clk.BlockUntil(2)
	bothAsked("an hour on")
	clk.Advance(requirementsTimeout)
	clk.BlockUntil(1)
}

// Why this test exists: the list and Home are read often, and asking
// the integration on each read (GitHub's health is four requests to
// GitHub) held them up for as long as the vendor took. The page, where
// the rows are shown and fixed, asks, and asks again once a few seconds
// have passed; the list and Home show what the integration last
// answered and never ask. A connection changed drops the answer, so
// the list shows no own rows until the page or the refresh asks again.
func TestListsShowTheLastAnswerWithoutAsking(t *testing.T) {
	srv, e, clk := healthServer(t)
	installEcho(t, srv, e, "acme")
	clk.Advance(requirementsCacheTTL) // past whatever the install's end read
	var asked atomic.Int32
	e.ScriptRequirements(func(context.Context, builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		asked.Add(1)
		return []builtinintegrations.Requirement{{ID: "permissions", Title: "Permissions"}}, nil
	})
	srv.Integrations.ConnectionChanged("echo") // nothing known
	if sum := echoSummary(t, srv); sum.Needs != 0 {
		t.Errorf("before any ask, the list needs %d", sum.Needs)
	}
	if needs := requirementNeeds(t, srv); len(needs) != 0 {
		t.Errorf("before any ask, Needs you = %v", needs)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the list and Home asked %d times", n)
	}
	echoPage(t, srv)
	if n := asked.Load(); n != 1 {
		t.Fatalf("the page asked %d times", n)
	}
	if sum := echoSummary(t, srv); sum.Needs != 1 {
		t.Errorf("after the page, the list needs %d", sum.Needs)
	}
	if needs := requirementNeeds(t, srv); len(needs) != 1 {
		t.Errorf("after the page, Needs you = %v", needs)
	}
	clk.Advance(requirementsCacheTTL)
	echoSummary(t, srv)
	requirementNeeds(t, srv)
	if n := asked.Load(); n != 1 {
		t.Errorf("after the time is up, the list and Home asked: %d times in all", n)
	}
	echoPage(t, srv)
	if n := asked.Load(); n != 2 {
		t.Errorf("after the time is up, the page asked: %d times in all", n)
	}
	srv.Integrations.ConnectionChanged("echo")
	if sum := echoSummary(t, srv); sum.Needs != 0 || asked.Load() != 2 {
		t.Errorf("after the connection changed, the list needs %d, asked %d times", sum.Needs, asked.Load())
	}
	srv.refreshRequirements(context.Background())
	if sum := echoSummary(t, srv); sum.Needs != 1 || asked.Load() != 3 {
		t.Errorf("after the refresh, the list needs %d, asked %d times", sum.Needs, asked.Load())
	}
}
