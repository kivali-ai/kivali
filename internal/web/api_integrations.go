package web

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
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
	mux.HandleFunc("DELETE /api/v1/org/integrations/{name}", s.handleAPIIntegrationRemove)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/enable", s.handleAPIIntegrationToggle(true))
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/disable", s.handleAPIIntegrationToggle(false))
	mux.HandleFunc("GET /api/v1/org/integrations/{name}/versions/{version}", s.handleAPIIntegrationVersion)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/versions/{version}/approve", s.handleAPIIntegrationAnswer(true))
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/versions/{version}/reject", s.handleAPIIntegrationAnswer(false))
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/versions/{version}/rollback", s.handleAPIIntegrationRollBack)
	mux.HandleFunc("PUT /api/v1/org/integrations/{name}/connection", s.handleAPIIntegrationConnectionPut)
	mux.HandleFunc("DELETE /api/v1/org/integrations/{name}/connection", s.handleAPIIntegrationConnectionDelete)
	mux.HandleFunc("POST /api/v1/org/integrations/{name}/connection/verify", s.handleAPIIntegrationVerify)
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
	conn     *store.IntegrationConnection
	// secretSet names the sensitive fields whose value is in the
	// connection's Secret; reconnect is true when the Secret, or a
	// value the record names, is not there.
	secretSet map[string]bool
	reconnect bool
	access    []store.IntegrationAccess
	// running is whether the integration's shim is connected.
	running bool
}

// loadIntegration reads one integration whole. A missing one is
// store.ErrNotFound.
func (s *Server) loadIntegration(ctx context.Context, name string) (*integrationView, error) {
	it, err := s.Store.GetIntegration(name)
	if err != nil {
		return nil, err
	}
	v := &integrationView{it: it, secretSet: map[string]bool{}, running: s.Integrations.Running(name)}
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
	switch c, err := s.Store.GetIntegrationConnection(name); {
	case err == nil:
		v.conn = &c
		s.readConnectionSecret(ctx, v)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if v.access, err = s.Store.ListIntegrationAccess(name); err != nil {
		return nil, err
	}
	return v, nil
}

// readConnectionSecret works out which secret fields of v's connection
// have their value in its Secret. The values are not kept.
func (s *Server) readConnectionSecret(ctx context.Context, v *integrationView) {
	if len(v.conn.SecretKeys) == 0 {
		return
	}
	data, err := s.Secrets.Get(ctx, v.conn.SecretName)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		v.reconnect = true
		return
	case err != nil:
		// Not knowing is not a missing Secret: the page shows the
		// fields as set rather than ask for them again.
		log.Printf("integration %s: read the connection's Secret: %v", v.it.Name, err)
		for _, k := range v.conn.SecretKeys {
			v.secretSet[k] = true
		}
		return
	}
	for _, k := range v.conn.SecretKeys {
		if data[k] == "" {
			v.reconnect = true
			continue
		}
		v.secretSet[k] = true
	}
}

