// Package connect runs the OAuth flows of a connection made by signing
// in (a manifest whose kivali.connection.method is device or browser):
// the device authorization grant (RFC 8628), the authorization code
// grant with PKCE (RFC 7636), refresh, and revocation (RFC 7009); and,
// for a client core registers itself, discovery of the authorization
// server (RFC 9728, RFC 8414), registration (RFC 7591) and deleting the
// registration (RFC 7592).
//
// Core runs every flow; an integration never does OAuth and never sees
// the client secret or the refresh token. It asks kivali/credential at
// call time and gets a current access token, which core refreshes here
// first when it is about to expire.
//
// The package knows nothing of the store or the web server. Every
// request goes through Engine.HTTP and every wait and expiry is read
// from Engine.Clock, so tests drive both (connecttest.FakeVendor and
// clock.Fake). docs/developers/integrations.md is the reference.
package connect

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/clock"
)

// The keys a connection's Secret keeps its tokens under. They are not
// connection fields: the manifest check refuses a user_config key equal
// to either, and neither is ever given to an integration.
const (
	SecretKeyAccessToken  = "oauth_access_token"
	SecretKeyRefreshToken = "oauth_refresh_token"
)

// The keys a connection's Secret keeps a client core registered itself
// under (Register), with the endpoints discovery found for it (Discover).
// Like the token keys they are no connection field and are never given
// to an integration. A list (the redirect addresses) is one address per
// line; a time is Unix seconds.
const (
	SecretKeyClientID                = "oauth_client_id"
	SecretKeyClientSecret            = "oauth_client_secret"
	SecretKeyClientSecretExpiresAt   = "oauth_client_secret_expires_at"
	SecretKeyClientAuthMethod        = "oauth_token_endpoint_auth_method"
	SecretKeyRedirectURIs            = "oauth_redirect_uris"
	SecretKeyResource                = "oauth_resource"
	SecretKeyAuthorizationEndpoint   = "oauth_authorization_endpoint"
	SecretKeyTokenEndpoint           = "oauth_token_endpoint"
	SecretKeyRevocationEndpoint      = "oauth_revocation_endpoint"
	SecretKeyRegistrationClientURI   = "oauth_registration_client_uri"
	SecretKeyRegistrationAccessToken = "oauth_registration_access_token"
)

// RegisteredSecretKeys are the keys a registered client is kept under,
// in a fixed order.
var RegisteredSecretKeys = []string{
	SecretKeyClientID, SecretKeyClientSecret, SecretKeyClientSecretExpiresAt, SecretKeyClientAuthMethod,
	SecretKeyRedirectURIs, SecretKeyResource,
	SecretKeyAuthorizationEndpoint, SecretKeyTokenEndpoint, SecretKeyRevocationEndpoint,
	SecretKeyRegistrationClientURI, SecretKeyRegistrationAccessToken,
}

// SecretKeys is every key core keeps of a sign-in in a connection's
// Secret: the tokens, then a registered client's. A save of the
// connection form keeps them, and no form field or setup key may be one.
var SecretKeys = append([]string{SecretKeyAccessToken, SecretKeyRefreshToken}, RegisteredSecretKeys...)

// SecretValueKeys are those of SecretKeys whose values are secrets,
// scrubbed from every log line and answer: the tokens, a registered
// client's secret and its registration access token.
var SecretValueKeys = []string{SecretKeyAccessToken, SecretKeyRefreshToken, SecretKeyClientSecret, SecretKeyRegistrationAccessToken}

// RefreshMargin is how close to its expiry an access token is
// refreshed before it is handed out.
const RefreshMargin = time.Minute

// maxAnswerBytes bounds what is read of a vendor's answer.
const maxAnswerBytes = 1 << 20

// deviceSlowDown is what a slow_down answer adds to the device poll's
// interval (RFC 8628, 3.5).
const deviceSlowDown = 5 * time.Second

// deviceDefaultInterval is the device poll's interval when the vendor
// names none (RFC 8628, 3.2).
const deviceDefaultInterval = 5 * time.Second

