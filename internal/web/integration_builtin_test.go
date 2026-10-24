package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// echoToken is the test built-in's connection secret: long enough for
// the scrubber, and its first three characters are what whoami shows.
const echoToken = "s3cret-echo-token"

// withEcho gives srv the test built-in, installed as boot installs it,
// with no pod provisioner: nothing it does may need a pod.
func withEcho(t *testing.T, srv *Server) *echotest.Echo {
	t.Helper()
	echo := echotest.New()
	srv.BuiltinIntegrations = builtinintegrations.Registry{echo}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	return echo
}

func connectEcho(t *testing.T, srv *Server, greeting string) {
	t.Helper()
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/echo/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": echoToken, "greeting": greeting}})
	if rr.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rr.Code, rr.Body.String())
	}
}

func echoSummary(t *testing.T, srv *Server) apitypes.IntegrationSummary {
	t.Helper()
	list := orgOK[apitypes.Integrations](t, orgGet(t, srv, intBase))
	for _, it := range list.Integrations {
		if it.Name == echotest.Name {
			return it
		}
	}
	t.Fatalf("echo is not listed: %+v", list)
	return apitypes.IntegrationSummary{}
}

// Why this test exists: a built-in has no pod, so nothing ever
// connects a shim for it. If its health came from the shim it would
// read "not running" forever and Test connection would stay disabled;
// it must read available, installed or off, with the built-in's own
// version and no running requirement, and its page must offer
// Uninstall only once something is installed and name Kivali as the
// publisher of its version.
func TestBuiltinIntegrationState(t *testing.T) {
	srv := newTestServer(t)
	withEcho(t, srv)
	sum := echoSummary(t, srv)
	if sum.State != apitypes.IntegrationStateAvailable || sum.Needs != 0 || sum.Version != "1.0.0" || !sum.Builtin || !sum.Enabled || sum.PendingVersion != nil {
		t.Errorf("before a connection = %+v", sum)
	}
	connectEcho(t, srv, "Hello")
	if sum := echoSummary(t, srv); sum.State != apitypes.IntegrationStateInstalled || sum.Needs != 0 {
		t.Errorf("installed = %+v", sum)
	}
	page := orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/disable"))
	if page.Summary.State != apitypes.IntegrationStateOff || !page.CanUninstall {
		t.Errorf("off = %+v", page.Summary)
	}
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/enable"))
	if !installedAndMet(page) || !page.Running || !page.CanUninstall || !slices.Equal(requirementIDs(page), []string{"version", "connection"}) {
		t.Errorf("page = %+v", page.Summary)
	}
	if len(page.Versions) != 1 || page.Versions[0].PublishedBy != (apitypes.PersonRef{Slug: store.BuiltinIntegrationPublisher, Name: "Kivali"}) ||
		page.Versions[0].NotesMD != store.BuiltinIntegrationNotes || page.Author != "Kivali" || len(page.Hosts) != 0 {
		t.Errorf("page = %+v", page)
	}
	v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/echo/versions/1.0.0"))
	if !v.Builtin || !v.Current || v.State != apitypes.IntegrationVersionStateApproved || v.PublishedBy.Name != "Kivali" || v.Runtime != "" {
		t.Errorf("install screen = %+v", v)
	}
	// A bundle is not built in, and says so.
	intInstalled(t, srv, "crm", "1.0.0")
	if v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.0.0")); v.Builtin {
		t.Errorf("a bundle's install screen = %+v", v)
	}
}

// Why this test exists: a built-in's versions follow the binary and it
// cannot be taken off the team. An approve, a reject, a roll back or
// an upload under its name that went through would make the
// data directory disagree with the code that runs; each must be a 409
// with a sentence that says why.
func TestBuiltinIntegrationRefusesTheOwnersVersionMoves(t *testing.T) {
	srv := newTestServer(t)
	echo := withEcho(t, srv)
	echo.Version = "1.1.0"
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	const why = "Echo ships with Kivali, and its version changes only with Kivali's updates; there is nothing to "
	for path, want := range map[string]string{
		"/echo/versions/1.0.0/rollback": why + "roll back to",
		"/echo/versions/1.0.0/approve":  why + "approve",
		"/echo/versions/1.1.0/reject":   why + "reject",
	} {
		if e := assertAPIError(t, intPost(t, srv, intBase+path), http.StatusConflict); e.Error != want {
			t.Errorf("%s = %+v", path, e)
		}
	}
	// The same words when the store is the one to refuse (a request in
	// Needs you answered after the integration became built in).
	if err := srv.approveIntegrationVersion("echo", "1.0.0"); err == nil || err.Error() != why+"approve" {
		t.Errorf("the store's refusal of an approval = %v", err)
	}
	if err := srv.rejectIntegrationVersion("echo", "1.1.0"); err == nil || err.Error() != why+"reject" {
		t.Errorf("the store's refusal of a rejection = %v", err)
	}
	if e := assertAPIError(t, intUpload(t, srv, intBundle(t, "echo", "2.0.0")), http.StatusConflict); e.Error != "Echo ships with Kivali, so a bundle cannot replace it" {
		t.Errorf("upload = %+v", e)
	}
	if sum := echoSummary(t, srv); sum.Version != "1.1.0" {
		t.Errorf("after the refusals = %+v", sum)
	}
}

