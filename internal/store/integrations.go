package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/integration"
)

// Integrations: the external services agents act on, each installed
// from a bundle (internal/integration) and kept under
// integrations/<name>/:
//
//	integration.yaml                 the record (Integration)
//	versions/<version>/version.yaml  one stored version (IntegrationVersion), with its bundle's digest
//	versions/<version>/manifest.json the bundle's manifest, byte for byte
//	versions/<version>/bundle/       the bundle, unpacked
//	connections/<id>.yaml            its connection's fields that are not secret (IntegrationConnection)
//	connections-retired/<id>.yaml    a connection set aside, as it was stored (MigrateIntegrationConnections)
//	access/<slug>.yaml               one agent's access (IntegrationAccess)
//	audit.jsonl                      one row per call (IntegrationAuditRow)
//	log.jsonl                        its log, capped (IntegrationLogRow, integration_logs.go)
//
// Every version stored is kept, its bundle with it, whatever its state,
// until the integration is removed: rolling back runs a superseded
// version's bundle again.
//
// A connection's secret fields are never here: they live in a
// Kubernetes Secret the record names (internal/secrets). Every write
// is a read-modify-write under the integration's own lock.

var (
	// ErrIntegrationVersionExists refuses storing a version already
	// stored, whatever its state.
	ErrIntegrationVersionExists = errors.New("that version of the integration is already stored")
	// ErrIntegrationBuiltin refuses replacing or removing an
	// integration that ships with Kivali, and approving, rejecting or
	// rolling back to one of its versions, which follow the binary.
	ErrIntegrationBuiltin = errors.New("the integration ships with Kivali")
	// ErrIntegrationNameMismatch refuses a bundle stored under another
	// name than its manifest's.
	ErrIntegrationNameMismatch = errors.New("the bundle's manifest names another integration")
	// ErrIntegrationVersionNotPending refuses approving or rejecting a
	// version that is not waiting for approval.
	ErrIntegrationVersionNotPending = errors.New("the version is not waiting for approval")
	// ErrIntegrationVersionNotSuperseded refuses rolling back to a
	// version that a later one did not replace in use.
	ErrIntegrationVersionNotSuperseded = errors.New("the version is not one a later version replaced")
	// ErrIntegrationNotApproved is returned where an approved version
	// is needed and there is none.
	ErrIntegrationNotApproved = errors.New("no version of the integration is approved")
	// ErrInvalidIntegrationName refuses a name that is not an
	// integration name (integration.ValidName), or a version or slug
	// that cannot name a file.
	ErrInvalidIntegrationName = errors.New("not an integration name")
	// ErrIntegrationConnectionsSeveral is an integration whose
	// connections/ holds more than one record, which
	// MigrateIntegrationConnections could not bring to one.
	ErrIntegrationConnectionsSeveral = errors.New("the integration holds more than one connection record, and Kivali could not bring it to one; see the log")
)

// IntegrationsDirName is the integrations' directory under the root.
const IntegrationsDirName = "integrations"

// Integration is integrations/<name>/integration.yaml.
type Integration struct {
	Name string `yaml:"name"`
	// DisplayName is the approved version's, or the first stored
	// version's until one is approved.
	DisplayName string `yaml:"display_name"`
	// CurrentVersion is the approved version in use; empty until one is
	// approved.
	CurrentVersion string `yaml:"current_version,omitempty"`
	// Enabled is the owner's switch; agents can use the integration only
	// while it is on.
	Enabled bool `yaml:"enabled"`
	// Builtin marks an integration that ships with Kivali: it cannot be
	// removed or replaced by an upload. An upload never sets it.
	Builtin     bool      `yaml:"builtin,omitempty"`
	InstalledAt time.Time `yaml:"installed_at"`
	// InstalledBy is the slug that stored the first version: an agent,
	// or the owner's (ceo) for an upload.
	InstalledBy string `yaml:"installed_by"`
}

// IntegrationVersionState is where a stored version stands.
type IntegrationVersionState string

const (
	// IntegrationVersionPending waits for the owner's approval.
	IntegrationVersionPending IntegrationVersionState = "pending"
	// IntegrationVersionApproved is the version in use.
	IntegrationVersionApproved IntegrationVersionState = "approved"
	// IntegrationVersionRejected was turned down; it never runs.
	IntegrationVersionRejected IntegrationVersionState = "rejected"
	// IntegrationVersionSuperseded was in use before another version
	// was approved or rolled back to; it can be rolled back to.
	IntegrationVersionSuperseded IntegrationVersionState = "superseded"
)

