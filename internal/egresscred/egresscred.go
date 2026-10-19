// Package egresscred is an agent pod's credential for the egress
// proxy. The `kivali agent` runtime draws a secret when it starts,
// registers only its SHA-256 with core, and hands the agent processes
// it starts a proxy URL carrying `<slug>:<secret>` as its user info.
// The proxy gives a request that presents a registered credential open
// access to the public internet; a request without one (the agent's
// shell) stays on the allowlist. The secret is held in memory only, and
// a Scrubber replaces it in everything the runtime relays to core or
// writes to disk.
//
// It imports nothing from this module.
package egresscred

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// SecretBytes is how many random bytes a secret holds.
const SecretBytes = 32

// NewSecret returns SecretBytes from crypto/rand, base64url without
// padding.
func NewSecret() (string, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("egresscred: secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash is what core keeps for secret: the lowercase hex SHA-256 of its
// bytes as written.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash reports whether h is a Hash: 64 lowercase hex characters.
func ValidHash(h string) bool { return hashPattern.MatchString(h) }

// ProxyEnvKeys are the variables an agent process reads its proxy from,
// in both cases so every common client picks it up.
var ProxyEnvKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"}

// ProxyEnv is ProxyEnvKeys set to proxyURL, as KEY=value entries.
func ProxyEnv(proxyURL string) []string {
	env := make([]string, 0, len(ProxyEnvKeys))
	for _, k := range ProxyEnvKeys {
		env = append(env, k+"="+proxyURL)
	}
	return env
}

// ProxyURL is bare (the pod's proxy URL, with no user info) carrying
// slug and secret as its user info.
func ProxyURL(bare, slug, secret string) (string, error) {
	u, err := url.Parse(bare)
	if err != nil {
		return "", fmt.Errorf("egresscred: proxy URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("egresscred: proxy URL %q has no scheme or host", bare)
	}
	if u.User != nil {
		return "", errors.New("egresscred: proxy URL already carries credentials")
	}
	if slug == "" || secret == "" {
		return "", errors.New("egresscred: slug and secret are required")
	}
	u.User = url.UserPassword(slug, secret)
	return u.String(), nil
}

// Credential is one runtime's credential: the proxy URL its agent
// processes get and the scrubber that keeps the secret out of what it
// relays.
type Credential struct {
	Slug string
	// Hash is what core is told (Hash of the secret).
	Hash string
	// URL is the proxy URL with the credential in it.
	URL string
	// Scrubber replaces the secret, in each form it can be written,
	// with Placeholder.
	Scrubber *Scrubber
}

// New draws a secret for slug and builds its Credential on bare, the
// pod's proxy URL.
func New(bare, slug string) (*Credential, error) {
	secret, err := NewSecret()
	if err != nil {
		return nil, err
	}
	return newCredential(bare, slug, secret)
}

func newCredential(bare, slug, secret string) (*Credential, error) {
	u, err := ProxyURL(bare, slug, secret)
	if err != nil {
		return nil, err
	}
	return &Credential{
		Slug:     slug,
		Hash:     Hash(secret),
		URL:      u,
		Scrubber: NewScrubber(slug, secret, u),
	}, nil
}

// Env is the proxy environment an agent process gets: ProxyEnvKeys set
// to the credentialed URL.
func (c *Credential) Env() []string { return ProxyEnv(c.URL) }

// BareProxyURL is v, a proxy variable's value, as a shell may have
// it: with no user info. A value holding an "@" that does not parse
// as a URL with user info is dropped (empty).
func BareProxyURL(v string) string {
	if !strings.Contains(v, "@") {
		return v
	}
	u, err := url.Parse(v)
	if err != nil || u.User == nil || u.Host == "" {
		return ""
	}
	u.User = nil
	return u.String()
}

// ScrubberFromEnv is the scrubber for the credential in the proxy
// variables of an environment (getenv), for a process that inherited
// them from the runtime rather than drawing them. Nil when none of them
// carries a password.
func ScrubberFromEnv(getenv func(string) string) *Scrubber {
	for _, k := range ProxyEnvKeys {
		raw := getenv(k)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			continue
		}
		if secret, ok := u.User.Password(); ok && secret != "" {
			return NewScrubber(u.User.Username(), secret, raw)
		}
	}
	return nil
}
