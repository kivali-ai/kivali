package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// saveSignInClient saves crm's one connection as the owner does
// before signing in: its own field and the app's client id and
// secret.
func saveSignInClient(t *testing.T, srv *Server) {
	t.Helper()
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{
		"token": "pasted-field-value", "client_id": connecttest.ClientID, "client_secret": connecttest.ClientSecret,
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
}

// firstConnectionSecret is crm's one connection, whatever its id, and
// its Secret's data.
func firstConnectionSecret(t *testing.T, srv *Server) (store.IntegrationConnection, map[string]string) {
	t.Helper()
	conns, err := srv.Store.ListIntegrationConnections("crm")
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections = %+v, %v", conns, err)
	}
	data, err := srv.Secrets.Get(context.Background(), conns[0].SecretName)
	if err != nil {
		t.Fatal(err)
	}
	return conns[0], data
}

// startDeviceSignIn starts the device sign-in of crm's connection and
// returns the answer and the flow's done channel.
func startDeviceSignIn(t *testing.T, srv *Server) (apitypes.DeviceSignIn, <-chan struct{}) {
	t.Helper()
	ds := orgOK[apitypes.DeviceSignIn](t, intPost(t, srv, intBase+"/crm/connection/sign-in/device"))
	return ds, signInFlowDone(t, srv, "crm")
}

// signInFlowDone is the done channel of name's one flow.
func signInFlowDone(t *testing.T, srv *Server, name string) <-chan struct{} {
	t.Helper()
	srv.signInFlows.mu.Lock()
	defer srv.signInFlows.mu.Unlock()
	for _, f := range srv.signInFlows.flows {
		if f.name == name {
			return f.done
		}
	}
	t.Fatalf("%s has no sign-in under way", name)
	return nil
}

// waitDone waits for a flow's goroutine to end, failing the test
// after failsafeWait.
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(failsafeWait):
		t.Fatal("the sign-in's goroutine did not end")
	}
}

// pollAfter lets the flow's poll timer arm, checks it waits want, and
// moves the clock past it.
func pollAfter(t *testing.T, clk *clock.Fake, want time.Duration) {
	t.Helper()
	clk.BlockUntil(1)
	if d := clk.Deadlines()[0].Sub(clk.Now()); d != want {
		t.Fatalf("the poll waits %v, want %v", d, want)
	}
	clk.Advance(want)
}

func signInProgress(t *testing.T, srv *Server) apitypes.SignInProgress {
	t.Helper()
	return orgOK[apitypes.SignInProgress](t, orgGet(t, srv, intBase+"/crm/connection/sign-in"))
}