// IntegrationVersion is versions/<version>/version.yaml.
type IntegrationVersion struct {
	Version string                  `yaml:"version"`
	State   IntegrationVersionState `yaml:"state"`
	// NotesMD is the publisher's notes on the version, Markdown.
	NotesMD string `yaml:"notes_md,omitempty"`
	// PublishedBy is the slug that stored it: an agent, or ceo for an
	// upload.
	PublishedBy string     `yaml:"published_by"`
	PublishedAt time.Time  `yaml:"published_at"`
	ApprovedAt  *time.Time `yaml:"approved_at,omitempty"`
	RejectedAt  *time.Time `yaml:"rejected_at,omitempty"`
	BundleBytes int64      `yaml:"bundle_bytes"`
	FileCount   int        `yaml:"file_count"`
	// BundleDigest is the digest of the bundle's files as stored
	// (BundleDigest, the function), written with them; empty for a
	// version with no bundle. The pod's spec carries it, so a pod built
	// from other files is replaced, not taken on.
	BundleDigest string `yaml:"bundle_digest,omitempty"`
	// Findings are the bundle check's warnings; a bundle with an error
	// is never stored.
	Findings []string `yaml:"findings,omitempty"`
}

// IntegrationConnection is connections/<id>.yaml: the integration's
// connection, its fields that are not secret, and which secret fields
// have a value in the Kubernetes Secret SecretName. An integration has
// one connection; MigrateIntegrationConnections sets any other record
// aside.
type IntegrationConnection struct {
	ID        string    `yaml:"id"`
	CreatedAt time.Time `yaml:"created_at"`
	// Fields holds the values of the fields that are not sensitive.
	Fields map[string]string `yaml:"fields,omitempty"`
	// SecretKeys names the sensitive fields that have a value: only the
	// keys the owner's form or a setup flow's Keep wrote, never core's
	// own entries in the Secret (the scoped proxy credential's secret, a
	// sign-in's oauth_* tokens). A setup connection with a key outside
	// its setup's keeps (an app's client id and secret aside) was made
	// by another version (web.setupMisfit), so an entry core wrote here
	// would read as one.
	SecretKeys []string `yaml:"secret_keys,omitempty"`
	// SecretName is the Kubernetes Secret holding them; empty while
	// none has a value.
	SecretName string `yaml:"secret_name,omitempty"`
	// VerifiedAt, Verified and VerifyMessage are what Test connection
	// last found; unset until it has run.
	VerifiedAt    *time.Time `yaml:"verified_at,omitempty"`
	Verified      *bool      `yaml:"verified,omitempty"`
	VerifyMessage string     `yaml:"verify_message,omitempty"`
	// SignIn is the state of a connection made by signing in (a
	// manifest whose kivali.connection.method is device or browser);
	// nil for a token connection, and for a sign-in connection the
	// owner has not signed in with yet. The tokens themselves are in
	// the Secret (internal/connect's SecretKey constants), never here.
	SignIn *ConnectionSignIn `yaml:"sign_in,omitempty"`
	// SetupPending is true from the first Keep of an install's setup
	// flow until that flow is done: the connection is an install's that
	// has not finished. Flows live in memory, so one found at boot (or
	// after a restore) was left by a Kivali that stopped mid-install, and
	// is discarded as the runner discards an install that ended without
	// being done; until then the integration reads available.
	SetupPending bool `yaml:"setup_pending,omitempty"`
	// SetupTakenOver is true, with SetupPending, for a record an earlier
	// version made that the install took over: discarding it empties
	// the record and keeps every agent's access, rather than deleting
	// them.
	SetupTakenOver bool `yaml:"setup_taken_over,omitempty"`
}

// ConnectionSignIn is a sign-in connection's state.
type ConnectionSignIn struct {
	// Method is how the owner signed in: device or browser.
	Method     string    `yaml:"method"`
	SignedInAt time.Time `yaml:"signed_in_at"`
	// ExpiresAt is when the access token expires; zero when the vendor
	// gave no expiry (the token does not expire).
	ExpiresAt time.Time `yaml:"expires_at,omitempty"`
	// Lifetime is how long the vendor last said an access token lasts,
	// for a refresh whose answer says nothing; zero until a refresh
	// gave one, when ExpiresAt less SignedInAt stands for it.
	Lifetime time.Duration `yaml:"lifetime,omitempty"`
	// Scope is what the vendor granted, as it said it.
	Scope string `yaml:"scope,omitempty"`
	// Error is why the connection needs the owner to sign in again: the
	// last refresh's failure in words. Empty while the tokens work.
	Error   string    `yaml:"error,omitempty"`
	ErrorAt time.Time `yaml:"error_at,omitempty"`
}

// NeedsSignIn reports whether c, a connection made by signing in,
// needs the owner to sign in: it never has (no SignIn), its Secret
// holds no access token (hasToken false: a Secret lost or removed), or
// its last refresh was refused for good (SignIn.Error). It is the
// caller's to ask only of a sign-in connection.
func (c IntegrationConnection) NeedsSignIn(hasToken bool) bool {
	return c.SignIn == nil || !hasToken || c.SignIn.Error != ""
}

