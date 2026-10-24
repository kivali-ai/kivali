// Package web (integration_setup_standard.go) — core's own flows for
// the standard methods, run by the setup runner (integration_setup.go)
// as a setup integration's are:
//
//   - token: one Form of the connection's fields, then Keep (saved as
//     PUT .../connection saves them) and Done.
//   - device: a Form of the connection's fields when the app's client
//     id and secret are not saved, then a Wait that shows the vendor's
//     code and page while core polls the token endpoint in the
//     background, then Done once the tokens are kept.
//   - browser: the same Form when needed, then a Go to the vendor's
//     authorization page (with PKCE), which sends the browser back with
//     a code; core exchanges it, keeps the tokens, and is Done. For a
//     client Kivali registers itself, the Go is preceded by discovery
//     and registration when the connection keeps no client for the
//     address it signs in at (integration_signin_registered.go).
//
// Each holds what it needs between calls (the client, the device code,
// the OAuth state and verifier) itself, never in the flow's State, and
// adds every secret of it to what the flow's words are scrubbed of.

package web

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Words of the standard flows' steps.
const (
	tokenFormWords  = "Fill in the connection."
	signInFormWords = "Fill in the connection. The client id and secret are those of the app you registered with the service."
	deviceWaitWords = "Enter this code on the service’s page, then approve the sign-in there. Kivali finishes it here once you do."
	browserGoWords  = "Sign in on the service’s page and approve the sign-in. The service sends you back here."
)

// errSignInNoState: a callback of a browser sign-in that carried no
// OAuth state, matched by the flow's cookie alone.
const errSignInNoState = "the service sent the sign-in back without its state; start it again"

// connectionSetupFields is m's connection form as a Form step's fields:
// a sensitive field is secret.
func connectionSetupFields(m *integration.Manifest) integration.SetupFields {
	out := integration.SetupFields{}
	for _, f := range integration.ConnectionFields(m) {
		out = append(out, integration.SetupField{Key: f.Key, Title: f.Title, Words: f.Description, Secret: f.Sensitive})
	}
	return out
}

// shownKeys are the keys of m's connection whose values a flow's words
// are not scrubbed of (integration.ConnectionKeyScrubbed is false): a
// form field that is not sensitive, a keep declared not secret.
func shownKeys(m *integration.Manifest) []string {
	out := []string{}
	for _, k := range integration.ConnectionKeys(m) {
		if !integration.ConnectionKeyScrubbed(m, k) {
			out = append(out, k)
		}
	}
	return out
}

// setupKeys is fields' keys, in order.
func setupKeys(fields integration.SetupFields) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Key)
	}
	return out
}

