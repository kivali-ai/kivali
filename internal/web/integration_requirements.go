// Package web (integration_requirements.go) — an integration's
// lifecycle and its health.
//
// Every integration moves through the same states
// (apitypes.IntegrationState): installed (on the team with no
// connection: a built-in this binary carries, a bundle once a version
// is approved), connected (the connection exists and the switch is on:
// agents can be given access), disabled (the switch is off, the
// connection and access kept), and back to installed by Disconnect. A
// record with no approved version is pending; a built-in this binary
// does not carry is not included.
//
// Requirements are a connected integration's health: Kivali's rows
// first (an approved version, the connection's secrets, a sign-in the
// service still accepts, the connection test, a bundle's server
// running), then the integration's own (builtinintegrations.
// RequirementsOf), asked for when the page, the list or Home is read,
// never on a timer, under requirementsTimeout. Each unmet row of a
// connected integration is a Needs-you row (integration_requirement),
// and its Fix runs a flow through the setup runner: setup, sign_in, or
// a flow the integration's rows name.

package web

import (
	"cmp"
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Kivali's own requirement rows, by id.
const (
	reqVersion        = "version"
	reqConnection     = "connection"
	reqSignIn         = "sign_in"
	reqConnectionTest = "connection_test"
	reqRunning        = "running"
	// reqChecks is the row that stands for an integration's own rows
	// when it could not give them.
	reqChecks = "checks"
)

// flowSignIn is the flow the sign_in row's Fix runs: the standard
// sign-in again, in the browser or with a code.
const flowSignIn = "sign_in"

// requirementsTimeout bounds an integration's own Requirements call:
// the page and the refresh wait for it.
const requirementsTimeout = 5 * time.Second

// requirementsRefreshEvery is how often the refresh asks every
// connected integration for its own rows, so what the list and Home
// show stays current without either asking: a vendor is asked on this
// clock and when the integration's page is read, never for a list.
const requirementsRefreshEvery = time.Hour

// Bounds of an integration's own rows: a title's and words' runes, and
// how many rows it may give (each unmet one is a Needs-you row, and a
// bundle's server is someone else's code).
const (
	requirementTitleRunes = 80
	requirementWordsRunes = 500
	requirementRowsMax    = 20
)

// requirementID is an id an integration's own row may have.
var requirementID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// coreRequirementIDs are the ids Kivali's own rows take; an
// integration's row of one of these ids is left out.
var coreRequirementIDs = []string{reqVersion, reqConnection, reqSignIn, reqConnectionTest, reqRunning, reqChecks}

// state is where v stands in its lifecycle.
func (v *integrationView) state() apitypes.IntegrationState {
	switch {
	case v.staleBuiltin():
		return apitypes.IntegrationStateNotIncluded
	case v.it.CurrentVersion == "":
		return apitypes.IntegrationStatePending
	case v.installing, v.conn == nil, v.discarding, v.connectionMisfit(), v.conn.rec.SetupPending:
		// No connection, or none that stands: the flow that connects it
		// is under way, a connection a failed connect made is on its way
		// out, one an earlier version made does not fit this version's
		// setup, or one a connect left unfinished when Kivali stopped
		// waits to be discarded.
		return apitypes.IntegrationStateInstalled
	case !v.it.Enabled:
		return apitypes.IntegrationStateDisabled
	}
	return apitypes.IntegrationStateConnected
}

// connectionMisfit reports whether v's connection was made by an
// earlier version's way of connecting and does not fit this version's
// setup flow (setupMisfit). v then reads installed, its one row says
// to set it up again, and connecting it takes the connection over
// (keepSetupValues): its record and every agent's access stay, and its
// values are replaced.
func (v *integrationView) connectionMisfit() bool {
	return v.conn != nil && !v.several && v.manifest != nil && setupMisfit(v.manifest.Kivali.Connection.Setup, v.conn.rec)
}

// setupMisfit reports whether rec does not fit setup, a manifest's
// kivali.connection.setup (nil for a connection not made by setup): it
// holds a value the flow does not keep (a field in the record, which
// setup never writes; a key in its Secret outside setup.keeps; a
// sign-in, for a flow that hands over to none), or none of what the
// flow keeps. A GitHub 1.0.0 connection, the owner's own app signed in
// with, is one for GitHub 2.0.0, whose setup makes the app.
//
// An app's client id and secret, the pair, in the Secret beside what
// the flow keeps (keptClient) fit: a setup flow keeps them for the app
// it made, and one whose keeps lack them leaves them unread, since a
// call is given no value its manifest does not declare. Either of them
// alone is a key outside the keeps like any other. A connection made
// by signing in holds its client id in the record, so it still does
// not fit.
func setupMisfit(setup *integration.Setup, rec store.IntegrationConnection) bool {
	switch {
	case setup == nil:
		return false
	case len(rec.Fields) > 0, rec.SignIn != nil && setup.Then == "":
		return true
	}
	client := keptClient(rec.SecretKeys)
	held := false
	for _, k := range rec.SecretKeys {
		switch {
		case setup.Keeps.Has(k):
			held = true
		case !client || (k != integration.FieldClientID && k != integration.FieldClientSecret):
			return true
		}
	}
	return !held
}

// keptClient reports whether keys hold both of the keys a setup flow
// keeps the app it made's client under: client_id and client_secret.
func keptClient(keys []string) bool {
	return slices.Contains(keys, integration.FieldClientID) && slices.Contains(keys, integration.FieldClientSecret)
}

// misfitRequirement is the one row of an integration whose connection
// an earlier version made and that this version's setup does not
// take: install it again.
func (v *integrationView) misfitRequirement() apitypes.IntegrationRequirement {
	return apitypes.IntegrationRequirement{
		ID: reqConnection, Title: "Connection", Fix: builtinintegrations.FlowSetup,
		Words: "Set up " + v.displayName() + " again: this version makes its connection by its own setup, and the one an earlier version made does not fit it. Every agent's access is kept.",
	}
}

// configured reports whether v holds a connection this version takes,
// and so has its requirements read: connected or disabled.
func (v *integrationView) configured() bool {
	st := v.state()
	return st == apitypes.IntegrationStateConnected || st == apitypes.IntegrationStateDisabled
}

// agentUsable reports whether an agent with access can call v now:
// connected and running, its connection with all its secrets and, made
// by signing in, signed in.
func (v *integrationView) agentUsable() bool {
	return v.state() == apitypes.IntegrationStateConnected && v.running && v.conn != nil && !v.several && v.usable(v.conn)
}

// coreRequirements are Kivali's own rows of v, connected or disabled:
// an approved version, the connection's secrets, the sign-in, the
// connection test once it has run, and, for a bundle enabled, its
// server running. at is when each unmet row has stood since, as far as
// Kivali knows, for Needs you.
func (v *integrationView) coreRequirements() ([]apitypes.IntegrationRequirement, map[string]time.Time) {
	first := v.conn
	at := map[string]time.Time{}
	since := first.rec.CreatedAt
	ver := apitypes.IntegrationRequirement{ID: reqVersion, Title: "Approved version", Met: true, Words: "Version " + v.it.CurrentVersion + " is in use."}
	if v.it.Builtin {
		ver.Words = "Version " + v.it.CurrentVersion + " ships with Kivali."
	}
	rows := []apitypes.IntegrationRequirement{ver}

	conn := apitypes.IntegrationRequirement{ID: reqConnection, Title: "Connection", Met: true, Words: "Its secrets are stored."}
	switch {
	case v.several:
		// No Fix: a flow would write into one of the records.
		conn.Met = false
		conn.Words = "It holds more than one connection record, and Kivali could not bring it to one; the log says why. Agents cannot use it until it holds one."
		at[reqConnection] = since
	case first.reconnect:
		conn.Met, conn.Fix = false, builtinintegrations.FlowSetup
		conn.Words = "Its secrets are not stored on this Kivali server, as after a restore from a backup. Enter them again."
		at[reqConnection] = since
	}
	rows = append(rows, conn)

	if v.signIn() {
		rows = append(rows, v.signInRequirement(first, at))
	}

	if v.connectionTest() && first.rec.VerifiedAt != nil && first.rec.Verified != nil {
		test := apitypes.IntegrationRequirement{ID: reqConnectionTest, Title: "Connection test", Met: *first.rec.Verified}
		switch {
		case test.Met:
			test.Words = cmp.Or(first.rec.VerifyMessage, "The connection test passed.")
		case first.rec.VerifyMessage != "":
			test.Words = "The connection test failed: " + first.rec.VerifyMessage
			at[reqConnectionTest] = *first.rec.VerifiedAt
		default:
			test.Words = "The connection test failed."
			at[reqConnectionTest] = *first.rec.VerifiedAt
		}
		rows = append(rows, test)
	}

	// A built-in runs while the binary carries it; a bundle while its
	// server runs, which it does only while the integration is on.
	if v.builtin == nil && v.it.Enabled {
		run := apitypes.IntegrationRequirement{ID: reqRunning, Title: "Running", Met: v.running, Words: "It is running."}
		if !v.running {
			run.Words = "It is not running: Kivali is starting it, or it stopped. Its log says what happened."
			at[reqRunning] = since
		}
		rows = append(rows, run)
	}
	return rows, at
}

// signInRequirement is the sign_in row of c, a connection made by
// signing in (or by a setup flow that hands over to a sign-in).
func (v *integrationView) signInRequirement(c *connView, at map[string]time.Time) apitypes.IntegrationRequirement {
	row := apitypes.IntegrationRequirement{ID: reqSignIn, Title: "Sign-in", Fix: flowSignIn, Met: !c.rec.NeedsSignIn(c.hasToken)}
	si := c.rec.SignIn
	switch {
	case row.Met && si.Method == "device":
		row.Words = "Signed in with a code."
	case row.Met:
		row.Words = "Signed in in the browser."
	case si == nil:
		row.Words = "Not signed in yet."
		at[reqSignIn] = c.rec.CreatedAt
	case si.Error != "":
		row.Words = "The service no longer accepts the sign-in: " + sentenceEnd(si.Error) + " Agents cannot use " + v.displayName() + " until you sign in again."
		at[reqSignIn] = si.ErrorAt
	default:
		row.Words = "Its sign-in is not stored on this Kivali server, as after a restore from a backup. Sign in again."
		at[reqSignIn] = si.SignedInAt
	}
	return row
}

// sentenceEnd is words ending in a full stop.
func sentenceEnd(words string) string {
	words = strings.TrimSpace(words)
	if words == "" || strings.HasSuffix(words, ".") || strings.HasSuffix(words, "?") || strings.HasSuffix(words, "!") {
		return words
	}
	return words + "."
}

// ownRequirements are v's own rows (builtinintegrations.RequirementsOf,
// or a setup bundle's kivali/requirements while its server runs) for
// its connection, with the connection's secrets scrubbed from their
// words. A row of an id that is not one, of one of Kivali's ids or of
// one already given is left out. An integration that could not give
// its rows has one unmet row (reqChecks) saying why. nil for an
// integration with none, and while the connection needs a sign-in or
// the service did not answer its renewal: the sign_in row says so.
//
// fresh asks the integration, under requirementsTimeout, unless it
// was asked within requirementsCacheTTL: the page and the refresh.
// Not fresh is what the last ask answered for this connection, and nil
// before any: the list and Home, read often, never wait on a vendor.
func (s *Server) ownRequirements(ctx context.Context, v *integrationView, fresh bool) []apitypes.IntegrationRequirement {
	ro, ok := s.requirementsOf(v)
	if !ok || v.conn == nil || v.several {
		return nil
	}
	name, connID := v.it.Name, v.conn.rec.ID
	gen := s.Integrations.connectionGen(name)
	now := s.clk().Now()
	rows, at, ok := s.requirementsCache.get(name, connID, gen)
	switch {
	case ok && (!fresh || now.Before(at.Add(requirementsCacheTTL))):
		return rows
	case !fresh:
		return nil
	}
	rows = s.askRequirements(ctx, v, ro)
	s.requirementsCache.put(name, connID, gen, now, rows)
	return rows
}

// ownAnswer is what an integration's Requirements gave, with the
// scrubber of the connection it was given.
type ownAnswer struct {
	rows  []builtinintegrations.Requirement
	scrub *strings.Replacer
	// skip is true when the connection needs a sign-in, or the service
	// did not answer its renewal: the sign_in row says so.
	skip bool
}

// askRequirements asks ro for v's rows, reading the connection (which
// may renew a sign-in with the service) and calling Requirements under
// one requirementsTimeout.
func (s *Server) askRequirements(ctx context.Context, v *integrationView, ro builtinintegrations.RequirementsOf) []apitypes.IntegrationRequirement {
	name, display := v.it.Name, v.displayName()
	connID := v.conn.rec.ID
	errNoConn := errors.New("the connection could not be read")
	got, err := runBuiltin(ctx, s.clk(), requirementsTimeout, func(ctx context.Context) (ownAnswer, error) {
		conn, _, scrub, err := s.builtinConnection(ctx, name, connID)
		switch {
		case errors.Is(err, errSignInNeeded), errors.Is(err, errVendorUnavailable), errors.Is(err, store.ErrNotFound):
			return ownAnswer{skip: true}, nil
		case err != nil:
			log.Printf("integration %s: read the connection for its requirements: %v", name, err)
			return ownAnswer{}, errNoConn
		}
		rows, err := ro.Requirements(ctx, conn)
		if err != nil {
			err = errors.New(scrub.Replace(err.Error()))
		}
		return ownAnswer{rows: rows, scrub: scrub}, err
	})
	switch {
	case got.skip && err == nil:
		return nil
	case errors.Is(err, errNoConn):
		return []apitypes.IntegrationRequirement{{ID: reqChecks, Title: display + "'s own checks", Words: "Kivali could not read the connection to check them."}}
	case err != nil:
		words := err.Error()
		if errors.Is(err, errIntegrationTimeout) {
			words = "it did not answer within 5 seconds"
		}
		return []apitypes.IntegrationRequirement{{ID: reqChecks, Title: display + "'s own checks", Words: display + " could not check its requirements: " + sentenceEnd(clipRunes(words, requirementWordsRunes))}}
	}
	out := []apitypes.IntegrationRequirement{}
	seen := map[string]bool{}
	for _, r := range got.rows {
		if !requirementID.MatchString(r.ID) || slices.Contains(coreRequirementIDs, r.ID) || seen[r.ID] {
			log.Printf("integration %s: a requirement with the id %q is left out", name, r.ID)
			continue
		}
		if r.Fix != "" && !requirementID.MatchString(r.Fix) {
			log.Printf("integration %s: the requirement %s names a fix that is not a flow's name, and is left out", name, r.ID)
			continue
		}
		if len(out) == requirementRowsMax {
			log.Printf("integration %s: it gave more than %d requirements; the requirement %s and those after it are left out", name, requirementRowsMax, r.ID)
			break
		}
		seen[r.ID] = true
		out = append(out, apitypes.IntegrationRequirement{
			ID:    r.ID,
			Title: clipRunes(got.scrub.Replace(cmp.Or(r.Title, r.ID)), requirementTitleRunes),
			Words: clipRunes(got.scrub.Replace(r.Words), requirementWordsRunes),
			Met:   r.Met,
			Fix:   r.Fix,
		})
	}
	return out
}

// requirementsCacheTTL is how long a fresh read takes the last answer
// for: long enough that a page read twice on one screen asks the
// integration once.
const requirementsCacheTTL = 3 * time.Second

// requirementsCache holds what each integration last answered, for
// the connection it was asked with and when, on the server's clock. A
// change of the connection (integrationRegistry's connection
// generation), a Fix done or Uninstall drops the answer, so a list
// shows no own rows until the page or the refresh asks again. The
// zero value is ready to use.
type requirementsCache struct {
	mu   sync.Mutex
	rows map[string]cachedRequirements
}

type cachedRequirements struct {
	connID string
	gen    uint64
	at     time.Time
	rows   []apitypes.IntegrationRequirement
}

// get is name's last answer for connID and gen, and when it was given.
func (c *requirementsCache) get(name, connID string, gen uint64) ([]apitypes.IntegrationRequirement, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.rows[name]
	if !ok || e.connID != connID || e.gen != gen {
		return nil, time.Time{}, false
	}
	return slices.Clone(e.rows), e.at, true
}

func (c *requirementsCache) put(name, connID string, gen uint64, now time.Time, rows []apitypes.IntegrationRequirement) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows == nil {
		c.rows = map[string]cachedRequirements{}
	}
	c.rows[name] = cachedRequirements{connID: connID, gen: gen, at: now, rows: slices.Clone(rows)}
}

