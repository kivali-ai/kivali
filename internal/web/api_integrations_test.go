package web

import (
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

const intBase = "/api/v1/org/integrations"

// intBundle is the integrationtest fixture's bundle for name at version.
func intBundle(t *testing.T, name, version string) []byte {
	t.Helper()
	return integrationtest.Zip(t, "", integrationtest.Manifest(name, version), nil)
}

func intUpload(t *testing.T, srv *Server, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := orgMultipart(t, []multipartFile{{field: "file", name: "bundle.zip", data: data}}, nil)
	return orgDo(t, srv, "", http.MethodPost, intBase, ct, body)
}

func intPost(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return orgDo(t, srv, "", http.MethodPost, path, "", nil)
}

func intDelete(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return orgDo(t, srv, "", http.MethodDelete, path, "", nil)
}

// intInstalled uploads name at version and approves it from the
// install screen.
func intInstalled(t *testing.T, srv *Server, name, version string) {
	t.Helper()
	if rr := intUpload(t, srv, intBundle(t, name, version)); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	if rr := intPost(t, srv, intBase+"/"+name+"/versions/"+version+"/approve"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
}

// intConnected is name installed and connected with a token.
func intConnected(t *testing.T, srv *Server, name string) {
	t.Helper()
	intInstalled(t, srv, name, "1.0.0")
	rr := orgJSON(t, srv, http.MethodPut, intBase+"/"+name+"/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "s3cret-" + name, "portal": "acme"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rr.Code, rr.Body.String())
	}
}

func intTeam(t *testing.T, srv *Server, slugs ...string) {
	t.Helper()
	_ = srv.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	for _, s := range slugs {
		if err := srv.Store.CreateAgent(store.Agent{Slug: s, Role: strings.ToUpper(s[:1]) + s[1:], ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// An uploaded bundle that passes the check is stored as a pending
// version published by the owner, and the install screen says in words
// everything approving would let in: hosts, the connection's secrets,
// the actions, the access form, the bundle's size and the check's
// warnings. A field missing here is something the owner approves
// without seeing.
func TestAPIIntegrationUploadIsAPendingVersion(t *testing.T) {
	srv := newTestServer(t)
	m := integrationtest.Manifest("crm", "1.0.0")
	delete(m["tools"].([]any)[2].(map[string]any), "description")
	rr := intUpload(t, srv, integrationtest.Zip(t, "crm", m, nil))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	v := decodeAPI[apitypes.IntegrationVersion](t, rr)
	if v.Name != "crm" || v.DisplayName != "Test crm" || v.Version != "1.0.0" || v.State != apitypes.IntegrationVersionStatePending ||
		v.PublishedBy != (apitypes.PersonRef{Slug: agent.CEOSlug, Name: "You"}) || v.Runtime != "python" || v.ConnectionMethod != "token" ||
		v.Author != "Kivali Developer (agent)" || v.Current || v.CurrentVersion != nil || v.FileCount != 2 || v.BundleBytes == 0 {
		t.Errorf("version = %+v", v)
	}
	if strings.Join(v.Hosts, ",") != "api.example.com" || len(v.Actions) != 3 || v.AccessForm == nil || len(v.AccessForm.Fields) != 3 {
		t.Errorf("hosts %v actions %v form %+v", v.Hosts, v.Actions, v.AccessForm)
	}
	if len(v.Findings) != 1 || v.Findings[0] != "tools[2] (whoami) has no description" {
		t.Errorf("findings = %v", v.Findings)
	}
	var token *apitypes.ConnectionField
	for i := range v.ConnectionFields {
		if v.ConnectionFields[i].Key == "token" {
			token = &v.ConnectionFields[i]
		}
	}
	if token == nil || !token.Sensitive || !token.Required || token.Set || token.Title != "Private app token" {
		t.Errorf("token field = %+v", token)
	}

	list := orgOK[apitypes.Integrations](t, orgGet(t, srv, intBase))
	if len(list.Integrations) != 1 {
		t.Fatalf("list = %+v", list)
	}
	sum := list.Integrations[0]
	if sum.State != apitypes.IntegrationStatePending || sum.Needs != 0 || sum.Version != "" || sum.PendingVersion == nil || *sum.PendingVersion != "1.0.0" || !sum.Enabled || sum.Description == "" {
		t.Errorf("summary = %+v", sum)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if len(page.Versions) != 1 || page.Versions[0].State != apitypes.IntegrationVersionStatePending || page.Connection != nil || !page.CanUninstall || len(page.Requirements) != 0 || page.Running {
		t.Errorf("page = %+v", page)
	}
}

// What the upload refuses, and how: a body that is not a form, a form
// without a file, a file that is not a bundle, a bundle that fails the
// check (every finding listed), a version already stored and a
// built-in integration. A refusal in the wrong shape would leave the
// upload dialog with nothing to show.
func TestAPIIntegrationUploadRefusals(t *testing.T) {
	srv := newTestServer(t)
	if rr := orgJSON(t, srv, http.MethodPost, intBase, map[string]string{}); rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("json body: %d", rr.Code)
	}
	body, ct := orgMultipart(t, nil, map[string]string{"x": "y"})
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, intBase, ct, body), http.StatusBadRequest)

	refused := func(rr *httptest.ResponseRecorder) apitypes.IntegrationUploadRefused {
		t.Helper()
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, body %s", rr.Code, rr.Body.String())
		}
		r := decodeAPI[apitypes.IntegrationUploadRefused](t, rr)
		if r.Error != "the bundle did not pass the check" || r.Who != "you" || len(r.Findings) == 0 {
			t.Errorf("refusal = %+v", r)
		}
		return r
	}
	if r := refused(intUpload(t, srv, []byte("not a zip"))); r.Findings[0] != "the bundle is not a zip file" {
		t.Errorf("findings = %v", r.Findings)
	}
	m := integrationtest.Manifest("crm", "1.0.0")
	m["server"].(map[string]any)["type"] = "node"
	m["kivali"].(map[string]any)["hosts"] = []any{}
	r := refused(intUpload(t, srv, integrationtest.Zip(t, "", m, nil)))
	if strings.Join(r.Findings, "|") != "server.type: Kivali runs Python integrations; server.type must be python|kivali.hosts is empty" {
		t.Errorf("findings = %v", r.Findings)
	}
	if its, _ := srv.Store.ListIntegrations(); len(its) != 0 {
		t.Errorf("a refused bundle was stored: %v", its)
	}

	if rr := intUpload(t, srv, intBundle(t, "crm", "1.0.0")); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d", rr.Code)
	}
	e := assertAPIError(t, intUpload(t, srv, intBundle(t, "crm", "1.0.0")), http.StatusConflict)
	if !strings.Contains(e.Error, "version 1.0.0 of crm is already stored") {
		t.Errorf("duplicate = %+v", e)
	}
	writeBuiltinIntegration(t, srv, "maps")
	e = assertAPIError(t, intUpload(t, srv, intBundle(t, "maps", "1.0.0")), http.StatusConflict)
	if !strings.Contains(e.Error, "ships with Kivali") {
		t.Errorf("builtin = %+v", e)
	}
}

// writeBuiltinIntegration records a built-in integration as a Kivali
// that ships one leaves it: integration.yaml with builtin set. srv's
// binary carries no code for it.
func writeBuiltinIntegration(t *testing.T, srv *Server, name string) {
	t.Helper()
	dir := filepath.Join(srv.Store.Root(), "integrations", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "integration.yaml"), []byte("name: "+name+"\ndisplay_name: Maps\nenabled: true\nbuiltin: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Approving from the install screen makes the version current and
// answers with the screen as it now stands; a version already answered
// is a 409, so a second click cannot approve a version over a newer
// one; rejecting leaves the current version in use. Unknown names and
// versions are 404s.
func TestAPIIntegrationApproveAndReject(t *testing.T) {
	srv := newTestServer(t)
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	rr := intPost(t, srv, intBase+"/crm/versions/1.0.0/approve")
	v := orgOK[apitypes.IntegrationVersion](t, rr)
	if v.State != apitypes.IntegrationVersionStateApproved || !v.Current || v.ApprovedAt == nil {
		t.Errorf("approved = %+v", v)
	}
	assertAPIError(t, intPost(t, srv, intBase+"/crm/versions/1.0.0/approve"), http.StatusConflict)
	assertAPIError(t, intPost(t, srv, intBase+"/crm/versions/1.0.0/reject"), http.StatusConflict)

	intUpload(t, srv, intBundle(t, "crm", "1.1.0"))
	pending := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.1.0"))
	if pending.CurrentVersion == nil || *pending.CurrentVersion != "1.0.0" || pending.Current {
		t.Errorf("pending screen = %+v", pending)
	}
	rejected := orgOK[apitypes.IntegrationVersion](t, intPost(t, srv, intBase+"/crm/versions/1.1.0/reject"))
	if rejected.State != apitypes.IntegrationVersionStateRejected {
		t.Errorf("rejected = %+v", rejected)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	// Approved and on, with no connection and no shim: available, not running.
	if page.Summary.Version != "1.0.0" || page.Summary.State != apitypes.IntegrationStateAvailable || page.Running || page.Summary.PendingVersion != nil {
		t.Errorf("summary = %+v", page.Summary)
	}
	if len(page.Versions) != 2 || page.Versions[0].Version != "1.1.0" {
		t.Errorf("versions = %+v", page.Versions)
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/crm/versions/9.9.9"), http.StatusNotFound)
	assertAPIError(t, intPost(t, srv, intBase+"/crm/versions/9.9.9/approve"), http.StatusNotFound)
	assertAPIError(t, orgGet(t, srv, intBase+"/nope"), http.StatusNotFound)
	assertAPIError(t, orgGet(t, srv, intBase+"/nope/versions/1.0.0"), http.StatusNotFound)
}

// Why this test exists: software a version's pod installs runs as
// root at every start, so the owner must read it before approving.
// The install screen lists the packages the manifest declares and,
// beside the version in use, names the ones it adds; the page lists
// those of the version in use. A version without packages says so with
// an empty list, never null, which the web app would not draw.
func TestAPIIntegrationPackages(t *testing.T) {
	srv := newTestServer(t)
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	first := orgGet(t, srv, intBase+"/crm/versions/1.0.0")
	if !strings.Contains(first.Body.String(), `"packages":[]`) {
		t.Errorf("a version without packages: %s", first.Body.String())
	}
	intPost(t, srv, intBase+"/crm/versions/1.0.0/approve")

	m := integrationtest.Manifest("crm", "1.1.0")
	m["kivali"].(map[string]any)["packages"] = []any{"gh", "jq"}
	if rr := intUpload(t, srv, integrationtest.Zip(t, "", m, nil)); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	v := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.1.0"))
	if strings.Join(v.Packages, ",") != "gh,jq" || strings.Join(v.Changes, "|") != "Software added: gh, jq" {
		t.Errorf("1.1.0's packages %q, changes %q", v.Packages, v.Changes)
	}
	if page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm")); page.Packages == nil || len(page.Packages) != 0 {
		t.Errorf("the page before approval lists %#v", page.Packages)
	}
	intPost(t, srv, intBase+"/crm/versions/1.1.0/approve")
	if page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm")); strings.Join(page.Packages, ",") != "gh,jq" {
		t.Errorf("the page after approval lists %q", page.Packages)
	}
}

// crmVersion2 is the fixture's crm at 2.0.0, reaching one more host
// and with Region's options changed from eu and us to us and apac, so
// access set for eu does not fit it.
func crmVersion2(t *testing.T) []byte {
	t.Helper()
	m := integrationtest.Manifest("crm", "2.0.0")
	k := m["kivali"].(map[string]any)
	k["hosts"] = []any{"api.example.com", "uploads.example.com"}
	k["access"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["region"] = map[string]any{"type": "string", "enum": []any{"us", "apac"}, "title": "Region"}
	return integrationtest.Zip(t, "", m, nil)
}

// Why this test exists: rolling back is how the owner undoes an
// approval, and it must run the version rolled back to and nothing
// else. A superseded version becomes the one in use, the one in use is
// superseded, the answer is that version's install screen, and the
// audit names both versions for the owner. Every other state is a
// 409 that says why, so a stale click cannot run a version the owner
// never approved, or one the owner turned down. The install screen of
// a version that is not in use says what it changes from the one that
// is, so the owner reads a new host before approving it or rolling
// back to it.
func TestAPIIntegrationRollBack(t *testing.T) {
	srv := newTestServer(t)
	intConnected(t, srv, "crm")
	first := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.0.0"))
	if first.Changes == nil || len(first.Changes) != 0 {
		t.Errorf("the version in use lists changes: %#v", first.Changes)
	}
	if rr := intUpload(t, srv, crmVersion2(t)); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	pending := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/2.0.0"))
	if strings.Join(pending.Changes, "|") != "Hosts added: uploads.example.com" {
		t.Errorf("2.0.0's changes from 1.0.0 = %q", pending.Changes)
	}
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/versions/2.0.0/rollback"), http.StatusConflict); e.Error != "version 2.0.0 of crm has never run; approve it from its install screen instead" {
		t.Errorf("roll back to a pending version = %+v", e)
	}
	intPost(t, srv, intBase+"/crm/versions/2.0.0/approve")
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/versions/2.0.0/rollback"), http.StatusConflict); e.Error != "version 2.0.0 of crm is the version in use" {
		t.Errorf("roll back to the version in use = %+v", e)
	}
	old := orgOK[apitypes.IntegrationVersion](t, orgGet(t, srv, intBase+"/crm/versions/1.0.0"))
	if old.State != apitypes.IntegrationVersionStateSuperseded || old.CurrentVersion == nil || *old.CurrentVersion != "2.0.0" || strings.Join(old.Changes, "|") != "Hosts removed: uploads.example.com" {
		t.Errorf("1.0.0 under 2.0.0 = %+v", old)
	}

	rolled := orgOK[apitypes.IntegrationVersion](t, intPost(t, srv, intBase+"/crm/versions/1.0.0/rollback"))
	if rolled.State != apitypes.IntegrationVersionStateApproved || !rolled.Current || rolled.CurrentVersion != nil || rolled.Changes == nil || len(rolled.Changes) != 0 {
		t.Errorf("rolled back = %+v", rolled)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if page.Summary.Version != "1.0.0" || strings.Join(page.Hosts, ",") != "api.example.com" {
		t.Errorf("page after the roll back = %+v hosts %v", page.Summary, page.Hosts)
	}
	var states []string
	for _, v := range page.Versions {
		states = append(states, v.Version+"="+string(v.State))
	}
	if strings.Join(states, " ") != "2.0.0=superseded 1.0.0=approved" {
		t.Errorf("versions = %v", states)
	}
	audit := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/crm/audit"))
	if len(audit.Rows) != 1 {
		t.Fatalf("audit = %+v", audit.Rows)
	}
	if row := audit.Rows[0]; row.Agent.Slug != agent.CEOSlug || row.Action != "rollback" || !row.Allowed || row.Reason != "from version 2.0.0 to version 1.0.0" {
		t.Errorf("audit row = %+v", row)
	}

	// Rolled back from is superseded, and can be rolled back to in turn.
	if again := orgOK[apitypes.IntegrationVersion](t, intPost(t, srv, intBase+"/crm/versions/2.0.0/rollback")); again.State != apitypes.IntegrationVersionStateApproved {
		t.Errorf("roll back to 2.0.0 = %+v", again)
	}
	if rr := intUpload(t, srv, intBundle(t, "crm", "3.0.0")); rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	intPost(t, srv, intBase+"/crm/versions/3.0.0/reject")
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/versions/3.0.0/rollback"), http.StatusConflict); e.Error != "version 3.0.0 of crm was rejected and never runs" {
		t.Errorf("roll back to a rejected version = %+v", e)
	}
	assertAPIError(t, intPost(t, srv, intBase+"/crm/versions/9.9.9/rollback"), http.StatusNotFound)
	assertAPIError(t, intPost(t, srv, intBase+"/nope/versions/1.0.0/rollback"), http.StatusNotFound)
	if it, _ := srv.Store.GetIntegration("crm"); it.CurrentVersion != "2.0.0" {
		t.Errorf("current after the refusals = %q", it.CurrentVersion)
	}
}

// Why this test exists: an agent's access is set once and outlives the
// version it was set under, so approving a version or rolling back to
// one re-checks every row against the version in use. A row the new
// form refuses is invalid on the integration's page and on the agent's
// own tab, with the form's reason, and is not counted as an agent with
// access; setting it again makes it valid. Without this, the page
// would show access that grants nothing as access that works.
func TestAPIIntegrationAccessAfterANewVersion(t *testing.T) {
	srv := newTestServer(t)
	intTeam(t, srv, "dana")
	intConnected(t, srv, "crm")
	putAccess := func(raw string) apitypes.IntegrationPage {
		t.Helper()
		return orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/crm/access/dana", apitypes.AccessWrite{Access: []byte(raw)}))
	}
	danaRow := func(page apitypes.IntegrationPage) apitypes.AccessRow {
		t.Helper()
		if len(page.Access) != 1 {
			t.Fatalf("access = %+v", page.Access)
		}
		return page.Access[0]
	}
	tabInvalid := func() string {
		t.Helper()
		tab := orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/dana/integrations"))
		if len(tab.Connected) != 1 {
			t.Fatalf("tab = %+v", tab)
		}
		if tab.Connected[0].Invalid == nil {
			return ""
		}
		return *tab.Connected[0].Invalid
	}
	if row := danaRow(putAccess(`{"blogs":["M"],"region":"eu"}`)); row.Invalid != nil {
		t.Fatalf("under 1.0.0: %+v", row)
	}
	intUpload(t, srv, crmVersion2(t))
	intPost(t, srv, intBase+"/crm/versions/2.0.0/approve")
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if row := danaRow(page); row.Invalid == nil || *row.Invalid != "Region must be one of us, apac" || row.Summary != "Blogs: M · Region: eu" {
		t.Errorf("under 2.0.0: %+v", row)
	}
	if page.Summary.Agents != 0 {
		t.Errorf("an invalid row counts as an agent with access: %d", page.Summary.Agents)
	}
	if got := tabInvalid(); got != "Region must be one of us, apac" {
		t.Errorf("tab under 2.0.0: %q", got)
	}

	page = putAccess(`{"blogs":["M"],"region":"apac"}`)
	if row := danaRow(page); row.Invalid != nil || page.Summary.Agents != 1 {
		t.Errorf("set again: %+v, agents %d", row, page.Summary.Agents)
	}
	if got := tabInvalid(); got != "" {
		t.Errorf("tab after setting it again: %q", got)
	}

	intPost(t, srv, intBase+"/crm/versions/1.0.0/rollback")
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if row := danaRow(page); row.Invalid == nil || *row.Invalid != "Region must be one of eu, us" {
		t.Errorf("rolled back to 1.0.0: %+v", row)
	}
	if got := tabInvalid(); got != "Region must be one of eu, us" {
		t.Errorf("tab after the roll back: %q", got)
	}
}

// seedIntegrationInstallRequest files an install request from the
// Kivali Developer in Needs you, as a publish will.
func seedIntegrationInstallRequest(t *testing.T, srv *Server, name, version string) string {
	t.Helper()
	req := store.Message{
		Type:               store.MsgCEOApprovalRequest,
		Title:              "Install " + name + " " + version,
		From:               "kivali-developer",
		To:                 store.Recipients{agent.CEOSlug},
		Date:               time.Now().UTC(),
		Body:               "The team asked for it.",
		IntegrationInstall: &store.IntegrationInstall{Name: name, Version: version},
	}
	abs, err := srv.Store.WriteMessage(req)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(srv.Store.Root(), abs)
	if err := srv.Store.AppendChatMessage(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: req.Title, Kind: "ceo_inbox", MessageRef: rel,
	}); err != nil {
		t.Fatal(err)
	}
	return rel
}

// integrationResponse is the one approval response on file.
func integrationResponse(t *testing.T, srv *Server) store.Message {
	t.Helper()
	resps, _ := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalResponse})
	if len(resps) != 1 {
		t.Fatalf("responses = %+v", resps)
	}
	return resps[0]
}

