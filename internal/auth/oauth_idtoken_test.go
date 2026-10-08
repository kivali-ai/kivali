package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// idTokenIssuer is a fake provider: one RSA key published as a JWKS
// document, and a mint function that signs id_tokens with it.
type idTokenIssuer struct {
	key  *rsa.PrivateKey
	kid  string
	jwks *httptest.Server
}

func newIDTokenIssuer(t *testing.T) *idTokenIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	iss := &idTokenIssuer{key: key, kid: "kid-1"}
	iss.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": iss.kid, "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	t.Cleanup(iss.jwks.Close)
	return iss
}

// mint signs claims as an RS256 id_token. Callers pass the claims they
// want; the defaults are a valid Google-shaped token for client "cid".
func (i *idTokenIssuer) mint(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func validClaims(nonce string) map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": "cid", "exp": time.Now().Add(time.Hour).Unix(),
		"nonce": nonce, "email": "Alice@example.com", "email_verified": true,
	}
}

// publicSignIn runs login through callback against a relay whose token
// response is whatever respond returns, with userinfo wired to fail the
// test if it is ever called: the public client must take the identity
// from the id_token alone.
func publicSignIn(t *testing.T, iss *idTokenIssuer, respond func(nonce string) any) *httptest.ResponseRecorder {
	t.Helper()
	var g *GoogleOAuth
	var codec *Codec
	var nonce string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		body := respond(nonce)
		if code, ok := body.(int); ok {
			http.Error(w, "relay says no", code)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(relay.Close)
	userinfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the public client must not fall back to userinfo")
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(userinfo.Close)

	g, codec = newPublicOAuth(t, relay.URL+"/oauth")
	g.JWKSURL = iss.jwks.URL
	g.UserInfoURL = userinfo.URL
	loginRR := loginOn(g, "org.example.com", "/home", nil)
	claim := stateClaimOf(t, codec, loginRR)
	nonce = claim.Nonce
	u, _ := url.Parse(loginRR.Header().Get("location"))
	if got := u.Query().Get("nonce"); got != nonce {
		t.Fatalf("auth request nonce = %q, want the cookie's nonce %q", got, nonce)
	}
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+url.QueryEscape(u.Query().Get("state")), nil)
	cbReq.Host = "org.example.com"
	cbReq.AddCookie(stateCookieOf(loginRR))
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	return cbRR
}

func hasSession(rr *httptest.ResponseRecorder) bool {
	for _, c := range rr.Result().Cookies() {
		if c.Name == CookieName && c.Value != "" {
			return true
		}
	}
	return false
}

// A valid id_token from the provider, relayed verbatim, signs the
// person in without any userinfo call.
func TestPublicSignInUsesIDToken(t *testing.T) {
	iss := newIDTokenIssuer(t)
	rr := publicSignIn(t, iss, func(nonce string) any {
		return map[string]string{"access_token": "tok", "id_token": iss.mint(t, iss.key, iss.kid, validClaims(nonce))}
	})
	if rr.Code != http.StatusFound || !hasSession(rr) {
		t.Fatalf("code = %d, session = %v, body = %s", rr.Code, hasSession(rr), rr.Body.String())
	}
}

// The relay sits on the token exchange and could answer with a token it
// saw from someone else's sign-in. That token's id_token carries that
// other sign-in's nonce, so it is rejected; an access token alone is not
// accepted at all.
func TestPublicSignInRejectsSubstitutedIdentity(t *testing.T) {
	iss := newIDTokenIssuer(t)
	cases := map[string]func(nonce string) any{
		"other sign-in's id_token": func(string) any {
			c := validClaims("someone-elses-nonce")
			c["email"] = "victim@example.com"
			return map[string]string{"access_token": "stolen", "id_token": iss.mint(t, iss.key, iss.kid, c)}
		},
		"access token only": func(string) any {
			return map[string]string{"access_token": "stolen"}
		},
		"relay error": func(string) any { return http.StatusBadRequest },
	}
	for name, respond := range cases {
		rr := publicSignIn(t, iss, respond)
		if rr.Code != http.StatusBadGateway || hasSession(rr) {
			t.Errorf("%s: code = %d, session = %v, want 502 and no session", name, rr.Code, hasSession(rr))
		}
	}
}

