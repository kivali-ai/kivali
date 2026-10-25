package web

import (
	"bytes"
	"io"
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

// startIn starts name's setup flow from a browser client signed in to
// ts, and returns it once it is on its Go step.
func startIn(t *testing.T, srv *Server, ts *httptest.Server, client *http.Client, name string) apitypes.SetupFlow {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+intBase+"/"+name+"/setup", bytes.NewReader([]byte(`{"flow":"setup"}`)))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("start %s: %v %v", name, resp, err)
	}
	_ = resp.Body.Close()
	return waitFlow(t, srv, name, apitypes.SetupFlowStateGo)
}

// startInWebview starts crm's browser sign-in from the Desktop team
// window: a browser signed in to Kivali, with cookies of its own. The
// window never loads /connect/start: Kivali Desktop takes it over.
func startInWebview(t *testing.T, srv *Server, ts *httptest.Server) apitypes.SetupFlow {
	t.Helper()
	webview, jar, _ := browserJar(t)
	base, _ := url.Parse(ts.URL)
	jar.SetCookies(base, []*http.Cookie{authCookie(t, srv)})
	return startIn(t, srv, ts, webview, "crm")
}

// authorizeOf is the vendor's page the flow's Go step sends a browser
// to, read from /connect/start without following it.
func authorizeOf(t *testing.T, srv *Server, f apitypes.SetupFlow) *url.URL {
	t.Helper()
	rr, _ := startPage(t, srv, f)
	u, err := url.Parse(rr.Header().Get("Location"))
	if err != nil || rr.Code != http.StatusSeeOther {
		t.Fatalf("start: %d %v", rr.Code, err)
	}
	return u
}

// Why this test exists: in Kivali Desktop the owner presses Connect in
// the team's window, a webview, and the sign-in runs in the system
// browser, which holds neither the Kivali session nor the flow's
// cookie. Desktop opens /auth/handoff?t=<token>&next=<flow> there: it
// must sign that browser in, give it the flow's cookie and send it on
// through /connect/start to the vendor, or the callback, which comes to
// the system browser, would ask the owner to sign in to Kivali again
// and then refuse the sign-in as started elsewhere. The system browser
// starts with no cookies and must end signed in, on the integration's
// page, with the connection holding the tokens.
func TestDesktopBrowserSignInThroughTheHandoff(t *testing.T) {
	srv, _, ts := desktopServer(t)
	f := startInWebview(t, srv, ts)
	base, _ := url.Parse(ts.URL)

	system, systemJar, landed := browserJar(t)
	resp, err := system.Get(ts.URL + auth.HandoffPath + "?t=" + url.QueryEscape(handoffToken(t, srv)) + "&next=" + f.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if *landed != nil || resp.StatusCode != http.StatusOK || resp.Request.URL.Path != connectCallbackPath {
		t.Fatalf("the system browser landed on %v, then %s (%d)", *landed, resp.Request.URL, resp.StatusCode)
	}
	assertDesktopReturnPage(t, string(page), "")
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("flow = %+v %q", f, textOf(f.Error))
	}
	rec, data := firstConnectionSecret(t, srv)
	if rec.SignIn == nil || rec.SignIn.Method != "browser" || data[connect.SecretKeyAccessToken] == "" {
		t.Errorf("not signed in: %+v %v", rec.SignIn, data)
	}
	if !hasCookie(systemJar, base, "/", auth.CookieName) {
		t.Error("the system browser was not signed in to Kivali")
	}
	if hasCookie(systemJar, base, connectCallbackPath, "kivali_connect") {
		t.Error("the flow's cookie outlived the callback in the system browser")
	}
}

// Why this test exists: the handoff hop is what makes the system
// browser pass the cookie check, and nothing else may. A system
// browser signed in to Kivali (by a handoff without next, or by hand)
// that goes to the vendor's page with the flow's address must still be
// refused as started elsewhere: the cookie binds the flow to a browser
// Kivali or Desktop gave it to.
func TestDesktopBrowserSignInNeedsTheHandoffsCookie(t *testing.T) {
	srv, _, ts := desktopServer(t)
	f := startInWebview(t, srv, ts)
	authorize := authorizeOf(t, srv, f)

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
	q := authorize.Query()
	q.Set("redirect_uri", ts.URL+connectCallbackPath)
	authorize.RawQuery = q.Encode()
	resp, err = system.Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if *landed == nil || (*landed).Query().Get("setup") != f.ID {
		t.Fatalf("landed on %v", *landed)
	}
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateFailed || textOf(f.Error) != errSignInStartAgain {
		t.Errorf("the flow reads %+v %q", f, textOf(f.Error))
	}
	assertNotSignedIn(t, srv)
}