// An install request is a proposal in Needs you whose review page is
// the version's install screen, not a proposal page. Approving from
// the install screen answers it exactly as Needs you would: the
// version is approved, the response names it, the publisher hears back
// and the row leaves Needs you. Without that, the row would stay after
// the version was approved, and approving it later would fail.
func TestAPIIntegrationApproveAnswersTheRequest(t *testing.T) {
	srv, _ := newTurnServer(t)
	intTeam(t, srv, "kivali-developer")
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	rel := seedIntegrationInstallRequest(t, srv, "crm", "1.0.0")

	home := decodeAPI[apitypes.Home](t, homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil))
	if len(home.Needs) != 1 {
		t.Fatalf("needs = %+v", home.Needs)
	}
	n := home.Needs[0]
	if n.Kind != apitypes.NeedKindProposal || n.ProposalKind == nil || *n.ProposalKind != apitypes.ProposalKindIntegrationInstall ||
		n.ReviewPath == nil || *n.ReviewPath != "/org/integrations/crm/versions/1.0.0" {
		t.Errorf("need = %+v", n)
	}
	// Its page is the install screen; the proposal page has none.
	mux := http.NewServeMux()
	srv.wireAPIProposalRoutes(mux)
	assertAPIError(t, serveMux(t, mux, http.MethodGet, "/api/v1/proposals/"+rel), http.StatusNotFound)

	v := orgOK[apitypes.IntegrationVersion](t, intPost(t, srv, intBase+"/crm/versions/1.0.0/approve"))
	if v.State != apitypes.IntegrationVersionStateApproved {
		t.Errorf("state = %s", v.State)
	}
	resp := integrationResponse(t, srv)
	if resp.Approved == nil || !*resp.Approved || resp.IntegrationInstall == nil || *resp.IntegrationInstall != (store.IntegrationInstall{Name: "crm", Version: "1.0.0"}) || resp.InReplyTo != rel {
		t.Errorf("response = %+v", resp)
	}
	if last := lastChat(t, srv, "kivali-developer"); !strings.Contains(last, "I approved version 1.0.0 of the `crm` integration. It is the version in use.") {
		t.Errorf("publisher heard %q", last)
	}
	home = decodeAPI[apitypes.Home](t, homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil))
	if len(home.Needs) != 0 {
		t.Errorf("needs after approve = %+v", home.Needs)
	}
}

