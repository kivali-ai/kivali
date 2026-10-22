// Package web (integration_signin_browser.go) — browser sign-in: the
// authorization code grant with PKCE.
//
// POST .../sign-in/browser starts a signInFlow (integration_signin_flows.go)
// that keeps a random OAuth state, the PKCE verifier and the redirect
// address, and answers with the vendor's sign-in page. It also sets a
// cookie, signed with the session codec, that names the flow: the
// sign-in finishes only in the browser that started it. The owner
// approves on the vendor's site, which sends the browser back to
// GET /connect/callback with a code and the state. The callback is part
// of the signed-in UI, behind the session middleware, so a browser not
// signed in to Kivali signs in first and comes back with the query as
// it was. It finds the flow by its state, once, checks the cookie,
// exchanges the code and keeps the tokens as a device sign-in does
// (finishSignIn), then sends the browser to the integration's page with
// the result in the query: ?signed_in=<connection id>, or
// ?sign_in_failed=<connection id>, whose words the page reads from
// GET .../sign-in, or ?sign_in_error=start_again when there is no
// sign-in to read. The query never carries words: anyone can link the
// owner to the page with any query, and the page shows only what
// Kivali said.
//
// The redirect address is the one the request reached Kivali at,
// <scheme>://<host>/connect/callback, from X-Forwarded-Proto and
// X-Forwarded-Host when a reverse proxy set them. The owner registers
// it with the vendor, which sends the code only there.

package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// connectCallbackPath is where a vendor sends the browser back to
// after a browser sign-in: one route for every integration, the state
// tells the sign-ins apart.
const connectCallbackPath = "/connect/callback"

// setupNotesRedirect is what a manifest's kivali.setup_notes writes
// for the redirect address, which the page and the install screen
// replace with it.
const setupNotesRedirect = "{{redirect_url}}"

// connectCookieBase is the browser sign-in cookie's name without the
// cookie suffix.
const connectCookieBase = "kivali_connect"

// connectCookieTTL is how long the browser keeps the sign-in cookie:
// past the flow's own ten minutes, so a callback that comes too late
// still finds which integration's page to return to.
const connectCookieTTL = 2 * signInFlowTTL

// connectCookiePrefix starts the signed payload of the sign-in cookie,
// so no other value the session codec signs reads as one.
const connectCookiePrefix = "connect:"

// Refusals and failures of browser sign-in.
const (
	// errSignInStartAgain: a callback whose state names no sign-in
	// under way (it took over ten minutes, it was used, or Kivali
	// restarted), or that came to a browser other than the one that
	// started the sign-in.
	errSignInStartAgain = "the sign-in took too long or was started elsewhere; start it again"
	errSignInNoCode     = "the service sent no sign-in code back; start it again"
	errSignInStopped    = "the sign-in was stopped before it finished; start it again"
	errSignInNoAddress  = "Kivali cannot tell the address it was reached at from this request"
)

// signInRedirectURL is the redirect address for browser sign-in as r
// reached Kivali: <scheme>://<host>/connect/callback. The scheme is
// the one the API's same-origin check reads (requestScheme): https on
// a TLS connection, else X-Forwarded-Proto's first value when it is
// https, else http; the host is
// X-Forwarded-Host's first value when it is a host, else r.Host,
// lower-cased and without the scheme's default port. Empty when
// neither is a host.
//
// Either header can be anything the browser sent. That is safe: the
// vendor sends the code only to the address the owner registered, and
// only to the browser that asks, which carries the cookie the
// callback checks.
func signInRedirectURL(r *http.Request) string {
	scheme := requestScheme(r)
	if scheme != "https" {
		scheme = "http"
	}
	host := ""
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = callbackHost(firstForwarded(fwd), scheme)
	}
	if host == "" {
		host = callbackHost(r.Host, scheme)
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host + connectCallbackPath
}

// callbackHost is host as a redirect address under scheme names it:
// lower-cased, without the scheme's default port. "" when host is not
// a name or an address with an optional port.
func callbackHost(host, scheme string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, c := range host {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && !strings.ContainsRune(".-_:[]", c) {
			return ""
		}
	}
	u, err := url.Parse(scheme + "://" + host)
	if err != nil || u.Host != host || u.Hostname() == "" {
		return ""
	}
	if p := u.Port(); (scheme == "https" && p == "443") || (scheme == "http" && p == "80") {
		host = strings.TrimSuffix(host, ":"+p)
	}
	return host
}