func (v *integrationView) status() apitypes.IntegrationStatus {
	switch {
	case v.it.CurrentVersion == "":
		return apitypes.IntegrationStatusPending
	case !v.it.Enabled:
		return apitypes.IntegrationStatusOff
	case !v.it.Builtin && !v.running:
		// Connected or not is the running server's to show: Test
		// connection and agents' calls need it up.
		return apitypes.IntegrationStatusNotRunning
	case v.conn != nil && !v.reconnect:
		return apitypes.IntegrationStatusConnected
	}
	return apitypes.IntegrationStatusNotConnected
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
		Status:      v.status(),
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

// connectionFields is m's connection form, with what conn holds.
func connectionFields(m *integration.Manifest, v *integrationView) []apitypes.ConnectionField {
	out := []apitypes.ConnectionField{}
	if m == nil {
		return out
	}
	for _, f := range m.UserConfig {
		cf := apitypes.ConnectionField{
			Key:         f.Key,
			Title:       f.Title,
			Description: f.Description,
			Sensitive:   f.Sensitive,
			Required:    f.Required,
		}
		switch {
		case f.Sensitive:
			cf.Set = v.secretSet[f.Key]
		case v.conn != nil:
			if val, ok := v.conn.Fields[f.Key]; ok && val != "" {
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

func integrationHosts(m *integration.Manifest) []string {
	if m == nil {
		return []string{}
	}
	return nonNilStrings(slices.Clone(m.Kivali.Hosts))
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

// integrationPage is the integration's page.
func (s *Server) integrationPage(v *integrationView) apitypes.IntegrationPage {
	names := s.agentNames()
	page := apitypes.IntegrationPage{
		Summary:          v.summary(),
		Hosts:            integrationHosts(v.manifest),
		Actions:          integrationActions(v.manifest),
		ConnectionFields: connectionFields(v.manifest, v),
		AccessForm:       accessFormWire(v.manifest),
		Access:           []apitypes.AccessRow{},
		AgentsWithout:    []apitypes.PersonRef{},
		Versions:         []apitypes.IntegrationVersionSummary{},
		CanRemove:        !v.it.Builtin,
	}
	if v.manifest != nil {
		page.Author = v.manifest.Author.Name
	}
	if c := v.conn; c != nil {
		page.Connection = &apitypes.ConnectionState{
			ID:              c.ID,
			CreatedAt:       c.CreatedAt,
			VerifiedAt:      c.VerifiedAt,
			Verified:        c.Verified,
			VerifyMessage:   optString(c.VerifyMessage),
			ReconnectNeeded: v.reconnect,
		}
	}
	has := map[string]bool{}
	for _, a := range v.access {
		has[a.Slug] = true
		page.Access = append(page.Access, apitypes.AccessRow{
			Agent:   personNamed(names, a.Slug),
			Access:  a.AccessJSON(),
			Summary: integration.AccessSummary(v.manifest, a.AccessJSON()),
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
			PublishedBy: personNamed(names, ver.PublishedBy),
			ApprovedAt:  ver.ApprovedAt,
			NotesMD:     ver.NotesMD,
		})
	}
	return page
}

// integrationVersionView is a version's install screen.
func (s *Server) integrationVersionView(v *integrationView, ver store.IntegrationVersion, m *integration.Manifest) apitypes.IntegrationVersion {
	out := apitypes.IntegrationVersion{
		Name:             v.it.Name,
		DisplayName:      cmp.Or(m.DisplayName, v.it.Name),
		Description:      m.Description,
		Version:          ver.Version,
		State:            apitypes.IntegrationVersionState(ver.State),
		Author:           m.Author.Name,
		PublishedAt:      ver.PublishedAt,
		PublishedBy:      personNamed(s.agentNames(), ver.PublishedBy),
		ApprovedAt:       ver.ApprovedAt,
		NotesMD:          ver.NotesMD,
		Runtime:          m.Server.Type,
		ConnectionMethod: m.Kivali.Connection.Method,
		Hosts:            integrationHosts(m),
		ConnectionFields: connectionFields(m, v),
		Actions:          integrationActions(m),
		AccessForm:       accessFormWire(m),
		BundleBytes:      ver.BundleBytes,
		FileCount:        ver.FileCount,
		Current:          v.it.CurrentVersion == ver.Version,
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
	writeJSON(w, http.StatusOK, s.integrationPage(v))
}

// ---- the list and upload ----

func (s *Server) handleAPIIntegrations(w http.ResponseWriter, r *http.Request) {
	its, err := s.Store.ListIntegrations()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the integrations could not be read", whoServer)
		return
	}
	out := apitypes.Integrations{Integrations: []apitypes.IntegrationSummary{}}
	for _, it := range its {
		v, err := s.loadIntegration(r.Context(), it.Name)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the integration "+it.Name+" could not be read", whoServer)
			return
		}
		out.Integrations = append(out.Integrations, v.summary())
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
		writeAPIError(w, http.StatusConflict, name+" ships with Kivali, so a bundle cannot replace it", whoYou)
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
	writeJSON(w, http.StatusCreated, s.integrationVersionView(v, ver, m))
}

// ---- one integration ----

func (s *Server) handleAPIIntegration(w http.ResponseWriter, r *http.Request) {
	s.writeIntegrationPage(w, r)
}

func (s *Server) handleAPIIntegrationToggle(on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.integrationOr404(w, r); !ok {
			return
		}
		if err := s.Store.SetIntegrationEnabled(r.PathValue("name"), on); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the integration's switch could not be saved", whoServer)
			return
		}
		s.Integrations.Changed()
		s.writeIntegrationPage(w, r)
	}
}

// handleAPIIntegrationRemove removes an integration: its connections'
// Secrets first, then everything the store keeps. A built-in one is a
// 409.
func (s *Server) handleAPIIntegrationRemove(w http.ResponseWriter, r *http.Request) {
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if v.it.Builtin {
		writeAPIError(w, http.StatusConflict, v.displayName()+" ships with Kivali and cannot be removed; switch it off instead", whoYou)
		return
	}
	if !s.deleteConnectionSecrets(w, r, v.it.Name) {
		return
	}
	if err := s.Store.RemoveIntegration(v.it.Name); err != nil {
		log.Printf("integration %s: remove: %v", v.it.Name, err)
		writeAPIError(w, http.StatusInternalServerError, "the integration could not be removed", whoServer)
		return
	}
	s.Integrations.Changed()
	w.WriteHeader(http.StatusNoContent)
}

// deleteConnectionSecrets deletes the Secret of every connection the
// integration has. On a failure it answers 500 and returns false, and
// the records stay, so the Secret is not left behind with nothing
// naming it.
func (s *Server) deleteConnectionSecrets(w http.ResponseWriter, r *http.Request, name string) bool {
	conns, err := s.Store.ListIntegrationConnections(name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be read", whoServer)
		return false
	}
	for _, c := range conns {
		if c.SecretName == "" {
			continue
		}
		if err := s.Secrets.Delete(r.Context(), c.SecretName); err != nil {
			log.Printf("integration %s: delete Secret %s: %v", name, c.SecretName, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be removed, so nothing was changed", whoServer)
			return false
		}
	}
	return true
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
	writeJSON(w, http.StatusOK, s.integrationVersionView(v, ver, m))
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
// and its pod follows.
func (s *Server) approveIntegrationVersion(name, version string) error {
	if err := s.Store.ApproveIntegrationVersion(name, version, s.clk().Now()); err != nil {
		return integrationAnswerError(name, version, err)
	}
	s.Integrations.Changed()
	return nil
}

// rejectIntegrationVersion turns a pending version down.
func (s *Server) rejectIntegrationVersion(name, version string) error {
	if err := s.Store.RejectIntegrationVersion(name, version, s.clk().Now()); err != nil {
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

// ---- the connection ----

// newConnectionID is a fresh connection id: 8 hex digits.
func newConnectionID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// handleAPIIntegrationConnectionPut writes the connection from the
// approved version's connection form: the fields that are not
// sensitive to the store, the sensitive ones to the connection's
// Secret. A sensitive field left out keeps its value and an empty one
// clears it; a field that is not sensitive is what is sent. The
// connection's last Test connection result is cleared.
func (s *Server) handleAPIIntegrationConnectionPut(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ConnectionWrite
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	name := v.it.Name
	if v.it.CurrentVersion == "" {
		writeAPIError(w, http.StatusConflict, "approve a version of "+v.displayName()+" first; the connection follows the approved version's form", whoYou)
		return
	}
	m := v.manifest
	for _, k := range sortedFieldKeys(req.Fields) {
		if _, ok := m.UserConfig.Field(k); !ok {
			writeAPIError(w, http.StatusBadRequest, k+" is not a field of this integration's connection", whoDevelopers)
			return
		}
	}
	stored := map[string]string{}
	if v.conn != nil && v.conn.SecretName != "" {
		got, err := s.Secrets.Get(r.Context(), v.conn.SecretName)
		switch {
		case err == nil:
			stored = got
		case !errors.Is(err, secrets.ErrNotFound):
			log.Printf("integration %s: read the connection's Secret: %v", name, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be read, so nothing was changed", whoServer)
			return
		}
	}
	fields, secret := map[string]string{}, map[string]string{}
	for _, f := range m.UserConfig {
		val, sent := req.Fields[f.Key]
		val = strings.TrimSpace(val)
		switch {
		case f.Sensitive && sent:
			if val != "" {
				secret[f.Key] = val
			}
		case f.Sensitive:
			if old := stored[f.Key]; old != "" {
				secret[f.Key] = old
			}
		case val != "":
			fields[f.Key] = val
		}
		have := cmp.Or(fields[f.Key], secret[f.Key])
		if have == "" {
			if f.Required {
				writeAPIError(w, http.StatusBadRequest, "the connection needs "+f.Title, whoYou)
				return
			}
			continue
		}
		if msg := connectionValueProblem(f, have); msg != "" {
			writeAPIError(w, http.StatusBadRequest, msg, whoYou)
			return
		}
	}
	rec := store.IntegrationConnection{ID: newConnectionID(), CreatedAt: s.clk().Now(), Fields: fields}
	if v.conn != nil {
		rec.ID, rec.CreatedAt = v.conn.ID, v.conn.CreatedAt
	}
	secretName := secrets.ConnectionSecretName(name, rec.ID)
	if v.conn != nil && v.conn.SecretName != "" {
		secretName = v.conn.SecretName
	}
	if len(secret) > 0 {
		if err := s.Secrets.Put(r.Context(), secretName, secret); err != nil {
			log.Printf("integration %s: write the connection's Secret: %v", name, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be saved, so nothing was changed", whoServer)
			return
		}
		rec.SecretName = secretName
		rec.SecretKeys = sortedFieldKeys(secret)
	} else if v.conn != nil && v.conn.SecretName != "" {
		if err := s.Secrets.Delete(r.Context(), v.conn.SecretName); err != nil {
			log.Printf("integration %s: delete the connection's Secret: %v", name, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be cleared, so nothing was changed", whoServer)
			return
		}
	}
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: write the connection: %v", name, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be saved", whoServer)
		return
	}
	// The running server starts again with the new values.
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
	s.writeIntegrationPage(w, r)
}

// connectionValueProblem is what is wrong with a connection field's
// value for its type, or "". It never quotes the value, which may be a
// secret.
func connectionValueProblem(f integration.UserConfigField, val string) string {
	switch f.Type {
	case "number":
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			return f.Title + " must be a number"
		}
	case "boolean":
		if val != "true" && val != "false" {
			return f.Title + " must be true or false"
		}
	}
	return ""
}

func sortedFieldKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// handleAPIIntegrationConnectionDelete removes the connection: its
// Secret, its record and every agent's access, which was set for it.
func (s *Server) handleAPIIntegrationConnectionDelete(w http.ResponseWriter, r *http.Request) {
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if !s.deleteConnectionSecrets(w, r, v.it.Name) {
		return
	}
	if _, err := s.Store.DeleteIntegrationConnection(v.it.Name); err != nil {
		log.Printf("integration %s: delete the connection: %v", v.it.Name, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be removed", whoServer)
		return
	}
	// The running server starts again without the values.
	s.Integrations.ConnectionChanged(v.it.Name)
	s.Integrations.Changed()
	s.writeIntegrationPage(w, r)
}

// integrationVerifyMessageMax bounds the integration's words kept from
// Test connection.
const integrationVerifyMessageMax = 1000

// toolResult is the part of an MCP tools/call result Kivali reads.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// firstText is the result's first text content.
func (t toolResult) firstText() string {
	for _, c := range t.Content {
		if c.Type == "text" {
			return c.Text
		}
	}
	return ""
}

// callIntegrationTool calls one of the integration's tools for the
// owner (no agent), writes the audit row, and returns the result.
// failed is the integration's words when the call did not go through:
// the server's error, its tool's error result, or the deadline.
// errIntegrationNotRunning and a caller that went away are returned as
// err, with no audit row.
func (s *Server) callIntegrationTool(r *http.Request, name, tool string) (res toolResult, failed string, err error) {
	start := s.clk().Now()
	raw, callErr := s.Integrations.Call(r.Context(), name, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
	var rpcErr *integrationpod.RPCError
	switch {
	case errors.Is(callErr, errIntegrationNotRunning), r.Context().Err() != nil:
		return res, "", cmp.Or(callErr, r.Context().Err())
	case errors.Is(callErr, errIntegrationTimeout):
		failed = fmt.Sprintf("the integration did not answer within %d seconds", int(integrationCallTimeout/time.Second))
	case errors.As(callErr, &rpcErr):
		failed = rpcErr.Message
	case callErr != nil:
		failed = callErr.Error()
	default:
		if json.Unmarshal(raw, &res) != nil {
			failed = "the integration's answer is not one Kivali reads"
		} else if res.IsError {
			failed = cmp.Or(res.firstText(), "the integration reported an error")
		}
	}
	failed = clipText(failed, integrationVerifyMessageMax)
	// The integration's words may quote a connection secret, as an
	// agent's call's answer may: scrubbed before they are kept, shown
	// or audited. With no scrubber (the secrets could not be read) the
	// words are held back the way an agent's answer is.
	if scrub := s.Integrations.scrubber(r.Context(), name); scrub != nil {
		failed = scrub.Replace(failed)
		for i := range res.Content {
			res.Content[i].Text = scrub.Replace(res.Content[i].Text)
		}
	} else {
		res = toolResult{}
		failed = "Kivali could not check the integration's answer for secrets, so it held the answer back"
	}
	row := store.IntegrationAuditRow{
		At:         start,
		Agent:      agent.CEOSlug,
		Action:     tool,
		Allowed:    true,
		Reason:     failed,
		DurationMS: s.clk().Now().Sub(start).Milliseconds(),
	}
	if err := s.Store.AppendIntegrationAudit(name, row); err != nil {
		log.Printf("integration %s: audit: %v", name, err)
	}
	return res, failed, nil
}

// handleAPIIntegrationVerify is Test connection: it calls the
// integration's verify action through the running server and keeps
// what it found on the connection.
func (s *Server) handleAPIIntegrationVerify(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	name := v.it.Name
	switch {
	case v.it.CurrentVersion == "":
		writeAPIError(w, http.StatusConflict, "approve a version of "+v.displayName()+" first", whoYou)
		return
	case v.conn == nil:
		writeAPIError(w, http.StatusConflict, "connect the integration first", whoYou)
		return
	case v.manifest.Kivali.Verify == "":
		writeAPIError(w, http.StatusConflict, v.displayName()+" has no connection test", whoBundle)
		return
	case !v.running:
		writeAPIError(w, http.StatusConflict, errNotRunning, whoBundle)
		return
	}
	connID := v.conn.ID
	res, failed, err := s.callIntegrationTool(r, name, v.manifest.Kivali.Verify)
	if errors.Is(err, errIntegrationNotRunning) {
		writeAPIError(w, http.StatusConflict, errNotRunning, whoBundle)
		return
	}
	if err != nil {
		return
	}
	verified, message := failed == "", failed
	if verified {
		message = clipText(res.firstText(), integrationVerifyMessageMax)
	}
	unlock := s.lockIntegrations()
	conn, err := s.Store.GetIntegrationConnection(name)
	if err == nil && conn.ID == connID {
		at := s.clk().Now().UTC()
		conn.VerifiedAt, conn.Verified, conn.VerifyMessage = &at, &verified, message
		err = s.Store.PutIntegrationConnection(name, conn)
	}
	unlock()
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("integration %s: keep the connection test: %v", name, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection test's result could not be saved", whoServer)
		return
	}
	s.writeIntegrationPage(w, r)
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
// filled access form, checked against the approved version's form.
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
		writeAPIError(w, http.StatusConflict, "connect the integration first", whoYou)
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
	err := s.Store.SetIntegrationAccess(v.it.Name, store.IntegrationAccess{
		Slug:         slug,
		ConnectionID: v.conn.ID,
		Access:       access,
		SetAt:        s.clk().Now(),
		SetBy:        agent.CEOSlug,
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
// integration, which needs its runtime.
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
	if !v.running {
		writeAPIError(w, http.StatusConflict, errNotRunning, whoBundle)
		return
	}
	res, failed, err := s.callIntegrationTool(r, v.it.Name, v.manifest.Kivali.Access.Choices[key])
	if errors.Is(err, errIntegrationNotRunning) {
		writeAPIError(w, http.StatusConflict, errNotRunning, whoBundle)
		return
	}
	if err != nil {
		return
	}
	if failed != "" {
		writeAPIError(w, http.StatusBadGateway, v.displayName()+" could not list the options: "+failed, whoBundle)
		return
	}
	options, ok := parseChoices(res.firstText())
	if !ok {
		writeAPIError(w, http.StatusBadGateway, v.displayName()+"'s options are not a list of names", whoBundle)
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
		row := apitypes.AgentIntegration{Name: it.Name, DisplayName: v.displayName(), Status: v.status()}
		i := slices.IndexFunc(v.access, func(a store.IntegrationAccess) bool { return a.Slug == slug })
		if i < 0 {
			out.Available = append(out.Available, row)
			continue
		}
		row.Summary = integration.AccessSummary(v.manifest, v.access[i].AccessJSON())
		row.Invalid = optString(v.access[i].Invalid)
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
