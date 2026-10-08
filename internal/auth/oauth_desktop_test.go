package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The desktop app's loopback listener, as it reports it at login.
const (
	returnPort  = "54321"
	returnToken = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	desktopQ    = "client=desktop&return_port=" + returnPort + "&return_token=" + returnToken
)

func loginAs(t *testing.T, g *GoogleOAuth, host, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/login?"+query, nil)
	req.Host = host
	rr := httptest.NewRecorder()
	g.LoginHandler(rr, req)
	return rr
}

func callback(g *GoogleOAuth, host, query string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?"+query, nil)
	req.Host = host
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	g.CallbackHandler(rr, req)
	return rr
}

// A desktop sign-in is marked in the state cookie and carries the app's
// loopback listener in the nonce, so the state the provider echoes back
// tells a browser with no cookie where to go. Without client=desktop
// nothing is marked; with it, the listener must be described properly.
func TestDesktopLoginPutsTheListenerInTheNonce(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")

	rr := loginAs(t, g, "org.example.com", "next=/home&"+desktopQ)
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	claim := stateClaimOf(t, codec, rr)
	wantPrefix := desktopNoncePrefix + returnPort + "~" + returnToken + "~"
	if !claim.Desktop || !strings.HasPrefix(claim.Nonce, wantPrefix) || len(claim.Nonce) != len(wantPrefix)+43 || claim.Next != "/home" {
		t.Fatalf("claim = %+v", claim)
	}
	u, _ := url.Parse(rr.Header().Get("location"))
	state := u.Query().Get("state")
	if u.Query().Get("nonce") != claim.Nonce || !strings.HasPrefix(state, wantPrefix) || strings.Count(state, ".") != 1 {
		t.Fatalf("provider URL nonce = %q, state = %q", u.Query().Get("nonce"), state)
	}
	if got, ok := desktopReturnURL(state); !ok || got != "http://127.0.0.1:54321/signin/"+returnToken {
		t.Fatalf("desktopReturnURL = %q, %v", got, ok)
	}

	rr = loginAs(t, g, "org.example.com", "next=/home&return_port="+returnPort+"&return_token="+returnToken)
	if claim := stateClaimOf(t, codec, rr); claim.Desktop || strings.HasPrefix(claim.Nonce, desktopNoncePrefix) {
		t.Fatalf("an ordinary sign-in was marked: %+v", claim)
	}

	for _, bad := range []string{
		"client=desktop",
		"client=desktop&return_port=54321",
		"client=desktop&return_token=" + returnToken,
		"client=desktop&return_port=0&return_token=" + returnToken,
		"client=desktop&return_port=65536&return_token=" + returnToken,
		"client=desktop&return_port=54321x&return_token=" + returnToken,
		"client=desktop&return_port=54321&return_token=short",
		"client=desktop&return_port=54321&return_token=" + strings.Repeat("*", 43),
	} {
		if rr := loginAs(t, g, "org.example.com", bad); rr.Code != http.StatusBadRequest {
			t.Errorf("%q: code = %d, want 400", bad, rr.Code)
		}
	}
}