// Why this test exists: this is the owner's whole device sign-in, from
// Sign in with a code to a working connection. The answer must carry
// the vendor's code and page, and an expiry no later than Kivali's ten
// minutes, or the page would promise a code Kivali stops listening for.
// Kivali must poll at the vendor's pace (slow_down adds five seconds,
// or GitHub cuts it off), send the client id and never the secret, then
// keep the tokens in the Secret beside the connection's own values and
// the sign-in on the record, with the token's lifetime (a refresh whose
// answer names none would otherwise not know when to refresh); restart
// the bundle's server (whose env otherwise keeps no token); run the
// connection test so the page names
// who signed in; audit the sign-in for the owner; and from then on
// hand the token to the integration's calls. Before the sign-in the
// integration reads not connected, after it connected.
func TestDeviceSignInEndToEnd(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	fs := attachShim(t, srv, "crm", toolTexts(map[string]string{"whoami": "Signed in as octocat"}))
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if page.Summary.Status != apitypes.IntegrationStatusNotConnected || page.Connection.SignIn == nil || page.Connection.SignIn.SignedIn {
		t.Fatalf("before the sign-in: %s, %+v", page.Summary.Status, page.Connection.SignIn)
	}
	vendor.Set(func(k *connecttest.Knobs) {
		k.GitHubStyle = true
		k.DeviceSlowDown = 1
		k.DevicePending = 1
		k.AccessTTL = 8 * time.Hour
	})

	ds, done := startDeviceSignIn(t, srv)
	if ds.UserCode != "WDJB-MJHT" || ds.VerificationURL != vendor.URL+"/activate" || ds.VerificationURLComplete == nil ||
		*ds.VerificationURLComplete != vendor.URL+"/activate?user_code=WDJB-MJHT" || !ds.ExpiresAt.Equal(clk.Now().Add(signInFlowTTL)) {
		t.Errorf("device sign-in = %+v", ds)
	}
	start := vendor.Requests()[0]
	if start.Path != "/device" || start.Form.Get("client_id") != connecttest.ClientID || start.Form.Has("client_secret") || start.Form.Get("scope") != "repo" {
		t.Errorf("the start sent %s %v", start.Path, start.Form)
	}
	if p := signInProgress(t, srv); p.State != apitypes.SignInStatePending || p.Error != nil {
		t.Errorf("while waiting: %+v", p)
	}
	signedInAt := clk.Now().Add(25 * time.Second)
	pollAfter(t, clk, 5*time.Second)  // slow_down
	pollAfter(t, clk, 10*time.Second) // authorization_pending
	pollAfter(t, clk, 10*time.Second) // approved
	waitDone(t, done)

	if p := signInProgress(t, srv); p.State != apitypes.SignInStateDone || p.Error != nil {
		t.Errorf("after the approval: %+v", p)
	}
	rec, data := firstConnectionSecret(t, srv)
	access := data[connect.SecretKeyAccessToken]
	if !vendor.Valid(access) || !vendor.RefreshAccepted(data[connect.SecretKeyRefreshToken]) ||
		data["token"] != "pasted-field-value" || data["client_secret"] != connecttest.ClientSecret {
		t.Errorf("Secret = %v", data)
	}
	if si := rec.SignIn; si == nil || si.Method != "device" || !si.SignedInAt.Equal(signedInAt) ||
		!si.ExpiresAt.Equal(signedInAt.Add(8*time.Hour)) || si.Lifetime != 8*time.Hour || si.Scope != "repo" || si.Error != "" {
		t.Errorf("record = %+v", rec.SignIn)
	}
	if rec.Verified == nil || !*rec.Verified || rec.VerifyMessage != "Signed in as octocat" {
		t.Errorf("connection test after the sign-in = %v %q", rec.Verified, rec.VerifyMessage)
	}
	fs.next(t, integrationpod.EventRestart)

	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if si := page.Connection.SignIn; page.Summary.Status != apitypes.IntegrationStatusConnected || si == nil || !si.SignedIn || si.Method != "device" {
		t.Errorf("after the sign-in: %s, %+v", page.Summary.Status, page.Connection.SignIn)
	}
	rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5)
	var signIn *store.IntegrationAuditRow
	for i := range rows {
		if rows[i].Action == "sign_in" {
			signIn = &rows[i]
		}
	}
	if signIn == nil || signIn.Agent != agent.CEOSlug || !signIn.Allowed || signIn.Reason != "" {
		t.Errorf("audit = %+v", rows)
	}
	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil || cred.Env["GH_TOKEN"] != access {
		t.Errorf("the credential after the sign-in = %+v, %+v", cred, rpcErr)
	}
}

