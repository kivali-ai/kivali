package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// verifyHandoffToken checks token's signature under key and its expiry
// at now, and returns its claim.
func verifyHandoffToken(key []byte, token string, now time.Time) (handoffClaim, error) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok || payload == "" || sig == "" {
		return handoffClaim{}, errors.New("bad token format")
	}
	if !hmac.Equal([]byte(handoffSig(key, payload)), []byte(sig)) {
		return handoffClaim{}, errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return handoffClaim{}, errors.New("bad payload")
	}
	var c handoffClaim
	if err := json.Unmarshal(body, &c); err != nil || c.Email == "" || c.Nonce == "" {
		return handoffClaim{}, errors.New("bad payload")
	}
	exp := time.Unix(c.Exp, 0)
	if !now.Before(exp) {
		return handoffClaim{}, errors.New("expired")
	}
	if exp.Sub(now) > handoffMaxAhead {
		return handoffClaim{}, errors.New("expiry too far ahead")
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

// ServeHTTP signs the request in on a valid token and redirects to "/".
// Any failure redirects to "/" without a session, where the ordinary
// sign-in takes over. The token is never logged.
func (h *Handoff) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLocal(r) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	email, ok := h.redeem(r.URL.Query().Get("t"))
	if !ok {
		log.Printf("auth handoff: refused a handoff token")
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	signed, err := h.Codec.EncodeSession(NewSession(email))
	if err != nil {
		log.Printf("auth handoff: session encode: %v", err)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	setSessionCookie(w, r, h.CookieSuffix, signed)
	http.Redirect(w, r, "/", http.StatusFound)
}

// redeem verifies token, spends its nonce and checks the allowlist.
func (h *Handoff) redeem(token string) (string, bool) {
	if token == "" || h.Codec == nil {
		return "", false
	}
	now := h.now()
	c, err := verifyHandoffToken(h.Codec.key, token, now)
	if err != nil {
		return "", false
	}
	h.mu.Lock()
	for n, exp := range h.used {
		if !now.Before(exp) {
			delete(h.used, n)
		}
	}
	if _, spent := h.used[c.Nonce]; spent {
		h.mu.Unlock()
		return "", false
	}
	if h.used == nil {
		h.used = map[string]time.Time{}
	}
	h.used[c.Nonce] = time.Unix(c.Exp, 0)
	h.mu.Unlock()
	if h.Allowlist == nil || !h.Allowlist.Allow(c.Email) {
		return "", false
	}
	return c.Email, true
}
