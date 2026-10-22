// Package web (integration_signin.go) — the tokens of a connection
// made by signing in (a manifest whose kivali.connection.method is
// device or browser), as core keeps and hands them out.
//
// The tokens live in the connection's Secret under
// connect.SecretKeyAccessToken and connect.SecretKeyRefreshToken; the
// record's SignIn says how and when the owner signed in, when the
// access token expires and, after a refresh the vendor refused for
// good, why the owner must sign in again. Every read of the
// connection's values for an integration's call (kivali/credential, a
// built-in's Connection.Fields) goes through signInAccessToken: a
// token within connect.RefreshMargin of its expiry is refreshed first,
// one refresh at a time per connection, and the new tokens are
// written to the Secret and the record under lockIntegrations. A
// refusal marks the record, which raises the connection's Needs you
// row (api_home.go); a vendor that does not answer leaves the tokens
// as they are.

package web

import (
	"context"
	"errors"
	"log"
	"time"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
)

var (
	// errSignInNeeded: a sign-in connection the owner has not signed
	// in with, whose Secret lost its tokens, or whose refresh the
	// vendor refused for good.
	errSignInNeeded = errors.New("the connection needs the owner to sign in again")
	// errVendorUnavailable: the access token has expired and the
	// vendor did not answer its refresh.
	errVendorUnavailable = errors.New("the vendor did not answer")
)

// connectHTTPTimeout bounds one request to a vendor's OAuth endpoint:
// one small POST. A refresh runs inside a pod's kivali/credential
// request, which the shim waits 10 seconds for, so it ends first.
const connectHTTPTimeout = 8 * time.Second

// revokeTimeout bounds the revocation a delete or a disconnect asks
// for, best effort, before it answers.
const revokeTimeout = 10 * time.Second

// signInEndpoints are m's OAuth endpoints.
func signInEndpoints(m *integration.Manifest) connect.Endpoints {
	c := m.Kivali.Connection
	return connect.Endpoints{Authorization: c.AuthorizationEndpoint, Token: c.TokenEndpoint, Device: c.DeviceEndpoint, Revocation: c.RevocationEndpoint}
}

// signInClient is the OAuth app a connection signs in with: the client
// id from its record, the client secret from its Secret's data.
func signInClient(rec store.IntegrationConnection, data map[string]string) connect.Client {
	return connect.Client{ID: rec.Fields[integration.FieldClientID], Secret: data[integration.FieldClientSecret]}
}

// signInKey is the Refresher's key of name's connection id.
func signInKey(name, id string) string { return name + "/" + id }

// signInAccessToken is the access token to hand out for name's
// sign-in connection rec, whose Secret holds data: the stored one
// while it is good for longer than connect.RefreshMargin (or does not
// expire), else a refreshed one. errSignInNeeded when the owner must
// sign in (again), including after a refresh the vendor refused for
// good; errVendorUnavailable when the token has expired and the vendor
// did not answer. A refresh that fails for a passing reason hands out
// the old token while it is still good.
func (s *Server) signInAccessToken(ctx context.Context, name string, m *integration.Manifest, rec store.IntegrationConnection, data map[string]string) (string, error) {
	token := data[connect.SecretKeyAccessToken]
	if token == "" || rec.NeedsSignIn(true) {
		return "", errSignInNeeded
	}
	now := s.clk().Now()
	exp := rec.SignIn.ExpiresAt
	if !connect.RefreshDue(now, exp) {
		return token, nil
	}
	tok, err := s.signInRefresher.Do(ctx, signInKey(name, rec.ID), func(ctx context.Context) (connect.Tokens, error) {
		return s.refreshSignIn(ctx, name, rec.ID, m)
	})
	var term *connect.Terminal
	switch {
	case err == nil:
		return tok.Access, nil
	case errors.Is(err, errSignInNeeded), errors.As(err, &term):
		return "", errSignInNeeded
	case ctx.Err() != nil:
		return "", ctx.Err()
	case s.clk().Now().Before(exp):
		// Still good now, after the refresh's wait on the vendor.
		return token, nil
	}
	return "", errVendorUnavailable
}

