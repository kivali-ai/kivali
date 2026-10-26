package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Integrations under Org: the list, an integration's page, a version's
// install screen, the connection, each agent's access, and an agent's
// Integrations tab. Installing is two steps: an upload stores a version
// as pending, and approving it, here or from Needs you, makes it the
// one in use. Rolling back makes a version a later one replaced the one
// in use again. A connection's secret fields go to s.Secrets and
// nowhere else: the store keeps which are set and the Secret's name,
// and no answer here carries a value.

// wireAPIIntegrationRoutes mounts the integration routes on the API mux.
func (s *Server) wireAPIIntegrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/org/integrations", s.handleAPIIntegrations)
	mux.HandleFunc("POST /api/v1/org/integrations", s.handleAPIIntegrationUpload)
	mux.HandleFunc("GET /api/v1/org/integrations/{name}", s.handleAPIIntegration)
	mux.HandleFunc("DELETE /api/v1/org/integrations/{name}", s.handleAPIIntegrationUninstall)
	mux.HandleFunc("DELETE /api/v1/org/integrations/{name}/connection", s.handleAPIIntegrationDisconnect)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/enable", s.handleAPIIntegrationToggle(true))
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/disable", s.handleAPIIntegrationToggle(false))
	mux.HandleFunc("GET /api/v1/org/integrations/{name}/versions/{version}", s.handleAPIIntegrationVersion)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/versions/{version}/approve", s.handleAPIIntegrationAnswer(true))
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/versions/{version}/reject", s.handleAPIIntegrationAnswer(false))
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/versions/{version}/rollback", s.handleAPIIntegrationRollBack)
	mux.HandleFunc("PUT /api/v1/org/integrations/{name}/connection", s.handleAPIIntegrationConnectionPut)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/connection/verify", s.handleAPIIntegrationVerify)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/connection/sign-out", s.handleAPISignOut)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/setup", s.handleAPISetupStart)
	mux.HandleFunc("GET /api/v1/org/integrations/{name}/setup", s.handleAPISetupGet)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/setup/answer", s.handleAPISetupAnswer)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/setup/cancel", s.handleAPISetupCancel)
	mux.HandleFunc("GET /api/v1/org/integrations/{name}/setup/stream", s.handleAPISetupStream)
	mux.HandleFunc("PUT /api/v1/org/integrations/{name}/access/{slug}", s.handleAPIIntegrationAccessPut)
	mux.HandleFunc("DELETE /api/v1/org/integrations/{name}/access/{slug}", s.handleAPIIntegrationAccessDelete)
	mux.HandleFunc("GET /api/v1/org/integrations/{name}/audit", s.handleAPIIntegrationAudit)
	mux.HandleFunc("GET /api/v1/org/integrations/{name}/choices/{key}", s.handleAPIIntegrationChoices)
	mux.HandleFunc("GET /api/v1/org/integrations/{name}/logs", s.handleAPIIntegrationLogs)
	mux.HandleFunc("GET /api/v1/agents/{slug}/integrations", s.handleAPIAgentIntegrations)
}

// whoBundle fixes what is wrong with a bundle.
const whoBundle = "whoever built the bundle"

// errNotRunning answers what needs the integration running: Test
// connection and the options an access form lists.
const errNotRunning = "the integration is not running"

// integrationVersionPath is the web app's install screen for a version.
func integrationVersionPath(name, version string) string {
	return "/org/integrations/" + url.PathEscape(name) + "/versions/" + url.PathEscape(version)
}

// lockIntegrations serialises the integration writes that span the
// store and s.Secrets (a connection's read-modify-write, a removal),
// so two at once cannot leave a Secret and its record disagreeing.
func (s *Server) lockIntegrations() func() {
	s.integrationsMu.Lock()
	return s.integrationsMu.Unlock
}

// integrationView is what an integration's answers are built from.
type integrationView struct {
	it       store.Integration
	versions []store.IntegrationVersion
	// manifest is the approved version's, or the newest stored
	// version's while none is approved; nil with no version at all.
	manifest *integration.Manifest
	// conn is its connection; nil with none.
	conn   *connView
	access []store.IntegrationAccess
	// running is whether the integration can be called: its shim is
	// connected, or it is a built-in this binary carries.
	running bool
	// builtin is its code when it ships with Kivali; nil for a bundle.
	builtin builtinintegrations.Integration
	// flow is its setup flow under way, or the one that last ended;
	// nil for none. installing is true while a flow that installs it
	// runs; discarding while the connection a failed or cancelled
	// install made is on its way out (Server.setupPeek).
	flow                   *apitypes.SetupFlow
	installing, discarding bool
	// several is true when connections/ holds more than one record,
	// which a migration could not bring to one
	// (store.ErrIntegrationConnectionsSeveral); conn is the oldest then.
	several bool
}

// connView is one connection as an integration's answers read it.
type connView struct {
	rec store.IntegrationConnection
	// secretSet names the sensitive fields whose value is in the
	// connection's Secret; reconnect is true when the Secret, or a
	// value the record names, is not there.
	secretSet map[string]bool
	reconnect bool
	// hasToken is true when the Secret holds a sign-in's access token
	// (or could not be read, which is not taken for a missing one).
	hasToken bool
	// registeredAt is the host of the authorization endpoint discovery
	// found for a client Kivali registered, "" before one is.
	registeredAt string
}

// signIn reports whether core holds sign-in tokens for v's connections:
// made by signing in, or by a setup flow that hands over to a sign-in.
func (v *integrationView) signIn() bool {
	return v.manifest != nil && v.manifest.Kivali.Connection.OAuth()
}

// usable reports whether agents can use c: it has all its secrets and,
// for a connection made by signing in, works.
func (v *integrationView) usable(c *connView) bool {
	return !c.reconnect && (!v.signIn() || !c.rec.NeedsSignIn(c.hasToken))
}

// loadIntegration reads one integration whole. A missing one is
// store.ErrNotFound.
func (s *Server) loadIntegration(ctx context.Context, name string) (*integrationView, error) {
	it, err := s.Store.GetIntegration(name)
	if err != nil {
		return nil, err
	}
	v := &integrationView{it: it, running: s.integrationRunning(it), builtin: s.builtinFor(it)}
	v.flow, v.installing, v.discarding = s.setupPeek(name)
	if v.versions, err = s.Store.IntegrationVersions(name); err != nil {
		return nil, err
	}
	switch _, m, err := s.Store.CurrentIntegrationVersion(name); {
	case err == nil:
		v.manifest = m
	case errors.Is(err, store.ErrIntegrationNotApproved):
		if len(v.versions) > 0 {
			if _, m, err := s.Store.GetIntegrationVersion(name, v.versions[0].Version); err == nil {
				v.manifest = m
			}
		}
	default:
		return nil, err
	}
	switch rec, err := s.Store.GetIntegrationConnection(name); {
	case err == nil:
		v.conn = s.readConnectionSecret(ctx, name, rec)
	case errors.Is(err, store.ErrIntegrationConnectionsSeveral):
		// The page shows the oldest record, so the owner can see and
		// disconnect it; no call reaches any (readStoredConnection), and
		// the connection requirement says why.
		all, err := s.Store.ListIntegrationConnections(name)
		if err != nil || len(all) == 0 {
			return nil, cmp.Or(err, store.ErrNotFound)
		}
		v.conn, v.several = s.readConnectionSecret(ctx, name, all[0]), true
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if v.access, err = s.Store.ListIntegrationAccess(name); err != nil {
		return nil, err
	}
	return v, nil
}

// readConnectionSecret works out which secret fields of rec have their
// value in its Secret, and whether it holds a sign-in's access token.
// The values are not kept.
func (s *Server) readConnectionSecret(ctx context.Context, name string, rec store.IntegrationConnection) *connView {
	c := &connView{rec: rec, secretSet: map[string]bool{}}
	if rec.SecretName == "" {
		return c
	}
	data, err := s.Secrets.Get(ctx, rec.SecretName)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		c.reconnect = len(rec.SecretKeys) > 0
		return c
	case err != nil:
		// Not knowing is not a missing Secret: the page shows the
		// fields as set rather than ask for them again.
		log.Printf("integration %s: read the connection's Secret: %v", name, err)
		for _, k := range rec.SecretKeys {
			c.secretSet[k] = true
		}
		c.hasToken = true
		return c
	}
	c.hasToken = data[connect.SecretKeyAccessToken] != ""
	c.registeredAt = integration.EndpointHost(data[connect.SecretKeyAuthorizationEndpoint])
	for _, k := range rec.SecretKeys {
		if data[k] == "" {
			c.reconnect = true
			continue
		}
		c.secretSet[k] = true
	}
	return c
}

