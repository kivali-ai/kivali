// Package web (integration_signin_registered.go) — a sign-in whose
// client core registers itself (kivali.connection.client registered).
//
// Before the browser sign-in's Go, when the connection holds no
// registered client that signs in at the redirect address the flow
// uses, core reads the service's metadata from the manifest's discover
// address (connect.Discover, held to the manifest's hosts) and
// registers the team as a client (connect.Register), with every
// redirect address registered so far and this one. The client, the
// endpoints found and the registration's management address and token
// are kept in the connection's Secret under connect.RegisteredSecretKeys,
// beside its fields and tokens, the connection made when there is none.
// The sign-in, refresh and revocation then go to the endpoints kept
// (signInAppOf). A client replaced by a new registration, and the
// client of a connection signed out, deleted or removed, is deleted
// with the service (RFC 7592) where it gave the means, best effort.

package web

import (
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
)

// signInApp is the OAuth app a connection signs in with and the
// endpoints it signs in at.
type signInApp struct {
	client connect.Client
	ep     connect.Endpoints
}

// signInAppOf is the app m's connection rec signs in with, its Secret
// holding data: for a registered client the one kept there with the
// endpoints discovery found, else the pasted or kept client
// (signInClient) and the manifest's endpoints.
func signInAppOf(m *integration.Manifest, rec store.IntegrationConnection, data map[string]string) signInApp {
	if m != nil && m.Kivali.Connection.Registered() {
		return registeredApp(data)
	}
	return signInApp{client: signInClient(rec, data), ep: signInEndpoints(m)}
}

// registeredApp is the registered client data keeps, and its endpoints;
// empty when it keeps none.
func registeredApp(data map[string]string) signInApp {
	return signInApp{
		client: connect.Client{ID: data[connect.SecretKeyClientID], Secret: data[connect.SecretKeyClientSecret], AuthMethod: data[connect.SecretKeyClientAuthMethod]},
		ep: connect.Endpoints{
			Authorization: data[connect.SecretKeyAuthorizationEndpoint], Token: data[connect.SecretKeyTokenEndpoint],
			Revocation: data[connect.SecretKeyRevocationEndpoint], Resource: data[connect.SecretKeyResource],
		},
	}
}

