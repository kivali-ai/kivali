package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// failingPuts is a Secrets store whose writes fail once fail is set.
type failingPuts struct {
	secrets.Store
	fail atomic.Bool
}

func (f *failingPuts) Put(ctx context.Context, name string, data map[string]string) error {
	if f.fail.Load() {
		return errors.New("the cluster refused the write")
	}
	return f.Store.Put(ctx, name, data)
}

// signInFlowCount is how many sign-ins srv holds for name.
func signInFlowCount(srv *Server, name string) int {
	srv.signInFlows.mu.Lock()
	defer srv.signInFlows.mu.Unlock()
	n := 0
	for _, f := range srv.signInFlows.flows {
		if f.name == name {
			n++
		}
	}
	return n
}

// Why this test exists: the vendor is asked to start a sign-in without
// the integrations lock, so the owner may switch the integration off or
// delete the connection while it answers. The flow must not be
// registered then, or a goroutine would poll for, and later write
// tokens into, an integration switched off or a connection gone; the
// owner gets the refusal the start would have given at first.
func TestDeviceSignInRechecksAfterTheVendor(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	vendor.Set(func(k *connecttest.Knobs) {
		k.OnDeviceStart = func() { _ = srv.Store.SetIntegrationEnabled("crm", false) }
	})
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/sign-in/device"), http.StatusConflict); e.Error != "switch Test crm on first" {
		t.Errorf("switched off meanwhile = %+v", e)
	}
	if n := signInFlowCount(srv, "crm"); n != 0 {
		t.Errorf("%d sign-ins registered for an integration switched off", n)
	}

	_ = srv.Store.SetIntegrationEnabled("crm", true)
	rec, _ := firstConnectionSecret(t, srv)
	vendor.Set(func(k *connecttest.Knobs) {
		k.OnDeviceStart = func() { _, _, _ = srv.Store.DeleteIntegrationConnectionByID("crm", rec.ID) }
	})
	assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/sign-in/device"), http.StatusNotFound)
	if n := signInFlowCount(srv, "crm"); n != 0 {
		t.Errorf("%d sign-ins registered for a connection gone", n)
	}
}

// Why this test exists: tokens the vendor issued but Kivali could not
// keep (the Secret write failed) are live credentials nothing will ever
// use or revoke. They must be revoked with the vendor at once, and the
// page must say the sign-in was not kept, so the owner starts again.
func TestDeviceSignInNotKeptRevokes(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	fp := &failingPuts{Store: srv.Secrets}
	srv.Secrets = fp
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	_, done := startDeviceSignIn(t, srv)
	fp.fail.Store(true)
	pollAfter(t, clk, 5*time.Second)
	waitDone(t, done)
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateFailed || p.Error == nil || *p.Error != errSignInNotKept.Error() {
		t.Errorf("progress = %+v", p)
	}
	if got := vendor.Revoked(); len(got) != 2 {
		t.Errorf("revoked %v, want the refresh and the access token", got)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5); len(rows) != 1 || rows[0].Action != "sign_in" || rows[0].Allowed {
		t.Errorf("audit = %+v", rows)
	}
}

// Why this test exists: a vendor's refusal may quote the request it
// refused, and the device code is what a device poll sends. Its words
// go to the page, the audit and the log, so the code must be scrubbed
// from them, as a refresh's words are scrubbed of the tokens.
func TestDeviceSignInFailureWordsAreScrubbed(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	vendor.Set(func(k *connecttest.Knobs) { k.DeviceError = "access_denied"; k.DeviceErrorEchoesCode = true })
	_, done := startDeviceSignIn(t, srv)
	pollAfter(t, clk, 5*time.Second)
	waitDone(t, done)
	code := ""
	for _, r := range vendor.Requests() {
		if r.Path == "/token" {
			code = r.Form.Get("device_code")
		}
	}
	p := signInProgress(t, srv)
	if code == "" || p.Error == nil || strings.Contains(*p.Error, code) || !strings.Contains(*p.Error, integrationSecretPlaceholder) {
		t.Errorf("progress = %+v (device code %q)", p, code)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5); len(rows) != 1 || strings.Contains(rows[0].Reason, code) {
		t.Errorf("audit = %+v", rows)
	}
}

