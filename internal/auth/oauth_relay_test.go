package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// sealedCode is the shape of the code the relay hands the browser in
// place of Google's: "k1." plus base64url. The install treats it as
// opaque.
const sealedCode = "k1.q83vEjRWeJAbzd7v-_8AESIzRFVmd4iZqrvM3e7_ABEiM0RVZneImaq7zN3u_w"

// relayIDTokenOnly is a fake relay whose token route answers exactly as
// the relay does on success: {id_token, token_type, expires_in}, no
// access_token. It records the form it was sent.
func relayIDTokenOnly(t *testing.T, iss *idTokenIssuer, nonce *string, form *url.Values) *httptest.Server {
	t.Helper()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		*form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id_token":"` + iss.mint(t, iss.key, iss.kid, validClaims(*nonce)) + `","token_type":"Bearer","expires_in":3599}`))
	}))
	t.Cleanup(relay.Close)
	return relay
}

// The public client signs in on the relay's id_token-only answer,
// sends the sealed code unchanged, and names in return_url the callback
// its state cookie recorded, for loopback and for the external address.
func TestPublicSignInOnRelayIDTokenOnly(t *testing.T) {
	for _, tc := range []struct{ host, wantReturn string }{
		{"127.0.0.1:8080", "http://127.0.0.1:8080/auth/callback"},
		{"org.example.com", "https://org.example.com/auth/callback"},
	} {
		iss := newIDTokenIssuer(t)
		var nonce string
		var form url.Values
		relay := relayIDTokenOnly(t, iss, &nonce, &form)
		g, codec := newPublicOAuth(t, relay.URL+"/oauth")
		g.JWKSURL = iss.jwks.URL
		g.UserInfoURL = "http://127.0.0.1:1/never"

		loginRR := loginOn(g, tc.host, "/home", nil)
		claim := stateClaimOf(t, codec, loginRR)
		nonce = claim.Nonce
		if claim.Return != tc.wantReturn {
			t.Fatalf("%s: return = %q, want %q", tc.host, claim.Return, tc.wantReturn)
		}
		u, _ := url.Parse(loginRR.Header().Get("location"))
		rr := callback(g, tc.host, "code="+url.QueryEscape(sealedCode)+"&state="+url.QueryEscape(u.Query().Get("state")), stateCookieOf(loginRR))
		if rr.Code != http.StatusFound || rr.Header().Get("location") != "/home" || !hasSession(rr) {
			t.Fatalf("%s: code = %d, location = %q, session = %v, body = %s", tc.host, rr.Code, rr.Header().Get("location"), hasSession(rr), rr.Body.String())
		}
		if got := form.Get("code"); got != sealedCode {
			t.Errorf("%s: code sent = %q, want the sealed code unchanged", tc.host, got)
		}
		if got := form.Get("return_url"); got != tc.wantReturn {
			t.Errorf("%s: return_url = %q, want the state cookie's %q", tc.host, got, tc.wantReturn)
		}
	}
}

// exchangeCode accepts the id_token-only shape and names return_url;
// an own client sends no return_url (the relay is not in its path).
func TestExchangeCodeReturnURL(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": "a.b.c", "token_type": "Bearer", "expires_in": 3599})
	}))
	defer srv.Close()

	g, _ := newPublicOAuth(t, srv.URL)
	tok, err := g.exchangeCode(t.Context(), sealedCode, "verifier", "http://localhost:9000/auth/callback")
	if err != nil || tok.IDToken != "a.b.c" || tok.AccessToken != "" {
		t.Fatalf("public exchange = %+v, %v", tok, err)
	}
	if form.Get("return_url") != "http://localhost:9000/auth/callback" || form.Get("code") != sealedCode {
		t.Errorf("public form = %v", form)
	}

	own, _ := newOAuth(t, nil)
	own.TokenURL = srv.URL
	if _, err := own.exchangeCode(t.Context(), "c", "verifier", "http://example.com/auth/callback"); err != nil {
		t.Fatal(err)
	}
	if _, has := form["return_url"]; has {
		t.Errorf("own client sent return_url: %v", form)
	}
}

