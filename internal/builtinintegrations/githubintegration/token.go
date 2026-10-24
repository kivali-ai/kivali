package githubintegration

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The JWT's window, on the server's clock: issued a minute in the past
// (GitHub allows for drift between its clock and Kivali's), expiring
// nine minutes ahead (GitHub takes at most ten).
const (
	jwtBackdate = 60 * time.Second
	jwtLifetime = 9 * time.Minute
)

// tokenMargin is how long before its expiry a cached installation
// token stops being handed out.
const tokenMargin = time.Minute

// parsePrivateKey reads the app's private key: PEM, PKCS#1 ("RSA
// PRIVATE KEY", as GitHub issues it) or PKCS#8 ("PRIVATE KEY") holding
// an RSA key.
func parsePrivateKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(s)))
	if block == nil {
		return nil, errors.New("the private key is not PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("the private key is not an RSA key")
		}
		return rk, nil
	}
	return nil, errors.New("the private key is a " + block.Type)
}

// appJWT is the app's JSON web token at now: RS256, iss the app id,
// iat a minute back and exp nine minutes ahead.
func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{now.Add(-jwtBackdate).Unix(), now.Add(jwtLifetime).Unix(), appID})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// jwt is the connection's JWT now.
func (g *GitHub) jwt(c connection) (string, error) {
	return appJWT(c.appID, c.key, g.now())
}

// permissions is what an installation token may do, GitHub's
// permission names to "read" or "write". metadata read is always in
// it: GitHub gives it with any other.
type permissions map[string]string

// key is the permissions in a fixed order, for the cache.
func (p permissions) key() string {
	names := make([]string, 0, len(p))
	for n, level := range p {
		names = append(names, n+"="+level)
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

// perms is metadata read and each name and level of kv.
func perms(kv ...string) permissions {
	p := permissions{"metadata": "read"}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i]] = kv[i+1]
	}
	return p
}

// tokenKey is what a cached token is for. The app and the installation
// are in it besides the connection, so a connection set up again with
// another app never reuses the old one's tokens.
type tokenKey struct {
	conn, app, installation string
	// repo is the repository name the token is narrowed to; "" for one
	// that reaches every repository of the installation.
	repo  string
	perms string
}

type cachedToken struct {
	token string
	// until is when it stops being handed out: a minute before GitHub
	// says it expires.
	until time.Time
}

// installationToken is a token of the connection's installation that
// may do perms, narrowed to the repository owner/name when repo is
// given. One minted earlier for the same connection, repository and
// permissions is handed out again until a minute before it expires;
// tokens live in this memory alone.
func (g *GitHub) installationToken(ctx context.Context, c connection, repo string, p permissions) (string, error) {
	name := ""
	if repo != "" {
		_, name, _ = strings.Cut(repo, "/")
		name = strings.ToLower(name)
	}
	k := tokenKey{conn: c.id, app: c.appID, installation: c.installationID, repo: name, perms: p.key()}
	now := g.now()
	g.tokensMu.Lock()
	if t, ok := g.tokens[k]; ok && now.Before(t.until) {
		g.tokensMu.Unlock()
		return t.token, nil
	}
	g.tokensMu.Unlock()

	jwt, err := g.jwt(c)
	if err != nil {
		return "", err
	}
	body := map[string]any{"permissions": p}
	if name != "" {
		body["repositories"] = []string{name}
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	err = g.sendJSON(ctx, request{method: http.MethodPost, path: "/app/installations/" + c.installationID + "/access_tokens", auth: jwt, body: body}, &out)
	if err != nil {
		var e *apiError
		if errors.As(err, &e) {
			return "", &tokenError{api: e, repo: repo}
		}
		return "", err
	}
	if out.Token == "" {
		return "", &tokenError{api: &apiError{Message: "its answer holds no token"}, repo: repo}
	}
	if exp, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil && now.Before(exp.Add(-tokenMargin)) {
		g.tokensMu.Lock()
		if g.tokens == nil {
			g.tokens = map[tokenKey]cachedToken{}
		}
		for key, t := range g.tokens {
			if !now.Before(t.until) {
				delete(g.tokens, key)
			}
		}
		g.tokens[k] = cachedToken{token: out.Token, until: exp.Add(-tokenMargin)}
		g.tokensMu.Unlock()
	}
	return out.Token, nil
}

// tokenError is GitHub refusing the app an installation token.
type tokenError struct {
	api  *apiError
	repo string
}

func (e *tokenError) Error() string { return "an installation token: " + e.api.Error() }

// words is the refusal in words for the agent.
func (e *tokenError) words() string {
	a := e.api
	msg := sentence(a.Message)
	switch {
	case a.Status == 0 || a.RateLimited:
		return agentWords(a)
	case a.Status == http.StatusUnauthorized:
		return "GitHub refused the GitHub App's key (HTTP 401): " + msg + ". Ask the owner."
	case a.Status == http.StatusNotFound:
		return "GitHub has no installation of the GitHub App that Kivali keeps (HTTP 404): " + msg + ". Ask the owner."
	case a.Status == http.StatusUnprocessableEntity && e.repo != "":
		return "GitHub refused the GitHub App a token for " + e.repo + " (HTTP 422): " + msg +
			". The app may not be installed on it, or lack the permission; ask the owner."
	}
	return "GitHub refused the GitHub App a token (HTTP " + strconv.Itoa(a.Status) + "): " + msg + ". Ask the owner."
}

// Installation is one installation of the app: its id and the account
// it is on.
type Installation struct {
	ID      int64
	Account string
}

// installations lists the app's installations with its JWT (GET
// /app/installations): what setup checks an installation id it was
// handed against.
func (g *GitHub) installations(ctx context.Context, c connection) ([]Installation, error) {
	jwt, err := g.jwt(c)
	if err != nil {
		return nil, err
	}
	var out []Installation
	err = g.pages(ctx, jwt, "/app/installations", maxPages, func(b []byte) (int, bool, error) {
		var page []struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, true, &apiError{Message: "its answer is not JSON Kivali reads", Transport: err}
		}
		for _, in := range page {
			out = append(out, Installation{ID: in.ID, Account: in.Account.Login})
		}
		return len(page), false, nil
	})
	return out, err
}
