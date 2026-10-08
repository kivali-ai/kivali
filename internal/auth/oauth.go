package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// stateCookieName is the state cookie's name without a cookie
	// suffix; StateCookieName gives it for any suffix.
	stateCookieName   = "kivali_oauth_state"
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
	googleJWKSURL     = "https://www.googleapis.com/oauth2/v3/certs"
	stateTTL          = 10 * time.Minute
)

// googleIssuers are the two issuer strings Google puts in id_tokens.
var googleIssuers = []string{"https://accounts.google.com", "accounts.google.com"}

// GoogleOAuth implements the OAuth 2.0 authorization-code flow with
// PKCE against Google, without an external SDK. It writes and verifies an
// HMAC-signed state cookie that carries the CSRF nonce, the PKCE
// verifier and where to return.
//
// Two clients can sign people in:
//
//   - This deployment's own client, when ClientSecret is set. The code
//     exchange goes straight to the provider with the secret, and
//     RedirectURL has to be the redirect URI registered on that client.
//   - The public client Kivali ships with, when ClientSecret is empty.
//     The browser returns to the relay at RelayURL, which bounces it on
//     to this install, and the code exchange goes through the relay,
//     which adds the secret it alone holds. The relay hands the browser
//     a sealed code bound to the callback it delivered it to, and PKCE
//     binds it to the sign-in that started it, so the relay can serve
//     every install with one client.
type GoogleOAuth struct {
	// ClientID identifies the OAuth client at the provider: an
	// operator's own, or the public one Kivali ships with.
	ClientID string
	// ClientSecret is set only for an operator's own client. Empty
	// selects the public client and the relay.
	ClientSecret string
	// RedirectURL is this install's /auth/callback as the provider must
	// reach it. Required for an own client. Optional for the public
	// client, whose registered redirect URI is the relay's: when set it
	// pins the callback; when empty, the callback is derived from each
	// sign-in request, but only for an allowed host (loopback or
	// ExternalURL's), never for whatever host a request names.
	RedirectURL string
	// ExternalURL is KIVALI_EXTERNAL_URL: the https origin other
	// computers reach this install at (a reverse proxy, Tailscale), or
	// empty. With the public client and no RedirectURL, its host is the
	// one name besides loopback a sign-in's callback is derived for.
	ExternalURL string
	// RelayURL is the relay's base for the public client. <RelayURL>/callback
	// is the redirect URI registered on that client; <RelayURL>/token is
	// where the code exchange goes. Unused with an own client.
	RelayURL   string
	Codec      *Codec
	Allowlist  *Allowlist
	HTTPClient *http.Client

	// AuthURL, TokenURL and UserInfoURL default to Google's endpoints.
	// Set them to sign in against another OAuth 2.0 server whose
	// userinfo document has Google's shape (email, verified_email).
	// With the public client, TokenURL defaults to the relay's token
	// route instead.
	AuthURL     string
	TokenURL    string
	UserInfoURL string
	// JWKSURL and Issuer are where id_tokens are checked against:
	// Google's signing keys and issuers by default. An id_token, when
	// the token response carries one, is what names the person; it is
	// signed by the provider, so nothing between the provider and this
	// install (the relay included) can substitute an identity. The
	// public client requires one; an own client falls back to the
	// userinfo document when the server issues none.
	JWKSURL string
	Issuer  string

	// CookieSuffix is KIVALI_COOKIE_SUFFIX: the state and session
	// cookies are named StateCookieName(CookieSuffix) and
	// SessionCookieName(CookieSuffix). It must match the Middleware's
	// that reads the session.
	CookieSuffix string

	jwksMu   sync.Mutex
	jwksKeys map[string]*rsa.PublicKey
}

// Ready reports whether sign-in can run: a client id, plus either the
// client's secret or the relay that holds it. Safe on a nil receiver,
// which is what a server started without sign-in holds.
func (g *GoogleOAuth) Ready() bool {
	return g != nil && g.ClientID != "" && (g.ClientSecret != "" || g.RelayURL != "")
}

