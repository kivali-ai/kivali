package web

import (
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

// startBrowser starts crm's browser sign-in from a page at example.com
// and returns the vendor's sign-in page and the cookie the answer set.
func startBrowser(t *testing.T, srv *Server) (*url.URL, *http.Cookie) {
	t.Helper()
	rr := serveSignedIn(t, srv, httptest.NewRequest(http.MethodPost, intBase+"/crm/connection/sign-in/browser", nil))
	bs := orgOK[apitypes.BrowserSignIn](t, rr)
	u, err := url.Parse(bs.URL)
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "kivali_connect" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no sign-in cookie in %v", rr.Result().Cookies())
	}
	return u, cookie
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

// failedWords is why the sign-in a callback sent the browser to as
// failed (?sign_in_failed=<id>) did not work, as the page reads it:
// crm's connection's GET .../sign-in.
func failedWords(t *testing.T, srv *Server, to *url.URL) string {
	t.Helper()
	rec, _ := firstConnectionSecret(t, srv)
	q := to.Query()
	if to.Path != "/org/integrations/crm" || q.Get("sign_in_failed") != rec.ID || len(q) != 1 {
		t.Fatalf("landed on %s, want crm's page with sign_in_failed", to)
	}
	p := orgOK[apitypes.SignInProgress](t, orgGet(t, srv, intBase+"/crm/connections/"+rec.ID+"/sign-in"))
	if p.State != apitypes.SignInStateFailed || p.Error == nil {
		t.Fatalf("the failed sign-in reads %+v", p)
	}
	return *p.Error
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
// Sign in in your browser, the vendor's page, the vendor sending the
// browser back to /connect/callback, and the integration's page. The
// vendor's page must carry the client id, the redirect address, the
// scopes, the manifest's extra parameters, a random state and a PKCE
// challenge (S256), or the vendor refuses or the code can be redeemed
// by whoever intercepts it. The cookie that binds the browser must be
// HttpOnly, SameSite=Lax (the vendor's redirect is a cross-site
// navigation) and host-only. Kivali must exchange the code with the
// verifier and the client secret, keep the tokens and the sign-in as
// browser, restart the bundle's server, run the connection test, audit
// the sign-in, drop the cookie and land the browser on the page with
// the connection it signed in. A step missing here is a sign-in that
// fails for every owner of a vendor like HubSpot.
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

	req, _ := http.NewRequest(http.MethodPost, ts.URL+intBase+"/crm/connection/sign-in/browser", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var bs apitypes.BrowserSignIn
	if err := json.NewDecoder(resp.Body).Decode(&bs); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("start: %d %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close()
	setCookie := resp.Header.Get("Set-Cookie")
	if !strings.HasPrefix(setCookie, "kivali_connect=") || !strings.Contains(setCookie, "Path=/;") ||
		!strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Lax") || strings.Contains(setCookie, "Domain=") {
		t.Errorf("Set-Cookie = %q", setCookie)
	}
	authorize, _ := url.Parse(bs.URL)
	q := authorize.Query()
	if authorize.Path != "/authorize" || q.Get("client_id") != connecttest.ClientID || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != ts.URL+"/connect/callback" || q.Get("scope") != "content" ||
		q.Get("optional_scope") != "crm.objects.contacts.read" || q.Get("code_challenge_method") != "S256" ||
		len(q.Get("code_challenge")) != 43 || len(q.Get("state")) != 43 || q.Has("client_secret") {
		t.Errorf("the vendor's page = %s", bs.URL)
	}

	signedInAt := clk.Now()
	resp, err = browser.Get(bs.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || landed == nil || landed.Path != "/org/integrations/crm" {
		t.Fatalf("landed on %v after %d", landed, resp.StatusCode)
	}
	rec, data := firstConnectionSecret(t, srv)
	if got := landed.Query(); got.Get("signed_in") != rec.ID || len(got) != 1 {
		t.Errorf("landed on %s", landed)
	}
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
	rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5)
	if last := rows[len(rows)-1]; last.Action != "sign_in" || last.Agent != agent.CEOSlug || !last.Allowed {
		t.Errorf("audit = %+v", rows)
	}
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateDone {
		t.Errorf("progress = %+v", p)
	}
	cred, rpcErr := srv.Integrations.credential(context.Background(), "crm")
	if rpcErr != nil || cred.Env["HUBSPOT_ACCESS_TOKEN"] != data[connect.SecretKeyAccessToken] {
		t.Errorf("the credential = %+v, %+v", cred, rpcErr)
	}
}

// Why this test exists: a browser holds one sign-in cookie, so of two
// browser sign-ins started in it (two integrations' pages, two tabs)
// only the later can finish. The earlier must end at once, and its
// callback must land on its own page with words that say another
// sign-in replaced it, without taking the later's cookie with it: the
// later sign-in must still finish. Without this both would fail, the
// first for its cookie and the second because the first callback
// dropped it.
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
	start := func(name string) string {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+intBase+"/"+name+"/connection/sign-in/browser", nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var bs apitypes.BrowserSignIn
		if err := json.NewDecoder(resp.Body).Decode(&bs); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("start %s: %d %v", name, resp.StatusCode, err)
		}
		return bs.URL
	}
	approveIn := func(authorize string) *url.URL {
		landed = nil
		resp, err := browser.Get(authorize)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return landed
	}
	first, second := start("crm"), start("crm2")

	to := approveIn(first)
	rec, _ := firstConnectionSecret(t, srv)
	if to == nil || to.Path != "/org/integrations/crm" || to.Query().Get("sign_in_failed") != rec.ID {
		t.Fatalf("the first sign-in landed on %v", to)
	}
	p := orgOK[apitypes.SignInProgress](t, orgGet(t, srv, intBase+"/crm/connection/sign-in"))
	if p.State != apitypes.SignInStateFailed || p.Error == nil || *p.Error != errSignInReplaced {
		t.Errorf("the first sign-in reads %+v", p)
	}
	if n := tokenRequests(vendor); n != 0 {
		t.Errorf("%d exchanges for the replaced sign-in", n)
	}

	if to := approveIn(second); to == nil || to.Path != "/org/integrations/crm2" || to.Query().Get("signed_in") == "" {
		t.Errorf("the second sign-in landed on %v", to)
	}
}