// offersBrowser reports whether m's connection is offered browser
// sign-in.
func offersBrowser(m *integration.Manifest) bool {
	return m != nil && slices.Contains(offeredSignInWays(m.Kivali.Connection), integration.MethodBrowser)
}

// browserRedirectURL is the redirect address the page shows for m, as
// r reached Kivali: nil when m does not offer browser sign-in, or r
// names no host.
func browserRedirectURL(m *integration.Manifest, r *http.Request) *string {
	if !offersBrowser(m) {
		return nil
	}
	return optString(signInRedirectURL(r))
}

// setupNotes is m's kivali.setup_notes with {{redirect_url}} replaced
// by the redirect address r reached Kivali at.
func setupNotes(m *integration.Manifest, r *http.Request) string {
	notes := m.Kivali.SetupNotes
	if strings.Contains(notes, setupNotesRedirect) {
		notes = strings.ReplaceAll(notes, setupNotesRedirect, signInRedirectURL(r))
	}
	return notes
}

// newOAuthState is a fresh OAuth state: 32 random bytes, base64url.
func newOAuthState() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// connectCookieName is the browser sign-in cookie's name, with the
// cookie suffix (KIVALI_COOKIE_SUFFIX) as the session cookie has it.
func (s *Server) connectCookieName() string {
	suffix := s.CookieSuffix
	if suffix == "" && s.AuthMW != nil {
		suffix = s.AuthMW.CookieSuffix
	}
	if suffix == "" {
		return connectCookieBase
	}
	return connectCookieBase + "_" + suffix
}

// connectCodec signs the browser sign-in cookie: the session codec, or,
// on a server given none, one with a key of its own for the life of
// the process, which is as long as any sign-in lasts.
func (s *Server) connectCodec() *auth.Codec {
	switch {
	case s.SessionCodec != nil:
		return s.SessionCodec
	case s.AuthMW != nil && s.AuthMW.Codec != nil:
		return s.AuthMW.Codec
	}
	s.connectCodecOnce.Do(func() {
		var key [32]byte
		_, _ = rand.Read(key[:])
		s.connectCodecOwn, _ = auth.NewCodec(key[:])
	})
	return s.connectCodecOwn
}