// Why this test exists: it is the whole path an agent's call of a
// built-in takes, over core.sock as an agent pod reaches it. dana, with
// access, is listed the actions the manifest declares (not the one the
// built-in lists beyond it), and her call reaches the Go function with
// her slug, her access and the connection's values, secret included,
// and is in the audit. lee, without access, is listed nothing and
// refused before the function runs. Without core's check here an agent
// could call a built-in it was never given; without the values the
// function could not reach its service.
func TestAgentCallsABuiltinIntegration(t *testing.T) {
	srv := newTestServer(t)
	echo := withEcho(t, srv)
	intTeam(t, srv, "dana", "lee")
	connectEcho(t, srv, "Hello")
	putIntegrationAccess(t, srv, "echo", "dana", `{"colors":["red"]}`)
	sock := serveCoreUDS(t, srv)
	dana := agentpod.NewClient(sock, "dana")
	lee := agentpod.NewClient(sock, "lee")
	ctx := context.Background()

	list, err := dana.IntegrationTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "echo_echo,echo_whoami" {
		t.Errorf("dana's actions = %v", names)
	}
	if d := list.Tools[0].Description; d != "Answer with the arguments. Through the Echo integration; the owner decides what you may do with it." {
		t.Errorf("description = %q", d)
	}
	if s := string(list.Tools[0].InputSchema); s != `{"type":"object"}` {
		t.Errorf("schema = %s", s)
	}
	if l, err := lee.IntegrationTools(ctx); err != nil || len(l.Tools) != 0 {
		t.Errorf("lee's actions = %+v, %v", l, err)
	}

	resp, err := dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "echo_echo", Arguments: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.IsError || len(resp.Content) != 1 || resp.Content[0] != `{"access":{"colors":["red"]},"agent":"dana","arguments":{"text":"hi"}}` {
		t.Errorf("echo = %+v", resp)
	}
	calls := echo.Calls()
	if len(calls) != 1 || calls[0].Action != "echo" || calls[0].Caller.Agent != "dana" ||
		calls[0].Conn.Fields["token"] != echoToken || calls[0].Conn.Fields["greeting"] != "Hello" || calls[0].Conn.ID == "" {
		t.Errorf("echo was called with %+v", calls)
	}
	resp, _ = dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "echo_whoami", Arguments: nil})
	if resp.IsError || resp.Content[0] != "Hello; token s3c..." {
		t.Errorf("whoami = %+v", resp)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("echo", 10)
	if len(rows) != 2 || rows[1].Agent != "dana" || rows[1].Action != "echo" || !rows[1].Allowed || rows[1].Reason != "" {
		t.Errorf("audit after dana's calls = %+v", rows)
	}

	resp, err = lee.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "echo_echo", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsError || resp.Content[0] != "You have no access to the Echo integration. Ask the owner if your work needs it." {
		t.Errorf("lee's call = %+v", resp)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("echo", 10); rows[0].Agent != "lee" || rows[0].Allowed || rows[0].Reason != "no access" {
		t.Errorf("audit after lee's call = %+v", rows[0])
	}
	if len(echo.Calls()) != 2 {
		t.Errorf("lee's refused call reached the built-in: %+v", echo.Calls())
	}

	// An action the built-in lists but the manifest does not declare is
	// refused as one the access does not include.
	resp, _ = dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "echo_undeclared", Arguments: nil})
	if !resp.IsError || resp.Content[0] != "Your access to the Echo integration does not include undeclared. Ask the owner if your work needs it." {
		t.Errorf("undeclared = %+v", resp)
	}

	// Switched off, the built-in is refused like a bundle.
	intPost(t, srv, intBase+"/echo/disable")
	resp, _ = dana.IntegrationCall(ctx, agentpod.IntegrationCallRequest{Name: "echo_echo", Arguments: nil})
	if !resp.IsError || resp.Content[0] != "The Echo integration is switched off. Ask the owner if your work needs it." {
		t.Errorf("off = %+v", resp)
	}
}

// Why this test exists: an agent's access reaches the built-in on
// every call, as the stamp reaches a pod's server, and the built-in
// holds the agent to it. Access changed by the owner must be what the
// next call carries, not a copy kept from before.
func TestBuiltinIntegrationReadsTheAccessAsItStands(t *testing.T) {
	srv := newTestServer(t)
	withEcho(t, srv)
	intTeam(t, srv, "dana")
	connectEcho(t, srv, "Hello")
	putIntegrationAccess(t, srv, "echo", "dana", `{"colors":["red"],"loud":true}`)
	resp := srv.callAgentIntegration(context.Background(), "dana", "echo_echo", json.RawMessage(`{"text":"hi"}`))
	if resp.IsError || resp.Content[0] != `{"ACCESS":{"COLORS":["RED"],"LOUD":TRUE},"AGENT":"DANA","ARGUMENTS":{"TEXT":"HI"}}` {
		t.Errorf("loud = %+v", resp)
	}
	putIntegrationAccess(t, srv, "echo", "dana", `{"colors":["blue"]}`)
	resp = srv.callAgentIntegration(context.Background(), "dana", "echo_echo", nil)
	if resp.IsError || resp.Content[0] != `{"access":{"colors":["blue"]},"agent":"dana","arguments":{}}` {
		t.Errorf("after the change = %+v", resp)
	}
}

