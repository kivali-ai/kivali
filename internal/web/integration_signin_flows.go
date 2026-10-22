// Package web (integration_signin_flows.go) — signing a connection in
// and out: the sign-ins under way, the device flow's routes, and Sign
// out.
//
// A sign-in under way is a signInFlow, kept in memory on the Server
// (signInFlows) under a random id, at most one per connection: a new
// start cancels the one before. A device sign-in shows the owner a
// code and the service's page to enter it on, then polls the vendor's
// token endpoint in a goroutine on the server's clock until the owner
// approves, the vendor refuses or the code expires. The tokens go to
// the connection's Secret and the sign-in to its record, the bundle's
// server starts again with the new token, and the connection test
// runs. The page asks GET .../sign-in how the sign-in stands. A flow
// that ended is kept signInFlowTTL for the page to read; a restart
// loses every flow, and GET .../sign-in then answers expired, in words
// that say so. Deleting the connection, disconnecting, removing the
// integration, switching it off and signing out cancel its flows.

package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// signInFlowTTL is how long a sign-in may take (a device code the
// vendor gives longer, or no expiry, is used for this long), and how
// long a flow is kept for the page to read once it ended.
const signInFlowTTL = 10 * time.Minute

// signInWaysOffered are the ways this server signs a connection in by,
// of those a manifest may offer (integration.Connection.Ways): the
// page is offered only these. Browser sign-in joins once its routes
// are in place.
var signInWaysOffered = []string{integration.MethodDevice}

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

// Refusals of the sign-in routes.
const (
	errSignInClientFirst = "save the client id and secret first"
	errSignInSignedIn    = "the connection is signed in; sign out first"
	// errSignInNoFlow answers GET .../sign-in for a connection with no
	// sign-in Kivali knows of: it ended long ago, or Kivali restarted.
	errSignInNoFlow = "Kivali has no sign-in under way for this connection, as after a restart; start it again"
)

// signInFlow is one sign-in under way.
type signInFlow struct {
	id, name, connID string
	ctx              context.Context
	cancel           context.CancelFunc
	// done is closed once the flow's goroutine has ended.
	done chan struct{}

	// Guarded by signInFlows.mu.
	state apitypes.SignInState
	err   string
	// until is when the flow is forgotten.
	until time.Time
}

// signInFlows are the sign-ins under way, by id. The zero value is
// ready to use.
type signInFlows struct {
	mu    sync.Mutex
	flows map[string]*signInFlow
}

// newSignInFlowID is a fresh flow id: 16 random bytes in hex.
func newSignInFlowID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// start begins a flow for name's connection connID that is forgotten
// at until, cancelling and forgetting any other of that connection.
func (t *signInFlows) start(now time.Time, name, connID string, until time.Time) *signInFlow {
	ctx, cancel := context.WithCancel(context.Background())
	f := &signInFlow{
		id: newSignInFlowID(), name: name, connID: connID, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), state: apitypes.SignInStatePending, until: until,
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	t.cancelLocked(name, connID)
	if t.flows == nil {
		t.flows = map[string]*signInFlow{}
	}
	t.flows[f.id] = f
	return f
}

// progress is how name's connection connID's flow stands; ok is false
// when it has none.
func (t *signInFlows) progress(now time.Time, name, connID string) (apitypes.SignInProgress, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	for _, f := range t.flows {
		if f.name == name && f.connID == connID {
			return apitypes.SignInProgress{State: f.state, Error: optString(f.err)}, true
		}
	}
	return apitypes.SignInProgress{}, false
}

// end records how f ended, unless it was cancelled and forgotten.
func (t *signInFlows) end(f *signInFlow, now time.Time, state apitypes.SignInState, words string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.flows[f.id] != f {
		return
	}
	f.state, f.err, f.until = state, words, now.Add(signInFlowTTL)
}

