// Package web (integration_signin_browser.go) — what a Go step's trip
// to a service and back needs: the callback address a request reached
// Kivali at, where a browser sign-in can finish, the setup notes'
// addresses, and the flow's cookie.
//
// The callback address is the one the request reached Kivali at,
// <scheme>://<host>/connect/callback, from X-Forwarded-Proto and
// X-Forwarded-Host when a reverse proxy set them. The owner registers
// it with the vendor, which sends the browser only there. The cookie,
// signed with the session codec, names the flow and its integration:
// /connect/start sets it in the browser that leaves, and the callback
// requires it (integration_setup_routes.go).

package web

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
)

// connectCallbackPath is where a service sends the browser back to
// after a Go step: one route for every integration, the state tells
// the flows apart.
const connectCallbackPath = "/connect/callback"

// connectStartPath is where the web app sends the browser for a Go
// step: ?flow=<flow id>. Kivali Desktop knows the path
// (desktop-app/src-tauri/src/windows.rs) and opens the handoff in the
// system browser in its place.
const connectStartPath = "/connect/start"

// setupNotesRedirect is what a manifest's kivali.setup_notes writes
// for the redirect address, which the page and the install screen
// replace with it.
const setupNotesRedirect = "{{redirect_url}}"

// setupNotesExternalRedirect is what kivali.setup_notes writes for the
// redirect address at the team's external address
// (externalRedirectURL), so the vendor's app can be given both the
// address the owner set it up from and the one other computers sign in
// at.
const setupNotesExternalRedirect = "{{external_redirect_url}}"

// connectCookieBase is the flow cookie's name without the cookie
// suffix.
const connectCookieBase = "kivali_connect"

// connectCookieTTL is how long the browser keeps the flow cookie: past
// the flow's own ten minutes, so a callback that comes too late still
// finds which integration's page to return to.
const connectCookieTTL = 2 * setupFlowTTL

// connectCookiePrefix starts the signed payload of the flow cookie, so
// no other value the session codec signs reads as one.
const connectCookiePrefix = "connect:"

// Refusals and failures of a Go step's trip and of browser sign-in.
const (
	// errSignInStartAgain: a callback that came to a browser other than
	// the one that left.
	errSignInStartAgain = "this came back to a different browser than the one that left; start it again"
	errSignInNoCode     = "the service sent no sign-in code back; start it again"
	// errSignInReplaced: a browser holds one flow cookie, so a Go step
	// started in it ends the one before.
	errSignInReplaced  = "another setup was started in this browser; start this one again"
	errSignInNoAddress = "Kivali cannot tell the address it was reached at from this request"
	// errSignInNotHere: a browser sign-in started where it cannot
	// finish (browserSignInHere).
	errSignInNotHere = "a sign-in in the browser cannot finish from here; sign in from a browser at the team's own address"
)

// desktopWindowCookie is the cookie Kivali Desktop's team windows set
// in their own web view (TEAM_WINDOW_SCRIPT in
// desktop-app/src-tauri/src/windows.rs), never in a browser's: a
// request that carries it comes from such a window. Anyone can set it;
// all it changes is which sign-in ways the page offers.
const desktopWindowCookie = "kivali_desktop_window"

// fromDesktopWindow reports whether r came from a Kivali Desktop team
// window.
func fromDesktopWindow(r *http.Request) bool {
	c, err := r.Cookie(desktopWindowCookie)
	return err == nil && c.Value == "1"
}

// browserSignInHere reports whether a browser sign-in started by r can
// finish: at a loopback address, where Kivali Desktop's window hands it
// to the system browser at the same address, or at the team's external
// address (externalRedirectURL) outside Kivali Desktop's window. Those
// are the addresses the setup notes give the vendor's app
// ({{redirect_url}} opened there, {{external_redirect_url}}). Anywhere
// else the vendor would send the browser back to an address its app may
// not know, which ends on the vendor's own error page, and Kivali
// Desktop's window for a team elsewhere has no browser signed in to hand
// the sign-in to.
func (s *Server) browserSignInHere(r *http.Request) bool {
	redirect := signInRedirectURL(r)
	u, err := url.Parse(redirect)
	if redirect == "" || err != nil {
		return false
	}
	if auth.LoopbackHost(u.Host) != "" {
		return true
	}
	return redirect == s.externalRedirectURL() && !fromDesktopWindow(r)
}

