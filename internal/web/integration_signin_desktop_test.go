package web

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/connect/connecttest"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// desktopServer is browserServer with the Desktop handoff served, on
// the server's clock, behind a real listener on 127.0.0.1 (the address
// of a team on this computer), and the handoff's next resolved as the
// server wires it.
func desktopServer(t *testing.T) (*Server, *connecttest.FakeVendor, *httptest.Server) {
	t.Helper()
	srv, vendor := browserServer(t)
	srv.Handoff = &auth.Handoff{Codec: srv.AuthMW.Codec, Allowlist: auth.NewAllowlist(testOwnerEmail), Now: srv.clk().Now}
	srv.wireRoutes()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, vendor, ts
}

// handoffToken is a handoff token for the owner, as the team's
// supervisor mints one for Kivali Desktop.
func handoffToken(t *testing.T, srv *Server) string {
	t.Helper()
	tok, err := auth.NewHandoffToken([]byte(testSessionKey), testOwnerEmail, srv.clk().Now().Add(auth.HandoffTTL))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// browserJar is a browser with its own cookies that follows redirects
// until it reaches a page of the web app (/org/... or /), whose address
// it returns in landed.
func browserJar(t *testing.T) (*http.Client, *cookiejar.Jar, **url.URL) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	landed := new(*url.URL)
	c := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(req.URL.Path, "/org/") || req.URL.Path == "/" {
			*landed = req.URL
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return c, jar, landed
}

// startInWebview starts crm's browser sign-in from the Desktop team
// window: a browser signed in to Kivali, with cookies of its own.
func startInWebview(t *testing.T, srv *Server, ts *httptest.Server) (apitypes.BrowserSignIn, *cookiejar.Jar) {
	t.Helper()
	webview, jar, _ := browserJar(t)
	base, _ := url.Parse(ts.URL)
	jar.SetCookies(base, []*http.Cookie{authCookie(t, srv)})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+intBase+"/crm/connection/sign-in/browser", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := webview.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var bs apitypes.BrowserSignIn
	if err := json.NewDecoder(resp.Body).Decode(&bs); err != nil || resp.StatusCode != http.StatusOK || bs.Flow == "" || bs.URL == "" {
		t.Fatalf("start: %d %v %+v", resp.StatusCode, err, bs)
	}
	return bs, jar
}

// Why this test exists: in Kivali Desktop the owner presses Sign in in
// your browser in the team's window, a webview, and the sign-in runs in
// the system browser, which holds neither the Kivali session nor the
// sign-in cookie the start answer set in the webview. Desktop opens
// /auth/handoff?t=<token>&next=<flow> there: it must sign that browser
// in and give it the sign-in's cookie before sending it to the vendor,
// or the callback, which comes to the system browser, would ask the
// owner to sign in to Kivali again and then refuse the sign-in as
// started elsewhere. Two cookie jars stand for the two browsers; the
// system browser starts with none and must end signed in, on the
// integration's page, with the connection holding the tokens.
func TestDesktopBrowserSignInThroughTheHandoff(t *testing.T) {
	srv, _, ts := desktopServer(t)
	bs, webviewJar := startInWebview(t, srv, ts)
	base, _ := url.Parse(ts.URL)
	if !hasCookie(webviewJar, base, connectCallbackPath, "kivali_connect") {
		t.Fatal("the start answer set no sign-in cookie in the webview")
	}

	system, systemJar, landed := browserJar(t)
	resp, err := system.Get(ts.URL + auth.HandoffPath + "?t=" + url.QueryEscape(handoffToken(t, srv)) + "&next=" + bs.Flow)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if *landed == nil || (*landed).Path != "/org/integrations/crm" {
		t.Fatalf("the system browser landed on %v (%d)", *landed, resp.StatusCode)
	}
	rec, data := firstConnectionSecret(t, srv)
	if q := (*landed).Query(); q.Get("signed_in") != rec.ID || q.Has("sign_in_error") {
		t.Errorf("landed on %s", *landed)
	}
	if rec.SignIn == nil || rec.SignIn.Method != "browser" || data[connect.SecretKeyAccessToken] == "" {
		t.Errorf("not signed in: %+v %v", rec.SignIn, data)
	}
	if !hasCookie(systemJar, base, "/", auth.CookieName) {
		t.Error("the system browser was not signed in to Kivali")
	}
	if hasCookie(systemJar, base, connectCallbackPath, "kivali_connect") {
		t.Error("the sign-in cookie outlived the callback in the system browser")
	}
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateDone {
		t.Errorf("the team window reads %+v", p)
	}
}

// Why this test exists: the handoff hop is what makes the system
// browser pass the cookie check, and nothing else may. A system
// browser signed in to Kivali (by a handoff without next, or by hand)
// that goes to the vendor's page with the sign-in's address must still
// be refused as started elsewhere: the cookie still binds the sign-in
// to a browser Kivali or Desktop gave it to.
func TestDesktopBrowserSignInNeedsTheHandoffsCookie(t *testing.T) {
	srv, _, ts := desktopServer(t)
	bs, _ := startInWebview(t, srv, ts)

	system, _, landed := browserJar(t)
	resp, err := system.Get(ts.URL + auth.HandoffPath + "?t=" + url.QueryEscape(handoffToken(t, srv)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if *landed == nil || (*landed).Path != "/" {
		t.Fatalf("the handoff without next landed on %v", *landed)
	}
	*landed = nil
	resp, err = system.Get(bs.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	rec, _ := firstConnectionSecret(t, srv)
	if *landed == nil || (*landed).Query().Get("sign_in_failed") != rec.ID {
		t.Fatalf("landed on %v", *landed)
	}
	if p := signInProgress(t, srv); p.State != apitypes.SignInStateFailed || p.Error == nil || *p.Error != errSignInStartAgain {
		t.Errorf("the sign-in reads %+v", p)
	}
	assertNotSignedIn(t, srv)
}

// Why this test exists: a browser holds one sign-in cookie, and the
// handoff hands the system browser one. A sign-in the owner started in
// that browser itself, of another integration, can then no longer
// finish there: it must end at once, in words that say another sign-in
// replaced it, as a second start in one browser ends the first, rather
// than wait ten minutes for a callback that is refused. The Desktop
// sign-in must still finish.
func TestDesktopHandoffReplacesTheBrowsersSignIn(t *testing.T) {
	srv, vendor, ts := desktopServer(t)
	browserBundleNamed(t, srv, vendor, "crm2", nil)
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm2/connection", apitypes.ConnectionWrite{Fields: map[string]string{
		"token": "pasted-field-value", "client_id": connecttest.ClientID, "client_secret": connecttest.ClientSecret,
	}})
	if rr.Code != http.StatusOK {
		t.Fatalf("save crm2: %d %s", rr.Code, rr.Body.String())
	}
	base, _ := url.Parse(ts.URL)

	// crm2's sign-in, started in the system browser itself.
	system, systemJar, landed := browserJar(t)
	systemJar.SetCookies(base, []*http.Cookie{authCookie(t, srv)})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+intBase+"/crm2/connection/sign-in/browser", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := system.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("start crm2: %v %v", resp, err)
	}
	_ = resp.Body.Close()

	// crm's, from the Desktop window, through the handoff.
	bs, _ := startInWebview(t, srv, ts)
	resp, err = system.Get(ts.URL + auth.HandoffPath + "?t=" + url.QueryEscape(handoffToken(t, srv)) + "&next=" + bs.Flow)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if *landed == nil || (*landed).Path != "/org/integrations/crm" || (*landed).Query().Get("signed_in") == "" {
		t.Fatalf("the Desktop sign-in landed on %v", *landed)
	}
	p := orgOK[apitypes.SignInProgress](t, orgGet(t, srv, intBase+"/crm2/connection/sign-in"))
	if p.State != apitypes.SignInStateFailed || p.Error == nil || *p.Error != errSignInReplaced {
		t.Errorf("crm2's sign-in reads %+v", p)
	}
}