// public reports whether this is the public client, whose code exchange
// goes through the relay.
func (g *GoogleOAuth) public() bool { return g.ClientSecret == "" }

// LoginHandler redirects the browser to the provider's consent page.
func (g *GoogleOAuth) LoginHandler(w http.ResponseWriter, r *http.Request) {
	// Only a path on this site comes back after sign-in: the callback
	// redirects to it verbatim, and a browser reads a leading "//" or
	// "/\" as another host.
	next := r.URL.Query().Get("next")
	if !safeNext(next) {
		next = "/"
	}
	// The desktop app's webview starts the sign-in but the provider
	// leg runs in the system browser, which brings Google's answer back
	// to a listener the app runs on loopback (docs/developers/auth.md, Desktop
	// sign-in). client=desktop marks the sign-in, and the listener's
	// port and token ride in the nonce so the browser side, which has
	// no cookie, knows where to send the browser.
	desktop := r.URL.Query().Get("client") == "desktop"
	returnPort, returnToken := r.URL.Query().Get("return_port"), r.URL.Query().Get("return_token")
	if desktop && (!validLoopbackPort(returnPort) || !validReturnToken(returnToken)) {
		http.Error(w, "Sign-in could not be started because the desktop app did not say where to return to.", http.StatusBadRequest)
		return
	}
	// The state cookie is host-only, and the browser comes back to the
	// configured redirect host. Sign-in started on any other host
	// (localhost vs 127.0.0.1) would set the cookie where the callback
	// never sees it, so first move the browser to the configured host.
	// Scheme and host come only from configuration, never the request.
	// With no configured RedirectURL the callback is derived from the
	// (allowed) host the request names, so there is nothing to hop to.
	if base := g.canonicalBase(); base != nil && !strings.EqualFold(effectiveHost(r), base.Host) {
		q := url.Values{"next": {next}}
		if desktop {
			q.Set("client", "desktop")
			q.Set("return_port", returnPort)
			q.Set("return_token", returnToken)
		}
		dest := base.Scheme + "://" + base.Host + "/auth/login?" + q.Encode()
		http.Redirect(w, r, dest, http.StatusFound)
		return
	}
	returnURL := g.returnURL(r)
	if returnURL == "" && g.public() {
		http.Error(w, g.hostRefusal(r), http.StatusForbidden)
		return
	}
	if returnURL == "" {
		http.Error(w, "Sign-in is not configured: this server has no redirect URL for its own client.", http.StatusInternalServerError)
		return
	}
	nonce, err := randomString(32)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if desktop {
		nonce = desktopNoncePrefix + returnPort + "~" + returnToken + "~" + nonce
	}
	// 32 random bytes encode to 43 unreserved characters, the shortest
	// verifier PKCE allows.
	verifier, err := randomString(32)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	claim := stateClaim{Nonce: nonce, Next: next, Exp: time.Now().UTC().Add(stateTTL), Verifier: verifier, Return: returnURL, Desktop: desktop}
	signed := g.Codec.Sign(packState(claim))
	http.SetCookie(w, &http.Cookie{
		Name:     g.stateCookieName(),
		Value:    signed,
		Path:     "/",
		HttpOnly: true,
		Secure:   !isLocal(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateTTL.Seconds()),
	})

	q := url.Values{}
	q.Set("client_id", g.ClientID)
	q.Set("redirect_uri", g.providerRedirectURI(returnURL))
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", encodeState(nonce, returnURL))
	q.Set("nonce", nonce)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("access_type", "online")
	q.Set("prompt", "select_account")

	http.Redirect(w, r, g.authURL()+"?"+q.Encode(), http.StatusFound)
}