// Why this test exists: a sign-in under way was started with the app's
// client id and secret as they were. Saving the form with a new client
// id or secret must cancel it, or the approval would store tokens of
// the old app under the new one's credentials, and every refresh would
// be refused. A save that leaves both as they were lets it go on.
func TestSaveConnectionCancelsASignInForANewClient(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	_, done := startDeviceSignIn(t, srv)

	rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"client_id": connecttest.ClientID, "portal": "acme"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	if p := signInProgress(t, srv); p.State != apitypes.SignInStatePending {
		t.Errorf("after a save that kept the client: %+v", p)
	}

	rr = orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"client_id": connecttest.ClientID, "client_secret": "a-rotated-secret-value"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	waitDone(t, done)
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateExpired || p.Error == nil || *p.Error != errSignInNoFlow {
		t.Errorf("after a new client secret: %+v", p)
	}
}

// Why this test exists: Sign in with a code clicked twice, or Try again
// while the first code still waits, starts a second sign-in for the
// same connection. The first must stop polling the vendor at once (two
// pollers would double the rate GitHub cuts off at), and the page must
// follow the second, which is the code it shows.
func TestDeviceSignInSecondStartReplacesTheFirst(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	_, first := startDeviceSignIn(t, srv)
	_, second := startDeviceSignIn(t, srv)
	waitDone(t, first)
	if n := signInFlowCount(srv, "crm"); n != 1 {
		t.Fatalf("%d sign-ins for one connection", n)
	}
	if p := signInProgress(t, srv); p.State != apitypes.SignInStatePending {
		t.Errorf("progress while the second waits = %+v", p)
	}
	pollAfter(t, clk, 5*time.Second)
	waitDone(t, second)
	polls := 0
	for _, r := range vendor.Requests() {
		if r.Path == "/token" {
			polls++
		}
	}
	if polls != 1 {
		t.Errorf("%d polls, want the second sign-in's one", polls)
	}
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateDone {
		t.Errorf("progress after the second's approval = %+v", p)
	}
}

// Why this test exists: Sign out of a connection that was never signed
// in and holds no tokens has nothing to undo. It must leave the
// connection as it is (no audit row claiming a sign-out, the last
// connection test kept, nothing sent to the vendor) and answer the page. And
// asking how a sign-in stands, or signing out, with no connection at
// all names that, not Sign in's "save the client id and secret first".
func TestSignOutOfNothing(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	for _, path := range []string{"/crm/connection/sign-out", "/crm/connection/sign-in"} {
		rr := intPost(t, srv, intBase+path)
		if path == "/crm/connection/sign-in" {
			rr = orgGet(t, srv, intBase+path)
		}
		if e := assertAPIError(t, rr, http.StatusConflict); e.Error != "Test crm has no connection" {
			t.Errorf("%s with no connection = %+v", path, e)
		}
	}

	saveSignInClient(t, srv)
	rec, _ := firstConnectionSecret(t, srv)
	at, yes := signInAt, true
	rec.VerifiedAt, rec.Verified, rec.VerifyMessage = &at, &yes, "Signed in as octocat"
	if err := srv.Store.PutIntegrationConnection("crm", rec); err != nil {
		t.Fatal(err)
	}
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/sign-out"))
	if after, _ := firstConnectionSecret(t, srv); after.Verified == nil || after.VerifyMessage != "Signed in as octocat" {
		t.Errorf("a sign-out of nothing cleared the test: %+v", after)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5); len(rows) != 0 {
		t.Errorf("audit = %+v", rows)
	}
	if got := vendor.Revoked(); len(got) != 0 {
		t.Errorf("revoked %v", got)
	}
}