// Why this test exists: a device sign-in ends without tokens in three
// ways, and the page must tell the owner which: the owner said no on
// the service's page (failed, with the vendor's words), the vendor
// said the code expired, or the code ran out on Kivali's clock while
// nobody approved (both expired: start again). A vendor that never
// answers must not be polled past the sign-in's ten minutes, or the
// goroutine would outlive the page for good. None of them may leave
// tokens or a sign-in on the connection, and each is in the owner's
// audit.
func TestDeviceSignInEnds(t *testing.T) {
	cases := []struct {
		name  string
		knobs func(k *connecttest.Knobs)
		// polls are the waits until the flow ends.
		polls []time.Duration
		state apitypes.SignInState
		words string
	}{
		{"access denied", func(k *connecttest.Knobs) { k.DeviceError = "access_denied" }, []time.Duration{5 * time.Second}, apitypes.SignInStateFailed, "the vendor refused the sign-in (access_denied): ended by the test"},
		{"vendor says expired", func(k *connecttest.Knobs) { k.DeviceError = "expired_token" }, []time.Duration{5 * time.Second}, apitypes.SignInStateExpired, connect.ErrDeviceExpired.Error()},
		// The vendor's code lasts fifteen minutes; Kivali stops at ten.
		{"ten minutes on the clock", func(k *connecttest.Knobs) {
			k.DevicePending = 1000
			k.DeviceInterval = 5 * time.Minute
		}, []time.Duration{5 * time.Minute, 5 * time.Minute}, apitypes.SignInStateExpired, connect.ErrDeviceExpired.Error()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, vendor, clk := signInServer(t)
			signInBundle(t, srv, vendor)
			saveSignInClient(t, srv)
			vendor.Set(c.knobs)
			_, done := startDeviceSignIn(t, srv)
			for _, d := range c.polls {
				pollAfter(t, clk, d)
			}
			waitDone(t, done)
			p := signInProgress(t, srv)
			if p.State != c.state || p.Error == nil || *p.Error != c.words {
				t.Errorf("progress = %+v", p)
			}
			rec, data := firstConnectionSecret(t, srv)
			if rec.SignIn != nil || data[connect.SecretKeyAccessToken] != "" {
				t.Errorf("a sign-in that ended without tokens left %+v, %v", rec.SignIn, data)
			}
			rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5)
			if len(rows) != 1 || rows[0].Action != "sign_in" || rows[0].Allowed || rows[0].Reason != c.words {
				t.Errorf("audit = %+v", rows)
			}
		})
	}
}

// Why this test exists: Sign in with a code is refused, with words the
// page shows, where it cannot work or would undo something: an
// integration connected with a pasted token has nothing to sign in
// to; without the app's client id and secret stored the vendor has no
// client to sign in for; a connection that is signed in would lose
// its working tokens to a second sign-in; an integration switched off
// cancels its sign-ins. A connection whose refresh the vendor refused
// may sign in again: that is how the owner mends it.
func TestDeviceSignInRefusals(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	intInstalled(t, srv, "plain", "1.0.0")
	if e := assertAPIError(t, intPost(t, srv, intBase+"/plain/connection/sign-in/device"), http.StatusConflict); e.Error != "Test plain is not connected by signing in" {
		t.Errorf("a token integration = %+v", e)
	}
	signInBundle(t, srv, vendor)
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/sign-in/device"), http.StatusConflict); e.Error != errSignInClientFirst {
		t.Errorf("no connection = %+v", e)
	}
	assertAPIError(t, intPost(t, srv, intBase+"/crm/connections/nope/sign-in/device"), http.StatusNotFound)

	signedIn(t, srv, "crm", vendor.Issue())
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connections/c1/sign-in/device"), http.StatusConflict); e.Error != errSignInSignedIn {
		t.Errorf("signed in = %+v", e)
	}
	// The Secret lost the client secret: nothing to sign in with.
	rec, data := connectionSecret(t, srv, "crm")
	delete(data, "client_secret")
	delete(data, connect.SecretKeyAccessToken)
	if err := srv.Secrets.Put(context.Background(), rec.SecretName, data); err != nil {
		t.Fatal(err)
	}
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/sign-in/device"), http.StatusConflict); e.Error != errSignInClientFirst {
		t.Errorf("no client secret = %+v", e)
	}

	signedIn(t, srv, "crm", vendor.Issue())
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	if rr := intPost(t, srv, intBase+"/crm/connection/sign-in/device"); rr.Code != http.StatusOK {
		t.Errorf("signing in again: %d %s", rr.Code, rr.Body.String())
	}
	waitDone(t, func() <-chan struct{} {
		done := signInFlowDone(t, srv, "crm")
		intPost(t, srv, intBase+"/crm/disable")
		return done
	}())
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/sign-in/device"), http.StatusConflict); e.Error != "switch Test crm on first" {
		t.Errorf("switched off = %+v", e)
	}
}

