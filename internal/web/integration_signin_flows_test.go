package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

// startDeviceSignIn starts crm's setup flow, a device sign-in, and
// returns it once it shows the code.
func startDeviceSignIn(t *testing.T, srv *Server) apitypes.SetupFlow {
	t.Helper()
	startSetupFlow(t, srv, "crm")
	return waitFlow(t, srv, "crm", apitypes.SetupFlowStateWait)
}

// flowEnds waits for crm's flow to end, done or failed, and returns it.
func flowEnds(t *testing.T, srv *Server) apitypes.SetupFlow {
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
		if f.State == apitypes.SetupFlowStateDone || f.State == apitypes.SetupFlowStateFailed {
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

// pollerOf is the channel closed once crm's device flow's poller ends.
func pollerOf(t *testing.T, srv *Server) <-chan struct{} {
	t.Helper()
	srv.setupRuns.mu.Lock()
	defer srv.setupRuns.mu.Unlock()
	if run := srv.setupRuns.currentLocked("crm"); run != nil {
		if d, ok := run.spec.driver.(*signInSetup); ok {
			return d.polled
		}
	}
	t.Fatal("crm has no sign-in under way")
	return nil
}

// waitClosed waits for ch to close, failing the test after
// failsafeWait.
func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(failsafeWait):
		t.Fatal("the sign-in's poller did not end")
	}
}

// decodeFlow is a flow state as a stream sends it.
func decodeFlow(t *testing.T, b []byte) apitypes.SetupFlow {
	t.Helper()
	var f apitypes.SetupFlow
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// crmFlow is crm's flow as GET .../setup answers it.
func crmFlow(t *testing.T, srv *Server) apitypes.SetupFlow {
	t.Helper()
	return orgOK[apitypes.SetupFlow](t, orgGet(t, srv, intBase+"/crm/setup"))
}

// Why this test exists: this is the owner's whole device sign-in, from
// Connect to a working connection, as a setup flow. The Wait must carry
// the vendor's code and page, and the flow must last no longer than
// Kivali's ten minutes, or the page would promise a code Kivali stops
// listening for. Kivali must poll at the vendor's pace (slow_down adds
// five seconds, or GitHub cuts it off), send the client id and never
// the secret, then keep the tokens in the Secret beside the connection's
// own values and the sign-in on the record, with the token's lifetime
// (a refresh whose answer names none would otherwise not know when to
// refresh); restart the bundle's server (whose env otherwise keeps no
// token); run the connection test so the page names who signed in;
// audit the flow for the owner; and from then on hand the token to the
// integration's calls. Before the sign-in its sign-in requirement is
// not met, after it is.
func TestDeviceSignInEndToEnd(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	signInBundle(t, srv, vendor)
	saveSignInClient(t, srv)
	fs := attachShim(t, srv, "crm", toolTexts(map[string]string{"whoami": "Signed in as octocat"}))
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if !slices.Equal(unmetIDs(page), []string{"sign_in"}) || page.Connection.SignIn == nil || page.Connection.SignIn.SignedIn {
		t.Fatalf("before the sign-in: unmet %v, %+v", unmetIDs(page), page.Connection.SignIn)
	}
	vendor.Set(func(k *connecttest.Knobs) {
		k.GitHubStyle = true
		k.DeviceSlowDown = 1
		k.DevicePending = 1
		k.AccessTTL = 8 * time.Hour
	})

	f := startDeviceSignIn(t, srv)
	if st := f.Step; st.Kind != apitypes.SetupFlowStepKindWait || textOf(st.Code) != "WDJB-MJHT" || textOf(st.URL) != vendor.URL+"/activate?user_code=WDJB-MJHT" ||
		st.Words != deviceWaitWords || !f.ExpiresAt.Equal(clk.Now().Add(setupFlowTTL)) {
		t.Errorf("device sign-in = %+v %+v", f, f.Step)
	}
	start := vendor.Requests()[0]
	if start.Path != "/device" || start.Form.Get("client_id") != connecttest.ClientID || start.Form.Has("client_secret") || start.Form.Get("scope") != "repo" {
		t.Errorf("the start sent %s %v", start.Path, start.Form)
	}
	signedInAt := clk.Now().Add(25 * time.Second)
	pollAfter(t, clk, 5*time.Second)  // slow_down
	pollAfter(t, clk, 10*time.Second) // authorization_pending
	pollAfter(t, clk, 10*time.Second) // approved
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateDone || f.Error != nil {
		t.Errorf("after the approval: %+v", f)
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
	if si := page.Connection.SignIn; !connectedAndMet(page) || si == nil || !si.SignedIn || si.Method != "device" {
		t.Errorf("after the sign-in: unmet %v, %+v", unmetIDs(page), page.Connection.SignIn)
	}
	if got := strings.Join(setupAudit(t, srv, "crm"), "|"); got != "started|done" {
		t.Errorf("audit = %s", got)
	}
	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil || cred.Env["GH_TOKEN"] != access {
		t.Errorf("the credential after the sign-in = %+v, %+v", cred, rpcErr)
	}
}

// Why this test exists: with no connection saved, a sign-in's flow asks
// for the connection's form first (the integration's own fields and the
// app's client id and secret, the secret marked), refuses an answer the
// form would refuse with words that leave the flow on the form, keeps
// the answer as a save of the form does, and then shows the code.
func TestDeviceSignInAsksForTheConnectionFirst(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	startSetupFlow(t, srv, "crm")
	f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateForm)
	var keys []string
	for _, fl := range f.Step.Fields {
		keys = append(keys, fl.Key)
		if fl.Key == "client_secret" && !fl.Secret {
			t.Errorf("the client secret is not secret: %+v", fl)
		}
	}
	if strings.Join(keys, ",") != "portal,token,client_id,client_secret" || f.Step.Words != signInFormWords {
		t.Errorf("form = %+v", f.Step)
	}
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{"client_id": connecttest.ClientID}}), http.StatusBadRequest); !strings.HasPrefix(e.Error, "the connection needs ") {
		t.Errorf("a missing field = %+v", e)
	}
	if f := crmFlow(t, srv); f.State != apitypes.SetupFlowStateForm {
		t.Errorf("after a refused answer: %+v", f)
	}
	orgOK[apitypes.SetupFlow](t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup/answer", apitypes.SetupFlowAnswer{Fields: map[string]string{
		"token": "pasted-field-value", "client_id": connecttest.ClientID, "client_secret": connecttest.ClientSecret,
	}}))
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateWait); textOf(f.Step.Code) == "" {
		t.Errorf("after the form: %+v", f.Step)
	}
	rec, data := firstConnectionSecret(t, srv)
	if rec.Fields["client_id"] != connecttest.ClientID || data["client_secret"] != connecttest.ClientSecret || data["token"] != "pasted-field-value" {
		t.Errorf("kept %+v %v", rec.Fields, data)
	}
}