// Why this test exists: the state is good for one callback. A callback
// replayed (the browser's back button, a copied address, an attacker
// who saw it) must not run the exchange again, nor overwrite the
// sign-in, even when it carries the cookie: it lands on the page saying
// to start again.
func TestBrowserSignInStateIsSingleUse(t *testing.T) {
	srv, vendor := browserServer(t)
	authorize, cookie := startBrowser(t, srv)
	back := vendorApprove(t, authorize)
	if to := callback(t, srv, back, cookie); to.Query().Get("signed_in") == "" {
		t.Fatalf("the first callback landed on %s", to)
	}
	rec, data := firstConnectionSecret(t, srv)

	startAgain(t, callback(t, srv, back, cookie), "/org/integrations/crm")
	if n := tokenRequests(vendor); n != 1 {
		t.Errorf("%d exchanges, want 1", n)
	}
	again, againData := firstConnectionSecret(t, srv)
	if !again.SignIn.SignedInAt.Equal(rec.SignIn.SignedInAt) || againData[connect.SecretKeyAccessToken] != data[connect.SecretKeyAccessToken] {
		t.Errorf("the replay changed the sign-in")
	}
}

// Why this test exists: a sign-in lasts ten minutes on Kivali's clock.
// A vendor's redirect that comes later (an owner who left the vendor's
// page open) must be refused in words that say to start again, with
// no exchange: the flow's verifier is gone with it. The cookie still
// names the integration, so the browser lands on its page.
func TestBrowserSignInExpires(t *testing.T) {
	srv, vendor := browserServer(t)
	clk := srv.Clock.(interface{ Advance(time.Duration) })
	authorize, cookie := startBrowser(t, srv)
	back := vendorApprove(t, authorize)
	clk.Advance(signInFlowTTL)

	startAgain(t, callback(t, srv, back, cookie), "/org/integrations/crm")
	if n := tokenRequests(vendor); n != 0 {
		t.Errorf("%d exchanges after the sign-in expired", n)
	}
	assertNotSignedIn(t, srv)
}

