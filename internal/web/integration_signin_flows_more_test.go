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

// Why this test exists: the vendor is asked to start a sign-in off any
// request, so the owner may switch the integration off or delete the
// connection while it answers. The flow must end then, and nothing may
// poll for, or later write tokens into, an integration switched off or
// a connection gone.
func TestDeviceSignInEndsWhenSwitchedOffMeanwhile(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  func(t *testing.T, srv *Server)
	}{
		{"switched off", func(t *testing.T, srv *Server) { intPost(t, srv, intBase+"/crm/disable") }},
		{"deleted", func(t *testing.T, srv *Server) {
			intDelete(t, srv, intBase+"/crm/connections/"+firstConnID(t, srv, "crm"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, vendor, _ := signInServer(t)
			signInBundle(t, srv, vendor)
			saveSignInClient(t, srv)
			started := make(chan struct{})
			vendor.Set(func(k *connecttest.Knobs) {
				k.OnDeviceStart = func() {
					tc.act(t, srv)
					close(started)
				}
			})
			startSetupFlow(t, srv, "crm")
			<-started
			assertAPIError(t, orgGet(t, srv, intBase+"/crm/setup"), http.StatusNotFound)
			if got := strings.Join(setupAudit(t, srv, "crm"), "|"); got != "started|cancelled" {
				t.Errorf("audit = %s", got)
			}
		})
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
	startDeviceSignIn(t, srv)
	fp.fail.Store(true)
	pollAfter(t, clk, 5*time.Second)
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateFailed || textOf(f.Error) != errSignInNotKept.Error() {
		t.Errorf("flow = %+v", f)
	}
	if got := vendor.Revoked(); len(got) != 2 {
		t.Errorf("revoked %v, want the refresh and the access token", got)
	}
	if got := setupAudit(t, srv, "crm"); len(got) != 2 || got[1] != "refused failed: "+errSignInNotKept.Error() {
		t.Errorf("audit = %q", got)
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
	startDeviceSignIn(t, srv)
	pollAfter(t, clk, 5*time.Second)
	f := flowEnds(t, srv)
	code := ""
	for _, r := range vendor.Requests() {
		if r.Path == "/token" {
			code = r.Form.Get("device_code")
		}
	}
	if code == "" || f.Error == nil || strings.Contains(*f.Error, code) || !strings.Contains(*f.Error, integrationSecretPlaceholder) {
		t.Errorf("flow = %+v (device code %q)", f, code)
	}
	for _, row := range setupAudit(t, srv, "crm") {
		if strings.Contains(row, code) {
			t.Errorf("audit = %s", row)
		}
	}
}

// Why this test exists: a sign-in under way was started with the app's
// client id and secret as they were. Saving the form with a new client
// id or secret must end it, or the approval would store tokens of the
// old app under the new one's credentials, and every refresh would be
// refused. A save that leaves both as they were lets it go on.
func TestSaveConnectionCancelsASignInForANewClient(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	startDeviceSignIn(t, srv)
	polled := pollerOf(t, srv)

	rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"client_id": connecttest.ClientID, "portal": "acme"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	if f := crmFlow(t, srv); f.State != apitypes.SetupFlowStateWait {
		t.Errorf("after a save that kept the client: %+v", f)
	}

	rr = orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"client_id": connecttest.ClientID, "client_secret": "a-rotated-secret-value"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	waitClosed(t, polled)
	assertAPIError(t, orgGet(t, srv, intBase+"/crm/setup"), http.StatusNotFound)
}

// Why this test exists: Connect clicked twice, or Try again while the
// first code still waits, starts a second flow for the integration. The
// first must stop polling the vendor at once (two pollers would double
// the rate GitHub cuts off at), and the page must follow the second,
// which is the code it shows.
func TestDeviceSignInSecondStartReplacesTheFirst(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	first := startDeviceSignIn(t, srv)
	firstPolled := pollerOf(t, srv)
	second := startDeviceSignIn(t, srv)
	waitClosed(t, firstPolled)
	if f := crmFlow(t, srv); f.ID != second.ID || f.ID == first.ID || f.State != apitypes.SetupFlowStateWait {
		t.Errorf("the page reads %+v", f)
	}
	pollAfter(t, clk, 5*time.Second)
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateDone {
		t.Errorf("after the second's approval = %+v", f)
	}
	polls := 0
	for _, r := range vendor.Requests() {
		if r.Path == "/token" {
			polls++
		}
	}
	if polls != 1 {
		t.Errorf("%d polls, want the second sign-in's one", polls)
	}
	if got := strings.Join(setupAudit(t, srv, "crm"), "|"); got != "started|cancelled|started|done" {
		t.Errorf("audit = %s", got)
	}
}

// Why this test exists: Sign out of a connection that was never signed
// in and holds no tokens has nothing to undo. It must leave the
// connection as it is (no audit row claiming a sign-out, the last
// connection test kept, nothing sent to the vendor) and answer the
// page. Signing out with no connection at all says so.
func TestSignOutOfNothing(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/sign-out"), http.StatusConflict); e.Error != "Test crm has no connection" {
		t.Errorf("sign out with no connection = %+v", e)
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
