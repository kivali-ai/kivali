package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newOAuth(t *testing.T, allowed []string) (*GoogleOAuth, *Codec) {
	t.Helper()
	codec, _ := NewCodec([]byte("01234567890123456789012345678901"))
	// httptest.NewRequest addresses example.com, so the tests' requests
	// arrive on the canonical host unless they say otherwise.
	return &GoogleOAuth{
		ClientID:     "cid",
		ClientSecret: "secret",
		RedirectURL:  "http://example.com/auth/callback",
		Codec:        codec,
		Allowlist:    NewAllowlist(allowed...),
	}, codec
}

func TestLoginHandlerRedirectsWithState(t *testing.T) {
	g, _ := newOAuth(t, []string{"alice@example.com"})
	g.AuthURL = "https://auth.example/authorize"
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next=/agents/alice", nil)
	rr := httptest.NewRecorder()
	g.LoginHandler(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d", rr.Code)
	}
	loc := rr.Header().Get("location")
	if !strings.HasPrefix(loc, "https://auth.example/authorize?") {
		t.Errorf("location = %q", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "cid" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("scope") != "openid email profile" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
	if q.Get("state") == "" {
		t.Errorf("missing state")
	}
	// State cookie should be set.
	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == stateCookieName {
			found = true
			if c.Value == "" {
				t.Error("state cookie empty")
			}
		}
	}
	if !found {
		t.Error("state cookie not set")
	}
}

func TestCallbackHappyPath(t *testing.T) {
	token := "tok-abc"
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "code=thecode") {
			t.Errorf("token body = %q", b)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token})
	}))
	defer tokenSrv.Close()

	userSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("authorization"); got != "Bearer "+token {
			t.Errorf("auth header = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email": "Alice@example.com", "verified_email": true,
		})
	}))
	defer userSrv.Close()

	g, codec := newOAuth(t, []string{"alice@example.com"})
	g.TokenURL = tokenSrv.URL
	g.UserInfoURL = userSrv.URL

	// First hit /login to get state cookie.
	loginReq := httptest.NewRequest(http.MethodGet, "/auth/login?next=/home", nil)
	loginRR := httptest.NewRecorder()
	g.LoginHandler(loginRR, loginReq)
	var stateCookie *http.Cookie
	var state string
	for _, c := range loginRR.Result().Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
		}
	}
	u, _ := url.Parse(loginRR.Header().Get("location"))
	state = u.Query().Get("state")
	if state == "" || stateCookie == nil {
		t.Fatalf("state or cookie missing: state=%q cookie=%v", state, stateCookie)
	}

	// Now call /callback with matching state + state cookie.
	cbURL := "/auth/callback?code=thecode&state=" + url.QueryEscape(state)
	cbReq := httptest.NewRequest(http.MethodGet, cbURL, nil)
	cbReq.AddCookie(stateCookie)
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)

	if cbRR.Code != http.StatusFound {
		t.Fatalf("code = %d, body = %s", cbRR.Code, cbRR.Body.String())
	}
	if cbRR.Header().Get("location") != "/home" {
		t.Errorf("location = %q", cbRR.Header().Get("location"))
	}
	var sessCookie *http.Cookie
	for _, c := range cbRR.Result().Cookies() {
		if c.Name == CookieName {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		t.Fatal("session cookie not set")
	}
	sess, err := codec.DecodeSession(sessCookie.Value)
	if err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if sess.Email != "alice@example.com" {
		t.Errorf("email = %q", sess.Email)
	}
}

func TestCallbackRejectsStateMismatch(t *testing.T) {
	g, _ := newOAuth(t, []string{"alice@example.com"})

	loginReq := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	loginRR := httptest.NewRecorder()
	g.LoginHandler(loginRR, loginReq)
	var stateCookie *http.Cookie
	for _, c := range loginRR.Result().Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
		}
	}
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state=bogus", nil)
	cbReq.AddCookie(stateCookie)
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	if cbRR.Code != http.StatusBadRequest {
		t.Errorf("code = %d", cbRR.Code)
	}
}