// Client is the OAuth app a connection signs in with: the one the
// owner registered with the vendor, or the one core registered itself
// (Register).
type Client struct {
	ID, Secret string
	// AuthMethod is how the client authenticates at the token and
	// revocation endpoints: AuthSecretPost (empty is the same: the id and
	// secret in the body), AuthSecretBasic (HTTP Basic) or AuthNone (a
	// public client: the id alone).
	AuthMethod string
}

// The ways a client authenticates at the token and revocation
// endpoints (RFC 7591's token_endpoint_auth_method).
const (
	AuthSecretPost  = "client_secret_post"
	AuthSecretBasic = "client_secret_basic"
	AuthNone        = "none"
)

// Endpoints are the vendor's OAuth addresses, from the manifest or, for
// a client core registers, from discovery. An endpoint not given is
// empty.
type Endpoints struct {
	Authorization, Token, Device, Revocation string
	// Resource is the protected resource the tokens are for (RFC 8707),
	// sent with the authorization request, the code exchange and each
	// refresh; empty sends none.
	Resource string
}

// Tokens is what a sign-in or a refresh gives.
type Tokens struct {
	Access, Refresh string
	// ExpiresAt is when Access expires; zero when the vendor gave no
	// expiry (the token does not expire).
	ExpiresAt time.Time
	// Scope is what the vendor granted, as it said it; empty when it
	// did not say.
	Scope string
}

// RefreshDue reports whether an access token that expires at expiresAt
// is to be refreshed at now: within RefreshMargin of its expiry, or
// past it. A token with no expiry (zero) never is.
func RefreshDue(now, expiresAt time.Time) bool {
	return !expiresAt.IsZero() && !now.Before(expiresAt.Add(-RefreshMargin))
}

// Engine makes the vendor's requests.
type Engine struct {
	Clock clock.Clock
	HTTP  *http.Client
}

// Terminal is a vendor's refusal that only a new sign-in mends: the
// refresh token or the client is no longer accepted, or the owner
// denied the sign-in. Any other failure is transient and the stored
// tokens stay.
type Terminal struct {
	// Code is the vendor's error code (invalid_grant, ...).
	Code string
	// Description is the vendor's own words, at most
	// MaxDescriptionRunes; may be empty.
	Description string
}

func (t *Terminal) Error() string {
	if t.Description != "" {
		return "the vendor refused the sign-in (" + t.Code + "): " + t.Description
	}
	return "the vendor refused the sign-in (" + t.Code + ")"
}

// terminalCodes are the token endpoint's error codes after which the
// owner must sign in again: RFC 6749's, and GitHub's own for a refresh
// token or client credentials it no longer accepts.
var terminalCodes = []string{
	"invalid_grant", "invalid_client", "unauthorized_client", "access_denied",
	"bad_refresh_token", "incorrect_client_credentials",
}

// deviceFailCodes end a device flow as failed, with the vendor's
// words, besides the terminal ones.
var deviceFailCodes = []string{"incorrect_device_code", "device_flow_disabled", "unsupported_grant_type"}

// ErrDeviceExpired ends a device flow whose code expired before the
// owner approved it.
var ErrDeviceExpired = errors.New("the code expired before the sign-in was approved")

// MaxDescriptionRunes bounds the vendor's error_description an error
// carries, well below what is read of an answer. It is long enough to
// hold whole any token the vendor echoes near the start, so the caller
// can scrub the words of its secrets before it cuts them further for
// what it shows. maxCodeRunes bounds the error code.
const (
	MaxDescriptionRunes = 2000
	maxCodeRunes        = 64
)

// clipRunes is s cut to at most n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// vendorError is an error answer of a token or device endpoint:
// RFC 6749's error object, which some vendors (GitHub) send with a 200.
type vendorError struct {
	Code, Description string
}

func (e *vendorError) Error() string {
	if e.Description != "" {
		return "the vendor answered " + e.Code + ": " + e.Description
	}
	return "the vendor answered " + e.Code
}