// With an own client the login hop to the configured host carries the
// desktop parameters along, or the sign-in would arrive there as an
// ordinary browser sign-in.
func TestDesktopLoginHopKeepsTheParameters(t *testing.T) {
	g, _ := newOAuth(t, []string{"alice@example.com"})
	g.RedirectURL = "https://org.example.com/auth/callback"
	rr := loginAs(t, g, "localhost:8080", "next=/home&"+desktopQ)
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d", rr.Code)
	}
	u, err := url.Parse(rr.Header().Get("location"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "org.example.com" || u.Path != "/auth/login" || q.Get("client") != "desktop" || q.Get("return_port") != returnPort || q.Get("return_token") != returnToken || q.Get("next") != "/home" {
		t.Fatalf("hop = %s", u)
	}
}

func desktopState(t *testing.T, g *GoogleOAuth, codec *Codec) (string, *http.Cookie) {
	t.Helper()
	loginRR := loginAs(t, g, "org.example.com", "next=/home&"+desktopQ)
	claim := stateClaimOf(t, codec, loginRR)
	return encodeState(claim.Nonce, claim.Return), stateCookieOf(loginRR)
}

// The system browser, which has no cookie for the sign-in, is sent with
// the provider's whole answer to the app's loopback listener, verified
// and stored nowhere, with no session. A browser with a cookie of its
// own from an abandoned ordinary sign-in, or a broken cookie, is sent
// the same way. An ordinary sign-in arriving without its cookie is
// still an error.
func TestBrowserWithoutCookieIsSentToTheListener(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	state, cookie := desktopState(t, g, codec)
	ordinary := loginAs(t, g, "org.example.com", "next=/home")
	want := "http://127.0.0.1:54321/signin/" + returnToken + "?"

	for name, c := range map[string]*http.Cookie{
		"no cookie": nil,
		"abandoned": stateCookieOf(ordinary),
		"garbage":   {Name: stateCookieName, Value: "not-a-signed-claim"},
	} {
		q := "code=thecode&state=" + url.QueryEscape(state) + "&scope=openid"
		rr := callback(g, "org.example.com", q, c)
		if rr.Code != http.StatusFound || rr.Header().Get("location") != want+q || hasSession(rr) {
			t.Fatalf("%s: code = %d, location = %q, session = %v", name, rr.Code, rr.Header().Get("location"), hasSession(rr))
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: cache-control = %q", name, rr.Header().Get("Cache-Control"))
		}
	}

	// A refusal by the provider goes to the listener too, as it came.
	q := "error=access_denied&state=" + url.QueryEscape(state)
	if rr := callback(g, "org.example.com", q, nil); rr.Code != http.StatusFound || rr.Header().Get("location") != want+q {
		t.Fatalf("error: code = %d, location = %q", rr.Code, rr.Header().Get("location"))
	}

	// The same answer with the cookie, as the webview sends it, is
	// checked as any other (here it fails at the token exchange, which
	// has no relay to go to; the point is that it got that far).
	rr := callback(g, "org.example.com", "code=thecode&state="+url.QueryEscape(state), cookie)
	if rr.Code == http.StatusFound {
		t.Fatalf("the webview's callback was sent to the listener: %s", rr.Header().Get("location"))
	}

	// A webview whose own desktop sign-in is in progress, fed another
	// desktop sign-in's answer: the ordinary mismatch error.
	_, other := desktopState(t, g, codec)
	rr = callback(g, "org.example.com", "code=thecode&state="+url.QueryEscape(state), other)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "did not match") {
		t.Fatalf("foreign answer in a webview: code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Without the provider's answer there is nothing to deliver: no
	// redirect, so this install never bounces a browser to a loopback
	// port on a bare state.
	rr = callback(g, "org.example.com", "state="+url.QueryEscape(state), nil)
	if rr.Code != http.StatusBadRequest || rr.Header().Get("location") != "" {
		t.Fatalf("bare state: code = %d, location = %q", rr.Code, rr.Header().Get("location"))
	}

	// A crafted state with the mark but a malformed listener is no
	// desktop sign-in: no redirect anywhere.
	crafted := desktopNoncePrefix + "99999~" + returnToken + "~x." + strings.TrimPrefix(state[strings.Index(state, ".")+1:], "")
	rr = callback(g, "org.example.com", "code=thecode&state="+url.QueryEscape(crafted), nil)
	if rr.Code != http.StatusBadRequest || rr.Header().Get("location") != "" {
		t.Fatalf("crafted state: code = %d, location = %q", rr.Code, rr.Header().Get("location"))
	}

	// An ordinary sign-in without its cookie: the missing-cookie error.
	oc := stateClaimOf(t, codec, ordinary)
	rr = callback(g, "org.example.com", "code=thecode&state="+url.QueryEscape(encodeState(oc.Nonce, oc.Return)), nil)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "did not send the sign-in cookie") {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// The app's webview completes a desktop sign-in exactly like a browser
// completes an ordinary one: the cookie it set at login, the relay, the
// id_token with the marked nonce, the session.
func TestDesktopSignInCompletesInTheWebview(t *testing.T) {
	iss := newIDTokenIssuer(t)
	var nonce string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok", "id_token": iss.mint(t, iss.key, iss.kid, validClaims(nonce))})
	}))
	defer relay.Close()
	g, codec := newPublicOAuth(t, relay.URL+"/oauth")
	g.JWKSURL = iss.jwks.URL

	loginRR := loginAs(t, g, "127.0.0.1:8080", "next=/home&"+desktopQ)
	claim := stateClaimOf(t, codec, loginRR)
	nonce = claim.Nonce
	state := encodeState(claim.Nonce, claim.Return)

	rr := callback(g, "127.0.0.1:8080", "code=thecode&state="+url.QueryEscape(state), stateCookieOf(loginRR))
	if rr.Code != http.StatusFound || rr.Header().Get("location") != "/home" || !hasSession(rr) {
		t.Fatalf("code = %d, location = %q, session = %v, body = %s", rr.Code, rr.Header().Get("location"), hasSession(rr), rr.Body.String())
	}
}

// next is an on-site path or nothing: a leading "//" or "/\" names
// another host, and a control character would let a browser read
// "/\t/evil" as "//evil".
func TestNextIsOnSiteOnly(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	for in, want := range map[string]string{
		"/home":                 "/home",
		"/a/b?c=d":              "/a/b?c=d",
		"":                      "/",
		"home":                  "/",
		"//evil.example":        "/",
		`/\evil.example`:        "/",
		"/\t/evil.example":      "/",
		"/home\n":               "/",
		"https://evil.example/": "/",
	} {
		rr := loginAs(t, g, "org.example.com", "next="+url.QueryEscape(in))
		if got := stateClaimOf(t, codec, rr).Next; got != want {
			t.Errorf("next %q: recorded %q, want %q", in, got, want)
		}
	}
}

// The cookies are Secure unless the host is loopback, in every spelling
// the desktop app may use for it. (127.1.2.3 and 10.0.0.5 are not hosts
// the public client signs in at; isLocal is checked for them directly.)
func TestCookiesAreSecureOffLoopback(t *testing.T) {
	for host, local := range map[string]bool{"127.1.2.3:8080": true, "10.0.0.5:8080": false} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		if isLocal(req) != local {
			t.Errorf("isLocal(%s) = %v", host, !local)
		}
	}
	g, _ := newPublicOAuth(t, "https://relay.example/oauth")
	for host, local := range map[string]bool{
		"127.0.0.1:8080":  true,
		"[::1]:8080":      true,
		"localhost:8080":  true,
		"LOCALHOST":       true,
		"org.example.com": false,
	} {
		rr := loginAs(t, g, host, "next=/")
		c := stateCookieOf(rr)
		if c == nil {
			t.Fatalf("%s: no state cookie", host)
		}
		if c.Secure == local {
			t.Errorf("host %s: Secure = %v", host, c.Secure)
		}
	}
}