// A request from an agent no longer on the team cannot be answered
// (respondAsCEO refuses an undeliverable response), but the version it
// names can still be approved from its install screen. Without that,
// an offboarded publisher would leave its version unapprovable.
func TestAPIIntegrationApproveWhenThePublisherLeft(t *testing.T) {
	srv, _ := newTurnServer(t)
	intTeam(t, srv, "kivali-developer")
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	seedIntegrationInstallRequest(t, srv, "crm", "1.0.0")
	if err := srv.Store.ArchiveAgent("kivali-developer"); err != nil {
		t.Fatal(err)
	}
	v := orgOK[apitypes.IntegrationVersion](t, intPost(t, srv, intBase+"/crm/versions/1.0.0/approve"))
	if v.State != apitypes.IntegrationVersionStateApproved {
		t.Errorf("state = %s", v.State)
	}
}

// serveMux serves one request through mux with the API's middleware.
func serveMux(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rr, req)
	return rr
}

// Rejecting from the install screen answers the request with a denial
// that names the version, and the version is rejected. If the reject
// path skipped the request, it would stay in Needs you and the
// publisher would never hear that the version will not run.
func TestAPIIntegrationRejectAnswersTheRequest(t *testing.T) {
	srv, _ := newTurnServer(t)
	intTeam(t, srv, "kivali-developer")
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	seedIntegrationInstallRequest(t, srv, "crm", "1.0.0")
	v := orgOK[apitypes.IntegrationVersion](t, intPost(t, srv, intBase+"/crm/versions/1.0.0/reject"))
	if v.State != apitypes.IntegrationVersionStateRejected {
		t.Errorf("state = %s", v.State)
	}
	resp := integrationResponse(t, srv)
	if resp.Approved == nil || *resp.Approved || resp.IntegrationInstall == nil || resp.IntegrationInstall.Version != "1.0.0" {
		t.Errorf("response = %+v", resp)
	}
	if last := lastChat(t, srv, "kivali-developer"); !strings.Contains(last, "I rejected version 1.0.0 of the `crm` integration. That version does not run.") {
		t.Errorf("publisher heard %q", last)
	}
}