// TestCallbackClearsStateCookieOnFailure locks in the defer-clear
// fix: any callback exit (success OR failure) must clear the state
// cookie. Without the defer, an early-exit on bad signature /
// mismatch / expired state left the stale cookie alive for stateTTL,
// briefly replayable with a stolen authorization code.
func TestCallbackClearsStateCookieOnFailure(t *testing.T) {
	g, _ := newOAuth(t, []string{"alice@example.com"})

	loginReq := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	loginRR := httptest.NewRecorder()
	g.LoginHandler(loginRR, loginReq)
	var stateCookie *http.Cookie
	for _, c := range loginRR.Result().Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("login didn't set state cookie")
	}

	// Force a failure path: mismatched state.
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state=bogus", nil)
	cbReq.AddCookie(stateCookie)
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	if cbRR.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 from state mismatch, got %d", cbRR.Code)
	}

	// The response must include a Set-Cookie that clears the state
	// cookie (MaxAge=-1, empty Value).
	cleared := false
	for _, c := range cbRR.Result().Cookies() {
		if c.Name == stateCookieName && c.Value == "" && c.MaxAge < 0 {
			cleared = true
			break
		}
	}
	if !cleared {
		t.Errorf("failure path did not clear state cookie; Set-Cookie headers = %v", cbRR.Result().Cookies())
	}
}

func TestCallbackForbidsNonAllowlisted(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "t"})
	}))
	defer tokenSrv.Close()
	userSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"email": "eve@example.com", "verified_email": true})
	}))
	defer userSrv.Close()

	g, _ := newOAuth(t, []string{"alice@example.com"})
	g.TokenURL, g.UserInfoURL = tokenSrv.URL, userSrv.URL

	loginReq := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	loginRR := httptest.NewRecorder()
	g.LoginHandler(loginRR, loginReq)
	var stateCookie *http.Cookie
	for _, c := range loginRR.Result().Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
		}
	}
	u, _ := url.Parse(loginRR.Header().Get("location"))
	state := u.Query().Get("state")

	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+url.QueryEscape(state), nil)
	cbReq.AddCookie(stateCookie)
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	if cbRR.Code != http.StatusForbidden {
		t.Errorf("code = %d, body = %s", cbRR.Code, cbRR.Body.String())
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	g, _ := newOAuth(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	rr := httptest.NewRecorder()
	g.LogoutHandler(rr, req)
	if rr.Code != http.StatusFound {
		t.Errorf("code = %d", rr.Code)
	}
	var cleared bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == CookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("session cookie not cleared")
	}
}

