package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var handoffKey = []byte("0123456789abcdef0123456789abcdef")

// handoffAt is a Handoff on handoffKey whose clock reads *now and whose
// allowlist holds owner@example.com.
func handoffAt(t *testing.T, now *time.Time, suffix string) *Handoff {
	t.Helper()
	codec, err := NewCodec(handoffKey)
	if err != nil {
		t.Fatal(err)
	}
	al := NewAllowlist("owner@example.com")
	return &Handoff{Codec: codec, Allowlist: al, CookieSuffix: suffix, Now: func() time.Time { return *now }}
}

// redeemAt sends GET /auth/handoff?t=token to h on host.
func redeemAt(h *Handoff, host, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, HandoffPath+"?t="+token, nil)
	req.Host = host
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func mint(t *testing.T, key []byte, email string, exp time.Time) string {
	t.Helper()
	tok, err := NewHandoffToken(key, email, exp)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// signedInAs asserts a redirect to "/" and returns the session's email,
// "" when no session cookie was set.
func signedInAs(t *testing.T, rr *httptest.ResponseRecorder, h *Handoff) string {
	t.Helper()
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/" {
		t.Fatalf("answer %d to %q, want 302 to /", rr.Code, rr.Header().Get("Location"))
	}
	c := cookieNamed(rr, SessionCookieName(h.CookieSuffix))
	if c == nil {
		return ""
	}
	s, err := h.Codec.DecodeSession(c.Value)
	if err != nil {
		t.Fatalf("session cookie: %v", err)
	}
	if !c.HttpOnly || c.Path != "/" {
		t.Fatalf("session cookie %+v", c)
	}
	return s.Email
}

func TestHandoffSignsInOnce(t *testing.T) {
	now := time.Now()
	h := handoffAt(t, &now, "home")
	tok := mint(t, handoffKey, "Owner@Example.com", now.Add(HandoffTTL))
	if got := signedInAs(t, redeemAt(h, "127.0.0.1:18081", tok), h); got != "owner@example.com" {
		t.Fatalf("signed in as %q", got)
	}
	if cookieNamed(redeemAt(h, "localhost:18081", mint(t, handoffKey, "owner@example.com", now.Add(time.Minute))), CookieName) != nil {
		t.Fatal("the unsuffixed cookie was set")
	}
	// A replay is refused, even a minute later.
	now = now.Add(time.Minute)
	if got := signedInAs(t, redeemAt(h, "127.0.0.1:18081", tok), h); got != "" {
		t.Fatalf("replay signed in as %q", got)
	}
}

func TestHandoffRefusals(t *testing.T) {
	now := time.Now()
	h := handoffAt(t, &now, "")
	exp := now.Add(HandoffTTL)
	good := mint(t, handoffKey, "owner@example.com", exp)
	payload, sig, _ := strings.Cut(good, ".")
	tampered := func() string {
		body, _ := base64.RawURLEncoding.DecodeString(payload)
		var c handoffClaim
		_ = json.Unmarshal(body, &c)
		c.Email = "mallory@example.com"
		b, _ := json.Marshal(c)
		return base64.RawURLEncoding.EncodeToString(b) + "." + sig
	}()
	cases := map[string]string{
		"expired":           mint(t, handoffKey, "owner@example.com", now),
		"too far ahead":     mint(t, handoffKey, "owner@example.com", now.Add(10*time.Minute)),
		"wrong key":         mint(t, []byte("another key of 32 bytes........."), "owner@example.com", exp),
		"not allowed":       mint(t, handoffKey, "stranger@example.com", exp),
		"tampered payload":  tampered,
		"session cookie":    mustSession(t, h.Codec),
		"empty":             "",
		"no signature":      payload,
		"signature swapped": sig + "." + payload,
	}
	for name, tok := range cases {
		if got := signedInAs(t, redeemAt(h, "127.0.0.1:18081", tok), h); got != "" {
			t.Errorf("%s: signed in as %q", name, got)
		}
	}
	// Only loopback is served: anywhere else the route does not exist,
	// and the token is not spent.
	rr := redeemAt(h, "kivali.example.com", good)
	if rr.Code != http.StatusNotFound || cookieNamed(rr, CookieName) != nil {
		t.Fatalf("non-loopback: %d", rr.Code)
	}
	if got := signedInAs(t, redeemAt(h, "[::1]:18081", good), h); got != "owner@example.com" {
		t.Fatalf("after the refusals the good token signed in as %q", got)
	}
}

// TestHandoffNextAfterASignInOnly guards the handoff's next against
// becoming a way around the token: Kivali Desktop opens
// /auth/handoff?t=<token>&next=<flow> in the system browser, and the
// server's resolver may set a sign-in cookie and send the browser to a
// vendor. If it ran for a refused, replayed or non-loopback token, any
// page could hand a browser that cookie, or bounce it through Kivali
// to a vendor, without the supervisor's token. A next the resolver
// does not know must still land on "/" signed in.
func TestHandoffNextAfterASignInOnly(t *testing.T) {
	now := time.Now()
	h := handoffAt(t, &now, "")
	var asked []string
	handler := h.WithNext(func(w http.ResponseWriter, r *http.Request, next string) string {
		asked = append(asked, next)
		if next == "known" {
			http.SetCookie(w, &http.Cookie{Name: "resolved", Value: "1"})
			return "https://vendor.example/authorize?state=s"
		}
		return ""
	})
	redeem := func(host, token, next string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, HandoffPath+"?t="+token+"&next="+next, nil)
		req.Host = host
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	tok := mint(t, handoffKey, "owner@example.com", now.Add(HandoffTTL))

	rr := redeem("127.0.0.1:18081", tok, "known")
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "https://vendor.example/authorize?state=s" {
		t.Fatalf("known next: %d to %q", rr.Code, rr.Header().Get("Location"))
	}
	if cookieNamed(rr, CookieName) == nil || cookieNamed(rr, "resolved") == nil {
		t.Fatal("known next: the session or the resolver's cookie was not set")
	}
	// The same token again: refused, and the resolver is not asked.
	if rr := redeem("127.0.0.1:18081", tok, "known"); rr.Header().Get("Location") != "/" || cookieNamed(rr, "resolved") != nil {
		t.Fatalf("replay with next: to %q", rr.Header().Get("Location"))
	}
	// Not loopback: no route, no resolver.
	other := mint(t, handoffKey, "owner@example.com", now.Add(HandoffTTL))
	if rr := redeem("kivali.example.com", other, "known"); rr.Code != http.StatusNotFound {
		t.Fatalf("non-loopback with next: %d", rr.Code)
	}
	// A next the resolver does not know: signed in, to "/".
	if got := signedInAs(t, redeem("127.0.0.1:18081", other, "unknown"), h); got != "owner@example.com" {
		t.Fatalf("unknown next signed in as %q", got)
	}
	if strings.Join(asked, ",") != "known,unknown" {
		t.Fatalf("the resolver was asked for %v, want only the two signed-in requests", asked)
	}
}

// mustSession is a valid session cookie value: never a handoff token.
func mustSession(t *testing.T, c *Codec) string {
	t.Helper()
	s, err := c.EncodeSession(NewSession("owner@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewHandoffTokenRefusesAShortKeyAndNoEmail(t *testing.T) {
	if _, err := NewHandoffToken([]byte("short"), "owner@example.com", time.Now()); err == nil {
		t.Error("short key accepted")
	}
	if _, err := NewHandoffToken(handoffKey, " ", time.Now()); err == nil {
		t.Error("empty email accepted")
	}
}