// Why this test exists: next is a flow id the server looks up, never
// an address, so the handoff cannot become an open redirect, and it
// stays what it was: loopback only and one use per token. A next that
// names no pending browser sign-in (an address, a path, an unknown id,
// one a callback already took, one that started at another address;
// one past its time, below) is refused as a bad token is: "/" with no
// session and no sign-in cookie, since Desktop names only a flow it
// just saw start and anything else is no errand of the app's. A
// replayed token signs nothing in and hands no cookie over; on a host
// that is not loopback the route does not exist.
func TestDesktopHandoffNextIsAPendingFlow(t *testing.T) {
	srv, _, ts := desktopServer(t)
	bs, _ := startInWebview(t, srv, ts)
	base, _ := url.Parse(ts.URL)
	host := base.Host

	for _, next := range []string{"https://evil.example/", "/org/integrations", "0000"} {
		assertHandoffRefused(t, serveHandoff(t, srv, host, handoffToken(t, srv), next), "next "+next)
	}
	// Started at 127.0.0.1:<port>: the same team reached as localhost
	// would hold the host-only cookie where the callback never looks.
	_, port, _ := strings.Cut(host, ":")
	assertHandoffRefused(t, serveHandoff(t, srv, "localhost:"+port, handoffToken(t, srv), bs.Flow), "another host")

	// One use per token: the replay signs nothing in, hands no cookie.
	tok := handoffToken(t, srv)
	first := serveHandoff(t, srv, host, tok, bs.Flow)
	if first.Code != http.StatusFound || first.Header().Get("Location") != bs.URL || cookieOf(first, "kivali_connect") == nil || cookieOf(first, auth.CookieName) == nil {
		t.Fatalf("first: %d to %q", first.Code, first.Header().Get("Location"))
	}
	assertHandoffRefused(t, serveHandoff(t, srv, host, tok, bs.Flow), "replay")
	// Not loopback: no such route.
	if rr := serveHandoff(t, srv, "kivali.example.com", handoffToken(t, srv), bs.Flow); rr.Code != http.StatusNotFound {
		t.Errorf("non-loopback: %d", rr.Code)
	}
	// Taken by its callback: no longer a flow to send a browser to.
	authorize, _ := url.Parse(bs.URL)
	if to := callback(t, srv, vendorApprove(t, authorize), cookieOf(first, "kivali_connect")); to.Query().Get("signed_in") == "" {
		t.Fatalf("the callback landed on %s", to)
	}
	assertHandoffRefused(t, serveHandoff(t, srv, host, handoffToken(t, srv), bs.Flow), "a taken flow")
}