// setupSpecFor is the flow POST .../setup runs for v, from where r
// reached Kivali, or why none can start: a refusal with its status and
// words.
func (s *Server) setupSpecFor(v *integrationView, r *http.Request) (setupSpec, *connWriteError) {
	m := v.manifest
	c := m.Kivali.Connection
	switch c.Method {
	case integration.MethodToken:
		fields := connectionSetupFields(m)
		return setupSpec{
			driver: &tokenSetup{s: s, name: v.it.Name, keys: setupKeys(fields)},
			fields: fields, keeps: setupKeys(fields), shown: shownKeys(m), keep: s.keepConnectionForm,
		}, nil
	case integration.MethodDevice, integration.MethodBrowser:
		ways := s.signInWaysHere(m, r)
		switch {
		case len(ways) == 0 && offersBrowser(m):
			return setupSpec{}, &connWriteError{http.StatusConflict, errSignInNotHere, whoYou}
		case len(ways) == 0:
			return setupSpec{}, &connWriteError{http.StatusConflict, "this version of Kivali has no way to sign in to " + v.displayName(), v.whoBuilt()}
		case ways[0] == integration.MethodBrowser && signInRedirectURL(r) == "":
			return setupSpec{}, &connWriteError{http.StatusConflict, errSignInNoAddress, whoServer}
		}
		if cur := v.conn; cur != nil && !cur.rec.NeedsSignIn(cur.hasToken) {
			return setupSpec{}, &connWriteError{http.StatusConflict, errSignInSignedIn, whoYou}
		}
		fields := connectionSetupFields(m)
		return setupSpec{
			driver: &signInSetup{s: s, name: v.it.Name, display: v.displayName(), m: m, way: ways[0], polled: make(chan struct{}), keys: setupKeys(fields)},
			fields: fields, hosts: c.SignInHosts(), keeps: setupKeys(fields), shown: shownKeys(m), keep: s.keepConnectionForm,
			vendorWaitPages: true,
		}, nil
	case integration.MethodSetup:
		si, ok := s.setupDriverOf(v)
		if !ok || c.Setup == nil {
			return setupSpec{}, &connWriteError{http.StatusConflict, v.displayName() + "'s setup is not in this version of Kivali", v.whoBuilt()}
		}
		spec := setupSpec{driver: si, fields: c.Setup.Fields, hosts: c.Setup.Hosts, keeps: c.Setup.Keeps.Keys(), shown: shownKeys(m), setup: c.Setup, keep: s.keepSetupValues}
		if then := c.Setup.Then; then != "" {
			// The sign-in setup hands over to must be able to finish from
			// here, or the owner would make the app for nothing.
			thenSpec, werr := s.thenSpecFor(v, r, then)
			if werr != nil {
				return setupSpec{}, werr
			}
			spec.then = &thenSpec
		}
		return spec, nil
	}
	return setupSpec{}, &connWriteError{http.StatusConflict, v.displayName() + "'s connection is not one Kivali makes", v.whoBuilt()}
}

// thenSpecFor is the sign-in a setup connection's flow hands over to
// (setup.then), with the app setup kept, or why it cannot finish from
// where r reached Kivali.
func (s *Server) thenSpecFor(v *integrationView, r *http.Request, then string) (setupSpec, *connWriteError) {
	switch {
	case then == integration.MethodBrowser && signInRedirectURL(r) == "":
		return setupSpec{}, &connWriteError{http.StatusConflict, errSignInNoAddress, whoServer}
	case then == integration.MethodBrowser && !s.browserSignInHere(r):
		return setupSpec{}, &connWriteError{http.StatusConflict, errSignInNotHere, whoYou}
	}
	return setupSpec{
		driver: &signInSetup{s: s, name: v.it.Name, display: v.displayName(), m: v.manifest, way: then, polled: make(chan struct{})},
		hosts:  v.manifest.Kivali.Connection.SignInHosts(), keep: s.keepSetupValues, vendorWaitPages: true,
	}, nil
}

// signInSpecFor is the flow the sign_in row's Fix runs: a sign-in
// connection's standard sign-in, in the first way that works from
// here, asking for the connection form only when the app's client id
// or secret is not stored; or, for a setup connection that hands over
// to a sign-in, that sign-in with the app setup kept. A connection
// signed in is a 409: sign out first.
func (s *Server) signInSpecFor(v *integrationView, r *http.Request) (setupSpec, *connWriteError) {
	c := v.manifest.Kivali.Connection
	switch {
	case c.SignIn():
		return s.setupSpecFor(v, r)
	case c.ThenMethod() == "":
		return setupSpec{}, &connWriteError{http.StatusConflict, v.displayName() + " is not connected by signing in", whoYou}
	}
	if cur := v.conn; cur != nil && !cur.rec.NeedsSignIn(cur.hasToken) {
		return setupSpec{}, &connWriteError{http.StatusConflict, errSignInSignedIn, whoYou}
	}
	return s.thenSpecFor(v, r, c.ThenMethod())
}