// Why this test exists: the cookie binds the sign-in to the browser
// that started it. A callback without it (another browser, or a link an
// attacker sends the owner to sign the owner's connection in to the
// attacker's account) must be refused as started elsewhere, with no
// exchange, and the state must be spent so the right browser cannot be
// raced either. A cookie Kivali did not sign is no cookie.
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
			// The right browser after a refused callback finds the state spent.
			startAgain(t, callback(t, srv, back, cookie), "/org/integrations/crm")
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
// in the vendor's own words, cut short so a vendor cannot fill the
// address, or in its error code when it gave none; the owner's audit
// keeps it; nothing is exchanged.
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
		rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5)
		if last := rows[len(rows)-1]; last.Action != "sign_in" || last.Allowed || last.Reason != tc.want {
			t.Errorf("%s: audit = %+v", tc.code, rows)
		}
	}
}

// Why this test exists: a vendor's error words may echo what it was
// sent, the state among them, and they go into the page's address, the
// audit and the flow. The state (with the code, the verifier and the
// client secret) must be scrubbed from them whole before they are cut,
// or a replayable state, or a piece of a secret, would sit in the
// browser's history.
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
	rows, _ := srv.Store.ReadIntegrationAuditTail("crm", 5)
	if last := rows[len(rows)-1]; strings.Contains(last.Reason, state[:10]) {
		t.Errorf("audit = %+v", last)
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
	srv.signInFlows.mu.Lock()
	for _, f := range srv.signInFlows.flows {
		f.browser.verifier = connect.NewPKCEVerifier()
	}
	srv.signInFlows.mu.Unlock()

	to := callback(t, srv, back, cookie)
	want := "the vendor refused the sign-in (invalid_grant): the PKCE verifier does not match"
	if words := failedWords(t, srv, to); words != want {
		t.Errorf("the page reads %q", words)
	}
	if n := tokenRequests(vendor); n != 1 {
		t.Errorf("%d exchanges, want 1", n)
	}
	assertNotSignedIn(t, srv)
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateFailed || p.Error == nil || *p.Error != want {
		t.Errorf("progress = %+v", p)
	}
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
	if to := callback(t, srv, vendorApprove(t, authorize), cookie); to.Query().Get("signed_in") == "" {
		t.Errorf("landed on %s", to)
	}
	for _, r := range vendor.Requests() {
		if r.Path == "/token" && r.Form.Has("code_verifier") {
			t.Errorf("the exchange sent a verifier: %v", r.Form)
		}
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
	browserBundle(t, srv, vendor, nil)
	const want = "https://team.tail1234.ts.net/connect/callback"
	viaProxy := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "team.tail1234.ts.net")
		return r
	}
	page := orgOK[apitypes.IntegrationPage](t, serveSignedIn(t, srv, viaProxy(http.MethodGet, intBase+"/crm")))
	if page.SignInRedirectURL == nil || *page.SignInRedirectURL != want || page.Connection != nil {
		t.Errorf("before any connection: redirect %v, connection %+v", page.SignInRedirectURL, page.Connection)
	}
	if page.SetupNotesMD != strings.ReplaceAll(browserSetupNotes, "{{redirect_url}}", want) {
		t.Errorf("setup notes = %q", page.SetupNotesMD)
	}
	v := orgOK[apitypes.IntegrationVersion](t, serveSignedIn(t, srv, viaProxy(http.MethodGet, intBase+"/crm/versions/1.0.0")))
	if !strings.Contains(v.SetupNotesMD, want) || strings.Contains(v.SetupNotesMD, "{{") {
		t.Errorf("install screen notes = %q", v.SetupNotesMD)
	}

	saveSignInClient(t, srv)
	page = orgOK[apitypes.IntegrationPage](t, serveSignedIn(t, srv, viaProxy(http.MethodGet, intBase+"/crm")))
	if si := page.Connection.SignIn; si == nil || si.RedirectURL == nil || *si.RedirectURL != want || strings.Join(si.Ways, ",") != "browser" {
		t.Errorf("the connection's sign-in = %+v", si)
	}
	rr := serveSignedIn(t, srv, viaProxy(http.MethodPost, intBase+"/crm/connection/sign-in/browser"))
	bs := orgOK[apitypes.BrowserSignIn](t, rr)
	if u, _ := url.Parse(bs.URL); u.Query().Get("redirect_uri") != want {
		t.Errorf("the vendor's page = %s", bs.URL)
	}
	if c := rr.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Errorf("cookies = %v", c)
	}

	device, deviceVendor, _ := signInServer(t)
	signInBundle(t, device, deviceVendor)
	saveSignInClient(t, device)
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, device, intBase+"/crm"))
	if page.SignInRedirectURL != nil || page.Connection.SignIn.RedirectURL != nil {
		t.Errorf("a device-only page carries a redirect address")
	}
	if rr := intPost(t, device, intBase+"/crm/connection/sign-in/browser"); rr.Code != http.StatusConflict {
		t.Errorf("browser sign-in of a device-only connection: %d", rr.Code)
	}
}