// signInWaysHere is m's ways (offeredSignInWays) that a sign-in r starts
// can finish by, the manifest's method first: browser only where
// browserSignInHere says so.
func (s *Server) signInWaysHere(m *integration.Manifest, r *http.Request) []string {
	ways := offeredSignInWays(m.Kivali.Connection)
	if s.browserSignInHere(r) {
		return ways
	}
	return slices.DeleteFunc(ways, func(w string) bool { return w == integration.MethodBrowser })
}

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
// r reached Kivali: nil when m does not offer browser sign-in, or a
// browser sign-in r started could not finish (browserSignInHere).
func (s *Server) browserRedirectURL(m *integration.Manifest, r *http.Request) *string {
	if !offersBrowser(m) || !s.browserSignInHere(r) {
		return nil
	}
	return optString(signInRedirectURL(r))
}

// externalHost is the host of the team's external address
// (ExternalURL, an https origin), lower-cased and without :443. Empty
// when the team has no external address.
func (s *Server) externalHost() string {
	u, err := url.Parse(s.ExternalURL)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	return callbackHost(u.Host, "https")
}

// externalRedirectURL is the redirect address for browser sign-in at
// the team's external address: https://<host>/connect/callback. Empty
// when the team has no external address.
func (s *Server) externalRedirectURL() string {
	host := s.externalHost()
	if host == "" {
		return ""
	}
	return "https://" + host + connectCallbackPath
}

// externalOrigin is the team's external address, https://<host>, where
// a browser signs a connection in when it cannot be signed in from
// where the owner is; nil when the team has none.
func (s *Server) externalOrigin() *string {
	host := s.externalHost()
	if host == "" {
		return nil
	}
	return optString("https://" + host)
}

// setupNotes is m's kivali.setup_notes with {{redirect_url}} replaced
// by the redirect address r reached Kivali at, and
// {{external_redirect_url}} by the one at the team's external address:
// empty when the team has none, or when r reached Kivali there, so
// notes that name both never name one address twice.
func (s *Server) setupNotes(m *integration.Manifest, r *http.Request) string {
	notes := m.Kivali.SetupNotes
	here := signInRedirectURL(r)
	if strings.Contains(notes, setupNotesRedirect) {
		notes = strings.ReplaceAll(notes, setupNotesRedirect, here)
	}
	if strings.Contains(notes, setupNotesExternalRedirect) {
		ext := s.externalRedirectURL()
		if ext == here {
			ext = ""
		}
		notes = strings.ReplaceAll(notes, setupNotesExternalRedirect, ext)
	}
	return notes
}

// newOAuthState is a fresh OAuth state: 32 random bytes, base64url.
func newOAuthState() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// connectCookieName is the flow cookie's name, with the
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

// connectCodec signs the flow cookie: the session codec, or,
// on a server given none, one with a key of its own for the life of
// the process, which is as long as any flow lasts.
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

// connectCookie is the cookie that ties the flow id of name's flow to
// the browser that left on its Go step. Its path is the whole site, not
// the callback alone: the next Go's /connect/start reads it to end the
// flow it replaces. Secure when the callback address is https.
func (s *Server) connectCookie(id, name, redirectURL string) *http.Cookie {
	return &http.Cookie{
		Name:     s.connectCookieName(),
		Value:    s.connectCodec().Sign([]byte(connectCookiePrefix + id + ":" + name)),
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(redirectURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(connectCookieTTL / time.Second),
	}
}

// readConnectCookie is the flow id and the integration's name r's
// flow cookie names; empty when it has none or it is not one Kivali
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