// Why this test exists: a browser holds one flow cookie, and the
// handoff hands the system browser one. A Go the owner started in that
// browser itself, of another integration, can then no longer come back
// there: it must end at once, in words that say another one replaced
// it, rather than wait ten minutes for a callback that is refused. The
// Desktop sign-in must still finish.
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

	// crm2's sign-in, started in the system browser itself, which then
	// holds its cookie.
	system, systemJar, landed := browserJar(t)
	systemJar.SetCookies(base, []*http.Cookie{authCookie(t, srv)})
	f2 := startIn(t, srv, ts, system, "crm2")
	noFollow := &http.Client{Jar: systemJar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(ts.URL + *f2.Step.StartURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// crm's, from the Desktop window, through the handoff.
	f := startInWebview(t, srv, ts)
	resp, err = system.Get(ts.URL + auth.HandoffPath + "?t=" + url.QueryEscape(handoffToken(t, srv)) + "&next=" + f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if *landed != nil || resp.StatusCode != http.StatusOK || resp.Request.URL.Path != connectCallbackPath {
		t.Fatalf("the Desktop sign-in landed on %v, then %s (%d)", *landed, resp.Request.URL, resp.StatusCode)
	}
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateDone); f.State != apitypes.SetupFlowStateDone {
		t.Errorf("crm's flow reads %+v %q", f, textOf(f.Error))
	}
	if f := waitFlow(t, srv, "crm2", apitypes.SetupFlowStateFailed); textOf(f.Error) != errSignInReplaced {
		t.Errorf("crm2's flow reads %q", textOf(f.Error))
	}
}

// Why this test exists: next is a flow id the server looks up, never
// an address, so the handoff cannot become an open redirect, and it
// stays what it was: loopback only and one use per token. A next that
// names no flow on a Go step (an address, a path, an unknown id, one
// whose callback came, one that started at another address; one past
// its time, below) is refused as a bad token is: "/" with no session
// and no flow cookie, since Desktop names only a flow it just saw start
// and anything else is no errand of the app's. A replayed token signs
// nothing in and hands no cookie over; on a host that is not loopback
// the route does not exist.
func TestDesktopHandoffNextIsAPendingFlow(t *testing.T) {
	srv, _, ts := desktopServer(t)
	f := startInWebview(t, srv, ts)
	base, _ := url.Parse(ts.URL)
	host := base.Host

	for _, next := range []string{"https://evil.example/", "/org/integrations", "0000"} {
		assertHandoffRefused(t, serveHandoff(t, srv, host, handoffToken(t, srv), next), "next "+next)
	}
	// Started at 127.0.0.1:<port>: the same team reached as localhost
	// would hold the host-only cookie where the callback never looks.
	_, port, _ := strings.Cut(host, ":")
	assertHandoffRefused(t, serveHandoff(t, srv, "localhost:"+port, handoffToken(t, srv), f.ID), "another host")

	// One use per token: the replay signs nothing in and hands no cookie;
	// the browser, which the first use sent on, is shown the flow's page.
	tok := handoffToken(t, srv)
	first := serveHandoff(t, srv, host, tok, f.ID)
	if first.Code != http.StatusFound || first.Header().Get("Location") != connectStartPath+"?flow="+f.ID || cookieOf(first, "kivali_connect") == nil || cookieOf(first, auth.CookieName) == nil {
		t.Fatalf("first: %d to %q", first.Code, first.Header().Get("Location"))
	}
	if rr := serveHandoff(t, srv, host, tok, f.ID); rr.Code != http.StatusFound || rr.Header().Get("Location") != setupReturnPath("crm", f.ID) || cookieOf(rr, auth.CookieName) != nil || cookieOf(rr, "kivali_connect") != nil {
		t.Errorf("replay: %d to %q with %v", rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
	if f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateGo); f.State != apitypes.SetupFlowStateGo {
		t.Fatalf("after the replay the flow reads %+v %q", f, textOf(f.Error))
	}
	// Not loopback: no such route.
	if rr := serveHandoff(t, srv, "kivali.example.com", handoffToken(t, srv), f.ID); rr.Code != http.StatusNotFound {
		t.Errorf("non-loopback: %d", rr.Code)
	}
	// Its callback came: no longer a flow to send a browser to.
	authorize := authorizeOf(t, srv, f)
	q := authorize.Query()
	q.Set("redirect_uri", ts.URL+connectCallbackPath)
	authorize.RawQuery = q.Encode()
	back := vendorApprove(t, authorize)
	desktopCallback(t, srv, back, "", cookieOf(first, "kivali_connect"))
	if f := flowEnds(t, srv); f.State != apitypes.SetupFlowStateDone {
		t.Fatalf("the flow ended %+v %q", f, textOf(f.Error))
	}
	assertHandoffRefused(t, serveHandoff(t, srv, host, handoffToken(t, srv), f.ID), "a flow that came back")
}

// desktopCallback is a browser Kivali Desktop handed off coming back to
// Kivali at back with cookies: the callback answers it with the page
// that sends it back to the app (assertDesktopReturnPage), telling the
// app's listener at ret when the handoff named one.
func desktopCallback(t *testing.T, srv *Server, back *url.URL, ret string, cookies ...*http.Cookie) {
	t.Helper()
	rr := serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, back.String(), nil), cookies...)
	if rr.Code != http.StatusOK {
		t.Fatalf("the callback answered %d %s", rr.Code, rr.Body.String())
	}
	csp, origin := rr.Header().Get("Content-Security-Policy"), originOf(ret)
	if reaches := strings.Contains(csp, "connect-src "); reaches != (origin != "") || (origin != "" && !strings.Contains(csp, "connect-src "+origin)) {
		t.Errorf("CSP = %q, want connect-src %q", csp, origin)
	}
	assertDesktopReturnPage(t, rr.Body.String(), ret)
}

