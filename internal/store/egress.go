package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EgressAllowlist is the hostname-pattern set the egress proxy holds
// the agent shells to: what a command in an agent's agent-shell
// container (curl, pip, git, apt) may reach, together with that agent's
// own hosts (Agent.NetworkHosts, egress_network.go). A request that
// presents an agent runtime's credential (the agent's own CLI and its
// subagents, see internal/egresscred) is not held to it, and neither is
// an exempt agent's (Agent.NetworkExempt). Each pattern covers
// itself and everything below it — "pypi.org" allows the apex and
// every subdomain, "files.pypi.org" allows that subtree only. A
// leading "*." is accepted and means the same as the bare form; IP
// literals match exactly. A pattern holding "*" or "[" past that is a
// per-label glob (cmd/egress-proxy, matchGlob). Every list holds
// permanentEgressPatterns. Kept order-independent — we sort on write
// so diffs in the YAML file stay readable.
type EgressAllowlist struct {
	Patterns []string `yaml:"patterns"`
}

// egressAllowlistFilename is the file under DataDir the Kivali server
// writes. The proxy sidecar reads the copy WriteEgressAllowlist and
// SyncEgressAllowlistToProxy mirror to the sync path on a shared
// volume, and hot-reloads it on mtime change.
const egressAllowlistFilename = "allowed_egress.yaml"

// permanentEgressPatterns are the hosts Kivali itself needs an agent
// shell to reach, so every list holds them: deb.debian.org, where the
// software an owner installs into agents' shells comes from (apt-get
// runs in the shell container at start, through the proxy without a
// credential; see Dockerfile.agent-shell). A fresh install's list is
// exactly these. ReadEgressAllowlist and WriteEgressAllowlist put them
// back into any list that lacks them, so no edit, hand-written file or
// restored backup removes them. Every other host is the owner's to add
// in Org → Network: the proxy refuses an unlisted host with its cause,
// and the agent tells the owner.
//
// No model provider host is seeded. A shell never calls the model (the
// agent's CLI does, with its runtime's credential), and a provider's
// API is a way out for data: a key planted in the shell could upload
// files to its holder's own account. The same goes for any host anyone
// can hold an account on, such as GitHub or PyPI, so none is seeded.
var permanentEgressPatterns = []string{"deb.debian.org"}

// PermanentEgressPattern reports whether p is a host every list holds.
func PermanentEgressPattern(p string) bool {
	return slices.Contains(permanentEgressPatterns, p)
}

// withPermanent returns patterns plus the permanent hosts, normalized.
// The caller's slice is left as it is.
func withPermanent(patterns []string) []string {
	all := make([]string, 0, len(patterns)+len(permanentEgressPatterns))
	all = append(all, patterns...)
	all = append(all, permanentEgressPatterns...)
	return normalizePatterns(all)
}

// ReadEgressAllowlist returns the current allowlist, the permanent
// hosts included. If the file does not yet exist, the default is
// seeded to disk and returned — the proxy watcher picks it up on its
// next poll.
func (s *FSStore) ReadEgressAllowlist() (EgressAllowlist, error) {
	path := s.path(egressAllowlistFilename)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		al := EgressAllowlist{Patterns: withPermanent(nil)}
		if werr := s.WriteEgressAllowlist(al); werr != nil {
			return al, werr
		}
		return al, nil
	}
	if err != nil {
		return EgressAllowlist{}, err
	}
	var al EgressAllowlist
	if err := yaml.Unmarshal(b, &al); err != nil {
		return EgressAllowlist{}, err
	}
	al.Patterns = withPermanent(al.Patterns)
	return al, nil
}

// WriteEgressAllowlist persists the allowlist atomically, with the
// permanent hosts added. Patterns are normalized (trim, lowercase,
// dedup) and sorted so the YAML file diffs remain readable across
// edits. If a sync path is configured, a second copy is written there
// so the egress-proxy sidecar can hot-reload.
func (s *FSStore) WriteEgressAllowlist(al EgressAllowlist) error {
	cleaned := withPermanent(al.Patterns)
	b, err := yaml.Marshal(EgressAllowlist{Patterns: cleaned})
	if err != nil {
		return err
	}
	path := s.path(egressAllowlistFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(path, b, 0o644); err != nil {
		return err
	}
	if s.egressSyncPath != "" {
		if err := os.MkdirAll(filepath.Dir(s.egressSyncPath), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(s.egressSyncPath, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// SetEgressSyncPath configures a secondary path (outside DataDir)
// where WriteEgressAllowlist also mirrors the file — typically a
// shared volume read by the egress-proxy sidecar. Calling with an
// empty path disables sync.
func (s *FSStore) SetEgressSyncPath(p string) {
	s.egressSyncPath = p
}

// SyncEgressAllowlistToProxy writes the current on-disk allowlist to
// the configured sync path. Used at startup so a fresh Pod populates
// the egress-proxy's emptyDir before agent traffic starts.
func (s *FSStore) SyncEgressAllowlistToProxy() error {
	if s.egressSyncPath == "" {
		return nil
	}
	al, err := s.ReadEgressAllowlist()
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(al)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.egressSyncPath), 0o755); err != nil {
		return err
	}
	return writeAtomic(s.egressSyncPath, b, 0o644)
}

// normalizePatterns trims, lowercases, dedups, and sorts the pattern
// list. Empty strings and comment lines (leading '#') are dropped.
func normalizePatterns(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