// connectCookie is the cookie that ties the flow id of name's sign-in
// to the browser that started it, sent only to the callback. Secure
// when the redirect address is https.
func (s *Server) connectCookie(id, name, redirectURL string) *http.Cookie {
	return &http.Cookie{
		Name:     s.connectCookieName(),
		Value:    s.connectCodec().Sign([]byte(connectCookiePrefix + id + ":" + name)),
		Path:     connectCallbackPath,
		HttpOnly: true,
		Secure:   strings.HasPrefix(redirectURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(connectCookieTTL / time.Second),
	}
}

// readConnectCookie is the flow id and the integration's name r's
// sign-in cookie names; empty when it has none or it is not one Kivali
// signed.
func (s *Server) readConnectCookie(r *http.Request) (id, name string) {
	c, err := r.Cookie(s.connectCookieName())
	if err != nil {
		return "", ""
	}
	payload, err := s.connectCodec().Verify(c.Value)
	if err != nil {
		return "", ""
	}
	rest, ok := strings.CutPrefix(string(payload), connectCookiePrefix)
	if !ok {
		return "", ""
	}
	id, name, _ = strings.Cut(rest, ":")
	return id, name
}

// handleAPISignInBrowser starts a browser sign-in of the connection
// {id} names (byID) or of the integration's one, and answers with the
// vendor's sign-in page, which the web app sends the browser to. It
// refuses as the device route does: the connection's client id and
// secret must be stored, and a connection signed in is signed out
// first. The flow keeps a random state, the PKCE verifier (unless the
// manifest says pkce false) and the redirect address of this request
// for ten minutes; the answer sets the cookie the callback checks.
func (s *Server) handleAPISignInBrowser(byID bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		unlock := s.lockIntegrations()
		defer unlock()
		v, ok := s.integrationOr404(w, r)
		if !ok {
			return
		}
		c, ok := s.signInTarget(w, r, v, byID, errSignInClientFirst)
		if !ok {
			return
		}
		name, connID, m := v.it.Name, c.rec.ID, v.manifest
		switch {
		case !offersBrowser(m):
			writeAPIError(w, http.StatusConflict, v.displayName()+" does not sign in in the browser", v.whoBuilt())
			return
		case !v.it.Enabled:
			writeAPIError(w, http.StatusConflict, "switch "+v.displayName()+" on first", whoYou)
			return
		case !c.rec.NeedsSignIn(c.hasToken):
			writeAPIError(w, http.StatusConflict, errSignInSignedIn, whoYou)
			return
		}
		rec, data, err := s.readSignIn(r.Context(), name, connID)
		if err != nil {
			log.Printf("integration %s: read connection %s: %v", name, connID, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection could not be read", whoServer)
			return
		}
		client := signInClient(rec, data)
		if client.ID == "" || client.Secret == "" {
			writeAPIError(w, http.StatusConflict, errSignInClientFirst, whoYou)
			return
		}
		redirect := signInRedirectURL(r)
		if redirect == "" {
			writeAPIError(w, http.StatusConflict, errSignInNoAddress, whoServer)
			return
		}
		conn := m.Kivali.Connection
		bf := &browserFlow{state: newOAuthState(), redirectURL: redirect}
		if conn.UsesPKCE() {
			bf.verifier = connect.NewPKCEVerifier()
		}
		target := s.connectEngine().AuthorizeURL(signInEndpoints(m), client, redirect, bf.state, conn.Scopes, bf.verifier, conn.ExtraAuthorizationParams)
		if target == "" {
			writeAPIError(w, http.StatusConflict, v.displayName()+"'s sign-in address is not one Kivali can use", v.whoBuilt())
			return
		}
		now := s.clk().Now()
		f := s.signInFlows.start(now, name, connID, now.Add(signInFlowTTL), bf)
		http.SetCookie(w, s.connectCookie(f.id, name, redirect))
		writeJSON(w, http.StatusOK, apitypes.BrowserSignIn{URL: target})
	}
}

// handleConnectCallback is GET /connect/callback, where the vendor
// sends the owner's browser back with a code and the state, or with
// its error. It is a GET that changes things (it keeps the tokens), as
// OAuth has it: the vendor's redirect is a GET. What makes it safe is
// that it runs behind the session middleware, that the state is
// random and good for one callback, and that the cookie set when the
// sign-in started must name the same sign-in. The state and the code
// are never logged.
//
// It answers with a redirect (303) to the integration's page
// (connectReturn): ?signed_in=<connection id> once the tokens are kept
// and the connection test ran, ?sign_in_failed=<connection id> once the
// flow ended failed with its words, which the page reads from
// GET .../sign-in. A state that names no sign-in, and a sign-in
// cancelled meanwhile, answer ?sign_in_error=start_again, to the page
// of the integration the flow or the cookie names, or to the
// integrations list.
func (s *Server) handleConnectCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	cookieID, cookieName := s.readConnectCookie(r)
	// The cookie is for one sign-in; it goes whatever happens.
	gone := s.connectCookie("", "", signInRedirectURL(r))
	gone.Value, gone.MaxAge = "", -1
	http.SetCookie(w, gone)

	f := s.signInFlows.take(s.clk().Now(), q.Get("state"))
	if f == nil {
		connectReturn(w, r, cookieName, connectStartAgain, "")
		return
	}
	defer close(f.done)
	if cookieID != f.id {
		s.signInFlows.end(f, s.clk().Now(), apitypes.SignInStateFailed, errSignInStartAgain)
		connectReturn(w, r, f.name, connectFailed, f.connID)
		return
	}
	switch words := s.completeBrowserSignIn(f, q); words {
	case "":
		connectReturn(w, r, f.name, connectSignedIn, f.connID)
	case errSignInStopped:
		// Cancelled and forgotten: there are no words to read.
		connectReturn(w, r, f.name, connectStartAgain, "")
	default:
		connectReturn(w, r, f.name, connectFailed, f.connID)
	}
}

