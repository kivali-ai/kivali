// Package web (integration_signout.go) — the ways a sign-in connection
// is offered, and Sign out. Signing in is a setup flow
// (integration_setup_standard.go).

package web

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"slices"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
)

// signInWaysOffered are the ways this server signs a connection in by,
// of those a manifest may offer (integration.Connection.Ways): the
// page is offered only these.
var signInWaysOffered = []string{integration.MethodDevice, integration.MethodBrowser}

// offeredSignInWays is c's ways that this server signs in by, the
// manifest's method first.
func offeredSignInWays(c integration.Connection) []string {
	out := []string{}
	for _, w := range c.Ways() {
		if slices.Contains(signInWaysOffered, w) {
			out = append(out, w)
		}
	}
	return out
}

// errSignInSignedIn refuses a sign-in of a connection signed in.
const errSignInSignedIn = "the connection is signed in; sign out first"

// newSignInFlowID is a fresh flow id: 16 random bytes in hex.
func newSignInFlowID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// signInTarget is the sign-in connection Sign out acts on: the
// integration's. It answers 409 for an integration whose connection is
// not made by signing in, one this version does not carry, and one
// with no approved version or no connection yet.
func (s *Server) signInTarget(w http.ResponseWriter, r *http.Request, v *integrationView) (*connView, bool) {
	switch {
	case v.staleBuiltin():
		writeAPIError(w, http.StatusConflict, builtinRefusal(v.displayName(), builtinNotIncluded), whoYou)
		return nil, false
	case v.it.CurrentVersion == "":
		writeAPIError(w, http.StatusConflict, "approve a version of "+v.displayName()+" first", whoYou)
		return nil, false
	case !v.signIn():
		writeAPIError(w, http.StatusConflict, v.displayName()+" is not connected by signing in", whoYou)
		return nil, false
	}
	c := v.conn
	if c == nil {
		writeAPIError(w, http.StatusConflict, v.displayName()+" has no connection", whoYou)
		return nil, false
	}
	return c, true
}

// handleAPISignOut signs out the integration's connection: it ends a flow under way, removes the tokens from
// the connection's Secret (its fields stay, the client id and secret
// among them; a client Kivali registered goes too), clears the sign-in
// and the last connection test from its record, asks the vendor to
// revoke the tokens and delete a registered client (best effort), appends
// the owner's audit row and starts the bundle's server again without
// the token. It answers with the page. The vendor is asked to revoke
// with lockIntegrations released, so a slow vendor holds up no other
// integration write. Of a connection not signed in that holds no
// tokens, only a flow under way is ended.
func (s *Server) handleAPISignOut(w http.ResponseWriter, r *http.Request) {
	unlock := s.lockIntegrations()
	revokes, ok := s.signOutLocked(w, r)
	unlock()
	s.revokeSignIns(r.Context(), revokes)
	if ok {
		s.writeIntegrationPage(w, r)
	}
}

// signOutLocked is Sign out under lockIntegrations: true once done,
// false with an error answered. It returns the tokens to revoke.
func (s *Server) signOutLocked(w http.ResponseWriter, r *http.Request) (signInRevocations, bool) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return nil, false
	}
	c, ok := s.signInTarget(w, r, v)
	if !ok {
		return nil, false
	}
	name, connID := v.it.Name, c.rec.ID
	start := s.clk().Now()
	s.setupCancelKeep(name)
	rec, data, err := s.readSignIn(r.Context(), name, connID)
	if err != nil {
		log.Printf("integration %s: read connection %s: %v", name, connID, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be read, so nothing was changed", whoServer)
		return nil, false
	}
	// Nothing signed in and no tokens: nothing to sign out of. The
	// server keeps running and the last test stays.
	if rec.SignIn == nil && data[connect.SecretKeyAccessToken] == "" && data[connect.SecretKeyRefreshToken] == "" {
		return nil, true
	}
	// A client Kivali registered goes with the tokens: it is deleted
	// with the service, and the next sign-in registers again.
	drop := []string{connect.SecretKeyAccessToken, connect.SecretKeyRefreshToken}
	if v.manifest.Kivali.Connection.Registered() {
		drop = connect.SecretKeys
	}
	kept := map[string]string{}
	for k, val := range data {
		if !slices.Contains(drop, k) {
			kept[k] = val
		}
	}
	if rec.SecretName != "" && len(kept) != len(data) {
		if err := s.Secrets.Put(r.Context(), rec.SecretName, kept); err != nil {
			log.Printf("integration %s: remove connection %s's tokens: %v", name, connID, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's tokens could not be removed, so nothing was changed", whoServer)
			return nil, false
		}
	}
	// The tokens are gone from the Secret: they are revoked whatever
	// happens to the record.
	revokes := signInRevocations(nil).add(name, v.manifest, rec, data)
	rec.SignIn = nil
	rec.VerifiedAt, rec.Verified, rec.VerifyMessage = nil, nil, ""
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: sign out connection %s: %v", name, connID, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection's tokens were removed, but its record could not be saved", whoServer)
		return revokes, false
	}
	s.auditAgentCall(name, start, agent.CEOSlug, "sign_out", true, "")
	// The running server starts again without the token.
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
	return revokes, true
}
