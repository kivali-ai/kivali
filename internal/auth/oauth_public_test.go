package auth

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newPublicOAuth is the public client: no secret, no configured
// redirect, a relay, and https://org.example.com as its external
// address. Sign-in derives this install's callback from an allowed
// request host and exchanges the code through the relay.
func newPublicOAuth(t *testing.T, relayURL string) (*GoogleOAuth, *Codec) {
	t.Helper()
	g, codec := newOAuth(t, []string{"alice@example.com"})
	g.ClientSecret = ""
	g.RedirectURL = ""
	g.ExternalURL = "https://org.example.com"
	g.RelayURL = relayURL
	g.AuthURL = "https://auth.example/authorize"
	return g, codec
}

func stateClaimOf(t *testing.T, codec *Codec, rr *httptest.ResponseRecorder) stateClaim {
	t.Helper()
	c := stateCookieOf(rr)
	if c == nil {
		t.Fatal("state cookie not set")
	}
	packed, err := codec.Verify(c.Value)
	if err != nil {
		t.Fatalf("verify state cookie: %v", err)
	}
	claim, err := unpackState(packed)
	if err != nil {
		t.Fatalf("unpack state: %v", err)
	}
	return claim
}

// The public client sends the browser to the provider with the relay's
// callback as redirect_uri, a PKCE challenge, and a state that names this
// install's own callback, derived from the request, so the relay can
// bounce the browser back here.
func TestPublicLoginUsesRelayAndPKCE(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth/")
	h := http.Header{"X-Forwarded-Proto": {"https"}}
	rr := loginOn(g, "org.example.com", "/agents/alice", h)
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d: %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	q := loc.Query()
	if got := q.Get("redirect_uri"); got != "https://relay.example/oauth/callback" {
		t.Errorf("redirect_uri = %q, want the relay's callback", got)
	}
	if q.Get("client_secret") != "" {
		t.Error("the public client must never send a secret")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	claim := stateClaimOf(t, codec, rr)
	if len(claim.Verifier) < 43 {
		t.Errorf("verifier %q is shorter than PKCE allows", claim.Verifier)
	}
	if got := q.Get("code_challenge"); got != pkceChallenge(claim.Verifier) {
		t.Errorf("code_challenge = %q, want the S256 hash of the cookie's verifier", got)
	}
	if claim.Return != "https://org.example.com/auth/callback" {
		t.Errorf("return URL = %q, want this install's callback from the forwarded scheme and host", claim.Return)
	}
	nonce, encoded, ok := strings.Cut(q.Get("state"), ".")
	if !ok || nonce != claim.Nonce {
		t.Fatalf("state = %q, want <nonce>.<return>", q.Get("state"))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != claim.Return {
		t.Errorf("state's return part decodes to %q (%v), want %q", decoded, err, claim.Return)
	}
}

// The derived callback's scheme: always https for the external name
// (it is an https origin); for loopback, the connection's own, with
// X-Forwarded-Proto never read.
func TestPublicReturnURLScheme(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		header http.Header
		tls    bool
		want   string
	}{
		{"external plain", "org.example.com", nil, false, "https://org.example.com/auth/callback"},
		{"external forwarded http", "org.example.com", http.Header{"X-Forwarded-Proto": {"http"}}, false, "https://org.example.com/auth/callback"},
		{"loopback plain", "127.0.0.1:8080", nil, false, "http://127.0.0.1:8080/auth/callback"},
		{"loopback tls", "127.0.0.1:8080", nil, true, "https://127.0.0.1:8080/auth/callback"},
		{"loopback forwarded https", "127.0.0.1:8080", http.Header{"X-Forwarded-Proto": {"https"}}, false, "http://127.0.0.1:8080/auth/callback"},
	}
	for _, tc := range cases {
		g, codec := newPublicOAuth(t, "https://relay.example/oauth")
		req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
		req.Host = tc.host
		for k, vs := range tc.header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if tc.tls {
			req.TLS = &tls.ConnectionState{}
		}
		rr := httptest.NewRecorder()
		g.LoginHandler(rr, req)
		if got := stateClaimOf(t, codec, rr).Return; got != tc.want {
			t.Errorf("%s: return URL = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An own client has one registered redirect URI; with none configured
// there is nothing to derive and sign-in refuses rather than guess.
func TestOwnClientWithoutRedirectRefuses(t *testing.T) {
	g, _ := newOAuth(t, nil)
	g.RedirectURL = ""
	rr := loginOn(g, "org.example.com", "/", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rr.Code)
	}
	if stateCookieOf(rr) != nil {
		t.Error("a refused sign-in set a state cookie")
	}
}

// The public client exchanges the code at the relay's token route with
// the standard form plus the PKCE verifier and no secret; the relay adds
// the secret and returns the provider's response, whose id_token names
// the person. The rest of the flow (allowlist, session) is the same as
// for an own client.
func TestPublicCallbackExchangesThroughRelay(t *testing.T) {
	iss := newIDTokenIssuer(t)
	var relayForm url.Values
	var nonce string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("relay path = %q, want /oauth/token", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		relayForm = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok", "id_token": iss.mint(t, iss.key, iss.kid, validClaims(nonce))})
	}))
	defer relay.Close()

	g, codec := newPublicOAuth(t, relay.URL+"/oauth")
	g.JWKSURL = iss.jwks.URL

	loginRR := loginOn(g, "org.example.com", "/home", nil)
	claim := stateClaimOf(t, codec, loginRR)
	nonce = claim.Nonce
	u, _ := url.Parse(loginRR.Header().Get("location"))
	state := u.Query().Get("state")

	// The relay bounced the browser here with code and state unchanged.
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=thecode&state="+url.QueryEscape(state), nil)
	cbReq.Host = "org.example.com"
	cbReq.AddCookie(stateCookieOf(loginRR))
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	if cbRR.Code != http.StatusFound {
		t.Fatalf("code = %d, body = %s", cbRR.Code, cbRR.Body.String())
	}
	if cbRR.Header().Get("location") != "/home" {
		t.Errorf("location = %q", cbRR.Header().Get("location"))
	}
	if relayForm == nil {
		t.Fatal("the relay's token route was never called")
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "thecode",
		"client_id":     "cid",
		"code_verifier": claim.Verifier,
		"redirect_uri":  relay.URL + "/oauth/callback",
		"return_url":    "https://org.example.com/auth/callback",
	}
	for k, v := range want {
		if got := relayForm.Get(k); got != v {
			t.Errorf("token form %s = %q, want %q", k, got, v)
		}
	}
	if _, has := relayForm["client_secret"]; has {
		t.Error("the public client sent a client_secret to the relay")
	}
	var sess *http.Cookie
	for _, c := range cbRR.Result().Cookies() {
		if c.Name == CookieName {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("session cookie not set")
	}
}

// An own client still sends its secret, and now the PKCE verifier too,
// to the provider's token endpoint with its own redirect URI.
func TestOwnClientSendsSecretAndVerifier(t *testing.T) {
	var form url.Values
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	}))
	defer tokenSrv.Close()
	userSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"email": "alice@example.com", "verified_email": true})
	}))
	defer userSrv.Close()

	g, codec := newOAuth(t, []string{"alice@example.com"})
	g.TokenURL, g.UserInfoURL = tokenSrv.URL, userSrv.URL
	loginRR := loginOn(g, "example.com", "/", nil)
	claim := stateClaimOf(t, codec, loginRR)
	u, _ := url.Parse(loginRR.Header().Get("location"))
	if got := u.Query().Get("redirect_uri"); got != "http://example.com/auth/callback" {
		t.Errorf("redirect_uri = %q, want the configured one", got)
	}
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+url.QueryEscape(u.Query().Get("state")), nil)
	cbReq.AddCookie(stateCookieOf(loginRR))
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	if cbRR.Code != http.StatusFound {
		t.Fatalf("code = %d, body = %s", cbRR.Code, cbRR.Body.String())
	}
	if form.Get("client_secret") != "secret" || form.Get("code_verifier") != claim.Verifier || form.Get("redirect_uri") != "http://example.com/auth/callback" {
		t.Errorf("token form = %v", form)
	}
}

