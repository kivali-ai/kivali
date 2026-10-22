package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
)

var intNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// testBundle is a checked bundle of the integrationtest fixture.
func testBundle(t *testing.T, name, version string) *integration.Bundle {
	t.Helper()
	b, err := integration.ReadBundle(integrationtest.Zip(t, name, integrationtest.Manifest(name, version), nil))
	if err != nil {
		t.Fatal(err)
	}
	if f := integration.Validate(b.Manifest, b.Files); f.HasErrors() {
		t.Fatalf("fixture findings: %v", f.Texts())
	}
	return b
}

func storeVersion(t *testing.T, s *FSStore, name, version string) {
	t.Helper()
	if err := s.StoreIntegrationVersion(name, testBundle(t, name, version), IntegrationVersion{
		PublishedBy: "ceo", PublishedAt: intNow, NotesMD: "notes " + version, Findings: []string{"a warning"},
	}); err != nil {
		t.Fatal(err)
	}
}

// A stored version reads back whole: the record the first version
// makes, the version's own record, its manifest byte for byte and the
// bundle unpacked where slice 2 mounts it. Losing any of these would
// show as an install screen with blanks, or a server with no code.
func TestStoreIntegrationVersionRoundTrip(t *testing.T) {
	s := mustStore(t)
	b := testBundle(t, "crm", "1.0.0")
	if err := s.StoreIntegrationVersion("crm", b, IntegrationVersion{PublishedBy: "ceo", PublishedAt: intNow, NotesMD: "first", Findings: []string{"w"}}); err != nil {
		t.Fatal(err)
	}
	its, err := s.ListIntegrations()
	if err != nil || len(its) != 1 {
		t.Fatalf("ListIntegrations = %v, %v", its, err)
	}
	it := its[0]
	if it.Name != "crm" || it.DisplayName != "Test crm" || !it.Enabled || it.Builtin || it.CurrentVersion != "" || it.InstalledBy != "ceo" || !it.InstalledAt.Equal(intNow) {
		t.Errorf("integration = %+v", it)
	}
	v, m, err := s.GetIntegrationVersion("crm", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != IntegrationVersionPending || v.NotesMD != "first" || v.PublishedBy != "ceo" || v.FileCount != 2 || v.BundleBytes != b.Size || len(v.Findings) != 1 || m.Name != "crm" {
		t.Errorf("version = %+v, manifest %v", v, m.Name)
	}
	raw, err := os.ReadFile(filepath.Join(s.Root(), "integrations", "crm", "versions", "1.0.0", "manifest.json"))
	if err != nil || !bytes.Equal(raw, b.ManifestBytes()) {
		t.Errorf("manifest.json not kept byte for byte: %v", err)
	}
	dir, _ := s.IntegrationBundleDir("crm", "1.0.0")
	if body, err := os.ReadFile(filepath.Join(dir, "server", "main.py")); err != nil || string(body) != integrationtest.MainPy {
		t.Errorf("bundle not unpacked: %q, %v", body, err)
	}
	// No staging directory is left behind.
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "integrations", "crm", "versions"))
	if len(entries) != 1 {
		t.Errorf("versions/ holds %d entries", len(entries))
	}
	if _, err := s.GetIntegration("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetIntegration(nope) = %v", err)
	}
	if _, _, err := s.GetIntegrationVersion("crm", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetIntegrationVersion(9.9.9) = %v", err)
	}
}

