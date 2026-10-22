package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// signInAt is when the sign-in tests start.
var signInAt = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// signInSetupNotes is the sign-in bundle's kivali.setup_notes.
const signInSetupNotes = "Register an OAuth app under **Developer settings** and paste its client id and secret."

// signInConnection is a device connection section against vendor that
// names the access token GH_TOKEN.
func signInConnection(vendor *connecttest.FakeVendor) integration.Connection {
	ep := vendor.Endpoints()
	return integration.Connection{
		Method: integration.MethodDevice, DeviceEndpoint: ep.Device, TokenEndpoint: ep.Token, RevocationEndpoint: ep.Revocation,
		Scopes: []string{"repo"}, CredentialKeys: map[string]string{"access_token": "GH_TOKEN"},
	}
}

// signInServer is a server on a fake clock whose OAuth requests go to
// a fake vendor on the same clock.
func signInServer(t *testing.T) (*Server, *connecttest.FakeVendor, *clock.Fake) {
	t.Helper()
	srv := newTestServer(t)
	clk := clock.NewFakeAt(signInAt)
	vendor := connecttest.New(t, clk)
	srv.Clock = clk
	srv.Connect = &connect.Engine{Clock: clk, HTTP: vendor.Server.Client()}
	return srv, vendor, clk
}

// signInBundle stores and approves a bundle called crm whose manifest
// signs in against vendor, as the check will let one once sign-in is
// offered (the store does not run the check). Its server's env names
// the client id and the client secret, so a test sees which of them
// reach the pod.
func signInBundle(t *testing.T, srv *Server, vendor *connecttest.FakeVendor) {
	t.Helper()
	m := integrationtest.Manifest("crm", "1.0.0")
	c := signInConnection(vendor)
	m["kivali"].(map[string]any)["connection"] = c
	m["kivali"].(map[string]any)["setup_notes"] = signInSetupNotes
	m["server"].(map[string]any)["mcp_config"].(map[string]any)["env"] = map[string]any{
		"TOKEN": "${user_config.token}", "CLIENT": "${user_config.client_id}", "CLIENT_SECRET": "${user_config.client_secret}",
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
}

// signedIn writes name's one connection as signed in with tok,
// directly: its record (the client id, the sign-in's expiry) and its
// Secret (a token field, the client secret and the tokens).
func signedIn(t *testing.T, srv *Server, name string, tok connect.Tokens) store.IntegrationConnection {
	t.Helper()
	secretName := secrets.ConnectionSecretName(name, "c1")
	rec := store.IntegrationConnection{
		ID: "c1", CreatedAt: signInAt, Fields: map[string]string{"client_id": connecttest.ClientID},
		SecretName: secretName, SecretKeys: []string{"client_secret", "token"},
		SignIn: &store.ConnectionSignIn{Method: "device", SignedInAt: signInAt, ExpiresAt: tok.ExpiresAt, Scope: "repo"},
	}
	if err := srv.Store.PutIntegrationConnection(name, rec); err != nil {
		t.Fatal(err)
	}
	data := map[string]string{
		"token": "pasted-field-value", "client_secret": connecttest.ClientSecret,
		connect.SecretKeyAccessToken: tok.Access, connect.SecretKeyRefreshToken: tok.Refresh,
	}
	if err := srv.Secrets.Put(context.Background(), secretName, data); err != nil {
		t.Fatal(err)
	}
	return rec
}

func connectionSecret(t *testing.T, srv *Server, name string) (store.IntegrationConnection, map[string]string) {
	t.Helper()
	rec, err := srv.Store.GetIntegrationConnectionByID(name, "c1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := srv.Secrets.Get(context.Background(), rec.SecretName)
	if err != nil {
		t.Fatal(err)
	}
	return rec, data
}

// Why this test exists: a bundle's server reads its token from
// kivali/credential on each call. A token good for longer than the
// margin must come back as stored, with no request to the vendor (one
// per call would hit its rate limits), under the name the manifest's
// credential_keys gives, in fields and env both; the client id may be
// there, the client secret and the refresh token never: an integration
// that held them could mint tokens behind core's back.
func TestSignInCredentialAnswersAFreshToken(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	tok := vendor.Issue()
	signedIn(t, srv, "crm", tok)
	clk.Advance(30 * time.Minute)

	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if cred.Fields["GH_TOKEN"] != tok.Access || cred.Env["GH_TOKEN"] != tok.Access {
		t.Errorf("credential = %+v, want the stored access token", cred)
	}
	if cred.Fields["client_id"] != connecttest.ClientID || cred.Env["CLIENT"] != connecttest.ClientID || cred.Fields["token"] != "pasted-field-value" {
		t.Errorf("credential = %+v", cred)
	}
	raw, _ := json.Marshal(cred)
	for _, secret := range []string{connecttest.ClientSecret, tok.Refresh} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the credential carries %q: %s", secret, raw)
		}
	}
	if _, ok := cred.Fields["client_secret"]; ok || cred.Env["CLIENT_SECRET"] != "" {
		t.Errorf("the client secret reached the pod: %+v", cred)
	}
	if n := vendor.Refreshes(); n != 0 {
		t.Errorf("%d refreshes for a token good for 30 minutes more", n)
	}
}