// staleBuiltin reports whether v is a built-in integration this binary
// does not carry: another version of Kivali
// shipped it, nothing runs it, and the owner may uninstall it.
func (v *integrationView) staleBuiltin() bool { return v.it.Builtin && v.builtin == nil }

// whoBuilt is who fixes what is wrong with v's code: Kivali's
// developers for a built-in one, whoever built the bundle otherwise.
func (v *integrationView) whoBuilt() string {
	if v.it.Builtin {
		return whoDevelopers
	}
	return whoBundle
}

// builtinMove is what the owner tried on a built-in integration that
// is refused.
type builtinMove int

const (
	builtinApprove builtinMove = iota
	builtinReject
	builtinRollBack
	builtinUpload
	builtinNotIncluded
)

// builtinRefusal is the sentence every refusal of a built-in
// integration answers with, naming it by display.
func builtinRefusal(display string, move builtinMove) string {
	switch move {
	case builtinUpload:
		return display + " ships with Kivali, so a bundle cannot replace it"
	case builtinNotIncluded:
		return display + " is not in this version of Kivali, so nothing runs it; you can uninstall it"
	}
	verb := map[builtinMove]string{builtinApprove: "approve", builtinReject: "reject", builtinRollBack: "roll back to"}[move]
	return display + " ships with Kivali, and its version changes only with Kivali's updates; there is nothing to " + verb
}

// integrationDisplay is name's display name as its record has it, or
// name.
func (s *Server) integrationDisplay(name string) string {
	if it, err := s.Store.GetIntegration(name); err == nil && it.DisplayName != "" {
		return it.DisplayName
	}
	return name
}

func (v *integrationView) displayName() string {
	if v.manifest != nil && v.it.CurrentVersion == "" && v.manifest.DisplayName != "" {
		return v.manifest.DisplayName
	}
	return cmp.Or(v.it.DisplayName, v.it.Name)
}

func (v *integrationView) summary() apitypes.IntegrationSummary {
	sum := apitypes.IntegrationSummary{
		Name:        v.it.Name,
		DisplayName: v.displayName(),
		Version:     v.it.CurrentVersion,
		State:       v.state(),
		Enabled:     v.it.Enabled,
		Builtin:     v.it.Builtin,
	}
	if v.manifest != nil {
		sum.Description = v.manifest.Description
	}
	for _, a := range v.access {
		if a.Invalid == "" {
			sum.Agents++
		}
	}
	// Versions are newest first: the first pending is the newest.
	for _, ver := range v.versions {
		if ver.State == store.IntegrationVersionPending {
			p := ver.Version
			sum.PendingVersion = &p
			break
		}
	}
	return sum
}

// connectionFields is m's connection form, with what c holds (nothing
// for nil).
func connectionFields(m *integration.Manifest, c *connView) []apitypes.ConnectionField {
	out := []apitypes.ConnectionField{}
	if m == nil {
		return out
	}
	for _, f := range integration.ConnectionFields(m) {
		cf := apitypes.ConnectionField{
			Key:         f.Key,
			Title:       f.Title,
			Description: f.Description,
			Sensitive:   f.Sensitive,
			Required:    f.Required,
		}
		switch {
		case c == nil:
		case f.Sensitive:
			cf.Set = c.secretSet[f.Key]
		default:
			if val, ok := c.rec.Fields[f.Key]; ok && val != "" {
				cf.Set = true
				cf.Value = &val
			}
		}
		out = append(out, cf)
	}
	return out
}

func integrationActions(m *integration.Manifest) []apitypes.IntegrationAction {
	out := []apitypes.IntegrationAction{}
	if m == nil {
		return out
	}
	for _, t := range m.Tools {
		out = append(out, apitypes.IntegrationAction{Name: t.Name, Description: t.Description})
	}
	return out
}

// signInScopes is what m's sign-in connection asks the vendor for;
// empty for a token connection.
func signInScopes(m *integration.Manifest) []string {
	if m == nil || !m.Kivali.Connection.SignIn() {
		return []string{}
	}
	return nonNilStrings(slices.Clone(m.Kivali.Connection.Scopes))
}

// signInHosts are the hosts m's connection signs in at
// (Connection.SignInHosts); empty for a connection core signs nothing
// in for.
func signInHosts(m *integration.Manifest) []string {
	if m == nil || !m.Kivali.Connection.OAuth() {
		return []string{}
	}
	return m.Kivali.Connection.SignInHosts()
}

// signInClientWire is whose app m's connection signs in with
// (IntegrationPage.SignInClient): operator or registered for a sign-in
// connection, setup for a setup flow that hands over to a sign-in, ""
// for a connection core signs nothing in for.
func signInClientWire(m *integration.Manifest) string {
	if m == nil {
		return ""
	}
	c := m.Kivali.Connection
	switch {
	case c.ThenMethod() != "":
		return integration.MethodSetup
	case !c.SignIn():
		return ""
	case c.Registered():
		return integration.ClientRegistered
	}
	return integration.ClientOperator
}

// setupWire is what m's setup flow may do, for the install screen and
// the page; nil for a connection that is not a setup one.
func setupWire(m *integration.Manifest) *apitypes.IntegrationSetup {
	if m == nil || m.Kivali.Connection.Method != integration.MethodSetup || m.Kivali.Connection.Setup == nil {
		return nil
	}
	s := m.Kivali.Connection.Setup
	out := &apitypes.IntegrationSetup{
		Summary: s.Summary,
		Hosts:   nonNilStrings(slices.Clone(s.Hosts)),
		Fields:  []apitypes.SetupField{},
		Keeps:   []apitypes.SetupKeep{},
		Then:    s.Then,
	}
	for _, f := range s.Fields {
		out.Fields = append(out.Fields, apitypes.SetupField{
			Key: f.Key, Title: integration.SetupFieldTitle(f), Words: f.Words, Secret: f.Secret, SetupOnly: f.SetupOnly,
		})
	}
	for _, k := range s.Keeps {
		out.Keeps = append(out.Keeps, apitypes.SetupKeep{Key: k.Key, Words: k.Words, Secret: !k.NotSecret})
	}
	return out
}

// signInPagesOf is, for each way this server offers m's connection, the
// host the sign-in goes to: the authorization endpoint's for browser,
// the device endpoint's for device. For a client Kivali registers, the
// browser's is registeredAt, the host of the authorization endpoint
// discovery found, once it has (empty before). nil for a token
// connection.
func signInPagesOf(m *integration.Manifest, registeredAt string) map[string]string {
	if m == nil || !m.Kivali.Connection.SignIn() {
		return nil
	}
	c := m.Kivali.Connection
	out := map[string]string{}
	for _, way := range offeredSignInWays(c) {
		ep := c.DeviceEndpoint
		if way == integration.MethodBrowser {
			ep = c.AuthorizationEndpoint
		}
		h := integration.EndpointHost(ep)
		if way == integration.MethodBrowser && c.Registered() {
			h = registeredAt
		}
		if h != "" {
			out[way] = h
		}
	}
	return out
}

