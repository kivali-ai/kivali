package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HandoffPath is where a handoff token signs the desktop app's webview
// in: GET /auth/handoff?t=<token> (docs/developers/auth.md, Desktop handoff).
const HandoffPath = "/auth/handoff"

// HandoffTTL is how long a handoff token the supervisor mints stays
// valid.
const HandoffTTL = 2 * time.Minute

// handoffMaxAhead bounds a token's exp from above: a token that claims
// to stay valid longer than this was not minted by the supervisor.
const handoffMaxAhead = 5 * time.Minute

// handoffDomain prefixes everything a handoff signature covers, so a
// handoff token can never pass as a session cookie or a sign-in state
// signed with the same key, nor either of those as a handoff token.
const handoffDomain = "kivali-handoff\n"

// handoffClaim is a handoff token's payload.
type handoffClaim struct {
	Email string `json:"email"`
	Exp   int64  `json:"exp"`
	Nonce string `json:"nonce"`
}

// NewHandoffToken mints a one-time token that signs email in on the
// server whose SESSION_KEY is key, until exp: <payload>.<sig>, where
// payload is base64url(JSON {email, exp, nonce}) and sig is
// base64url(HMAC-SHA256(key, "kivali-handoff\n" + payload)).
func NewHandoffToken(key []byte, email string, exp time.Time) (string, error) {
	if len(key) < 16 {
		return "", errors.New("auth: session key must be at least 16 bytes")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", errors.New("auth: handoff needs an email")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	body, err := json.Marshal(handoffClaim{Email: email, Exp: exp.Unix(), Nonce: base64.RawURLEncoding.EncodeToString(nonce)})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + handoffSig(key, payload), nil
}

func handoffSig(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(handoffDomain + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// HandoffRefusal is why a handoff token was refused: a cause for the
// log and tests, and the words a person reads.
type HandoffRefusal struct {
	// Cause is one of the HandoffRefused* constants.
	Cause string
	// Skew, for HandoffRefusedExpired and HandoffRefusedTooFarAhead, is
	// how far the clock that minted the token stands from this
	// server's: positive when the minter's clock is ahead. A token is
	// minted HandoffTTL ahead of the minter's clock, so an expiry that
	// stands elsewhere than HandoffTTL ahead of this clock measures the
	// two clocks' disagreement.
	Skew time.Duration
}

// The causes of a HandoffRefusal.
const (
	// HandoffRefusedMalformed: not a token, or not signed with this
	// server's session key.
	HandoffRefusedMalformed = "malformed"
	// HandoffRefusedExpired: the token's expiry has passed on this
	// server's clock.
	HandoffRefusedExpired = "expired"
	// HandoffRefusedTooFarAhead: the token's expiry stands further ahead
	// than any the supervisor mints, so the minter's clock is ahead of
	// this server's.
	HandoffRefusedTooFarAhead = "too far ahead"
	// HandoffRefusedReplayed: the token was redeemed before.
	HandoffRefusedReplayed = "replayed"
	// HandoffRefusedNotInvited: the token's account is not on the
	// allowlist.
	HandoffRefusedNotInvited = "not invited"
)

// handoffLateness is how far past its expiry a token is taken as opened
// too late rather than as a sign that the minter's clock is behind.
const handoffLateness = 3 * time.Minute

// Error is r's cause with its measure: "too far ahead by 10h0m17s".
func (r HandoffRefusal) Error() string {
	switch r.Cause {
	case HandoffRefusedExpired, HandoffRefusedTooFarAhead:
		return r.Cause + " by " + r.Skew.Abs().Round(time.Second).String()
	}
	return r.Cause
}

// Words is r for a person, a clause to follow "could not be signed
// in:". For a token whose expiry stands far from this server's clock
// they name the two clocks' disagreement, which is what a person can
// put right.
func (r HandoffRefusal) Words() string {
	switch r.Cause {
	case HandoffRefusedTooFarAhead:
		return "the team's clock is about " + aboutDuration(r.Skew) + " behind this computer's"
	case HandoffRefusedExpired:
		if r.Skew <= -handoffLateness {
			return "the team's clock is about " + aboutDuration(r.Skew) + " ahead of this computer's"
		}
		return "the sign-in link was opened too late"
	case HandoffRefusedReplayed:
		return "the sign-in link was already used"
	case HandoffRefusedNotInvited:
		return "the account the link signs in is not invited to this team"
	}
	return "the sign-in link was not issued by this team"
}

// aboutDuration is d's size in round words: "10 hours", "3 minutes",
// "40 seconds".
func aboutDuration(d time.Duration) string {
	d = d.Abs()
	unit, n := "second", int64(d.Round(time.Second)/time.Second)
	switch {
	case d >= time.Hour:
		unit, n = "hour", int64(d.Round(time.Hour)/time.Hour)
	case d >= time.Minute:
		unit, n = "minute", int64(d.Round(time.Minute)/time.Minute)
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// verifyHandoffToken checks token's signature under key and its expiry
// at now, and returns its claim, or why it is refused.
func verifyHandoffToken(key []byte, token string, now time.Time) (handoffClaim, *HandoffRefusal) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" {
		return handoffClaim{}, &HandoffRefusal{Cause: HandoffRefusedMalformed}
	}
	if !hmac.Equal([]byte(handoffSig(key, payload)), []byte(sig)) {
		return handoffClaim{}, &HandoffRefusal{Cause: HandoffRefusedMalformed}
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return handoffClaim{}, &HandoffRefusal{Cause: HandoffRefusedMalformed}
	}
	var c handoffClaim
	if err := json.Unmarshal(body, &c); err != nil || c.Email == "" || c.Nonce == "" {
		return handoffClaim{}, &HandoffRefusal{Cause: HandoffRefusedMalformed}
	}
	exp := time.Unix(c.Exp, 0)
	// The minter set exp to its own now plus HandoffTTL.
	skew := exp.Sub(now) - HandoffTTL
	if !now.Before(exp) {
		return handoffClaim{}, &HandoffRefusal{Cause: HandoffRefusedExpired, Skew: skew}
	}
	if exp.Sub(now) > handoffMaxAhead {
		return handoffClaim{}, &HandoffRefusal{Cause: HandoffRefusedTooFarAhead, Skew: skew}
	}
	return c, nil
}

// Handoff serves HandoffPath: it trades a one-time token the
// supervisor minted from this server's session key for a session, so
// the desktop app opens a team it just set up already signed in as the
// owner who signed in during setup. Only loopback requests are served;
// each token works once.
type Handoff struct {
	Codec     *Codec
	Allowlist *Allowlist
	// CookieSuffix is KIVALI_COOKIE_SUFFIX, as on GoogleOAuth.
	CookieSuffix string
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu   sync.Mutex
	used map[string]time.Time // nonce -> its token's expiry
}

func (h *Handoff) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// HandoffNext resolves a handoff's next query parameter once its token
// has passed: where to send the browser, an address the server itself
// made and never one taken from the request, or "" when next names
// nothing to go on to, which refuses the handoff. It may set cookies on
// w.
type HandoffNext func(w http.ResponseWriter, r *http.Request, next string) string

// HandoffRefused is told of a refused token that came with a next, so
// what next names can learn why its browser never arrived: it answers
// where to send the browser instead of "/", an address the server
// itself made, or "" for "/". It signs nothing in and sets no cookie;
// next is unverified, from a request whose token failed.
type HandoffRefused func(r *http.Request, next string, why HandoffRefusal) string

// ServeHTTP signs the request in on a valid token and redirects to "/".
// Any failure redirects to "/" without a session, where the ordinary
// sign-in takes over. The token is never logged.
func (h *Handoff) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, nil, nil)
}

// WithNext is the handoff route with next resolved by resolve: a
// request whose token passes and that carries a next is signed in and
// sent where resolve says, or, when resolve finds nothing, refused as
// a refused token is: no session, a 302 to "/". A refused token signs
// nothing in whatever its next; refused, when not nil, is told of the
// pair and may name where the browser goes instead of "/".
func (h *Handoff) WithNext(resolve HandoffNext, refused HandoffRefused) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, resolve, refused)
	})
}

