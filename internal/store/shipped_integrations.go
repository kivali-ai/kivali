package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/integration"
)

// Shipped bundles (builtinintegrations.ShippedBundle) are integrations
// that ship with Kivali as bundles embedded in the binary. Boot writes
// each one here as it writes a built-in, and with its bundle, so core
// runs it in a pod of its own like an uploaded bundle:
//
//	integration.yaml                 builtin: true; installed_by kivali
//	versions/<version>/version.yaml  approved, published by kivali
//	versions/<version>/manifest.json the binary's manifest, byte for byte
//	versions/<version>/bundle/       the binary's files

// InstallShippedBundles writes every integration that ships with
// Kivali as a bundle, its files by its name, at now. It is idempotent,
// and runs at every boot and after a restore:
//
//   - The bundle is read and checked as an upload's is
//     (integration.ReadDirectory, integration.Validate). One that does
//     not read, does not pass the check or whose manifest names
//     another integration is not installed, and the error names it;
//     the others are installed.
//   - One not installed yet is created switched on, installed by
//     kivali. One already installed keeps the owner's switch.
//   - The manifest's version is stored approved, published by kivali,
//     with the bundle unpacked beside it. A version already stored
//     whose files differ from the binary's has its manifest and bundle
//     written again: the binary is the truth.
//   - When the version differs from the one in use, the one in use is
//     superseded and the binary's becomes it, as an approval does.
//
// An installed integration of the same name that is not built in is
// left as it is, and the shipped one is not installed; the error says
// so.
//
// rewritten names the integrations whose version in use kept its
// number but had its files written again: a pod already running that
// version runs the files from before, and must be replaced
// (the web registry's Renew).
func (s *FSStore) InstallShippedBundles(bundles map[string]fs.FS, now time.Time) (rewritten []string, err error) {
	names := make([]string, 0, len(bundles))
	for name := range bundles {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		re, err := s.installShippedBundle(name, bundles[name], now.UTC())
		if err != nil {
			errs = append(errs, fmt.Errorf("shipped integration %s: %w", name, err))
			continue
		}
		if re {
			rewritten = append(rewritten, name)
		}
	}
	return rewritten, errors.Join(errs...)
}

func (s *FSStore) installShippedBundle(name string, fsys fs.FS, at time.Time) (bool, error) {
	if fsys == nil {
		return false, errors.New("the binary holds no files for it")
	}
	b, err := integration.ReadDirectory(fsys)
	if err != nil {
		return false, err
	}
	m := b.Manifest
	if m.Name != name {
		return false, fmt.Errorf("its manifest names %s: %w", m.Name, ErrIntegrationNameMismatch)
	}
	findings := integration.Validate(m, b.Files)
	if findings.HasErrors() {
		return false, fmt.Errorf("the bundle did not pass the check: %s", strings.Join(findings.Texts(), "; "))
	}
	vdir, err := s.integrationVersionDir(name, m.Version)
	if err != nil {
		return false, err
	}
	unlock := s.lockIntegration(name)
	defer unlock()

	it, err := s.GetIntegration(name)
	switch {
	case errors.Is(err, ErrNotFound):
		it = Integration{
			Name:        name,
			Enabled:     true,
			Builtin:     true,
			InstalledAt: at,
			InstalledBy: BuiltinIntegrationPublisher,
		}
	case err != nil:
		return false, err
	case !it.Builtin:
		return false, errors.New("an installed integration that does not ship with Kivali has its name")
	}
	before := it

	rewritten := false
	var v IntegrationVersion
	err = readYAMLFile(filepath.Join(vdir, "version.yaml"), &v)
	switch {
	case errors.Is(err, ErrNotFound):
		// The version goes first: a crash before the record lands
		// leaves a version nothing lists, which the next boot finds
		// and keeps.
		if err := s.storeShippedVersion(vdir, b, IntegrationVersion{
			Version:     m.Version,
			State:       IntegrationVersionApproved,
			NotesMD:     BuiltinIntegrationNotes,
			PublishedBy: BuiltinIntegrationPublisher,
			PublishedAt: at,
			ApprovedAt:  &at,
			BundleBytes: b.Size,
			FileCount:   len(b.Files),
			Findings:    findings.Warnings(),
		}); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		same, err := shippedFilesMatch(vdir, b, fsys)
		if err != nil {
			return false, err
		}
		if !same {
			if err := rewriteShippedBundle(vdir, b); err != nil {
				return false, err
			}
			rewritten = before.CurrentVersion == m.Version
		}
		next := v
		next.Version = m.Version
		next.BundleBytes, next.FileCount, next.Findings = b.Size, len(b.Files), findings.Warnings()
		if v.State != IntegrationVersionApproved {
			// Superseded by a later binary's, and this binary's again.
			next.State, next.ApprovedAt = IntegrationVersionApproved, &at
		}
		if next.Version != v.Version || next.State != v.State || next.BundleBytes != v.BundleBytes ||
			next.FileCount != v.FileCount || !slices.Equal(next.Findings, v.Findings) {
			if err := s.writeIntegrationVersion(name, next); err != nil {
				return false, err
			}
		}
	}
	if prev := it.CurrentVersion; prev != "" && prev != m.Version {
		if pv, _, err := s.GetIntegrationVersion(name, prev); err == nil && pv.State == IntegrationVersionApproved {
			pv.State = IntegrationVersionSuperseded
			if err := s.writeIntegrationVersion(name, pv); err != nil {
				return false, err
			}
		}
	}
	it.CurrentVersion, it.DisplayName = m.Version, m.DisplayName
	if it == before {
		return rewritten, nil
	}
	return rewritten, s.writeIntegration(it)
}