// The version state machine. Approving makes a version current and
// supersedes the one before; rejecting leaves current alone; neither
// works twice; a version already stored, in any state, cannot be
// stored again. A slip here would let two versions be in use, or let
// a rejected bundle be swapped for other code under the same version.
func TestIntegrationVersionStates(t *testing.T) {
	s := mustStore(t)
	storeVersion(t, s, "crm", "1.0.0")
	if err := s.ApproveIntegrationVersion("crm", "1.0.0", intNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, s, "crm", "1.1.0")
	storeVersion(t, s, "crm", "1.2.0")
	if err := s.ApproveIntegrationVersion("crm", "1.1.0", intNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RejectIntegrationVersion("crm", "1.2.0", intNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	vs, err := s.IntegrationVersions("crm")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]IntegrationVersionState{}
	var order []string
	for _, v := range vs {
		got[v.Version] = v.State
		order = append(order, v.Version)
	}
	if strings.Join(order, ",") != "1.2.0,1.1.0,1.0.0" {
		t.Errorf("order = %v, want newest first", order)
	}
	want := map[string]IntegrationVersionState{"1.0.0": IntegrationVersionSuperseded, "1.1.0": IntegrationVersionApproved, "1.2.0": IntegrationVersionRejected}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %s, want %s", k, got[k], w)
		}
	}
	it, _ := s.GetIntegration("crm")
	if it.CurrentVersion != "1.1.0" {
		t.Errorf("current = %s", it.CurrentVersion)
	}
	v, _, _ := s.GetIntegrationVersion("crm", "1.1.0")
	if v.ApprovedAt == nil || !v.ApprovedAt.Equal(intNow.Add(2*time.Minute)) {
		t.Errorf("approved_at = %v", v.ApprovedAt)
	}
	r, _, _ := s.GetIntegrationVersion("crm", "1.2.0")
	if r.RejectedAt == nil || r.ApprovedAt != nil {
		t.Errorf("rejected version = %+v", r)
	}
	for _, ver := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		if err := s.ApproveIntegrationVersion("crm", ver, intNow); !errors.Is(err, ErrIntegrationVersionNotPending) {
			t.Errorf("approve %s again: %v", ver, err)
		}
		if err := s.RejectIntegrationVersion("crm", ver, intNow); !errors.Is(err, ErrIntegrationVersionNotPending) {
			t.Errorf("reject %s: %v", ver, err)
		}
		if err := s.StoreIntegrationVersion("crm", testBundle(t, "crm", ver), IntegrationVersion{PublishedAt: intNow}); !errors.Is(err, ErrIntegrationVersionExists) {
			t.Errorf("store %s again: %v", ver, err)
		}
	}
	if _, m, err := s.CurrentIntegrationVersion("crm"); err != nil || m.Version != "1.1.0" {
		t.Errorf("CurrentIntegrationVersion = %v, %v", m, err)
	}
}