// Why this test exists: a token about to expire is refreshed before it
// is handed out, so the integration's call does not fail on it a few
// seconds later. The new tokens must be kept (the access token and a
// rotated refresh token in the Secret, the expiry on the record), or
// every call would refresh again and a rotating vendor would refuse
// the second. The next call is answered from what was kept. A save of
// the connection form afterwards keeps the tokens and the sign-in.
func TestSignInCredentialRefreshesANearlyExpiredToken(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour; k.Rotate = true; k.GitHubStyle = true })
	old := vendor.Issue()
	signedIn(t, srv, "crm", old)
	clk.Advance(time.Hour - 30*time.Second)

	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	fresh := cred.Fields["GH_TOKEN"]
	if fresh == old.Access || !vendor.Valid(fresh) {
		t.Fatalf("handed out %q, want a refreshed token", fresh)
	}
	rec, data := connectionSecret(t, srv, "crm")
	if data[connect.SecretKeyAccessToken] != fresh || data[connect.SecretKeyRefreshToken] == old.Refresh || !vendor.RefreshAccepted(data[connect.SecretKeyRefreshToken]) {
		t.Errorf("Secret after the refresh = %v", data)
	}
	if want := clk.Now().Add(time.Hour); !rec.SignIn.ExpiresAt.Equal(want) || rec.SignIn.Error != "" || rec.SignIn.Scope != "repo" {
		t.Errorf("record after the refresh = %+v, want expiry %v", rec.SignIn, want)
	}
	if data["token"] != "pasted-field-value" || data["client_secret"] != connecttest.ClientSecret {
		t.Errorf("the refresh lost the connection's own secrets: %v", data)
	}

	if again, _ := srv.Integrations.credential(context.Background(), "crm"); again.Fields["GH_TOKEN"] != fresh || vendor.Refreshes() != 1 {
		t.Errorf("second call: %q after %d refreshes", again.Fields["GH_TOKEN"], vendor.Refreshes())
	}

	// The owner saves the form (a new portal); the sign-in stays.
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"portal": "acme", "client_id": connecttest.ClientID}})
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	rec, data = connectionSecret(t, srv, "crm")
	if data[connect.SecretKeyAccessToken] != fresh || rec.SignIn == nil || rec.Fields["portal"] != "acme" {
		t.Errorf("after a save: record %+v, Secret %v", rec, data)
	}
}