// Why this test exists: a device sign-in ends without tokens in three
// ways, and the page must say which: the owner said no on the service's
// page (the vendor's words), the vendor said the code expired, or the
// code ran out on Kivali's clock while nobody approved. A vendor that
// never answers must not be polled past the flow's ten minutes, or the
// poller would outlive the page for good. None of them may leave tokens
// or a sign-in on the connection, and each is in the owner's audit.
func TestDeviceSignInEnds(t *testing.T) {
	cases := []struct {
		name  string
		knobs func(k *connecttest.Knobs)
		polls []time.Duration
		words string
	}{
		{"access denied", func(k *connecttest.Knobs) { k.DeviceError = "access_denied" }, []time.Duration{5 * time.Second}, "the vendor refused the sign-in (access_denied): ended by the test"},
		{"vendor says expired", func(k *connecttest.Knobs) { k.DeviceError = "expired_token" }, []time.Duration{5 * time.Second}, connect.ErrDeviceExpired.Error()},
		// The vendor's code lasts fifteen minutes; Kivali stops at ten.
		{"ten minutes on the clock", func(k *connecttest.Knobs) {
			k.DevicePending = 1000
			k.DeviceInterval = 5 * time.Minute
		}, []time.Duration{5 * time.Minute, 5 * time.Minute}, connect.ErrDeviceExpired.Error()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, vendor, clk := signInServer(t)
			signInBundle(t, srv, vendor)
			saveSignInClient(t, srv)
			vendor.Set(c.knobs)
			startDeviceSignIn(t, srv)
			for _, d := range c.polls {
				pollAfter(t, clk, d)
			}
			f := flowEnds(t, srv)
			if f.State != apitypes.SetupFlowStateFailed || textOf(f.Error) != c.words {
				t.Errorf("flow = %+v %q", f, textOf(f.Error))
			}
			rec, data := firstConnectionSecret(t, srv)
			if rec.SignIn != nil || data[connect.SecretKeyAccessToken] != "" {
				t.Errorf("a sign-in that ended without tokens left %+v, %v", rec.SignIn, data)
			}
			if got := strings.Join(setupAudit(t, srv, "crm"), "|"); got != "started|refused failed: "+c.words {
				t.Errorf("audit = %s", got)
			}
		})
	}
}

