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
	case len(v.conns) == 0 || v.discarding:
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

// usableConn is the connection an agent whose access names connID
// calls with: that one for an integration that takes several, the
// oldest otherwise; nil for none.
func (v *integrationView) usableConn(connID string) *connView {
	if v.multiple() {
		return v.connByID(connID)
	}
	return v.first()
}

// agentUsable reports whether an agent whose access names connID can
// call v now: installed and running, its connection with all its
// secrets and, made by signing in, signed in.
func (v *integrationView) agentUsable(connID string) bool {
	c := v.usableConn(connID)
	return v.state() == apitypes.IntegrationStateInstalled && v.running && c != nil && v.usable(c)
}

// coreRequirements are Kivali's own rows of v, installed or off: an
// approved version, the connection's secrets, the sign-in, the
// connection test once it has run, and, for a bundle switched on, its
// server running. at is when each unmet row has stood since, as far as
// Kivali knows, for Needs you.
func (v *integrationView) coreRequirements() ([]apitypes.IntegrationRequirement, map[string]time.Time) {
	first := v.first()
	at := map[string]time.Time{}
	since := first.rec.CreatedAt
	ver := apitypes.IntegrationRequirement{ID: reqVersion, Title: "Approved version", Met: true, Words: "Version " + v.it.CurrentVersion + " is in use."}
	if v.it.Builtin {
		ver.Words = "Version " + v.it.CurrentVersion + " ships with Kivali."
	}
	rows := []apitypes.IntegrationRequirement{ver}

	conn := apitypes.IntegrationRequirement{ID: reqConnection, Title: "Connection", Met: true, Words: "Its secrets are stored."}
	var missing []string
	for _, c := range v.conns {
		if c.reconnect {
			missing = append(missing, cmp.Or(v.connLabel(c), "The connection"))
		}
		if !v.multiple() {
			break
		}
	}
	if len(missing) > 0 {
		conn.Met, conn.Fix = false, builtinintegrations.FlowSetup
		subject := "Its secrets are"
		if v.multiple() {
			subject = strings.Join(missing, ", ") + "'s secrets are"
		}
		conn.Words = subject + " not stored on this Kivali server, as after a restore from a backup. Enter them again."
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
// for its first connection, asked for under requirementsTimeout, with
// the connection's secrets scrubbed from their words. A row of an id
// that is not one, of one of Kivali's ids or of one already given is
// left out. An integration that could not give its rows has one unmet
// row (reqChecks) saying why. nil for an integration with none, and
// while the connection needs a sign-in or the service did not answer
// its renewal: the sign_in row says so.
func (s *Server) ownRequirements(ctx context.Context, v *integrationView) []apitypes.IntegrationRequirement {
	ro, ok := v.builtin.(builtinintegrations.RequirementsOf)
	if !ok || v.first() == nil {
		return nil
	}
	name, display := v.it.Name, v.displayName()
	conn, _, scrub, err := s.builtinConnection(ctx, name, v.first().rec.ID)
	switch {
	case errors.Is(err, errSignInNeeded), errors.Is(err, errVendorUnavailable), errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		log.Printf("integration %s: read the connection for its requirements: %v", name, err)
		return []apitypes.IntegrationRequirement{{ID: reqChecks, Title: display + "'s own checks", Words: "Kivali could not read the connection to check them."}}
	}
	got, err := runBuiltin(ctx, s.clk(), requirementsTimeout, func(ctx context.Context) ([]builtinintegrations.Requirement, error) {
		return ro.Requirements(ctx, conn)
	})
	if err != nil {
		words := err.Error()
		if errors.Is(err, errIntegrationTimeout) {
			words = "it did not answer within 5 seconds"
		}
		words = clipRunes(scrub.Replace(words), requirementWordsRunes)
		return []apitypes.IntegrationRequirement{{ID: reqChecks, Title: display + "'s own checks", Words: display + " could not check its requirements: " + sentenceEnd(words)}}
	}
	out := []apitypes.IntegrationRequirement{}
	seen := map[string]bool{}
	for _, r := range got {
		if !requirementID.MatchString(r.ID) || slices.Contains(coreRequirementIDs, r.ID) || seen[r.ID] {
			log.Printf("integration %s: a requirement with the id %q is left out", name, r.ID)
			continue
		}
		seen[r.ID] = true
		out = append(out, apitypes.IntegrationRequirement{
			ID:    r.ID,
			Title: clipRunes(scrub.Replace(cmp.Or(r.Title, r.ID)), requirementTitleRunes),
			Words: clipRunes(scrub.Replace(r.Words), requirementWordsRunes),
			Met:   r.Met,
			Fix:   r.Fix,
		})
	}
	return out
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

// integrationSummary is v's row of the list: its state and, installed
// and on, how many of its requirements are not met.
func (s *Server) integrationSummary(ctx context.Context, v *integrationView) apitypes.IntegrationSummary {
	sum := v.summary()
	if sum.State == apitypes.IntegrationStateInstalled {
		rows, _ := s.requirements(ctx, v)
		sum.Needs = unmet(rows)
	}
	return sum
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
					since = v.first().rec.CreatedAt
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
		return setupSpec{}, &connWriteError{http.StatusConflict, errFlowNotOffered(flow, v.displayName()), v.whoBuilt()}
	}
	return setupSpec{driver: si, fields: c.Setup.Fields, hosts: c.Setup.Hosts, keeps: c.Setup.Keeps.Keys(), keep: s.keepSetupValues}, nil
}
