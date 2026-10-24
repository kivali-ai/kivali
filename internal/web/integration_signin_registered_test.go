package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// registeredBundle stores and approves a bundle called crm whose
// client Kivali registers itself with vendor, ZoomInfo's way: a
// discover address and the hosts its endpoints may be on, no form, a
// thirty-minute token. edit, when set, changes the manifest first.
func registeredBundle(t *testing.T, srv *Server, vendor *connecttest.FakeVendor, edit func(m map[string]any)) {
	t.Helper()
	m := integrationtest.Manifest("crm", "1.0.0")
	m["user_config"] = map[string]any{}
	m["server"].(map[string]any)["mcp_config"].(map[string]any)["env"] = map[string]any{}
	m["kivali"].(map[string]any)["connection"] = integration.Connection{
		Method: integration.MethodBrowser, Client: integration.ClientRegistered,
		Discover: vendor.Resource(), Hosts: []string{"127.0.0.1"},
		Scopes: []string{"zi_mcp", "offline_access"}, CredentialKeys: map[string]string{"access_token": "ZI_TOKEN"},
	}
	if edit != nil {
		edit(m)
	}
	b, err := integration.ReadBundle(integrationtest.Zip(t, "", m, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.StoreIntegrationVersion("crm", b, store.IntegrationVersion{PublishedBy: agent.CEOSlug, PublishedAt: signInAt}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.ApproveIntegrationVersion("crm", "1.0.0", signInAt); err != nil {
		t.Fatal(err)
	}
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = 30 * time.Minute })
}

// via is how a test's browser reaches the team: a request to path.
type via func(method, path string, body []byte) *http.Request

// onLoopback reaches the team at its address on this computer.
func onLoopback(method, path string, body []byte) *http.Request {
	r := httptest.NewRequest(method, loopbackOrigin+path, bytes.NewReader(body))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// atExternal reaches the team at its external address, through a proxy.
func atExternal(method, path string, body []byte) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "team.tail1234.ts.net")
	return r
}

const externalCallback = "https://team.tail1234.ts.net/connect/callback"

