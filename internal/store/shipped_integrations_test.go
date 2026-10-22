package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
)

// shippedFS is a bundle laid out as the binary embeds one: the
// integrationtest fixture for name at version, its server's code main.
func shippedFS(t *testing.T, name, version, main string) fs.FS {
	t.Helper()
	return integrationtest.Dir(t, integrationtest.Manifest(name, version), map[string]integrationtest.File{
		"server/main.py": {Body: main},
	})
}

// readBundleFile is a file of name's stored version's bundle.
func readBundleFile(t *testing.T, s *FSStore, name, version, rel string) string {
	t.Helper()
	dir, err := s.IntegrationBundleDir(name, version)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Why this test exists: boot writes every bundle shipped in the binary
// on every start, and core runs the files it writes. A first boot must
// install it on, approved and published by Kivali, with its bundle
// where the pod mounts it; a later boot must keep the owner's switch
// (one switched off that came back on at the next restart would hand
// agents an integration the owner turned off); files that changed
// under the same version must be written again and named, or the pod
// would go on running the old code; and a new version must supersede
// the one in use as an approval does, or two versions would be in use
// at once.
func TestInstallShippedBundles(t *testing.T) {
	s := mustStore(t)
	install := func(fsys fs.FS, at time.Time) []string {
		t.Helper()
		re, err := s.InstallShippedBundles(map[string]fs.FS{"notes": fsys}, at)
		if err != nil {
			t.Fatal(err)
		}
		return re
	}
	if re := install(shippedFS(t, "notes", "1.0.0", "print('one')\n"), intNow); len(re) != 0 {
		t.Errorf("a first install rewrote %v", re)
	}
	it, err := s.GetIntegration("notes")
	if err != nil {
		t.Fatal(err)
	}
	if !it.Builtin || !it.Enabled || it.CurrentVersion != "1.0.0" || it.DisplayName != "Test notes" ||
		it.InstalledBy != BuiltinIntegrationPublisher || !it.InstalledAt.Equal(intNow) {
		t.Errorf("record = %+v", it)
	}
	v, m, err := s.CurrentIntegrationVersion("notes")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != IntegrationVersionApproved || v.PublishedBy != BuiltinIntegrationPublisher || v.NotesMD != BuiltinIntegrationNotes ||
		v.ApprovedAt == nil || !v.ApprovedAt.Equal(intNow) || v.FileCount != 2 || v.BundleBytes == 0 || m.Server.Type != "python" {
		t.Errorf("version = %+v, manifest = %+v", v, m)
	}
	if got := readBundleFile(t, s, "notes", "1.0.0", "server/main.py"); got != "print('one')\n" {
		t.Errorf("bundle's server = %q", got)
	}
	if got := readBundleFile(t, s, "notes", "1.0.0", integration.ManifestName); !strings.Contains(got, `"name": "notes"`) {
		t.Errorf("bundle's manifest = %q", got)
	}

	// The owner switches it off; the same binary boots again.
	if err := s.SetIntegrationEnabled("notes", false); err != nil {
		t.Fatal(err)
	}
	later := intNow.Add(time.Hour)
	if re := install(shippedFS(t, "notes", "1.0.0", "print('one')\n"), later); len(re) != 0 {
		t.Errorf("the same files were rewritten: %v", re)
	}
	if it, _ := s.GetIntegration("notes"); it.Enabled || !it.InstalledAt.Equal(intNow) {
		t.Errorf("after a second boot = %+v", it)
	}

	// The same version with other files: written again, and named.
	if re := install(shippedFS(t, "notes", "1.0.0", "print('two')\n"), later); strings.Join(re, ",") != "notes" {
		t.Errorf("rewritten = %v", re)
	}
	if got := readBundleFile(t, s, "notes", "1.0.0", "server/main.py"); got != "print('two')\n" {
		t.Errorf("bundle's server after a rewrite = %q", got)
	}
	// A file the binary no longer has is gone with the old bundle.
	extra := integrationtest.Dir(t, integrationtest.Manifest("notes", "1.0.0"), map[string]integrationtest.File{"server/extra.py": {Body: "x = 1\n"}})
	install(extra, later)
	if re := install(shippedFS(t, "notes", "1.0.0", "print('two')\n"), later); len(re) != 1 {
		t.Errorf("dropping a file rewrote %v", re)
	}
	dir, _ := s.IntegrationBundleDir("notes", "1.0.0")
	if _, err := os.Stat(filepath.Join(dir, "server", "extra.py")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file the binary dropped is still there: %v", err)
	}
	if v, _, _ := s.GetIntegrationVersion("notes", "1.0.0"); v.FileCount != 2 {
		t.Errorf("file count after the rewrite = %d", v.FileCount)
	}

	// A binary with a new version: the old one is superseded, the
	// owner's switch stays, and nothing is named, since a new version
	// is a new pod anyway.
	if re := install(shippedFS(t, "notes", "1.1.0", "print('three')\n"), later); len(re) != 0 {
		t.Errorf("a new version was named rewritten: %v", re)
	}
	it, _ = s.GetIntegration("notes")
	if it.CurrentVersion != "1.1.0" || it.Enabled {
		t.Errorf("record after a new version = %+v", it)
	}
	vs, _ := s.IntegrationVersions("notes")
	if len(vs) != 2 || vs[0].State != IntegrationVersionApproved || vs[1].State != IntegrationVersionSuperseded {
		t.Errorf("versions = %+v", vs)
	}
	if got := readBundleFile(t, s, "notes", "1.1.0", "server/main.py"); got != "print('three')\n" {
		t.Errorf("new version's server = %q", got)
	}

	// An older binary makes its version the one in use again.
	install(shippedFS(t, "notes", "1.0.0", "print('two')\n"), later)
	vs, _ = s.IntegrationVersions("notes")
	if it, _ := s.GetIntegration("notes"); it.CurrentVersion != "1.0.0" || vs[0].State != IntegrationVersionSuperseded || vs[1].State != IntegrationVersionApproved {
		t.Errorf("after an older binary = %+v, %+v", it, vs)
	}
}

// Why this test exists: a shipped bundle is checked at boot as an
// upload is, and its failures are its own. One that fails the check,
// names another integration in its manifest, or has the name of an
// integration the owner uploaded must not be installed (the last
// would replace code the owner reviewed with code the owner never
// saw under that name), and must not keep the other shipped bundles
// from installing: one bad bundle would otherwise take every shipped
// integration off every team.
func TestInstallShippedBundlesRefusesOneAndInstallsTheRest(t *testing.T) {
	s := mustStore(t)
	storeVersion(t, s, "crm", "1.0.0")
	bad := integrationtest.Manifest("broken", "1.0.0")
	bad["kivali"].(map[string]any)["hosts"] = []any{}
	_, err := s.InstallShippedBundles(map[string]fs.FS{
		"crm":     shippedFS(t, "crm", "2.0.0", "print('shipped')\n"),
		"broken":  integrationtest.Dir(t, bad, nil),
		"misname": shippedFS(t, "other", "1.0.0", "print('x')\n"),
		"notes":   shippedFS(t, "notes", "1.0.0", "print('ok')\n"),
	}, intNow)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"shipped integration crm:", "shipped integration broken: the bundle did not pass the check", "shipped integration misname:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if !errors.Is(err, ErrIntegrationNameMismatch) {
		t.Errorf("error %v is not a name mismatch", err)
	}
	if it, _ := s.GetIntegration("crm"); it.Builtin || it.CurrentVersion != "" {
		t.Errorf("the uploaded integration = %+v", it)
	}
	if vs, _ := s.IntegrationVersions("crm"); len(vs) != 1 || vs[0].Version != "1.0.0" {
		t.Errorf("the uploaded integration's versions = %+v", vs)
	}
	for _, name := range []string{"broken", "misname", "other"} {
		if _, err := s.GetIntegration(name); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s was installed: %v", name, err)
		}
	}
	if it, err := s.GetIntegration("notes"); err != nil || !it.Builtin || it.CurrentVersion != "1.0.0" {
		t.Errorf("notes = %+v, %v", it, err)
	}
}

// Why this test exists: a shipped bundle is built in. An upload under
// its name, an approve, a reject, a roll back or a removal that went
// through would make the store disagree with the files the binary
// carries, until the next boot put them back.
func TestShippedBundleRefusesTheOwnersMoves(t *testing.T) {
	s := mustStore(t)
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if _, err := s.InstallShippedBundles(map[string]fs.FS{"notes": shippedFS(t, "notes", v, "print(1)\n")}, intNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.StoreIntegrationVersion("notes", testBundle(t, "notes", "2.0.0"), IntegrationVersion{PublishedBy: "ceo", PublishedAt: intNow}); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("upload: %v", err)
	}
	if _, err := s.RollBackIntegrationVersion("notes", "1.0.0", intNow); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("roll back: %v", err)
	}
	if err := s.RejectIntegrationVersion("notes", "1.1.0", intNow); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("reject: %v", err)
	}
	if err := s.RemoveIntegration("notes"); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("remove: %v", err)
	}
	if err := s.RemoveStaleBuiltinIntegration("notes"); err != nil {
		t.Errorf("remove a stale one: %v", err)
	}
}