// registeredRedirects are the redirect addresses data's registered
// client was registered with.
func registeredRedirects(data map[string]string) []string {
	out := []string{}
	for _, u := range strings.Split(data[connect.SecretKeyRedirectURIs], "\n") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// registeredUsable reports whether data keeps a registered client a
// sign-in at redirect can use at now: one with its endpoints, registered
// with that address, whose secret has not expired.
func registeredUsable(data map[string]string, redirect string, now time.Time) bool {
	app := registeredApp(data)
	if app.client.ID == "" || app.ep.Authorization == "" || app.ep.Token == "" || !slices.Contains(registeredRedirects(data), redirect) {
		return false
	}
	if n, err := strconv.ParseInt(data[connect.SecretKeyClientSecretExpiresAt], 10, 64); err == nil && n > 0 && !now.Before(time.Unix(n, 0)) {
		return false
	}
	return true
}

// registeredValues is what the Secret keeps of reg, registered with the
// server md describes: every key of connect.RegisteredSecretKeys, empty
// for what the service gave not.
func registeredValues(md *connect.Metadata, reg *connect.Registration) map[string]string {
	expires := ""
	if !reg.SecretExpiresAt.IsZero() {
		expires = strconv.FormatInt(reg.SecretExpiresAt.Unix(), 10)
	}
	return map[string]string{
		connect.SecretKeyClientID:                reg.Client.ID,
		connect.SecretKeyClientSecret:            reg.Client.Secret,
		connect.SecretKeyClientSecretExpiresAt:   expires,
		connect.SecretKeyClientAuthMethod:        reg.Client.AuthMethod,
		connect.SecretKeyRedirectURIs:            strings.Join(reg.RedirectURIs, "\n"),
		connect.SecretKeyResource:                md.Resource,
		connect.SecretKeyAuthorizationEndpoint:   md.Authorization,
		connect.SecretKeyTokenEndpoint:           md.Token,
		connect.SecretKeyRevocationEndpoint:      md.Revocation,
		connect.SecretKeyRegistrationClientURI:   reg.ClientURI,
		connect.SecretKeyRegistrationAccessToken: reg.AccessToken,
	}
}

// registeredClientName is the name a registration gives the service to
// show the owner: "Kivali" and the team's name, or "Kivali" alone for a
// team with none.
func (s *Server) registeredClientName() string {
	br, err := s.Store.ReadBranding()
	if err != nil || strings.TrimSpace(br.CompanyName) == "" {
		return "Kivali"
	}
	return "Kivali " + strings.TrimSpace(br.CompanyName)
}

// errRegisteredNotKept: the service registered Kivali, but Kivali could
// not keep the client.
const errRegisteredNotKept = "the service registered Kivali, but Kivali could not keep the registration; start again"

// registeredSignInApp is the client name's browser sign-in at redirect
// uses for a manifest m whose client is registered: the one its
// connection keeps when that one is usable there (registeredUsable),
// else a new registration, found and made as the file's comment says,
// kept before it is used. A client the new one replaces is deleted
// with the service, best effort. The client secret and the registration
// token join run's scrubbed values.
func (s *Server) registeredSignInApp(ctx context.Context, run *setupRun, name, display string, m *integration.Manifest, redirect string, p builtinintegrations.Progress) (signInApp, error) {
	c := m.Kivali.Connection
	var old map[string]string
	if rec, err := s.Store.GetIntegrationConnection(name); err == nil {
		_, data, err := s.readSignIn(ctx, name, rec.ID)
		if err != nil {
			log.Printf("integration %s: read the connection's registered client: %v", name, err)
			return signInApp{}, errors.New("the connection could not be read; start the sign-in again")
		}
		old = data
	} else if !errors.Is(err, store.ErrNotFound) {
		log.Printf("integration %s: read the connection's registered client: %v", name, err)
		return signInApp{}, errors.New("the connection could not be read; start the sign-in again")
	}
	if registeredUsable(old, redirect, s.clk().Now()) {
		app := registeredApp(old)
		s.setupScrub(run, app.client.Secret, old[connect.SecretKeyRegistrationAccessToken])
		return app, nil
	}
	if p != nil {
		p.Stage("Registering Kivali with "+display, 0, 0)
	}
	engine := s.connectEngine()
	md, err := engine.Discover(ctx, c.Discover, c.Hosts)
	if err != nil {
		if ctx.Err() != nil {
			return signInApp{}, errors.New(errSetupStopped)
		}
		log.Printf("integration %s: discover the authorization server: %s", name, clipRunes(err.Error(), signInWordsRunes))
		return signInApp{}, errors.New("Kivali could not find where to sign in to " + display + ": " + err.Error())
	}
	redirects := registeredRedirects(old)
	if !slices.Contains(redirects, redirect) {
		redirects = append(redirects, redirect)
	}
	reg, err := engine.Register(ctx, md.Registration, connect.RegisterRequest{
		RedirectURIs: redirects, ClientName: s.registeredClientName(), Scope: strings.Join(c.Scopes, " "), AuthMethods: md.AuthMethods,
	}, append(slices.Clone(c.Hosts), integration.EndpointHost(c.Discover)))
	if err != nil {
		if ctx.Err() != nil {
			return signInApp{}, errors.New(errSetupStopped)
		}
		var refused *connect.RegistrationRefused
		if !errors.As(err, &refused) {
			log.Printf("integration %s: register with the service: %s", name, clipRunes(err.Error(), signInWordsRunes))
		}
		return signInApp{}, errors.New("Kivali could not register itself with " + display + ": " + err.Error())
	}
	values := registeredValues(md, reg)
	s.setupScrub(run, reg.Client.Secret, reg.AccessToken)
	if err := s.keepRegisteredClient(ctx, run, name, values); err != nil {
		// A client Kivali could not keep is of use to nobody.
		s.deregisterClient(ctx, name, "", values)
		return signInApp{}, err
	}
	if old[connect.SecretKeyClientID] != "" && old[connect.SecretKeyClientID] != reg.Client.ID {
		// The client replaced goes, and the tokens it still holds first.
		tok := connect.Tokens{Access: old[connect.SecretKeyAccessToken], Refresh: old[connect.SecretKeyRefreshToken]}
		s.revokeTokens(ctx, name, "", registeredApp(old), tok)
		s.deregisterClient(ctx, name, "", old)
	}
	return registeredApp(values), nil
}

// keepRegisteredClient writes values, a registered client
// (registeredValues), into name's connection's Secret in place of the
// one it keeps, beside its fields and tokens (dropped when they were
// issued to another client), under lockIntegrations,
// making the connection when there is none. The keys are no fields, so
// the record's secret keys are left as they are. A flow cancelled
// meanwhile keeps nothing.
func (s *Server) keepRegisteredClient(ctx context.Context, run *setupRun, name string, values map[string]string) error {
	defer s.lockIntegrations()()
	if run.ctx.Err() != nil {
		return errors.New(errSetupStopped)
	}
	rec, err := s.Store.GetIntegrationConnection(name)
	made := errors.Is(err, store.ErrNotFound)
	switch {
	case made:
		rec = store.IntegrationConnection{ID: s.newConnectionID(name), CreatedAt: s.clk().Now()}
	case err != nil:
		log.Printf("integration %s: read the connection to keep its registered client: %v", name, err)
		return errors.New(errRegisteredNotKept)
	}
	data := map[string]string{}
	if rec.SecretName != "" {
		got, err := s.Secrets.Get(ctx, rec.SecretName)
		switch {
		case err == nil:
			data = got
		case !errors.Is(err, secrets.ErrNotFound):
			log.Printf("integration %s: read the connection's Secret to keep its registered client: %v", name, err)
			return errors.New(errRegisteredNotKept)
		}
	}
	if data[connect.SecretKeyClientID] != values[connect.SecretKeyClientID] {
		// Tokens issued to the client this one replaces are revoked
		// with it (registeredSignInApp), not kept for the new one.
		delete(data, connect.SecretKeyAccessToken)
		delete(data, connect.SecretKeyRefreshToken)
	}
	for _, k := range connect.RegisteredSecretKeys {
		delete(data, k)
		if v := values[k]; v != "" {
			data[k] = v
		}
	}
	if rec.SecretName == "" {
		rec.SecretName = secrets.ConnectionSecretName(name, rec.ID)
	}
	if made && run.install {
		// An install's connection, marked unfinished until the flow is
		// done, so one a stopped Kivali left is discarded at its start.
		rec.SetupPending = true
	}
	if err := s.Secrets.Put(ctx, rec.SecretName, data); err != nil {
		log.Printf("integration %s: write the connection's registered client: %v", name, err)
		return errors.New(errRegisteredNotKept)
	}
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: write the connection for its registered client: %v", name, err)
		return errors.New(errRegisteredNotKept)
	}
	if made {
		// An install that does not finish forgets the connection it
		// made, and deletes the client with it.
		s.setupMade(run, rec.ID, false)
	}
	return nil
}

// deregisterClient deletes the registered client data keeps with the
// service (RFC 7592), best effort within revokeTimeout, when the
// service gave its management address and token; a failure is logged.
// id names the connection in the log.
func (s *Server) deregisterClient(ctx context.Context, name, id string, data map[string]string) {
	uri, token := data[connect.SecretKeyRegistrationClientURI], data[connect.SecretKeyRegistrationAccessToken]
	if uri == "" || token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	if err := s.connectEngine().Deregister(ctx, uri, token); err != nil {
		log.Printf("integration %s: delete connection %s's registered client: %v", name, id, err)
	}
}