// registeredSignIn runs crm's whole browser sign-in reached by at: the
// start, the Go step's /connect/start, the vendor's approval and the
// callback. It returns the vendor's sign-in page the browser was sent
// to and the flow as it ended.
func registeredSignIn(t *testing.T, srv *Server, at via) (*url.URL, apitypes.SetupFlow) {
	t.Helper()
	orgOK[apitypes.SetupFlow](t, serveSignedIn(t, srv, at(http.MethodPost, intBase+"/crm/setup", []byte(`{"flow":"setup"}`))))
	f := waitFlowEnd(t, srv, apitypes.SetupFlowStateGo)
	if f.State != apitypes.SetupFlowStateGo {
		return nil, f
	}
	rr := serveSignedIn(t, srv, at(http.MethodGet, *f.Step.StartURL, nil))
	cookie := cookieOf(rr, "kivali_connect")
	authorize, err := url.Parse(rr.Header().Get("Location"))
	if rr.Code != http.StatusSeeOther || cookie == nil || err != nil {
		t.Fatalf("start: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	back := vendorApprove(t, authorize)
	cb := serveSignedIn(t, srv, at(http.MethodGet, back.RequestURI(), nil), cookie)
	if cb.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	return authorize, flowEnds(t, srv)
}

// waitFlowEnd waits for crm's flow to reach state or end, and returns
// it.
func waitFlowEnd(t *testing.T, srv *Server, state apitypes.SetupFlowState) apitypes.SetupFlow {
	t.Helper()
	run, initial, ch, ok := srv.setupSubscribe("crm")
	if !ok {
		t.Fatal("crm has no setup flow")
	}
	defer srv.setupUnsubscribe(run, ch)
	b := initial
	deadline := time.After(failsafeWait)
	for {
		f := decodeFlow(t, b)
		if f.State == state || f.State == apitypes.SetupFlowStateDone || f.State == apitypes.SetupFlowStateFailed {
			return f
		}
		select {
		case next, ok := <-ch:
			if !ok {
				t.Fatal("crm's flow was forgotten")
			}
			b = next
		case <-deadline:
			t.Fatalf("crm's flow is still %s", f.State)
		}
	}
}

// registrations counts the registration requests the vendor took.
func registrations(vendor *connecttest.FakeVendor) int {
	n := 0
	for _, r := range vendor.Requests() {
		if r.Method == http.MethodPost && r.Path == "/register" {
			n++
		}
	}
	return n
}

// Why this test exists: this is the whole point of client: registered,
// the owner signing in with nothing registered and nothing pasted. From
// one Sign in, core must find the authorization server from the
// manifest's discover address, register the team with the address it
// signs in at and the team's name, keep the client and the endpoints it
// found in the connection's Secret (making the connection), send the
// browser to the discovered authorization endpoint with the registered
// client, PKCE and the resource, exchange the code at the discovered
// token endpoint by the authentication the server listed, and keep the
// tokens. The page then names the host it signs in at. None of the
// client's secrets may reach the flow's words or the integration, and
// a refresh must go to the discovered token endpoint as the client
// registered.
func TestRegisteredSignInEndToEnd(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	if err := srv.Store.WriteCompanyName("Acme"); err != nil {
		t.Fatal(err)
	}
	registeredBundle(t, srv, vendor, nil)
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if page.SignInClient != integration.ClientRegistered || page.SignInAt["browser"] != "" || page.Connection != nil {
		t.Errorf("before: client %q, at %v, connection %+v", page.SignInClient, page.SignInAt, page.Connection)
	}

	authorize, f := registeredSignIn(t, srv, onLoopback)
	if f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("the flow ended %s: %s", f.State, textOf(f.Error))
	}
	regs := vendor.Registered()
	if len(regs) != 1 {
		t.Fatalf("registered %+v", regs)
	}
	reg := regs[0]
	want := connecttest.RegisteredClient{
		ID: reg.ID, Secret: reg.Secret, AuthMethod: connect.AuthSecretBasic, Name: "Kivali Acme", Scope: "zi_mcp offline_access",
		RedirectURIs: []string{loopbackOrigin + "/connect/callback"}, AccessToken: reg.AccessToken,
	}
	if reg.Name != want.Name || reg.Scope != want.Scope || reg.AuthMethod != want.AuthMethod || !slices.Equal(reg.RedirectURIs, want.RedirectURIs) {
		t.Errorf("registered %+v, want %+v", reg, want)
	}
	q := authorize.Query()
	if authorize.Path != "/authorize" || q.Get("client_id") != reg.ID || q.Get("resource") != vendor.Resource() ||
		q.Get("code_challenge_method") != "S256" || q.Get("scope") != "zi_mcp offline_access" {
		t.Errorf("the vendor's page = %s", authorize)
	}
	for _, r := range vendor.Requests() {
		if r.Path == "/token" {
			user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {r.Authorization}}}).BasicAuth()
			if !ok || user != reg.ID || pass != reg.Secret || r.Form.Has("client_secret") || r.Form.Get("resource") != vendor.Resource() {
				t.Errorf("the exchange: Authorization %q, form %v", r.Authorization, r.Form)
			}
		}
	}

	rec, data := firstConnectionSecret(t, srv)
	if rec.SignIn == nil || rec.SignIn.Method != "browser" || len(rec.SecretKeys) != 0 || !vendor.Valid(data[connect.SecretKeyAccessToken]) {
		t.Errorf("connection %+v", rec)
	}
	wantData := map[string]string{
		connect.SecretKeyClientID: reg.ID, connect.SecretKeyClientSecret: reg.Secret, connect.SecretKeyClientAuthMethod: connect.AuthSecretBasic,
		connect.SecretKeyRedirectURIs: loopbackOrigin + "/connect/callback", connect.SecretKeyResource: vendor.Resource(),
		connect.SecretKeyAuthorizationEndpoint: vendor.URL + "/authorize", connect.SecretKeyTokenEndpoint: vendor.URL + "/token",
		connect.SecretKeyRevocationEndpoint: vendor.URL + "/revoke", connect.SecretKeyRegistrationClientURI: vendor.URL + "/register/" + reg.ID,
		connect.SecretKeyRegistrationAccessToken: reg.AccessToken,
	}
	for k, w := range wantData {
		if data[k] != w {
			t.Errorf("Secret %s = %q, want %q", k, data[k], w)
		}
	}
	if _, ok := data[connect.SecretKeyClientSecretExpiresAt]; ok {
		t.Errorf("a secret that does not expire keeps an expiry: %v", data)
	}

	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if page.SignInAt["browser"] != integration.EndpointHost(vendor.URL) || !page.Connection.SignIn.SignedIn {
		t.Errorf("after: at %v, sign-in %+v", page.SignInAt, page.Connection.SignIn)
	}
	raw, _ := json.Marshal(f)
	for _, secret := range []string{reg.Secret, reg.AccessToken} {
		if strings.Contains(string(raw), secret) || strings.Contains(fmtAudit(t, srv), secret) {
			t.Errorf("the flow or the audit carries %q", secret)
		}
	}

	clk.Advance(30 * time.Minute)
	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !vendor.Valid(cred.Fields["ZI_TOKEN"]) || cred.Fields["ZI_TOKEN"] == data[connect.SecretKeyAccessToken] || vendor.Refreshes() != 1 {
		t.Errorf("credential %+v after %d refreshes", cred, vendor.Refreshes())
	}
	for k := range cred.Fields {
		if strings.HasPrefix(k, "oauth_") {
			t.Errorf("the integration is given %s", k)
		}
	}
	_, _, _, secretValues, _, err := srv.Integrations.readConnection(context.Background(), "crm", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{reg.Secret, reg.AccessToken} {
		if !slices.Contains(secretValues, secret) {
			t.Errorf("the log scrubber does not know %q", secret)
		}
	}
}