// Why this test exists: a vendor that refuses the refresh token for
// good (revoked, expired, the app's secret rotated) leaves the
// connection unusable until the owner signs in again. The record must
// say so and why, which is what the page and Needs you show; the
// integration gets CodeSignInNeeded, which its server passes to the
// agent in words, not a token that no longer works; the owner's audit
// shows the failed refresh; and later calls do not keep sending the
// vendor a token it refused.
func TestSignInCredentialAfterARefusedRefresh(t *testing.T) {
	for _, githubStyle := range []bool{false, true} {
		srv, vendor, clk := signInServer(t)
		signInBundle(t, srv, vendor)
		vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour; k.GitHubStyle = githubStyle })
		signedIn(t, srv, "crm", vendor.Issue())
		vendor.Set(func(k *connecttest.Knobs) { k.RefreshError = "bad_refresh_token" })
		if !githubStyle {
			vendor.Set(func(k *connecttest.Knobs) { k.RefreshError = "invalid_grant" })
		}
		clk.Advance(time.Hour)

		_, rpcErr := srv.Integrations.credential(context.Background(), "crm")
		if rpcErr == nil || rpcErr.Code != integrationpod.CodeSignInNeeded || rpcErr.Message != "the connection needs the owner to sign in again" {
			t.Fatalf("github %v: error = %+v", githubStyle, rpcErr)
		}
		rec, _ := connectionSecret(t, srv, "crm")
		if rec.SignIn.Error == "" || !rec.SignIn.ErrorAt.Equal(clk.Now()) || !strings.Contains(rec.SignIn.Error, "refused by the test") {
			t.Errorf("github %v: record = %+v", githubStyle, rec.SignIn)
		}
		rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5)
		if len(rows) != 1 || rows[0].Agent != agent.CEOSlug || rows[0].Action != "refresh" || rows[0].Allowed || rows[0].Reason != rec.SignIn.Error {
			t.Errorf("github %v: audit = %+v", githubStyle, rows)
		}
		if _, rpcErr := srv.Integrations.credential(context.Background(), "crm"); rpcErr == nil || rpcErr.Code != integrationpod.CodeSignInNeeded || vendor.Refreshes() != 1 {
			t.Errorf("github %v: a second call = %+v after %d refreshes", githubStyle, rpcErr, vendor.Refreshes())
		}
	}
}

// Why this test exists: a vendor that is down for a moment must not
// cost the owner a sign-in. While the stored token is still good the
// call gets it, and the record is left alone; once it has expired the
// call gets CodeVendorUnavailable, which says to try again, not to sign
// in. Both failures are in the owner's audit.
func TestSignInCredentialWhenTheVendorIsDown(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	tok := vendor.Issue()
	signedIn(t, srv, "crm", tok)
	vendor.Set(func(k *connecttest.Knobs) { k.RefreshStatus = http.StatusBadGateway })
	clk.Advance(time.Hour - 20*time.Second)

	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil || cred.Fields["GH_TOKEN"] != tok.Access {
		t.Fatalf("before expiry: %+v, %+v; want the stored token", cred, rpcErr)
	}
	rec, data := connectionSecret(t, srv, "crm")
	if rec.SignIn.Error != "" || data[connect.SecretKeyAccessToken] != tok.Access {
		t.Errorf("a passing failure changed the connection: %+v", rec.SignIn)
	}

	clk.Advance(time.Minute)
	if _, rpcErr := srv.Integrations.credential(context.Background(), "crm"); rpcErr == nil || rpcErr.Code != integrationpod.CodeVendorUnavailable {
		t.Errorf("after expiry: %+v", rpcErr)
	}
	if rec, _ := connectionSecret(t, srv, "crm"); rec.SignIn.Error != "" {
		t.Errorf("a vendor that is down marked the connection for a new sign-in: %+v", rec.SignIn)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5); len(rows) != 2 || rows[0].Action != "refresh" || rows[0].Allowed || !strings.Contains(rows[0].Reason, "502") {
		t.Errorf("audit = %+v", rows)
	}
}

// Why this test exists: several agents' calls land at once. Two that
// find the token about to expire must cause one refresh, and both get
// the new token: a vendor that rotates refresh tokens refuses the
// second use of the old one, which would mark the connection "sign in
// again" though nothing is wrong. The second call is held up while the
// first one's refresh is in flight.
func TestSignInCredentialRefreshesOnceForCallsAtOnce(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	arrived := make(chan struct{}, 4)
	gate := make(chan struct{})
	vendor.Set(func(k *connecttest.Knobs) {
		k.AccessTTL = time.Hour
		k.Rotate = true
		k.OnRefresh = func() {
			arrived <- struct{}{}
			<-gate
		}
	})
	signedIn(t, srv, "crm", vendor.Issue())
	clk.Advance(time.Hour)

	var wg sync.WaitGroup
	got := make([]string, 2)
	errs := make([]*integrationpod.RPCError, 2)
	call := func(i int) {
		defer wg.Done()
		cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
		errs[i] = rpcErr
		if cred != nil {
			got[i] = cred.Fields["GH_TOKEN"]
		}
	}
	wg.Add(2)
	go call(0)
	<-arrived
	go call(1)
	close(gate)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || got[0] == "" || got[0] != got[1] || !vendor.Valid(got[0]) {
		t.Errorf("answers %v, errors %v %v", got, errs[0], errs[1])
	}
	if n := vendor.Refreshes(); n != 1 {
		t.Errorf("%d refreshes for two calls at once", n)
	}
	if rec, _ := connectionSecret(t, srv, "crm"); rec.SignIn.Error != "" {
		t.Errorf("the connection was marked for a new sign-in: %+v", rec.SignIn)
	}
}