// Answering from Needs you applies the answer to the version first:
// approve makes it current, deny rejects it, and the result names the
// version. A version already answered refuses an approval (the request
// stays), and a denial of a version no longer stored goes through, so
// a removed integration's request can still be cleared.
func TestNeedsYouAnswersIntegrationInstall(t *testing.T) {
	srv, _ := newTurnServer(t)
	intTeam(t, srv, "kivali-developer")
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	intUpload(t, srv, intBundle(t, "crm", "1.1.0"))
	approveRel := seedIntegrationInstallRequest(t, srv, "crm", "1.0.0")

	body, ct := needsForm(t, approveRel, "", nil)
	rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil)
	got := decodeAPI[apitypes.NeedActionResponse](t, rr)
	if rr.Code != http.StatusOK || got.Result != "Test crm 1.0.0 is approved" || got.Link != "/org/integrations/crm" {
		t.Errorf("approve: %d %+v", rr.Code, got)
	}
	if it, _ := srv.Store.GetIntegration("crm"); it.CurrentVersion != "1.0.0" {
		t.Errorf("current = %q", it.CurrentVersion)
	}
	// A second request for the version in use is what was asked for.
	againRel := seedIntegrationInstallRequest(t, srv, "crm", "1.0.0")
	body, ct = needsForm(t, againRel, "", nil)
	if rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil); rr.Code != http.StatusOK {
		t.Errorf("approving the version in use: %d %s", rr.Code, rr.Body.String())
	}
	// Denying the version in use would be a denial that is not true.
	denyRel := seedIntegrationInstallRequest(t, srv, "crm", "1.0.0")
	body, ct = needsForm(t, denyRel, "", nil)
	if e := assertAPIError(t, homeDo(t, srv, http.MethodPost, "/api/v1/needs/deny", ct, body, nil), http.StatusConflict); !strings.Contains(e.Error, "could not be turned down") {
		t.Errorf("denying the version in use = %+v", e)
	}

	// A request for a version answered on the install screen already.
	if rr := intPost(t, srv, intBase+"/crm/versions/1.1.0/reject"); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	staleRel := seedIntegrationInstallRequest(t, srv, "crm", "1.1.0")
	body, ct = needsForm(t, staleRel, "", nil)
	e := assertAPIError(t, homeDo(t, srv, http.MethodPost, "/api/v1/needs/approve", ct, body, nil), http.StatusConflict)
	if !strings.Contains(e.Error, "has already been answered") {
		t.Errorf("stale approve = %+v", e)
	}
	// The same request denied goes through: it was rejected already.
	rr = homeDo(t, srv, http.MethodPost, "/api/v1/needs/deny", ct, body, nil)
	if got := decodeAPI[apitypes.NeedActionResponse](t, rr); rr.Code != http.StatusOK || got.Result != "Test crm 1.1.0 is rejected" {
		t.Errorf("deny: %d %+v", rr.Code, got)
	}

	// Removed since: a denial clears it.
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatal(rr.Body.String())
	}
	goneRel := seedIntegrationInstallRequest(t, srv, "crm", "1.0.0")
	body, ct = needsForm(t, goneRel, "", nil)
	if rr := homeDo(t, srv, http.MethodPost, "/api/v1/needs/deny", ct, body, nil); rr.Code != http.StatusOK {
		t.Errorf("deny of a removed integration: %d %s", rr.Code, rr.Body.String())
	}
}

