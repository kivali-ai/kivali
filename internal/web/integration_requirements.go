// Package web (integration_requirements.go) — an integration's
// lifecycle and its health.
//
// Every integration moves through the same states
// (apitypes.IntegrationState): available (a built-in this binary
// carries, a bundle once a version is approved), setting up (its setup
// flow runs, from no connection), installed (the connection exists,
// and agents can be given access), off (switched off, the connection
// and access kept), and back to available by Uninstall. A record with
// no approved version is pending; a built-in this binary does not
// carry is not included.
//
// Requirements are an installed integration's health: Kivali's rows
// first (an approved version, the connection's secrets, a sign-in the
// service still accepts, the connection test, a bundle's server
// running), then the integration's own (builtinintegrations.
// RequirementsOf), asked for when the page, the list or Home is read,
// never on a timer, under requirementsTimeout. Each unmet row of an
// installed integration is a Needs-you row (integration_requirement),
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
// the page, the list and Home wait for it.
const requirementsTimeout = 5 * time.Second

// Bounds of an integration's own rows, in runes.
const (
	requirementTitleRunes = 80
	requirementWordsRunes = 500
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
	case v.installing:
		return apitypes.IntegrationStateSettingUp
	case v.conn == nil || v.discarding:
		// A connection a failed install made is on its way out.
		return apitypes.IntegrationStateAvailable
	case !v.it.Enabled:
		return apitypes.IntegrationStateOff
	}
	return apitypes.IntegrationStateInstalled
}

// installed reports whether v has its requirements read: installed or
// off.
func (v *integrationView) installed() bool {
	st := v.state()
	return st == apitypes.IntegrationStateInstalled || st == apitypes.IntegrationStateOff
}

// agentUsable reports whether an agent with access can call v now:
// installed and running, its connection with all its secrets and, made
// by signing in, signed in.
func (v *integrationView) agentUsable() bool {
	return v.state() == apitypes.IntegrationStateInstalled && v.running && v.conn != nil && !v.several && v.usable(v.conn)
}

// coreRequirements are Kivali's own rows of v, installed or off: an
// approved version, the connection's secrets, the sign-in, the
// connection test once it has run, and, for a bundle switched on, its
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

	// A built-in of Go code runs while the binary carries it; a bundle,
	// a shipped one too, while its server runs, which it does only while
	// the integration is on.
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

// ownRequirements are v's own rows (builtinintegrations.RequirementsOf)
// for its connection, asked for under requirementsTimeout, with
// the connection's secrets scrubbed from their words. A row of an id
// that is not one, of one of Kivali's ids or of one already given is
// left out. An integration that could not give its rows has one unmet
// row (reqChecks) saying why. nil for an integration with none, and
// while the connection needs a sign-in or the service did not answer
// its renewal: the sign_in row says so.
func (s *Server) ownRequirements(ctx context.Context, v *integrationView) []apitypes.IntegrationRequirement {
	ro, ok := v.builtin.(builtinintegrations.RequirementsOf)
	if !ok || v.conn == nil || v.several {
		return nil
	}
	name, connID := v.it.Name, v.conn.rec.ID
	gen := s.Integrations.connectionGen(name)
	if rows, ok := s.requirementsCache.get(name, connID, gen, s.clk().Now()); ok {
		return rows
	}
	rows := s.askRequirements(ctx, v, ro)
	s.requirementsCache.put(name, connID, gen, s.clk().Now(), rows)
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

// requirementsCacheTTL is how long an integration's own rows are kept:
// long enough that the page, the list and Home read on one screen ask
// the integration once.
const requirementsCacheTTL = 3 * time.Second

// requirementsCache holds each integration's own rows for
// requirementsCacheTTL on the server's clock, for the connection it
// asked with: a change of the connection (integrationRegistry's
// connection generation), a Fix done or Uninstall makes it ask again.
// The zero value is ready to use.
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

func (c *requirementsCache) get(name, connID string, gen uint64, now time.Time) ([]apitypes.IntegrationRequirement, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.rows[name]
	if !ok || e.connID != connID || e.gen != gen || !now.Before(e.at.Add(requirementsCacheTTL)) {
		return nil, false
	}
	return slices.Clone(e.rows), true
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

// requirements are v's rows, Kivali's then its own, with when each
// unmet one of Kivali's has stood since; none for an integration that
// is not installed or off.
func (s *Server) requirements(ctx context.Context, v *integrationView) ([]apitypes.IntegrationRequirement, map[string]time.Time) {
	if !v.installed() {
		return []apitypes.IntegrationRequirement{}, nil
	}
	rows, at := v.coreRequirements()
	return append(rows, s.ownRequirements(ctx, v)...), at
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
// requirement is met, or the integration is switched off or
// uninstalled.
const integrationRequirementNeedPrefix = "integration_requirement:"

// integrationNeedFrom is who an integration_requirement row is from.
var integrationNeedFrom = apitypes.PersonRef{Slug: store.BuiltinIntegrationPublisher, Name: "Kivali"}

// integrationRequirementItems are the Needs-you rows of every installed
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
		// Switched off, nothing calls it, and an integration that has
		// never had a version approved has no connection.
		if !it.Enabled || it.CurrentVersion == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.loadIntegration(ctx, it.Name)
			if err != nil || v.state() != apitypes.IntegrationStateInstalled {
				return
			}
			rows, at := s.requirements(ctx, v)
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
// its Fix: the flows a built-in's setup runs besides installing.
func (s *Server) fixNamed(ctx context.Context, v *integrationView, flow string) bool {
	if !v.installed() {
		return false
	}
	return slices.ContainsFunc(s.ownRequirements(ctx, v), func(r apitypes.IntegrationRequirement) bool { return r.Fix == flow })
}

// fixSpecFor is the flow POST .../setup runs for a requirement's Fix
// named flow (one fixNamed found): the built-in's own setup, held to
// its declarations, run with that flow's name. It hands over to no
// sign-in: the sign_in row's Fix does that.
func (s *Server) fixSpecFor(v *integrationView, flow string) (setupSpec, *connWriteError) {
	si, ok := v.builtin.(builtinintegrations.SetupIntegration)
	c := v.manifest.Kivali.Connection
	if !ok || c.Method != integration.MethodSetup || c.Setup == nil {
		// Its requirements name a flow, but only a setup connection's
		// own setup runs one.
		return setupSpec{}, &connWriteError{http.StatusConflict, v.displayName() + "'s setup does not run its own flows", v.whoBuilt()}
	}
	return setupSpec{driver: si, fields: c.Setup.Fields, hosts: c.Setup.Hosts, keeps: c.Setup.Keeps.Keys(), keep: s.keepSetupValues}, nil
}

// listedIntegration is one integration as a list reads it: its row and
// its requirements (none unless installed or off).
type listedIntegration struct {
	sum  apitypes.IntegrationSummary
	rows []apitypes.IntegrationRequirement
}

// listIntegrations reads each of its, and asks each installed one for
// its requirements, all at once, so a list waits requirementsTimeout at
// most however many integrations there are. The answer keeps its'
// order; err is the first integration's that could not be read, and
// bad its name.
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
			rows, _ := s.requirements(ctx, v)
			sum := v.summary()
			if sum.State == apitypes.IntegrationStateInstalled {
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