func integrationHosts(m *integration.Manifest) []string {
	if m == nil {
		return []string{}
	}
	return nonNilStrings(slices.Clone(m.Kivali.Hosts))
}

// integrationPackages is the Debian packages m's pod installs.
func integrationPackages(m *integration.Manifest) []string {
	if m == nil {
		return []string{}
	}
	return nonNilStrings(slices.Clone(m.Kivali.Packages))
}

// accessSummary is one agent's access in words: the built-in's own
// (builtinintegrations.AccessSummarizer), or the manifest's field by
// field.
func (v *integrationView) accessSummary(access json.RawMessage) string {
	if s, ok := v.builtin.(builtinintegrations.AccessSummarizer); ok {
		var m map[string]any
		if json.Unmarshal(access, &m) != nil {
			return integration.SummaryNothingSet
		}
		return s.AccessSummary(m)
	}
	return integration.AccessSummary(v.manifest, access)
}

// accessFormWire is m's access form on the wire, nil when access is all
// or nothing.
func accessFormWire(m *integration.Manifest) *apitypes.AccessForm {
	form := integration.AccessForm(m)
	if form == nil {
		return nil
	}
	out := &apitypes.AccessForm{Fields: make([]apitypes.AccessField, 0, len(form.Fields))}
	for _, f := range form.Fields {
		out.Fields = append(out.Fields, apitypes.AccessField{
			Key:         f.Key,
			Title:       f.Title,
			Description: f.Description,
			Kind:        apitypes.AccessFieldKind(f.Kind),
			Required:    f.Required,
			Options:     nonNilStrings(slices.Clone(f.Options)),
			Choices:     f.Choices,
			Danger:      f.Danger,
		})
	}
	return out
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// integrationPage is the integration's page, as the request r reached
// Kivali: the redirect address for browser sign-in is r's
// (signInRedirectURL).
func (s *Server) integrationPage(v *integrationView, r *http.Request) apitypes.IntegrationPage {
	names := s.agentNames()
	registeredAt := ""
	if c := v.conn; c != nil {
		registeredAt = c.registeredAt
	}
	page := apitypes.IntegrationPage{
		Summary:          v.summary(),
		Hosts:            integrationHosts(v.manifest),
		Packages:         integrationPackages(v.manifest),
		Actions:          integrationActions(v.manifest),
		ConnectionFields: connectionFields(v.manifest, v.conn),
		SignInClient:     signInClientWire(v.manifest),
		Scopes:           signInScopes(v.manifest),
		SignInHosts:      signInHosts(v.manifest),
		SignInAt:         signInPagesOf(v.manifest, registeredAt),
		Setup:            setupWire(v.manifest),
		ConnectionTest:   v.connectionTest(),
		AccessForm:       accessFormWire(v.manifest),
		Access:           []apitypes.AccessRow{},
		AgentsWithout:    []apitypes.PersonRef{},
		Versions:         []apitypes.IntegrationVersionSummary{},
		Running:          v.running,
		Flow:             v.flow,
		CanDisconnect:    v.conn != nil && !v.staleBuiltin(),
		CanUninstall:     !v.it.Builtin || v.staleBuiltin(),
	}
	// The page asks the integration afresh: it is where the rows are
	// shown and fixed, and read seldom.
	page.Requirements, _ = s.requirements(r.Context(), v, true)
	if page.Summary.State == apitypes.IntegrationStateConnected {
		page.Summary.Needs = unmet(page.Requirements)
	}
	if v.manifest != nil {
		page.Author = v.manifest.Author.Name
		page.ConnectionMethod = v.manifest.Kivali.Connection.Method
		page.SetupNotesMD = s.setupNotes(v.manifest, r)
	}
	redirect := s.browserRedirectURL(v.manifest, r)
	page.SignInRedirectURL = redirect
	if v.conn != nil {
		state := v.connectionState(v.conn)
		if si := state.SignIn; si != nil {
			si.Ways = s.signInWaysHere(v.manifest, r)
			si.RedirectURL = redirect
			if len(si.Ways) == 0 && offersBrowser(v.manifest) {
				si.BrowserElsewhere = true
				si.OpenAt = s.externalOrigin()
			}
		}
		page.Connection = &state
	}
	has := map[string]bool{}
	for _, a := range v.access {
		has[a.Slug] = true
		page.Access = append(page.Access, apitypes.AccessRow{
			Agent:   personNamed(names, a.Slug),
			Access:  a.AccessJSON(),
			Summary: v.accessSummary(a.AccessJSON()),
			SetAt:   a.SetAt,
			Invalid: optString(a.Invalid),
		})
	}
	sort.SliceStable(page.Access, func(i, j int) bool { return page.Access[i].Agent.Name < page.Access[j].Agent.Name })
	active, _ := s.Store.ListActiveAgents()
	for _, a := range active {
		if a.Slug == agent.CEOSlug || has[a.Slug] {
			continue
		}
		page.AgentsWithout = append(page.AgentsWithout, personNamed(names, a.Slug))
	}
	sort.SliceStable(page.AgentsWithout, func(i, j int) bool { return page.AgentsWithout[i].Name < page.AgentsWithout[j].Name })
	for _, ver := range v.versions {
		page.Versions = append(page.Versions, apitypes.IntegrationVersionSummary{
			Version:     ver.Version,
			State:       apitypes.IntegrationVersionState(ver.State),
			PublishedAt: ver.PublishedAt,
			PublishedBy: publisherNamed(names, v.it, ver.PublishedBy),
			ApprovedAt:  ver.ApprovedAt,
			NotesMD:     ver.NotesMD,
		})
	}
	return page
}

// publisherNamed is who published a version of it: Kivali for a
// built-in one's, else the agent or the owner.
func publisherNamed(names map[string]string, it store.Integration, slug string) apitypes.PersonRef {
	if it.Builtin && slug == store.BuiltinIntegrationPublisher {
		return apitypes.PersonRef{Slug: slug, Name: "Kivali"}
	}
	return personNamed(names, slug)
}

// integrationVersionView is a version's install screen, as the request
// r reached Kivali (the setup notes name r's redirect address).
func (s *Server) integrationVersionView(v *integrationView, ver store.IntegrationVersion, m *integration.Manifest, r *http.Request) apitypes.IntegrationVersion {
	out := apitypes.IntegrationVersion{
		Name:             v.it.Name,
		DisplayName:      cmp.Or(m.DisplayName, v.it.Name),
		Description:      m.Description,
		Version:          ver.Version,
		State:            apitypes.IntegrationVersionState(ver.State),
		Author:           m.Author.Name,
		PublishedAt:      ver.PublishedAt,
		PublishedBy:      publisherNamed(s.agentNames(), v.it, ver.PublishedBy),
		ApprovedAt:       ver.ApprovedAt,
		NotesMD:          ver.NotesMD,
		Runtime:          m.Server.Type,
		ConnectionMethod: m.Kivali.Connection.Method,
		SignInClient:     signInClientWire(m),
		Scopes:           signInScopes(m),
		SignInHosts:      signInHosts(m),
		SetupNotesMD:     s.setupNotes(m, r),
		Setup:            setupWire(m),
		Hosts:            integrationHosts(m),
		Packages:         integrationPackages(m),
		ConnectionFields: connectionFields(m, v.conn),
		Actions:          integrationActions(m),
		AccessForm:       accessFormWire(m),
		BundleBytes:      ver.BundleBytes,
		FileCount:        ver.FileCount,
		Current:          v.it.CurrentVersion == ver.Version,
		Builtin:          v.it.Builtin,
		Changes:          []string{},
		Findings:         nonNilStrings(slices.Clone(ver.Findings)),
	}
	if cur := v.it.CurrentVersion; cur != "" && cur != ver.Version {
		out.CurrentVersion = &cur
		// v.manifest is the version in use's while one is approved.
		out.Changes = nonNilStrings(integration.Diff(v.manifest, m))
	}
	return out
}

// integrationOr404 loads the integration {name} names, answering 404
// for one not installed and 500 for one that cannot be read.
func (s *Server) integrationOr404(w http.ResponseWriter, r *http.Request) (*integrationView, bool) {
	name := r.PathValue("name")
	v, err := s.loadIntegration(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "no integration called "+name+" is installed", whoLink)
		return nil, false
	case err != nil:
		log.Printf("integration %s: %v", name, err)
		writeAPIError(w, http.StatusInternalServerError, "the integration could not be read", whoServer)
		return nil, false
	}
	return v, true
}

