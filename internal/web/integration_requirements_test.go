package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// healthServer is setupServer running echo's own flow, with its
// permissions granted so that it is healthy once installed.
func healthServer(t *testing.T) (*Server, *setupEcho, *clock.Fake) {
	t.Helper()
	srv, e, clk := setupServer(t, nil)
	e.SetPermissions(true)
	return srv, e, clk
}

// echoPage is echo's page as it stands.
func echoPage(t *testing.T, srv *Server) apitypes.IntegrationPage {
	t.Helper()
	return orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/echo"))
}

// echoKey is the key installEcho enters on echo's Form.
const echoKey = "pak-123456"

// echoGo takes echo's flow from its Form, answered with account and
// echoKey, to its Go, and returns the flow there and the flow's cookie
// /connect/start set.
func echoGo(t *testing.T, srv *Server, account string) (apitypes.SetupFlow, *http.Cookie) {
	t.Helper()
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": account, "key": echoKey}}); rr.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
	}
	f := waitFlow(t, srv, "echo", apitypes.SetupFlowStateGo)
	rr, cookie := startPage(t, srv, f)
	if rr.Code != http.StatusOK || cookie == nil {
		t.Fatalf("start = %d, cookie %v", rr.Code, cookie)
	}
	return f, cookie
}

// echoBack is setup.example sending the browser back from echo's Go
// with query added to the callback echo gave it, and returns where
// Kivali sends the browser on.
func echoBack(t *testing.T, srv *Server, e *setupEcho, cookie *http.Cookie, query url.Values) *url.URL {
	t.Helper()
	calls := e.callsOf("setup")
	back := mustURL(t, calls[len(calls)-1].Answer.Callback)
	q := back.Query()
	for k, v := range query {
		q[k] = v
	}
	back.RawQuery = q.Encode()
	return callback(t, srv, back, cookie)
}

// echoCheck has the Wait echo's flow shows checked once, with a stream
// watching it, and returns the check's call.
func echoCheck(t *testing.T, srv *Server, e *setupEcho, clk *clock.Fake) echotest.Seen {
	t.Helper()
	run, _, ch, ok := srv.setupSubscribe("echo")
	if !ok {
		t.Fatal("echo has no setup flow")
	}
	defer srv.setupUnsubscribe(run, ch)
	// Every call before the check has returned (a check that just
	// returned leaves its deadline timer on the clock until it does, and
	// that timer would answer BlockUntil before the wait loop arms its
	// own), and what they sent is drained, so the next call seen is the
	// check.
	callsEnd(t, run)
	for len(e.seen) > 0 {
		<-e.seen
	}
	clk.BlockUntil(1)
	clk.Advance(setupWaitEvery)
	return e.nextCall(t)
}

// installEcho runs echo's install flow to its end through every step
// it takes, as the owner would: the Form answered with account, the Go
// there and back with a code, and the Wait checked once the app is
// installed.
func installEcho(t *testing.T, srv *Server, e *setupEcho, account string) {
	t.Helper()
	e.SetInstalled(true)
	startSetupFlow(t, srv, "echo")
	_, cookie := echoGo(t, srv, account)
	landedOnSetup(t, echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-1"}}), "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
	echoCheck(t, srv, e, srv.Clock.(*clock.Fake))
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)
}

// auditActions is name's audit rows with action, oldest first, as
// "<allowed> <reason>".
func auditActions(t *testing.T, srv *Server, name, action string) []string {
	t.Helper()
	rows, err := srv.Store.ReadIntegrationAuditTail(name, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		if r := rows[i]; r.Action == action && r.Agent == agent.CEOSlug {
			out = append(out, strings.TrimSpace(map[bool]string{true: "", false: "refused "}[r.Allowed]+r.Reason))
		}
	}
	return out
}