// Sign-in is ready with a client id plus either its secret (an own
// client) or the relay that holds the secret (the public client); a
// server started without sign-in holds a nil *GoogleOAuth.
func TestReadyNeedsClientIDAndSecretOrRelay(t *testing.T) {
	var none *GoogleOAuth
	cases := []struct {
		name string
		g    *GoogleOAuth
		want bool
	}{
		{"nil", none, false},
		{"neither", &GoogleOAuth{}, false},
		{"id only", &GoogleOAuth{ClientID: "id"}, false},
		{"secret only", &GoogleOAuth{ClientSecret: "secret"}, false},
		{"relay only", &GoogleOAuth{RelayURL: "https://relay.example"}, false},
		{"id and secret", &GoogleOAuth{ClientID: "id", ClientSecret: "secret"}, true},
		{"id and relay", &GoogleOAuth{ClientID: "id", RelayURL: "https://relay.example"}, true},
	}
	for _, tc := range cases {
		if got := tc.g.Ready(); got != tc.want {
			t.Errorf("%s: Ready() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The path the callback sends the browser back to is whatever the state
// carries, so only a path on this site may be packed into it: a value
// the browser would read as another host becomes "/".
func TestLoginHandlerKeepsNextOnThisSite(t *testing.T) {
	g, codec := newOAuth(t, []string{"alice@example.com"})
	g.AuthURL = "https://auth.example/authorize"
	for raw, want := range map[string]string{
		"/agents/alice?tab=1":   "/agents/alice?tab=1",
		"":                      "/",
		"agents/alice":          "/",
		"https://evil.example/": "/",
		"//evil.example/x":      "/",
		`/\evil.example/x`:      "/",
	} {
		req := httptest.NewRequest(http.MethodGet, "/auth/login?next="+url.QueryEscape(raw), nil)
		rr := httptest.NewRecorder()
		g.LoginHandler(rr, req)
		if rr.Code != http.StatusFound {
			t.Fatalf("next=%q: code = %d", raw, rr.Code)
		}
		var packed []byte
		for _, c := range rr.Result().Cookies() {
			if c.Name == stateCookieName {
				var err error
				if packed, err = codec.Verify(c.Value); err != nil {
					t.Fatalf("next=%q: verify state cookie: %v", raw, err)
				}
			}
		}
		claim, err := unpackState(packed)
		if err != nil {
			t.Fatalf("next=%q: unpack state: %v", raw, err)
		}
		if claim.Next != want {
			t.Errorf("next=%q: state carries %q, want %q", raw, claim.Next, want)
		}
	}
}

// newCanonicalOAuth configures the redirect on 127.0.0.1:8080, the dev
// cluster's shape, with a recognisable consent URL.
func newCanonicalOAuth(t *testing.T) (*GoogleOAuth, *Codec) {
	t.Helper()
	g, codec := newOAuth(t, []string{"alice@example.com"})
	g.RedirectURL = "http://127.0.0.1:8080/auth/callback"
	g.AuthURL = "https://auth.example/authorize"
	return g, codec
}

func loginOn(g *GoogleOAuth, host, next string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/auth/login?next="+url.QueryEscape(next), nil)
	req.Host = host
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rr := httptest.NewRecorder()
	g.LoginHandler(rr, req)
	return rr
}

func stateCookieOf(rr *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookieName {
			return c
		}
	}
	return nil
}

func packedNext(t *testing.T, codec *Codec, c *http.Cookie) string {
	t.Helper()
	packed, err := codec.Verify(c.Value)
	if err != nil {
		t.Fatalf("verify state cookie: %v", err)
	}
	claim, err := unpackState(packed)
	if err != nil {
		t.Fatalf("unpack state: %v", err)
	}
	return claim.Next
}

// Sign-in started on a host other than the redirect's (localhost vs
// 127.0.0.1) would set the host-only state cookie where the callback
// never sees it, so the first hop moves the browser to the configured
// host, keeping next and setting no cookie.
func TestLoginOnOtherHostRedirectsToCanonicalHost(t *testing.T) {
	g, _ := newCanonicalOAuth(t)
	rr := loginOn(g, "localhost:8080", "/agents/alice?tab=1", nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d", rr.Code)
	}
	loc, err := url.Parse(rr.Header().Get("location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if loc.Scheme != "http" || loc.Host != "127.0.0.1:8080" || loc.Path != "/auth/login" {
		t.Errorf("location = %q, want http://127.0.0.1:8080/auth/login?next=...", loc)
	}
	if got := loc.Query().Get("next"); got != "/agents/alice?tab=1" {
		t.Errorf("next = %q", got)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Errorf("first hop set cookies: %v", rr.Result().Cookies())
	}
}

func TestLoginOnCanonicalHostProceedsToGoogle(t *testing.T) {
	g, codec := newCanonicalOAuth(t)
	rr := loginOn(g, "127.0.0.1:8080", "/home", nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d", rr.Code)
	}
	if loc := rr.Header().Get("location"); !strings.HasPrefix(loc, "https://auth.example/authorize?") {
		t.Errorf("location = %q", loc)
	}
	c := stateCookieOf(rr)
	if c == nil {
		t.Fatal("state cookie not set")
	}
	if got := packedNext(t, codec, c); got != "/home" {
		t.Errorf("state carries next %q", got)
	}
}

// The host comparison ignores case: a DNS name is case-insensitive.
func TestLoginHostComparisonIgnoresCase(t *testing.T) {
	g, _ := newOAuth(t, nil)
	g.RedirectURL = "https://App.Example.com/auth/callback"
	rr := loginOn(g, "app.example.COM", "/", nil)
	if stateCookieOf(rr) == nil {
		t.Errorf("same host in another case was redirected: location = %q", rr.Header().Get("location"))
	}
}

// An off-site next becomes "/" on the first hop, and again on the second.
func TestLoginOffSiteNextResetOnBothHops(t *testing.T) {
	g, codec := newCanonicalOAuth(t)
	for _, raw := range []string{"https://evil.example/", "//evil.example/x", `/\evil.example/x`} {
		rr := loginOn(g, "localhost:8080", raw, nil)
		loc, err := url.Parse(rr.Header().Get("location"))
		if err != nil {
			t.Fatalf("next=%q: parse location: %v", raw, err)
		}
		if loc.Host != "127.0.0.1:8080" {
			t.Fatalf("next=%q: first hop went to %q", raw, loc)
		}
		if got := loc.Query().Get("next"); got != "/" {
			t.Errorf("next=%q: first hop carries next %q, want /", raw, got)
		}
		rr = loginOn(g, "127.0.0.1:8080", raw, nil)
		c := stateCookieOf(rr)
		if c == nil {
			t.Fatalf("next=%q: second hop set no state cookie", raw)
		}
		if got := packedNext(t, codec, c); got != "/" {
			t.Errorf("next=%q: second hop state carries %q, want /", raw, got)
		}
	}
}

// Behind a reverse proxy the browser's host is the first
// X-Forwarded-Host value, not the backend's r.Host.
func TestLoginForwardedHostMatchingCanonicalProceeds(t *testing.T) {
	g, _ := newOAuth(t, nil)
	g.RedirectURL = "https://app.example.com/auth/callback"
	g.AuthURL = "https://auth.example/authorize"
	h := http.Header{"X-Forwarded-Host": {"app.example.com, internal.proxy"}}
	rr := loginOn(g, "10.0.0.5:8080", "/", h)
	if loc := rr.Header().Get("location"); !strings.HasPrefix(loc, "https://auth.example/authorize?") {
		t.Errorf("location = %q, want the consent page", loc)
	}
	if stateCookieOf(rr) == nil {
		t.Error("state cookie not set")
	}
}

// A RedirectURL with no usable host cannot name a canonical host, so
// sign-in proceeds on whatever host it started. With no RedirectURL at
// all only the public client can sign in, deriving its callback from
// the request, so that case runs as the public client.
func TestLoginUnparsableRedirectURLSkipsHop(t *testing.T) {
	for _, redirect := range []string{"", "::not a url", "/auth/callback"} {
		g, _ := newOAuth(t, nil)
		g.RedirectURL = redirect
		if redirect == "" {
			g.ClientSecret = ""
			g.RelayURL = "https://relay.example/oauth"
		}
		g.AuthURL = "https://auth.example/authorize"
		rr := loginOn(g, "localhost:8080", "/", nil)
		if loc := rr.Header().Get("location"); !strings.HasPrefix(loc, "https://auth.example/authorize?") {
			t.Errorf("redirect=%q: location = %q, want the consent page", redirect, loc)
		}
		if stateCookieOf(rr) == nil {
			t.Errorf("redirect=%q: state cookie not set", redirect)
		}
	}
}

// A callback without the state cookie says what happened and where to
// start again, in one sentence a person can act on.
func TestCallbackMissingCookieNamesCanonicalBase(t *testing.T) {
	g, _ := newCanonicalOAuth(t)
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state=s", nil)
	rr := httptest.NewRecorder()
	g.CallbackHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "did not send the sign-in cookie") {
		t.Errorf("body = %q, want the missing-cookie explanation", body)
	}
	if !strings.Contains(body, "Open http://127.0.0.1:8080 and try again.") {
		t.Errorf("body = %q, want the canonical base", body)
	}
}