// writeIntegrationPage answers with the page as it now stands.
func (s *Server) writeIntegrationPage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.integrationPage(v, r))
}

// ---- the list and upload ----

func (s *Server) handleAPIIntegrations(w http.ResponseWriter, r *http.Request) {
	its, err := s.Store.ListIntegrations()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the integrations could not be read", whoServer)
		return
	}
	listed, bad, err := s.listIntegrations(r.Context(), its)
	if err != nil {
		log.Printf("integration %s: %v", bad, err)
		writeAPIError(w, http.StatusInternalServerError, "the integration "+bad+" could not be read", whoServer)
		return
	}
	out := apitypes.Integrations{Integrations: []apitypes.IntegrationSummary{}}
	for _, l := range listed {
		out.Integrations = append(out.Integrations, l.sum)
	}
	sort.SliceStable(out.Integrations, func(i, j int) bool {
		return strings.ToLower(out.Integrations[i].DisplayName) < strings.ToLower(out.Integrations[j].DisplayName)
	})
	writeJSON(w, http.StatusOK, out)
}

// readUpload reads an uploaded file of at most max bytes.
func readUpload(fh *multipart.FileHeader, max int64) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("the bundle is over %d MB", max>>20)
	}
	return b, nil
}

// writeUploadRefused answers an upload whose bundle did not pass the
// check, with every finding.
func writeUploadRefused(w http.ResponseWriter, findings []string) {
	writeJSON(w, http.StatusBadRequest, apitypes.IntegrationUploadRefused{
		Error:    "the bundle did not pass the check",
		Who:      whoYou,
		Findings: nonNilStrings(findings),
	})
}

// handleAPIIntegrationUpload stores an uploaded bundle (multipart
// "file", a .zip) as a pending version, published by the owner, and
// answers 201 with its install screen.
func (s *Server) handleAPIIntegrationUpload(w http.ResponseWriter, r *http.Request) {
	if !parseAPIMultipart(w, r, integration.MaxBundleBytes+multipartOverhead, "the bundle and its form") {
		return
	}
	defer removeMultipartForm(r)
	files := formFiles(r, "file")
	if len(files) == 0 {
		writeAPIError(w, http.StatusBadRequest, "choose a bundle to upload: a .zip file", whoYou)
		return
	}
	data, err := readUpload(files[0], integration.MaxBundleBytes)
	if err != nil {
		writeUploadRefused(w, []string{err.Error()})
		return
	}
	b, err := integration.ReadBundle(data)
	if err != nil {
		writeUploadRefused(w, []string{err.Error()})
		return
	}
	findings := integration.Validate(b.Manifest, b.Files)
	if findings.HasErrors() {
		writeUploadRefused(w, findings.Texts())
		return
	}
	name, version := b.Manifest.Name, b.Manifest.Version
	err = s.Store.StoreIntegrationVersion(name, b, store.IntegrationVersion{
		PublishedBy: agent.CEOSlug,
		PublishedAt: s.clk().Now(),
		Findings:    findings.Warnings(),
	})
	switch {
	case errors.Is(err, store.ErrIntegrationBuiltin):
		writeAPIError(w, http.StatusConflict, builtinRefusal(s.integrationDisplay(name), builtinUpload), whoYou)
		return
	case errors.Is(err, store.ErrIntegrationVersionExists):
		writeAPIError(w, http.StatusConflict, fmt.Sprintf("version %s of %s is already stored; a changed bundle needs a new version number", version, name), whoBundle)
		return
	case err != nil:
		log.Printf("integration %s %s: store: %v", name, version, err)
		writeAPIError(w, http.StatusInternalServerError, "the bundle could not be stored", whoServer)
		return
	}
	v, err := s.loadIntegration(r.Context(), name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the integration could not be read", whoServer)
		return
	}
	ver, m, err := s.Store.GetIntegrationVersion(name, version)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the version could not be read", whoServer)
		return
	}
	writeJSON(w, http.StatusCreated, s.integrationVersionView(v, ver, m, r))
}

// ---- one integration ----

func (s *Server) handleAPIIntegration(w http.ResponseWriter, r *http.Request) {
	s.writeIntegrationPage(w, r)
}

func (s *Server) handleAPIIntegrationToggle(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Under the lock a connection's save takes, so a save racing the
		// switch registers or drops the proxy credential by the switch as
		// it ends up.
		defer s.lockIntegrations()()
		v, ok := s.integrationOr404(w, r)
		if !ok {
			return
		}
		if err := s.Store.SetIntegrationEnabled(r.PathValue("name"), on); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the integration's switch could not be saved", whoServer)
			return
		}
		// A sign-in under way ends with the integration disabled.
		if !on {
			s.setupCancel(v.it.Name)
		}
		// A built-in's connection reaches its hosts only while it is on.
		if err := s.builtinEgressAfterSwitch(r.Context(), v.it.Name, v.builtin, on); err != nil {
			log.Printf("integration %s: the connection's proxy credential: %v", v.it.Name, err)
		}
		s.Integrations.Changed()
		s.writeIntegrationPage(w, r)
	}
}

// The audit's actions for the owner's Disconnect and Uninstall.
const (
	disconnectAction = "disconnect"
	uninstallAction  = "uninstall"
)

// errNotConnected refuses Disconnect of an integration with no
// connection.
func errNotConnected(display string) string {
	return display + " is not connected, so there is nothing to disconnect"
}

// errBuiltinUninstall refuses Uninstall of a built-in this binary
// carries: it is on the team as long as this version of Kivali is.
func errBuiltinUninstall(display string) string {
	return display + " ships with Kivali, so it cannot be uninstalled; disconnect it instead"
}

// handleAPIIntegrationDisconnect is Disconnect, for an integration with
// a connection, this version's or one an earlier version made: a
// built-in that undoes its installation on the service
// (builtinintegrations.Uninstaller), or a setup bundle whose server
// runs, does so first, and its error is a 502 that leaves everything as
// it was. Then its setup flow under way ends, the connection's Secret
// goes (a sign-in's tokens revoked with the service where it has a
// revocation endpoint), and every agent's access with the connection's
// record. The integration is then installed again, with an audit row
// disconnect, and the answer is its page. One with no connection, or
// not in this version of Kivali, is a 409.
func (s *Server) handleAPIIntegrationDisconnect(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	switch {
	case v.staleBuiltin():
		writeAPIError(w, http.StatusConflict, builtinRefusal(v.displayName(), builtinNotIncluded), whoYou)
		return
	case v.conn == nil:
		writeAPIError(w, http.StatusConflict, errNotConnected(v.displayName()), whoNoOne)
		return
	}
	if !s.serviceUndone(w, r, v, disconnectAction) {
		return
	}
	unlock := s.lockIntegrations()
	revokes, ok := s.disconnectLocked(w, r)
	unlock()
	// The vendor is asked to revoke a sign-in's tokens with the lock
	// released, so a slow vendor holds up no other integration write.
	s.revokeSignIns(r.Context(), revokes)
	if ok {
		s.writeIntegrationPage(w, r)
	}
}