// completeBrowserSignIn finishes f with the callback's query q: it
// exchanges the code for tokens, keeps them (finishSignIn) and runs the
// connection test. It returns "" once signed in, else why not in words
// for the page. The flow ends as it went, and every failure past the
// vendor's answer is in the owner's audit.
func (s *Server) completeBrowserSignIn(f *signInFlow, q url.Values) string {
	start := s.clk().Now()
	fail := func(words string, audit bool) string {
		if audit {
			s.auditAgentCall(f.name, start, agent.CEOSlug, "sign_in", false, words)
		}
		s.signInFlows.end(f, s.clk().Now(), apitypes.SignInStateFailed, words)
		return words
	}
	v, err := s.loadIntegration(f.ctx, f.name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fail(errConnectionGone, false)
	case err != nil:
		log.Printf("integration %s: read it to finish a sign-in: %v", f.name, err)
		return fail("the integration could not be read; start the sign-in again", false)
	case !offersBrowser(v.manifest):
		return fail(v.displayName()+" does not sign in in the browser", true)
	}
	m := v.manifest
	rec, data, err := s.readSignIn(f.ctx, f.name, f.connID)
	switch {
	case f.ctx.Err() != nil:
		return errSignInStopped
	case errors.Is(err, store.ErrNotFound):
		return fail(errConnectionGone, false)
	case err != nil:
		log.Printf("integration %s: read connection %s: %v", f.name, f.connID, err)
		return fail("the connection could not be read; start the sign-in again", false)
	}
	client := signInClient(rec, data)
	code := q.Get("code")
	// The vendor's words may echo what it was sent or sends back: the
	// state, the code, the verifier, the client secret. They are
	// scrubbed of each before they are cut.
	echoed := []string{f.browser.state, code, f.browser.verifier}
	if vendorCode := q.Get("error"); vendorCode != "" {
		return fail(vendorRefusalWords(vendorCode, q.Get("error_description"), client, echoed...), true)
	}
	if code == "" {
		return fail(errSignInNoCode, true)
	}
	tok, err := s.connectEngine().Exchange(f.ctx, signInEndpoints(m), client, code, f.browser.redirectURL, f.browser.verifier)
	if f.ctx.Err() != nil {
		// Cancelled meanwhile: tokens the vendor gave are revoked, not
		// kept.
		if err == nil {
			s.revokeTokens(f.ctx, f.name, f.connID, m, client, tok)
		}
		return errSignInStopped
	}
	if err != nil {
		words := signInFailureWords(err, connect.Tokens{}, client, echoed...)
		var term *connect.Terminal
		if !errors.As(err, &term) {
			log.Printf("integration %s: exchange connection %s's sign-in code: %s", f.name, f.connID, words)
		}
		return fail(words, true)
	}
	if err := s.finishSignIn(f, m, client, integration.MethodBrowser, tok, start); err != nil {
		if f.ctx.Err() != nil {
			return errSignInStopped
		}
		return fail(err.Error(), false)
	}
	s.testAfterSignIn(f.ctx, f.name, f.connID)
	s.signInFlows.end(f, s.clk().Now(), apitypes.SignInStateDone, "")
	return ""
}

// vendorRefusalWords is the vendor's error on a callback in words for
// the page: its own description, or its error code when it gave none,
// scrubbed of client's secret and of more whole and then cut to
// signInWordsRunes, as signInFailureWords does.
func vendorRefusalWords(code, description string, client connect.Client, more ...string) string {
	scrub := newScrubber(append([]string{client.Secret}, more...))
	if d := strings.TrimSpace(description); d != "" {
		return clipRunes(scrub.Replace(d), signInWordsRunes)
	}
	return clipRunes("the service refused the sign-in ("+scrub.Replace(code)+")", signInWordsRunes)
}

// How a callback sends the browser back (connectReturn): the query
// key, and for connectStartAgain its one value.
const (
	// connectSignedIn=<connection id>: signed in.
	connectSignedIn = "signed_in"
	// connectFailed=<connection id>: the sign-in ended failed; its
	// words are the connection's GET .../sign-in.
	connectFailed = "sign_in_failed"
	// connectStartAgain: sign_in_error=start_again, there is no
	// sign-in to read (it took too long, was used, was cancelled, or
	// Kivali restarted). The page says so in its own words.
	connectStartAgain = "start_again"
)

// connectReturn sends the browser back to name's page after a
// callback, to the integrations list when name is empty, with
// ?<key>=<connID>, or ?sign_in_error=start_again for connectStartAgain.
func connectReturn(w http.ResponseWriter, r *http.Request, name, key, connID string) {
	target := "/org/integrations"
	if name != "" {
		target += "/" + url.PathEscape(name)
	}
	q := url.Values{}
	if key == connectStartAgain {
		q.Set("sign_in_error", connectStartAgain)
	} else {
		q.Set(key, connID)
	}
	http.Redirect(w, r, target+"?"+q.Encode(), http.StatusSeeOther)
}
