package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kivali-ai/kivali/internal/integration"
)

// Built-in integrations (internal/builtinintegrations) live in the
// binary. Boot writes each one's record and its version here, so the
// Org page, the access rows and the audit treat it as any integration:
//
//	integration.yaml                 builtin: true; installed_by kivali
//	versions/<version>/version.yaml  approved, published by kivali
//	versions/<version>/manifest.json the binary's manifest
//
// A built-in version has no bundle/ directory: nothing runs from the
// data directory. An integration that ships as a bundle has one
// (InstallShippedBundles, shipped_integrations.go).

// BuiltinIntegrationPublisher is who installs and publishes a built-in
// integration's versions: Kivali itself.
const BuiltinIntegrationPublisher = "kivali"

// BuiltinIntegrationNotes is a built-in version's notes.
const BuiltinIntegrationNotes = "Ships with Kivali."

// InstallBuiltinIntegrations writes every built-in integration the
// binary carries, by its manifest, at now. It is idempotent, and runs
// at every boot and after a restore:
//
//   - A built-in not installed yet is created switched on, installed by
//     kivali. One already installed keeps the owner's switch.
//   - The manifest's version is stored approved, published by kivali,
//     with no bundle, and its manifest.json is written again whenever it
//     differs from the binary's: the binary is the truth.
//   - When the version differs from the one in use, the one in use is
//     superseded and the binary's becomes it, as an approval does.
//
// An installed integration of the same name that is not built in is
// left as it is, and the built-in is not installed; the error says so.
func (s *FSStore) InstallBuiltinIntegrations(manifests []*integration.Manifest, now time.Time) error {
	var errs []error
	for _, m := range manifests {
		if m == nil {
			continue
		}
		if err := s.installBuiltinIntegration(m, now.UTC()); err != nil {
			errs = append(errs, fmt.Errorf("built-in integration %s: %w", m.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *FSStore) installBuiltinIntegration(m *integration.Manifest, at time.Time) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	vdir, err := s.integrationVersionDir(m.Name, m.Version)
	if err != nil {
		return err
	}
	unlock := s.lockIntegration(m.Name)
	defer unlock()

	it, err := s.GetIntegration(m.Name)
	switch {
	case errors.Is(err, ErrNotFound):
		it = Integration{
			Name:        m.Name,
			Enabled:     true,
			Builtin:     true,
			InstalledAt: at,
			InstalledBy: BuiltinIntegrationPublisher,
		}
	case err != nil:
		return err
	case !it.Builtin:
		return errors.New("an installed integration that does not ship with Kivali has its name")
	}
	before := it

	if err := os.MkdirAll(vdir, 0o755); err != nil {
		return err
	}
	mpath := filepath.Join(vdir, integration.ManifestName)
	if old, err := os.ReadFile(mpath); err != nil || !bytes.Equal(old, raw) {
		if err := writeAtomic(mpath, raw, 0o644); err != nil {
			return err
		}
	}
	var v IntegrationVersion
	err = readYAMLFile(filepath.Join(vdir, "version.yaml"), &v)
	switch {
	case errors.Is(err, ErrNotFound):
		v = IntegrationVersion{
			Version:     m.Version,
			State:       IntegrationVersionApproved,
			NotesMD:     BuiltinIntegrationNotes,
			PublishedBy: BuiltinIntegrationPublisher,
			PublishedAt: at,
			ApprovedAt:  &at,
		}
		if err := s.writeIntegrationVersion(m.Name, v); err != nil {
			return err
		}
	case err != nil:
		return err
	case v.State != IntegrationVersionApproved:
		// Superseded by a later binary's, and this binary's again.
		v.Version, v.State, v.ApprovedAt = m.Version, IntegrationVersionApproved, &at
		if err := s.writeIntegrationVersion(m.Name, v); err != nil {
			return err
		}
	}
	// An earlier binary may have shipped this name and version as a
	// bundle: a version of Go code holds none, and its install screen
	// must not read as one that does.
	if v.FileCount != 0 || v.BundleBytes != 0 || v.BundleDigest != "" {
		v.Version, v.FileCount, v.BundleBytes, v.BundleDigest = m.Version, 0, 0, ""
		if err := s.writeIntegrationVersion(m.Name, v); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(vdir, "bundle")); err != nil {
		return err
	}
	if prev := it.CurrentVersion; prev != "" && prev != m.Version {
		if pv, _, err := s.GetIntegrationVersion(m.Name, prev); err == nil && pv.State == IntegrationVersionApproved {
			pv.State = IntegrationVersionSuperseded
			if err := s.writeIntegrationVersion(m.Name, pv); err != nil {
				return err
			}
		}
	}
	it.CurrentVersion, it.DisplayName = m.Version, m.DisplayName
	if it == before {
		return nil
	}
	return s.writeIntegration(it)
}