// Why this test exists: every integration has the same lifecycle, and
// the page and the list draw it from the state alone. A built-in must
// read available until its install flow runs, setting up while it
// does, installed once it is done (with Uninstall offered and every
// requirement, Kivali's and its own, met), off with the switch, and
// available again after Uninstall, which calls the integration's own
// Uninstall with the connection, forgets the Secret, drops every
// agent's access and is audited. A state read wrong would offer
// Install on an installed integration, or access on one that is not.
func TestIntegrationLifecycle(t *testing.T) {
	srv, e, _ := healthServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	intTeam(t, srv, "dana")

	page := echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateAvailable || page.CanUninstall || len(page.Requirements) != 0 || page.Flow != nil {
		t.Errorf("before = %s, can uninstall %v, requirements %v", page.Summary.State, page.CanUninstall, page.Requirements)
	}
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	page = echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateSettingUp || page.Flow == nil || page.Flow.State != apitypes.SetupFlowStateForm || page.Flow.Flow != "setup" {
		t.Errorf("while setting up = %s, flow %+v", page.Summary.State, page.Flow)
	}
	if !srv.setupCancel("echo") {
		t.Fatal("no flow to cancel")
	}
	installEcho(t, srv, e, "acme")
	page = echoPage(t, srv)
	if !installedAndMet(page) || !page.CanUninstall || !slices.Equal(requirementIDs(page), []string{"version", "connection", "installed", "permissions"}) {
		t.Errorf("installed = %s, unmet %v, ids %v", page.Summary.State, unmetIDs(page), requirementIDs(page))
	}
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)

	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/disable"))
	if page.Summary.State != apitypes.IntegrationStateOff || len(page.Access) != 1 || !page.CanUninstall {
		t.Errorf("off = %s, access %v", page.Summary.State, page.Access)
	}
	if rr := intDelete(t, srv, intBase+"/echo"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	got := e.callsOf("uninstall")
	if len(got) != 1 || got[0].Conn.Fields["account"] != "acme" || got[0].Conn.Fields["token"] != "tok-c0de-1" {
		t.Errorf("Uninstall was given %+v", got)
	}
	page = echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateAvailable || len(page.Access) != 0 || page.Connection != nil || page.CanUninstall {
		t.Errorf("after uninstall = %s, access %v, connections %v", page.Summary.State, page.Access, page.Connection)
	}
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets left: %v", names)
	}
	if got := auditActions(t, srv, "echo", uninstallAction); !slices.Equal(got, []string{""}) {
		t.Errorf("uninstall audit = %v", got)
	}
	// Installing it again switches it on: nothing installed has nothing
	// to keep off.
	installEcho(t, srv, e, "acme")
	if page := echoPage(t, srv); !installedAndMet(page) || !page.Summary.Enabled {
		t.Errorf("installed again = %+v", page.Summary)
	}
}

// Why this test exists: an install that fails or is cancelled keeps
// nothing, and the integration is available again. Echo's flow keeps
// along the way (the account as the browser goes to the service, the
// token once it is back), so a failure after a Keep would otherwise
// leave a half-made connection that reads as installed, with secrets
// nobody finished. A Fix that fails on an installed integration keeps
// what it kept: the connection was there before it.
func TestFailedInstallKeepsNothing(t *testing.T) {
	for _, end := range []struct {
		name string
		do   func(t *testing.T, srv *Server, e *setupEcho, cookie *http.Cookie)
	}{
		{"failed", func(t *testing.T, srv *Server, e *setupEcho, cookie *http.Cookie) {
			e.FailSetup(true)
			echoBack(t, srv, e, cookie, url.Values{"code": {"c0de-1"}})
			waitFlow(t, srv, "echo", apitypes.SetupFlowStateWait)
			echoCheck(t, srv, e, srv.Clock.(*clock.Fake))
			waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
		}},
		{"cancelled", func(t *testing.T, srv *Server, _ *setupEcho, _ *http.Cookie) {
			if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil); rr.Code != http.StatusNoContent {
				t.Fatalf("cancel: %d", rr.Code)
			}
		}},
		{"forgotten", func(t *testing.T, srv *Server, _ *setupEcho, _ *http.Cookie) {
			srv.Clock.(*clock.Fake).Advance(setupFlowTTL)
			assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			srv, e, _ := healthServer(t)
			mem := secrets.NewMemory()
			srv.Secrets = mem
			startSetupFlow(t, srv, "echo")
			_, cookie := echoGo(t, srv, "acme")
			if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateSettingUp || page.Connection == nil {
				t.Fatalf("after the Keep = %s, connections %v", page.Summary.State, page.Connection)
			}
			end.do(t, srv, e, cookie)
			srv.setupAwaitDiscard(context.Background(), "echo")
			if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable || page.Connection != nil {
				t.Errorf("after it %s = %s, connections %v", end.name, page.Summary.State, page.Connection)
			}
			if names := mem.Names(); len(names) != 0 {
				t.Errorf("secrets left: %v", names)
			}
		})
	}

	t.Run("a fix", func(t *testing.T) {
		srv, e, _ := healthServer(t)
		installEcho(t, srv, e, "acme")
		e.SetPermissions(false)
		e.ScriptSetup(func(_ int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
			if last.Form == nil {
				return builtinintegrations.Step{Keep: map[string]string{"account": "half-made-app"}, Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
			}
			return builtinintegrations.Step{}, errors.New("the service refused")
		})
		orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup", apitypes.SetupFlowStart{Flow: "permissions"}))
		waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
		if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}); rr.Code != http.StatusOK {
			t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
		}
		waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
		srv.setupAwaitDiscard(context.Background(), "echo")
		page := echoPage(t, srv)
		if page.Summary.State != apitypes.IntegrationStateInstalled || page.Connection == nil {
			t.Errorf("after a failed fix = %s, connections %v", page.Summary.State, page.Connection)
		}
		if conn := srv.setupConnection(context.Background(), "echo"); conn.Fields["account"] != "half-made-app" || conn.Fields["token"] != "tok-c0de-1" {
			t.Errorf("the connection after a failed fix = %v", conn.Fields)
		}
	})
}

