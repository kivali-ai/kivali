package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Test connection calls the verify action through the running server
// with no agent on the call, keeps what it found on the connection
// (verified, the integration's words, when), and writes an audit row
// for the owner; a failed check is kept as failed with the
// integration's words. It answers 409 without a connection and while
// the integration is not running. A verify that reached an agent's
// stamp, or forgot its result, would mislead the owner about the
// connection.
func TestAPIIntegrationVerify(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intInstalled(t, srv, "crm", "1.0.0")
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/verify"), http.StatusConflict); e.Error != "install Test crm first" {
		t.Errorf("no connection = %+v", e)
	}
	orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "t"}})
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/verify"), http.StatusConflict); e.Error != "the integration is not running" || e.Who != whoBundle {
		t.Errorf("not running = %+v", e)
	}
	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if page.Summary.State != apitypes.IntegrationStateInstalled || page.Running || !slices.Equal(unmetIDs(page), []string{"running"}) {
		t.Errorf("without a shim: state %s, running %v, unmet %v", page.Summary.State, page.Running, unmetIDs(page))
	}
	if !page.ConnectionTest {
		t.Error("connection_test is false for a manifest that names kivali.verify")
	}

	fs := attachShim(t, srv, "crm", toolTexts(map[string]string{"whoami": "Signed in as Acme"}))
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/verify"))
	c := page.Connection
	if c == nil || c.Verified == nil || !*c.Verified || c.VerifyMessage == nil || *c.VerifyMessage != "Signed in as Acme" || c.VerifiedAt == nil {
		t.Fatalf("connection = %+v", c)
	}
	if !installedAndMet(page) || !page.Running {
		t.Errorf("state %s, unmet %v", page.Summary.State, unmetIDs(page))
	}
	fs.mu.Lock()
	var call integrationpod.Message
	for _, m := range fs.requests {
		if m.Method == "tools/call" {
			call = m
		}
	}
	fs.mu.Unlock()
	if string(call.Params) != `{"arguments":{},"name":"whoami"}` {
		t.Errorf("the call carried %s", call.Params)
	}
	audit := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/crm/audit"))
	if len(audit.Rows) != 1 || audit.Rows[0].Agent.Slug != agent.CEOSlug || audit.Rows[0].Action != "whoami" || !audit.Rows[0].Allowed || audit.Rows[0].Reason != "" {
		t.Errorf("audit = %+v", audit.Rows)
	}

	// The tool's own error result: kept as a failed test.
	fs.setAnswer(toolTexts(nil))
	page = orgOK[apitypes.IntegrationPage](t, intPost(t, srv, intBase+"/crm/connection/verify"))
	if c := page.Connection; c.Verified == nil || *c.Verified || c.VerifyMessage == nil || *c.VerifyMessage != "no tool whoami" {
		t.Errorf("failed test = %+v", c)
	}
	audit = orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/crm/audit"))
	if audit.Rows[0].Reason != "no tool whoami" {
		t.Errorf("audit = %+v", audit.Rows[0])
	}

	// A connection write clears the result, as before.
	page = orgOK[apitypes.IntegrationPage](t, orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "t2"}}))
	if page.Connection.Verified != nil {
		t.Errorf("result kept over a new connection: %+v", page.Connection)
	}
}

// A manifest without a verify action has no Test connection, and says
// so rather than call something. This fails if the route calls a tool
// named "", if the page says there is a test (the web app would offer a
// button that can only fail), or if a result kept under an earlier
// version that had a test still reaches the page, where it would read
// as a test the owner can run again.
func TestAPIIntegrationVerifyWithoutAnAction(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	m := integrationManifestWith(t, "crm", func(m map[string]any) { delete(m["kivali"].(map[string]any), "verify") })
	intUploadApprove(t, srv, m)
	orgJSON(t, srv, http.MethodPut, intBase+"/crm/connection", apitypes.ConnectionWrite{Fields: map[string]string{"token": "t"}})
	attachShim(t, srv, "crm", toolTexts(nil))
	if e := assertAPIError(t, intPost(t, srv, intBase+"/crm/connection/verify"), http.StatusConflict); e.Error != "Test crm has no connection test" {
		t.Errorf("= %+v", e)
	}

	page := orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if page.ConnectionTest {
		t.Error("connection_test is true for a manifest without kivali.verify")
	}
	conn, err := srv.Store.GetIntegrationConnectionByID("crm", page.Connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	at, failed := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), false
	conn.VerifiedAt, conn.Verified, conn.VerifyMessage = &at, &failed, "The token was refused."
	if err := srv.Store.PutIntegrationConnection("crm", conn); err != nil {
		t.Fatal(err)
	}
	page = orgOK[apitypes.IntegrationPage](t, orgGet(t, srv, intBase+"/crm"))
	if c := page.Connection; c.VerifiedAt != nil || c.Verified != nil || c.VerifyMessage != nil {
		t.Errorf("a kept result reached the page: %+v", c)
	}
}