// cancel cancels and forgets name's flows: those of its connection
// connID, or every one with connID "".
func (t *signInFlows) cancel(name, connID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancelLocked(name, connID)
}

func (t *signInFlows) cancelLocked(name, connID string) {
	for id, f := range t.flows {
		if f.name == name && (connID == "" || f.connID == connID) {
			f.cancel()
			delete(t.flows, id)
		}
	}
}

// pruneLocked forgets the flows whose time is up, cancelling any still
// polling.
func (t *signInFlows) pruneLocked(now time.Time) {
	for id, f := range t.flows {
		if !now.Before(f.until) {
			f.cancel()
			delete(t.flows, id)
		}
	}
}

// signInTarget is the sign-in connection a sign-in route acts on: the
// one {id} names (byID), or the integration's one, the oldest. It
// answers 404 for an id that names none, and 409 for an integration
// whose connection is not made by signing in, one this version does
// not carry, and one with no approved version or no connection yet.
func (s *Server) signInTarget(w http.ResponseWriter, r *http.Request, v *integrationView, byID bool) (*connView, bool) {
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
	if byID {
		return connectionOr404(w, r, v)
	}
	c := v.first()
	if c == nil {
		writeAPIError(w, http.StatusConflict, errSignInClientFirst, whoYou)
		return nil, false
	}
	return c, true
}

