package githubintegration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Uninstall removes the app's installation from GitHub (DELETE
// /app/installations/{id}, with the app's JWT). An installation already
// gone (404), and one a connection cannot name (it lacks a value, or
// GitHub no longer takes the key: the app was deleted there), are
// done. A 401 over the JWT's times (this server's clock off from
// GitHub's) is not: the key still names the installation, and GitHub's
// words say what to mend. The app's registration stays on GitHub, for
// the owner to delete.
func (g *GitHub) Uninstall(ctx context.Context, conn builtinintegrations.Connection) error {
	c, err := readConnection(conn)
	if err != nil {
		return nil
	}
	jwt, err := g.jwt(c)
	if err != nil {
		return nil
	}
	err = g.sendJSON(ctx, request{method: http.MethodDelete, path: "/app/installations/" + url.PathEscape(c.installationID), auth: jwt}, nil)
	var ae *apiError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return nil
	case errors.As(err, &ae) && ae.Status == http.StatusUnauthorized && !jwtTimeRefused(ae.Message):
		return nil
	case errors.As(err, &ae) && ae.Status == http.StatusUnauthorized:
		return errors.New("GitHub did not remove the app's installation: it refused the times Kivali signed with (HTTP 401): " + sentence(ae.Message) +
			". Check the clock of the computer Kivali runs on, then uninstall again")
	case errors.As(err, &ae):
		return errors.New("GitHub did not remove the app's installation: " + sentence(ownerWords(ae)))
	}
	return err
}

// jwtTimeRefused reports whether GitHub's words refuse a JWT over its
// time claims ("'Expiration time' claim ('exp') is too far in the
// future", "'Issued at' claim ('iat') must be an Integer"), rather than
// over its key.
func jwtTimeRefused(words string) bool {
	w := strings.ToLower(words)
	for _, s := range []string{"'exp'", "'iat'", "expiration time", "issued at", "too far"} {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}

// DefaultTokenURL is GitHub's OAuth token endpoint, where a sign-in of
// the owner's own app is renewed.
const DefaultTokenURL = "https://github.com/login/oauth/access_token"

// RevokeSignIn revokes, best effort, the sign-in a connection made by
// an earlier version held: the owner's own app's grant to their
// account (DELETE /applications/{client_id}/grant, with the app's
// client id and secret), which ends its access and refresh tokens
// both. GitHub takes a grant's revocation only with a live access
// token, so one that expired is renewed with the refresh token first.
// A grant already gone is done.
func (g *GitHub) RevokeSignIn(ctx context.Context, old builtinintegrations.SignIn) error {
	if old.ClientID == "" || old.ClientSecret == "" {
		return nil
	}
	if old.AccessToken != "" {
		err := g.deleteGrant(ctx, old, old.AccessToken)
		var ae *apiError
		if err == nil || !errors.As(err, &ae) || (ae.Status != http.StatusNotFound && ae.Status != http.StatusUnprocessableEntity) {
			return err
		}
	}
	if old.RefreshToken == "" {
		return nil
	}
	fresh, err := g.renew(ctx, old)
	if err != nil || fresh == "" {
		return err
	}
	err = g.deleteGrant(ctx, old, fresh)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// deleteGrant revokes old's app's grant, named by a live access token.
func (g *GitHub) deleteGrant(ctx context.Context, old builtinintegrations.SignIn, token string) error {
	return g.sendJSON(ctx, request{
		method: http.MethodDelete,
		path:   "/applications/" + url.PathEscape(old.ClientID) + "/grant",
		basic:  &[2]string{old.ClientID, old.ClientSecret},
		body:   map[string]string{"access_token": token},
	}, nil)
}

// renew trades old's refresh token for a fresh access token at
// GitHub's token endpoint (TokenURL). An error names no token.
func (g *GitHub) renew(ctx context.Context, old builtinintegrations.SignIn) (string, error) {
	endpoint := g.TokenURL
	if endpoint == "" {
		endpoint = DefaultTokenURL
	}
	u, err := url.Parse(endpoint)
	github := err == nil && u.Scheme == "https" && u.Host == "github.com"
	local := err == nil && u.Scheme == "http" && loopback(u.Hostname())
	if !github && !local {
		return "", errors.New("the GitHub built-in's token endpoint is not GitHub's")
	}
	form := url.Values{
		"client_id":     {old.ClientID},
		"client_secret": {old.ClientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {old.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Kivali")
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", errors.New("GitHub's token endpoint did not answer")
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", errors.New("GitHub's token endpoint did not answer")
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if json.Unmarshal(b, &out) != nil || resp.StatusCode != http.StatusOK {
		return "", errors.New("GitHub's token endpoint answered " + resp.Status)
	}
	if out.Error != "" {
		// The refresh token no longer works: there is nothing left to
		// revoke with it.
		return "", nil
	}
	return out.AccessToken, nil
}

func loopback(host string) bool { return host == "127.0.0.1" || host == "localhost" || host == "::1" }