// The system browser finishing a desktop sign-in is bounced to the
// app's listener with the relay's sealed code exactly as it came.
func TestDesktopBouncePassesSealedCode(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	g.ExternalURL = ""
	loginRR := loginAs(t, g, "127.0.0.1:8080", "next=/home&"+desktopQ)
	claim := stateClaimOf(t, codec, loginRR)
	q := "state=" + url.QueryEscape(encodeState(claim.Nonce, claim.Return)) + "&code=" + sealedCode + "&scope=openid"
	rr := callback(g, "127.0.0.1:8080", q, nil)
	want := "http://127.0.0.1:54321/signin/" + returnToken + "?" + q
	if rr.Code != http.StatusFound || rr.Header().Get("location") != want {
		t.Fatalf("code = %d, location = %q, want %q", rr.Code, rr.Header().Get("location"), want)
	}
	got, _ := url.Parse(rr.Header().Get("location"))
	if got.Query().Get("code") != sealedCode {
		t.Errorf("bounced code = %q", got.Query().Get("code"))
	}
}

// The public client derives a callback only for loopback and the
// external address. Loopback works with nothing configured; any other
// Host or X-Forwarded-Host is refused, with no cookie and a message that
// names the setting; the external name works directly and forwarded.
func TestPublicCallbackHostAllowlist(t *testing.T) {
	type tc struct {
		name, host, fwd, want string
	}
	noConfig := []tc{
		{"127.0.0.1", "127.0.0.1:8080", "", "http://127.0.0.1:8080/auth/callback"},
		{"localhost", "LocalHost:9000", "", "http://localhost:9000/auth/callback"},
		{"[::1]", "[::1]:8080", "", "http://[::1]:8080/auth/callback"},
		{"no port", "127.0.0.1", "", "http://127.0.0.1/auth/callback"},
		{"unknown fwd, loopback host", "127.0.0.1:8080", "evil.example", "http://127.0.0.1:8080/auth/callback"},
		{"unknown host", "evil.example", "", ""},
		{"other loopback ip", "127.0.0.2:8080", "", ""},
		{"in-cluster ip", "10.0.0.5:8080", "", ""},
		{"unknown fwd and host", "10.0.0.5:8080", "evil.example", ""},
		{"bad port", "127.0.0.1:0", "", ""},
		{"userinfo trick", "127.0.0.1:80@evil.example", "", ""},
		{"external without config", "org.example.com", "", ""},
	}
	external := []tc{
		{"direct", "org.example.com", "", "https://org.example.com/auth/callback"},
		{"direct, case and :443", "ORG.example.com:443", "", "https://org.example.com/auth/callback"},
		{"forwarded", "10.0.0.5:8080", "org.example.com", "https://org.example.com/auth/callback"},
		{"forwarded first of several", "10.0.0.5:8080", "org.example.com, proxy.internal", "https://org.example.com/auth/callback"},
		{"client-prepended fwd falls back to host", "org.example.com", "evil.example, org.example.com", "https://org.example.com/auth/callback"},
		{"loopback still works", "localhost:8080", "", "http://localhost:8080/auth/callback"},
		{"other port", "org.example.com:8443", "", ""},
		{"lookalike", "org.example.com.evil.example", "", ""},
		{"forged fwd", "10.0.0.5:8080", "evil.example", ""},
	}
	run := func(ext string, cases []tc) {
		for _, c := range cases {
			g, codec := newPublicOAuth(t, "https://relay.example/oauth")
			g.ExternalURL = ext
			var h http.Header
			if c.fwd != "" {
				h = http.Header{"X-Forwarded-Host": {c.fwd}, "X-Forwarded-Proto": {"https"}}
			}
			rr := loginOn(g, c.host, "/", h)
			if c.want == "" {
				body := rr.Body.String()
				if rr.Code != http.StatusForbidden || stateCookieOf(rr) != nil || !strings.Contains(body, "KIVALI_EXTERNAL_URL") || !strings.Contains(body, "isn't available at") {
					t.Errorf("ext %q, %s: code = %d, cookie = %v, body = %q; want a refusal", ext, c.name, rr.Code, stateCookieOf(rr) != nil, body)
				}
				continue
			}
			if rr.Code != http.StatusFound {
				t.Errorf("ext %q, %s: code = %d, body = %s", ext, c.name, rr.Code, rr.Body.String())
				continue
			}
			if got := stateClaimOf(t, codec, rr).Return; got != c.want {
				t.Errorf("ext %q, %s: return = %q, want %q", ext, c.name, got, c.want)
			}
		}
	}
	run("", noConfig)
	run("https://org.example.com", external)
	run("https://org.example.com/", external[:1])
}