// Why this test exists: rolling back is the second way a version comes
// to be in use, and it must keep the state machine's promise that one
// version is approved at a time. Only a superseded version can be
// rolled back to; it becomes approved, the one in use becomes
// superseded, and the integration's current version and display name
// follow, so the pod is built from the right bundle and the page names
// the right version. A pending version rolled back to would run code
// the owner never approved, and a rejected one code the owner turned
// down. Every version's bundle stays on disk whatever its state, since
// a roll back runs it again, until the integration is removed.
func TestRollBackIntegrationVersion(t *testing.T) {
	s := mustStore(t)
	storeVersion(t, s, "crm", "1.0.0")
	if err := s.ApproveIntegrationVersion("crm", "1.0.0", intNow); err != nil {
		t.Fatal(err)
	}
	m := integrationtest.Manifest("crm", "1.1.0")
	m["display_name"] = "CRM two"
	b, err := integration.ReadBundle(integrationtest.Zip(t, "crm", m, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreIntegrationVersion("crm", b, IntegrationVersion{PublishedBy: "ceo", PublishedAt: intNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveIntegrationVersion("crm", "1.1.0", intNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, s, "crm", "1.1.1")
	if err := s.RejectIntegrationVersion("crm", "1.1.1", intNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, s, "crm", "1.2.0")
	states := func() string {
		t.Helper()
		vs, err := s.IntegrationVersions("crm")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, v := range vs {
			out = append(out, v.Version+"="+string(v.State))
		}
		return strings.Join(out, " ")
	}
	if it, _ := s.GetIntegration("crm"); it.DisplayName != "CRM two" {
		t.Fatalf("display name before = %q", it.DisplayName)
	}

	at := intNow.Add(time.Hour)
	replaced, err := s.RollBackIntegrationVersion("crm", "1.0.0", at)
	if err != nil || replaced != "1.1.0" {
		t.Fatalf("roll back = %q, %v", replaced, err)
	}
	if got := states(); got != "1.2.0=pending 1.1.1=rejected 1.1.0=superseded 1.0.0=approved" {
		t.Errorf("states after the roll back = %s", got)
	}
	it, _ := s.GetIntegration("crm")
	if it.CurrentVersion != "1.0.0" || it.DisplayName != "Test crm" {
		t.Errorf("integration = %+v", it)
	}
	if v, cur, err := s.CurrentIntegrationVersion("crm"); err != nil || cur.Version != "1.0.0" || v.ApprovedAt == nil || !v.ApprovedAt.Equal(at) {
		t.Errorf("current = %+v, %v", v, err)
	}

	// Only a superseded version can be rolled back to, and a refusal
	// changes nothing.
	for _, ver := range []string{"1.0.0", "1.1.1", "1.2.0"} {
		if _, err := s.RollBackIntegrationVersion("crm", ver, at); !errors.Is(err, ErrIntegrationVersionNotSuperseded) {
			t.Errorf("roll back to %s: %v", ver, err)
		}
	}
	if _, err := s.RollBackIntegrationVersion("crm", "9.9.9", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("roll back to a version not stored: %v", err)
	}
	if _, err := s.RollBackIntegrationVersion("nope", "1.0.0", at); !errors.Is(err, ErrNotFound) {
		t.Errorf("roll back of an integration not installed: %v", err)
	}
	if got := states(); got != "1.2.0=pending 1.1.1=rejected 1.1.0=superseded 1.0.0=approved" {
		t.Errorf("states after the refusals = %s", got)
	}

	// A version rolled back from can be rolled back to in turn, and an
	// approval after a roll back supersedes the version rolled back to.
	if replaced, err := s.RollBackIntegrationVersion("crm", "1.1.0", at); err != nil || replaced != "1.0.0" {
		t.Errorf("roll back to 1.1.0 = %q, %v", replaced, err)
	}
	if err := s.ApproveIntegrationVersion("crm", "1.2.0", at); err != nil {
		t.Fatal(err)
	}
	if got := states(); got != "1.2.0=approved 1.1.1=rejected 1.1.0=superseded 1.0.0=superseded" {
		t.Errorf("states after approving 1.2.0 = %s", got)
	}

	for _, ver := range []string{"1.0.0", "1.1.0", "1.1.1", "1.2.0"} {
		dir, _ := s.IntegrationBundleDir("crm", ver)
		if _, err := os.Stat(filepath.Join(dir, "server", "main.py")); err != nil {
			t.Errorf("the bundle of %s is gone: %v", ver, err)
		}
	}
	if err := s.RemoveIntegration("crm"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root(), "integrations")); len(entries) != 0 {
		t.Errorf("integrations/ after removing holds %v", entries)
	}
}

// Why this test exists: access is checked against the version in use
// when it is read, never when it is written, so approving a version or
// rolling back to one re-validates every row at once. A row the new
// version's form refuses says why and grants nothing, and is valid
// again under a version whose form accepts it. Checked against the
// version a row was set under instead, an agent would keep access the
// version in use cannot express.
func TestIntegrationAccessFollowsTheVersionInUse(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	approvedWithConnection(t, s, "crm")
	if err := s.SetIntegrationAccess("crm", IntegrationAccess{Slug: "dana", ConnectionID: "c1", Access: map[string]any{"blogs": []any{"M"}, "region": "eu"}, SetAt: intNow}); err != nil {
		t.Fatal(err)
	}
	m := integrationtest.Manifest("crm", "2.0.0")
	m["kivali"].(map[string]any)["access"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)["region"] = map[string]any{"type": "string", "enum": []any{"us", "apac"}, "title": "Region"}
	b, err := integration.ReadBundle(integrationtest.Zip(t, "crm", m, nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreIntegrationVersion("crm", b, IntegrationVersion{PublishedBy: "ceo", PublishedAt: intNow}); err != nil {
		t.Fatal(err)
	}
	invalid := func() string {
		t.Helper()
		rows, err := s.ListIntegrationAccess("crm")
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows = %v, %v", rows, err)
		}
		return rows[0].Invalid
	}
	if got := invalid(); got != "" {
		t.Errorf("under 1.0.0: %q", got)
	}
	if err := s.ApproveIntegrationVersion("crm", "2.0.0", intNow); err != nil {
		t.Fatal(err)
	}
	if got := invalid(); got != "Region must be one of us, apac" {
		t.Errorf("under 2.0.0: %q", got)
	}
	if _, err := s.RollBackIntegrationVersion("crm", "1.0.0", intNow); err != nil {
		t.Fatal(err)
	}
	if got := invalid(); got != "" {
		t.Errorf("rolled back to 1.0.0: %q", got)
	}
}

// A bundle is stored under its manifest's name only, and a built-in
// integration takes no upload and cannot be removed: either would let
// an upload replace what ships with Kivali.
func TestStoreIntegrationVersionRefusals(t *testing.T) {
	s := mustStore(t)
	if err := s.StoreIntegrationVersion("other", testBundle(t, "crm", "1.0.0"), IntegrationVersion{}); !errors.Is(err, ErrIntegrationNameMismatch) {
		t.Errorf("mismatch: %v", err)
	}
	if err := s.writeIntegration(Integration{Name: "builtin", DisplayName: "Built in", Enabled: true, Builtin: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreIntegrationVersion("builtin", testBundle(t, "builtin", "1.0.0"), IntegrationVersion{}); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("builtin store: %v", err)
	}
	if err := s.RemoveIntegration("builtin"); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("builtin remove: %v", err)
	}
	if _, err := s.GetIntegration("builtin"); err != nil {
		t.Errorf("builtin gone: %v", err)
	}
	if _, err := s.GetIntegration("../agents"); !errors.Is(err, ErrNotFound) {
		t.Errorf("path-like name: %v", err)
	}
}

// Removing takes everything with it: versions, connection records,
// every agent's access and the audit. A leftover access row would
// grant an agent access to a reinstalled integration nobody granted.
func TestRemoveIntegration(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	storeVersion(t, s, "crm", "1.0.0")
	if err := s.ApproveIntegrationVersion("crm", "1.0.0", intNow); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntegrationConnection("crm", IntegrationConnection{ID: "c1", CreatedAt: intNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntegrationAccess("crm", IntegrationAccess{Slug: "dana", ConnectionID: "c1", Access: map[string]any{"blogs": []any{"M"}}, SetAt: intNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendIntegrationAudit("crm", IntegrationAuditRow{At: intNow, Agent: "dana", Action: "create_post", Allowed: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveIntegration("crm"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "integrations"))
	if len(entries) != 0 {
		t.Errorf("integrations/ holds %v", entries)
	}
	if acc, _ := s.AgentIntegrationAccess("dana"); len(acc) != 0 {
		t.Errorf("access survived: %v", acc)
	}
	storeVersion(t, s, "crm", "1.0.0")
	if rows, _ := s.ListIntegrationAccess("crm"); len(rows) != 0 {
		t.Errorf("reinstall has access rows: %v", rows)
	}
}

func mustAgent(t *testing.T, s *FSStore, slug string) {
	t.Helper()
	if err := s.CreateAgent(Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "# role\n"); err != nil {
		t.Fatal(err)
	}
}

// approvedWithConnection is crm 1.0.0 approved with connection c1.
func approvedWithConnection(t *testing.T, s *FSStore, name string) {
	t.Helper()
	storeVersion(t, s, name, "1.0.0")
	if err := s.ApproveIntegrationVersion(name, "1.0.0", intNow); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntegrationConnection(name, IntegrationConnection{ID: "c1", CreatedAt: intNow, Fields: map[string]string{"portal": "acme"}, SecretKeys: []string{"token"}, SecretName: "kivali-connection-" + name + "-c1"}); err != nil {
		t.Fatal(err)
	}
}

// Access is per agent and per integration, read back across
// integrations for an agent's tab, and checked at read against the
// approved version's form and connection: a row that no longer fits
// says why and grants nothing. An archived agent cannot be given
// access.
func TestIntegrationAccessAcrossAgents(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	mustAgent(t, s, "lee")
	approvedWithConnection(t, s, "crm")
	approvedWithConnection(t, s, "blog")
	set := func(name, slug, conn string, access map[string]any) {
		t.Helper()
		if err := s.SetIntegrationAccess(name, IntegrationAccess{Slug: slug, ConnectionID: conn, Access: access, SetAt: intNow, SetBy: "ceo"}); err != nil {
			t.Fatal(err)
		}
	}
	set("crm", "dana", "c1", map[string]any{"blogs": []any{"Marketing"}, "write": true})
	set("crm", "lee", "c1", map[string]any{"blogs": []any{"Eng"}, "region": "apac"})
	set("blog", "dana", "c0", map[string]any{"blogs": []any{"x"}})

	rows, err := s.ListIntegrationAccess("crm")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if rows[0].Slug != "dana" || rows[0].Invalid != "" || rows[0].SetBy != "ceo" || string(rows[0].AccessJSON()) != `{"blogs":["Marketing"],"write":true}` {
		t.Errorf("dana = %+v %s", rows[0], rows[0].AccessJSON())
	}
	if rows[1].Slug != "lee" || rows[1].Invalid != "Region must be one of eu, us" {
		t.Errorf("lee = %+v", rows[1])
	}
	acc, err := s.AgentIntegrationAccess("dana")
	if err != nil || len(acc) != 2 {
		t.Fatalf("dana's access = %v, %v", acc, err)
	}
	if acc["blog"].Invalid != "it was set for a connection that is no longer there" {
		t.Errorf("blog row = %+v", acc["blog"])
	}
	if err := s.ClearIntegrationAccess("crm", "lee"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearIntegrationAccess("crm", "lee"); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
	if rows, _ := s.ListIntegrationAccess("crm"); len(rows) != 1 {
		t.Errorf("rows after clear = %v", rows)
	}
	if err := s.SetIntegrationAccess("crm", IntegrationAccess{Slug: "nobody", ConnectionID: "c1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("access for no agent: %v", err)
	}
	if err := s.SetIntegrationAccess("crm", IntegrationAccess{Slug: "../x", ConnectionID: "c1"}); err == nil {
		t.Error("path-like slug accepted")
	}
}

// Archiving an agent clears its access everywhere, so restoring or
// rehiring the slug does not inherit grants nobody made for it.
func TestArchiveAgentClearsIntegrationAccess(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "chief-of-staff")
	mustAgent(t, s, "dana")
	approvedWithConnection(t, s, "crm")
	approvedWithConnection(t, s, "blog")
	for _, name := range []string{"crm", "blog"} {
		if err := s.SetIntegrationAccess(name, IntegrationAccess{Slug: "dana", ConnectionID: "c1", Access: map[string]any{"blogs": []any{"M"}}, SetAt: intNow}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ArchiveAgent("dana"); err != nil {
		t.Fatal(err)
	}
	if acc, err := s.AgentIntegrationAccess("dana"); err != nil || len(acc) != 0 {
		t.Errorf("access after archive = %v, %v", acc, err)
	}
}

// Deleting the connection takes every agent's access with it (access
// is set for a connection) and hands back the records whose Secrets
// the caller deletes.
func TestDeleteIntegrationConnection(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	approvedWithConnection(t, s, "crm")
	c, err := s.GetIntegrationConnection("crm")
	if err != nil || c.ID != "c1" || c.Fields["portal"] != "acme" || c.SecretName != "kivali-connection-crm-c1" || len(c.SecretKeys) != 1 {
		t.Fatalf("connection = %+v, %v", c, err)
	}
	if err := s.SetIntegrationAccess("crm", IntegrationAccess{Slug: "dana", ConnectionID: "c1", Access: map[string]any{"blogs": []any{"M"}}}); err != nil {
		t.Fatal(err)
	}
	gone, err := s.DeleteIntegrationConnection("crm")
	if err != nil || len(gone) != 1 || gone[0].SecretName != "kivali-connection-crm-c1" {
		t.Fatalf("deleted = %v, %v", gone, err)
	}
	if _, err := s.GetIntegrationConnection("crm"); !errors.Is(err, ErrNotFound) {
		t.Errorf("connection after delete: %v", err)
	}
	if rows, _ := s.ListIntegrationAccess("crm"); len(rows) != 0 {
		t.Errorf("access after delete: %v", rows)
	}
}

// The audit tail is newest first, at most n rows, skips a line an
// append cut short, and reads back across chunk boundaries the same as
// within one, so the page shows the latest calls however long the log.
func TestIntegrationAuditTail(t *testing.T) {
	prev := auditReadChunk
	auditReadChunk = 64
	t.Cleanup(func() { auditReadChunk = prev })
	s := mustStore(t)
	storeVersion(t, s, "crm", "1.0.0")
	if rows, err := s.ReadIntegrationAuditTail("crm", 10); err != nil || len(rows) != 0 {
		t.Errorf("empty audit = %v, %v", rows, err)
	}
	for i := range 20 {
		if err := s.AppendIntegrationAudit("crm", IntegrationAuditRow{At: intNow.Add(time.Duration(i) * time.Second), Agent: "dana", Action: "create_post", Allowed: i%2 == 0, DurationMS: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ReadIntegrationAuditTail("crm", 5)
	if err != nil || len(rows) != 5 {
		t.Fatalf("tail = %v, %v", rows, err)
	}
	for i, r := range rows {
		if r.DurationMS != int64(19-i) {
			t.Errorf("row %d = %+v, want duration %d", i, r, 19-i)
		}
	}
	if all, _ := s.ReadIntegrationAuditTail("crm", 100); len(all) != 20 || all[19].DurationMS != 0 {
		t.Errorf("whole tail = %d rows", len(all))
	}
	path := filepath.Join(s.Root(), "integrations", "crm", "audit.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"at":"2026-10-21T`)
	_ = f.Close()
	if rows, _ := s.ReadIntegrationAuditTail("crm", 1); len(rows) != 1 || rows[0].DurationMS != 19 {
		t.Errorf("tail past a cut append = %v", rows)
	}
}

// An install request and its answer carry the integration and version
// through the message file. WriteMessage and parseMessage each list
// the front matter's fields, so a field added to one list only would
// be dropped on the way to disk or back, and Needs you would lose
// what the request asks to approve.
func TestMessageCarriesIntegrationInstall(t *testing.T) {
	s := mustStore(t)
	p, err := s.WriteMessage(Message{
		Type: MsgCEOApprovalRequest, Title: "Install HubSpot 1.2.0", From: "kivali-developer", To: Recipients{"ceo"},
		Date: intNow, Body: "why", IntegrationInstall: &IntegrationInstall{Name: "hubspot", Version: "1.2.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.ReadMessage(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.IntegrationInstall == nil || *m.IntegrationInstall != (IntegrationInstall{Name: "hubspot", Version: "1.2.0"}) {
		t.Errorf("read back %+v", m.IntegrationInstall)
	}
}

// integration.ValidHost duplicates ValidEgressHost so the integration
// package stays a leaf. If the two drift, a bundle's host could pass
// the check and then never match in the proxy, or the reverse.
func TestIntegrationValidHostAgreesWithEgress(t *testing.T) {
	for _, h := range []string{
		"api.hubapi.com", "*.hubspot.com", "api-[a-z].example.com", "localhost", "", "#comment",
		"https://x.com", "x.com:443", "x.com/v1", "a b", "user@x.com", "x.com?q", "x\tcom", "*", "*.*",
	} {
		if integration.ValidHost(h) != ValidEgressHost(h) {
			t.Errorf("%q: integration.ValidHost %v, ValidEgressHost %v", h, integration.ValidHost(h), ValidEgressHost(h))
		}
	}
}