// handleAPIIntegrationUninstall is Uninstall, for an uploaded
// integration in any state and for a built-in this binary does not
// carry; a built-in it carries is a 409, since it is disconnected
// instead. With a connection, the service's side is undone first, as
// Disconnect has it. Then its setup flow under way ends, each
// connection's Secret goes (a sign-in's tokens revoked with the service
// where it has a revocation endpoint), and it goes whole: every
// version, its audit and its log. 204 once done.
func (s *Server) handleAPIIntegrationUninstall(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if v.it.Builtin && !v.staleBuiltin() {
		writeAPIError(w, http.StatusConflict, errBuiltinUninstall(v.displayName()), whoNoOne)
		return
	}
	if !s.serviceUndone(w, r, v, uninstallAction) {
		return
	}
	unlock := s.lockIntegrations()
	revokes, ok := s.uninstallLocked(w, r)
	unlock()
	// The vendor is asked to revoke a sign-in's tokens with the lock
	// released, so a slow vendor holds up no other integration write.
	s.revokeSignIns(r.Context(), revokes)
	if ok {
		w.WriteHeader(http.StatusNoContent)
	}
}

// serviceUndone has v's installation on the service undone before
// Disconnect or Uninstall (action) forgets its connection: true once
// it is, or there was nothing to undo; false with the refusal
// answered. A setup bundle enabled whose server is not running cannot
// be asked, and is a 409 that waits for it (serviceUndoRefusal); one
// disabled leaves the service's side as it is. The service's side goes
// first, with no lock held: it may be slow, and a failure (a 502)
// changes nothing here.
func (s *Server) serviceUndone(w http.ResponseWriter, r *http.Request, v *integrationView, action string) bool {
	if words := s.serviceUndoRefusal(v, action); words != "" {
		writeAPIError(w, http.StatusConflict, words, whoYou)
		return false
	}
	if words := s.serviceUninstall(r.Context(), v, action); words != "" {
		writeAPIError(w, http.StatusBadGateway, words, v.whoBuilt())
		return false
	}
	return true
}

// uninstallConnection is name's connection id as Uninstall is given it:
// as a call reads it, or as it is stored when it needs a sign-in or the
// service did not answer its renewal; with the scrubber of its secrets.
func (s *Server) uninstallConnection(ctx context.Context, name, id string) (builtinintegrations.Connection, *strings.Replacer, error) {
	conn, _, scrub, err := s.builtinConnection(ctx, name, id)
	if errors.Is(err, errSignInNeeded) || errors.Is(err, errVendorUnavailable) {
		_, rec, values, secretValues, _, _, rerr := s.Integrations.readStoredConnection(ctx, name, id)
		conn, scrub, err = builtinintegrations.Connection{ID: rec.ID, Fields: values}, newScrubber(secretValues), rerr
	}
	return conn, scrub, err
}

// serviceUninstall undoes v's installation on the service: Uninstall
// of v's built-in when it implements builtinintegrations.Uninstaller,
// or a setup bundle's kivali/uninstall while its server runs
// (uninstallerOf), with the connection as a call reads it (as stored
// when it needs a sign-in), under integrationCallTimeout: the words of
// a failure, scrubbed of the connection's secrets and audited as
// action (disconnect or uninstall), or "" once it went through.
func (s *Server) serviceUninstall(ctx context.Context, v *integrationView, action string) string {
	u, ok := s.uninstallerOf(v)
	if !ok {
		return ""
	}
	name, display := v.it.Name, v.displayName()
	if c := v.conn; c != nil {
		start := s.clk().Now()
		conn, scrub, err := s.uninstallConnection(ctx, name, c.rec.ID)
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		if err != nil {
			log.Printf("integration %s: read connection %s to undo its installation: %v", name, c.rec.ID, err)
			words := "Kivali could not read " + display + "'s connection to undo its installation on the service; nothing was changed"
			s.auditAgentCall(name, start, agent.CEOSlug, action, false, words)
			return words
		}
		_, err = runBuiltin(ctx, s.clk(), integrationCallTimeout, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, u.Uninstall(ctx, conn)
		})
		if err != nil {
			said := err.Error()
			if errors.Is(err, errIntegrationTimeout) {
				said = fmt.Sprintf("it did not answer within %d seconds", int(integrationCallTimeout/time.Second))
			}
			words := display + " could not undo its installation: " + clipRunes(scrub.Replace(said), signInWordsRunes) + "; nothing was changed"
			s.auditAgentCall(name, start, agent.CEOSlug, action, false, words)
			return words
		}
	}
	return ""
}

// disconnectLocked is Disconnect's part under lockIntegrations, once
// the service's side is undone: the connection's Secret goes (its
// sign-in's tokens to revoke), then its record, its scoped proxy
// credential and every agent's access, and the integration is
// installed again. A Secret that cannot be deleted keeps the record, so
// the Secret is still named and Disconnect again finishes it, and the
// answer is a 500 saying the service's side was undone. Once the
// connection is gone, the Secrets of any other record and of the
// connections set aside go, best effort, and the owner's audit gains
// disconnect. True once done, false with an error answered; it returns
// the sign-in tokens to revoke.
func (s *Server) disconnectLocked(w http.ResponseWriter, r *http.Request) (signInRevocations, bool) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return nil, false
	}
	if v.conn == nil {
		// Disconnected meanwhile.
		writeAPIError(w, http.StatusConflict, errNotConnected(v.displayName()), whoNoOne)
		return nil, false
	}
	name, c := v.it.Name, v.conn
	start := s.clk().Now()
	s.setupCancel(name)
	var revokes signInRevocations
	failed := false
	if c.rec.SecretName != "" {
		tokens := s.revocableTokens(r.Context(), name, v.manifest, c.rec)
		// A connection an earlier version made holds that version's
		// sign-in, which this version's manifest knows nothing of: it is
		// revoked as an install taking it over would.
		var earlier map[string]string
		if v.connectionMisfit() {
			if data, err := s.Secrets.Get(r.Context(), c.rec.SecretName); err == nil {
				earlier = data
			}
		}
		if err := s.Secrets.Delete(r.Context(), c.rec.SecretName); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			log.Printf("integration %s: disconnect: delete Secret %s: %v", name, c.rec.SecretName, err)
			failed = true
		} else {
			revokes = revokes.add(name, v.manifest, c.rec, tokens)
			if earlier != nil {
				s.revokeReplacedSignIn(name, c.rec, earlier)
			}
		}
	}
	if !failed {
		// With its Secret gone the credential's secret is gone too.
		if err := s.dropBuiltinEgress(name, []store.IntegrationConnection{c.rec}); err != nil {
			log.Printf("integration %s: drop the connection's proxy credential: %v", name, err)
		}
		if _, _, err := s.Store.DeleteIntegrationConnectionByID(name, c.rec.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Printf("integration %s: disconnect: delete connection %s: %v", name, c.rec.ID, err)
			failed = true
		}
	}
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
	if failed {
		words := v.displayName() + "'s installation was undone on the service, but Kivali could not remove its connection; disconnect it again to finish"
		s.auditAgentCall(name, start, agent.CEOSlug, disconnectAction, false, words)
		writeAPIError(w, http.StatusInternalServerError, words, whoServer)
		return revokes, false
	}
	// Any record left beside the connection (a migration that could not
	// finish) and every connection set aside go too, their Secrets
	// first, best effort.
	if rest, err := s.Store.ListIntegrationConnections(name); err == nil {
		for _, other := range rest {
			if other.SecretName != "" {
				if err := s.Secrets.Delete(r.Context(), other.SecretName); err != nil && !errors.Is(err, secrets.ErrNotFound) {
					log.Printf("integration %s: disconnect: delete Secret %s: %v", name, other.SecretName, err)
				}
			}
			if err := s.dropBuiltinEgress(name, []store.IntegrationConnection{other}); err != nil {
				log.Printf("integration %s: drop the connection's proxy credential: %v", name, err)
			}
		}
	}
	s.forgetRetiredConnectionSecrets(r.Context(), name)
	if _, err := s.Store.DeleteIntegrationConnection(name); err != nil {
		log.Printf("integration %s: disconnect: %v", name, err)
	}
	s.auditAgentCall(name, start, agent.CEOSlug, disconnectAction, true, "")
	return revokes, true
}