// forget drops name's rows, so the next read asks again.
func (c *requirementsCache) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.rows, name)
}

// requirements are v's rows, Kivali's then its own (ownRequirements,
// asked afresh when fresh), with when each unmet one of Kivali's has
// stood since; none for an integration that is not connected or
// disabled, but the one row of an installed one whose connection an
// earlier version made (connectionMisfit).
func (s *Server) requirements(ctx context.Context, v *integrationView, fresh bool) ([]apitypes.IntegrationRequirement, map[string]time.Time) {
	if !v.configured() {
		if v.state() == apitypes.IntegrationStateInstalled && v.connectionMisfit() {
			return []apitypes.IntegrationRequirement{v.misfitRequirement()}, nil
		}
		return []apitypes.IntegrationRequirement{}, nil
	}
	rows, at := v.coreRequirements()
	return append(rows, s.ownRequirements(ctx, v, fresh)...), at
}

// StartRequirementsRefresh starts the loop that asks every connected
// integration for its own rows: once now, then every
// requirementsRefreshEvery on the server's clock. Returns a stop
// function, which waits for a pass under way to end.
func (s *Server) StartRequirementsRefresh() func() {
	if s.Store == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			s.refreshRequirements(ctx)
			// Armed after the pass, so the next one is an hour after
			// this one ended.
			t := s.clk().NewTimer(requirementsRefreshEvery)
			select {
			case <-stop:
				t.Stop()
				return
			case <-t.C():
			}
		}
	}()
	return func() {
		cancel()
		close(stop)
		<-done
	}
}