// Why this test exists: a sign-in lasts ten minutes. A handoff for one
// that ran out (the owner's browser slow to open, the computer asleep)
// must not send the browser to the vendor, whose code Kivali could no
// longer exchange: it is refused, with no session.
func TestDesktopHandoffNextPastItsTime(t *testing.T) {
	srv, _, ts := desktopServer(t)
	bs, _ := startInWebview(t, srv, ts)
	base, _ := url.Parse(ts.URL)
	srv.Clock.(interface{ Advance(time.Duration) }).Advance(signInFlowTTL)
	assertHandoffRefused(t, serveHandoff(t, srv, base.Host, handoffToken(t, srv), bs.Flow), "an expired flow")
}

// assertHandoffRefused fails unless rr is a refused handoff: a 302 to
// "/" that sets neither a session nor a sign-in cookie.
func assertHandoffRefused(t *testing.T, rr *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/" || cookieOf(rr, auth.CookieName) != nil || cookieOf(rr, "kivali_connect") != nil {
		t.Errorf("%s: %d to %q with %v", what, rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
}

// Why this test exists: the web app begins every browser sign-in at
// GET /connect/start?flow=<id> (Kivali Desktop takes that address over
// in its team window). In a browser it must send the tab on to the
// vendor's page of that flow, the address Kivali made and nothing from
// the request, and set no cookie of its own; for a flow it does not
// know (a typo, a sign-in that ran out or was already used) it returns
// to the integrations list saying to start again, never to a vendor.
func TestConnectStartSendsTheBrowserToTheFlow(t *testing.T) {
	srv, _ := browserServer(t)
	rr := serveSignedIn(t, srv, httptest.NewRequest(http.MethodPost, intBase+"/crm/connection/sign-in/browser", nil))
	bs := orgOK[apitypes.BrowserSignIn](t, rr)

	rr = serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, connectStartPath+"?flow="+bs.Flow, nil))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != bs.URL || len(rr.Result().Cookies()) != 0 {
		t.Errorf("a pending flow: %d to %q, cookies %v", rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
	for _, q := range []string{"?flow=0000", "?flow=", "?url=https://evil.example/"} {
		rr = serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, connectStartPath+q, nil))
		to, _ := url.Parse(rr.Header().Get("Location"))
		if rr.Code != http.StatusSeeOther || to.Path != "/org/integrations" || to.Query().Get("sign_in_error") != connectStartAgain {
			t.Errorf("%s: %d to %q", q, rr.Code, rr.Header().Get("Location"))
		}
	}
	srv.Clock.(interface{ Advance(time.Duration) }).Advance(signInFlowTTL)
	rr = serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, connectStartPath+"?flow="+bs.Flow, nil))
	if to, _ := url.Parse(rr.Header().Get("Location")); to.Path != "/org/integrations" {
		t.Errorf("an expired flow: to %q", rr.Header().Get("Location"))
	}
}

// serveHandoff sends GET /auth/handoff?t=token&next=next to srv on host.
func serveHandoff(t *testing.T, srv *Server, host, token, next string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, auth.HandoffPath+"?t="+url.QueryEscape(token)+"&next="+url.QueryEscape(next), nil)
	req.Host = host
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

// cookieOf is the cookie rr sets named name, or nil.
func cookieOf(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// hasCookie reports whether jar sends a cookie named name to path on
// base's host.
func hasCookie(jar *cookiejar.Jar, base *url.URL, path, name string) bool {
	for _, c := range jar.Cookies(&url.URL{Scheme: base.Scheme, Host: base.Host, Path: path}) {
		if c.Name == name {
			return true
		}
	}
	return false
}