// Why this test exists: a sign-in's flow is refused, with words the
// page shows, where it would undo something or cannot run: a
// connection that is signed in would lose its working tokens to a
// second sign-in, and an integration switched off cancels its flows. A
// connection whose refresh the vendor refused may sign in again: that
// is how the owner mends it.
func TestDeviceSignInRefusals(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	signedIn(t, srv, "crm", vendor.Issue())
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup", apitypes.SetupFlowStart{Flow: "setup"}), http.StatusConflict); e.Error != errSignInSignedIn {
		t.Errorf("signed in = %+v", e)
	}
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	startDeviceSignIn(t, srv)
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/disable"))
	assertAPIError(t, orgGet(t, srv, intBase+"/crm/setup"), http.StatusNotFound)
	if e := assertAPIError(t, flowDo(t, srv, http.MethodPost, intBase+"/crm/setup", apitypes.SetupFlowStart{Flow: "setup"}), http.StatusConflict); e.Error != "switch Test crm on first" {
		t.Errorf("switched off = %+v", e)
	}
}

// Why this test exists: Sign out must leave the connection unusable by
// agents and the vendor's tokens dead: the tokens out of the Secret
// (the connection's own values and the app's client id and secret
// kept, so the owner can sign in again at once), the sign-in off the
// record, both tokens revoked with the vendor, the owner's audit row,
// and the sign-in requirement not met. A flow under way is cancelled with
// it, and the page then reads that none is under way.
func TestSignOut(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	signInBundle(t, srv, vendor)
	tok := vendor.Issue()
	signedIn(t, srv, "crm", tok)

	page := orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/sign-out"))
	if si := page.Connection.SignIn; !slices.Contains(unmetIDs(page), "sign_in") || si == nil || si.SignedIn {
		t.Errorf("page after the sign-out: unmet %v, %+v", unmetIDs(page), si)
	}
	rec, data := connectionSecret(t, srv, "crm")
	if rec.SignIn != nil || data[connect.SecretKeyAccessToken] != "" || data[connect.SecretKeyRefreshToken] != "" ||
		data["client_secret"] != connecttest.ClientSecret || data["token"] != "pasted-field-value" || rec.Fields["client_id"] != connecttest.ClientID {
		t.Errorf("after the sign-out: %+v, %v", rec, data)
	}
	if got := vendor.Revoked(); len(got) != 2 || got[0] != tok.Refresh || got[1] != tok.Access {
		t.Errorf("revoked %v", got)
	}
	if rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5); len(rows) != 1 || rows[0].Action != "sign_out" || !rows[0].Allowed {
		t.Errorf("audit = %+v", rows)
	}

	startDeviceSignIn(t, srv)
	orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/sign-out"))
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/crm/setup"), http.StatusNotFound); e.Error != errSetupNoFlow {
		t.Errorf("after a sign-out mid-sign-in: %+v", e)
	}
}

// Why this test exists: uninstalling the integration while a sign-in
// waits for the owner must end it: an approval that came later would
// otherwise write tokens into a connection that is gone, or bring back
// one the owner removed.
func TestSignInCancelledByUninstall(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	signInBundle(t, srv, vendor)
	signedIn(t, srv, "crm", vendor.Issue())
	srv.markSignInFailed("crm", "c1", "the vendor refused the sign-in (invalid_grant)")
	startDeviceSignIn(t, srv)
	polled := pollerOf(t, srv)
	clk.BlockUntil(1)
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	waitClosed(t, polled)
	if _, err := srv.Store.GetIntegrationConnectionByID("crm", "c1"); err == nil {
		t.Error("the connection is back")
	}
	for _, r := range vendor.Requests() {
		if r.Path == "/token" && r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("a cancelled sign-in polled the vendor: %v", r.Form)
		}
	}
}