// refreshRequirements asks each connected integration for its own
// rows, all at once, so the list and Home show what it answers. One
// that cannot be asked (none of its own, its server not running) is
// left as it is.
func (s *Server) refreshRequirements(ctx context.Context) {
	its, err := s.Store.ListIntegrations()
	if err != nil {
		log.Printf("integration requirements: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, it := range its {
		if !it.Enabled || it.CurrentVersion == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.loadIntegration(ctx, it.Name)
			if err != nil || v.state() != apitypes.IntegrationStateConnected {
				return
			}
			s.ownRequirements(ctx, v, true)
		}()
	}
	wg.Wait()
}

// unmet counts the rows not met.
func unmet(rows []apitypes.IntegrationRequirement) int {
	n := 0
	for _, r := range rows {
		if !r.Met {
			n++
		}
	}
	return n
}

// integrationRequirementNeedPrefix is the ID prefix of an integration's
// Needs-you row. The row takes no answer: it leaves once its
// requirement is met, or the integration is disabled or
// uninstalled.
const integrationRequirementNeedPrefix = "integration_requirement:"

// integrationNeedFrom is who an integration_requirement row is from.
var integrationNeedFrom = apitypes.PersonRef{Slug: store.BuiltinIntegrationPublisher, Name: "Kivali"}