// CallbackHandler handles the OAuth2 redirect back to this install,
// directly from the provider for an own client, or bounced by the relay
// for the public client.
func (g *GoogleOAuth) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	// Clear the state cookie eagerly — every exit path, success or
	// failure, must invalidate the cookie. Setting it BEFORE any
	// http.Error call is load-bearing: http.Error invokes
	// WriteHeader immediately, after which Set-Cookie additions
	// are silently dropped. (defer http.SetCookie does NOT work
	// here for the same reason: deferred SetCookie runs after
	// WriteHeader has already shipped headers.) Without this, a
	// failure-path early exit leaves a stale state cookie alive
	// for stateTTL — narrow window where a leaked state cookie +
	// stolen authorization code could be replayed against a
	// re-issued state.
	http.SetCookie(w, &http.Cookie{Name: g.stateCookieName(), Value: "", Path: "/", MaxAge: -1})

	retry := g.retryHint()
	incomingState := r.URL.Query().Get("state")
	// The system browser finishing a desktop sign-in has no cookie for
	// it by design: that sign-in's cookie is in the app's webview. It
	// may hold a cookie of its own, from an ordinary sign-in it started
	// and abandoned; that is not this sign-in's either. The provider's
	// whole answer, code or error, goes to the app's loopback listener,
	// and the app's webview then runs everything below with the cookie
	// that started it. Only a cookie from a desktop sign-in, which only
	// a webview sets, is taken as the webview's.
	answered := r.URL.Query().Get("code") != "" || r.URL.Query().Get("error") != ""
	if ret, ok := desktopReturnURL(incomingState); ok && answered && !g.hasDesktopCookie(r) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, ret+"?"+r.URL.RawQuery, http.StatusFound)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		// The provider (or the relay, passing it on) declined. The
		// value is reflected into a text/plain body only.
		if len(e) > 64 {
			e = e[:64]
		}
		http.Error(w, "Sign-in was not completed: the sign-in provider reported "+e+". "+retry, http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || incomingState == "" {
		http.Error(w, "Sign-in could not be completed because the reply from Google was incomplete. "+retry, http.StatusBadRequest)
		return
	}
	stateCookie, err := r.Cookie(g.stateCookieName())
	if err != nil {
		http.Error(w, "Sign-in could not be completed because the browser did not send the sign-in cookie. "+retry, http.StatusBadRequest)
		return
	}
	packed, err := g.Codec.Verify(stateCookie.Value)
	if err != nil {
		http.Error(w, "Sign-in could not be completed because the sign-in cookie was not valid. "+retry, http.StatusBadRequest)
		return
	}
	claim, err := unpackState(packed)
	if err != nil {
		http.Error(w, "Sign-in could not be completed because the sign-in cookie could not be read. "+retry, http.StatusBadRequest)
		return
	}
	if time.Now().After(claim.Exp) {
		http.Error(w, "Sign-in could not be completed because it took too long. "+retry, http.StatusBadRequest)
		return
	}
	// The whole state round-trips: the nonce proves this browser
	// started the sign-in, and the return part proves nothing between
	// the provider and this install rewrote where it was sent.
	if incomingState != encodeState(claim.Nonce, claim.Return) {
		http.Error(w, "Sign-in could not be completed because the reply from Google did not match this browser's sign-in. "+retry, http.StatusBadRequest)
		return
	}
	// The cookie is this install's own, but it may predate a change of
	// the addresses sign-in is allowed at (an upgrade, a removed
	// external address); a callback this install would not derive now
	// is not redeemed.
	if !g.returnAllowed(claim.Return) {
		http.Error(w, "Sign-in could not be completed because it was started at an address this team no longer accepts sign-ins at. "+retry, http.StatusBadRequest)
		return
	}

	tok, err := g.exchangeCode(r.Context(), code, claim.Verifier, claim.Return)
	if err != nil {
		log.Printf("oauth callback: token exchange: %v", err)
		http.Error(w, "Sign-in could not be completed because Google did not confirm it. "+retry, http.StatusBadGateway)
		return
	}
	email, err := g.identity(r.Context(), tok, claim.Nonce)
	if err != nil {
		log.Printf("oauth callback: identity: %v", err)
		http.Error(w, "Sign-in could not be completed because Google did not share a verified email address for this account. "+retry, http.StatusBadGateway)
		return
	}
	if !g.Allowlist.Allow(email) {
		// The desktop app's webview watches where it is sent and cannot
		// read a status code, so its sign-in lands on a page of its own.
		// The email stays out of the URL.
		if claim.Desktop {
			setDeniedCookie(w, r, g.CookieSuffix, email)
			http.Redirect(w, r, NotInvitedPath, http.StatusFound)
			return
		}
		http.Error(w, "The Google account "+email+" "+notInvitedSentence, http.StatusForbidden)
		return
	}
	signed, err := g.Codec.EncodeSession(NewSession(email))
	if err != nil {
		log.Printf("oauth callback: session encode: %v", err)
		http.Error(w, "Sign-in could not be completed because of a problem on the server. "+retry, http.StatusInternalServerError)
		return
	}
	g.setSession(w, r, signed)
	http.Redirect(w, r, claim.Next, http.StatusFound)
}