// uninstallLocked is Uninstall's part under lockIntegrations: true
// once done, false with an error answered. It returns the sign-in
// tokens to revoke.
func (s *Server) uninstallLocked(w http.ResponseWriter, r *http.Request) (signInRevocations, bool) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return nil, false
	}
	name := v.it.Name
	s.setupCancel(name)
	revokes, ok := s.deleteConnectionSecrets(w, r, name)
	if !ok {
		return revokes, false
	}
	// A built-in's connections have proxy credentials of their own; a
	// bundle's none, and dropping nothing is no error.
	if err := s.dropAllBuiltinEgress(name); err != nil {
		log.Printf("integration %s: drop the connection's proxy credential: %v", name, err)
	}
	remove := s.Store.RemoveIntegration
	if v.staleBuiltin() {
		remove = s.Store.RemoveStaleBuiltinIntegration
	}
	if err := remove(name); err != nil {
		log.Printf("integration %s: uninstall: %v", name, err)
		writeAPIError(w, http.StatusInternalServerError, "the integration could not be uninstalled", whoServer)
		return revokes, false
	}
	s.Integrations.Changed()
	return revokes, true
}

// deleteConnectionSecrets deletes the Secret of every connection the
// integration has. On a failure it answers 500 and returns false, and
// the records stay, so the Secret is not left behind with nothing
// naming it. It returns the sign-in tokens of each Secret it deleted,
// for the caller to revoke with the vendor once it has released
// lockIntegrations (revokeSignIns), whatever else fails. Once they are
// gone, the Secrets of the connections set aside go too, best effort
// (forgetRetiredConnectionSecrets).
func (s *Server) deleteConnectionSecrets(w http.ResponseWriter, r *http.Request, name string) (signInRevocations, bool) {
	conns, err := s.Store.ListIntegrationConnections(name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be read", whoServer)
		return nil, false
	}
	// nil while no version is approved: nothing was signed in with.
	_, m, _ := s.Store.CurrentIntegrationVersion(name)
	var revokes signInRevocations
	for _, c := range conns {
		if c.SecretName == "" {
			continue
		}
		tokens := s.revocableTokens(r.Context(), name, m, c)
		if err := s.Secrets.Delete(r.Context(), c.SecretName); err != nil {
			log.Printf("integration %s: delete Secret %s: %v", name, c.SecretName, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be removed, so nothing was changed", whoServer)
			return revokes, false
		}
		revokes = revokes.add(name, m, c, tokens)
	}
	s.forgetRetiredConnectionSecrets(r.Context(), name)
	return revokes, true
}

// forgetRetiredConnectionSecrets deletes the Secret of every connection
// name's migration set aside (store.RetiredIntegrationConnections),
// best effort: Uninstall forgets every secret the integration holds. A
// failure is logged and does not stop the Uninstall, whose own
// connection's Secret is already gone. The caller holds
// lockIntegrations.
func (s *Server) forgetRetiredConnectionSecrets(ctx context.Context, name string) {
	retired, err := s.Store.RetiredIntegrationConnections(name)
	if err != nil {
		log.Printf("integration %s: uninstall: read the connections set aside: %v", name, err)
		return
	}
	for _, c := range retired {
		if c.SecretName == "" {
			continue
		}
		if err := s.Secrets.Delete(ctx, c.SecretName); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			log.Printf("integration %s: uninstall: delete Secret %s of a connection set aside: %v", name, c.SecretName, err)
		}
	}
}

// ---- versions ----

func (s *Server) handleAPIIntegrationVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	s.writeIntegrationVersionView(w, r, v)
}

func (s *Server) writeIntegrationVersionView(w http.ResponseWriter, r *http.Request, v *integrationView) {
	version := r.PathValue("version")
	ver, m, err := s.Store.GetIntegrationVersion(v.it.Name, version)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "version "+version+" of "+v.it.Name+" is not stored", whoLink)
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the version could not be read", whoServer)
		return
	}
	writeJSON(w, http.StatusOK, s.integrationVersionView(v, ver, m, r))
}

// handleAPIIntegrationAnswer approves or rejects a pending version from
// its install screen, answering the Needs-you request for it when there
// is one, and answers with the install screen as it now stands.
func (s *Server) handleAPIIntegrationAnswer(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.integrationOr404(w, r)
		if !ok {
			return
		}
		name, version := v.it.Name, r.PathValue("version")
		ver, _, err := s.Store.GetIntegrationVersion(name, version)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeAPIError(w, http.StatusNotFound, "version "+version+" of "+name+" is not stored", whoLink)
			return
		case err != nil:
			writeAPIError(w, http.StatusInternalServerError, "the version could not be read", whoServer)
			return
		case v.it.Builtin:
			move := builtinReject
			if approve {
				move = builtinApprove
			}
			writeAPIError(w, http.StatusConflict, builtinRefusal(v.displayName(), move), whoNoOne)
			return
		case ver.State != store.IntegrationVersionPending:
			writeAPIError(w, http.StatusConflict, fmt.Sprintf("version %s of %s is not waiting for approval: it is %s", version, name, ver.State), whoNoOne)
			return
		}
		if err := s.answerIntegrationInstall(r.Context(), name, version, approve); err != nil {
			var refusal *ceoRefusal
			if errors.As(err, &refusal) {
				writeNeedsError(w, err)
				return
			}
			writeAPIError(w, http.StatusConflict, sentenceCase(err.Error()), whoNoOne)
			return
		}
		v, ok = s.integrationOr404(w, r)
		if !ok {
			return
		}
		s.writeIntegrationVersionView(w, r, v)
	}
}

// answerIntegrationInstall approves or rejects a version. When a
// pending install request for it waits in Needs you, the answer goes
// through respondAsCEO, which applies it and writes the owner's
// response to the request, so Needs you clears and the publisher hears
// back. Otherwise, or when the agent that asked is no longer on the
// team and no answer can reach it, the version is approved or rejected
// alone.
func (s *Server) answerIntegrationInstall(ctx context.Context, name, version string, approve bool) error {
	if it, ok := s.pendingIntegrationInstall(name, version); ok {
		if _, err := s.Store.GetAgent(it.Request.From); err == nil {
			_, err := s.respondAsCEO(ctx, responseKindApproval, &approve, it.RequestPath, "", nil)
			return err
		}
	}
	if approve {
		return s.approveIntegrationVersion(name, version)
	}
	return s.rejectIntegrationVersion(name, version)
}

// pendingIntegrationInstall is the unanswered request in Needs you that
// asks to install name at version, if there is one.
func (s *Server) pendingIntegrationInstall(name, version string) (CEOInboxItem, bool) {
	view, err := s.buildCEOInbox(1)
	if err != nil {
		return CEOInboxItem{}, false
	}
	for _, it := range view.Needs {
		ii := it.Request.IntegrationInstall
		if it.RequestKind == "approval" && ii != nil && ii.Name == name && ii.Version == version {
			return it, true
		}
	}
	return CEOInboxItem{}, false
}

