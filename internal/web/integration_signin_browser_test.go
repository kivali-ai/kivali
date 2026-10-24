package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// browserSetupNotes is the browser bundle's kivali.setup_notes, which
// names the redirect address.
const browserSetupNotes = "Create an app, set its redirect URL to {{redirect_url}} and tick **content**."

// browserBundle stores and approves a bundle called crm whose manifest
// signs in in the browser against vendor, HubSpot's way: a scope, an
// optional one passed as an extra parameter, a thirty-minute token.
// edit, when set, changes the connection section first.
func browserBundle(t *testing.T, srv *Server, vendor *connecttest.FakeVendor, edit func(c *integration.Connection)) {
	t.Helper()
	browserBundleNamed(t, srv, vendor, "crm", edit)
}

// browserBundleNamed is browserBundle under another name.
func browserBundleNamed(t *testing.T, srv *Server, vendor *connecttest.FakeVendor, name string, edit func(c *integration.Connection)) {
	t.Helper()
	ep := vendor.Endpoints()
	c := integration.Connection{
		Method: integration.MethodBrowser, AuthorizationEndpoint: ep.Authorization, TokenEndpoint: ep.Token, RevocationEndpoint: ep.Revocation,
		Scopes: []string{"content"}, ExtraAuthorizationParams: map[string]string{"optional_scope": "crm.objects.contacts.read"},
		CredentialKeys: map[string]string{"access_token": "HUBSPOT_ACCESS_TOKEN"},
	}
	if edit != nil {
		edit(&c)
	}
	m := integrationtest.Manifest(name, "1.0.0")
	m["kivali"].(map[string]any)["connection"] = c
	m["kivali"].(map[string]any)["setup_notes"] = browserSetupNotes
	b, err := integration.ReadBundle(integrationtest.Zip(t, "", m, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.StoreIntegrationVersion(name, b, store.IntegrationVersion{PublishedBy: agent.CEOSlug, PublishedAt: signInAt}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.ApproveIntegrationVersion(name, "1.0.0", signInAt); err != nil {
		t.Fatal(err)
	}
	vendor.Set(func(k *connecttest.Knobs) { k.AccessTTL = 30 * time.Minute })
}

// browserServer is a sign-in server with crm installed for browser
// sign-in and its client id and secret saved.
func browserServer(t *testing.T) (*Server, *connecttest.FakeVendor) {
	t.Helper()
	srv, vendor, _ := signInServer(t)
	browserBundle(t, srv, vendor, nil)
	saveSignInClient(t, srv)
	return srv, vendor
}

// serveSignedIn serves req through the whole server, signed in as the
// owner, as the browser that carries cookies sends it.
func serveSignedIn(t *testing.T, srv *Server, req *http.Request, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req.AddCookie(authCookie(t, srv))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if req.Method != http.MethodGet {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// loopbackOrigin is where the tests' pages are, Kivali Desktop's way: a
// browser sign-in started there can finish (browserSignInHere).
const loopbackOrigin = "http://127.0.0.1:8080"

// startBrowserNamed starts name's browser sign-in from a page on
// loopback, follows its Go step to /connect/start and returns the
// vendor's sign-in page it sends the browser to and the flow's cookie.
func startBrowserNamed(t *testing.T, srv *Server, name string) (*url.URL, *http.Cookie) {
	t.Helper()
	startSetupFlow(t, srv, name)
	f := waitFlow(t, srv, name, apitypes.SetupFlowStateGo)
	rr, cookie := startPage(t, srv, f)
	if rr.Code != http.StatusSeeOther || cookie == nil {
		t.Fatalf("start: %d, cookie %v", rr.Code, cookie)
	}
	u, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u, cookie
}

// startBrowser is startBrowserNamed for crm.
func startBrowser(t *testing.T, srv *Server) (*url.URL, *http.Cookie) {
	t.Helper()
	return startBrowserNamed(t, srv, "crm")
}

// vendorApprove stands for the owner approving on the vendor's page: it asks
// the vendor's authorization page and returns where the vendor sends
// the browser back to.
func vendorApprove(t *testing.T, authorize *url.URL) *url.URL {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the vendor answered %d", resp.StatusCode)
	}
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// callback is the browser coming back to Kivali at back with cookies;
// it returns where Kivali sends it on, path and query.
func callback(t *testing.T, srv *Server, back *url.URL, cookies ...*http.Cookie) *url.URL {
	t.Helper()
	rr := serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, back.String(), nil), cookies...)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("the callback answered %d %s", rr.Code, rr.Body.String())
	}
	to, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return to
}

// landedOnSetup fails unless a callback sent the browser to name's page
// with ?setup=<a flow id> and nothing else, and returns the id.
func landedOnSetup(t *testing.T, to *url.URL, name string) string {
	t.Helper()
	q := to.Query()
	if to.Path != "/org/integrations/"+name || len(q) != 1 || len(q.Get("setup")) != 32 {
		t.Fatalf("landed on %s, want %s's page with ?setup=", to, name)
	}
	return q.Get("setup")
}

// signedInThere fails unless the callback sent the browser to crm's
// page and its flow then ended done.
func signedInThere(t *testing.T, srv *Server, to *url.URL) {
	t.Helper()
	id := landedOnSetup(t, to, "crm")
	if f := flowEnds(t, srv); f.ID != id || f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("the flow ended %+v %q", f, textOf(f.Error))
	}
}

// failedWords is why the flow a callback sent the browser to failed, as
// the page reads it.
func failedWords(t *testing.T, srv *Server, to *url.URL) string {
	t.Helper()
	id := landedOnSetup(t, to, "crm")
	f := flowEnds(t, srv)
	if f.ID != id || f.State != apitypes.SetupFlowStateFailed || f.Error == nil {
		t.Fatalf("the flow reads %+v", f)
	}
	return *f.Error
}

// startAgain fails unless a callback sent the browser to path with
// ?sign_in_error=start_again and nothing else.
func startAgain(t *testing.T, to *url.URL, path string) {
	t.Helper()
	if q := to.Query(); to.Path != path || q.Get("sign_in_error") != "start_again" || len(q) != 1 {
		t.Errorf("landed on %s, want %s?sign_in_error=start_again", to, path)
	}
}

// tokenRequests counts the code exchanges the vendor took.
func tokenRequests(vendor *connecttest.FakeVendor) int {
	n := 0
	for _, r := range vendor.Requests() {
		if r.Path == "/token" && r.Form.Get("grant_type") == "authorization_code" {
			n++
		}
	}
	return n
}

// assertNotSignedIn fails when crm's connection holds tokens or a
// sign-in.
func assertNotSignedIn(t *testing.T, srv *Server) {
	t.Helper()
	rec, data := firstConnectionSecret(t, srv)
	if rec.SignIn != nil || data[connect.SecretKeyAccessToken] != "" || data[connect.SecretKeyRefreshToken] != "" {
		t.Errorf("signed in after a refused callback: %+v, %v", rec.SignIn, data)
	}
}

// Why this test exists: this is the owner's whole browser sign-in, as a
// browser does it, through the real server and its session middleware:
// Connect, the Go step's /connect/start, the vendor's page, the vendor
// sending the browser back to /connect/callback, and the integration's
// page. The vendor's page must carry the client id, the redirect
// address, the scopes, the manifest's extra parameters, a random state
// and a PKCE challenge (S256), or the vendor refuses or the code can be
// redeemed by whoever intercepts it. The cookie that binds the browser
// must be HttpOnly, SameSite=Lax (the vendor's redirect is a cross-site
// navigation) and host-only. Kivali must exchange the code with the
// verifier and the client secret, keep the tokens and the sign-in as
// browser, restart the bundle's server, run the connection test, audit
// the flow, drop the cookie and land the browser on the page with the
// flow. A step missing here is a sign-in that fails for every owner of
// a vendor like HubSpot.
func TestBrowserSignInEndToEnd(t *testing.T) {
	srv, vendor, clk := signInServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	browserBundle(t, srv, vendor, nil)
	saveSignInClient(t, srv)
	fs := attachShim(t, srv, "crm", toolTexts(map[string]string{"whoami": "Connected as Acme Marketing"}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	base, _ := url.Parse(ts.URL)
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(base, []*http.Cookie{authCookie(t, srv)})
	var landed *url.URL
	browser := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(req.URL.Path, "/org/") {
			landed = req.URL
			return http.ErrUseLastResponse
		}
		return nil
	}}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+intBase+"/crm/setup", bytes.NewReader([]byte(`{"flow":"setup"}`)))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var started apitypes.SetupFlow
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("start: %d %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateGo)
	if f.Step.Words != browserGoWords || textOf(f.Step.Host) != strings.TrimPrefix(vendor.URL, "http://") {
		t.Errorf("go = %+v", f.Step)
	}

	// /connect/start, seen without following it.
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noFollow.Get(ts.URL + *f.Step.StartURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	setCookie := resp.Header.Get("Set-Cookie")
	if !strings.HasPrefix(setCookie, "kivali_connect=") || !strings.Contains(setCookie, "Path=/;") ||
		!strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Lax") || strings.Contains(setCookie, "Domain=") {
		t.Errorf("Set-Cookie = %q", setCookie)
	}
	authorize, _ := url.Parse(resp.Header.Get("Location"))
	q := authorize.Query()
	if authorize.Path != "/authorize" || q.Get("client_id") != connecttest.ClientID || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != ts.URL+"/connect/callback" || q.Get("scope") != "content" ||
		q.Get("optional_scope") != "crm.objects.contacts.read" || q.Get("code_challenge_method") != "S256" ||
		len(q.Get("code_challenge")) != 43 || len(q.Get("state")) != 43 || q.Has("client_secret") {
		t.Errorf("the vendor's page = %s", authorize)
	}

	signedInAt := clk.Now()
	resp, err = browser.Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || landed == nil || landed.Path != "/org/integrations/crm" {
		t.Fatalf("landed on %v after %d", landed, resp.StatusCode)
	}
	if got := landed.Query(); got.Get("setup") != f.ID || len(got) != 1 {
		t.Errorf("landed on %s", landed)
	}
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("flow = %+v %q", f, textOf(f.Error))
	}
	rec, data := firstConnectionSecret(t, srv)
	var exchange *connecttest.Request
	for _, r := range vendor.Requests() {
		if r.Path == "/token" {
			exchange = &r
		}
	}
	if exchange == nil || exchange.Form.Get("grant_type") != "authorization_code" || exchange.Form.Get("redirect_uri") != ts.URL+"/connect/callback" ||
		exchange.Form.Get("client_secret") != connecttest.ClientSecret || connect.PKCEChallenge(exchange.Form.Get("code_verifier")) != q.Get("code_challenge") {
		t.Errorf("the exchange = %+v", exchange)
	}
	if !vendor.Valid(data[connect.SecretKeyAccessToken]) || !vendor.RefreshAccepted(data[connect.SecretKeyRefreshToken]) || data["token"] != "pasted-field-value" {
		t.Errorf("Secret = %v", data)
	}
	if si := rec.SignIn; si == nil || si.Method != "browser" || !si.SignedInAt.Equal(signedInAt) || !si.ExpiresAt.Equal(signedInAt.Add(30*time.Minute)) || si.Scope != "repo" {
		t.Errorf("record = %+v", rec.SignIn)
	}
	if rec.Verified == nil || !*rec.Verified || rec.VerifyMessage != "Connected as Acme Marketing" {
		t.Errorf("connection test after the sign-in = %v %q", rec.Verified, rec.VerifyMessage)
	}
	fs.next(t, integrationpod.EventRestart)
	for _, c := range jar.Cookies(&url.URL{Scheme: "http", Host: base.Host, Path: "/connect/callback"}) {
		if c.Name == "kivali_connect" {
			t.Errorf("the sign-in cookie outlived the callback: %v", c)
		}
	}
	if got := strings.Join(setupAudit(t, srv, "crm"), "|"); got != "started|returned|done" {
		t.Errorf("audit = %s", got)
	}
	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil || cred.Env["HUBSPOT_ACCESS_TOKEN"] != data[connect.SecretKeyAccessToken] {
		t.Errorf("the credential = %+v, %+v", cred, rpcErr)
	}
}

