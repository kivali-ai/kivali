package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/integration"
)

// Why this test exists: boot writes every built-in on every start. A
// first boot must install it on, approved and published by Kivali; a
// later boot must keep the owner's switch (a built-in switched off
// that came back on at the next restart would hand agents an
// integration the owner turned off) and rewrite a manifest edited on
// disk (the binary is the truth); and a binary with a new version must
// supersede the old one as an approval does, or two versions would be
// in use at once.
func TestInstallBuiltinIntegrations(t *testing.T) {
	s := mustStore(t)
	echo := echotest.New()
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	it, err := s.GetIntegration(echotest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !it.Builtin || !it.Enabled || it.CurrentVersion != "1.0.0" || it.DisplayName != "Echo" ||
		it.InstalledBy != BuiltinIntegrationPublisher || !it.InstalledAt.Equal(intNow) {
		t.Errorf("record = %+v", it)
	}
	v, m, err := s.CurrentIntegrationVersion(echotest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != IntegrationVersionApproved || v.PublishedBy != BuiltinIntegrationPublisher || v.NotesMD != BuiltinIntegrationNotes ||
		v.ApprovedAt == nil || !v.ApprovedAt.Equal(intNow) || m.Name != echotest.Name || len(m.UserConfig) != 2 || !m.UserConfig[0].Sensitive {
		t.Errorf("version = %+v, manifest = %+v", v, m)
	}
	vdir := filepath.Join(s.Root(), IntegrationsDirName, echotest.Name, "versions", "1.0.0")
	if _, err := os.Stat(filepath.Join(vdir, "bundle")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a built-in version has a bundle directory: %v", err)
	}

	// The owner switches it off, and the manifest on disk is edited.
	if err := s.SetIntegrationEnabled(echotest.Name, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vdir, integration.ManifestName), []byte(`{"name":"echo","version":"1.0.0","display_name":"Edited"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	later := intNow.Add(time.Hour)
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, later); err != nil {
		t.Fatal(err)
	}
	it, _ = s.GetIntegration(echotest.Name)
	if it.Enabled || !it.InstalledAt.Equal(intNow) {
		t.Errorf("after a second boot = %+v", it)
	}
	if _, m, _ := s.CurrentIntegrationVersion(echotest.Name); m.DisplayName != "Echo" || len(m.Tools) != 2 {
		t.Errorf("manifest after a second boot = %+v", m)
	}
	if v, _, _ := s.CurrentIntegrationVersion(echotest.Name); !v.ApprovedAt.Equal(intNow) {
		t.Errorf("a second boot approved the version again: %+v", v)
	}

	// A binary with a newer version supersedes the one before.
	echo.Version = "1.1.0"
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, later); err != nil {
		t.Fatal(err)
	}
	it, _ = s.GetIntegration(echotest.Name)
	if it.CurrentVersion != "1.1.0" || it.Enabled {
		t.Errorf("after a new version = %+v", it)
	}
	vs, err := s.IntegrationVersions(echotest.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].Version != "1.1.0" || vs[0].State != IntegrationVersionApproved || vs[1].State != IntegrationVersionSuperseded {
		t.Errorf("versions = %+v", vs)
	}

	// A binary that carries the older version again makes it the one in
	// use: the binary is the truth.
	echo.Version = "1.0.0"
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, later); err != nil {
		t.Fatal(err)
	}
	vs, _ = s.IntegrationVersions(echotest.Name)
	if it, _ := s.GetIntegration(echotest.Name); it.CurrentVersion != "1.0.0" || vs[0].State != IntegrationVersionSuperseded || vs[1].State != IntegrationVersionApproved {
		t.Errorf("after the older binary: %+v, versions %+v", it, vs)
	}
}

// Why this test exists: an integration the owner uploaded is the
// owner's. A built-in that took its name at boot would replace a bundle
// the owner approved with code the owner never reviewed under that
// name.
func TestInstallBuiltinIntegrationsLeavesAnUploadedOne(t *testing.T) {
	s := mustStore(t)
	storeVersion(t, s, echotest.Name, "1.0.0")
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echotest.New().Manifest()}, intNow); err == nil {
		t.Fatal("a built-in replaced an uploaded integration")
	}
	if it, _ := s.GetIntegration(echotest.Name); it.Builtin || it.CurrentVersion != "" {
		t.Errorf("record = %+v", it)
	}
}

// Why this test exists: a built-in's versions follow the binary. An
// approve, a reject or a roll back that went through would make the
// store disagree with the code that runs, until the next boot put it
// back.
func TestBuiltinIntegrationVersionsRefuseTheOwnersMoves(t *testing.T) {
	s := mustStore(t)
	echo := echotest.New()
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	echo.Version = "1.1.0"
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollBackIntegrationVersion(echotest.Name, "1.0.0", intNow); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("roll back: %v", err)
	}
	if err := s.ApproveIntegrationVersion(echotest.Name, "1.0.0", intNow); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("approve: %v", err)
	}
	if err := s.RejectIntegrationVersion(echotest.Name, "1.1.0", intNow); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("reject: %v", err)
	}
	if err := s.RemoveIntegration(echotest.Name); !errors.Is(err, ErrIntegrationBuiltin) {
		t.Errorf("remove: %v", err)
	}
	if it, _ := s.GetIntegration(echotest.Name); it.CurrentVersion != "1.1.0" {
		t.Errorf("record = %+v", it)
	}
}

// Why this test exists: a built-in record outlives the binary that
// carried it (a Kivali that dropped it, a backup restored from one that
// had it). Without a way to remove it the owner would keep an
// integration nothing runs and nothing can take away; and that way must
// not remove an integration the owner uploaded.
func TestRemoveStaleBuiltinIntegration(t *testing.T) {
	s := mustStore(t)
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echotest.New().Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	storeVersion(t, s, "crm", "1.0.0")
	if err := s.RemoveStaleBuiltinIntegration("crm"); !errors.Is(err, ErrInvalidIntegrationName) {
		t.Errorf("an uploaded one: %v", err)
	}
	if err := s.RemoveStaleBuiltinIntegration(echotest.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIntegration(echotest.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the removal: %v", err)
	}
	if _, err := s.GetIntegration("crm"); err != nil {
		t.Errorf("the uploaded one: %v", err)
	}
}

// Why this test exists: an earlier Kivali shipped some integrations as
// bundles, and a later one may carry as Go code the name and version
// it shipped. The version must then hold no bundle: the install screen
// and the page would otherwise show a built-in of Go code with a
// bundle size and files it does not have.
func TestBuiltinInstallClearsABundleShippedUnderItsVersion(t *testing.T) {
	s := mustStore(t)
	echo := echotest.New()
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	// As the earlier binary left it: the version with its bundle.
	v, _, err := s.GetIntegrationVersion(echotest.Name, echo.Version)
	if err != nil {
		t.Fatal(err)
	}
	v.FileCount, v.BundleBytes, v.BundleDigest = 2, 2048, "sha256:0000"
	if err := s.writeIntegrationVersion(echotest.Name, v); err != nil {
		t.Fatal(err)
	}
	dir, _ := s.IntegrationBundleDir(echotest.Name, echo.Version)
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server", "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	v, m, err := s.GetIntegrationVersion(echotest.Name, echo.Version)
	if err != nil {
		t.Fatal(err)
	}
	if v.FileCount != 0 || v.BundleBytes != 0 || v.BundleDigest != "" || m.Server.Type != "" {
		t.Errorf("version = %+v, manifest server = %+v", v, m.Server)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the bundle is still there: %v", err)
	}
}