// acceptIntegrationInstall is an approval's effect on the version it
// names: a pending version is approved, and one already in use is what
// was asked for. Any other (rejected, superseded, no longer stored)
// cannot be approved.
func (s *Server) acceptIntegrationInstall(ii store.IntegrationInstall) error {
	ver, _, err := s.Store.GetIntegrationVersion(ii.Name, ii.Version)
	if err == nil && ver.State == store.IntegrationVersionApproved {
		return nil
	}
	return s.approveIntegrationVersion(ii.Name, ii.Version)
}

// integrationAnswerError words a store refusal to approve or reject a
// version.
func integrationAnswerError(name, version string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("version %s of %s is no longer stored", version, name)
	case errors.Is(err, store.ErrIntegrationVersionNotPending):
		return fmt.Errorf("version %s of %s has already been answered", version, name)
	}
	return err
}

// approveIntegrationVersion makes a pending version the one in use,
// and its pod follows. A built-in integration's is refused in
// builtinRefusal's words.
func (s *Server) approveIntegrationVersion(name, version string) error {
	if err := s.Store.ApproveIntegrationVersion(name, version, s.clk().Now()); err != nil {
		if errors.Is(err, store.ErrIntegrationBuiltin) {
			return errors.New(builtinRefusal(s.integrationDisplay(name), builtinApprove))
		}
		return integrationAnswerError(name, version, err)
	}
	s.Integrations.Changed()
	return nil
}

// rejectIntegrationVersion turns a pending version down. A built-in
// integration's is refused in builtinRefusal's words.
func (s *Server) rejectIntegrationVersion(name, version string) error {
	if err := s.Store.RejectIntegrationVersion(name, version, s.clk().Now()); err != nil {
		if errors.Is(err, store.ErrIntegrationBuiltin) {
			return errors.New(builtinRefusal(s.integrationDisplay(name), builtinReject))
		}
		return integrationAnswerError(name, version, err)
	}
	s.Integrations.Changed()
	return nil
}

// rollBackAction is the audit's action for the owner's roll back.
const rollBackAction = "rollback"

// rollBackRefusal says why version, in state, cannot be rolled back
// to: only a version a later one replaced can be.
func rollBackRefusal(name, version string, state store.IntegrationVersionState) string {
	switch state {
	case store.IntegrationVersionApproved:
		return fmt.Sprintf("version %s of %s is the version in use", version, name)
	case store.IntegrationVersionPending:
		return fmt.Sprintf("version %s of %s has never run; approve it from its install screen instead", version, name)
	case store.IntegrationVersionRejected:
		return fmt.Sprintf("version %s of %s was rejected and never runs", version, name)
	}
	return fmt.Sprintf("version %s of %s cannot be rolled back to: it is %s", version, name, state)
}

// handleAPIIntegrationRollBack makes a superseded version the one in
// use again: the version in use becomes superseded, the pod follows,
// and the audit has a row for the owner naming both versions. Every
// agent's access stays as it is and is checked against the version
// rolled back to when it is next read. Any other state is a 409.
func (s *Server) handleAPIIntegrationRollBack(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	name, version := v.it.Name, r.PathValue("version")
	ver, _, err := s.Store.GetIntegrationVersion(name, version)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "version "+version+" of "+name+" is not stored", whoLink)
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the version could not be read", whoServer)
		return
	case v.it.Builtin:
		writeAPIError(w, http.StatusConflict, builtinRefusal(v.displayName(), builtinRollBack), whoNoOne)
		return
	case ver.State != store.IntegrationVersionSuperseded:
		writeAPIError(w, http.StatusConflict, rollBackRefusal(name, version, ver.State), whoNoOne)
		return
	}
	now := s.clk().Now()
	replaced, err := s.Store.RollBackIntegrationVersion(name, version, now)
	switch {
	case errors.Is(err, store.ErrIntegrationVersionNotSuperseded):
		// Answered since it was read: another approval or roll back.
		writeAPIError(w, http.StatusConflict, fmt.Sprintf("version %s of %s is no longer one a later version replaced", version, name), whoNoOne)
		return
	case errors.Is(err, store.ErrIntegrationBuiltin):
		// Built in since it was read: a restore wrote it.
		writeAPIError(w, http.StatusConflict, builtinRefusal(s.integrationDisplay(name), builtinRollBack), whoNoOne)
		return
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "version "+version+" of "+name+" is not stored", whoLink)
		return
	case err != nil:
		log.Printf("integration %s: roll back to %s: %v", name, version, err)
		writeAPIError(w, http.StatusInternalServerError, "the roll back could not be saved", whoServer)
		return
	}
	s.Integrations.Changed()
	reason := "to version " + version
	if replaced != "" {
		reason = "from version " + replaced + " " + reason
	}
	if err := s.Store.AppendIntegrationAudit(name, store.IntegrationAuditRow{
		At: now, Agent: agent.CEOSlug, Action: rollBackAction, Allowed: true, Reason: reason,
	}); err != nil {
		log.Printf("integration %s: audit: %v", name, err)
	}
	v, ok = s.integrationOr404(w, r)
	if !ok {
		return
	}
	s.writeIntegrationVersionView(w, r, v)
}

// denyIntegrationInstall is a denial's effect on the version it names:
// a pending version is rejected; one no longer stored, or already
// rejected, has nothing left to turn down. A version in use cannot be
// denied.
func (s *Server) denyIntegrationInstall(ii store.IntegrationInstall) error {
	ver, _, err := s.Store.GetIntegrationVersion(ii.Name, ii.Version)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	case ver.State == store.IntegrationVersionRejected:
		return nil
	case ver.State != store.IntegrationVersionPending:
		return fmt.Errorf("version %s of %s is %s", ii.Version, ii.Name, ver.State)
	}
	return s.rejectIntegrationVersion(ii.Name, ii.Version)
}

// integrationWords names an integration's version for a sentence:
// "HubSpot 1.2.0".
func (s *Server) integrationWords(name, version string) string {
	display := name
	if _, m, err := s.Store.GetIntegrationVersion(name, version); err == nil && m.DisplayName != "" {
		display = m.DisplayName
	} else if it, err := s.Store.GetIntegration(name); err == nil && it.DisplayName != "" {
		display = it.DisplayName
	}
	return display + " " + version
}

// ---- access ----

// integrationAgent reads {slug} as an agent on the team that can be
// given access; it writes the error itself.
func (s *Server) integrationAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	slug := r.PathValue("slug")
	if !plainSlug(slug) {
		writeAgentNotFound(w, slug)
		return "", false
	}
	if slug == agent.CEOSlug {
		writeAPIError(w, http.StatusBadRequest, "access is given to agents; you need none", whoDevelopers)
		return "", false
	}
	if _, err := s.Store.GetAgent(slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAgentNotFound(w, slug)
			return "", false
		}
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoServer)
		return "", false
	}
	return slug, true
}