// refreshSignIn is one refresh of name's connection id, run by the
// Refresher. It reads the connection again first: a refresh that ended
// just before this one began has stored tokens that are not due, and
// sending the vendor the refresh token it replaced would be refused.
// New tokens go to the Secret and the record under lockIntegrations,
// read again there so a save of the connection in between keeps its
// fields. A refusal for good marks the record (markSignInFailed); every
// failure writes an audit row for the owner.
func (s *Server) refreshSignIn(ctx context.Context, name, id string, m *integration.Manifest) (connect.Tokens, error) {
	start := s.clk().Now()
	rec, data, err := s.readSignIn(ctx, name, id)
	if err != nil {
		return connect.Tokens{}, err
	}
	if rec.NeedsSignIn(data[connect.SecretKeyAccessToken] != "") {
		return connect.Tokens{}, errSignInNeeded
	}
	stored := connect.Tokens{Access: data[connect.SecretKeyAccessToken], Refresh: data[connect.SecretKeyRefreshToken], ExpiresAt: rec.SignIn.ExpiresAt, Scope: rec.SignIn.Scope}
	if !connect.RefreshDue(start, stored.ExpiresAt) {
		return stored, nil
	}
	if stored.Refresh == "" {
		const words = "the access token expired and the vendor gave no refresh token"
		if stored.ExpiresAt.After(start) {
			// Still good for a little while; nothing to refresh it with.
			return stored, nil
		}
		s.markSignInFailed(name, id, words)
		s.auditAgentCall(name, start, agent.CEOSlug, "refresh", false, words)
		return connect.Tokens{}, errSignInNeeded
	}
	client := signInClient(rec, data)
	tok, err := s.connectEngine().Refresh(ctx, signInEndpoints(m), client, stored.Refresh)
	if err != nil {
		words := signInFailureWords(err, stored, client)
		var term *connect.Terminal
		if errors.As(err, &term) {
			s.markSignInFailed(name, id, words)
		} else {
			log.Printf("integration %s: refresh connection %s: %s", name, id, words)
		}
		s.auditAgentCall(name, start, agent.CEOSlug, "refresh", false, words)
		return connect.Tokens{}, err
	}
	if tok.Scope == "" {
		tok.Scope = stored.Scope
	}
	// An answer with no expires_in says nothing of the new token's
	// life; one that expired before is taken to last as long again.
	now := s.clk().Now()
	life := rec.SignIn.Lifetime
	switch {
	case !tok.ExpiresAt.IsZero():
		life = tok.ExpiresAt.Sub(now)
	case !stored.ExpiresAt.IsZero():
		if life <= 0 {
			life = stored.ExpiresAt.Sub(rec.SignIn.SignedInAt)
		}
		if life > 0 {
			tok.ExpiresAt = now.Add(life)
		}
	}
	if err := s.storeSignInTokens(ctx, name, id, tok, life); err != nil {
		// The vendor may have rotated the refresh token, so the stored
		// one may be refused next time; the new access token works now.
		log.Printf("integration %s: keep connection %s's refreshed tokens: %v", name, id, err)
	}
	return tok, nil
}

// signInWordsRunes bounds the vendor's words a refresh's failure keeps:
// they are shown on the page, in Needs you and in the audit.
const signInWordsRunes = 200

// signInFailureWords is a sign-in's or a refresh's failure err in words to keep, log
// and audit. The vendor's words may echo what it was sent, so they are
// scrubbed of the stored tokens and the client secret first, whole,
// and only then cut to signInWordsRunes: cut first, a secret astride
// the cut would leave a piece no scrubber knows. more are other values
// the vendor was sent that are scrubbed too, such as a device code.
func signInFailureWords(err error, stored connect.Tokens, client connect.Client, more ...string) string {
	scrub := newScrubber(append([]string{stored.Access, stored.Refresh, client.Secret}, more...))
	var term *connect.Terminal
	if errors.As(err, &term) {
		clean := &connect.Terminal{Code: scrub.Replace(term.Code), Description: clipRunes(scrub.Replace(term.Description), signInWordsRunes)}
		return clean.Error()
	}
	return clipRunes(scrub.Replace(err.Error()), signInWordsRunes)
}

// clipRunes is s cut to at most n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// readSignIn reads name's connection id and its Secret's data.
func (s *Server) readSignIn(ctx context.Context, name, id string) (store.IntegrationConnection, map[string]string, error) {
	rec, err := s.Store.GetIntegrationConnectionByID(name, id)
	if err != nil {
		return rec, nil, err
	}
	data := map[string]string{}
	if rec.SecretName != "" {
		got, err := s.Secrets.Get(ctx, rec.SecretName)
		switch {
		case err == nil:
			data = got
		case !errors.Is(err, secrets.ErrNotFound):
			return rec, nil, err
		}
	}
	return rec, data, nil
}

// storeSignInTokens writes tok to name's connection id: the tokens to
// its Secret (a refresh token tok lacks keeps the stored one), the
// expiry, the scope and the token's lifetime (life, when known) to its
// record. A connection deleted since is left gone.
func (s *Server) storeSignInTokens(ctx context.Context, name, id string, tok connect.Tokens, life time.Duration) error {
	defer s.lockIntegrations()()
	rec, data, err := s.readSignIn(ctx, name, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case rec.SignIn == nil:
		// Signed out while the refresh ran.
		return nil
	}
	data[connect.SecretKeyAccessToken] = tok.Access
	if tok.Refresh != "" {
		data[connect.SecretKeyRefreshToken] = tok.Refresh
	}
	secretName := rec.SecretName
	if secretName == "" {
		secretName = secrets.ConnectionSecretName(name, rec.ID)
	}
	if err := s.Secrets.Put(ctx, secretName, data); err != nil {
		return err
	}
	rec.SecretName = secretName
	rec.SignIn.ExpiresAt, rec.SignIn.Scope = tok.ExpiresAt, tok.Scope
	if life > 0 {
		rec.SignIn.Lifetime = life
	}
	return s.Store.PutIntegrationConnection(name, rec)
}

