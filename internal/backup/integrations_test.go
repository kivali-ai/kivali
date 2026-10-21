package backup

import (
	"bytes"
	"strings"
	"testing"
)

// An installed integration as the store leaves it: one approved
// version with its manifest and unpacked bundle, a connection whose
// secret lives elsewhere, one agent's access and an audit row.
var integrationTree = map[string]string{
	"agents/alice/agent.yaml":                                  "slug: alice\nrole: Analyst\nstatus: active\n",
	"integrations/crm/integration.yaml":                        "name: crm\ndisplay_name: CRM\ncurrent_version: 1.0.0\nenabled: true\ninstalled_at: 2026-10-21T09:00:00Z\ninstalled_by: ceo\n",
	"integrations/crm/versions/1.0.0/version.yaml":             "version: 1.0.0\nstate: approved\npublished_by: ceo\npublished_at: 2026-10-21T09:00:00Z\nbundle_bytes: 40\nfile_count: 2\n",
	"integrations/crm/versions/1.0.0/manifest.json":            `{"manifest_version":"0.3","name":"crm","version":"1.0.0","kivali":{"connection":{"method":"token"},"hosts":["api.example.com"]}}`,
	"integrations/crm/versions/1.0.0/bundle/manifest.json":     `{"name":"crm"}`,
	"integrations/crm/versions/1.0.0/bundle/server/main.py":    "print('x')\n",
	"integrations/crm/connections/c1.yaml":                     "id: c1\ncreated_at: 2026-10-21T09:00:00Z\nsecret_keys: [token]\nsecret_name: kivali-connection-crm-c1\n",
	"integrations/crm/access/alice.yaml":                       "slug: alice\nconnection_id: c1\naccess: {}\nset_at: 2026-10-21T09:00:00Z\nset_by: ceo\n",
	"integrations/crm/audit.jsonl":                             `{"at":"2026-10-21T09:00:00Z","agent":"alice","action":"x","allowed":true,"duration_ms":1}` + "\n",
	"integrations/crm/versions/1.0.0/bundle/server/helper.txt": "data\n",
}

// A backup carries every integration file, so a restore brings back
// what was installed, approved and granted; the audit reads it with the
// store's readers and finds nothing wrong. If integrations/ were left
// out of the walk, a restored team would lose its integrations without
// a word.
func TestBackupCarriesIntegrations(t *testing.T) {
	src := writeTree(t, integrationTree)
	var buf bytes.Buffer
	m, err := WriteZip(src, &buf, "test")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, f := range m.Files {
		listed[f.Path] = true
	}
	for rel := range integrationTree {
		if !listed[rel] {
			t.Errorf("backup leaves out %s", rel)
		}
	}
	inv, err := Audit(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Problems) > 0 {
		t.Errorf("problems in an integration tree: %v", inv.Problems)
	}
}

// A corrupt integration record is a restore problem, named by its
// path, so the owner hears about it before the first boot rather than
// from an Integrations page that cannot load.
func TestAuditFindsCorruptIntegrationRecords(t *testing.T) {
	for rel, want := range map[string]string{
		"integrations/crm/integration.yaml":             "integrations: integration crm",
		"integrations/crm/versions/1.0.0/version.yaml":  "integrations/crm/versions: version 1.0.0",
		"integrations/crm/versions/1.0.0/manifest.json": "integrations/crm/versions/1.0.0: ",
		"integrations/crm/connections/c1.yaml":          "integrations/crm/connections: connection c1",
		"integrations/crm/access/alice.yaml":            "integrations/crm/access: access alice",
		"integrations/crm/audit.jsonl":                  "integrations/crm/audit.jsonl: line 1",
	} {
		tree := map[string]string{}
		for k, v := range integrationTree {
			tree[k] = v
		}
		tree[rel] = "{not: [valid"
		inv, err := Audit(writeTree(t, tree))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range inv.Problems {
			found = found || strings.HasPrefix(p, want)
		}
		if !found {
			t.Errorf("%s corrupt: no problem starting %q in %v", rel, want, inv.Problems)
		}
	}
}