// assertDesktopReturnPage fails unless page is the one a handed-off
// browser gets at the callback: it says the setup goes on in Kivali
// Desktop, loads only from Kivali's own origin, and tells the app's
// listener at ret, or no one when ret is empty.
func assertDesktopReturnPage(t *testing.T, page, ret string) {
	t.Helper()
	if !strings.Contains(page, "<h1>Back to Kivali</h1>") || !strings.Contains(page, "The setup goes on in Kivali Desktop") {
		t.Errorf("page = %s", page)
	}
	if told := strings.Contains(page, "fetch("); told != (ret != "") || (ret != "" && !strings.Contains(page, `fetch("`+ret+`",`)) {
		t.Errorf("page tells %q: %s", ret, page)
	}
	assertLoadsOnlyItself(t, page)
}

// Why this test exists: a flow lasts ten minutes after its last change.
// A handoff for one that ran out (the owner's browser slow to open, the
// computer asleep) must not send the browser to the vendor, whose code
// Kivali could no longer exchange: it is refused, with no session.
func TestDesktopHandoffNextPastItsTime(t *testing.T) {
	srv, _, ts := desktopServer(t)
	f := startInWebview(t, srv, ts)
	base, _ := url.Parse(ts.URL)
	srv.Clock.(interface{ Advance(time.Duration) }).Advance(setupFlowTTL)
	assertHandoffRefused(t, serveHandoff(t, srv, base.Host, handoffToken(t, srv), f.ID), "an expired flow")
}

// assertHandoffRefused fails unless rr is a refused handoff: a 302 to
// "/" that sets neither a session nor a flow cookie.
func assertHandoffRefused(t *testing.T, rr *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/" || cookieOf(rr, auth.CookieName) != nil || cookieOf(rr, "kivali_connect") != nil {
		t.Errorf("%s: %d to %q with %v", what, rr.Code, rr.Header().Get("Location"), rr.Result().Cookies())
	}
}

// Why this test exists: the web app sends the browser for every Go step
// to GET /connect/start?flow=<id> (Kivali Desktop takes that address
// over in its team window). It must send the browser on to the step's
// address, the one the flow gave and nothing from the request, with the
// flow's cookie; a flow not on a Go step returns to its page, and an id
// it does not know (a typo, a flow that ran out) to the integrations
// list saying to start again, never to a vendor.
func TestConnectStartSendsTheBrowserToTheFlow(t *testing.T) {
	srv, _ := browserServer(t)
	startSetupFlow(t, srv, "crm")
	f := waitFlow(t, srv, "crm", apitypes.SetupFlowStateGo)
	rr, cookie := startPage(t, srv, f)
	if u, _ := url.Parse(rr.Header().Get("Location")); rr.Code != http.StatusSeeOther || u.Path != "/authorize" || cookie == nil {
		t.Errorf("a flow on its Go: %d to %q, cookie %v", rr.Code, rr.Header().Get("Location"), cookie)
	}
	for _, q := range []string{"?flow=0000", "?flow=", "?url=https://evil.example/"} {
		rr = serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, connectStartPath+q, nil))
		to, _ := url.Parse(rr.Header().Get("Location"))
		if rr.Code != http.StatusSeeOther || to.Path != "/org/integrations" || to.Query().Get("sign_in_error") != connectStartAgain {
			t.Errorf("%s: %d to %q", q, rr.Code, rr.Header().Get("Location"))
		}
	}
	srv.Clock.(interface{ Advance(time.Duration) }).Advance(setupFlowTTL)
	rr = serveSignedIn(t, srv, httptest.NewRequest(http.MethodGet, connectStartPath+"?flow="+f.ID, nil))
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