func (g *GoogleOAuth) setSession(w http.ResponseWriter, r *http.Request, signed string) {
	setSessionCookie(w, r, g.CookieSuffix, signed)
}

// setSessionCookie sets the session cookie a successful sign-in, by
// Google or by a desktop handoff, leaves.
func setSessionCookie(w http.ResponseWriter, r *http.Request, suffix, signed string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName(suffix),
		Value:    signed,
		Path:     "/",
		HttpOnly: true,
		Secure:   !isLocal(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL.Seconds()),
	})
}

// stateCookieName is the state cookie this sign-in sets and reads.
func (g *GoogleOAuth) stateCookieName() string { return StateCookieName(g.CookieSuffix) }

// StateCookieName is the sign-in state cookie's name for a server whose
// cookies carry suffix (KIVALI_COOKIE_SUFFIX): kivali_oauth_state when
// suffix is empty, else kivali_oauth_state_<suffix>.
func StateCookieName(suffix string) string { return withCookieSuffix(stateCookieName, suffix) }

// NotInvitedPath is where a desktop sign-in by an account the allowlist
// refuses is sent. The desktop app recognises the path.
const NotInvitedPath = "/auth/not-invited"

// notInvitedSentence ends the refusal for an account the allowlist does
// not admit, after "The Google account <email>" or "This Google account".
const notInvitedSentence = "is not this team's owner. Only the owner can sign in."