// Why this test exists: an integration's own requirements follow
// Kivali's on the page, count toward the list's needs and are Needs-you
// rows when unmet, and they are its words in Kivali's screens. A row
// whose id is not one, is one of Kivali's or repeats one must be left
// out (it would stand for or shadow a row Kivali owns), its words
// scrubbed of the connection's secrets, and an integration that cannot
// answer must show one unmet row saying so, never nothing, which would
// read as healthy.
func TestIntegrationOwnRequirements(t *testing.T) {
	srv, e, clk := healthServer(t)
	installEcho(t, srv, e, "acme")
	e.ScriptRequirements(func(_ context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		return []builtinintegrations.Requirement{
			{ID: "installed", Title: "Installed on the service", Words: "Installed on " + conn.Fields["account"] + ".", Met: true},
			{ID: "permissions", Title: "Permissions", Words: "The app " + conn.Fields["token"] + " lacks a permission.", Fix: "permissions"},
			{ID: "connection", Title: "Shadow", Met: true},
			{ID: "permissions", Title: "Again"},
			{ID: "not one", Title: "Bad"},
			{ID: "untitled", Met: true},
		}, nil
	})
	page := echoPage(t, srv)
	if got := strings.Join(requirementIDs(page), ","); got != "version,connection,installed,permissions,untitled" {
		t.Fatalf("rows = %s", got)
	}
	perm, _ := requirementOf(page, "permissions")
	if perm.Met || perm.Fix != "permissions" || perm.Words != "The app "+integrationSecretPlaceholder+" lacks a permission." || page.Summary.Needs != 1 {
		t.Errorf("permissions = %+v, needs %d", perm, page.Summary.Needs)
	}
	if conn, _ := requirementOf(page, "connection"); conn.Title != "Connection" {
		t.Errorf("Kivali's connection row = %+v", conn)
	}
	if u, _ := requirementOf(page, "untitled"); u.Title != "untitled" {
		t.Errorf("a row with no title = %+v", u)
	}
	if sum := echoSummary(t, srv); sum.Needs != 1 || sum.State != apitypes.IntegrationStateInstalled {
		t.Errorf("summary = %+v", sum)
	}
	needs := requirementNeeds(t, srv)
	row, ok := needs["integration_requirement:echo/permissions"]
	if !ok || len(needs) != 1 || row.Title != "Echo: Permissions" || row.BodyMD != perm.Words || row.ReviewPath == nil ||
		*row.ReviewPath != "/org/integrations/echo" || row.From.Name != "Kivali" || !row.At.Equal(signInAt) {
		t.Errorf("Needs you = %+v", needs)
	}

	// Met: the row leaves Needs you.
	clk.Advance(requirementsCacheTTL) // past what the last read kept
	e.ScriptRequirements(func(context.Context, builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		return []builtinintegrations.Requirement{{ID: "permissions", Title: "Permissions", Met: true}}, nil
	})
	if needs := requirementNeeds(t, srv); len(needs) != 0 {
		t.Errorf("met, Needs you = %v", needs)
	}

	// An error is one unmet row in the integration's words, scrubbed.
	clk.Advance(requirementsCacheTTL) // past what the last read kept
	e.ScriptRequirements(func(_ context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		return nil, errors.New("the service refused " + conn.Fields["token"])
	})
	page = echoPage(t, srv)
	if c, _ := requirementOf(page, "checks"); c.Met || c.Words != "Echo could not check its requirements: the service refused "+integrationSecretPlaceholder+"." {
		t.Errorf("an error = %+v", c)
	}

	// No answer in time is the same, once the deadline passes.
	clk.Advance(requirementsCacheTTL) // past what the last read kept
	e.ScriptRequirements(func(ctx context.Context, _ builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	got := make(chan apitypes.IntegrationPage, 1)
	go func() { got <- echoPage(t, srv) }()
	clk.BlockUntil(1)
	clk.Advance(requirementsTimeout)
	select {
	case page = <-got:
	case <-time.After(failsafeWait):
		t.Fatal("the page waited past the deadline")
	}
	if c, _ := requirementOf(page, "checks"); c.Met || c.Words != "Echo could not check its requirements: it did not answer within 5 seconds." {
		t.Errorf("no answer = %+v", c)
	}
}

// Why this test exists: a requirement's Fix runs a flow through the
// integration's own setup, with the flow's name, and only a flow one
// of its requirements names: anything else would let the page run any
// code path of Setup by name. Echo's permissions row, unmet, is a
// Needs-you row whose Fix runs its one-step flow, after which the row
// is met and leaves Needs you. The integration stays installed while a
// fix runs (it is not setting up again), the page carries the fix's
// flow, and a fix waits for the integration to be switched on.
func TestRequirementFixRunsItsFlow(t *testing.T) {
	srv, e, clk := healthServer(t)
	start := func(flow string) *httptest.ResponseRecorder {
		return flowDo(t, srv, http.MethodPost, intBase+"/echo/setup", apitypes.SetupFlowStart{Flow: flow})
	}
	if e := assertAPIError(t, start("permissions"), http.StatusConflict); e.Error != "permissions is not a flow Echo has" {
		t.Errorf("before install = %+v", e)
	}
	installEcho(t, srv, e, "acme")
	e.SetPermissions(false)
	clk.Advance(requirementsCacheTTL) // past what the last read kept
	page := echoPage(t, srv)
	if perm, _ := requirementOf(page, "permissions"); perm.Met || perm.Fix != echotest.FlowPermissions || page.Summary.Needs != 1 {
		t.Fatalf("permissions = %+v, needs %d", perm, page.Summary.Needs)
	}
	if _, ok := requirementNeeds(t, srv)["integration_requirement:echo/permissions"]; !ok {
		t.Error("the unmet row is not in Needs you")
	}
	if e := assertAPIError(t, start("update"), http.StatusConflict); e.Error != "update is not a flow Echo has" {
		t.Errorf("a flow no row names = %+v", e)
	}
	orgOK[apitypes.SetupFlow](t, start("permissions"))
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)
	if got := e.lastFlow(); got != "permissions" {
		t.Errorf("Setup ran %q", got)
	}
	// A Fix runs on the installed connection, with what install kept.
	if calls := e.callsOf("setup"); calls[len(calls)-1].Conn.Fields["token"] != "tok-c0de-1" {
		t.Errorf("the fix was given %+v", calls[len(calls)-1].Conn)
	}
	page = echoPage(t, srv)
	if !installedAndMet(page) || page.Flow == nil || page.Flow.Flow != "permissions" {
		t.Errorf("after the fix = %s, unmet %v, flow %+v", page.Summary.State, unmetIDs(page), page.Flow)
	}
	if needs := requirementNeeds(t, srv); len(needs) != 0 {
		t.Errorf("after the fix, Needs you = %v", needs)
	}

	// A fix under way leaves the integration installed.
	e.SetPermissions(false)
	clk.Advance(requirementsCacheTTL) // past what the last read kept
	e.ScriptSetup(func(int, context.Context, builtinintegrations.Connection, builtinintegrations.Answer, builtinintegrations.Progress) (builtinintegrations.Step, error) {
		return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
	})
	orgOK[apitypes.SetupFlow](t, start("permissions"))
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	page = echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateInstalled || page.Flow == nil || page.Flow.Flow != "permissions" {
		t.Errorf("while fixing = %s, flow %+v", page.Summary.State, page.Flow)
	}
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/disable"))
	if e := assertAPIError(t, start("permissions"), http.StatusConflict); e.Error != "switch Echo on first" {
		t.Errorf("off = %+v", e)
	}
	if e := assertAPIError(t, start("sign_in"), http.StatusConflict); e.Error != "Echo is not connected by signing in" {
		t.Errorf("sign_in of a setup without one = %+v", e)
	}
}

// Why this test exists: Uninstall undoes the installation on the
// service before Kivali forgets anything. When the integration says it
// could not, the owner must read why (with no secret in it) and keep
// everything: the connection, its Secret and every agent's access. A
// failure that went on to forget the connection would leave an app on
// the service that nothing can reach to remove.
func TestUninstallRefusedByTheIntegration(t *testing.T) {
	srv, e, _ := healthServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	intTeam(t, srv, "dana")
	installEcho(t, srv, e, "acme")
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	e.FailUninstall(true)
	apiErr := assertAPIError(t, intDelete(t, srv, intBase+"/echo"), http.StatusBadGateway)
	if apiErr.Error != "Echo could not undo its installation: setup.example kept the app of token "+integrationSecretPlaceholder+"; nothing was changed" || apiErr.Who != whoDevelopers {
		t.Errorf("refusal = %+v", apiErr)
	}
	if got := e.callsOf("uninstall"); len(got) != 1 || got[0].Conn.Fields["token"] != "tok-c0de-1" {
		t.Errorf("Uninstall was given %+v", got)
	}
	page := echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateInstalled || len(page.Access) != 1 || len(mem.Names()) != 1 {
		t.Errorf("after the refusal = %s, access %v, secrets %v", page.Summary.State, page.Access, mem.Names())
	}
	if got := auditActions(t, srv, "echo", uninstallAction); len(got) != 1 || !strings.HasPrefix(got[0], "refused Echo could not undo") || strings.Contains(got[0], "tok-c0de-1") {
		t.Errorf("audit = %v", got)
	}

	// Once the service lets it go, Uninstall goes through.
	e.FailUninstall(false)
	if rr := intDelete(t, srv, intBase+"/echo"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable || len(mem.Names()) != 0 {
		t.Errorf("after uninstall = %s, secrets %v", page.Summary.State, mem.Names())
	}
}

// Why this test exists: the sign-in row's Fix signs in again through
// the standard flow, and the integration stays installed meanwhile:
// signing in again must not read as installing it from scratch, or the
// page would hide its access and requirements while the owner signs
// in. A connection made with a token has no sign-in to make.
func TestSignInFixSignsInAgain(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	signedIn(t, srv, "crm", vendor.Issue())
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if row, _ := requirementOf(page, "sign_in"); row.Met || row.Fix != "sign_in" {
		t.Fatalf("sign-in row = %+v", row)
	}
	f := orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup", apitypes.SetupFlowStart{Flow: "sign_in"}))
	if f.Flow != "sign_in" {
		t.Errorf("flow = %+v", f)
	}
	waitFlow(t, srv, "crm", apitypes.SetupFlowStateWait)
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if page.Summary.State != apitypes.IntegrationStateInstalled || page.Flow == nil || page.Flow.Flow != "sign_in" {
		t.Errorf("while signing in again = %s, %+v", page.Summary.State, page.Flow)
	}

	intConnected(t, srv, "hubspot")
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/hubspot/setup", apitypes.SetupFlowStart{Flow: "sign_in"}), http.StatusConflict); e.Error != "Test hubspot is not connected by signing in" {
		t.Errorf("a token connection = %+v", e)
	}
}

// Why this test exists: Kivali's own rows are built from what the
// connection holds: a bundle's server not running is unmet (agents'
// calls wait for it) and a connection test that failed is unmet with
// the integration's words, standing in Needs you since the test ran.
// Read from anything else, the owner would be told to fix what works,
// or not told what does not.
func TestCoreRequirementsOfABundle(t *testing.T) {
	srv := newTestServer(t)
	clk := clock.NewFake()
	srv.Clock = clk
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intConnected(t, srv, "crm")
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if run, _ := requirementOf(page, "running"); run.Met || page.Running || page.Summary.Needs != 1 {
		t.Errorf("no shim: running %+v, needs %d", run, page.Summary.Needs)
	}
	attachShim(t, srv, "crm", toolTexts(nil))
	clk.Advance(time.Minute)
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/verify"))
	test, _ := requirementOf(page, "connection_test")
	if test.Met || test.Words != "The connection test failed: no tool whoami" || !slices.Equal(unmetIDs(page), []string{"connection_test"}) {
		t.Errorf("a failed test = %+v, unmet %v", test, unmetIDs(page))
	}
	row, ok := requirementNeeds(t, srv)["integration_requirement:crm/connection_test"]
	if !ok || !row.At.Equal(clk.Now()) || row.Title != "Test crm: Connection test" {
		t.Errorf("Needs you = %+v", row)
	}
}
