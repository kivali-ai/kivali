package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EgressAllowlist is the hostname-pattern set the egress proxy uses
// to gate outbound HTTPS from the Kivali pod's egress sidecar — and
// every per-agent pod's claude CLI traffic (agent pods set
// HTTP(S)_PROXY to the same sidecar). Each pattern
// covers itself and everything below it — "pypi.org" allows the apex
// and every subdomain, "files.pypi.org" allows that subtree only. A
// leading "*." is accepted and means the same as the bare form; IP
// literals match exactly. A pattern holding "*" or "[" past that is a
// per-label glob (cmd/egress-proxy, matchGlob). Kept order-independent — we sort on write
// so diffs in the YAML file stay readable.
type EgressAllowlist struct {
	Patterns []string `yaml:"patterns"`
}

// egressAllowlistFilename is the file under DataDir the Kivali server
// writes. The proxy sidecar reads the same file via a shared volume
// and hot-reloads on mtime change.
const egressAllowlistFilename = "allowed_egress.yaml"

// defaultEgressPatterns seeds a fresh install with a working baseline,
// written once when the file does not exist yet. Two groups:
//
//   - The hosts the embedded `claude` CLI calls for each way it can be
//     signed in. Anthropic (a subscription or a Console
//     account): api.anthropic.com for the Messages API,
//     platform.claude.com for OAuth refresh and session validation.
//     Amazon Bedrock: each region's bedrock-runtime (inference) and
//     bedrock (inference profiles, model checks), STS (credential
//     checks), and the IAM Identity Center portal and OIDC hosts an
//     SSO profile resolves and refreshes role credentials through.
//     Google Vertex AI: the global, regional and multi-region
//     aiplatform hosts, and the OAuth, STS and IAM credentials hosts
//     Application Default Credentials refresh through. The region
//     globs match region-shaped labels only, so no S3 bucket name
//     under amazonaws.com fits them. Microsoft Foundry: every
//     resource's <resource>.services.ai.azure.com (the "*." form
//     covers all names under services.ai.azure.com and nothing
//     outside it), and login.microsoftonline.com, where a service
//     principal gets its Entra ID token. Without these, every agent
//     pod boots signed in but the proxy denies CONNECT and the CLI
//     fails to authenticate on the first chat-turn.
//   - Common dev tooling hosts (Debian apt, PyPI, GitHub, Go module
//     proxy) that agent shell tools tend to reach for. Kept liberal
//     because the operator can prune via /settings/egress; missing
//     entries are way more disruptive than over-broad ones.
//
// Telemetry endpoints are intentionally NOT seeded — the CLI does
// fire `http-intake.logs.us5.datadoghq.com` etc, but those are
// non-functional and the operator should opt in deliberately.
var defaultEgressPatterns = []string{
	"api.anthropic.com",
	"platform.claude.com",
	"bedrock-runtime.[a-z][a-z]-*-[0-9].amazonaws.com",
	"bedrock.[a-z][a-z]-*-[0-9].amazonaws.com",
	"sts.amazonaws.com",
	"sts.[a-z][a-z]-*-[0-9].amazonaws.com",
	"portal.sso.[a-z][a-z]-*-[0-9].amazonaws.com",
	"oidc.[a-z][a-z]-*-[0-9].amazonaws.com",
	"aiplatform.googleapis.com",
	"*-aiplatform.googleapis.com",
	"aiplatform.*.rep.googleapis.com",
	"oauth2.googleapis.com",
	"sts.googleapis.com",
	"iamcredentials.googleapis.com",
	"www.googleapis.com",
	"*.services.ai.azure.com",
	"login.microsoftonline.com",
	"deb.debian.org",
	"security.debian.org",
	"*.pypi.org",
	"*.pythonhosted.org",
	"github.com",
	"*.githubusercontent.com",
	"api.github.com",
	"proxy.golang.org",
	"sum.golang.org",
}

// ReadEgressAllowlist returns the current allowlist. If the file does
// not yet exist, a default is seeded to disk and returned — the proxy
// watcher picks it up on its next poll.
func (s *FSStore) ReadEgressAllowlist() (EgressAllowlist, error) {
	path := s.path(egressAllowlistFilename)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		al := EgressAllowlist{Patterns: append([]string(nil), defaultEgressPatterns...)}
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
	return al, nil
}

// WriteEgressAllowlist persists the allowlist atomically. Patterns
// are normalized (trim, lowercase, dedup) and sorted so the YAML
// file diffs remain readable across edits. If a sync path is
// configured, a second copy is written there so the egress-proxy
// sidecar can hot-reload.
func (s *FSStore) WriteEgressAllowlist(al EgressAllowlist) error {
	cleaned := normalizePatterns(al.Patterns)
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