// IntegrationAccess is access/<slug>.yaml: what one agent may do
// through the integration.
type IntegrationAccess struct {
	Slug string `yaml:"slug"`
	// Access is the filled access form, field by field.
	Access map[string]any `yaml:"access"`
	SetAt  time.Time      `yaml:"set_at"`
	SetBy  string         `yaml:"set_by"`
	// Retired says why the row was set aside with the connection it was
	// set for (MigrateIntegrationConnections); a row with it grants
	// nothing until the owner sets the access again, which writes a row
	// without it.
	Retired string `yaml:"retired,omitempty"`
	// Invalid says why the row grants nothing: Retired, no approved
	// version, no connection, or access the approved version's form
	// refuses. Worked out when the row is read, never stored.
	Invalid string `yaml:"-"`
}

// AccessJSON is the row's access as JSON, the form the access form's
// checks read. A row with no fields is {}.
func (a IntegrationAccess) AccessJSON() json.RawMessage {
	if len(a.Access) == 0 {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(a.Access)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// IntegrationAuditRow is one line of audit.jsonl: one call through the
// integration.
type IntegrationAuditRow struct {
	At time.Time `json:"at"`
	// Agent is the caller's slug; ceo for the owner's own calls.
	Agent      string `json:"agent"`
	Action     string `json:"action"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	// Outside names, for a built-in's call, who outside the owner's
	// domain wrote content it returned to the agent: a message's
	// sender, a document's owner, each by address or domain
	// (builtinintegrations.Result.Outside).
	Outside []string `json:"outside,omitempty"`
}

// integrationLocks holds a *sync.Mutex per integration name.
func (s *FSStore) lockIntegration(name string) func() {
	v, _ := s.integrationLocks.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *FSStore) integrationDir(name string) (string, error) {
	if !integration.ValidName(name) {
		return "", fmt.Errorf("%q: %w", name, ErrInvalidIntegrationName)
	}
	return s.path(IntegrationsDirName, name), nil
}

func (s *FSStore) integrationVersionDir(name, version string) (string, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return "", err
	}
	if !integration.ValidVersion(version) {
		return "", fmt.Errorf("version %q: %w", version, ErrInvalidIntegrationName)
	}
	return filepath.Join(dir, "versions", version), nil
}

// readYAMLFile reads path into v; a missing file is ErrNotFound.
func readYAMLFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeYAMLFile(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, b, 0o644)
}

// ListIntegrations returns every installed integration, sorted by
// name. A record that does not parse is an error.
func (s *FSStore) ListIntegrations() ([]Integration, error) {
	entries, err := os.ReadDir(s.path(IntegrationsDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Integration
	for _, e := range entries {
		if !e.IsDir() || !integration.ValidName(e.Name()) {
			continue
		}
		it, err := s.GetIntegration(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("integration %s: %w", e.Name(), err)
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetIntegration returns one integration's record, or ErrNotFound.
func (s *FSStore) GetIntegration(name string) (Integration, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return Integration{}, ErrNotFound
	}
	var it Integration
	if err := readYAMLFile(filepath.Join(dir, "integration.yaml"), &it); err != nil {
		return Integration{}, err
	}
	it.Name = name
	return it, nil
}

func (s *FSStore) writeIntegration(it Integration) error {
	dir, err := s.integrationDir(it.Name)
	if err != nil {
		return err
	}
	return writeYAMLFile(filepath.Join(dir, "integration.yaml"), it)
}

// IntegrationVersions returns every stored version of an integration,
// newest first. A version record that does not parse is an error.
func (s *FSStore) IntegrationVersions(name string) ([]IntegrationVersion, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return nil, ErrNotFound
	}
	entries, err := os.ReadDir(filepath.Join(dir, "versions"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []IntegrationVersion
	for _, e := range entries {
		if !e.IsDir() || !integration.ValidVersion(e.Name()) {
			continue
		}
		var v IntegrationVersion
		err := readYAMLFile(filepath.Join(dir, "versions", e.Name(), "version.yaml"), &v)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("version %s: %w", e.Name(), err)
		}
		v.Version = e.Name()
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return integration.CompareVersions(out[i].Version, out[j].Version) > 0 })
	return out, nil
}

// GetIntegrationVersion returns one stored version and its manifest,
// or ErrNotFound.
func (s *FSStore) GetIntegrationVersion(name, version string) (IntegrationVersion, *integration.Manifest, error) {
	vdir, err := s.integrationVersionDir(name, version)
	if err != nil {
		return IntegrationVersion{}, nil, ErrNotFound
	}
	var v IntegrationVersion
	if err := readYAMLFile(filepath.Join(vdir, "version.yaml"), &v); err != nil {
		return IntegrationVersion{}, nil, err
	}
	v.Version = version
	raw, err := os.ReadFile(filepath.Join(vdir, integration.ManifestName))
	if err != nil {
		return IntegrationVersion{}, nil, fmt.Errorf("version %s: %w", version, err)
	}
	m, err := integration.ParseManifest(raw)
	if err != nil {
		return IntegrationVersion{}, nil, fmt.Errorf("version %s: %w", version, err)
	}
	return v, m, nil
}

// IntegrationBundleDir is the directory a stored version's bundle is
// unpacked in.
func (s *FSStore) IntegrationBundleDir(name, version string) (string, error) {
	vdir, err := s.integrationVersionDir(name, version)
	if err != nil {
		return "", err
	}
	return filepath.Join(vdir, "bundle"), nil
}

// CurrentIntegrationVersion returns the approved version in use and its
// manifest; ErrIntegrationNotApproved when none is approved.
func (s *FSStore) CurrentIntegrationVersion(name string) (IntegrationVersion, *integration.Manifest, error) {
	it, err := s.GetIntegration(name)
	if err != nil {
		return IntegrationVersion{}, nil, err
	}
	if it.CurrentVersion == "" {
		return IntegrationVersion{}, nil, ErrIntegrationNotApproved
	}
	return s.GetIntegrationVersion(name, it.CurrentVersion)
}

// StoreIntegrationVersion stores a checked bundle as a pending version
// of the integration name: its manifest byte for byte, the bundle
// unpacked, and rec, from which it keeps NotesMD, PublishedBy,
// PublishedAt and Findings. The first version creates the integration,
// switched on. A version already stored is ErrIntegrationVersionExists,
// a built-in integration ErrIntegrationBuiltin, and a bundle whose
// manifest names another integration ErrIntegrationNameMismatch.
func (s *FSStore) StoreIntegrationVersion(name string, b *integration.Bundle, rec IntegrationVersion) (err error) {
	if b == nil || b.Manifest == nil {
		return errors.New("store integration version: no bundle")
	}
	if b.Manifest.Name != name {
		return fmt.Errorf("%s is not %s: %w", b.Manifest.Name, name, ErrIntegrationNameMismatch)
	}
	version := b.Manifest.Version
	vdir, err := s.integrationVersionDir(name, version)
	if err != nil {
		return err
	}
	unlock := s.lockIntegration(name)
	defer unlock()

	it, err := s.GetIntegration(name)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if exists && it.Builtin {
		return fmt.Errorf("%s: %w", name, ErrIntegrationBuiltin)
	}
	if _, err := os.Stat(vdir); err == nil {
		return fmt.Errorf("%s %s: %w", name, version, ErrIntegrationVersionExists)
	}
	if !exists {
		// The record goes first: a crash before the version lands
		// leaves an integration with no version, which lists as
		// pending and can be removed, never a version nothing lists.
		if err := s.writeIntegration(Integration{
			Name:        name,
			DisplayName: b.Manifest.DisplayName,
			Enabled:     true,
			InstalledAt: rec.PublishedAt.UTC(),
			InstalledBy: rec.PublishedBy,
		}); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				_ = os.RemoveAll(filepath.Dir(filepath.Dir(vdir)))
			}
		}()
	}
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
	rec.Version = version
	rec.State = IntegrationVersionPending
	rec.PublishedAt = rec.PublishedAt.UTC()
	rec.ApprovedAt, rec.RejectedAt = nil, nil
	rec.BundleBytes = b.Size
	rec.FileCount = len(b.Files)
	if rec.BundleDigest, err = BundleDigest(filepath.Join(stage, "bundle")); err != nil {
		return err
	}
	if err := writeYAMLFile(filepath.Join(stage, "version.yaml"), rec); err != nil {
		return err
	}
	if err := os.Rename(stage, vdir); err != nil {
		return err
	}
	return syncDir(versions)
}

// BundleDigest is the digest of the bundle unpacked in dir: a SHA-256
// over every regular file's path, size and bytes, in path order. Two
// bundles with the same files have the same digest wherever they are.
// A missing dir is "": a version with no bundle. A symlink or special
// file is an error: no stored bundle holds one.
func BundleDigest(dir string) (string, error) {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("bundle: %s is not a regular file", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// IntegrationBundleDigest is the digest of a stored version's bundle,
// what its pod's spec carries. A version stored before digests were
// kept has none in its record: it is worked out from the files on disk
// and written then. "" for a version with no bundle.
func (s *FSStore) IntegrationBundleDigest(name, version string) (string, error) {
	vdir, err := s.integrationVersionDir(name, version)
	if err != nil {
		return "", err
	}
	unlock := s.lockIntegration(name)
	defer unlock()
	var v IntegrationVersion
	if err := readYAMLFile(filepath.Join(vdir, "version.yaml"), &v); err != nil {
		return "", err
	}
	if v.BundleDigest != "" {
		return v.BundleDigest, nil
	}
	digest, err := BundleDigest(filepath.Join(vdir, "bundle"))
	if err != nil || digest == "" {
		return "", err
	}
	v.Version, v.BundleDigest = version, digest
	if err := s.writeIntegrationVersion(name, v); err != nil {
		return "", err
	}
	return digest, nil
}

// ApproveIntegrationVersion makes a pending version the one in use:
// it becomes approved at now, the version approved before it becomes
// superseded, and the integration's current version and display name
// follow it. A version that is not pending is
// ErrIntegrationVersionNotPending.
func (s *FSStore) ApproveIntegrationVersion(name, version string, now time.Time) error {
	_, err := s.makeIntegrationVersionCurrent(name, version, IntegrationVersionPending, ErrIntegrationVersionNotPending, now)
	return err
}

// RollBackIntegrationVersion makes a superseded version the one in use
// again, as approving does: it becomes approved at now, the version in
// use becomes superseded, and the integration's current version and
// display name follow it. It returns the version it replaced. The
// bundle that runs is the one stored with it: every version is kept,
// in every state, until the integration is removed. A version that is
// not superseded is ErrIntegrationVersionNotSuperseded.
func (s *FSStore) RollBackIntegrationVersion(name, version string, now time.Time) (replaced string, err error) {
	return s.makeIntegrationVersionCurrent(name, version, IntegrationVersionSuperseded, ErrIntegrationVersionNotSuperseded, now)
}

// makeIntegrationVersionCurrent makes version, which must be in state
// from (or it is notFrom), the approved version in use, superseding
// the one in use before it, and returns that one's version ("" for
// none). A built-in integration's versions follow the binary
// (InstallBuiltinIntegrations): ErrIntegrationBuiltin.
func (s *FSStore) makeIntegrationVersionCurrent(name, version string, from IntegrationVersionState, notFrom error, now time.Time) (string, error) {
	unlock := s.lockIntegration(name)
	defer unlock()
	it, err := s.GetIntegration(name)
	if err != nil {
		return "", err
	}
	if it.Builtin {
		return "", fmt.Errorf("%s: %w", name, ErrIntegrationBuiltin)
	}
	v, m, err := s.GetIntegrationVersion(name, version)
	if err != nil {
		return "", err
	}
	if v.State != from {
		return "", fmt.Errorf("%s %s is %s: %w", name, version, v.State, notFrom)
	}
	at := now.UTC()
	v.State, v.ApprovedAt = IntegrationVersionApproved, &at
	if err := s.writeIntegrationVersion(name, v); err != nil {
		return "", err
	}
	prev := it.CurrentVersion
	if prev == version {
		prev = ""
	}
	if prev != "" {
		if pv, _, err := s.GetIntegrationVersion(name, prev); err == nil && pv.State == IntegrationVersionApproved {
			pv.State = IntegrationVersionSuperseded
			if err := s.writeIntegrationVersion(name, pv); err != nil {
				return "", err
			}
		}
	}
	it.CurrentVersion = version
	if m.DisplayName != "" {
		it.DisplayName = m.DisplayName
	}
	if err := s.writeIntegration(it); err != nil {
		return "", err
	}
	return prev, nil
}

// RejectIntegrationVersion turns a pending version down at now. A
// version that is not pending is ErrIntegrationVersionNotPending, and
// a built-in integration's ErrIntegrationBuiltin.
func (s *FSStore) RejectIntegrationVersion(name, version string, now time.Time) error {
	unlock := s.lockIntegration(name)
	defer unlock()
	it, err := s.GetIntegration(name)
	if err != nil {
		return err
	}
	if it.Builtin {
		return fmt.Errorf("%s: %w", name, ErrIntegrationBuiltin)
	}
	v, _, err := s.GetIntegrationVersion(name, version)
	if err != nil {
		return err
	}
	if v.State != IntegrationVersionPending {
		return fmt.Errorf("%s %s is %s: %w", name, version, v.State, ErrIntegrationVersionNotPending)
	}
	at := now.UTC()
	v.State, v.RejectedAt = IntegrationVersionRejected, &at
	return s.writeIntegrationVersion(name, v)
}

func (s *FSStore) writeIntegrationVersion(name string, v IntegrationVersion) error {
	vdir, err := s.integrationVersionDir(name, v.Version)
	if err != nil {
		return err
	}
	return writeYAMLFile(filepath.Join(vdir, "version.yaml"), v)
}

// SetIntegrationEnabled switches an integration on or off.
func (s *FSStore) SetIntegrationEnabled(name string, enabled bool) error {
	unlock := s.lockIntegration(name)
	defer unlock()
	it, err := s.GetIntegration(name)
	if err != nil {
		return err
	}
	it.Enabled = enabled
	return s.writeIntegration(it)
}

// RemoveIntegration deletes an integration: every version, whatever its
// state, its connections' records, every agent's access, its audit and
// its log. The connections' Secrets are the caller's to delete first.
// A built-in integration is ErrIntegrationBuiltin.
func (s *FSStore) RemoveIntegration(name string) error {
	return s.removeIntegration(name, false)
}

// RemoveStaleBuiltinIntegration deletes a built-in integration's record
// and everything under it, as RemoveIntegration does, for a built-in
// this binary does not carry: a later or earlier Kivali shipped it,
// and nothing here runs it. The caller checks that the binary has no
// code for the name; boot would write the record again if it had. An
// integration that is not built in is refused (ErrInvalidIntegrationName):
// RemoveIntegration removes those.
func (s *FSStore) RemoveStaleBuiltinIntegration(name string) error {
	return s.removeIntegration(name, true)
}

func (s *FSStore) removeIntegration(name string, staleBuiltin bool) error {
	unlock := s.lockIntegration(name)
	defer unlock()
	it, err := s.GetIntegration(name)
	if err != nil {
		return err
	}
	switch {
	case it.Builtin && !staleBuiltin:
		return fmt.Errorf("%s: %w", name, ErrIntegrationBuiltin)
	case !it.Builtin && staleBuiltin:
		return fmt.Errorf("%s is not built in: %w", name, ErrInvalidIntegrationName)
	}
	dir, _ := s.integrationDir(name)
	// Renamed aside first, so no reader sees half a removal: the
	// trash name is never a valid integration name.
	trash, err := os.MkdirTemp(s.path(IntegrationsDirName), ".trash-*")
	if err != nil {
		return err
	}
	if err := os.Rename(dir, filepath.Join(trash, name)); err != nil {
		_ = os.RemoveAll(trash)
		return err
	}
	s.integrationLogLines.Delete(name)
	return os.RemoveAll(trash)
}

// ---- connections ----

func validFileName(s string) bool {
	return s != "" && !strings.HasPrefix(s, ".") && !strings.ContainsAny(s, `/\`)
}

// ListIntegrationConnections returns every connection record under the
// integration's connections/, oldest first: its one connection, or
// none. Records from before one connection per installation are there
// until MigrateIntegrationConnections sets all but one aside. A record
// that does not parse is an error.
func (s *FSStore) ListIntegrationConnections(name string) ([]IntegrationConnection, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return nil, ErrNotFound
	}
	entries, err := os.ReadDir(filepath.Join(dir, "connections"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []IntegrationConnection
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() || !validFileName(id) {
			continue
		}
		var c IntegrationConnection
		if err := readYAMLFile(filepath.Join(dir, "connections", e.Name()), &c); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("connection %s: %w", id, err)
		}
		c.ID = id
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GetIntegrationConnection returns the integration's connection;
// ErrNotFound when it has none, and ErrIntegrationConnectionsSeveral
// when connections/ holds more than one record (a migration that could
// not finish): no record is the connection then, so nothing is called
// through the wrong one.
func (s *FSStore) GetIntegrationConnection(name string) (IntegrationConnection, error) {
	all, err := s.ListIntegrationConnections(name)
	if err != nil {
		return IntegrationConnection{}, err
	}
	switch len(all) {
	case 0:
		return IntegrationConnection{}, ErrNotFound
	case 1:
		return all[0], nil
	}
	return IntegrationConnection{}, fmt.Errorf("%s: %w", name, ErrIntegrationConnectionsSeveral)
}

// GetIntegrationConnectionByID returns the integration's connection
// when its id is id: a caller that holds an id (a setup flow's, a shell
// job's) learns whether the connection is still the one it had.
// ErrNotFound when it has none of that id.
func (s *FSStore) GetIntegrationConnectionByID(name, id string) (IntegrationConnection, error) {
	dir, err := s.integrationDir(name)
	if err != nil || !validFileName(id) {
		return IntegrationConnection{}, ErrNotFound
	}
	var c IntegrationConnection
	if err := readYAMLFile(filepath.Join(dir, "connections", id+".yaml"), &c); err != nil {
		if errors.Is(err, ErrNotFound) {
			return IntegrationConnection{}, ErrNotFound
		}
		return IntegrationConnection{}, fmt.Errorf("connection %s: %w", id, err)
	}
	c.ID = id
	return c, nil
}

// PutIntegrationConnection writes a connection's record, by its ID.
func (s *FSStore) PutIntegrationConnection(name string, c IntegrationConnection) error {
	if !validFileName(c.ID) {
		return fmt.Errorf("connection id %q: %w", c.ID, ErrInvalidIntegrationName)
	}
	unlock := s.lockIntegration(name)
	defer unlock()
	dir, err := s.integrationDir(name)
	if err != nil {
		return err
	}
	if _, err := s.GetIntegration(name); err != nil {
		return err
	}
	c.CreatedAt = c.CreatedAt.UTC()
	if c.SignIn != nil {
		si := *c.SignIn
		si.SignedInAt, si.ExpiresAt, si.ErrorAt = si.SignedInAt.UTC(), si.ExpiresAt.UTC(), si.ErrorAt.UTC()
		c.SignIn = &si
	}
	return writeYAMLFile(filepath.Join(dir, "connections", c.ID+".yaml"), c)
}

// DeleteIntegrationConnection removes the connection's record and,
// with it, every agent's access and the connections set aside
// (connections-retired/). It returns the records of connections/ it
// removed, whose Secrets are the caller's to delete, as are those of
// the ones set aside (RetiredIntegrationConnections), first.
func (s *FSStore) DeleteIntegrationConnection(name string) ([]IntegrationConnection, error) {
	unlock := s.lockIntegration(name)
	defer unlock()
	if _, err := s.GetIntegration(name); err != nil {
		return nil, err
	}
	dir, _ := s.integrationDir(name)
	gone, err := s.ListIntegrationConnections(name)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"connections", retiredConnectionsDir, "access"} {
		if err := os.RemoveAll(filepath.Join(dir, sub)); err != nil {
			return nil, err
		}
	}
	return gone, nil
}

// DeleteIntegrationConnectionByID removes the integration's connection
// when its id is id, and every agent's access with it, and returns the
// record, whose Secret is the caller's to delete, and the slugs whose
// access it cleared. A connection of another id stays, and so does the
// access. ErrNotFound when the integration has no connection of that
// id.
func (s *FSStore) DeleteIntegrationConnectionByID(name, id string) (IntegrationConnection, []string, error) {
	unlock := s.lockIntegration(name)
	defer unlock()
	if _, err := s.GetIntegration(name); err != nil {
		return IntegrationConnection{}, nil, err
	}
	c, err := s.GetIntegrationConnectionByID(name, id)
	if err != nil {
		return IntegrationConnection{}, nil, err
	}
	// The access goes first: a crash between the two leaves a
	// connection no agent uses, never a row outliving it.
	cleared, err := s.clearIntegrationAccessLocked(name)
	if err != nil {
		return IntegrationConnection{}, nil, err
	}
	dir, _ := s.integrationDir(name)
	if err := os.Remove(filepath.Join(dir, "connections", id+".yaml")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return IntegrationConnection{}, nil, err
	}
	return c, cleared, nil
}

// ---- access ----

// readIntegrationAccessRows reads every access row as it is stored, by
// file name order, with no Invalid worked out.
func (s *FSStore) readIntegrationAccessRows(name string) ([]IntegrationAccess, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return nil, ErrNotFound
	}
	entries, err := os.ReadDir(filepath.Join(dir, "access"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []IntegrationAccess
	for _, e := range entries {
		slug, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() || !validFileName(slug) {
			continue
		}
		var row IntegrationAccess
		if err := readYAMLFile(filepath.Join(dir, "access", e.Name()), &row); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("access %s: %w", slug, err)
		}
		row.Slug = slug
		rows = append(rows, row)
	}
	return rows, nil
}

// clearIntegrationAccessLocked removes every agent's access and returns
// their slugs, sorted. The caller holds the integration's lock.
func (s *FSStore) clearIntegrationAccessLocked(name string) ([]string, error) {
	rows, err := s.readIntegrationAccessRows(name)
	if err != nil {
		return nil, err
	}
	dir, _ := s.integrationDir(name)
	var cleared []string
	for _, row := range rows {
		if err := os.Remove(filepath.Join(dir, "access", row.Slug+".yaml")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return cleared, err
		}
		cleared = append(cleared, row.Slug)
	}
	sort.Strings(cleared)
	return cleared, nil
}

// SetIntegrationAccess writes one agent's access. The integration and
// the agent must exist; an agent that is not active is ErrNotFound, so
// an archive racing the write leaves no row behind (the archive clears
// the agent's rows under the same lock once the agent has moved).
func (s *FSStore) SetIntegrationAccess(name string, row IntegrationAccess) error {
	if !validFileName(row.Slug) {
		return fmt.Errorf("slug %q: %w", row.Slug, ErrInvalidIntegrationName)
	}
	unlock := s.lockIntegration(name)
	defer unlock()
	dir, err := s.integrationDir(name)
	if err != nil {
		return err
	}
	if _, err := s.GetIntegration(name); err != nil {
		return err
	}
	if _, err := s.GetAgent(row.Slug); err != nil {
		return err
	}
	if row.Access == nil {
		row.Access = map[string]any{}
	}
	row.SetAt = row.SetAt.UTC()
	return writeYAMLFile(filepath.Join(dir, "access", row.Slug+".yaml"), row)
}

// ClearIntegrationAccess removes one agent's access; silent when it
// has none.
func (s *FSStore) ClearIntegrationAccess(name, slug string) error {
	if !validFileName(slug) {
		return fmt.Errorf("slug %q: %w", slug, ErrInvalidIntegrationName)
	}
	unlock := s.lockIntegration(name)
	defer unlock()
	dir, err := s.integrationDir(name)
	if err != nil {
		return err
	}
	if _, err := s.GetIntegration(name); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, "access", slug+".yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ListIntegrationAccess returns every agent's access, by slug, each
// with Invalid set when it grants nothing: set aside with a retired
// connection (Retired), more than one connection record (a migration
// that could not finish), no version approved, no connection, or access
// the approved version's form refuses (integration.ValidateAccess). A
// record that does not parse is an error.
func (s *FSStore) ListIntegrationAccess(name string) ([]IntegrationAccess, error) {
	rows, err := s.readIntegrationAccessRows(name)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	_, m, merr := s.CurrentIntegrationVersion(name)
	conns, err := s.ListIntegrationConnections(name)
	if err != nil {
		return nil, err
	}
	several := ""
	if len(conns) > 1 {
		// A migration that could not finish: which record a row was set
		// for is not read, so no row may reach any of them.
		display := name
		if it, err := s.GetIntegration(name); err == nil && it.DisplayName != "" {
			display = it.DisplayName
		}
		several = display + " holds more than one connection record and Kivali could not bring it to one; see the log"
	}
	for i := range rows {
		rows[i].Invalid = accessInvalid(rows[i], m, merr, len(conns) > 0)
		if several != "" && rows[i].Retired == "" {
			rows[i].Invalid = several
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Slug < rows[j].Slug })
	return rows, nil
}

// accessConnectionGone is why a row set for a connection the
// integration no longer has grants nothing.
const accessConnectionGone = "it was set for a connection that is no longer there"

// accessInvalid is why row grants nothing, or "". connected is whether
// the integration has its connection.
func accessInvalid(row IntegrationAccess, m *integration.Manifest, merr error, connected bool) string {
	switch {
	case row.Retired != "":
		return row.Retired
	case merr != nil:
		return "no version of the integration is approved"
	case !connected:
		return accessConnectionGone
	}
	if err := integration.ValidateAccess(m, row.AccessJSON()); err != nil {
		return err.Error()
	}
	return ""
}

// AgentIntegrationAccess returns one agent's access across every
// integration, by integration name.
func (s *FSStore) AgentIntegrationAccess(slug string) (map[string]IntegrationAccess, error) {
	out := map[string]IntegrationAccess{}
	its, err := s.ListIntegrations()
	if err != nil {
		return nil, err
	}
	for _, it := range its {
		rows, err := s.ListIntegrationAccess(it.Name)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.Slug == slug {
				out[it.Name] = r
			}
		}
	}
	return out, nil
}

// ClearAgentIntegrationAccess removes one agent's access from every
// integration. ArchiveAgent calls it once the agent has moved, taking
// each integration's lock in turn and holding no other.
func (s *FSStore) ClearAgentIntegrationAccess(slug string) error {
	its, err := s.ListIntegrations()
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range its {
		if err := s.ClearIntegrationAccess(it.Name, slug); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", it.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ---- audit ----

// AppendIntegrationAudit appends one row to the integration's
// audit.jsonl.
func (s *FSStore) AppendIntegrationAudit(name string, row IntegrationAuditRow) error {
	unlock := s.lockIntegration(name)
	defer unlock()
	dir, err := s.integrationDir(name)
	if err != nil {
		return err
	}
	if _, err := s.GetIntegration(name); err != nil {
		return err
	}
	row.At = row.At.UTC()
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return appendJSONL(filepath.Join(dir, "audit.jsonl"), append(line, '\n'))
}

// auditReadChunk is how much tailLines reads at a time, from the end.
var auditReadChunk = 64 << 10

// ReadIntegrationAuditTail returns the integration's latest n audit
// rows, newest first. A line that does not parse (an append cut short)
// is skipped. No audit yet is no rows.
func (s *FSStore) ReadIntegrationAuditTail(name string, n int) ([]IntegrationAuditRow, error) {
	dir, err := s.integrationDir(name)
	if err != nil {
		return nil, ErrNotFound
	}
	lines, err := tailLines(filepath.Join(dir, "audit.jsonl"), n)
	if err != nil {
		return nil, err
	}
	var out []IntegrationAuditRow
	for _, line := range lines {
		if len(out) == n {
			break
		}
		var row IntegrationAuditRow
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// tailLines reads the JSONL file at path back from its end until it
// holds n whole lines, or the whole file, and returns the non-empty
// lines it read, newest first. A missing file, or n <= 0, is none.
func tailLines(path string, n int) ([][]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var buf []byte
	off := fi.Size()
	for off > 0 && bytes.Count(buf, []byte{'\n'}) <= n {
		step := int64(auditReadChunk)
		if step > off {
			step = off
		}
		off -= step
		part := make([]byte, step)
		if _, err := f.ReadAt(part, off); err != nil {
			return nil, err
		}
		buf = append(part, buf...)
	}
	lines := bytes.Split(buf, []byte{'\n'})
	if off > 0 {
		lines = lines[1:] // starts mid-line
	}
	var out [][]byte
	for i := len(lines) - 1; i >= 0; i-- {
		if line := bytes.TrimSpace(lines[i]); len(line) > 0 {
			out = append(out, line)
		}
	}
	return out, nil
}
