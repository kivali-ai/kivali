// Package connecttest is an OAuth vendor in-process, for tests of
// internal/connect and of core's credential path: FakeVendor serves
// the device authorization grant (RFC 8628), the authorization code
// grant with PKCE (S256) and state passed back, refresh with rotation
// and revocation (RFC 7009), with a knob for each failure a real
// vendor answers. Expiries are read from an injected clock, so they
// agree with the test's.
package connecttest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
)

// The client FakeVendor knows unless a test says otherwise.
const (
	ClientID     = "fake-client-id"
	ClientSecret = "fake-client-secret-value"
)

// Knobs say how FakeVendor answers. A test sets them with Set; the
// zero value answers every request as a well-behaved vendor would,
// with tokens that do not expire and no rotation.
type Knobs struct {
	// GitHubStyle answers every error with a 200 and the error object
	// (GitHub's token endpoint does), rather than RFC 6749's 400 (401
	// for a client it does not know).
	GitHubStyle bool
	// AccessTTL is the expires_in of each access token issued; zero
	// sends none, a token that does not expire.
	AccessTTL time.Duration
	// Rotate issues a new refresh token on each refresh and stops
	// accepting the old one; without it the answer carries no refresh
	// token and the old one stays good.
	Rotate bool
	// ExpiresInString sends expires_in as a quoted string, as some
	// vendors do.
	ExpiresInString bool

	// RefreshError answers each refresh with this error code
	// (invalid_grant, bad_refresh_token, temporarily_unavailable...).
	RefreshError string
	// RefreshErrorDescription, when set, is RefreshError's
	// error_description, given the refresh request's form (a vendor
	// that echoes what it was sent); "refused by the test" otherwise.
	RefreshErrorDescription func(form url.Values) string
	// RefreshStatus answers each refresh with this HTTP status and a
	// body that is not JSON (500: a vendor that is down).
	RefreshStatus int
	// RefreshMalformed answers each refresh with a 200 that is not
	// JSON.
	RefreshMalformed bool

	// DevicePending answers this many polls with
	// authorization_pending before the owner approves.
	DevicePending int
	// DeviceSlowDown answers this many polls (before the pending ones)
	// with slow_down.
	DeviceSlowDown int
	// DeviceError answers the poll after the pending ones with this
	// error code (access_denied, expired_token, incorrect_device_code,
	// device_flow_disabled...) in place of the approval.
	DeviceError string
	// DeviceExpiresIn and DeviceInterval are the device answer's
	// expires_in and interval; zero sends 900 and 5.
	DeviceExpiresIn, DeviceInterval time.Duration
	// DeviceVerificationURI and DeviceVerificationURIComplete replace
	// the device answer's pages; empty sends the vendor's own.
	DeviceVerificationURI, DeviceVerificationURIComplete string

	// RevokeStatus answers each revocation with this status; zero is
	// 200.
	RevokeStatus int

	// OnRefresh, when set, runs as a refresh request arrives, before it
	// is answered: a test holds the answer back with it to have other
	// callers arrive while the refresh is in flight.
	OnRefresh func()
	// OnRevoke, when set, runs as a revocation request arrives, before
	// it is answered.
	OnRevoke func()
}

// Request is one request the vendor took: its path, form and Accept
// header.
type Request struct {
	Path   string
	Form   url.Values
	Accept string
}

// FakeVendor is the vendor. Its endpoints are at URL + /authorize,
// /token, /device and /revoke (Endpoints).
type FakeVendor struct {
	*httptest.Server

	clk          clock.Clock
	clientID     string
	clientSecret string

	mu       sync.Mutex
	knobs    Knobs
	requests []Request
	seq      int
	// access is each access token issued, with its expiry (zero: none).
	access map[string]time.Time
	// refresh is each refresh token the vendor accepts.
	refresh map[string]bool
	revoked []string
	// codes are the authorization codes issued and not yet used.
	codes map[string]authCode
	// devices are the device codes issued.
	devices map[string]*device
}

type authCode struct {
	clientID, redirect, challenge, method string
}

type device struct {
	expiresAt          time.Time
	slowDowns, pending int
}