// Why this test exists: a built-in integration is given the connection
// as Connection.Fields, not through kivali/credential. A sign-in
// built-in must find the current access token there under its
// credential_keys name and never the client secret; its answers are
// scrubbed of the token like any secret's; and a connection that needs
// a sign-in refuses the agent's call in words that send it to the
// owner, with the built-in never called.
func TestSignInBuiltinFields(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	echo := echotest.New()
	c := signInConnection(vendor)
	echo.SignIn = &c
	srv.BuiltinIntegrations = builtinintegrations.Registry{echo}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), signInAt); err != nil {
		t.Fatal(err)
	}
	intTeam(t, srv, "dana")
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	tok := vendor.Issue()
	signedIn(t, srv, echotest.Name, tok)
	putIntegrationAccess(t, srv, echotest.Name, "dana", `{}`)

	resp := srv.callAgentIntegration(context.Background(), "dana", "echo_echo", json.RawMessage(`{"say":"`+tok.Access+`"}`))
	if resp.IsError || strings.Contains(resp.Content[0], tok.Access) || !strings.Contains(resp.Content[0], `"say":"[secret]"`) {
		t.Errorf("echo of the access token = %+v", resp)
	}
	calls := echo.Calls()
	if len(calls) != 1 || calls[0].Conn.Fields["GH_TOKEN"] != tok.Access || calls[0].Conn.Fields["client_id"] != connecttest.ClientID {
		t.Fatalf("the built-in was given %+v", calls)
	}
	for k, v := range calls[0].Conn.Fields {
		if k == "client_secret" || v == connecttest.ClientSecret || v == tok.Refresh {
			t.Errorf("the built-in was given %s", k)
		}
	}

	// Past expiry, with a vendor that refuses the refresh: no call.
	vendor.Set(func(k *connecttest.Knobs) { k.RefreshError = "invalid_grant" })
	clk.Advance(time.Hour)
	resp = srv.callAgentIntegration(context.Background(), "dana", "echo_echo", json.RawMessage(`{}`))
	if !resp.IsError || resp.Content[0] != "Echo's connection needs the owner to sign in again; ask the owner." {
		t.Errorf("a connection that needs a sign-in = %+v", resp)
	}
	if len(echo.Calls()) != 1 {
		t.Errorf("the built-in was called with no working token: %+v", echo.Calls())
	}
}

// Why this test exists: an integration's log lines may quote the token
// it was given (an error that echoes a request header). The log
// scrubber must replace the access token, the one refreshed just now
// too, as it replaces any secret field, or the owner's log page would
// show a live credential.
func TestSignInTokensAreScrubbedFromTheLog(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	old := vendor.Issue()
	signedIn(t, srv, "crm", old)
	srv.Integrations.appendLog("crm", "error", "401 with token "+old.Access)
	clk.Advance(time.Hour)
	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	srv.Integrations.ConnectionChanged("crm")
	srv.Integrations.appendLog("crm", "error", "401 with token "+cred.Fields["GH_TOKEN"])
	rows, err := srv.Store.ReadIntegrationLogTail("crm", 5)
	if err != nil || len(rows) != 2 {
		t.Fatalf("log = %+v, %v", rows, err)
	}
	for _, row := range rows {
		if row.Message != "401 with token "+integrationSecretPlaceholder {
			t.Errorf("log line = %q", row.Message)
		}
	}
}

