package connect

// register.go — core registering itself as a vendor's client (RFC 7591)
// and forgetting the registration (RFC 7592), for a manifest's client:
// registered.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// RegisterRequest is what Register asks the vendor for.
type RegisterRequest struct {
	// RedirectURIs are every address the client signs in at.
	RedirectURIs []string
	// ClientName is the name the vendor shows the owner ("Kivali Acme").
	ClientName string
	// Scope is the scopes asked for, space-separated; empty sends none.
	Scope string
	// AuthMethods are how the token endpoint lets a client authenticate
	// (Metadata.AuthMethods), from which ChooseAuthMethod picks.
	AuthMethods []string
}

// Registration is the client a vendor registered.
type Registration struct {
	// Client is the client's id, its secret (empty for a public client)
	// and how it authenticates at the token endpoint.
	Client Client
	// SecretExpiresAt is when the client secret expires; zero when it
	// does not.
	SecretExpiresAt time.Time
	// RedirectURIs are the addresses registered.
	RedirectURIs []string
	// ClientURI and AccessToken are where and with what the
	// registration is read or deleted (RFC 7592), both empty when the
	// vendor gave either not, or gave an address on a host Kivali does
	// not send to.
	ClientURI, AccessToken string
}

// ChooseAuthMethod is how a client registers to authenticate at a token
// endpoint that lists supported: client_secret_basic where it is listed
// or nothing is (RFC 8414's default), else client_secret_post, else
// none (a public client). A list with none of those is an error.
func ChooseAuthMethod(supported []string) (string, error) {
	switch {
	case len(supported) == 0 || slices.Contains(supported, AuthSecretBasic):
		return AuthSecretBasic, nil
	case slices.Contains(supported, AuthSecretPost):
		return AuthSecretPost, nil
	case slices.Contains(supported, AuthNone):
		return AuthNone, nil
	}
	return "", fmt.Errorf("the service lets a client sign in to its token endpoint only by %s, which Kivali does not", vendorValue(strings.Join(supported, ", ")))
}