// tokenAnswer is a token endpoint's success answer, and a device
// endpoint's.
type tokenAnswer struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    json.RawMessage `json:"expires_in"`
	Scope        string          `json:"scope"`

	DeviceCode              string          `json:"device_code"`
	UserCode                string          `json:"user_code"`
	VerificationURI         string          `json:"verification_uri"`
	VerificationURL         string          `json:"verification_url"`
	VerificationURIComplete string          `json:"verification_uri_complete"`
	Interval                json.RawMessage `json:"interval"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// seconds reads a JSON number of seconds, or a string holding one
// (some vendors quote it); 0 when absent or unreadable.
func seconds(raw json.RawMessage) time.Duration {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n * float64(time.Second))
}

// post sends form to endpoint as a token request is sent: a
// form-encoded POST that asks for JSON (GitHub answers form-encoded
// without the Accept header). It returns the answer, or a
// *vendorError when the answer carries an error code, whatever its
// status, or another error for a failure to reach the vendor or an
// answer that is not one. It follows no redirect: a 307 or 308 would
// send the form (a code, a verifier, the client secret) on to wherever
// it names, so a redirect is an answer that is not one ("the vendor
// answered 307 Temporary Redirect"). Revoke follows none either.
func (e *Engine) post(ctx context.Context, endpoint string, form url.Values) (*tokenAnswer, error) {
	return e.postAs(ctx, endpoint, form, nil)
}

// authenticate puts c's credentials on a token or revocation request:
// the id and secret in form for AuthSecretPost (and empty), the id
// alone for AuthNone, or for AuthSecretBasic an Authorization header
// (each part form-encoded first, RFC 6749 2.3.1) and nothing in form.
// It returns the header to set, "" for none.
func (c Client) authenticate(form url.Values) string {
	switch c.AuthMethod {
	case AuthSecretBasic:
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(c.ID)+":"+url.QueryEscape(c.Secret)))
	case AuthNone:
		form.Set("client_id", c.ID)
	default:
		form.Set("client_id", c.ID)
		form.Set("client_secret", c.Secret)
	}
	return ""
}

// postAs is post with c's credentials (Client.authenticate), when c is
// set.
func (e *Engine) postAs(ctx context.Context, endpoint string, form url.Values, c *Client) (*tokenAnswer, error) {
	auth := ""
	if c != nil {
		auth = c.authenticate(form)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := e.clientWith(noRedirects).Do(req)
	if err != nil {
		return nil, fmt.Errorf("the vendor did not answer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return nil, fmt.Errorf("the vendor's answer could not be read: %w", err)
	}
	var a tokenAnswer
	if jerr := json.Unmarshal(body, &a); jerr != nil {
		// A vendor that answers form-encoded despite the Accept header.
		if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "application/x-www-form-urlencoded" {
			if v, perr := url.ParseQuery(string(body)); perr == nil {
				a = tokenAnswer{
					AccessToken: v.Get("access_token"), RefreshToken: v.Get("refresh_token"),
					ExpiresIn: json.RawMessage(strconv.Quote(v.Get("expires_in"))), Scope: v.Get("scope"),
					Error: v.Get("error"), ErrorDescription: v.Get("error_description"),
				}
				jerr = nil
			}
		}
		if jerr != nil {
			if resp.StatusCode >= 300 {
				return nil, fmt.Errorf("the vendor answered %s", resp.Status)
			}
			return nil, errors.New("the vendor's answer is not one Kivali reads")
		}
	}
	if a.Error != "" {
		return nil, &vendorError{Code: clipRunes(a.Error, maxCodeRunes), Description: clipRunes(a.ErrorDescription, MaxDescriptionRunes)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the vendor answered %s", resp.Status)
	}
	return &a, nil
}

func (e *Engine) client() *http.Client {
	if e.HTTP != nil {
		return e.HTTP
	}
	return http.DefaultClient
}

func (e *Engine) clk() clock.Clock {
	if e.Clock == nil {
		return clock.New()
	}
	return e.Clock
}

func (e *Engine) now() time.Time { return e.clk().Now() }

// tokens reads a token answer at now; keep is the refresh token to keep
// when the answer carries none.
func (e *Engine) tokens(a *tokenAnswer, keep string) (Tokens, error) {
	if a.AccessToken == "" {
		return Tokens{}, errors.New("the vendor's answer has no access token")
	}
	t := Tokens{Access: a.AccessToken, Refresh: a.RefreshToken, Scope: a.Scope}
	if t.Refresh == "" {
		t.Refresh = keep
	}
	if d := seconds(a.ExpiresIn); d > 0 {
		t.ExpiresAt = e.now().Add(d)
	}
	return t, nil
}

// asTerminal is err as a *Terminal when it is a vendor error whose code
// is in codes; err itself otherwise.
func asTerminal(err error, codes ...[]string) error {
	var ve *vendorError
	if !errors.As(err, &ve) {
		return err
	}
	for _, set := range codes {
		if slices.Contains(set, ve.Code) {
			return &Terminal{Code: ve.Code, Description: ve.Description}
		}
	}
	return err
}

// Refresh exchanges a refresh token for new tokens, with the client's
// credentials as its AuthMethod says (the id and secret in the body by
// default) and the resource when the endpoints name one. A vendor answer of invalid_grant,
// invalid_client, unauthorized_client or access_denied (or GitHub's
// bad_refresh_token, incorrect_client_credentials), with whatever
// status, is a *Terminal error: the owner must sign in again. Anything
// else (no answer, a 5xx, an answer Kivali cannot read, another error
// code) is transient. A rotated refresh token is in the answer; a
// vendor that sends none keeps the old one.
func (e *Engine) Refresh(ctx context.Context, ep Endpoints, c Client, refresh string) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}
	if ep.Resource != "" {
		form.Set("resource", ep.Resource)
	}
	a, err := e.postAs(ctx, ep.Token, form, &c)
	if err != nil {
		return Tokens{}, asTerminal(err, terminalCodes)
	}
	return e.tokens(a, refresh)
}

// Revoke asks the vendor to revoke the tokens (RFC 7009), the refresh
// token first and then the access token, one request each, with the
// client's credentials. With no revocation endpoint there is nothing
// to do. A failure is the caller's to log; it never blocks a delete.
func (e *Engine) Revoke(ctx context.Context, ep Endpoints, c Client, t Tokens) error {
	if ep.Revocation == "" {
		return nil
	}
	var errs []error
	for _, tok := range []struct{ value, hint string }{{t.Refresh, "refresh_token"}, {t.Access, "access_token"}} {
		if tok.value == "" {
			continue
		}
		form := url.Values{"token": {tok.value}, "token_type_hint": {tok.hint}}
		auth := c.authenticate(form)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.Revocation, strings.NewReader(form.Encode()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := e.clientWith(noRedirects).Do(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("revoke the %s: %w", tok.hint, err))
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerBytes))
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errs = append(errs, fmt.Errorf("revoke the %s: the vendor answered %s", tok.hint, resp.Status))
		}
	}
	return errors.Join(errs...)
}

// DeviceStart is a device flow under way: what the owner is shown (the
// code and where to enter it), when the code expires and how often the
// token endpoint is polled.
type DeviceStart struct {
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresAt               time.Time
	Interval                time.Duration
	deviceCode              string
}

// StartDevice starts the device authorization grant: it posts the
// client id and the scopes to the device endpoint. The client secret is
// not sent.
func (e *Engine) StartDevice(ctx context.Context, ep Endpoints, c Client, scopes []string) (*DeviceStart, error) {
	form := url.Values{"client_id": {c.ID}}
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	a, err := e.post(ctx, ep.Device, form)
	if err != nil {
		return nil, asTerminal(err, terminalCodes, deviceFailCodes)
	}
	uri := a.VerificationURI
	if uri == "" {
		uri = a.VerificationURL
	}
	if a.DeviceCode == "" || a.UserCode == "" {
		return nil, errors.New("the vendor's answer has no code to show")
	}
	// The owner's browser is sent to these: an address that is not an
	// absolute http(s) one (javascript:, a relative path) is no page.
	if !webAddress(uri) || (a.VerificationURIComplete != "" && !webAddress(a.VerificationURIComplete)) {
		return nil, errors.New("the vendor's answer has no page to show")
	}
	ds := &DeviceStart{
		UserCode:                a.UserCode,
		VerificationURI:         uri,
		VerificationURIComplete: a.VerificationURIComplete,
		Interval:                seconds(a.Interval),
		deviceCode:              a.DeviceCode,
	}
	if ds.Interval <= 0 {
		ds.Interval = deviceDefaultInterval
	}
	if d := seconds(a.ExpiresIn); d > 0 {
		ds.ExpiresAt = e.now().Add(d)
	}
	return ds, nil
}

// DeviceCode is the code the vendor gave ds to poll with: never shown,
// and scrubbed from the vendor's words should they echo it.
func (ds *DeviceStart) DeviceCode() string { return ds.deviceCode }

// webAddress reports whether u is an absolute http:// or https://
// address with a host.
func webAddress(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != ""
}

// PollDevice polls the token endpoint for ds on the clock, at its
// interval, until the owner approves (the tokens), denies or the
// vendor ends the flow (a *Terminal with the vendor's words), the code
// expires (ErrDeviceExpired) or ctx ends (its error). It sends the
// client id only. authorization_pending polls again; slow_down adds
// five seconds to the interval; a transient failure (no answer, a 5xx)
// polls again at the next interval.
func (e *Engine) PollDevice(ctx context.Context, ep Endpoints, c Client, ds *DeviceStart) (Tokens, error) {
	interval := ds.Interval
	if interval <= 0 {
		interval = deviceDefaultInterval
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {ds.deviceCode},
		"client_id":   {c.ID},
	}
	for {
		if !ds.ExpiresAt.IsZero() && !e.now().Before(ds.ExpiresAt) {
			return Tokens{}, ErrDeviceExpired
		}
		t := e.clk().NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return Tokens{}, ctx.Err()
		case <-t.C():
		}
		a, err := e.post(ctx, ep.Token, form)
		var ve *vendorError
		switch {
		case err == nil:
			return e.tokens(a, "")
		case ctx.Err() != nil:
			return Tokens{}, ctx.Err()
		case errors.As(err, &ve) && ve.Code == "authorization_pending":
		case errors.As(err, &ve) && ve.Code == "slow_down":
			interval += deviceSlowDown
		case errors.As(err, &ve) && ve.Code == "expired_token":
			return Tokens{}, ErrDeviceExpired
		case errors.As(err, &ve):
			return Tokens{}, &Terminal{Code: ve.Code, Description: ve.Description}
		}
	}
}

// AuthorizeURL is the vendor's authorization address for the code
// grant: the endpoint with response_type=code, the client id, the
// redirect address, the scopes, state, the PKCE challenge (S256) when
// pkceVerifier is set, the resource when the endpoints name one (RFC
// 8707), and the manifest's extra parameters. The endpoint's own query
// parameters are kept.
func (e *Engine) AuthorizeURL(ep Endpoints, c Client, redirectURL, state string, scopes []string, pkceVerifier string, extra map[string]string) string {
	u, err := url.Parse(ep.Authorization)
	if err != nil {
		return ""
	}
	q := u.Query()
	for k, v := range extra {
		q.Set(k, v)
	}
	q.Set("response_type", "code")
	q.Set("client_id", c.ID)
	q.Set("redirect_uri", redirectURL)
	q.Set("state", state)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	if pkceVerifier != "" {
		q.Set("code_challenge", PKCEChallenge(pkceVerifier))
		q.Set("code_challenge_method", "S256")
	}
	if ep.Resource != "" {
		q.Set("resource", ep.Resource)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Exchange trades an authorization code for tokens, with the client's
// credentials, the redirect address the code was issued for, the PKCE
// verifier when one was used and the resource when the endpoints name
// one. A refusal in the terminal set is a
// *Terminal; any other failure is an error in words.
func (e *Engine) Exchange(ctx context.Context, ep Endpoints, c Client, code, redirectURL, pkceVerifier string) (Tokens, error) {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURL},
	}
	if pkceVerifier != "" {
		form.Set("code_verifier", pkceVerifier)
	}
	if ep.Resource != "" {
		form.Set("resource", ep.Resource)
	}
	a, err := e.postAs(ctx, ep.Token, form, &c)
	if err != nil {
		return Tokens{}, asTerminal(err, terminalCodes)
	}
	return e.tokens(a, "")
}

// NewPKCEVerifier is a fresh PKCE code verifier: 32 random bytes,
// base64url without padding (43 characters, within RFC 7636's 43 to
// 128).
func NewPKCEVerifier() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// PKCEChallenge is the S256 code challenge of a verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