// keepConnectionForm is a standard flow's Keep: the connection form's
// values, saved under lockIntegrations as PUT .../connection saves
// them, into the integration's one connection, made when there is
// none. A flow cancelled meanwhile keeps nothing.
func (s *Server) keepConnectionForm(ctx context.Context, run *setupRun, values map[string]string, final bool) error {
	defer s.lockIntegrations()()
	if run.ctx.Err() != nil {
		return errors.New(errSetupStopped)
	}
	v, err := s.loadIntegration(ctx, run.name)
	if err != nil {
		log.Printf("integration %s: read it to keep the connection: %v", run.name, err)
		return errors.New(errSetupNotKept)
	}
	cur := v.conn
	if werr := s.writeConnection(ctx, v, cur, formWrite(v.manifest, values), false); werr != nil {
		return werr
	}
	// The connection an install makes, or one a stopped Kivali's install
	// left (still marked unfinished), is the install's: marked
	// unfinished until the flow is done, and discarded should it end
	// without being done.
	// The flow's last step (final) leaves it finished.
	if cur == nil || (run.install && cur.rec.SetupPending) {
		if rec, err := s.Store.GetIntegrationConnection(run.name); err == nil {
			takenOver := rec.SetupTakenOver
			if pending := run.install && !final; rec.SetupPending != pending {
				rec.SetupPending = pending
				rec.SetupTakenOver = pending && takenOver
				if err := s.Store.PutIntegrationConnection(run.name, rec); err != nil {
					log.Printf("integration %s: mark the connection an install made: %v", run.name, err)
					if final {
						return errors.New(errSetupNotKept)
					}
				}
			}
			s.setupMade(run, rec.ID, takenOver)
		}
	}
	return nil
}

// formWrite is a Form's answer as a save of m's connection form: the
// Form sends every field it asks for, so a secret field left blank is
// left out, which keeps the value stored, as the page's form does.
func formWrite(m *integration.Manifest, values map[string]string) apitypes.ConnectionWrite {
	fields := integration.ConnectionFields(m)
	out := map[string]string{}
	for k, v := range values {
		if f, ok := fields.Field(k); ok && f.Sensitive && v == "" {
			continue
		}
		out[k] = v
	}
	return apitypes.ConnectionWrite{Fields: out}
}

// checkConnectionForm is why values cannot be the integration's one
// connection's form, as a save of it would refuse them, or "".
func (s *Server) checkConnectionForm(ctx context.Context, name string, values map[string]string) string {
	defer s.lockIntegrations()()
	v, err := s.loadIntegration(ctx, name)
	if err != nil {
		return "the integration could not be read; start again"
	}
	if _, werr := s.planConnectionWrite(ctx, v, v.conn, formWrite(v.manifest, values)); werr != nil {
		return werr.words
	}
	return ""
}

// tokenSetup is the token flow: the connection form, then Done.
type tokenSetup struct {
	s    *Server
	name string
	keys []string
}

func (d *tokenSetup) Setup(_ context.Context, _ builtinintegrations.Connection, _ string, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
	if last.Form == nil {
		return builtinintegrations.Step{Form: &builtinintegrations.Form{Words: tokenFormWords, Fields: d.keys}}, nil
	}
	return builtinintegrations.Step{Keep: last.Form, Done: true}, nil
}

func (d *tokenSetup) checkForm(ctx context.Context, values map[string]string) string {
	return d.s.checkConnectionForm(ctx, d.name, values)
}

// signInSetup is the device or browser flow, by way.
type signInSetup struct {
	s             *Server
	name, display string
	m             *integration.Manifest
	way           string
	keys          []string
	run           *setupRun

	// Set by a call, read by the next call or the poller, which the
	// runner starts only once that call's step is shown.
	app                       signInApp
	device                    *connect.DeviceStart
	redirect, state, verifier string
	// polled is closed once the device flow's poller has ended.
	polled chan struct{}
}

func (d *signInSetup) bind(run *setupRun) { d.run = run }

func (d *signInSetup) checkForm(ctx context.Context, values map[string]string) string {
	return d.s.checkConnectionForm(ctx, d.name, values)
}