// Why this test exists: Sign out must leave the connection unusable by
// agents and the vendor's tokens dead: the tokens out of the Secret
// (the connection's own values and the app's client id and secret
// kept, so the owner can sign in again at once), the sign-in off the
// record, both tokens revoked with the vendor, the owner's audit row,
// and the integration not connected. A sign-in under way is cancelled
// with it, and the page then reads that none is under way.
func TestSignOut(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	tok := vendor.Issue()
	signedIn(t, srv, "crm", tok)

	page := orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connections/c1/sign-out"))
	if si := page.Connection.SignIn; page.Summary.Status == apitypes.IntegrationStatusConnected || si == nil || si.SignedIn {
		t.Errorf("page after the sign-out: %s, %+v", page.Summary.Status, si)
	}
	rec, data := connectionSecret(t, srv, "crm")
	if rec.SignIn != nil || data[connect.SecretKeyAccessToken] != "" || data[connect.SecretKeyRefreshToken] != "" ||
		data["client_secret"] != connecttest.ClientSecret || data["token"] != "pasted-field-value" || rec.Fields["client_id"] != connecttest.ClientID {
		t.Errorf("after the sign-out: %+v, %v", rec, data)
	}
	if got := vendor.Revoked(); len(got) != 2 || got[0] != tok.Refresh || got[1] != tok.Access {
		t.Errorf("revoked %v", got)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5); len(rows) != 1 || rows[0].Action != "sign_out" || !rows[0].Allowed || rows[0].Agent != agent.CEOSlug {
		t.Errorf("audit = %+v", rows)
	}

	_, done := startDeviceSignIn(t, srv)
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/sign-out"))
	waitDone(t, done)
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateExpired || p.Error == nil || *p.Error != errSignInNoFlow {
		t.Errorf("after a sign-out mid-sign-in: %+v", p)
	}
}

// Why this test exists: deleting the connection, disconnecting or
// removing the integration while a sign-in waits for the owner must
// end it: an approval that came later would otherwise write tokens
// into a connection that is gone, or bring back one the owner removed.
func TestSignInCancelledByDelete(t *testing.T) {
	for _, path := range []string{intBase + "/crm/connections/c1", intBase + "/crm/connection", intBase + "/crm"} {
		srv, vendor, clk := signInServer(t)
		signInBundle(t, srv, vendor)
		signedIn(t, srv, "crm", vendor.Issue())
		srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
		_, done := startDeviceSignIn(t, srv)
		clk.BlockUntil(1)
		if rr := intDelete(t, srv, path); rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		waitDone(t, done)
		if _, err := srv.Store.GetIntegrationConnectionByID("crm", "c1"); err == nil {
			t.Errorf("%s: the connection is back", path)
		}
		for _, r := range vendor.Requests() {
			if r.Path == "/token" && r.Form.Get("grant_type") != "refresh_token" {
				t.Errorf("%s: a cancelled sign-in polled the vendor: %v", path, r.Form)
			}
		}
	}
}

// Why this test exists: sign-ins live in memory, so a restart loses
// the one the page was waiting on. GET .../sign-in must then answer
// expired with words that say why, so the page offers Try again rather
// than wait for ever.
func TestSignInProgressWithNoFlow(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	p := signInProgress(t, srv)
	if p.State != apitypes.SignInStateExpired || p.Error == nil || !strings.Contains(*p.Error, "restart") {
		t.Errorf("progress = %+v", p)
	}
}
