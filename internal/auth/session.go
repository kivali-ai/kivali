// Package auth handles Google OAuth login, an email allowlist, and the
// middleware that enforces authentication on protected routes.
//
// Surface:
//   - HMAC-signed session cookies (no server-side session store).
//   - An allowlist of the team's owner (OWNER_EMAILS); nobody else
//     signs in.
//   - No bypass mode — every request goes through the same code path
//     in dev, prod, and tests. Tests mint cookies via Codec.EncodeSession.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// CookieName is the session cookie's name on a server started without a
// cookie suffix. SessionCookieName gives the name for any suffix.
const CookieName = "kivali_session"

// SessionCookieName is the session cookie's name for a server whose
// cookies carry suffix (KIVALI_COOKIE_SUFFIX): CookieName when suffix
// is empty, else CookieName + "_" + suffix. Browsers key cookies by
// host and ignore the port, so two servers on one host need different
// names or each signs the other's users out.
func SessionCookieName(suffix string) string { return withCookieSuffix(CookieName, suffix) }

func withCookieSuffix(name, suffix string) string {
	if suffix == "" {
		return name
	}
	return name + "_" + suffix
}

// SessionTTL is how long a session cookie stays valid.
const SessionTTL = 24 * time.Hour

// Session is the minimal identity we persist in the browser.
type Session struct {
	Email     string    `json:"email"`
	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
}

// NewSession returns a session for email with fresh timestamps.
func NewSession(email string) Session {
	now := time.Now().UTC()
	return Session{
		Email:     strings.ToLower(strings.TrimSpace(email)),
		IssuedAt:  now,
		ExpiresAt: now.Add(SessionTTL),
	}
}

// Codec signs and verifies opaque payloads with HMAC-SHA256.
//
// Use Sign/Verify for arbitrary bytes (e.g. OAuth state); use
// EncodeSession/DecodeSession for Session values.
type Codec struct {
	key []byte
}

// NewCodec returns a codec using the given key. Key must be at least 16
// bytes; use a longer random value in production (e.g. 32 bytes).
func NewCodec(key []byte) (*Codec, error) {
	if len(key) < 16 {
		return nil, errors.New("auth: session key must be at least 16 bytes")
	}
	return &Codec{key: append([]byte(nil), key...)}, nil
}

// Sign returns base64(payload) + "." + base64(hmac).
func (c *Codec) Sign(payload []byte) string {
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + sig
}

// Verify checks the signature and returns the original payload.
func (c *Codec) Verify(signed string) ([]byte, error) {
	parts := strings.SplitN(signed, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("auth: bad token format")
	}
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return nil, errors.New("auth: bad signature")
	}
	return base64.RawURLEncoding.DecodeString(parts[0])
}

// EncodeSession signs a Session value for use as a cookie value.
func (c *Codec) EncodeSession(s Session) (string, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return c.Sign(body), nil
}

// DecodeSession verifies a cookie value and returns the Session. Returns
// an error if the signature is invalid, the body is malformed, or the
// session is expired.
func (c *Codec) DecodeSession(token string) (Session, error) {
	body, err := c.Verify(token)
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(body, &s); err != nil {
		return Session{}, err
	}
	if time.Now().After(s.ExpiresAt) {
		return Session{}, errors.New("auth: session expired")
	}
	return s, nil
}