// markSignInFailed records on name's connection id that the owner must
// sign in again, and why, which raises its Needs you row. A connection
// with no sign-in (signed out since) is left as it is.
func (s *Server) markSignInFailed(name, id, words string) {
	defer s.lockIntegrations()()
	rec, err := s.Store.GetIntegrationConnectionByID(name, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("integration %s: mark connection %s for a new sign-in: %v", name, id, err)
		}
		return
	}
	if rec.SignIn == nil {
		// Signed out while the refresh ran: nothing to sign in again
		// to, and no row to raise.
		return
	}
	rec.SignIn.Error, rec.SignIn.ErrorAt = words, s.clk().Now()
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: mark connection %s for a new sign-in: %v", name, id, err)
	}
}

// signInRevocation is one deleted connection's tokens to revoke with
// the vendor: collected while lockIntegrations is held, revoked once
// it is released, so a slow vendor stalls no other integration write.
type signInRevocation struct {
	name string
	m    *integration.Manifest
	rec  store.IntegrationConnection
	data map[string]string
}

type signInRevocations []signInRevocation

// add is rs with name's connection rec, whose Secret held data
// (revocableTokens; nil: nothing to revoke).
func (rs signInRevocations) add(name string, m *integration.Manifest, rec store.IntegrationConnection, data map[string]string) signInRevocations {
	if data == nil {
		return rs
	}
	return append(rs, signInRevocation{name: name, m: m, rec: rec, data: data})
}

// revokeSignIns revokes each of rs in turn. The caller holds no lock.
func (s *Server) revokeSignIns(ctx context.Context, rs signInRevocations) {
	for _, r := range rs {
		s.revokeSignIn(ctx, r.name, r.m, r.rec, r.data)
	}
}

// revokeSignIn asks the vendor to revoke rec's tokens, found in data,
// best effort: a manifest with no revocation endpoint, or a connection
// that holds no tokens, sends nothing, and a failure is logged. It
// runs once the connection is deleted or signed out, which it never
// holds up for longer than revokeTimeout.
func (s *Server) revokeSignIn(ctx context.Context, name string, m *integration.Manifest, rec store.IntegrationConnection, data map[string]string) {
	if m == nil || !m.Kivali.Connection.SignIn() || m.Kivali.Connection.RevocationEndpoint == "" {
		return
	}
	tok := connect.Tokens{Access: data[connect.SecretKeyAccessToken], Refresh: data[connect.SecretKeyRefreshToken]}
	s.revokeTokens(ctx, name, rec.ID, m, signInClient(rec, data), tok)
}

// revokeTokens asks the vendor to revoke tok, issued to client for
// name's connection id, best effort, as revokeSignIn does: nothing to
// send with no revocation endpoint or no token, a failure logged.
func (s *Server) revokeTokens(ctx context.Context, name, id string, m *integration.Manifest, client connect.Client, tok connect.Tokens) {
	if m.Kivali.Connection.RevocationEndpoint == "" || (tok.Access == "" && tok.Refresh == "") {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	if err := s.connectEngine().Revoke(ctx, signInEndpoints(m), client, tok); err != nil {
		log.Printf("integration %s: revoke connection %s's tokens: %v", name, id, err)
	}
}

// revocableTokens is the Secret's data of name's connection rec when
// its tokens are to be revoked as it is deleted: a sign-in connection
// whose manifest m has a revocation endpoint. nil otherwise, and when
// the Secret cannot be read (the delete goes on; nothing is revoked).
func (s *Server) revocableTokens(ctx context.Context, name string, m *integration.Manifest, rec store.IntegrationConnection) map[string]string {
	if m == nil || !m.Kivali.Connection.SignIn() || m.Kivali.Connection.RevocationEndpoint == "" || rec.SecretName == "" {
		return nil
	}
	data, err := s.Secrets.Get(ctx, rec.SecretName)
	if err != nil {
		if !errors.Is(err, secrets.ErrNotFound) {
			log.Printf("integration %s: read connection %s's tokens to revoke them: %v", name, rec.ID, err)
		}
		return nil
	}
	return data
}

// connectEngine is the engine sign-ins and refreshes go through.
func (s *Server) connectEngine() *connect.Engine {
	if s.Connect == nil {
		return &connect.Engine{Clock: s.clk()}
	}
	return s.Connect
}