// New starts a FakeVendor on clk that knows the client ClientID with
// ClientSecret. It is closed when the test ends.
func New(tb interface {
	Helper()
	Cleanup(func())
}, clk clock.Clock) *FakeVendor {
	tb.Helper()
	v := &FakeVendor{
		clk: clk, clientID: ClientID, clientSecret: ClientSecret,
		access: map[string]time.Time{}, refresh: map[string]bool{},
		codes: map[string]authCode{}, devices: map[string]*device{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", v.authorize)
	mux.HandleFunc("POST /token", v.token)
	mux.HandleFunc("POST /device", v.deviceStart)
	mux.HandleFunc("POST /revoke", v.revoke)
	v.Server = httptest.NewServer(mux)
	tb.Cleanup(v.Close)
	return v
}

// Endpoints are the vendor's endpoints, a revocation endpoint among
// them.
func (v *FakeVendor) Endpoints() connect.Endpoints {
	return connect.Endpoints{
		Authorization: v.URL + "/authorize",
		Token:         v.URL + "/token",
		Device:        v.URL + "/device",
		Revocation:    v.URL + "/revoke",
	}
}

// Client is the client the vendor knows.
func (v *FakeVendor) Client() connect.Client {
	return connect.Client{ID: v.clientID, Secret: v.clientSecret}
}

// Set changes the knobs.
func (v *FakeVendor) Set(fn func(k *Knobs)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fn(&v.knobs)
}

// Requests is every request the vendor took, in order.
func (v *FakeVendor) Requests() []Request {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.requests)
}

// Refreshes counts the refresh requests the vendor took.
func (v *FakeVendor) Refreshes() int {
	n := 0
	for _, r := range v.Requests() {
		if r.Path == "/token" && r.Form.Get("grant_type") == "refresh_token" {
			n++
		}
	}
	return n
}

// Revoked is every token revoked, in order.
func (v *FakeVendor) Revoked() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.revoked)
}

// Issue gives tokens as a sign-in would, for a test that starts from a
// connection already signed in.
func (v *FakeVendor) Issue() connect.Tokens {
	v.mu.Lock()
	defer v.mu.Unlock()
	access, refresh, expires := v.issueLocked()
	t := connect.Tokens{Access: access, Refresh: refresh, Scope: "repo"}
	if expires > 0 {
		t.ExpiresAt = v.clk.Now().Add(expires)
	}
	return t
}

// Valid reports whether access is a token the vendor issued that has
// not expired on the clock and was not revoked.
func (v *FakeVendor) Valid(access string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	exp, ok := v.access[access]
	if !ok || slices.Contains(v.revoked, access) {
		return false
	}
	return exp.IsZero() || v.clk.Now().Before(exp)
}

// RefreshAccepted reports whether the vendor takes refresh now.
func (v *FakeVendor) RefreshAccepted(refresh string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.refresh[refresh]
}

