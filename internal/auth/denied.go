package auth

import (
	"net/http"
	"time"
)

// DeniedCookieName is the cookie a refused desktop sign-in leaves for
// Kivali Desktop: the Google account the allowlist did not admit, so the
// app can say whose account isn't invited ("sam@example.com isn't invited
// to Plainsong") while the email stays out of the URL. The app reads it
// from its webview's cookie store once the not-invited page has loaded.
const DeniedCookieName = "kivali_denied"

// DeniedCookieTTL is how long the refused account is kept: long enough
// for the app to read it, short enough to be gone soon after.
const DeniedCookieTTL = 2 * time.Minute

// DeniedCookieNameFor is DeniedCookieName for a server whose cookies
// carry suffix (KIVALI_COOKIE_SUFFIX), as SessionCookieName does.
func DeniedCookieNameFor(suffix string) string { return withCookieSuffix(DeniedCookieName, suffix) }

// setDeniedCookie records the refused account for the not-invited page
// only: HttpOnly, scoped to NotInvitedPath, two minutes.
func setDeniedCookie(w http.ResponseWriter, r *http.Request, suffix, email string) {
	http.SetCookie(w, &http.Cookie{
		Name:     DeniedCookieNameFor(suffix),
		Value:    email,
		Path:     NotInvitedPath,
		HttpOnly: true,
		Secure:   !isLocal(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(DeniedCookieTTL.Seconds()),
	})
}