// Why this test exists: a registered client's install makes its
// connection when it keeps the client, before the browser goes to the
// service. A restart while the owner is away there must find that
// connection marked unfinished and discard it, as for every other
// install, rather than read it installed with a client and no sign-in;
// and the mark goes once the sign-in is done.
func TestRegisteredSignInMarksItsConnectionUntilDone(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	registeredBundle(t, srv, vendor, nil)
	orgOK[apitypes.SetupFlow](t, serveSignedIn(t, srv, onLoopback(http.MethodPost, intBase+"/crm/setup", []byte(`{"flow":"setup"}`))))
	if f := waitFlowEnd(t, srv, apitypes.SetupFlowStateGo); f.State != apitypes.SetupFlowStateGo {
		t.Fatalf("the flow = %+v", f)
	}
	if rec, err := srv.Store.GetIntegrationConnection("crm"); err != nil || !rec.SetupPending {
		t.Errorf("at the Go: %+v %v", rec, err)
	}
	srv.setupCancel("crm")
	srv.setupAwaitDiscard(context.Background(), "crm")

	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("the flow ended %s: %s", f.State, textOf(f.Error))
	}
	if rec, err := srv.Store.GetIntegrationConnection("crm"); err != nil || rec.SetupPending {
		t.Errorf("once signed in: %+v %v", rec, err)
	}
}