// A state whose return part was rewritten between the provider and
// this install is rejected even with the right nonce: the relay must
// pass state through unchanged.
func TestCallbackRejectsRewrittenReturn(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	loginRR := loginOn(g, "org.example.com", "/", nil)
	claim := stateClaimOf(t, codec, loginRR)
	tampered := encodeState(claim.Nonce, "https://evil.example/auth/callback")
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+url.QueryEscape(tampered), nil)
	cbReq.AddCookie(stateCookieOf(loginRR))
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	if cbRR.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", cbRR.Code)
	}
}

// A provider error bounced to the callback is reported in words, and the
// state cookie is cleared like on every other exit.
func TestCallbackReportsProviderError(t *testing.T) {
	g, _ := newPublicOAuth(t, "https://relay.example/oauth")
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?error=access_denied&state=x", nil)
	rr := httptest.NewRecorder()
	g.CallbackHandler(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "access_denied") {
		t.Errorf("body = %q, want the provider's error named", rr.Body.String())
	}
	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == stateCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("state cookie not cleared")
	}
}

// An explicit TokenURL wins over the relay's token route, so the public
// client can be pointed at another relay or server for the exchange.
func TestPublicTokenURLOverride(t *testing.T) {
	g, _ := newPublicOAuth(t, "https://relay.example/oauth")
	if got := g.tokenURL(); got != "https://relay.example/oauth/token" {
		t.Errorf("tokenURL() = %q, want the relay's token route", got)
	}
	g.TokenURL = "https://other.example/token"
	if got := g.tokenURL(); got != "https://other.example/token" {
		t.Errorf("tokenURL() = %q, want the explicit override", got)
	}
}