// Why this test exists: Sign in in your browser sends the owner's
// browser wherever the manifest's authorization endpoint points, and a
// bundle written under a prompt injection could point it at a lookalike
// of the service's sign-in page. The install screen and the page must
// name every host the sign-in talks to (each once, sorted), and the
// page the host each way sends the owner to, so the owner can tell
// before approving and before signing in. A token connection has none.
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
// the page with its method first, and each way's route works for it.
func TestSignInWaysBoth(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	browserBundle(t, srv, vendor, func(c *integration.Connection) {
		c.Method, c.DeviceEndpoint = integration.MethodDevice, vendor.Endpoints().Device
	})
	saveSignInClient(t, srv)
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if si := page.Connection.SignIn; si == nil || strings.Join(si.Ways, ",") != "device,browser" || si.RedirectURL == nil {
		t.Errorf("sign-in = %+v", si)
	}
	authorize, cookie := startBrowser(t, srv)
	if to := callback(t, srv, vendorApprove(t, authorize), cookie); to.Query().Get("signed_in") == "" {
		t.Errorf("landed on %s", to)
	}
}

// Why this test exists: the browser sign-in route refuses as the
// device one does, before any flow or cookie: the client id and
// secret not saved, a connection already signed in, the integration
// switched off. A sign-in started from any of these would send the
// owner to the vendor for nothing.
func TestBrowserSignInRefusals(t *testing.T) {
	srv, vendor, _ := signInServer(t)
	browserBundle(t, srv, vendor, nil)
	start := func() *httptest.ResponseRecorder {
		return serveSignedIn(t, srv, httptest.NewRequest(http.MethodPost, intBase+"/crm/connection/sign-in/browser", nil))
	}
	if rr := start(); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), errSignInClientFirst) {
		t.Errorf("no connection: %d %s", rr.Code, rr.Body.String())
	}
	saveSignInClient(t, srv)
	authorize, cookie := startBrowser(t, srv)
	callback(t, srv, vendorApprove(t, authorize), cookie)
	if rr := start(); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), errSignInSignedIn) || len(rr.Result().Cookies()) != 0 {
		t.Errorf("signed in: %d %s", rr.Code, rr.Body.String())
	}
	if rr := intPost(t, srv, intBase+"/crm/disable"); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d", rr.Code)
	}
	if rr := intPost(t, srv, intBase+"/crm/connection/sign-out"); rr.Code != http.StatusOK {
		t.Fatalf("sign out: %d", rr.Code)
	}
	if rr := start(); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "switch") {
		t.Errorf("switched off: %d %s", rr.Code, rr.Body.String())
	}
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