// Why this test exists: a built-in's answer may quote a connection
// secret, as a pod's may. Every string it gives the agent, and its
// words kept in the audit, must go through the connection's scrubber,
// or an agent could read the token by asking an action to say it back.
func TestBuiltinIntegrationAnswersAreScrubbed(t *testing.T) {
	srv := newTestServer(t)
	withEcho(t, srv)
	intTeam(t, srv, "dana")
	connectEcho(t, srv, "Hello")
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	resp := srv.callAgentIntegration(context.Background(), "dana", "echo_echo", json.RawMessage(`{"say":"`+echoToken+`"}`))
	if resp.IsError || strings.Contains(resp.Content[0], echoToken) || !strings.Contains(resp.Content[0], `"say":"[secret]"`) {
		t.Errorf("echo of the token = %+v", resp)
	}
	resp = srv.callAgentIntegration(context.Background(), "dana", "echo_nope", nil)
	if !resp.IsError {
		t.Errorf("an action the manifest does not declare = %+v", resp)
	}
}

// Why this test exists: Test connection and an access field's options
// reach a built-in's Verify and Choices with the connection, with no
// pod: a 409 "not running" here would leave the owner no way to check a
// built-in's connection or fill its access form. A failed test keeps
// the built-in's words with the token scrubbed, and both are audited
// for the owner.
func TestBuiltinIntegrationVerifyAndChoices(t *testing.T) {
	srv := newTestServer(t)
	echo := withEcho(t, srv)
	if e := assertAPIError(t, intPost(t, srv, intBase+"/echo/connection/verify"), http.StatusConflict); e.Error != "install Echo first" {
		t.Errorf("verify with no connection = %+v", e)
	}
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/echo/choices/colors"), http.StatusConflict); e.Error != "install Echo first" {
		t.Errorf("choices with no connection = %+v", e)
	}
	connectEcho(t, srv, "Hello")
	page := orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/connection/verify"))
	c := page.Connection
	if c == nil || c.Verified == nil || !*c.Verified || c.VerifyMessage == nil || *c.VerifyMessage != "Hello; token s3c..." || c.VerifiedAt == nil {
		t.Fatalf("connection = %+v", c)
	}
	if calls := echo.Calls(); len(calls) != 1 || calls[0].Action != "verify" || calls[0].Conn.Fields["token"] != echoToken {
		t.Errorf("Verify was given %+v", calls)
	}
	choices := orgOK[apitypes.AccessChoices](t, orgGet(t, srv, intBase+"/echo/choices/colors"))
	if strings.Join(choices.Options, ",") != strings.Join(echotest.Colors, ",") {
		t.Errorf("choices = %+v", choices)
	}
	audit := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/echo/audit"))
	if len(audit.Rows) != 2 || audit.Rows[1].Agent.Slug != agent.CEOSlug || audit.Rows[1].Action != "whoami" || !audit.Rows[1].Allowed ||
		audit.Rows[0].Action != "colors" || audit.Rows[0].Reason != "" {
		t.Errorf("audit = %+v", audit.Rows)
	}

	connectEcho(t, srv, echotest.FailGreeting)
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/echo/connection/verify"))
	if c := page.Connection; c.Verified == nil || *c.Verified || c.VerifyMessage == nil || *c.VerifyMessage != "the token [secret] was refused" {
		t.Errorf("failed test = %+v", c)
	}
	audit = orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/echo/audit"))
	if audit.Rows[0].Reason != "the token [secret] was refused" {
		t.Errorf("audit = %+v", audit.Rows[0])
	}
}

// Why this test exists: access needs a connection, so a call that
// passes the access check finds one; but the connection can go between
// the check and the call. The built-in must then be refused in words
// that say what is missing, not called with no values.
func TestBuiltinIntegrationCallWithNoConnection(t *testing.T) {
	srv := newTestServer(t)
	echo := withEcho(t, srv)
	_, m, err := srv.Store.CurrentIntegrationVersion(echotest.Name)
	if err != nil {
		t.Fatal(err)
	}
	v := agentIntegration{name: echotest.Name, display: "Echo", manifest: m, access: json.RawMessage(`{}`), builtin: echo}
	resp := srv.runAgentIntegrationCall(context.Background(), srv.clk().Now(), "dana", v, "echo", nil, nil)
	if !resp.IsError || resp.Content[0] != "The Echo integration has no connection yet. Ask the owner to connect it." {
		t.Errorf("no connection = %+v", resp)
	}
	if len(echo.Calls()) != 0 {
		t.Errorf("the built-in was called with no connection: %+v", echo.Calls())
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("echo", 1); len(rows) != 1 || rows[0].Allowed || rows[0].Reason != "the integration has no connection" {
		t.Errorf("audit = %+v", rows)
	}
}