// The connection: refused until a version is approved; a required
// field must have a value, stored or sent; a key the form does not
// have is a 400. The secret goes to the Secret store and nowhere else:
// not the response, not the data directory. A sensitive field left
// out keeps its value. A Secret that has gone missing reads as
// "reconnect needed", an unmet connection requirement whose Fix is setup.
// Uninstalling removes the record, the Secret and every agent's access.
func TestAPIIntegrationConnection(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	intTeam(t, srv, "dana")
	intUpload(t, srv, intBundle(t, "crm", "1.0.0"))
	put := func(fields map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		return orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: fields})
	}
	assertAPIError(t, put(map[string]string{"token": "x"}), http.StatusConflict)
	if rr := intPost(t, srv, intBase+"/crm/versions/1.0.0/approve"); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	// Connected or not is said once the integration is running.
	attachShim(t, srv, "crm", toolTexts(nil))
	if e := assertAPIError(t, put(map[string]string{"portal": "acme"}), http.StatusBadRequest); e.Error != "the connection needs Private app token" {
		t.Errorf("required = %+v", e)
	}
	if e := assertAPIError(t, put(map[string]string{"token": "x", "region": "eu"}), http.StatusBadRequest); !strings.Contains(e.Error, "region is not a field") {
		t.Errorf("unknown = %+v", e)
	}

	rr := put(map[string]string{"token": "s3cret-value", "portal": "acme"})
	page := orgOK[apitypes.IntegrationPage](t, rr)
	if strings.Contains(rr.Body.String(), "s3cret-value") {
		t.Fatal("the response carries the secret")
	}
	if !installedAndMet(page) || page.Connection == nil || page.Connection.ReconnectNeeded || len(page.Connection.ID) != 8 {
		t.Errorf("connected page = %+v %+v", page.Summary, page.Connection)
	}
	fields := map[string]apitypes.ConnectionField{}
	for _, f := range page.ConnectionFields {
		fields[f.Key] = f
	}
	if !fields["token"].Set || fields["token"].Value != nil || fields["portal"].Value == nil || *fields["portal"].Value != "acme" {
		t.Errorf("fields = %+v", fields)
	}
	secretName := secrets.ConnectionSecretName("crm", page.Connection.ID)
	if got, err := mem.Get(context.Background(), secretName); err != nil || got["token"] != "s3cret-value" || len(got) != 1 {
		t.Errorf("secret = %v, %v", got, err)
	}
	assertSecretNotOnDisk(t, srv, "s3cret-value")

	// The token left out keeps its value; the portal is what is sent.
	page = orgOK[apitypes.IntegrationPage](t, put(map[string]string{"portal": "acme2"}))
	if got, _ := mem.Get(context.Background(), secretName); got["token"] != "s3cret-value" {
		t.Errorf("token after a write without it = %q", got["token"])
	}
	// Clearing a required secret is refused, and changes nothing.
	assertAPIError(t, put(map[string]string{"token": "", "portal": "acme2"}), http.StatusBadRequest)
	if got, _ := mem.Get(context.Background(), secretName); got["token"] != "s3cret-value" {
		t.Errorf("token after a refused clear = %q", got["token"])
	}

	// Access for dana, then the Secret disappears (a restore into
	// another install).
	if rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm/access/dana", apitypes.AccessWrite{Access: []byte(`{"blogs":["M"]}`)}); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	_ = mem.Delete(context.Background(), secretName)
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if row, _ := requirementOf(page, "connection"); page.Connection == nil || !page.Connection.ReconnectNeeded || page.Summary.State != apitypes.IntegrationStateInstalled ||
		page.Summary.Needs != 1 || row.Met || row.Fix != "setup" {
		t.Errorf("after the Secret went: %+v %+v", page.Summary, page.Connection)
	}
	for _, f := range page.ConnectionFields {
		if f.Key == "token" && f.Set {
			t.Error("token reads as set with no Secret")
		}
	}
	// Entering it again needs the token: nothing stored to keep.
	assertAPIError(t, put(map[string]string{"portal": "acme2"}), http.StatusBadRequest)
	orgOK[apitypes.IntegrationPage](t, put(map[string]string{"token": "again", "portal": "acme2"}))

	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/crm"), http.StatusNotFound)
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets left: %v", names)
	}
}

