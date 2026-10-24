package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// fakeHealth is fakeSetup with health of its own: Requirements and
// Uninstall answer as the test sets them, and each records what it was
// given, as Setup records the flow it ran.
type fakeHealth struct {
	*fakeSetup

	mu          sync.Mutex
	rows        func(ctx context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error)
	uninstall   error
	uninstalled []builtinintegrations.Connection
	flows       []string
}

func (f *fakeHealth) Setup(ctx context.Context, conn builtinintegrations.Connection, flow string, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	f.mu.Lock()
	f.flows = append(f.flows, flow)
	f.mu.Unlock()
	return f.fakeSetup.Setup(ctx, conn, flow, last, p)
}

func (f *fakeHealth) Requirements(ctx context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
	f.mu.Lock()
	rows := f.rows
	f.mu.Unlock()
	if rows == nil {
		return nil, nil
	}
	return rows(ctx, conn)
}

func (f *fakeHealth) Uninstall(_ context.Context, conn builtinintegrations.Connection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uninstalled = append(f.uninstalled, conn)
	return f.uninstall
}

// setRows sets what Requirements answers.
func (f *fakeHealth) setRows(rows func(ctx context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = rows
}

// lastFlow is the flow Setup last ran.
func (f *fakeHealth) lastFlow() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.flows) == 0 {
		return ""
	}
	return f.flows[len(f.flows)-1]
}

// healthServer is setupServer with fakeHealth as the built-in echo.
func healthServer(t *testing.T, script func(n int, ctx context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error)) (*Server, *fakeHealth, *clock.Fake) {
	t.Helper()
	srv := newTestServer(t)
	clk := clock.NewFakeAt(signInAt)
	srv.Clock = clk
	fh := &fakeHealth{fakeSetup: &fakeSetup{Echo: echotest.New(), setup: testSetupDecl(), script: script, seen: make(chan builtinintegrations.Answer, 64)}}
	srv.BuiltinIntegrations = builtinintegrations.Registry{fh}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	return srv, fh, clk
}

// keepThenDone is a setup flow of one Form, whose answer it keeps as
// the app's id and secret, and Done.
func keepThenDone(_ int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
	if last.Form == nil {
		return builtinintegrations.Step{Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
	}
	return builtinintegrations.Step{Done: true, Keep: map[string]string{"app_id": last.Form["account"], "app_secret": "s3cret-app-secret"}}, nil
}

// echoPage is echo's page as it stands.
func echoPage(t *testing.T, srv *Server) apitypes.IntegrationPage {
	t.Helper()
	return orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/echo"))
}

// installEcho runs echo's install flow to its end, answering its Form
// with account.
func installEcho(t *testing.T, srv *Server, account string) {
	t.Helper()
	startSetupFlow(t, srv, "echo")
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": account}}); rr.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
	}
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
// does, installed once it is done (with Uninstall offered and Kivali's
// requirements met), off with the switch, and available again after
// Uninstall, which calls the integration's own Uninstall with the
// connection, forgets the Secret, drops every agent's access and is
// audited. A state read wrong would offer Install on an installed
// integration, or access on one that is not.
func TestIntegrationLifecycle(t *testing.T) {
	srv, fh, _ := healthServer(t, keepThenDone)
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
	if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}); rr.Code != http.StatusOK {
		t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
	}
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateDone)
	page = echoPage(t, srv)
	if !installedAndMet(page) || !page.CanUninstall || !slices.Equal(requirementIDs(page), []string{"version", "connection"}) {
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
	fh.mu.Lock()
	got := fh.uninstalled
	fh.mu.Unlock()
	if len(got) != 1 || got[0].Fields["app_id"] != "acme" || got[0].Fields["app_secret"] != "s3cret-app-secret" {
		t.Errorf("Uninstall was given %+v", got)
	}
	page = echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateAvailable || len(page.Access) != 0 || len(page.Connections) != 0 || page.CanUninstall {
		t.Errorf("after uninstall = %s, access %v, connections %v", page.Summary.State, page.Access, page.Connections)
	}
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets left: %v", names)
	}
	if got := auditActions(t, srv, "echo", uninstallAction); !slices.Equal(got, []string{""}) {
		t.Errorf("uninstall audit = %v", got)
	}
	// Installing it again switches it on: nothing installed has nothing
	// to keep off.
	installEcho(t, srv, "acme")
	if page := echoPage(t, srv); !installedAndMet(page) || !page.Summary.Enabled {
		t.Errorf("installed again = %+v", page.Summary)
	}
}