// fmtAudit is crm's audit, rows joined, for a test that looks for a
// value in it.
func fmtAudit(t *testing.T, srv *Server) string {
	t.Helper()
	rows, err := srv.Store.ReadIntegrationAuditTail("crm", 100)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

// Why this test exists: a registered client is the team's at the
// address it registered, so a sign-in again from that address must use
// the client kept (one registration per team, not one per sign-in),
// while a sign-in from an address the client was not registered with
// (the team moved, or the owner opened it at its external address)
// must register again with every address so far, keep the new client
// and delete the old one with the service; otherwise the service would
// send the browser back to an address it refuses, or the team would
// leave a client behind for each move.
func TestRegisteredSignInReusesOrRegistersAgain(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	srv.ExternalURL = "https://team.tail1234.ts.net"
	registeredBundle(t, srv, vendor, nil)
	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("first: %s", textOf(f.Error))
	}
	first := vendor.Registered()[0]
	markSignInAgain(t, srv)

	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("again here: %s", textOf(f.Error))
	}
	if registrations(vendor) != 1 || len(vendor.Registered()) != 1 {
		t.Fatalf("signing in again from the same address registered again: %+v", vendor.Registered())
	}
	markSignInAgain(t, srv)

	authorize, f := registeredSignIn(t, srv, atExternal)
	if f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("from the external address: %s", textOf(f.Error))
	}
	regs := vendor.Registered()
	if registrations(vendor) != 2 || len(regs) != 1 || regs[0].ID == first.ID {
		t.Fatalf("registered %+v", regs)
	}
	if want := []string{loopbackOrigin + "/connect/callback", externalCallback}; !slices.Equal(regs[0].RedirectURIs, want) {
		t.Errorf("registered addresses %v, want %v", regs[0].RedirectURIs, want)
	}
	if got := vendor.Deregistered(); !slices.Equal(got, []string{first.ID}) {
		t.Errorf("deregistered %v, want the first client", got)
	}
	if authorize.Query().Get("client_id") != regs[0].ID || authorize.Query().Get("redirect_uri") != externalCallback {
		t.Errorf("the vendor's page = %s", authorize)
	}
	_, data := firstConnectionSecret(t, srv)
	if data[connect.SecretKeyClientID] != regs[0].ID || data[connect.SecretKeyRedirectURIs] != loopbackOrigin+"/connect/callback\n"+externalCallback {
		t.Errorf("Secret %v", data)
	}
}

// markSignInAgain marks crm's connection as the vendor having refused
// its refresh for good: the owner may sign in again.
func markSignInAgain(t *testing.T, srv *Server) {
	t.Helper()
	rec, _ := firstConnectionSecret(t, srv)
	rec.SignIn.Error, rec.SignIn.ErrorAt = "the vendor refused the sign-in (invalid_grant)", signInAt
	if err := srv.Store.PutIntegrationConnection("crm", rec); err != nil {
		t.Fatal(err)
	}
}

// Why this test exists: a registered client is Kivali's own standing
// at the service, so ending the sign-in must end it too: Sign out must
// revoke the tokens at the endpoint discovery found, by the client's
// own authentication, delete the registration (RFC 7592) and forget the
// client, so the next sign-in registers again; Uninstall
// must do the same for the client registered then. A client left
// registered, or tokens sent to the manifest's (empty) revocation
// endpoint, would leave a working client at the service after the
// owner removed it.
func TestRegisteredSignOutAndUninstallForgetTheClient(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	registeredBundle(t, srv, vendor, nil)
	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in: %s", textOf(f.Error))
	}
	first := vendor.Registered()[0]
	_, before := firstConnectionSecret(t, srv)

	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/sign-out"))
	revoked := vendor.Revoked()
	if !slices.Equal(revoked, []string{before[connect.SecretKeyRefreshToken], before[connect.SecretKeyAccessToken]}) {
		t.Errorf("revoked %v", revoked)
	}
	for _, r := range vendor.Requests() {
		if r.Path == "/revoke" {
			if user, _, ok := (&http.Request{Header: http.Header{"Authorization": {r.Authorization}}}).BasicAuth(); !ok || user != first.ID {
				t.Errorf("revoked without the client's authentication: %q", r.Authorization)
			}
		}
	}
	if got := vendor.Deregistered(); !slices.Equal(got, []string{first.ID}) {
		t.Errorf("deregistered %v", got)
	}
	_, data := firstConnectionSecret(t, srv)
	for _, k := range connect.SecretKeys {
		if _, ok := data[k]; ok {
			t.Errorf("signed out, the Secret keeps %s", k)
		}
	}

	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in again: %s", textOf(f.Error))
	}
	second := vendor.Registered()
	if registrations(vendor) != 2 || len(second) != 1 || second[0].ID == first.ID {
		t.Fatalf("registered %+v", second)
	}
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if got := vendor.Deregistered(); !slices.Equal(got, []string{first.ID, second[0].ID}) || len(vendor.Registered()) != 0 {
		t.Errorf("deregistered %v, still registered %+v", got, vendor.Registered())
	}
	if len(vendor.Revoked()) != 4 {
		t.Errorf("revoked %v", vendor.Revoked())
	}
}