// registrationAnswer is a registration endpoint's answer (RFC 7591,
// 3.2), the parts Kivali reads, or its error (3.2.2).
type registrationAnswer struct {
	ClientID                string          `json:"client_id"`
	ClientSecret            string          `json:"client_secret"`
	ClientSecretExpiresAt   json.RawMessage `json:"client_secret_expires_at"`
	AuthMethod              string          `json:"token_endpoint_auth_method"`
	RedirectURIs            []string        `json:"redirect_uris"`
	RegistrationAccessToken string          `json:"registration_access_token"`
	RegistrationClientURI   string          `json:"registration_client_uri"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// RegistrationRefused is a registration endpoint's refusal, in its
// words.
type RegistrationRefused struct {
	// Code is the vendor's error code (invalid_redirect_uri, ...).
	Code string
	// Description is the vendor's own words, at most
	// MaxDescriptionRunes; may be empty.
	Description string
}

func (r *RegistrationRefused) Error() string {
	if r.Description != "" {
		return "the service refused to register Kivali (" + r.Code + "): " + r.Description
	}
	return "the service refused to register Kivali (" + r.Code + ")"
}

// Register registers a client at the registration endpoint (RFC 7591):
// a JSON POST of req's redirect addresses, its name, the authorization
// code and refresh token grants, the code response type, the token
// endpoint authentication ChooseAuthMethod picks from req.AuthMethods,
// and req.Scope when set. The endpoint must be fetchable on one of
// hosts. The answer's client id is required; its token endpoint
// authentication, when it names one, replaces the one asked for and
// must be one Kivali uses; a client with no secret is a public one
// (AuthNone); an answer that lists redirect addresses must list every
// one asked for. The vendor's refusal is a *RegistrationRefused. The
// request is bounded by RequestTimeout on the engine's clock and by
// maxMetadataBytes, and follows no redirect.
func (e *Engine) Register(ctx context.Context, endpoint string, req RegisterRequest, hosts []string) (*Registration, error) {
	u, err := url.Parse(endpoint)
	switch {
	case endpoint == "":
		return nil, fmt.Errorf("the service's metadata names no registration endpoint, so Kivali cannot register itself")
	case err != nil || !fetchable(u) || !hostAllowed(u, hosts):
		return nil, fmt.Errorf("%s is not an address Kivali registers itself at", endpoint)
	}
	method, err := ChooseAuthMethod(req.AuthMethods)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"redirect_uris":              req.RedirectURIs,
		"client_name":                req.ClientName,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
	}
	if req.Scope != "" {
		body["scope"] = req.Scope
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(u.Host)
	ctx, done := e.bounded(ctx)
	defer done()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	resp, err := e.clientWith(noRedirects).Do(hr)
	if err != nil {
		return nil, requestFailure(ctx, host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := readBounded(ctx, host, resp.Body)
	if err != nil {
		return nil, err
	}
	var a registrationAnswer
	jerr := json.Unmarshal(raw, &a)
	switch {
	case jerr == nil && a.Error != "":
		return nil, &RegistrationRefused{Code: clipRunes(a.Error, maxCodeRunes), Description: clipRunes(a.ErrorDescription, MaxDescriptionRunes)}
	case resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered %s to Kivali's registration", host, statusWords(resp.StatusCode))
	case jerr != nil:
		return nil, fmt.Errorf("%s's answer to Kivali's registration is not one Kivali reads", host)
	case a.ClientID == "":
		return nil, fmt.Errorf("%s registered Kivali but gave no client id", host)
	}
	reg := &Registration{Client: Client{ID: a.ClientID, Secret: a.ClientSecret, AuthMethod: method}, RedirectURIs: req.RedirectURIs}
	if a.AuthMethod != "" {
		if !slices.Contains([]string{AuthSecretBasic, AuthSecretPost, AuthNone}, a.AuthMethod) {
			return nil, fmt.Errorf("%s registered Kivali to sign in to its token endpoint by %s, which Kivali does not", host, vendorValue(a.AuthMethod))
		}
		reg.Client.AuthMethod = a.AuthMethod
	}
	if a.ClientSecret == "" {
		reg.Client.AuthMethod = AuthNone
	}
	if len(a.RedirectURIs) > 0 {
		for _, want := range req.RedirectURIs {
			if !slices.Contains(a.RedirectURIs, want) {
				return nil, fmt.Errorf("%s did not register %s, the address Kivali signs in at", host, want)
			}
		}
	}
	if n := int64(seconds(a.ClientSecretExpiresAt) / time.Second); n > 0 {
		reg.SecretExpiresAt = time.Unix(n, 0).UTC()
	}
	if cu, err := url.Parse(a.RegistrationClientURI); err == nil && a.RegistrationAccessToken != "" && fetchable(cu) && hostAllowed(cu, hosts) {
		reg.ClientURI, reg.AccessToken = a.RegistrationClientURI, a.RegistrationAccessToken
	}
	return reg, nil
}

// Deregister deletes a registration (RFC 7592, 2.3): a DELETE of its
// client address with its registration access token. A 204 or 200 is
// done, and so is a 404 or 401 (the vendor no longer knows it); any
// other answer is an error. The request is bounded by RequestTimeout
// on the engine's clock and follows no redirect.
func (e *Engine) Deregister(ctx context.Context, clientURI, accessToken string) error {
	u, err := url.Parse(clientURI)
	if err != nil || !fetchable(u) || accessToken == "" {
		return fmt.Errorf("%q is not a registration Kivali can delete", clientURI)
	}
	host := strings.ToLower(u.Host)
	ctx, done := e.bounded(ctx)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, clientURI, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := e.clientWith(noRedirects).Do(req)
	if err != nil {
		return requestFailure(ctx, host, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetadataBytes))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound, http.StatusUnauthorized:
		return nil
	}
	return fmt.Errorf("%s answered %s to deleting Kivali's registration", host, statusWords(resp.StatusCode))
}