// Why this test exists: a browser holds one flow cookie, so of two Go
// steps started in it (two integrations' pages, two tabs) only the
// later can come back. The earlier must end at once, and its callback
// must land on its own page, whose flow says another one replaced it,
// without taking the later's cookie with it: the later sign-in must
// still finish. Without this both would fail, the first for its cookie
// and the second because the first callback dropped it.
func TestBrowserSignInOneAtATimePerBrowser(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	browserBundle(t, srv, vendor, nil)
	browserBundleNamed(t, srv, vendor, "crm2", nil)
	for _, name := range []string{"crm", "crm2"} {
		rr := orgJSON(t, srv, http.MethodPut, intBase+"/"+name+"/connection", apitypes.ConnectionWrite{Fields: map[string]string{
			"token": "pasted-field-value", "client_id": connecttest.ClientID, "client_secret": connecttest.ClientSecret,
		}})
		if rr.Code != http.StatusOK {
			t.Fatalf("save %s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	first, firstCookie := startBrowserNamed(t, srv, "crm")
	// The second /connect/start comes from the same browser, which
	// holds the first's cookie.
	startSetupFlow(t, srv, "crm2")
	f2 := waitFlow(t, srv, "crm2", apitypes.SetupFlowStateGo)
	rr := flowDo(t, srv, http.MethodGet, *f2.Step.StartURL, nil, firstCookie)
	second, _ := url.Parse(rr.Header().Get("Location"))
	secondCookie := cookieOf(rr, "kivali_connect")

	to := callback(t, srv, vendorApprove(t, first), secondCookie)
	if words := failedWords(t, srv, to); words != errSignInReplaced {
		t.Errorf("the first sign-in reads %q", words)
	}
	if n := tokenRequests(vendor); n != 0 {
		t.Errorf("%d exchanges for the replaced sign-in", n)
	}
	to = callback(t, srv, vendorApprove(t, second), secondCookie)
	landedOnSetup(t, to, "crm2")
	if f := waitFlow(t, srv, "crm2", apitypes.SetupFlowStateDone); f.ID != f2.ID {
		t.Errorf("crm2 = %+v", f)
	}
}

// Why this test exists: a callback replayed (the browser's back button,
// a copied address, an attacker who saw it) after the flow moved on
// must not run the exchange again, nor overwrite the sign-in, even when
// it carries the cookie: it lands on the page, which reads the flow as
// it is.
func TestBrowserSignInReplayRunsNothing(t *testing.T) {
	srv, vendor := browserServer(t)
	authorize, cookie := startBrowser(t, srv)
	back := vendorApprove(t, authorize)
	signedInThere(t, srv, callback(t, srv, back, cookie))
	rec, data := firstConnectionSecret(t, srv)

	landedOnSetup(t, callback(t, srv, back, cookie), "crm")
	if n := tokenRequests(vendor); n != 1 {
		t.Errorf("%d exchanges, want 1", n)
	}
	again, againData := firstConnectionSecret(t, srv)
	if !again.SignIn.SignedInAt.Equal(rec.SignIn.SignedInAt) || againData[connect.SecretKeyAccessToken] != data[connect.SecretKeyAccessToken] {
		t.Errorf("the replay changed the sign-in")
	}
}

// Why this test exists: a flow lasts ten minutes after its last change
// on Kivali's clock. A vendor's redirect that comes later (an owner who
// left the vendor's page open) must find nothing, with no exchange: the
// flow's verifier is gone with it. The cookie still names the
// integration and its flow, so the browser lands on its page, which
// says to start again.
func TestBrowserSignInExpires(t *testing.T) {
	srv, vendor := browserServer(t)
	clk := srv.Clock.(interface{ Advance(time.Duration) })
	authorize, cookie := startBrowser(t, srv)
	back := vendorApprove(t, authorize)
	clk.Advance(setupFlowTTL)

	landedOnSetup(t, callback(t, srv, back, cookie), "crm")
	assertAPIError(t, orgGet(t, srv, intBase+"/crm/setup"), http.StatusNotFound)
	if n := tokenRequests(vendor); n != 0 {
		t.Errorf("%d exchanges after the sign-in expired", n)
	}
	assertNotSignedIn(t, srv)
}

// Why this test exists: the cookie binds the flow to the browser that
// left. A callback without it (another browser, or a link an attacker
// sends the owner to sign the owner's connection in to the attacker's
// account) must end the flow as started elsewhere, with no exchange, so
// the right browser cannot be raced either. A cookie Kivali did not
// sign is no cookie.
func TestBrowserSignInNeedsTheCookie(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cookie func(c *http.Cookie) []*http.Cookie
	}{
		{"none", func(*http.Cookie) []*http.Cookie { return nil }},
		{"forged", func(c *http.Cookie) []*http.Cookie {
			other, _ := auth.NewCodec([]byte("another key of thirty two bytes!"))
			return []*http.Cookie{{Name: c.Name, Value: other.Sign([]byte("connect:0000:crm"))}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, vendor := browserServer(t)
			authorize, cookie := startBrowser(t, srv)
			back := vendorApprove(t, authorize)

			if words := failedWords(t, srv, callback(t, srv, back, tc.cookie(cookie)...)); words != errSignInStartAgain {
				t.Errorf("the page reads %q", words)
			}
			// The right browser after a refused callback finds the flow ended.
			landedOnSetup(t, callback(t, srv, back, cookie), "crm")
			if n := tokenRequests(vendor); n != 0 {
				t.Errorf("%d exchanges without the cookie", n)
			}
			assertNotSignedIn(t, srv)
		})
	}
}

// Why this test exists: when the owner says no on the vendor's page, or
// the vendor refuses the request (a scope the app does not have), the
// vendor sends the browser back with its error. The page must say so
// in the vendor's own words, cut short, or in its error code when it
// gave none; the owner's audit keeps it; nothing is exchanged.
func TestBrowserSignInPassesOnTheVendorsWords(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct{ code, description, want string }{
		{"access_denied", "The user did not approve the app.", "The user did not approve the app."},
		{"invalid_scope", long, long[:signInWordsRunes]},
		{"access_denied", "", "the service refused the sign-in (access_denied)"},
	} {
		srv, vendor := browserServer(t)
		vendor.Set(func(k *connecttest.Knobs) { k.AuthorizeError, k.AuthorizeErrorDescription = tc.code, tc.description })
		authorize, cookie := startBrowser(t, srv)
		to := callback(t, srv, vendorApprove(t, authorize), cookie)
		if words := failedWords(t, srv, to); words != tc.want {
			t.Errorf("%s: the page reads %q", tc.code, words)
		}
		if n := tokenRequests(vendor); n != 0 {
			t.Errorf("%s: %d exchanges", tc.code, n)
		}
		assertNotSignedIn(t, srv)
		if got := setupAudit(t, srv, "crm"); got[len(got)-1] != "refused failed: "+tc.want {
			t.Errorf("%s: audit = %q", tc.code, got)
		}
	}
}

// Why this test exists: a vendor's error words may echo what it was
// sent, the state among them, and they go to the page, the audit and
// the flow. The state (with the code, the verifier and the client
// secret) must be scrubbed from them whole before they are cut, or a
// piece of a secret would be shown.
func TestBrowserSignInScrubsTheVendorsWords(t *testing.T) {
	srv, vendor := browserServer(t)
	authorize, cookie := startBrowser(t, srv)
	state := authorize.Query().Get("state")
	vendor.Set(func(k *connecttest.Knobs) {
		k.AuthorizeError = "invalid_request"
		k.AuthorizeErrorDescription = strings.Repeat("y", signInWordsRunes-10) + " state " + state + " and " + connecttest.ClientSecret
	})
	to := callback(t, srv, vendorApprove(t, authorize), cookie)
	words := failedWords(t, srv, to)
	if strings.Contains(words, state[:10]) || strings.Contains(words, connecttest.ClientSecret[:10]) || len([]rune(words)) > signInWordsRunes {
		t.Errorf("the page was sent %q", words)
	}
	for _, row := range setupAudit(t, srv, "crm") {
		if strings.Contains(row, state[:10]) {
			t.Errorf("audit = %s", row)
		}
	}
}

// Why this test exists: PKCE is what keeps a code someone intercepted
// from being redeemed without the verifier only Kivali holds. When the
// verifier Kivali sends does not match the challenge (here, changed in
// the flow), the vendor refuses the exchange, and the page must say the
// vendor refused, in its words, with nothing kept.
func TestBrowserSignInPKCEMismatch(t *testing.T) {
	srv, vendor := browserServer(t)
	authorize, cookie := startBrowser(t, srv)
	back := vendorApprove(t, authorize)
	srv.setupRuns.mu.Lock()
	srv.setupRuns.currentLocked("crm").spec.driver.(*signInSetup).verifier = connect.NewPKCEVerifier()
	srv.setupRuns.mu.Unlock()

	to := callback(t, srv, back, cookie)
	want := "the vendor refused the sign-in (invalid_grant): the PKCE verifier does not match"
	if words := failedWords(t, srv, to); words != want {
		t.Errorf("the page reads %q", words)
	}
	if n := tokenRequests(vendor); n != 1 {
		t.Errorf("%d exchanges, want 1", n)
	}
	assertNotSignedIn(t, srv)
}

// Why this test exists: a manifest with pkce false is a vendor that
// rejects a challenge it does not know; the vendor's page must carry
// none, and the exchange no verifier.
func TestBrowserSignInWithoutPKCE(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	off := false
	browserBundle(t, srv, vendor, func(c *integration.Connection) { c.PKCE = &off })
	saveSignInClient(t, srv)
	authorize, cookie := startBrowser(t, srv)
	if q := authorize.Query(); q.Has("code_challenge") || q.Has("code_challenge_method") {
		t.Errorf("the vendor's page = %s", authorize)
	}
	signedInThere(t, srv, callback(t, srv, vendorApprove(t, authorize), cookie))
	for _, r := range vendor.Requests() {
		if r.Path == "/token" && r.Form.Has("code_verifier") {
			t.Errorf("the exchange sent a verifier: %v", r.Form)
		}
	}
}

// Why this test exists: an OAuth return carries the state the vendor
// was given. A callback matched by the cookie alone, with no state (a
// vendor that dropped it, or a link someone made), is no sign-in's
// return: the runner ends the flow, as for any Go step that did not say
// its service drops the state, without an exchange.
func TestBrowserSignInNeedsTheState(t *testing.T) {
	srv, vendor := browserServer(t)
	_, cookie := startBrowser(t, srv)
	to := callback(t, srv, &url.URL{Path: "/connect/callback", RawQuery: "code=abc"}, cookie)
	if words := failedWords(t, srv, to); words != errSetupNoState {
		t.Errorf("the page reads %q", words)
	}
	if n := tokenRequests(vendor); n != 0 {
		t.Errorf("%d exchanges", n)
	}
}

// Why this test exists: the redirect address is the one thing the
// owner must register with the vendor exactly. Behind a reverse proxy
// (Tailscale serve, a TLS terminator) the request Kivali sees is not
// the address the browser used, so it must be read from
// X-Forwarded-Proto and X-Forwarded-Host when they are set (a TLS
// connection is https whatever the header says), the first value when
// a proxy appended, without a default port, and fall back to
// the request's own when a header is not a host. A wrong address here
// is a vendor that refuses every sign-in.
func TestSignInRedirectURL(t *testing.T) {
	for _, tc := range []struct {
		name, host, proto, fwdHost string
		tls                        bool
		want                       string
	}{
		{"plain", "127.0.0.1:8080", "", "", false, "http://127.0.0.1:8080/connect/callback"},
		{"tls", "kivali.example.com", "", "", true, "https://kivali.example.com/connect/callback"},
		{"proxy", "10.0.0.5:8080", "https", "team.tail1234.ts.net", false, "https://team.tail1234.ts.net/connect/callback"},
		{"proxy appended", "10.0.0.5:8080", "https, http", "Team.Example.com:443, inner:8080", false, "https://team.example.com/connect/callback"},
		{"default http port", "kivali.local:80", "", "", false, "http://kivali.local/connect/callback"},
		{"ipv6 loopback", "[::1]:8080", "", "", false, "http://[::1]:8080/connect/callback"},
		{"junk proto", "127.0.0.1:8080", "gopher", "", false, "http://127.0.0.1:8080/connect/callback"},
		{"tls wins over the header", "kivali.example.com", "http", "", true, "https://kivali.example.com/connect/callback"},
		{"junk host", "127.0.0.1:8080", "", "evil.example/x?y", false, "http://127.0.0.1:8080/connect/callback"},
		{"no host", "", "", "", false, ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = tc.host
		if tc.proto != "" {
			r.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		if tc.fwdHost != "" {
			r.Header.Set("X-Forwarded-Host", tc.fwdHost)
		}
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := signInRedirectURL(r); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Why this test exists: the owner copies the redirect address from the
// page into the vendor's app before there is a client id to save, so
// the page must carry it with no connection yet, and on each
// connection's sign-in, from the address the owner's browser used
// (here through a proxy), and the setup notes must name it where they
// say {{redirect_url}}, on the install screen too. The sign-in must
// then give the vendor that same address, and mark its cookie Secure on
// https. A device-only manifest carries no address: it has nothing to
// register.
func TestBrowserRedirectAddressOnThePage(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	srv.ExternalURL = "https://team.tail1234.ts.net"
	browserBundle(t, srv, vendor, nil)
	const want = "https://team.tail1234.ts.net/connect/callback"
	viaProxy := func(method, path string, body []byte) *http.Request {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "team.tail1234.ts.net")
		return r
	}
	page := orgOK[apitypes.IntegrationPage](t, serveSignedIn(t, srv, viaProxy(http.MethodGet, intBase+"/crm", nil)))
	if page.SignInRedirectURL == nil || *page.SignInRedirectURL != want || page.Connection != nil {
		t.Errorf("before any connection: redirect %v, connection %+v", page.SignInRedirectURL, page.Connection)
	}
	if page.SetupNotesMD != strings.ReplaceAll(browserSetupNotes, "{{redirect_url}}", want) {
		t.Errorf("setup notes = %q", page.SetupNotesMD)
	}
	v := orgOK[apitypes.IntegrationVersion](t, serveSignedIn(t, srv, viaProxy(http.MethodGet, intBase+"/crm/versions/1.0.0", nil)))
	if !strings.Contains(v.SetupNotesMD, want) || strings.Contains(v.SetupNotesMD, "{{") {
		t.Errorf("install screen notes = %q", v.SetupNotesMD)
	}

	saveSignInClient(t, srv)
	page = orgOK[apitypes.IntegrationPage](t, serveSignedIn(t, srv, viaProxy(http.MethodGet, intBase+"/crm", nil)))
	if si := page.Connection.SignIn; si == nil || si.RedirectURL == nil || *si.RedirectURL != want || strings.Join(si.Ways, ",") != "browser" {
		t.Errorf("the connection's sign-in = %+v", si)
	}
	orgOK[apitypes.SetupFlow](t, serveSignedIn(t, srv, viaProxy(http.MethodPost, intBase+"/crm/setup", []byte(`{"flow":"setup"}`))))
	f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateGo)
	rr := serveSignedIn(t, srv, viaProxy(http.MethodGet, *f.Step.StartURL, nil))
	if u, _ := url.Parse(rr.Header().Get("Location")); u.Query().Get("redirect_uri") != want {
		t.Errorf("the vendor's page = %s", rr.Header().Get("Location"))
	}
	if c := cookieOf(rr, "kivali_connect"); c == nil || !c.Secure {
		t.Errorf("cookies = %v", rr.Result().Cookies())
	}

	device, deviceVendor, _ := signInServer(t)
	signInBundle(t, device, deviceVendor)
	saveSignInClient(t, device)
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, device, intBase+"/crm"))
	if page.SignInRedirectURL != nil || page.Connection.SignIn.RedirectURL != nil {
		t.Errorf("a device-only page carries a redirect address")
	}
}

// Why this test exists: a vendor sends a browser sign-in back only to an
// address registered with its app, so setup notes that pre-fill the
// app's callback addresses must name both the one the owner set it up
// from and the team's external address, or a sign-in from the other
// one ends on the vendor's error page. Opened at the external address,
// {{redirect_url}} already names it, and naming it twice could make the
// vendor refuse the form; a team with no external address names none.
func TestSetupNotesNameTheExternalRedirectAddress(t *testing.T) {
	m := &integration.Manifest{}
	m.Kivali.SetupNotes = "here={{redirect_url}} there={{external_redirect_url}}"
	loopback := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/v1/org/integrations/crm", nil)
	external := httptest.NewRequest(http.MethodGet, "/api/v1/org/integrations/crm", nil)
	external.Header.Set("X-Forwarded-Proto", "https")
	external.Header.Set("X-Forwarded-Host", "team.tail1234.ts.net")
	for _, tc := range []struct {
		name, externalURL string
		r                 *http.Request
		want              string
	}{
		{"opened on loopback", "https://Team.tail1234.ts.net:443", loopback,
			"here=http://127.0.0.1:8080/connect/callback there=https://team.tail1234.ts.net/connect/callback"},
		{"opened at the external address", "https://team.tail1234.ts.net", external,
			"here=https://team.tail1234.ts.net/connect/callback there="},
		{"an external address with its own port", "https://team.example.com:8443", loopback,
			"here=http://127.0.0.1:8080/connect/callback there=https://team.example.com:8443/connect/callback"},
		{"no external address", "", loopback,
			"here=http://127.0.0.1:8080/connect/callback there="},
	} {
		s := &Server{ExternalURL: tc.externalURL}
		if got := s.setupNotes(m, tc.r); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Why this test exists: a sign-in sends the owner's browser wherever
// the manifest's authorization endpoint points, and a bundle written
// under a prompt injection could point it at a lookalike of the
// service's sign-in page. The install screen and the page must name
// every host the sign-in talks to (each once, sorted), and the page the
// host each way sends the owner to, so the owner can tell before
// approving and before signing in. A token connection has none.
func TestSignInHostsAreShown(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	browserBundle(t, srv, vendor, func(c *integration.Connection) {
		c.Method = integration.MethodDevice
		c.AuthorizationEndpoint = "https://App.HubSpot.com/oauth/authorize"
		c.TokenEndpoint = "https://api.hubapi.com/oauth/v1/token"
		c.DeviceEndpoint = "https://login.example.com/device"
		c.RevocationEndpoint = "https://api.hubapi.com/oauth/v1/revoke"
	})
	want := "api.hubapi.com,app.hubspot.com,login.example.com"
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if strings.Join(page.SignInHosts, ",") != want {
		t.Errorf("page sign-in hosts = %v", page.SignInHosts)
	}
	if page.SignInAt["browser"] != "app.hubspot.com" || page.SignInAt["device"] != "login.example.com" || len(page.SignInAt) != 2 {
		t.Errorf("page signs in at %v", page.SignInAt)
	}
	v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.0.0"))
	if strings.Join(v.SignInHosts, ",") != want {
		t.Errorf("install screen sign-in hosts = %v", v.SignInHosts)
	}

	intConnected(t, srv, "hubspot")
	token := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/hubspot"))
	if token.SignInHosts == nil || len(token.SignInHosts) != 0 || token.SignInAt != nil {
		t.Errorf("a token connection's page: hosts %v, at %v", token.SignInHosts, token.SignInAt)
	}
}

// Why this test exists: a manifest that offers both ways lists them on
// the page with its method first, and the flow takes the first, as the
// page says it will.
func TestSignInWaysBoth(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	browserBundle(t, srv, vendor, func(c *integration.Connection) {
		c.Method, c.DeviceEndpoint = integration.MethodDevice, vendor.Endpoints().Device
	})
	saveSignInClient(t, srv)
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, loopbackOrigin+intBase+"/crm"))
	if si := page.Connection.SignIn; si == nil || strings.Join(si.Ways, ",") != "device,browser" || si.RedirectURL == nil {
		t.Errorf("sign-in = %+v", si)
	}
	if f := startDeviceSignIn(t, srv); textOf(f.Step.Code) == "" {
		t.Errorf("the flow's step = %+v", f.Step)
	}
}

// Why this test exists: a browser sign-in finishes only where the
// vendor's app knows the address it sends the browser back to and a
// browser signed in to Kivali is there to take it: loopback (Kivali
// Desktop's window hands it to the system browser at the same address)
// and the team's external address in a browser. Offered anywhere else,
// it would end on the vendor's error page, or open nothing from Kivali
// Desktop's window for a team elsewhere. There the page must offer the
// code instead when the manifest has one, and the flow must take it;
// with browser alone, the page offers no way and says where to sign in,
// and the flow is refused.
func TestSignInWaysFollowWhereTheOwnerIs(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	srv.ExternalURL = "https://team.tail1234.ts.net"
	browserBundle(t, srv, vendor, func(c *integration.Connection) { c.DeviceEndpoint = vendor.Endpoints().Device })
	saveSignInClient(t, srv)
	req := func(method, origin string, desktop bool, fwdHost string) *http.Request {
		r := httptest.NewRequest(method, origin+intBase+"/crm", nil)
		if method == http.MethodPost {
			r = httptest.NewRequest(method, origin+intBase+"/crm/setup", bytes.NewReader([]byte(`{"flow":"setup"}`)))
			r.Header.Set("Content-Type", "application/json")
		}
		if fwdHost != "" {
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-Host", fwdHost)
		}
		if desktop {
			r.AddCookie(&http.Cookie{Name: desktopWindowCookie, Value: "1"})
		}
		return r
	}
	for _, tc := range []struct {
		name     string
		origin   string
		desktop  bool
		fwdHost  string
		ways     string
		redirect string
	}{
		{"a browser on loopback", loopbackOrigin, false, "", "browser,device", loopbackOrigin + "/connect/callback"},
		{"Kivali Desktop's window on loopback", loopbackOrigin, true, "", "browser,device", loopbackOrigin + "/connect/callback"},
		{"a browser at the external address", "http://10.0.0.5:8080", false, "team.tail1234.ts.net", "browser,device", "https://team.tail1234.ts.net/connect/callback"},
		{"Kivali Desktop's window for a team elsewhere", "http://10.0.0.5:8080", true, "team.tail1234.ts.net", "device", ""},
		{"a plain http address on the network", "http://192.168.1.5:8080", false, "", "device", ""},
		{"another https name for the team", "http://10.0.0.5:8080", false, "kivali.other.example", "device", ""},
	} {
		page := orgOK[apitypes.IntegrationPage](t, serveSignedIn(t, srv, req(http.MethodGet, tc.origin, tc.desktop, tc.fwdHost)))
		si := page.Connection.SignIn
		if got := strings.Join(si.Ways, ","); got != tc.ways || si.BrowserElsewhere || si.OpenAt != nil {
			t.Errorf("%s: ways %q (want %q), elsewhere %v, open at %v", tc.name, got, tc.ways, si.BrowserElsewhere, si.OpenAt)
		}
		if got := textOf(si.RedirectURL); got != tc.redirect || textOf(page.SignInRedirectURL) != tc.redirect {
			t.Errorf("%s: redirect %q, page %q, want %q", tc.name, got, textOf(page.SignInRedirectURL), tc.redirect)
		}
		orgOK[apitypes.SetupFlow](t, serveSignedIn(t, srv, req(http.MethodPost, tc.origin, tc.desktop, tc.fwdHost)))
		want := apitypes.SetupFlowStateGo
		if !strings.HasPrefix(tc.ways, "browser") {
			want = apitypes.SetupFlowStateWait
		}
		waitFlow(t, srv, "crm", want)
		srv.setupCancel("crm")
	}

	only, onlyVendor, _ := signInServer(t)
	browserBundle(t, only, onlyVendor, nil)
	saveSignInClient(t, only)
	for _, ext := range []string{"https://team.tail1234.ts.net", ""} {
		only.ExternalURL = ext
		page := orgOK[apitypes.IntegrationPage](t, serveSignedIn(t, only, req(http.MethodGet, "http://192.168.1.5:8080", false, "")))
		si := page.Connection.SignIn
		if len(si.Ways) != 0 || !si.BrowserElsewhere || textOf(si.OpenAt) != ext {
			t.Errorf("browser alone, external %q: ways %v, elsewhere %v, open at %v", ext, si.Ways, si.BrowserElsewhere, si.OpenAt)
		}
	}
	if e := assertAPIError(t, serveSignedIn(t, only, req(http.MethodPost, "http://192.168.1.5:8080", false, "")), http.StatusConflict); e.Error != errSignInNotHere {
		t.Errorf("browser alone from elsewhere: %+v", e)
	}
	if si := orgOK[apitypes.IntegrationPage](t, serveSignedIn(t, only, req(http.MethodGet, loopbackOrigin, true, ""))).Connection.SignIn; strings.Join(si.Ways, ",") != "browser" || si.BrowserElsewhere {
		t.Errorf("browser alone on loopback: %+v", si)
	}
}

// textOf is *s, or "" for nil.
func textOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Why this test exists: the callback is part of the signed-in UI. A
// browser not signed in to Kivali (the vendor's redirect landed in a
// fresh window, or the session ran out while the owner approved) must
// be sent to sign in with the whole callback address, code and state
// included, as where to come back to; a next without the query would
// return the owner to a callback with nothing to finish.
func TestConnectCallbackSignsInFirstAndComesBack(t *testing.T) {
	srv := newTestServer(t)
	const path = "/connect/callback?code=abc&state=xyz"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	loc, err := url.Parse(rr.Header().Get("Location"))
	if rr.Code != http.StatusFound || err != nil || loc.Path != "/login" || loc.Query().Get("next") != path {
		t.Errorf("an unsigned callback answered %d to %q", rr.Code, rr.Header().Get("Location"))
	}
	if _, pattern := srv.protectedMux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern != "GET /connect/callback" {
		t.Errorf("the callback is served by %q", pattern)
	}
}

// Why this test exists: a callback whose state Kivali knows nothing of,
// with no cookie to say whose it was (Kivali restarted, the address was
// pasted), still lands somewhere that says what happened: the
// integrations list, in words.
func TestConnectCallbackUnknownState(t *testing.T) {
	srv := newTestServer(t)
	startAgain(t, callback(t, srv, &url.URL{Path: "/connect/callback", RawQuery: "code=abc&state=nothing-like-it"}), "/org/integrations")
}