// signInClientFor is the app a sign-in of name's connection uses, from
// what the owner just entered (form, nil for none) or else as stored,
// and whether the owner must first enter the connection's form. For a
// client Kivali registers itself (integration.ClientRegistered) it is
// the one the connection keeps, empty before one is registered
// (registeredSignInApp registers it), and the form is needed only to
// make a connection whose form has fields.
func (s *Server) signInClientFor(ctx context.Context, name string, m *integration.Manifest, form map[string]string) (signInApp, bool, error) {
	registered := m.Kivali.Connection.Registered()
	entered := connect.Client{ID: form[integration.FieldClientID], Secret: form[integration.FieldClientSecret]}
	if form != nil && !registered && entered.ID != "" && entered.Secret != "" {
		return signInApp{client: entered, ep: signInEndpoints(m)}, false, nil
	}
	app := signInApp{ep: signInEndpoints(m)}
	rec, err := s.Store.GetIntegrationConnection(name)
	switch {
	case errors.Is(err, store.ErrNotFound) && registered:
		return signInApp{}, form == nil && len(integration.ConnectionFields(m)) > 0, nil
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return signInApp{}, false, err
	default:
		rec, data, err := s.readSignIn(ctx, name, rec.ID)
		if err != nil {
			return signInApp{}, false, err
		}
		app = signInAppOf(m, rec, data)
		if registered {
			return app, false, nil
		}
	}
	// A field the owner left blank keeps what is stored.
	app.client = connect.Client{ID: cmp.Or(entered.ID, app.client.ID), Secret: cmp.Or(entered.Secret, app.client.Secret)}
	return app, form == nil && (app.client.ID == "" || app.client.Secret == ""), nil
}

func (d *signInSetup) Setup(ctx context.Context, _ builtinintegrations.Connection, _ string, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	if last.Return != nil {
		return d.back(ctx, last.Return)
	}
	app, needForm, err := d.s.signInClientFor(ctx, d.name, d.m, last.Form)
	if err != nil {
		log.Printf("integration %s: read the connection to sign in: %v", d.name, err)
		return builtinintegrations.Step{}, errors.New("the connection could not be read; start the sign-in again")
	}
	if needForm && len(d.keys) == 0 {
		// A sign-in setup hands over to, with no form of its own.
		return builtinintegrations.Step{}, errors.New(d.display + "'s setup did not keep the app's client id and secret, so Kivali cannot sign in; start it again")
	}
	if needForm {
		return builtinintegrations.Step{Form: &builtinintegrations.Form{Words: signInFormWords, Fields: d.keys}}, nil
	}
	d.app = app
	d.s.setupScrub(d.run, app.client.Secret)
	step := builtinintegrations.Step{Keep: last.Form}
	if d.way == integration.MethodDevice {
		ds, err := d.s.connectEngine().StartDevice(ctx, app.ep, app.client, d.m.Kivali.Connection.Scopes)
		if err != nil {
			var term *connect.Terminal
			words := signInFailureWords(err, connect.Tokens{}, app.client)
			if !errors.As(err, &term) {
				log.Printf("integration %s: start the sign-in: %s", d.name, words)
				return step, errors.New(d.display + " did not start the sign-in: " + words)
			}
			return step, errors.New(d.display + " refused to start the sign-in: " + words)
		}
		// The code lasts setupFlowTTL at most; PollDevice stops at its
		// expiry on the clock, so a vendor that never answers is not
		// polled past it.
		if limit := d.s.clk().Now().Add(setupFlowTTL); ds.ExpiresAt.IsZero() || ds.ExpiresAt.After(limit) {
			ds.ExpiresAt = limit
		}
		d.device = ds
		d.s.setupScrub(d.run, ds.DeviceCode())
		page := ds.VerificationURIComplete
		if page == "" {
			page = ds.VerificationURI
		}
		step.Wait = &builtinintegrations.Wait{Words: deviceWaitWords, URL: page, Code: ds.UserCode}
		return step, nil
	}
	cb, err := url.Parse(last.Callback)
	if last.Callback == "" || err != nil {
		return step, errors.New("the flow has no callback address; start it again from the team's own address")
	}
	d.state = cb.Query().Get("state")
	cb.RawQuery = ""
	d.redirect = cb.String()
	c := d.m.Kivali.Connection
	if c.Registered() {
		// The client registered with the address this sign-in comes back to.
		if d.app, err = d.s.registeredSignInApp(ctx, d.run, d.name, d.display, d.m, d.redirect, p); err != nil {
			return step, err
		}
	}
	d.verifier = ""
	if c.UsesPKCE() {
		d.verifier = connect.NewPKCEVerifier()
	}
	d.s.setupScrub(d.run, d.state, d.verifier)
	target := d.s.connectEngine().AuthorizeURL(d.app.ep, d.app.client, d.redirect, d.state, c.Scopes, d.verifier, c.ExtraAuthorizationParams)
	if target == "" {
		return step, errors.New(d.display + "'s sign-in address is not one Kivali can use")
	}
	step.Go = &builtinintegrations.Go{Words: browserGoWords, URL: target}
	return step, nil
}