// handleAPISignInDevice starts a device sign-in of the connection
// {id} names (byID) or of the integration's one, and answers with the
// code and the service's page. The connection's client id and secret
// must be stored, and a connection signed in is signed out first. The
// vendor is asked outside lockIntegrations; the flow is registered
// under it, once the connection is found still there.
func (s *Server) handleAPISignInDevice(byID bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		unlock := s.lockIntegrations()
		v, ok := s.integrationOr404(w, r)
		if !ok {
			unlock()
			return
		}
		c, ok := s.signInTarget(w, r, v, byID)
		if !ok {
			unlock()
			return
		}
		name, connID, m := v.it.Name, c.rec.ID, v.manifest
		if !slices.Contains(offeredSignInWays(m.Kivali.Connection), integration.MethodDevice) {
			unlock()
			writeAPIError(w, http.StatusConflict, v.displayName()+" does not sign in with a code", v.whoBuilt())
			return
		}
		if !v.it.Enabled {
			unlock()
			writeAPIError(w, http.StatusConflict, "switch "+v.displayName()+" on first", whoYou)
			return
		}
		if !c.rec.NeedsSignIn(c.hasToken) {
			unlock()
			writeAPIError(w, http.StatusConflict, errSignInSignedIn, whoYou)
			return
		}
		rec, data, err := s.readSignIn(r.Context(), name, connID)
		unlock()
		if err != nil {
			log.Printf("integration %s: read connection %s: %v", name, connID, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection could not be read", whoServer)
			return
		}
		client := signInClient(rec, data)
		if client.ID == "" || client.Secret == "" {
			writeAPIError(w, http.StatusConflict, errSignInClientFirst, whoYou)
			return
		}
		ds, err := s.connectEngine().StartDevice(r.Context(), signInEndpoints(m), client, m.Kivali.Connection.Scopes)
		var term *connect.Terminal
		switch {
		case errors.As(err, &term):
			writeAPIError(w, http.StatusConflict, v.displayName()+" refused to start the sign-in: "+term.Error(), whoYou)
			return
		case err != nil:
			log.Printf("integration %s: start connection %s's sign-in: %v", name, connID, err)
			writeAPIError(w, http.StatusBadGateway, v.displayName()+" did not start the sign-in: "+err.Error(), whoServer)
			return
		}
		now := s.clk().Now()
		// The flow lasts signInFlowTTL, or less when the code expires
		// sooner; PollDevice stops at ExpiresAt on the clock, so a vendor
		// that never answers is not polled past it.
		if limit := now.Add(signInFlowTTL); ds.ExpiresAt.IsZero() || ds.ExpiresAt.After(limit) {
			ds.ExpiresAt = limit
		}
		unlock = s.lockIntegrations()
		if _, err := s.Store.GetIntegrationConnectionByID(name, connID); err != nil {
			unlock()
			writeAPIError(w, http.StatusNotFound, errConnectionGone, whoLink)
			return
		}
		f := s.signInFlows.start(now, name, connID, ds.ExpiresAt.Add(signInFlowTTL))
		unlock()
		go s.runDeviceSignIn(f, m, client, ds)
		out := apitypes.DeviceSignIn{
			UserCode: ds.UserCode, VerificationURL: ds.VerificationURI,
			VerificationURLComplete: optString(ds.VerificationURIComplete), ExpiresAt: ds.ExpiresAt,
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// runDeviceSignIn is f's goroutine: it polls the vendor until the
// owner approves, then keeps the sign-in (finishSignIn) and runs the
// connection test, and records how the flow ended. A cancelled flow
// ends with nothing recorded.
func (s *Server) runDeviceSignIn(f *signInFlow, m *integration.Manifest, client connect.Client, ds *connect.DeviceStart) {
	defer close(f.done)
	start := s.clk().Now()
	tok, err := s.connectEngine().PollDevice(f.ctx, signInEndpoints(m), client, ds)
	if f.ctx.Err() != nil {
		return
	}
	if err != nil {
		state := apitypes.SignInStateFailed
		if errors.Is(err, connect.ErrDeviceExpired) {
			state = apitypes.SignInStateExpired
		}
		s.auditAgentCall(f.name, start, agent.CEOSlug, "sign_in", false, err.Error())
		s.signInFlows.end(f, s.clk().Now(), state, err.Error())
		return
	}
	if err := s.finishSignIn(f, m, client, integration.MethodDevice, tok, start); err != nil {
		if f.ctx.Err() == nil {
			s.signInFlows.end(f, s.clk().Now(), apitypes.SignInStateFailed, err.Error())
		}
		return
	}
	s.testAfterSignIn(f.ctx, f.name, f.connID)
	s.signInFlows.end(f, s.clk().Now(), apitypes.SignInStateDone, "")
}

// errSignInNotKept: the vendor gave the tokens, but Kivali could not
// keep them.
var errSignInNotKept = errors.New("the sign-in worked, but Kivali could not keep it; start it again")

// finishSignIn keeps the tokens of f's sign-in by method, under
// lockIntegrations: the tokens to the connection's Secret, which keeps
// its other values (a refresh token tok lacks drops the stored one),
// and the sign-in to its record, which clears a refused refresh's
// error (and so the Needs you row) and the last connection test. It
// appends the owner's audit row and starts the bundle's server again,
// which then asks for the new token. A flow cancelled meanwhile, or a
// connection gone, keeps nothing, and the tokens are revoked with the
// vendor, best effort.
func (s *Server) finishSignIn(f *signInFlow, m *integration.Manifest, client connect.Client, method string, tok connect.Tokens, start time.Time) error {
	ctx := f.ctx
	unlock := s.lockIntegrations()
	rec, data, err := s.readSignIn(ctx, f.name, f.connID)
	if f.ctx.Err() != nil || errors.Is(err, store.ErrNotFound) {
		unlock()
		s.revokeTokens(ctx, f.name, f.connID, m, client, tok)
		return errors.New(errConnectionGone)
	}
	if err == nil {
		data[connect.SecretKeyAccessToken] = tok.Access
		delete(data, connect.SecretKeyRefreshToken)
		if tok.Refresh != "" {
			data[connect.SecretKeyRefreshToken] = tok.Refresh
		}
		if rec.SecretName == "" {
			rec.SecretName = secrets.ConnectionSecretName(f.name, rec.ID)
		}
		err = s.Secrets.Put(ctx, rec.SecretName, data)
	}
	if err == nil {
		rec.SignIn = &store.ConnectionSignIn{Method: method, SignedInAt: s.clk().Now(), ExpiresAt: tok.ExpiresAt, Scope: tok.Scope}
		rec.VerifiedAt, rec.Verified, rec.VerifyMessage = nil, nil, ""
		err = s.Store.PutIntegrationConnection(f.name, rec)
	}
	unlock()
	if err != nil {
		log.Printf("integration %s: keep connection %s's sign-in: %v", f.name, f.connID, err)
		s.auditAgentCall(f.name, start, agent.CEOSlug, "sign_in", false, errSignInNotKept.Error())
		return errSignInNotKept
	}
	s.auditAgentCall(f.name, start, agent.CEOSlug, "sign_in", true, "")
	// The running server starts again with the new token.
	s.Integrations.ConnectionChanged(f.name)
	s.Integrations.Changed()
	return nil
}

// testAfterSignIn runs the connection test of name's connection id
// just signed in, when the integration has one and is running, so the
// page shows who the owner signed in as. Its result is kept as Test
// connection's is; a test that cannot run is skipped.
func (s *Server) testAfterSignIn(ctx context.Context, name, id string) {
	v, err := s.loadIntegration(ctx, name)
	if err != nil || !v.connectionTest() || !v.running || !v.it.Enabled {
		return
	}
	if _, keepErr := s.testConnection(ctx, v, id); keepErr != nil {
		log.Printf("integration %s: keep the connection test after a sign-in: %v", name, keepErr)
	}
}

// handleAPISignInProgress answers how the latest sign-in of the
// connection {id} names (byID), or of the integration's one, stands.
// With none (it ended over signInFlowTTL ago, or Kivali restarted
// since it began) it is expired, in words that say so.
func (s *Server) handleAPISignInProgress(byID bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.integrationOr404(w, r)
		if !ok {
			return
		}
		c, ok := s.signInTarget(w, r, v, byID)
		if !ok {
			return
		}
		p, ok := s.signInFlows.progress(s.clk().Now(), v.it.Name, c.rec.ID)
		if !ok {
			p = apitypes.SignInProgress{State: apitypes.SignInStateExpired, Error: optString(errSignInNoFlow)}
		}
		writeJSON(w, http.StatusOK, p)
	}
}

// handleAPISignOut signs out the connection {id} names (byID), or the
// integration's one: it cancels a sign-in under way, removes the
// tokens from the connection's Secret (its fields stay, the client id
// and secret among them), clears the sign-in and the last connection
// test from its record, asks the vendor to revoke the tokens (best
// effort), appends the owner's audit row and starts the bundle's
// server again without the token. It answers with the page. The vendor
// is asked to revoke with lockIntegrations released, so a slow vendor
// holds up no other integration write.
func (s *Server) handleAPISignOut(byID bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		unlock := s.lockIntegrations()
		revokes, ok := s.signOutLocked(w, r, byID)
		unlock()
		s.revokeSignIns(r.Context(), revokes)
		if ok {
			s.writeIntegrationPage(w, r)
		}
	}
}

// signOutLocked is Sign out under lockIntegrations: true once done,
// false with an error answered. It returns the tokens to revoke.
func (s *Server) signOutLocked(w http.ResponseWriter, r *http.Request, byID bool) (signInRevocations, bool) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return nil, false
	}
	c, ok := s.signInTarget(w, r, v, byID)
	if !ok {
		return nil, false
	}
	name, connID := v.it.Name, c.rec.ID
	start := s.clk().Now()
	s.signInFlows.cancel(name, connID)
	rec, data, err := s.readSignIn(r.Context(), name, connID)
	if err != nil {
		log.Printf("integration %s: read connection %s: %v", name, connID, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be read, so nothing was changed", whoServer)
		return nil, false
	}
	kept := map[string]string{}
	for k, val := range data {
		if k != connect.SecretKeyAccessToken && k != connect.SecretKeyRefreshToken {
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