// assertSecretNotOnDisk fails when any file under the data directory
// holds secret: connection values belong in the Secret store only, and
// the data directory is what a backup carries.
func assertSecretNotOnDisk(t *testing.T, srv *Server, secret string) {
	t.Helper()
	_ = filepath.WalkDir(srv.Store.Root(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(secret)) {
			t.Errorf("%s holds the secret", p)
		}
		return nil
	})
}

// Access per agent: it needs an approved version and a connection,
// an agent on the team (not the owner), and a filled form the version
// accepts, with the field's message on a refusal. A granted agent
// moves from agents_without to the access rows, with its access in
// words, and its Integrations tab lists the integration under
// Connected; removing it moves it back.
func TestAPIIntegrationAccess(t *testing.T) {
	srv := newTestServer(t)
	intTeam(t, srv, "dana", "lee")
	putAccess := func(slug, raw string) *httptest.ResponseRecorder {
		t.Helper()
		return orgJSON(t, srv, http.MethodPut, intBase+"/crm/access/"+slug, apitypes.AccessWrite{Access: []byte(raw)})
	}
	intInstalled(t, srv, "crm", "1.0.0")
	if e := assertAPIError(t, putAccess("dana", `{"blogs":["M"]}`), http.StatusConflict); e.Error != "install Test crm first" {
		t.Errorf("no connection = %+v", e)
	}
	if rr := orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "t"}}); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	attachShim(t, srv, "crm", toolTexts(nil))
	assertAPIError(t, putAccess("nobody", `{"blogs":["M"]}`), http.StatusNotFound)
	assertAPIError(t, putAccess(agent.CEOSlug, `{"blogs":["M"]}`), http.StatusBadRequest)
	if e := assertAPIError(t, putAccess("dana", `{"blogs":["M"],"region":"apac"}`), http.StatusBadRequest); e.Error != "Region must be one of eu, us" {
		t.Errorf("bad access = %+v", e)
	}
	if e := assertAPIError(t, putAccess("dana", `{}`), http.StatusBadRequest); e.Error != "Blogs is required" {
		t.Errorf("missing = %+v", e)
	}
	page := orgOK[apitypes.IntegrationPage](t, putAccess("dana", `{"blogs":["Marketing","Eng"],"write":true}`))
	if len(page.Access) != 1 || page.Access[0].Agent.Slug != "dana" || page.Access[0].Summary != "Blogs: Marketing, Eng · May publish and edit" || page.Access[0].Invalid != nil {
		t.Errorf("access = %+v", page.Access)
	}
	if len(page.AgentsWithout) != 1 || page.AgentsWithout[0].Slug != "lee" {
		t.Errorf("agents without = %+v", page.AgentsWithout)
	}
	if page.Summary.Agents != 1 {
		t.Errorf("agents = %d", page.Summary.Agents)
	}

	intUpload(t, srv, intBundle(t, "blog", "1.0.0"))
	tab := orgOK[apitypes.AgentIntegrations](t, orgGet(t, srv, "/api/v1/agents/dana/integrations"))
	if len(tab.Connected) != 1 || tab.Connected[0].Name != "crm" || tab.Connected[0].Summary != "Blogs: Marketing, Eng · May publish and edit" || tab.Connected[0].State != apitypes.IntegrationStateInstalled || !tab.Connected[0].Usable {
		t.Errorf("connected = %+v", tab.Connected)
	}
	if len(tab.Available) != 1 || tab.Available[0].Name != "blog" || tab.Available[0].State != apitypes.IntegrationStatePending || tab.Available[0].Usable || tab.Available[0].Summary != "" {
		t.Errorf("available = %+v", tab.Available)
	}

	page = orgOK[apitypes.IntegrationPage](t, intDelete(t, srv, intBase+"/crm/access/dana"))
	if len(page.Access) != 0 || len(page.AgentsWithout) != 2 {
		t.Errorf("after removing access: %+v", page)
	}
	assertAPIError(t, orgGet(t, srv, "/api/v1/agents/nobody/integrations"), http.StatusNotFound)
	if err := srv.Store.ArchiveAgent("lee"); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, orgGet(t, srv, "/api/v1/agents/lee/integrations"), http.StatusNotFound)
}

