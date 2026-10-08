package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// DefaultLoginPath is used by Middleware when LoginPath is empty.
const DefaultLoginPath = "/auth/login"

type ctxKey struct{}

var userKey = ctxKey{}

// Middleware enforces authentication on protected routes. There is no
// bypass — every request must carry a valid HMAC-signed session cookie
// for an allowlisted email. Tests construct a session via Codec.EncodeSession.
type Middleware struct {
	Codec     *Codec
	Allowlist *Allowlist
	LoginPath string // defaults to DefaultLoginPath
	// CookieSuffix is KIVALI_COOKIE_SUFFIX; the session cookie is read
	// under SessionCookieName(CookieSuffix). It must match the
	// GoogleOAuth's that sets the cookie.
	CookieSuffix string
}

// SessionCookieName is the session cookie this middleware reads. Safe
// on a nil receiver, which gives the unsuffixed name.
func (m *Middleware) SessionCookieName() string {
	if m == nil {
		return CookieName
	}
	return SessionCookieName(m.CookieSuffix)
}

// APIPrefix is the path prefix of the JSON API. Requests under it are
// answered with JSON errors instead of redirects: a fetch() that
// follows a 302 to the login page gets an HTML 200, which the client
// cannot tell from a real answer.
const APIPrefix = "/api/"

// Wrap returns an http.Handler enforcing auth. Unauthenticated GET
// requests are redirected to LoginPath?next=<original>; other methods
// receive 401. Under APIPrefix every unauthenticated request gets a
// JSON 401 and a signed-in email the allowlist refuses gets a JSON 403.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	loginPath := m.LoginPath
	if loginPath == "" {
		loginPath = DefaultLoginPath
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(m.SessionCookieName())
		if err != nil {
			redirectToLogin(w, r, loginPath)
			return
		}
		sess, err := m.Codec.DecodeSession(c.Value)
		if err != nil {
			redirectToLogin(w, r, loginPath)
			return
		}
		if !m.Allowlist.Allow(sess.Email) {
			if isAPIRequest(r) {
				writeAPIAuthError(w, http.StatusForbidden, "this account is not allowed in", "an owner of this org can add you")
				return
			}
			http.Error(w, "forbidden: "+sess.Email, http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, sess.Email)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func redirectToLogin(w http.ResponseWriter, r *http.Request, loginPath string) {
	if isAPIRequest(r) {
		writeAPIAuthError(w, http.StatusUnauthorized, "unauthenticated", "sign in again")
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	next := r.URL.RequestURI()
	if strings.HasPrefix(next, loginPath) {
		next = "/"
	}
	q := url.Values{}
	q.Set("next", next)
	http.Redirect(w, r, loginPath+"?"+q.Encode(), http.StatusFound)
}

// isAPIRequest reports whether r is for the JSON API.
func isAPIRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, APIPrefix)
}

// writeAPIAuthError writes the API's error shape: what happened, then
// who can fix it. Mirrors internal/web's writeAPIError, which this
// package cannot import.
func writeAPIAuthError(w http.ResponseWriter, status int, what, who string) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
		Who   string `json:"who"`
	}{what, who})
}

// DevBypass returns a middleware that installs email as the request
// user without checking any credential. It is the DEV_MODE substitute
// for Middleware.Wrap: there is no cookie, no allowlist, and no login
// page in that mode, but downstream handlers still expect a user on the
// context (page headers, handbook-edit attribution).
//
// This must never be reachable in a real deployment. main.go gates it
// behind config.DevMode, which itself refuses to coexist with
// KIVALI_ENV=prod or a non-loopback listen address.
func DevBypass(email string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), userKey, email)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserFromContext returns the authenticated user's email, or "" if none.
func UserFromContext(ctx context.Context) string {
	email, _ := ctx.Value(userKey).(string)
	return email
}