// The refusal names the host the browser addressed and, when one is
// configured, the external address to use instead.
func TestPublicRefusalNamesTheSetting(t *testing.T) {
	g, _ := newPublicOAuth(t, "https://relay.example/oauth")
	rr := loginOn(g, "kivali.internal:8080", "/", nil)
	want := "Sign-in isn't available at kivali.internal:8080: this team only accepts sign-ins at 127.0.0.1, localhost and its external address https://org.example.com (KIVALI_EXTERNAL_URL"
	if !strings.Contains(rr.Body.String(), want) {
		t.Errorf("body = %q", rr.Body.String())
	}
}

// The forged-host attack: an attacker reaches an exposed install with a
// Host or X-Forwarded-Host naming their own server, hoping for a state
// cookie whose callback is theirs. No cookie is issued, so there is
// nothing to replay a lured code with.
func TestForgedHostGetsNoCookie(t *testing.T) {
	g, _ := newPublicOAuth(t, "https://relay.example/oauth")
	for _, h := range []http.Header{
		{"X-Forwarded-Host": {"evil.example"}},
		{"X-Forwarded-Host": {"evil.example, org.example.com"}},
	} {
		rr := loginOn(g, "10.0.0.5:8080", "/", h)
		if rr.Code != http.StatusForbidden || stateCookieOf(rr) != nil {
			t.Errorf("%v: code = %d, cookie = %v", h, rr.Code, stateCookieOf(rr) != nil)
		}
	}
	if rr := loginOn(g, "evil.example", "/", nil); rr.Code != http.StatusForbidden || stateCookieOf(rr) != nil {
		t.Errorf("Host evil.example: code = %d", rr.Code)
	}
}

// A state cookie whose callback this install would not derive now (its
// external address was removed after the sign-in started) is not
// redeemed.
func TestCallbackRefusesAReturnNoLongerAllowed(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	loginRR := loginOn(g, "org.example.com", "/", nil)
	claim := stateClaimOf(t, codec, loginRR)
	g.ExternalURL = ""
	rr := callback(g, "org.example.com", "code=c&state="+url.QueryEscape(encodeState(claim.Nonce, claim.Return)), stateCookieOf(loginRR))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "no longer accepts") {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	for ret, ok := range map[string]bool{
		"http://127.0.0.1:8080/auth/callback":   true,
		"https://localhost/auth/callback":       true,
		"http://[::1]:1/auth/callback":          true,
		"http://evil.example/auth/callback":     false,
		"http://127.0.0.1:8080/auth/other":      false,
		"http://127.0.0.1:8080/auth/callback?x": false,
		"ftp://127.0.0.1/auth/callback":         false,
	} {
		if g.returnAllowed(ret) != ok {
			t.Errorf("returnAllowed(%q) = %v", ret, !ok)
		}
	}
}

// With the public client, OAUTH_REDIRECT_URL keeps pinning the callback
// (it is configuration, not a request's word), loopback included: a
// loopback sign-in hops to it as before.
func TestPublicRedirectURLStillPins(t *testing.T) {
	g, codec := newPublicOAuth(t, "https://relay.example/oauth")
	g.ExternalURL = ""
	g.RedirectURL = "https://pinned.example.com/auth/callback"
	rr := loginOn(g, "pinned.example.com", "/", http.Header{"X-Forwarded-Host": {"evil.example"}})
	if rr.Code != http.StatusFound {
		t.Fatalf("code = %d", rr.Code)
	}
	if loc := rr.Header().Get("location"); !strings.HasPrefix(loc, "https://pinned.example.com/auth/login?") {
		// X-Forwarded-Host only decides the hop; the hop goes to the
		// configured host.
		t.Fatalf("location = %q, want a hop to the configured host", loc)
	}
	rr = loginOn(g, "pinned.example.com", "/", nil)
	if got := stateClaimOf(t, codec, rr).Return; got != g.RedirectURL {
		t.Errorf("return = %q", got)
	}
}
