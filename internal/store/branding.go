package store

import (
	"errors"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/owner"
)

const (
	brandingDir      = "branding"
	brandingMetaFile = "branding.yaml"
)

// Branding holds operator-set white-label settings: the company name
// shown in the sidebar + browser title, and whether a custom favicon
// has been uploaded. Persisted at branding/branding.yaml; the favicon
// assets themselves are derived files alongside it (see
// faviconAssetMIME). A missing file means "nothing set" — callers get a
// zero Branding, never ErrNotFound, so the empty default is the natural
// fallback to the built-in "Org" / "Kivali" labels.
type Branding struct {
	CompanyName string `yaml:"company_name"`
	HasFavicon  bool   `yaml:"has_favicon"`
	// TeamKind is TeamKindWork, TeamKindPersonal, or empty when never
	// set, which reads as work.
	TeamKind string `yaml:"team_kind,omitempty"`
	// OwnerName is what the person this team works for chose to be
	// called (internal/owner), or empty when they chose nothing.
	OwnerName string `yaml:"owner_name,omitempty"`
	// OwnerNameSet is true once a name was stored or cleared, by boot
	// (KIVALI_OWNER_NAME) or in the app. Boot seeds a name only while it
	// is false, so a name the person cleared stays cleared across
	// restarts.
	OwnerNameSet bool `yaml:"owner_name_set,omitempty"`
}

// Owner is the term the org's agents and pages use for its person.
func (b Branding) Owner() owner.Term { return owner.For(b.TeamKind, b.OwnerName) }

// The kinds of team an org can be: one that runs a business, or one
// that helps a person with their life.
const (
	TeamKindWork     = "work"
	TeamKindPersonal = "personal"
)

// ValidTeamKind reports whether kind is one a Branding can store.
func ValidTeamKind(kind string) bool {
	return kind == TeamKindWork || kind == TeamKindPersonal
}

// faviconAssetMIME is the allowlist of servable favicon asset names →
// content type. The serving handler validates the requested name
// against this map rather than trusting user input, which also keeps
// path traversal out (the name is never joined as an arbitrary path).
// The set is fixed: a multi-size .ico for legacy /favicon.ico fetchers
// plus PNGs for modern + retina + Apple-touch use, all derived from one
// uploaded source PNG.
var faviconAssetMIME = map[string]string{
	"favicon.ico":  "image/x-icon",
	"icon-32.png":  "image/png",
	"icon-180.png": "image/png",
	"icon-512.png": "image/png",
}

// ReadBranding returns the current branding settings. A missing file
// yields a zero Branding (empty name, no favicon) with a nil error.
func (s *FSStore) ReadBranding() (Branding, error) {
	b, err := os.ReadFile(s.path(brandingDir, brandingMetaFile))
	if errors.Is(err, os.ErrNotExist) {
		return Branding{}, nil
	}
	if err != nil {
		return Branding{}, err
	}
	var br Branding
	if err := yaml.Unmarshal(b, &br); err != nil {
		return Branding{}, err
	}
	br.CompanyName = strings.TrimSpace(br.CompanyName)
	br.OwnerName = strings.TrimSpace(br.OwnerName)
	return br, nil
}

func (s *FSStore) writeBranding(br Branding) error {
	if err := os.MkdirAll(s.path(brandingDir), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(br)
	if err != nil {
		return err
	}
	return writeAtomic(s.path(brandingDir, brandingMetaFile), b, 0o644)
}

// updateBranding applies change to the stored branding under brandingMu,
// so each writer keeps the fields the others set.
func (s *FSStore) updateBranding(change func(*Branding)) error {
	s.brandingMu.Lock()
	defer s.brandingMu.Unlock()
	br, err := s.ReadBranding()
	if err != nil {
		return err
	}
	change(&br)
	return s.writeBranding(br)
}

// WriteCompanyName sets (or, with an empty string, clears) the company
// name while preserving the favicon flag.
func (s *FSStore) WriteCompanyName(name string) error {
	return s.updateBranding(func(br *Branding) { br.CompanyName = strings.TrimSpace(name) })
}

// WriteOwnerName sets (or, with an empty string, clears) the person's
// chosen name, preserving the rest of the branding, and records that a
// name was set (OwnerNameSet), even when it clears it. The name is
// trimmed; one over owner.MaxNameRunes or holding a control character
// is refused.
func (s *FSStore) WriteOwnerName(name string) error {
	name, err := owner.CleanName(name)
	if err != nil {
		return err
	}
	return s.updateBranding(func(br *Branding) { br.OwnerName, br.OwnerNameSet = name, true })
}

// WriteTeamKind sets the team kind, preserving the rest of the
// branding. Anything but TeamKindWork or TeamKindPersonal is refused.
func (s *FSStore) WriteTeamKind(kind string) error {
	if !ValidTeamKind(kind) {
		return errors.New("store: unknown team kind: " + kind)
	}
	return s.updateBranding(func(br *Branding) { br.TeamKind = kind })
}

// WriteFaviconAssets atomically writes the derived favicon asset set and
// flips HasFavicon true. assets is keyed by on-disk filename, which must
// be one of faviconAssetMIME's keys.
func (s *FSStore) WriteFaviconAssets(assets map[string][]byte) error {
	if err := os.MkdirAll(s.path(brandingDir), 0o755); err != nil {
		return err
	}
	for name, data := range assets {
		if _, ok := faviconAssetMIME[name]; !ok {
			return errors.New("store: unknown favicon asset name: " + name)
		}
		if err := writeAtomic(s.path(brandingDir, name), data, 0o644); err != nil {
			return err
		}
	}
	return s.updateBranding(func(br *Branding) { br.HasFavicon = true })
}

// ReadFaviconAsset returns the bytes + content type for a public asset
// name. Unknown names (and any traversal attempt) return ErrNotFound,
// as does a name that is valid but not yet written.
func (s *FSStore) ReadFaviconAsset(name string) ([]byte, string, error) {
	mime, ok := faviconAssetMIME[name]
	if !ok {
		return nil, "", ErrNotFound
	}
	b, err := os.ReadFile(s.path(brandingDir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return b, mime, nil
}

// RemoveFavicon deletes every favicon asset and clears HasFavicon. The
// company name is preserved.
func (s *FSStore) RemoveFavicon() error {
	for name := range faviconAssetMIME {
		if err := os.Remove(s.path(brandingDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.updateBranding(func(br *Branding) { br.HasFavicon = false })
}