// Why this test exists: the metadata and the registration are the
// service's word, and a sign-in must not go on when either is not one
// core can use: metadata naming a host the manifest does not list ends
// the flow before anything is registered or kept, and a registration
// the service refuses ends it in the service's words. Either way the
// owner reads why, and no connection is left half made.
func TestRegisteredSignInRefusals(t *testing.T) {
	cases := []struct {
		name  string
		knobs func(k *connecttest.Knobs, vendor *connecttest.FakeVendor)
		want  string
	}{
		{"metadata naming another host", func(k *connecttest.Knobs, vendor *connecttest.FakeVendor) {
			k.ServerMetadata = func(doc map[string]any) {
				doc["token_endpoint"] = strings.Replace(vendor.URL, "127.0.0.1", "localhost", 1) + "/token"
			}
		}, "Kivali could not find where to sign in to Test crm: the service's metadata names its token endpoint on \"localhost:"},
		{"a refused registration", func(k *connecttest.Knobs, _ *connecttest.FakeVendor) { k.RegisterError = "invalid_redirect_uri" },
			"Kivali could not register itself with Test crm: the service refused to register Kivali (invalid_redirect_uri): refused by the test"},
		{"no registration endpoint", func(k *connecttest.Knobs, _ *connecttest.FakeVendor) {
			k.ServerMetadata = func(doc map[string]any) { delete(doc, "registration_endpoint") }
		}, "Kivali could not register itself with Test crm: the service's metadata names no registration endpoint"},
		{"a resource naming a server on another host", func(k *connecttest.Knobs, _ *connecttest.FakeVendor) {
			k.ResourceMetadata = func(doc map[string]any) { doc["authorization_servers"] = []any{"https://login.evil.example"} }
		}, `Kivali could not find where to sign in to Test crm: the service's metadata names its authorization server on "login.evil.example"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, vendor, _ := signInServer(t)
			registeredBundle(t, srv, vendor, nil)
			vendor.Set(func(k *connecttest.Knobs) { c.knobs(k, vendor) })
			_, f := registeredSignIn(t, srv, onLoopback)
			if f.State != apitypes.SetupFlowStateFailed || !strings.HasPrefix(textOf(f.Error), c.want) {
				t.Fatalf("the flow ended %s: %q, want %q", f.State, textOf(f.Error), c.want)
			}
			if conns, _ := srv.Store.ListIntegrationConnections("crm"); len(conns) != 0 {
				t.Errorf("a connection was made: %+v", conns)
			}
			if len(vendor.Registered()) != 0 {
				t.Errorf("registered %+v", vendor.Registered())
			}
		})
	}
}

// Why this test exists: a registered client may come with a connection
// form (a field the integration needs besides the sign-in). The form
// comes first, and then a save of it (PUT .../connection) must keep the
// client, its endpoints and the tokens in the Secret, which are no
// fields of the form; a save that dropped them would sign the owner out
// and strand a registration at the service.
func TestRegisteredClientSurvivesAFormSave(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	registeredBundle(t, srv, vendor, func(m map[string]any) {
		m["user_config"] = map[string]any{"region": map[string]any{"type": "string", "title": "Region", "required": true}}
	})
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup", apitypes.SetupFlowStart{Flow: "setup"}))
	f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
	if f.Step == nil || len(f.Step.Fields) != 1 || f.Step.Fields[0].Key != "region" {
		t.Fatalf("form %+v", f.Step)
	}
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"region": "eu"}}))
	f = waitFlow(t, srv, "crm", apitypes.SetupFlowStateGo)
	rr, cookie := startPage(t, srv, f)
	authorize, _ := url.Parse(rr.Header().Get("Location"))
	signedInThere(t, srv, callback(t, srv, vendorApprove(t, authorize), cookie))
	rec, before := firstConnectionSecret(t, srv)
	if rec.Fields["region"] != "eu" || before[connect.SecretKeyClientID] == "" {
		t.Fatalf("connection %+v, Secret %v", rec, before)
	}

	orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"region": "us"}}))
	rec, after := firstConnectionSecret(t, srv)
	if rec.Fields["region"] != "us" || rec.SignIn == nil {
		t.Errorf("connection %+v", rec)
	}
	for _, k := range connect.SecretKeys {
		if after[k] != before[k] {
			t.Errorf("a save changed %s: %q, was %q", k, after[k], before[k])
		}
	}
}

// registrationDeletes counts the registration deletes the vendor took.
func registrationDeletes(vendor *connecttest.FakeVendor) int {
	n := 0
	for _, r := range vendor.Requests() {
		if r.Method == http.MethodDelete {
			n++
		}
	}
	return n
}

// Why this test exists: RFC 7592's management address and token are
// optional, and a service that gives neither has no registration to
// delete. Signing out and Uninstall must still forget the
// client (the next sign-in registers again) and send the service no
// DELETE, rather than fail or guess an address.
func TestRegisteredClientWithoutManagementIsForgotten(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	registeredBundle(t, srv, vendor, nil)
	vendor.Set(func(k *connecttest.Knobs) { k.RegisterNoManagement = true })
	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in: %s", textOf(f.Error))
	}
	if _, data := firstConnectionSecret(t, srv); data[connect.SecretKeyRegistrationClientURI] != "" || data[connect.SecretKeyRegistrationAccessToken] != "" {
		t.Fatalf("Secret %v", data)
	}
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/sign-out"))
	_, data := firstConnectionSecret(t, srv)
	for _, k := range connect.SecretKeys {
		if _, ok := data[k]; ok {
			t.Errorf("signed out, the Secret keeps %s", k)
		}
	}
	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in again: %s", textOf(f.Error))
	}
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if registrations(vendor) != 2 || registrationDeletes(vendor) != 0 || len(vendor.Revoked()) != 4 {
		t.Errorf("registrations %d, deletes %d, revoked %v", registrations(vendor), registrationDeletes(vendor), vendor.Revoked())
	}
}

// Why this test exists: the token endpoint's authentication is the one
// the server listed and registered, kept in the Secret, and every later
// request reads it there: a server that takes only client_secret_post
// must get the id and secret in the body, never HTTP Basic, on the code
// exchange, each refresh and the revocation, or it refuses the client
// it registered.
func TestRegisteredSignInBySecretPost(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	registeredBundle(t, srv, vendor, nil)
	vendor.Set(func(k *connecttest.Knobs) { k.AuthMethods = []string{connect.AuthSecretPost} })
	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in: %s", textOf(f.Error))
	}
	reg := vendor.Registered()[0]
	if _, data := firstConnectionSecret(t, srv); reg.AuthMethod != connect.AuthSecretPost || data[connect.SecretKeyClientAuthMethod] != connect.AuthSecretPost {
		t.Fatalf("registered %+v, kept %q", reg, data[connect.SecretKeyClientAuthMethod])
	}
	clk.Advance(30 * time.Minute)
	if _, rpcErr := srv.Integrations.credential(context.Background(), "crm"); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/sign-out"))
	seen := map[string]int{}
	for _, r := range vendor.Requests() {
		if r.Path != "/token" && r.Path != "/revoke" {
			continue
		}
		seen[r.Path+" "+r.Form.Get("grant_type")]++
		if r.Authorization != "" || r.Form.Get("client_id") != reg.ID || r.Form.Get("client_secret") != reg.Secret {
			t.Errorf("%s: Authorization %q, form %v", r.Path, r.Authorization, r.Form)
		}
	}
	if seen["/token authorization_code"] != 1 || seen["/token refresh_token"] != 1 || seen["/revoke "] != 2 {
		t.Errorf("requests %v", seen)
	}
}

// Why this test exists: a discover address may name an authorization
// server that publishes no protected resource's metadata (it is its own
// resource). Core must find it at its root after the resource's places
// come up empty, register and sign in as for any other, and then name
// no resource on the sign-in or the token requests, since none was
// declared to ask for.
func TestRegisteredSignInAtAServerThatIsItsOwnResource(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	registeredBundle(t, srv, vendor, nil)
	vendor.Set(func(k *connecttest.Knobs) { k.NoResourceMetadata = true })
	authorize, f := registeredSignIn(t, srv, onLoopback)
	if f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in: %s", textOf(f.Error))
	}
	if authorize.Query().Has("resource") {
		t.Errorf("the vendor's page names a resource: %s", authorize)
	}
	for _, r := range vendor.Requests() {
		if r.Path == "/token" && r.Form.Has("resource") {
			t.Errorf("the exchange names a resource: %v", r.Form)
		}
	}
	if _, data := firstConnectionSecret(t, srv); data[connect.SecretKeyTokenEndpoint] != vendor.URL+"/token" || data[connect.SecretKeyResource] != "" {
		t.Errorf("Secret %v", data)
	}
}

// Why this test exists: a registered client's secret may expire
// (client_secret_expires_at), after which the service refuses it. A
// sign-in after that must register again rather than send the expired
// secret, and the client it replaces goes: the tokens it still holds
// are revoked with it, then it is deleted, and they are not kept for
// the new client.
func TestRegisteredSignInRegistersAgainOnceTheSecretExpires(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	registeredBundle(t, srv, vendor, nil)
	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in: %s", textOf(f.Error))
	}
	first := vendor.Registered()[0]
	rec, data := firstConnectionSecret(t, srv)
	old := connect.Tokens{Access: data[connect.SecretKeyAccessToken], Refresh: data[connect.SecretKeyRefreshToken]}
	data[connect.SecretKeyClientSecretExpiresAt] = strconv.FormatInt(signInAt.Add(-time.Hour).Unix(), 10)
	if err := srv.Secrets.Put(context.Background(), rec.SecretName, data); err != nil {
		t.Fatal(err)
	}
	markSignInAgain(t, srv)

	if _, f := registeredSignIn(t, srv, onLoopback); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("sign in again: %s", textOf(f.Error))
	}
	regs := vendor.Registered()
	if registrations(vendor) != 2 || len(regs) != 1 || regs[0].ID == first.ID {
		t.Fatalf("registered %+v", regs)
	}
	if got := vendor.Deregistered(); !slices.Equal(got, []string{first.ID}) {
		t.Errorf("deregistered %v", got)
	}
	if got := vendor.Revoked(); !slices.Equal(got, []string{old.Refresh, old.Access}) {
		t.Errorf("revoked %v, want the old client's tokens", got)
	}
	_, data = firstConnectionSecret(t, srv)
	if data[connect.SecretKeyClientID] != regs[0].ID || data[connect.SecretKeyAccessToken] == old.Access || !vendor.Valid(data[connect.SecretKeyAccessToken]) {
		t.Errorf("Secret %v", data)
	}
}
