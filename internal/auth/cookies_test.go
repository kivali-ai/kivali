package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ownClientAs is an own-client GoogleOAuth whose provider names email.
func ownClientAs(t *testing.T, email string, allowed ...string) (*GoogleOAuth, *Codec) {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t"})
	}))
	t.Cleanup(tokenSrv.Close)
	userSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"email": email, "verified_email": true})
	}))
	t.Cleanup(userSrv.Close)
	g, codec := newOAuth(t, allowed)
	g.TokenURL, g.UserInfoURL = tokenSrv.URL, userSrv.URL
	return g, codec
}

func cookieNamed(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func providerState(t *testing.T, loginRR *httptest.ResponseRecorder) string {
	t.Helper()
	u, err := url.Parse(loginRR.Header().Get("location"))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func TestCookieNamesCarryTheSuffix(t *testing.T) {
	if SessionCookieName("") != "kivali_session" || StateCookieName("") != "kivali_oauth_state" {
		t.Fatalf("unsuffixed names = %q, %q", SessionCookieName(""), StateCookieName(""))
	}
	if SessionCookieName("home") != "kivali_session_home" || StateCookieName("home") != "kivali_oauth_state_home" {
		t.Fatalf("suffixed names = %q, %q", SessionCookieName("home"), StateCookieName("home"))
	}
	var nilMW *Middleware
	if nilMW.SessionCookieName() != CookieName {
		t.Fatalf("nil middleware reads %q", nilMW.SessionCookieName())
	}
}

// With a cookie suffix every cookie sign-in sets, reads or clears
// carries it, and the plain names are neither set nor honoured: a
// second server on the same host keeps its own sign-in.
func TestCookieSuffixIsUsedEverywhere(t *testing.T) {
	g, codec := ownClientAs(t, "alice@example.com", "alice@example.com")
	g.CookieSuffix = "home"

	loginRR := loginAs(t, g, "example.com", "next=/home")
	state := cookieNamed(loginRR, "kivali_oauth_state_home")
	if state == nil || cookieNamed(loginRR, "kivali_oauth_state") != nil {
		t.Fatalf("login set %v", loginRR.Result().Cookies())
	}
	q := "code=c&state=" + url.QueryEscape(providerState(t, loginRR))

	// The same value under the plain name is not this server's cookie.
	plain := &http.Cookie{Name: "kivali_oauth_state", Value: state.Value}
	if rr := callback(g, "example.com", q, plain); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "did not send the sign-in cookie") {
		t.Fatalf("plain-named state: code = %d, body = %s", rr.Code, rr.Body.String())
	}

	rr := callback(g, "example.com", q, state)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback: code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c := cookieNamed(rr, "kivali_oauth_state_home"); c == nil || c.MaxAge >= 0 {
		t.Errorf("the suffixed state cookie was not cleared: %v", c)
	}
	sess := cookieNamed(rr, "kivali_session_home")
	if sess == nil || sess.Value == "" || cookieNamed(rr, CookieName) != nil {
		t.Fatalf("callback set %v", rr.Result().Cookies())
	}

	mw := &Middleware{Codec: codec, Allowlist: g.Allowlist, CookieSuffix: "home"}
	for name, want := range map[string]int{"kivali_session_home": http.StatusOK, CookieName: http.StatusFound} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: name, Value: sess.Value})
		got := httptest.NewRecorder()
		mw.Wrap(passthrough()).ServeHTTP(got, req)
		if got.Code != want {
			t.Errorf("middleware with %s: code = %d, want %d", name, got.Code, want)
		}
	}

	out := httptest.NewRecorder()
	g.LogoutHandler(out, httptest.NewRequest(http.MethodGet, "/auth/logout", nil))
	if c := cookieNamed(out, "kivali_session_home"); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout did not clear the suffixed session: %v", out.Result().Cookies())
	}
}

// A desktop sign-in by an account the allowlist refuses lands on the
// not-invited page, which the app can recognise by its path; the email
// stays out of the URL. A browser sign-in keeps the plain-text 403.
func TestNotInvitedDesktopIsRedirected(t *testing.T) {
	g, _ := ownClientAs(t, "eve@example.com", "alice@example.com")

	loginRR := loginAs(t, g, "example.com", "next=/home&"+desktopQ)
	rr := callback(g, "example.com", "code=c&state="+url.QueryEscape(providerState(t, loginRR)), stateCookieOf(loginRR))
	if rr.Code != http.StatusFound || rr.Header().Get("location") != NotInvitedPath || hasSession(rr) {
		t.Fatalf("desktop: code = %d, location = %q, session = %v, body = %s", rr.Code, rr.Header().Get("location"), hasSession(rr), rr.Body.String())
	}
	if c := stateCookieOf(rr); c == nil || c.MaxAge >= 0 {
		t.Error("desktop: the state cookie was not cleared")
	}
	// The refused account, for the app's "eve@example.com isn't invited":
	// HttpOnly, only on the not-invited page, two minutes.
	d := cookieNamed(rr, DeniedCookieName)
	if d == nil || d.Value != "eve@example.com" || !d.HttpOnly || d.Path != NotInvitedPath || d.MaxAge != 120 || !d.Secure {
		t.Errorf("desktop: denied cookie = %+v", d)
	}

	loginRR = loginAs(t, g, "example.com", "next=/home")
	rr = callback(g, "example.com", "code=c&state="+url.QueryEscape(providerState(t, loginRR)), stateCookieOf(loginRR))
	if rr.Code != http.StatusForbidden || rr.Header().Get("location") != "" || !strings.Contains(rr.Body.String(), "eve@example.com is not this team's owner") {
		t.Fatalf("browser: code = %d, location = %q, body = %s", rr.Code, rr.Header().Get("location"), rr.Body.String())
	}
	if cookieNamed(rr, DeniedCookieName) != nil {
		t.Error("browser: a denied cookie was set")
	}
}

// With a cookie suffix the denied cookie carries it, as every other does.
func TestDeniedCookieCarriesTheSuffix(t *testing.T) {
	if DeniedCookieNameFor("") != "kivali_denied" || DeniedCookieNameFor("home-3f2a") != "kivali_denied_home-3f2a" {
		t.Fatalf("names = %q, %q", DeniedCookieNameFor(""), DeniedCookieNameFor("home-3f2a"))
	}
	g, _ := ownClientAs(t, "eve@example.com", "alice@example.com")
	g.CookieSuffix = "home-3f2a"
	loginRR := loginAs(t, g, "example.com", "next=/home&"+desktopQ)
	state := cookieNamed(loginRR, StateCookieName("home-3f2a"))
	rr := callback(g, "example.com", "code=c&state="+url.QueryEscape(providerState(t, loginRR)), state)
	if c := cookieNamed(rr, "kivali_denied_home-3f2a"); c == nil || c.Value != "eve@example.com" || cookieNamed(rr, DeniedCookieName) != nil {
		t.Fatalf("callback set %v", rr.Result().Cookies())
	}
}

func TestNotInvitedPage(t *testing.T) {
	rr := httptest.NewRecorder()
	NotInvitedHandler(rr, httptest.NewRequest(http.MethodGet, NotInvitedPath, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("code = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rr.Body.String(), "This Google account is not this team's owner. Only the owner can sign in.") {
		t.Errorf("body = %s", rr.Body.String())
	}
}