// integrationRequirementItems are the Needs-you rows of every connected
// integration's unmet requirements, read as they stand, each
// integration's asked for at once, oldest first. A row whose start
// Kivali does not know (an integration's own) stands from when the
// connection was made.
func (s *Server) integrationRequirementItems(ctx context.Context) []apitypes.NeedItem {
	its, err := s.Store.ListIntegrations()
	if err != nil {
		return nil
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out []apitypes.NeedItem
	)
	for _, it := range its {
		// Disabled, nothing calls it, and an integration that has
		// never had a version approved has no connection.
		if !it.Enabled || it.CurrentVersion == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.loadIntegration(ctx, it.Name)
			if err != nil || v.state() != apitypes.IntegrationStateConnected {
				return
			}
			rows, at := s.requirements(ctx, v, false)
			page := "/org/integrations/" + url.PathEscape(it.Name)
			display := v.displayName()
			var items []apitypes.NeedItem
			for _, r := range rows {
				if r.Met {
					continue
				}
				since, ok := at[r.ID]
				if !ok || since.IsZero() {
					since = v.conn.rec.CreatedAt
				}
				items = append(items, apitypes.NeedItem{
					ID:          integrationRequirementNeedPrefix + it.Name + "/" + r.ID,
					Kind:        apitypes.NeedKindIntegrationRequirement,
					Title:       display + ": " + r.Title,
					From:        integrationNeedFrom,
					At:          since,
					BodyMD:      r.Words,
					Attachments: []apitypes.AttachmentRef{},
					Packages:    []apitypes.SoftwarePackage{},
					ReviewPath:  &page,
				})
			}
			mu.Lock()
			out = append(out, items...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// errFlowNotOffered refuses a flow v neither installs with nor names in
// its requirements.
func errFlowNotOffered(flow, display string) string {
	if !requirementID.MatchString(flow) {
		// The caller's words are not echoed back.
		return "that is not a flow " + display + " has"
	}
	return flow + " is not a flow " + display + " has"
}

// fixNamed reports whether one of v's own requirements names flow as
// its Fix: the flows an integration's own setup runs besides
// connecting.
func (s *Server) fixNamed(ctx context.Context, v *integrationView, flow string) bool {
	if !v.configured() {
		return false
	}
	return slices.ContainsFunc(s.ownRequirements(ctx, v, true), func(r apitypes.IntegrationRequirement) bool { return r.Fix == flow })
}

// fixSpecFor is the flow POST .../setup runs for a requirement's Fix
// named flow (one fixNamed found): the integration's own setup (a
// built-in's, or a setup bundle's server), held to its declarations,
// run with that flow's name. It hands over to no sign-in: the sign_in
// row's Fix does that.
func (s *Server) fixSpecFor(v *integrationView, flow string) (setupSpec, *connWriteError) {
	si, ok := s.setupDriverOf(v)
	c := v.manifest.Kivali.Connection
	if !ok || c.Method != integration.MethodSetup || c.Setup == nil {
		// Its requirements name a flow, but only a setup connection's
		// own setup runs one.
		return setupSpec{}, &connWriteError{http.StatusConflict, v.displayName() + "'s setup does not run its own flows", v.whoBuilt()}
	}
	return setupSpec{driver: si, fields: c.Setup.Fields, hosts: c.Setup.Hosts, keeps: c.Setup.Keeps.Keys(), shown: shownKeys(v.manifest), keep: s.keepSetupValues}, nil
}

// listedIntegration is one integration as a list reads it: its row and
// its requirements (none unless connected or disabled).
type listedIntegration struct {
	sum  apitypes.IntegrationSummary
	rows []apitypes.IntegrationRequirement
}

// listIntegrations reads each of its, all at once, with the
// requirements each one last answered (never asking: a list is read
// often). The answer keeps its' order; err is the first integration's
// that could not be read, and bad its name.
func (s *Server) listIntegrations(ctx context.Context, its []store.Integration) (out []listedIntegration, bad string, err error) {
	out = make([]listedIntegration, len(its))
	errs := make([]error, len(its))
	var wg sync.WaitGroup
	for i, it := range its {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.loadIntegration(ctx, it.Name)
			if err != nil {
				errs[i] = err
				return
			}
			rows, _ := s.requirements(ctx, v, false)
			sum := v.summary()
			if sum.State == apitypes.IntegrationStateConnected {
				sum.Needs = unmet(rows)
			}
			out[i] = listedIntegration{sum: sum, rows: rows}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, its[i].Name, err
		}
	}
	return out, "", nil
}