// handleAPIIntegrationAccessPut sets one agent's access from the
// filled access form, checked against the approved version's form. A
// row set aside with a retired connection is written again without
// that mark.
func (s *Server) handleAPIIntegrationAccessPut(w http.ResponseWriter, r *http.Request) {
	var req apitypes.AccessWrite
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	slug, ok := s.integrationAgent(w, r)
	if !ok {
		return
	}
	if v.it.CurrentVersion == "" {
		writeAPIError(w, http.StatusConflict, "approve a version of "+v.displayName()+" first", whoYou)
		return
	}
	if v.conn == nil {
		writeAPIError(w, http.StatusConflict, "connect "+v.displayName()+" first", whoYou)
		return
	}
	if err := integration.ValidateAccess(v.manifest, req.Access); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), whoYou)
		return
	}
	access := map[string]any{}
	if len(req.Access) > 0 && string(req.Access) != "null" {
		if err := json.Unmarshal(req.Access, &access); err != nil {
			writeAPIError(w, http.StatusBadRequest, "the access must be an object of field values", whoDevelopers)
			return
		}
	}
	// A built-in may hold the access to more than its form does.
	if c, ok := v.builtin.(builtinintegrations.AccessChecker); ok {
		if err := c.CheckAccess(access); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error(), whoYou)
			return
		}
	}
	err := s.Store.SetIntegrationAccess(v.it.Name, store.IntegrationAccess{
		Slug:   slug,
		Access: access,
		SetAt:  s.clk().Now(),
		SetBy:  agent.CEOSlug,
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAgentNotFound(w, slug)
		return
	case err != nil:
		log.Printf("integration %s: set %s's access: %v", v.it.Name, slug, err)
		writeAPIError(w, http.StatusInternalServerError, "the access could not be saved", whoServer)
		return
	}
	s.writeIntegrationPage(w, r)
}

// handleAPIIntegrationAccessDelete removes one agent's access.
func (s *Server) handleAPIIntegrationAccessDelete(w http.ResponseWriter, r *http.Request) {
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	slug := r.PathValue("slug")
	if !plainSlug(slug) {
		writeAgentNotFound(w, slug)
		return
	}
	if err := s.Store.ClearIntegrationAccess(v.it.Name, slug); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the access could not be removed", whoServer)
		return
	}
	s.writeIntegrationPage(w, r)
}

// handleAPIIntegrationChoices lists a field's options through the
// integration, which needs its runtime; a built-in one's Choices lists
// them through its connection, which it needs.
func (s *Server) handleAPIIntegrationChoices(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	form := integration.AccessForm(v.manifest)
	if form == nil || !slices.ContainsFunc(form.Fields, func(f integration.Field) bool { return f.Key == key && f.Choices }) {
		writeAPIError(w, http.StatusNotFound, v.displayName()+" lists no options for "+key, whoDevelopers)
		return
	}
	conn := v.conn
	switch {
	case v.staleBuiltin():
		writeAPIError(w, http.StatusConflict, builtinRefusal(v.displayName(), builtinNotIncluded), whoYou)
		return
	case !v.running:
		writeAPIError(w, http.StatusConflict, errNotRunning, v.whoBuilt())
		return
	case v.builtin != nil && conn == nil:
		writeAPIError(w, http.StatusConflict, "connect "+v.displayName()+" first", whoYou)
		return
	case v.several:
		writeAPIError(w, http.StatusConflict, v.displayName()+" holds more than one connection record and Kivali could not bring it to one; see the log", whoServer)
		return
	}
	connID := ""
	if conn != nil {
		connID = conn.rec.ID
	}
	listName := v.manifest.Kivali.Access.Choices[key]
	res, failed, err := s.callIntegrationTool(r.Context(), v, listName, func(ctx context.Context) (json.RawMessage, *strings.Replacer, error) {
		return s.builtinOwnerCall(ctx, v.builtin, v.it.Name, connID, false, listName)
	})
	if errors.Is(err, errIntegrationNotRunning) {
		writeAPIError(w, http.StatusConflict, errNotRunning, v.whoBuilt())
		return
	}
	if err != nil {
		return
	}
	if failed != "" {
		writeAPIError(w, http.StatusBadGateway, v.displayName()+" could not list the options: "+failed, v.whoBuilt())
		return
	}
	options, ok := parseChoices(res.firstText())
	if !ok {
		writeAPIError(w, http.StatusBadGateway, v.displayName()+"'s options are not a list of names", v.whoBuilt())
		return
	}
	writeJSON(w, http.StatusOK, apitypes.AccessChoices{Options: options})
}

// parseChoices reads a tool's text as the options of a field: a JSON
// array of strings, or an object whose options is one.
func parseChoices(text string) ([]string, bool) {
	var list []string
	if err := json.Unmarshal([]byte(text), &list); err == nil {
		return nonNilStrings(list), true
	}
	var obj struct {
		Options *[]string `json:"options"`
	}
	if err := json.Unmarshal([]byte(text), &obj); err == nil && obj.Options != nil {
		return nonNilStrings(*obj.Options), true
	}
	return nil, false
}

// ---- the log ----

const (
	integrationLogDefaultLimit = 200
	integrationLogMaxLimit     = 2000
)

// handleAPIIntegrationLogs is the integration's log, oldest first.
func (s *Server) handleAPIIntegrationLogs(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	limit := integrationLogDefaultLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("limit must be a number from 1 to %d", integrationLogMaxLimit), whoDevelopers)
			return
		}
		limit = min(n, integrationLogMaxLimit)
	}
	rows, err := s.Store.ReadIntegrationLogTail(v.it.Name, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the integration's log could not be read", whoServer)
		return
	}
	out := apitypes.IntegrationLogs{Lines: make([]apitypes.IntegrationLogLine, 0, len(rows))}
	for _, row := range rows {
		out.Lines = append(out.Lines, apitypes.IntegrationLogLine{At: row.At, Level: row.Level, Message: row.Message})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- audit ----

const (
	auditDefaultLimit = 100
	auditMaxLimit     = 1000
)

func (s *Server) handleAPIIntegrationAudit(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	limit := auditDefaultLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("limit must be a number from 1 to %d", auditMaxLimit), whoDevelopers)
			return
		}
		limit = min(n, auditMaxLimit)
	}
	rows, err := s.Store.ReadIntegrationAuditTail(v.it.Name, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the integration's audit could not be read", whoServer)
		return
	}
	names := s.agentNames()
	out := apitypes.IntegrationAudit{Rows: make([]apitypes.IntegrationAuditRow, 0, len(rows))}
	for _, row := range rows {
		out.Rows = append(out.Rows, apitypes.IntegrationAuditRow{
			At:         row.At,
			Agent:      personNamed(names, row.Agent),
			Action:     row.Action,
			Allowed:    row.Allowed,
			Reason:     row.Reason,
			DurationMS: row.DurationMS,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- an agent's tab ----

// handleAPIAgentIntegrations is the agent's Integrations tab: the
// integrations it has access to, with the access in words, and every
// other one installed. An agent no longer on the team is a 404.
func (s *Server) handleAPIAgentIntegrations(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.integrationAgent(w, r)
	if !ok {
		return
	}
	its, err := s.Store.ListIntegrations()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the integrations could not be read", whoServer)
		return
	}
	out := apitypes.AgentIntegrations{Connected: []apitypes.AgentIntegration{}, Available: []apitypes.AgentIntegration{}}
	for _, it := range its {
		v, err := s.loadIntegration(r.Context(), it.Name)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the integration "+it.Name+" could not be read", whoServer)
			return
		}
		row := apitypes.AgentIntegration{Name: it.Name, DisplayName: v.displayName(), State: v.state()}
		i := slices.IndexFunc(v.access, func(a store.IntegrationAccess) bool { return a.Slug == slug })
		if i < 0 {
			out.Available = append(out.Available, row)
			continue
		}
		row.Summary = v.accessSummary(v.access[i].AccessJSON())
		row.Invalid = optString(v.access[i].Invalid)
		row.Usable = v.access[i].Invalid == "" && v.agentUsable()
		out.Connected = append(out.Connected, row)
	}
	byName := func(list []apitypes.AgentIntegration) {
		sort.SliceStable(list, func(i, j int) bool {
			return strings.ToLower(list[i].DisplayName) < strings.ToLower(list[j].DisplayName)
		})
	}
	byName(out.Connected)
	byName(out.Available)
	writeJSON(w, http.StatusOK, out)
}