// The switch and Uninstall. Off is its own state; uninstalling an
// uploaded integration deletes the connection's Secret and the whole
// integration and answers 204, and the list is empty after; a built-in
// with nothing installed has nothing to uninstall (409).
func TestAPIIntegrationSwitchAndRemove(t *testing.T) {
	srv := newTestServer(t)
	mem := secrets.NewMemory()
	srv.Secrets = mem
	intConnected(t, srv, "crm")
	attachShim(t, srv, "crm", toolTexts(nil))
	page := orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/disable"))
	if page.Summary.State != apitypes.IntegrationStateOff || page.Summary.Enabled || page.Summary.Needs != 0 || slices.Contains(requirementIDs(page), "running") {
		t.Errorf("off = %+v", page.Summary)
	}
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/enable"))
	if !installedAndMet(page) {
		t.Errorf("on = %+v", page.Summary)
	}
	if rr := intDelete(t, srv, intBase+"/crm"); rr.Code != http.StatusNoContent {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if list := orgOK[apitypes.Integrations](t, orgGet(t, srv, intBase)); len(list.Integrations) != 0 {
		t.Errorf("list after remove = %+v", list)
	}
	if names := mem.Names(); len(names) != 0 {
		t.Errorf("secrets left: %v", names)
	}
	assertAPIError(t, intDelete(t, srv, intBase+"/crm"), http.StatusNotFound)
	withEcho(t, srv)
	if e := assertAPIError(t, intDelete(t, srv, intBase+"/echo"), http.StatusConflict); e.Error != "Echo is not installed, so there is nothing to uninstall" {
		t.Errorf("uninstall of a built-in with nothing installed = %+v", e)
	}
	assertAPIError(t, intPost(t, srv, intBase+"/nope/enable"), http.StatusNotFound)
}

// The audit lists what is stored, newest first, up to limit, with the
// caller named; the options a field lists need the integration
// running, and a field that lists none is a 404.
func TestAPIIntegrationAuditAndChoices(t *testing.T) {
	srv := newTestServer(t)
	intTeam(t, srv, "dana")
	intInstalled(t, srv, "crm", "1.0.0")
	if a := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/crm/audit")); len(a.Rows) != 0 {
		t.Errorf("empty audit = %+v", a)
	}
	for i, allowed := range []bool{true, false} {
		if err := srv.Store.AppendIntegrationAudit("crm", store.IntegrationAuditRow{At: time.Date(2026, 10, 21, 9, i, 0, 0, time.UTC), Agent: "dana", Action: "create_post", Allowed: allowed, Reason: map[bool]string{false: "no access"}[allowed], DurationMS: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	a := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/crm/audit?limit=1"))
	if len(a.Rows) != 1 || a.Rows[0].Allowed || a.Rows[0].Reason != "no access" || a.Rows[0].Agent != (apitypes.PersonRef{Slug: "dana", Name: "Dana"}) {
		t.Errorf("audit = %+v", a)
	}
	if a := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/crm/audit?limit=5000")); len(a.Rows) != 2 {
		t.Errorf("capped audit = %+v", a)
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/crm/audit?limit=abc"), http.StatusBadRequest)
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/crm/choices/blogs"), http.StatusConflict); e.Error != "the integration is not running" {
		t.Errorf("choices = %+v", e)
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/crm/choices/write"), http.StatusNotFound)
}
