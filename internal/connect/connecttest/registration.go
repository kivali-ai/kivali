package connecttest

// registration.go — FakeVendor as an MCP server's authorization server
// is: the protected resource's metadata (RFC 9728) for the resource
// URL + /mcp, the authorization server's (RFC 8414) at the root, a
// registration endpoint (RFC 7591) and deleting a registration (RFC
// 7592). A registered client signs in, refreshes and revokes as the
// client FakeVendor is made with does, at the addresses it registered.

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/connect"
)

// RegisteredClient is a client registered with the vendor.
type RegisteredClient struct {
	ID, Secret, AuthMethod, Name, Scope string
	RedirectURIs                        []string
	// AccessToken is the registration access token.
	AccessToken string
}

// Resource is the protected resource's address, the discover address
// of a registered client's manifest.
func (v *FakeVendor) Resource() string { return v.URL + "/mcp" }

// Registered is every client registered and not deleted, in the order
// registered.
func (v *FakeVendor) Registered() []RegisteredClient {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := []RegisteredClient{}
	for _, c := range v.registered {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b RegisteredClient) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Deregistered is every client id whose registration was deleted, in
// order.
func (v *FakeVendor) Deregistered() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.deregistered)
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (v *FakeVendor) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	k, _ := v.record(r)
	if k.NoResourceMetadata {
		http.NotFound(w, r)
		return
	}
	doc := map[string]any{
		"resource":                 v.Resource(),
		"authorization_servers":    []any{v.URL},
		"bearer_methods_supported": []any{"header"},
	}
	if k.ResourceMetadata != nil {
		k.ResourceMetadata(doc)
	}
	writeJSON(w, http.StatusOK, doc)
}

func (v *FakeVendor) serverMetadata(w http.ResponseWriter, r *http.Request) {
	k, _ := v.record(r)
	methods := []any{connect.AuthSecretBasic, connect.AuthSecretPost}
	if k.AuthMethods != nil {
		methods = []any{}
		for _, m := range k.AuthMethods {
			methods = append(methods, m)
		}
	}
	doc := map[string]any{
		"issuer":                                v.URL,
		"authorization_endpoint":                v.URL + "/authorize",
		"token_endpoint":                        v.URL + "/token",
		"revocation_endpoint":                   v.URL + "/revoke",
		"registration_endpoint":                 v.URL + "/register",
		"response_types_supported":              []any{"code"},
		"grant_types_supported":                 []any{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []any{"S256"},
		"token_endpoint_auth_methods_supported": methods,
	}
	if k.ServerMetadata != nil {
		k.ServerMetadata(doc)
	}
	writeJSON(w, http.StatusOK, doc)
}

// register registers the client the JSON body describes, as RFC 7591
// has it: its redirect addresses, name, scope and token endpoint
// authentication, answered 201 with its id, its secret unless it is
// public, and where to delete it.
func (v *FakeVendor) register(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
		Scope        string   `json:"scope"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	jerr := json.Unmarshal(body, &req)
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	k, _ := v.record(r)
	v.mu.Lock()
	v.requests[len(v.requests)-1].JSON = raw
	v.mu.Unlock()
	switch {
	case k.RegisterError != "":
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": k.RegisterError, "error_description": "refused by the test"})
		return
	case jerr != nil || len(req.RedirectURIs) == 0:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_redirect_uri", "error_description": "no redirect address"})
		return
	}
	v.mu.Lock()
	c := &RegisteredClient{
		ID: v.nextLocked("registered"), Secret: v.nextLocked("client-secret"), AuthMethod: req.AuthMethod,
		Name: req.ClientName, Scope: req.Scope, RedirectURIs: req.RedirectURIs, AccessToken: v.nextLocked("registration-token"),
	}
	if k.RegisterNoSecret {
		c.Secret, c.AuthMethod = "", connect.AuthNone
	}
	v.registered[c.ID] = c
	v.mu.Unlock()
	out := map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        v.clk.Now().Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []any{"authorization_code", "refresh_token"},
		"response_types":             []any{"code"},
		"token_endpoint_auth_method": c.AuthMethod,
	}
	if c.Secret != "" {
		out["client_secret"] = c.Secret
		out["client_secret_expires_at"] = 0
	}
	if !k.RegisterNoManagement {
		out["registration_access_token"] = c.AccessToken
		out["registration_client_uri"] = v.URL + "/register/" + c.ID
	}
	writeJSON(w, http.StatusCreated, out)
}

// deregister deletes the registration {id} names, given its
// registration access token.
func (v *FakeVendor) deregister(w http.ResponseWriter, r *http.Request) {
	_, _ = v.record(r)
	id := r.PathValue("id")
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.registered[id]
	switch {
	case c == nil:
		http.NotFound(w, r)
		return
	case r.Header.Get("Authorization") != "Bearer "+c.AccessToken:
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	delete(v.registered, id)
	v.deregistered = append(v.deregistered, id)
	w.WriteHeader(http.StatusNoContent)
}