// storeShippedVersion writes a version whole into vdir, as an upload's
// is stored: its manifest, its bundle and rec, staged beside the other
// versions and renamed into place. A directory already at vdir with no
// version.yaml (a write cut short) is replaced.
func (s *FSStore) storeShippedVersion(vdir string, b *integration.Bundle, rec IntegrationVersion) (err error) {
	versions := filepath.Dir(vdir)
	if err := os.MkdirAll(versions, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(versions, ".stage-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := writeAtomic(filepath.Join(stage, integration.ManifestName), b.ManifestBytes(), 0o644); err != nil {
		return err
	}
	if err := b.Extract(filepath.Join(stage, "bundle")); err != nil {
		return err
	}
	if err := writeYAMLFile(filepath.Join(stage, "version.yaml"), rec); err != nil {
		return err
	}
	if err := os.RemoveAll(vdir); err != nil {
		return err
	}
	if err := os.Rename(stage, vdir); err != nil {
		return err
	}
	return syncDir(versions)
}

// shippedFilesMatch reports whether the version stored in vdir holds
// exactly b's files, fsys's bytes: its manifest.json, and a bundle
// directory with every file of b, the same bytes, and no other.
func shippedFilesMatch(vdir string, b *integration.Bundle, fsys fs.FS) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(vdir, integration.ManifestName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !bytes.Equal(raw, b.ManifestBytes()):
		return false, nil
	}
	dir := filepath.Join(vdir, "bundle")
	for _, f := range b.Files {
		want, err := fs.ReadFile(fsys, f.Path)
		if err != nil {
			return false, fmt.Errorf("the binary's %s: %w", f.Path, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil || !bytes.Equal(got, want) {
			return false, nil
		}
	}
	n := 0
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
		case d.Type().IsRegular():
			n++
		default:
			n = -1
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, nil
	}
	return n == len(b.Files), nil
}

// rewriteShippedBundle writes b's manifest and bundle into the version
// stored in vdir, in place of what is there: the new bundle is
// unpacked beside the old and renamed into its place.
func rewriteShippedBundle(vdir string, b *integration.Bundle) (err error) {
	stage, err := os.MkdirTemp(vdir, ".bundle-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := b.Extract(stage); err != nil {
		return err
	}
	bundle := filepath.Join(vdir, "bundle")
	trash, err := os.MkdirTemp(vdir, ".trash-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(trash) }()
	if err := os.Rename(bundle, filepath.Join(trash, "bundle")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(stage, bundle); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(vdir, integration.ManifestName), b.ManifestBytes(), 0o644); err != nil {
		return err
	}
	return syncDir(vdir)
}