// waiting is the device flow's Wait: core polls the vendor at its pace
// until the owner approves, the vendor refuses or the code expires,
// then keeps the tokens and runs the connection test.
func (d *signInSetup) waiting(run *setupRun) {
	d.s.setupRuns.calling.Add(1)
	go func() {
		defer d.s.setupRuns.calling.Done()
		defer close(d.polled)
		ds := d.device
		tok, err := d.s.connectEngine().PollDevice(run.ctx, d.app.ep, d.app.client, ds)
		if run.ctx.Err() != nil {
			return
		}
		if err != nil {
			// The vendor's words may echo the device code it was polled with.
			d.s.setupEnd(run, errors.New(signInFailureWords(err, connect.Tokens{}, d.app.client, ds.DeviceCode())))
			return
		}
		d.s.setupEnd(run, d.keepSignIn(run.ctx, integration.MethodDevice, tok))
	}()
}

// back is the browser flow's return: the vendor's error, or a code to
// exchange for the tokens with the verifier and the redirect address
// it was given.
func (d *signInSetup) back(ctx context.Context, q url.Values) (builtinintegrations.Step, error) {
	if d.way != integration.MethodBrowser || d.redirect == "" {
		return builtinintegrations.Step{}, errors.New(errSignInStartAgain)
	}
	code := q.Get("code")
	d.s.setupScrub(d.run, code)
	// The vendor's words may echo what it was sent or sends back.
	echoed := []string{d.state, code, d.verifier}
	if vendorCode := q.Get("error"); vendorCode != "" {
		return builtinintegrations.Step{}, errors.New(vendorRefusalWords(vendorCode, q.Get("error_description"), d.app.client, echoed...))
	}
	switch {
	case q.Get("state") != d.state:
		return builtinintegrations.Step{}, errors.New(errSignInNoState)
	case code == "":
		return builtinintegrations.Step{}, errors.New(errSignInNoCode)
	}
	app, _, err := d.s.signInClientFor(ctx, d.name, d.m, nil)
	if err != nil {
		log.Printf("integration %s: read the connection to finish the sign-in: %v", d.name, err)
		return builtinintegrations.Step{}, errors.New("the connection could not be read; start the sign-in again")
	}
	tok, err := d.s.connectEngine().Exchange(ctx, app.ep, app.client, code, d.redirect, d.verifier)
	if ctx.Err() != nil {
		// Stopped meanwhile: tokens the vendor gave are revoked, not kept.
		if err == nil {
			d.s.revokeTokens(ctx, d.name, "", app, tok)
		}
		return builtinintegrations.Step{}, errors.New(errSetupStopped)
	}
	if err != nil {
		words := signInFailureWords(err, connect.Tokens{}, app.client, echoed...)
		var term *connect.Terminal
		if !errors.As(err, &term) {
			log.Printf("integration %s: exchange the sign-in code: %s", d.name, words)
		}
		return builtinintegrations.Step{}, errors.New(words)
	}
	d.app = app
	if err := d.keepSignIn(ctx, integration.MethodBrowser, tok); err != nil {
		return builtinintegrations.Step{}, err
	}
	return builtinintegrations.Step{Done: true}, nil
}