// NotInvitedHandler serves NotInvitedPath: the refusal a browser
// sign-in gets as text, as a minimal page with status 403. It needs no
// session and names no account.
func NotInvitedHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Can't sign in</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;line-height:1.5}</style>
</head><body><p>This Google account `+notInvitedSentence+`</p></body></html>
`)
}

// LogoutHandler clears the session cookie and redirects home.
func (g *GoogleOAuth) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName(g.CookieSuffix), Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusFound)
}

// safeNext accepts only an on-site path for next: it must start with a
// single "/", and it must carry no control character, because browsers
// drop tabs and newlines while parsing, so "/\t/evil" would read as
// "//evil" there while passing the prefix checks here.
func safeNext(next string) bool {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return false
	}
	for i := 0; i < len(next); i++ {
		if next[i] < 0x20 || next[i] == 0x7f {
			return false
		}
	}
	return true
}

// validLoopbackPort is a decimal TCP port, 1 to 65535, as the desktop
// app reports the port of its loopback listener.
func validLoopbackPort(s string) bool {
	if s == "" || len(s) > 5 || strings.TrimLeft(s, "0123456789") != "" {
		return false
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// validReturnToken is the listener's path token: 32 random bytes as
// base64url without padding, 43 characters, so it fits a nonce and a
// URL path verbatim.
func validReturnToken(s string) bool {
	if len(s) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

// desktopReturnURL reads the app's loopback listener out of a desktop
// sign-in's state: the nonce part is desktop~<port>~<token>~<random>.
// Anything else, or a port or token of the wrong shape, is not a desktop
// sign-in. The address is 127.0.0.1 by construction: a desktop sign-in
// never names any other host.
func desktopReturnURL(state string) (string, bool) {
	nonce, _, ok := strings.Cut(state, ".")
	if !ok || !strings.HasPrefix(nonce, desktopNoncePrefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(nonce, desktopNoncePrefix), "~")
	if len(parts) != 3 || !validLoopbackPort(parts[0]) || !validReturnToken(parts[1]) {
		return "", false
	}
	return "http://127.0.0.1:" + parts[0] + "/signin/" + parts[1], true
}

// hasDesktopCookie reports whether the request carries a valid state
// cookie from a desktop sign-in: the mark only a webview's cookie has.
func (g *GoogleOAuth) hasDesktopCookie(r *http.Request) bool {
	c, err := r.Cookie(g.stateCookieName())
	if err != nil {
		return false
	}
	packed, err := g.Codec.Verify(c.Value)
	if err != nil {
		return false
	}
	claim, err := unpackState(packed)
	return err == nil && claim.Desktop
}

// tokenResponse is what the token endpoint (or the relay) returned. The
// relay answers with the id_token alone ({id_token, token_type,
// expires_in}), so AccessToken is empty for the public client.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

// exchangeCode trades the authorization code for tokens. The form is the
// standard one; the differences between the two clients: this install
// supplies the secret for its own client, the relay for the public one;
// and the public client also names, in return_url, the callback the code
// was delivered to (the state cookie's), because the relay's code is
// sealed for that callback and redeemed only for it. The code is opaque
// either way: Google's own, or the relay's sealed "k1." value.
func (g *GoogleOAuth) exchangeCode(ctx context.Context, code, verifier, returnURL string) (tokenResponse, error) {
	var tr tokenResponse
	v := url.Values{}
	v.Set("code", code)
	v.Set("client_id", g.ClientID)
	if g.public() {
		v.Set("return_url", returnURL)
	} else {
		v.Set("client_secret", g.ClientSecret)
	}
	v.Set("code_verifier", verifier)
	v.Set("redirect_uri", g.providerRedirectURI(returnURL))
	v.Set("grant_type", "authorization_code")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL(), strings.NewReader(v.Encode()))
	if err != nil {
		return tr, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return tr, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return tr, fmt.Errorf("token status %d: %s", resp.StatusCode, string(b))
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		return tr, err
	}
	if tr.AccessToken == "" && tr.IDToken == "" {
		return tr, errors.New("empty token response")
	}
	return tr, nil
}

// identity names the person who signed in. An id_token, when present,
// is verified against the provider's keys and the sign-in's nonce and
// is the only source the public client accepts: the relay saw the
// token response and could have substituted an access token, but it
// cannot forge the provider's signature. An own client whose provider
// issued no id_token falls back to the userinfo document, which it
// fetched from the provider directly.
func (g *GoogleOAuth) identity(ctx context.Context, tok tokenResponse, nonce string) (string, error) {
	if tok.IDToken != "" {
		return g.emailFromIDToken(ctx, tok.IDToken, nonce)
	}
	if g.public() {
		return "", errors.New("the token response carried no id_token")
	}
	return g.fetchEmail(ctx, tok.AccessToken)
}

// idTokenClaims are the id_token fields sign-in checks or reads.
type idTokenClaims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"`
	Exp           int64           `json:"exp"`
	Nonce         string          `json:"nonce"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
}

// emailFromIDToken verifies an RS256 id_token (signature by one of the
// provider's published keys, issuer, audience, expiry and nonce) and
// returns its verified email.
func (g *GoogleOAuth) emailFromIDToken(ctx context.Context, raw, nonce string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("id_token is not a JWT")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("id_token header: %w", err)
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return "", fmt.Errorf("id_token header: %w", err)
	}
	if header.Alg != "RS256" {
		return "", fmt.Errorf("id_token alg %q, want RS256", header.Alg)
	}
	key, err := g.signingKey(ctx, header.Kid)
	if err != nil {
		return "", err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("id_token signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return "", errors.New("id_token signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("id_token payload: %w", err)
	}
	var c idTokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", fmt.Errorf("id_token payload: %w", err)
	}
	if !g.issuerOK(c.Iss) {
		return "", fmt.Errorf("id_token issuer %q not accepted", c.Iss)
	}
	if !audienceContains(c.Aud, g.ClientID) {
		return "", errors.New("id_token audience is not this client")
	}
	if c.Exp == 0 || time.Now().After(time.Unix(c.Exp, 0)) {
		return "", errors.New("id_token expired")
	}
	if c.Nonce == "" || c.Nonce != nonce {
		return "", errors.New("id_token nonce does not match this sign-in")
	}
	if c.Email == "" {
		return "", errors.New("id_token carries no email")
	}
	if !jsonTrue(c.EmailVerified) {
		return "", fmt.Errorf("email not verified: %s", c.Email)
	}
	return strings.ToLower(c.Email), nil
}

func (g *GoogleOAuth) issuerOK(iss string) bool {
	if g.Issuer != "" {
		return iss == g.Issuer
	}
	for _, want := range googleIssuers {
		if iss == want {
			return true
		}
	}
	return false
}

// audienceContains accepts aud as a string or a list of strings.
func audienceContains(raw json.RawMessage, clientID string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == clientID
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == clientID {
				return true
			}
		}
	}
	return false
}

// jsonTrue accepts a boolean true or the string "true", both of which
// providers have been seen to emit for email_verified.
func jsonTrue(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == "true"
}

// signingKey returns the provider's public key for kid, fetching the
// JWKS document when the kid is not cached. Keys rotate, so an unknown
// kid always triggers one fetch.
func (g *GoogleOAuth) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.jwksMu.Lock()
	defer g.jwksMu.Unlock()
	if k, ok := g.jwksKeys[kid]; ok {
		return k, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.jwksURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) == 0 || len(e) == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	g.jwksKeys = keys
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("id_token signed with unknown key %q", kid)
}

func (g *GoogleOAuth) jwksURL() string {
	if g.JWKSURL != "" {
		return g.JWKSURL
	}
	return googleJWKSURL
}

func (g *GoogleOAuth) fetchEmail(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.userInfoURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("authorization", "Bearer "+token)
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("userinfo status %d: %s", resp.StatusCode, string(b))
	}
	var ur struct {
		Email         string `json:"email"`
		VerifiedEmail bool   `json:"verified_email"`
	}
	if err := json.Unmarshal(b, &ur); err != nil {
		return "", err
	}
	if ur.Email == "" {
		return "", errors.New("empty email")
	}
	if !ur.VerifiedEmail {
		return "", fmt.Errorf("email not verified: %s", ur.Email)
	}
	return strings.ToLower(ur.Email), nil
}

func (g *GoogleOAuth) httpClient() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *GoogleOAuth) authURL() string {
	if g.AuthURL != "" {
		return g.AuthURL
	}
	return googleAuthURL
}

// tokenURL is where the code exchange goes: an explicit TokenURL, else
// the relay's token route for the public client, else Google's.
func (g *GoogleOAuth) tokenURL() string {
	if g.TokenURL != "" {
		return g.TokenURL
	}
	if g.public() {
		return g.relayRoute("token")
	}
	return googleTokenURL
}

func (g *GoogleOAuth) userInfoURL() string {
	if g.UserInfoURL != "" {
		return g.UserInfoURL
	}
	return googleUserInfoURL
}

// relayRoute joins RelayURL and one of its two routes.
func (g *GoogleOAuth) relayRoute(name string) string {
	return strings.TrimRight(g.RelayURL, "/") + "/" + name
}

// returnURL is this install's callback for one sign-in: the configured
// RedirectURL, else, for the public client only, one derived from an
// allowed host (derivedReturnURL). An own client's redirect must be the
// one registered on it, so with none configured the answer is empty and
// sign-in refuses; the public client's is empty for a host that is not
// allowed.
func (g *GoogleOAuth) returnURL(r *http.Request) string {
	if g.RedirectURL != "" {
		return g.RedirectURL
	}
	if !g.public() {
		return ""
	}
	return g.derivedReturnURL(r)
}

// derivedReturnURL is the public client's callback for a request, taken
// from an allowed host only: loopback (127.0.0.1, localhost, [::1], any
// port), or ExternalURL's host. The host is the first X-Forwarded-Host
// value if that is allowed, else r.Host if that is allowed, else there
// is none (""). Both are checked because either can be anything a client
// sent: a proxy that appends passes the client's own X-Forwarded-Host
// first, and an exposed install can be reached directly with any Host.
//
// Why that is enough: the relay seals Google's code for the callback in
// state and redeems it only for the callback this install names in
// return_url, which is the one in the signed state cookie. A callback
// derived from an attacker's host would let an attacker hold a cookie
// whose callback is their own server, lure a victim's sealed code there
// and redeem it here. A loopback callback sends the victim's browser to
// the victim's own machine, and the external one to this install's own
// address, so neither delivers a code to anyone but its owner, whatever
// header picked it.
//
// The external name's callback is always https (KIVALI_EXTERNAL_URL is
// an https origin). Loopback's uses the connection's own scheme;
// X-Forwarded-Proto is never read.
func (g *GoogleOAuth) derivedReturnURL(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		if u := g.callbackForHost(strings.TrimSpace(strings.Split(fwd, ",")[0]), r); u != "" {
			return u
		}
	}
	return g.callbackForHost(r.Host, r)
}

// callbackForHost is the callback for host when host is allowed, else "".
func (g *GoogleOAuth) callbackForHost(host string, r *http.Request) string {
	if ext := g.externalHost(); ext != "" && strings.EqualFold(withoutHTTPSPort(host), ext) {
		return "https://" + ext + "/auth/callback"
	}
	if lb := loopbackHost(host); lb != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return scheme + "://" + lb + "/auth/callback"
	}
	return ""
}

// returnAllowed reports whether a state cookie's callback is one this
// install would hand out now: always for an own client (its callback is
// its configuration); for the public client, the configured RedirectURL
// when set, else a loopback callback or the external name's.
func (g *GoogleOAuth) returnAllowed(ret string) bool {
	if !g.public() {
		return true
	}
	if g.RedirectURL != "" {
		return ret == g.RedirectURL
	}
	u, err := url.Parse(ret)
	if err != nil || u.Path != "/auth/callback" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	if ext := g.externalHost(); ext != "" && u.Scheme == "https" && strings.EqualFold(u.Host, ext) {
		return true
	}
	return (u.Scheme == "http" || u.Scheme == "https") && loopbackHost(u.Host) != ""
}

// externalHost is ExternalURL's host (and port, unless it is 443),
// lower-cased; empty when ExternalURL is unset or not an https origin.
func (g *GoogleOAuth) externalHost() string {
	if g.ExternalURL == "" {
		return ""
	}
	u, err := url.Parse(g.ExternalURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return ""
	}
	return strings.ToLower(withoutHTTPSPort(u.Host))
}

// withoutHTTPSPort drops an explicit :443, so "host" and "host:443"
// name the same https address.
func withoutHTTPSPort(host string) string {
	return strings.TrimSuffix(host, ":443")
}

// loopbackHost is host in canonical form ("127.0.0.1", "localhost" or
// "[::1]", with its port if it has one) when it names loopback the way
// the relay accepts an http callback, else "". Other 127/8 addresses are
// not taken: the relay would refuse their plain-http callback.
func loopbackHost(host string) string {
	name, port := host, ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		if !validLoopbackPort(p) {
			return ""
		}
		name, port = h, p
	}
	switch strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]")) {
	case "127.0.0.1":
		name = "127.0.0.1"
	case "localhost":
		name = "localhost"
	case "::1":
		name = "[::1]"
	default:
		return ""
	}
	if port == "" {
		return name
	}
	return name + ":" + port
}

// hostRefusal is the public client's answer to a sign-in started at a
// host it derives no callback for. The host is the request's own words
// in a text/plain body, shortened.
func (g *GoogleOAuth) hostRefusal(r *http.Request) string {
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if len(host) > 100 {
		host = host[:100]
	}
	external := "its external address"
	if ext := g.externalHost(); ext != "" {
		external = "its external address https://" + ext
	}
	return "Sign-in isn't available at " + host + ": this team only accepts sign-ins at 127.0.0.1, localhost and " + external +
		" (KIVALI_EXTERNAL_URL, or \"Let other computers connect\" in Kivali Desktop's Other devices settings)."
}

// providerRedirectURI is the redirect_uri sent to the provider and
// repeated in the code exchange: the relay's callback for the public
// client, this install's own otherwise.
func (g *GoogleOAuth) providerRedirectURI(returnURL string) string {
	if g.public() {
		return g.relayRoute("callback")
	}
	return returnURL
}

// encodeState is the state parameter: "<nonce>.<base64url(return URL)>".
// The relay splits on the first "." to learn where to send the browser
// (the nonce is base64url and so never contains one); the install
// compares the whole value with what it packed into its cookie.
func encodeState(nonce, returnURL string) string {
	return nonce + "." + base64.RawURLEncoding.EncodeToString([]byte(returnURL))
}

// pkceChallenge is the S256 code challenge for a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// stateClaim is what the signed state cookie carries between the login
// redirect and the callback.
type stateClaim struct {
	// Nonce is the CSRF token the provider echoes back in state.
	Nonce string `json:"nonce"`
	// Next is the on-site path to land on after sign-in.
	Next string    `json:"next"`
	Exp  time.Time `json:"exp"`
	// Verifier is the PKCE code verifier; its S256 hash went to the
	// provider, and the verifier itself goes with the code exchange.
	Verifier string `json:"verifier"`
	// Return is this install's callback URL as it was put into state.
	Return string `json:"return"`
	// Desktop marks a sign-in started from the desktop app's webview,
	// whose provider leg runs in the system browser (docs/developers/auth.md,
	// Desktop sign-in). The nonce carries the same mark, so the browser
	// side, which has no cookie for it, can tell; and a cookie without
	// this mark is a browser's own, never a webview's.
	Desktop bool `json:"desktop,omitempty"`
}

// desktopNoncePrefix marks a desktop sign-in's nonce. The tilde is not a
// base64url character, so a random nonce never starts this way, and it
// is not a dot, so the relay still splits state on the first dot.
const desktopNoncePrefix = "desktop~"

// packState encodes the claim into an opaque byte slice for signing.
func packState(c stateClaim) []byte {
	b, _ := json.Marshal(c)
	return b
}

func unpackState(raw []byte) (stateClaim, error) {
	var c stateClaim
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if !strings.HasPrefix(c.Next, "/") {
		c.Next = "/"
	}
	return c, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// canonicalBase is the scheme and host of the configured RedirectURL:
// the one host the state cookie can round-trip through. Nil when the
// RedirectURL does not parse or names no host, which skips the hop.
func (g *GoogleOAuth) canonicalBase() *url.URL {
	u, err := url.Parse(g.RedirectURL)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return nil
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}
}

// retryHint is the sentence a failed callback ends with: where to
// start sign-in again.
func (g *GoogleOAuth) retryHint() string {
	if base := g.canonicalBase(); base != nil {
		return "Open " + base.String() + " and try again."
	}
	return "Open the app again and try again."
}

// effectiveHost is the host the browser addressed: the first
// X-Forwarded-Host value when a reverse proxy set one, else r.Host.
// It mirrors requestHost in internal/web. It only decides whether to hop
// to the configured RedirectURL's host, whose address comes from
// configuration alone; no callback is ever built from it.
func effectiveHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return r.Host
}

// isLocal reports whether the request addressed a loopback host, by its
// Host header: it drops Secure from the cookies and gates the handoff.
// RemoteAddr cannot decide it, because in the desktop VM every request
// arrives from the in-cluster address of the supervisor's forward. A
// forged loopback Host gains nothing: the cookie it unlocks is the
// sender's own, and a handoff token still needs SESSION_KEY.
func isLocal(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