// Why this test exists: deleting a sign-in connection, or disconnecting
// the integration, must ask the vendor to revoke its tokens where the
// vendor allows: the Secret is gone from Kivali, and the tokens should
// not outlive it at the vendor. A vendor that fails the revocation
// does not stop the delete.
func TestSignInDeleteRevokes(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	tok := vendor.Issue()
	signedIn(t, srv, "crm", tok)
	if rr := intDelete(t, srv, intBase+"/crm/connections/c1"); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if got := vendor.Revoked(); len(got) != 2 || got[0] != tok.Refresh || got[1] != tok.Access {
		t.Errorf("revoked %v", got)
	}

	tok = vendor.Issue()
	signedIn(t, srv, "crm", tok)
	vendor.Set(func(k *connecttest.Knobs) { k.RevokeStatus = http.StatusInternalServerError })
	if rr := intDelete(t, srv, intBase+"/crm/connection"); rr.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := srv.Store.GetIntegrationConnectionByID("crm", "c1"); err == nil {
		t.Error("a failed revocation kept the connection")
	}
}

// Why this test exists: a revocation waits on the vendor for up to ten
// seconds. Made while the delete held lockIntegrations, it would stall
// every other integration write for that long: connection saves,
// switches, access, approvals, and the proxy credential a Git call's
// shell job needs. Each delete route must collect the tokens under the
// lock and ask the vendor once it is released, before it answers.
func TestSignInRevokesWithTheLockReleased(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	var free []bool
	vendor.Set(func(k *connecttest.Knobs) {
		k.OnRevoke = func() {
			ok := srv.integrationsMu.TryLock()
			if ok {
				srv.integrationsMu.Unlock()
			}
			free = append(free, ok)
		}
	})
	for _, del := range []func() int{
		func() int { return intDelete(t, srv, intBase+"/crm/connections/c1").Code },
		func() int { return intDelete(t, srv, intBase+"/crm/connection").Code },
		func() int { return intDelete(t, srv, intBase+"/crm").Code },
	} {
		signedIn(t, srv, "crm", vendor.Issue())
		if code := del(); code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("delete: %d", code)
		}
	}
	if len(free) != 6 {
		t.Fatalf("%d revocations, want two per delete", len(free))
	}
	for i, ok := range free {
		if !ok {
			t.Errorf("revocation %d ran while lockIntegrations was held", i)
		}
	}
}

// Why this test exists: a vendor's refusal becomes the connection's
// error, which the page, the Needs you row (as Markdown) and the audit
// show. A vendor that echoes the refresh token it was sent, or answers
// with a page of text, would put a live secret, or a megabyte, in front
// of the owner. The words are cut to MaxDescriptionRunes and scrubbed
// of the connection's tokens and client secret before they are kept.
func TestSignInRefusalWordsAreClippedAndScrubbed(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = time.Hour })
	tok := vendor.Issue()
	signedIn(t, srv, "crm", tok)
	vendor.Set(func(k *connecttest.Knobs) {
		k.RefreshError = "invalid_grant"
		k.RefreshErrorDescription = func(f url.Values) string {
			return "token " + f.Get("refresh_token") + " is revoked " + strings.Repeat("and more words ", 500)
		}
	})
	clk.Advance(time.Hour)
	if _, rpcErr := srv.Integrations.credential(context.Background(), "crm"); rpcErr == nil || rpcErr.Code != integrationpod.CodeSignInNeeded {
		t.Fatalf("error = %+v", rpcErr)
	}
	rec, _ := connectionSecret(t, srv, "crm")
	words := rec.SignIn.Error
	const lead = "the vendor refused the sign-in (invalid_grant): "
	if !strings.HasPrefix(words, lead+"token "+integrationSecretPlaceholder+" is revoked") || strings.Contains(words, tok.Refresh) {
		t.Errorf("error = %q", words)
	}
	if n := utf8.RuneCountInString(words); n > utf8.RuneCountInString(lead)+connect.MaxDescriptionRunes {
		t.Errorf("error is %d runes, longer than the clip", n)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 1)
	if len(rows) != 1 || rows[0].Reason != words {
		t.Errorf("audit = %+v", rows)
	}
}