// keepSignIn keeps tok on the integration's one connection
// (finishSignIn) and runs the connection test.
func (d *signInSetup) keepSignIn(ctx context.Context, method string, tok connect.Tokens) error {
	d.s.setupScrub(d.run, tok.Access, tok.Refresh)
	rec, err := d.s.Store.GetIntegrationConnection(d.name)
	if err != nil {
		d.s.revokeTokens(ctx, d.name, "", d.app, tok)
		return errors.New(errConnectionGone)
	}
	if err := d.s.finishSignIn(ctx, d.name, rec.ID, d.m, d.app, method, tok); err != nil {
		return err
	}
	d.s.testAfterSignIn(ctx, d.name, rec.ID)
	return nil
}

// errSignInNotKept: the vendor gave the tokens, but Kivali could not
// keep them.
var errSignInNotKept = errors.New("the sign-in worked, but Kivali could not keep it; start it again")

// finishSignIn keeps tok, the tokens of a sign-in by method of name's
// connection connID, under lockIntegrations: the tokens to the
// connection's Secret, which keeps its other values (a refresh token
// tok lacks drops the stored one), and the sign-in to its record, which
// clears a refused refresh's error (and so the Needs you row) and the
// last connection test. It starts the bundle's server again, which then
// asks for the new token. A flow cancelled meanwhile (ctx), a
// connection gone, or a Secret or record that cannot be written keeps
// nothing, and the tokens are revoked with the vendor, best effort.
func (s *Server) finishSignIn(ctx context.Context, name, connID string, m *integration.Manifest, app signInApp, method string, tok connect.Tokens) error {
	unlock := s.lockIntegrations()
	rec, data, err := s.readSignIn(ctx, name, connID)
	if ctx.Err() != nil || errors.Is(err, store.ErrNotFound) {
		unlock()
		s.revokeTokens(ctx, name, connID, app, tok)
		if ctx.Err() != nil {
			return errors.New(errSetupStopped)
		}
		return errors.New(errConnectionGone)
	}
	if err == nil {
		data[connect.SecretKeyAccessToken] = tok.Access
		delete(data, connect.SecretKeyRefreshToken)
		if tok.Refresh != "" {
			data[connect.SecretKeyRefreshToken] = tok.Refresh
		}
		if rec.SecretName == "" {
			rec.SecretName = secrets.ConnectionSecretName(name, rec.ID)
		}
		err = s.Secrets.Put(ctx, rec.SecretName, data)
	}
	if err == nil {
		now := s.clk().Now()
		rec.SignIn = &store.ConnectionSignIn{Method: method, SignedInAt: now, ExpiresAt: tok.ExpiresAt, Scope: tok.Scope}
		// How long the vendor says a token lasts, for a refresh whose
		// answer says nothing of it.
		if !tok.ExpiresAt.IsZero() {
			rec.SignIn.Lifetime = tok.ExpiresAt.Sub(now)
		}
		rec.VerifiedAt, rec.Verified, rec.VerifyMessage = nil, nil, ""
		err = s.Store.PutIntegrationConnection(name, rec)
	}
	unlock()
	if err != nil {
		log.Printf("integration %s: keep connection %s's sign-in: %v", name, connID, err)
		// Tokens Kivali could not keep are of use to nobody: revoked
		// with the vendor, best effort, as for a cancelled flow.
		s.revokeTokens(ctx, name, connID, app, tok)
		return errSignInNotKept
	}
	// The running server starts again with the new token.
	s.Integrations.ConnectionChanged(name)
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

var (
	_ setupFormChecker = (*tokenSetup)(nil)
	_ setupFormChecker = (*signInSetup)(nil)
	_ setupWaiter      = (*signInSetup)(nil)
	_ setupBinder      = (*signInSetup)(nil)
)