// Every check on the id_token is load-bearing.
func TestIDTokenChecks(t *testing.T) {
	iss := newIDTokenIssuer(t)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := map[string]func(nonce string) string{
		"wrong audience": func(n string) string {
			c := validClaims(n)
			c["aud"] = "someone-else"
			return iss.mint(t, iss.key, iss.kid, c)
		},
		"wrong issuer": func(n string) string {
			c := validClaims(n)
			c["iss"] = "https://evil.example"
			return iss.mint(t, iss.key, iss.kid, c)
		},
		"expired": func(n string) string {
			c := validClaims(n)
			c["exp"] = time.Now().Add(-time.Minute).Unix()
			return iss.mint(t, iss.key, iss.kid, c)
		},
		"unverified email": func(n string) string {
			c := validClaims(n)
			c["email_verified"] = false
			return iss.mint(t, iss.key, iss.kid, c)
		},
		"no email": func(n string) string {
			c := validClaims(n)
			delete(c, "email")
			return iss.mint(t, iss.key, iss.kid, c)
		},
		"signed by another key": func(n string) string {
			return iss.mint(t, other, iss.kid, validClaims(n))
		},
		"unknown kid": func(n string) string {
			return iss.mint(t, iss.key, "kid-9", validClaims(n))
		},
		"not a jwt": func(string) string { return "nope" },
	}
	for name, token := range cases {
		rr := publicSignIn(t, iss, func(nonce string) any {
			return map[string]string{"access_token": "tok", "id_token": token(nonce)}
		})
		if rr.Code != http.StatusBadGateway || hasSession(rr) {
			t.Errorf("%s: code = %d, session = %v, want 502 and no session", name, rr.Code, hasSession(rr))
		}
	}
	// aud as a list, email_verified as a string: accepted.
	rr := publicSignIn(t, iss, func(nonce string) any {
		c := validClaims(nonce)
		c["aud"] = []string{"other", "cid"}
		c["email_verified"] = "true"
		return map[string]string{"access_token": "tok", "id_token": iss.mint(t, iss.key, iss.kid, c)}
	})
	if rr.Code != http.StatusFound || !hasSession(rr) {
		t.Errorf("list audience + string verified: code = %d, session = %v", rr.Code, hasSession(rr))
	}
}

// An own client also takes the id_token when its provider issues one,
// and falls back to the userinfo document only when it does not.
func TestOwnClientPrefersIDToken(t *testing.T) {
	iss := newIDTokenIssuer(t)
	var nonce string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c := validClaims(nonce)
		c["email"] = "alice@example.com"
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok", "id_token": iss.mint(t, iss.key, iss.kid, c)})
	}))
	defer tokenSrv.Close()
	userinfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("userinfo called although the provider issued an id_token")
	}))
	defer userinfo.Close()
	g, codec := newOAuth(t, []string{"alice@example.com"})
	g.TokenURL, g.UserInfoURL, g.JWKSURL = tokenSrv.URL, userinfo.URL, iss.jwks.URL
	loginRR := loginOn(g, "example.com", "/", nil)
	nonce = stateClaimOf(t, codec, loginRR).Nonce
	u, _ := url.Parse(loginRR.Header().Get("location"))
	cbReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=c&state="+url.QueryEscape(u.Query().Get("state")), nil)
	cbReq.AddCookie(stateCookieOf(loginRR))
	cbRR := httptest.NewRecorder()
	g.CallbackHandler(cbRR, cbReq)
	if cbRR.Code != http.StatusFound || !hasSession(cbRR) {
		t.Fatalf("code = %d, session = %v, body = %s", cbRR.Code, hasSession(cbRR), cbRR.Body.String())
	}
}

// The derived callback uses the first X-Forwarded-Host when a proxy set
// one and it names the external address.
func TestPublicReturnURLForwardedHost(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	h := http.Header{"X-Forwarded-Host": {"org.example.com, inner.proxy"}, "X-Forwarded-Proto": {"https"}}
	rr := loginOn(g, "10.0.0.5:8080", "/", h)
	if got := stateClaimOf(t, codec, rr).Return; got != "https://org.example.com/auth/callback" {
		t.Errorf("return URL = %q", got)
	}
}

// With the public client and a configured RedirectURL, the state names
// the configured callback while the provider's redirect_uri stays the
// relay's, and the canonical-host hop applies as for an own client.
func TestPublicConfiguredRedirect(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	g.RedirectURL = "https://org.example.com/auth/callback"
	rr := loginOn(g, "localhost:8080", "/", nil)
	if loc := rr.Header().Get("location"); !hasPrefixFold(loc, "https://org.example.com/auth/login?") {
		t.Fatalf("first hop location = %q, want the configured host", loc)
	}
	rr = loginOn(g, "org.example.com", "/", nil)
	u, _ := url.Parse(rr.Header().Get("location"))
	if got := u.Query().Get("redirect_uri"); got != "https://relay.example/oauth/callback" {
		t.Errorf("redirect_uri = %q, want the relay's callback", got)
	}
	if got := stateClaimOf(t, codec, rr).Return; got != "https://org.example.com/auth/callback" {
		t.Errorf("return URL = %q, want the configured one", got)
	}
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && equalFold(s[:len(prefix)], prefix)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