func (v *FakeVendor) record(r *http.Request) (Knobs, bool) {
	if err := r.ParseForm(); err != nil {
		return Knobs{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	form := url.Values{}
	for k, vals := range r.PostForm {
		form[k] = slices.Clone(vals)
	}
	if r.Method == http.MethodGet {
		form = r.URL.Query()
	}
	v.requests = append(v.requests, Request{Path: r.URL.Path, Form: form, Accept: r.Header.Get("Accept")})
	return v.knobs, true
}

func (v *FakeVendor) nextLocked(kind string) string {
	v.seq++
	return fmt.Sprintf("%s-%04d-%s", kind, v.seq, "x9Qe7Lw2")
}

func (v *FakeVendor) issueLocked() (access, refresh string, expires time.Duration) {
	access = v.nextLocked("access")
	refresh = v.nextLocked("refresh")
	var exp time.Time
	if v.knobs.AccessTTL > 0 {
		exp = v.clk.Now().Add(v.knobs.AccessTTL)
	}
	v.access[access] = exp
	v.refresh[refresh] = true
	return access, refresh, v.knobs.AccessTTL
}

// answer writes a JSON answer, or the form-encoded one GitHub sends to
// a request that does not ask for JSON.
func answer(w http.ResponseWriter, r *http.Request, status int, body map[string]any) {
	if r.Header.Get("Accept") != "application/json" {
		form := url.Values{}
		for k, val := range body {
			form.Set(k, fmt.Sprint(val))
		}
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(form.Encode()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// fail answers an error code in the vendor's style.
func fail(w http.ResponseWriter, r *http.Request, k Knobs, code, desc string) {
	status := http.StatusBadRequest
	if code == "invalid_client" {
		status = http.StatusUnauthorized
	}
	if k.GitHubStyle {
		status = http.StatusOK
	}
	answer(w, r, status, map[string]any{"error": code, "error_description": desc})
}

func (v *FakeVendor) tokensAnswer(k Knobs, access, refresh string, expires time.Duration) map[string]any {
	out := map[string]any{"access_token": access, "token_type": "bearer", "scope": "repo"}
	if refresh != "" {
		out["refresh_token"] = refresh
		// GitHub's; Kivali tolerates it and reads nothing from it.
		out["refresh_token_expires_in"] = 15897600
	}
	if expires > 0 {
		secs := int(expires / time.Second)
		if k.ExpiresInString {
			out["expires_in"] = fmt.Sprint(secs)
		} else {
			out["expires_in"] = secs
		}
	}
	return out
}

func (v *FakeVendor) clientOK(f url.Values, needSecret bool) bool {
	if f.Get("client_id") != v.clientID {
		return false
	}
	return !needSecret || f.Get("client_secret") == v.clientSecret
}

func (v *FakeVendor) token(w http.ResponseWriter, r *http.Request) {
	k, ok := v.record(r)
	if !ok {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	switch f.Get("grant_type") {
	case "refresh_token":
		v.refreshGrant(w, r, k, f)
	case "urn:ietf:params:oauth:grant-type:device_code":
		v.deviceGrant(w, r, k, f)
	case "authorization_code":
		v.codeGrant(w, r, k, f)
	default:
		fail(w, r, k, "unsupported_grant_type", "the grant type is not supported")
	}
}

func (v *FakeVendor) refreshGrant(w http.ResponseWriter, r *http.Request, k Knobs, f url.Values) {
	if k.OnRefresh != nil {
		k.OnRefresh()
	}
	switch {
	case k.RefreshStatus != 0:
		http.Error(w, "the vendor is down", k.RefreshStatus)
		return
	case k.RefreshMalformed:
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>maintenance</html>"))
		return
	case k.RefreshError != "":
		desc := "refused by the test"
		if k.RefreshErrorDescription != nil {
			desc = k.RefreshErrorDescription(f)
		}
		fail(w, r, k, k.RefreshError, desc)
		return
	case !v.clientOK(f, true):
		code := "invalid_client"
		if k.GitHubStyle {
			code = "incorrect_client_credentials"
		}
		fail(w, r, k, code, "the client id or secret is wrong")
		return
	}
	v.mu.Lock()
	old := f.Get("refresh_token")
	if !v.refresh[old] {
		v.mu.Unlock()
		code := "invalid_grant"
		if k.GitHubStyle {
			code = "bad_refresh_token"
		}
		fail(w, r, k, code, "the refresh token is not good")
		return
	}
	access, refresh, expires := v.issueLocked()
	if k.Rotate {
		delete(v.refresh, old)
	} else {
		delete(v.refresh, refresh)
		refresh = ""
	}
	v.mu.Unlock()
	answer(w, r, http.StatusOK, v.tokensAnswer(k, access, refresh, expires))
}

func (v *FakeVendor) deviceStart(w http.ResponseWriter, r *http.Request) {
	k, ok := v.record(r)
	if !ok || !v.clientOK(r.PostForm, false) {
		fail(w, r, k, "invalid_client", "unknown client")
		return
	}
	expires, interval := k.DeviceExpiresIn, k.DeviceInterval
	if expires == 0 {
		expires = 900 * time.Second
	}
	if interval == 0 {
		interval = 5 * time.Second
	}
	page, complete := v.URL+"/activate", v.URL+"/activate?user_code=WDJB-MJHT"
	if k.DeviceVerificationURI != "" {
		page = k.DeviceVerificationURI
	}
	if k.DeviceVerificationURIComplete != "" {
		complete = k.DeviceVerificationURIComplete
	}
	v.mu.Lock()
	code := v.nextLocked("device")
	v.devices[code] = &device{expiresAt: v.clk.Now().Add(expires)}
	v.mu.Unlock()
	answer(w, r, http.StatusOK, map[string]any{
		"device_code":               code,
		"user_code":                 "WDJB-MJHT",
		"verification_uri":          page,
		"verification_uri_complete": complete,
		"expires_in":                int(expires / time.Second),
		"interval":                  int(interval / time.Second),
	})
}

func (v *FakeVendor) deviceGrant(w http.ResponseWriter, r *http.Request, k Knobs, f url.Values) {
	if !v.clientOK(f, false) {
		fail(w, r, k, "invalid_client", "unknown client")
		return
	}
	v.mu.Lock()
	d := v.devices[f.Get("device_code")]
	switch {
	case d == nil:
		v.mu.Unlock()
		fail(w, r, k, "incorrect_device_code", "the device code is not one the vendor issued")
		return
	case !v.clk.Now().Before(d.expiresAt):
		v.mu.Unlock()
		fail(w, r, k, "expired_token", "the device code expired")
		return
	case d.slowDowns < k.DeviceSlowDown:
		d.slowDowns++
		v.mu.Unlock()
		fail(w, r, k, "slow_down", "too many requests")
		return
	case d.pending < k.DevicePending:
		d.pending++
		v.mu.Unlock()
		fail(w, r, k, "authorization_pending", "the owner has not approved yet")
		return
	case k.DeviceError != "":
		v.mu.Unlock()
		fail(w, r, k, k.DeviceError, "ended by the test")
		return
	}
	delete(v.devices, f.Get("device_code"))
	access, refresh, expires := v.issueLocked()
	v.mu.Unlock()
	answer(w, r, http.StatusOK, v.tokensAnswer(k, access, refresh, expires))
}

// authorize stands for the owner approving on the vendor's site: it
// sends the browser back to redirect_uri with a code and the state.
func (v *FakeVendor) authorize(w http.ResponseWriter, r *http.Request) {
	_, _ = v.record(r)
	q := r.URL.Query()
	if q.Get("client_id") != v.clientID || q.Get("response_type") != "code" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	code := v.nextLocked("code")
	v.codes[code] = authCode{clientID: q.Get("client_id"), redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method")}
	v.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (v *FakeVendor) codeGrant(w http.ResponseWriter, r *http.Request, k Knobs, f url.Values) {
	if !v.clientOK(f, true) {
		fail(w, r, k, "invalid_client", "the client id or secret is wrong")
		return
	}
	v.mu.Lock()
	c, ok := v.codes[f.Get("code")]
	delete(v.codes, f.Get("code"))
	v.mu.Unlock()
	switch {
	case !ok:
		fail(w, r, k, "invalid_grant", "the code is not good")
		return
	case c.redirect != f.Get("redirect_uri"):
		fail(w, r, k, "invalid_grant", "the redirect address does not match")
		return
	case c.challenge != "" && (c.method != "S256" || s256(f.Get("code_verifier")) != c.challenge):
		fail(w, r, k, "invalid_grant", "the PKCE verifier does not match")
		return
	}
	v.mu.Lock()
	access, refresh, expires := v.issueLocked()
	v.mu.Unlock()
	answer(w, r, http.StatusOK, v.tokensAnswer(k, access, refresh, expires))
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (v *FakeVendor) revoke(w http.ResponseWriter, r *http.Request) {
	k, ok := v.record(r)
	if !ok || !v.clientOK(r.PostForm, true) {
		fail(w, r, k, "invalid_client", "the client id or secret is wrong")
		return
	}
	if k.OnRevoke != nil {
		k.OnRevoke()
	}
	if k.RevokeStatus != 0 && k.RevokeStatus != http.StatusOK {
		http.Error(w, "revocation failed", k.RevokeStatus)
		return
	}
	v.mu.Lock()
	tok := r.PostForm.Get("token")
	v.revoked = append(v.revoked, tok)
	delete(v.refresh, tok)
	v.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}