// Why this test exists: an install that fails or is cancelled keeps
// nothing, and the integration is available again. A setup flow keeps
// along the way (an app's id before the browser goes to the service),
// so a failure after a Keep would otherwise leave a half-made
// connection that reads as installed, with secrets nobody finished. A
// Fix that fails on an installed integration keeps what it kept: the
// connection was there before it.
func TestFailedInstallKeepsNothing(t *testing.T) {
	failAfterKeep := func(_ int, _ context.Context, _ builtinintegrations.Connection, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
		if last.Form == nil {
			return builtinintegrations.Step{Keep: map[string]string{"app_id": "half-made-app"}, Form: &builtinintegrations.Form{Fields: []string{"account"}}}, nil
		}
		return builtinintegrations.Step{}, errors.New("the service refused")
	}
	for _, end := range []struct {
		name string
		do   func(t *testing.T, srv *Server)
	}{
		{"failed", func(t *testing.T, srv *Server) {
			if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}); rr.Code != http.StatusOK {
				t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
			}
			waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
		}},
		{"cancelled", func(t *testing.T, srv *Server) {
			if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/cancel", nil); rr.Code != http.StatusNoContent {
				t.Fatalf("cancel: %d", rr.Code)
			}
		}},
		{"forgotten", func(t *testing.T, srv *Server) {
			srv.Clock.(*clock.Fake).Advance(setupFlowTTL)
			assertAPIError(t, orgGet(t, srv, intBase+"/echo/setup"), http.StatusNotFound)
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			srv, _, _ := healthServer(t, failAfterKeep)
			mem := secrets.NewMemory()
			srv.Secrets = mem
			startSetupFlow(t, srv, "echo")
			waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
			if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateSettingUp || len(page.Connections) != 1 {
				t.Fatalf("after the Keep = %s, connections %v", page.Summary.State, page.Connections)
			}
			end.do(t, srv)
			srv.setupAwaitDiscard(context.Background(), "echo")
			if page := echoPage(t, srv); page.Summary.State != apitypes.IntegrationStateAvailable || len(page.Connections) != 0 {
				t.Errorf("after it %s = %s, connections %v", end.name, page.Summary.State, page.Connections)
			}
			if names := mem.Names(); len(names) != 0 {
				t.Errorf("secrets left: %v", names)
			}
		})
	}

	t.Run("a fix", func(t *testing.T) {
		srv, fh, _ := healthServer(t, keepThenDone)
		installEcho(t, srv, "acme")
		fh.script = failAfterKeep
		fh.setRows(func(context.Context, builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
			return []builtinintegrations.Requirement{{ID: "permissions", Title: "Permissions", Fix: "permissions"}}, nil
		})
		orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/echo/setup", apitypes.SetupFlowStart{Flow: "permissions"}))
		waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
		if rr := flowDo(t, srv, http.MethodPost, intBase+"/echo/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"account": "acme"}}); rr.Code != http.StatusOK {
			t.Fatalf("answer: %d %s", rr.Code, rr.Body.String())
		}
		waitFlow(t, srv, "echo", apitypes.SetupFlowStateFailed)
		srv.setupAwaitDiscard(context.Background(), "echo")
		page := echoPage(t, srv)
		if page.Summary.State != apitypes.IntegrationStateInstalled || len(page.Connections) != 1 {
			t.Errorf("after a failed fix = %s, connections %v", page.Summary.State, page.Connections)
		}
		if conn := srv.setupConnection(context.Background(), "echo"); conn.Fields["app_id"] != "half-made-app" || conn.Fields["app_secret"] != "s3cret-app-secret" {
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
	srv, fh, clk := healthServer(t, keepThenDone)
	installEcho(t, srv, "acme")
	fh.setRows(func(_ context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		return []builtinintegrations.Requirement{
			{ID: "installed", Title: "Installed on the service", Words: "Installed on " + conn.Fields["app_id"] + ".", Met: true},
			{ID: "permissions", Title: "Permissions", Words: "The app " + conn.Fields["app_secret"] + " lacks a permission.", Fix: "permissions"},
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
	fh.setRows(func(context.Context, builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		return []builtinintegrations.Requirement{{ID: "permissions", Title: "Permissions", Met: true}}, nil
	})
	if needs := requirementNeeds(t, srv); len(needs) != 0 {
		t.Errorf("met, Needs you = %v", needs)
	}

	// An error is one unmet row in the integration's words, scrubbed.
	clk.Advance(requirementsCacheTTL) // past what the last read kept
	fh.setRows(func(_ context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		return nil, errors.New("the service refused " + conn.Fields["app_secret"])
	})
	page = echoPage(t, srv)
	if c, _ := requirementOf(page, "checks"); c.Met || c.Words != "Echo could not check its requirements: the service refused "+integrationSecretPlaceholder+"." {
		t.Errorf("an error = %+v", c)
	}

	// No answer in time is the same, once the deadline passes.
	clk.Advance(requirementsCacheTTL) // past what the last read kept
	fh.setRows(func(ctx context.Context, _ builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
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
// code path of Setup by name. The integration stays installed while
// the fix runs (it is not setting up again), the page carries the
// fix's flow, and a fix waits for the integration to be switched on.
func TestRequirementFixRunsItsFlow(t *testing.T) {
	srv, fh, _ := healthServer(t, keepThenDone)
	start := func(flow string) *httptest.ResponseRecorder {
		return flowDo(t, srv, http.MethodPost, intBase+"/echo/setup", apitypes.SetupFlowStart{Flow: flow})
	}
	if e := assertAPIError(t, start("permissions"), http.StatusConflict); e.Error != "permissions is not a flow Echo has" {
		t.Errorf("before install = %+v", e)
	}
	installEcho(t, srv, "acme")
	fh.setRows(func(context.Context, builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
		return []builtinintegrations.Requirement{{ID: "permissions", Title: "Permissions", Fix: "permissions"}}, nil
	})
	if e := assertAPIError(t, start("update"), http.StatusConflict); e.Error != "update is not a flow Echo has" {
		t.Errorf("a flow no row names = %+v", e)
	}
	orgOK[apitypes.SetupFlow](t, start("permissions"))
	waitFlow(t, srv, "echo", apitypes.SetupFlowStateForm)
	if got := fh.lastFlow(); got != "permissions" {
		t.Errorf("Setup ran %q", got)
	}
	page := echoPage(t, srv)
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
	srv, fh, _ := healthServer(t, keepThenDone)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	intTeam(t, srv, "dana")
	installEcho(t, srv, "acme")
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	fh.mu.Lock()
	fh.uninstall = errors.New("the service kept s3cret-app-secret's app")
	fh.mu.Unlock()
	e := assertAPIError(t, intDelete(t, srv, intBase+"/echo"), http.StatusBadGateway)
	if e.Error != "Echo could not undo its installation: the service kept "+integrationSecretPlaceholder+"'s app; nothing was changed" || e.Who != whoDevelopers {
		t.Errorf("refusal = %+v", e)
	}
	page := echoPage(t, srv)
	if page.Summary.State != apitypes.IntegrationStateInstalled || len(page.Access) != 1 || len(mem.Names()) != 1 {
		t.Errorf("after the refusal = %s, access %v, secrets %v", page.Summary.State, page.Access, mem.Names())
	}
	if got := auditActions(t, srv, "echo", uninstallAction); len(got) != 1 || !strings.HasPrefix(got[0], "refused Echo could not undo") {
		t.Errorf("audit = %v", got)
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