func (h *Handoff) serve(w http.ResponseWriter, r *http.Request, resolve HandoffNext, refused HandoffRefused) {
	if !isLocal(r) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	email, why := h.redeem(r.URL.Query().Get("t"))
	if why != nil {
		log.Printf("auth handoff: refused a handoff token: %v", why)
		target := "/"
		if next := r.URL.Query().Get("next"); next != "" && refused != nil {
			if t := refused(r, next, *why); t != "" {
				target = t
			}
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	signed, err := h.Codec.EncodeSession(NewSession(email))
	if err != nil {
		log.Printf("auth handoff: session encode: %v", err)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	target := "/"
	if next := r.URL.Query().Get("next"); next != "" && resolve != nil {
		// A next with nowhere to go is no errand of the app's: refused
		// as a refused token is, with no session.
		if target = resolve(w, r, next); target == "" {
			log.Printf("auth handoff: refused a handoff whose next names nothing")
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}
	setSessionCookie(w, r, h.CookieSuffix, signed)
	http.Redirect(w, r, target, http.StatusFound)
}

// redeem verifies token, spends its nonce and checks the allowlist: the
// email it signs in, or why it is refused.
func (h *Handoff) redeem(token string) (string, *HandoffRefusal) {
	if token == "" || h.Codec == nil {
		return "", &HandoffRefusal{Cause: HandoffRefusedMalformed}
	}
	now := h.now()
	c, why := verifyHandoffToken(h.Codec.key, token, now)
	if why != nil {
		return "", why
	}
	h.mu.Lock()
	for n, exp := range h.used {
		if !now.Before(exp) {
			delete(h.used, n)
		}
	}
	if _, spent := h.used[c.Nonce]; spent {
		h.mu.Unlock()
		return "", &HandoffRefusal{Cause: HandoffRefusedReplayed}
	}
	if h.used == nil {
		h.used = map[string]time.Time{}
	}
	h.used[c.Nonce] = time.Unix(c.Exp, 0)
	h.mu.Unlock()
	if h.Allowlist == nil || !h.Allowlist.Allow(c.Email) {
		return "", &HandoffRefusal{Cause: HandoffRefusedNotInvited}
	}
	return c.Email, nil
}