// The options a field lists come from its tool through the running
// server: a JSON array of names, or an object whose options is one,
// each with an audit row. Text that is neither, or the tool's error, is
// a 502 in the integration's words. A field whose options came back
// wrong would grant access to names the integration does not have.
func TestAPIIntegrationChoices(t *testing.T) {
	srv := newTestServer(t)
	withIntegrationRegistry(srv, newFakeIntegrationProv())
	intConnected(t, srv, "crm")
	fs := attachShim(t, srv, "crm", toolTexts(map[string]string{"list_blogs": `["Marketing","Eng"]`}))
	got := orgOK[apitypes.AccessChoices](t, orgGet(t, srv, intBase+"/crm/choices/blogs"))
	if strings.Join(got.Options, ",") != "Marketing,Eng" {
		t.Errorf("options = %v", got.Options)
	}
	fs.setAnswer(toolTexts(map[string]string{"list_blogs": `{"options":["Sales"]}`}))
	if got := orgOK[apitypes.AccessChoices](t, orgGet(t, srv, intBase+"/crm/choices/blogs")); strings.Join(got.Options, ",") != "Sales" {
		t.Errorf("object options = %v", got.Options)
	}
	fs.setAnswer(toolTexts(map[string]string{"list_blogs": `{"options":[]}`}))
	if rr := orgGet(t, srv, intBase+"/crm/choices/blogs"); !strings.Contains(rr.Body.String(), `"options":[]`) {
		t.Errorf("no options = %s", rr.Body.String())
	}
	fs.setAnswer(toolTexts(map[string]string{"list_blogs": "Marketing, Eng"}))
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/crm/choices/blogs"), http.StatusBadGateway); e.Error != "Test crm's options are not a list of names" {
		t.Errorf("text = %+v", e)
	}
	fs.setAnswer(toolTexts(nil))
	if e := assertAPIError(t, orgGet(t, srv, intBase+"/crm/choices/blogs"), http.StatusBadGateway); e.Error != "Test crm could not list the options: no tool list_blogs" {
		t.Errorf("error result = %+v", e)
	}
	audit := orgOK[apitypes.IntegrationAudit](t, orgGet(t, srv, intBase+"/crm/audit"))
	if len(audit.Rows) != 5 || audit.Rows[0].Action != "list_blogs" || audit.Rows[0].Reason != "no tool list_blogs" || audit.Rows[4].Reason != "" {
		t.Errorf("audit = %+v", audit.Rows)
	}
}

// The log reads oldest first, the latest 200 by default and up to
// 2000, and refuses a limit that is not a number. A log in the other
// order, or one that read the whole file, would hide what just
// happened.
func TestAPIIntegrationLogs(t *testing.T) {
	srv := newTestServer(t)
	intInstalled(t, srv, "crm", "1.0.0")
	if l := orgOK[apitypes.IntegrationLogs](t, orgGet(t, srv, intBase+"/crm/logs")); len(l.Lines) != 0 {
		t.Errorf("empty log = %+v", l)
	}
	base := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 250; i++ {
		if err := srv.Store.AppendIntegrationLog("crm", store.IntegrationLogRow{At: base.Add(time.Duration(i) * time.Second), Level: "info", Message: "line"}); err != nil {
			t.Fatal(err)
		}
	}
	l := orgOK[apitypes.IntegrationLogs](t, orgGet(t, srv, intBase+"/crm/logs"))
	if len(l.Lines) != 200 || !l.Lines[0].At.Equal(base.Add(50*time.Second)) || !l.Lines[199].At.Equal(base.Add(249*time.Second)) {
		t.Errorf("default = %d lines, %v..%v", len(l.Lines), l.Lines[0].At, l.Lines[len(l.Lines)-1].At)
	}
	if l := orgOK[apitypes.IntegrationLogs](t, orgGet(t, srv, intBase+"/crm/logs?limit=2")); len(l.Lines) != 2 || l.Lines[1].Level != "info" {
		t.Errorf("limit 2 = %+v", l)
	}
	if l := orgOK[apitypes.IntegrationLogs](t, orgGet(t, srv, intBase+"/crm/logs?limit=99999")); len(l.Lines) != 250 {
		t.Errorf("capped = %d", len(l.Lines))
	}
	assertAPIError(t, orgGet(t, srv, intBase+"/crm/logs?limit=x"), http.StatusBadRequest)
	assertAPIError(t, orgGet(t, srv, intBase+"/nope/logs"), http.StatusNotFound)
}

// integrationManifestWith is the fixture manifest for name, changed by
// edit.
func integrationManifestWith(t *testing.T, name string, edit func(map[string]any)) map[string]any {
	t.Helper()
	m := integrationtest.Manifest(name, "1.0.0")
	edit(m)
	return m
}

// intUploadApprove uploads the bundle of manifest m and approves it.
func intUploadApprove(t *testing.T, srv *Server, m map[string]any) {
	t.Helper()
	rr := intUpload(t, srv, integrationtest.Zip(t, "", m, nil))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var v apitypes.IntegrationVersion
	_ = json.Unmarshal(rr.Body.Bytes(), &v)
	if rr := intPost(t, srv, intBase+"/"+v.Name+"/versions/"+v.Version+"/approve"); rr.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rr.Code, rr.Body.String())
	}
}
