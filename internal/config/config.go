// Package config loads runtime configuration from environment variables.
//
// All persistent app state (project files, messages, chat history, etc.)
// lives under DataDir. Secrets are injected via env vars and never written
// to disk.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/owner"
	"github.com/kivali-ai/kivali/internal/provider"
)

// Config is the runtime configuration for a Kivali process.
type Config struct {
	Addr    string
	DataDir string
	// OwnerEmails are the Google accounts that can sign in: the team's
	// owner. Sourced from OWNER_EMAILS (comma-sep).
	OwnerEmails []string
	SessionKey  []byte

	// GoogleClientID is the OAuth client sign-in uses. Sourced from
	// GOOGLE_OAUTH_CLIENT_ID; when that is unset it is the public
	// client Kivali ships with (auth.DefaultPublicClientID), whose
	// secret lives only in the relay at OAuthRelayURL.
	GoogleClientID string
	// googleClientIDBaked records that GoogleClientID came from the
	// build, not the environment. A secret without an id of its own is
	// a misconfiguration Validate names.
	googleClientIDBaked bool
	// GoogleClientSec, when set, makes the client this deployment's
	// own: the code exchange goes straight to the provider with the
	// secret, and OAuthRedirectURL must be the redirect URI registered
	// on that client. Sourced from GOOGLE_OAUTH_CLIENT_SECRET.
	GoogleClientSec string
	// OAuthRedirectURL is this deployment's /auth/callback as the
	// provider reaches it. Required for an own client; optional for
	// the public client, where each sign-in derives it from the
	// request. Sourced from OAUTH_REDIRECT_URL.
	OAuthRedirectURL string
	// OAuthRelayURL is the base of the public client's relay
	// (<base>/callback and <base>/token). Sourced from OAUTH_RELAY_URL,
	// default auth.DefaultRelayURL. Unused with an own client.
	OAuthRelayURL string
	// ExternalURL is the https origin other computers reach this
	// deployment at (a reverse proxy, Tailscale), or empty. With the
	// public client its host is, besides loopback, the one host a
	// sign-in's callback is derived for. Sourced from
	// KIVALI_EXTERNAL_URL; Load refuses anything but an https origin.
	ExternalURL string
	// OAuthAuthURL, OAuthTokenURL and OAuthUserInfoURL override the
	// provider's endpoints (default Google's; the token URL defaults to
	// the relay's token route for the public client). Sourced from
	// OAUTH_AUTH_URL, OAUTH_TOKEN_URL and OAUTH_USERINFO_URL.
	OAuthAuthURL     string
	OAuthTokenURL    string
	OAuthUserInfoURL string
	// OAuthJWKSURL and OAuthIssuer are where id_tokens are verified
	// against (default Google's keys and issuers). Sourced from
	// OAUTH_JWKS_URL and OAUTH_ISSUER.
	OAuthJWKSURL string
	OAuthIssuer  string
	// AgentModel and SummaryModel are the models agent turns and
	// Kivali's own summaries run on when nothing more specific is
	// pinned. Load reads AGENT_MODEL / SUMMARY_MODEL as set (empty
	// when unset); ResolveModels fills them from the provider.
	AgentModel   string
	SummaryModel string

	// Env distinguishes "dev" (default) from "prod". Destructive
	// operations (reset scripts, restore) gate on this: prod refuses.
	// Tests can also assert they aren't running against a prod
	// cluster by reading this value.
	Env string

	// DevMode is the local-only escape hatch that lets `make run`
	// boot with nothing configured: no Google OAuth client, no
	// SESSION_KEY, no allowlist. Every request is treated as coming
	// from DevUser and the auth middleware is not installed at all.
	//
	// It is deliberately hostile to accidental production use — main.go
	// refuses to start when DevMode is combined with KIVALI_ENV=prod or
	// with a non-loopback listen address. Sourced from DEV_MODE.
	DevMode bool
	// DevUser is the email every request is attributed to under
	// DevMode. Only read when DevMode is true. Sourced from DEV_USER.
	DevUser string
	// DevAllowNonLoopback disables the loopback-only guard on DevMode.
	// Exists for containerized dev setups that must bind 0.0.0.0;
	// setting it turns an unauthenticated server loose on every
	// interface, so it is opt-in and loudly logged. Sourced from
	// DEV_MODE_ALLOW_NONLOOPBACK.
	DevAllowNonLoopback bool

	// EgressAllowlistSyncPath is where the Kivali container mirrors a
	// copy of allowed_egress.yaml for the egress-proxy sidecar to
	// read. When empty, sync is disabled (useful in tests).
	EgressAllowlistSyncPath string
	// EgressProxyURL is what agent pods set HTTP_PROXY / HTTPS_PROXY
	// to so the embedded claude CLI's outbound HTTPS routes through
	// the egress proxy. Empty → no proxy env is injected.
	EgressProxyURL string

	// AgentpodNamespace is the Kubernetes namespace per-agent Pods +
	// PVCs are created in. Defaults to "kivali-dev". Production sets
	// it via the manifest's downward-API fieldRef on metadata.namespace.
	AgentpodNamespace string
	// AgentpodImage is the container image used for per-agent Pods.
	// Defaults to "kivali:dev"; production pins this to the same tag
	// as the Kivali web image so a single image rolls out web +
	// agent-pod runtime atomically.
	AgentpodImage string
	// AgentpodUDSDir is the hostPath directory holding the agent-pod-
	// facing Unix-domain socket (core.sock). Mounted into both Kivali
	// web and every agent pod scheduled on the same node. Empty
	// disables the agent-pod listener entirely — useful in tests; in
	// production the manifest sets this to "/var/run/kivali/uds".
	AgentpodUDSDir string

	// CookieSuffix is appended, after an underscore, to the session and
	// sign-in state cookie names, so servers sharing a host (cookies
	// ignore the port) keep separate sign-ins. Empty keeps the plain
	// names. Lower-case letters, digits, "_" and "-", at most 24.
	// Sourced from KIVALI_COOKIE_SUFFIX.
	CookieSuffix string
	// SeedOrgName is stored as the org's name at boot when no name is
	// stored yet. Sourced from KIVALI_SEED_ORG_NAME.
	SeedOrgName string
	// TeamKind is "work", "personal", or empty. Stored at boot when no
	// kind is stored yet; a stored kind is never overwritten. Sourced
	// from KIVALI_TEAM_KIND.
	TeamKind string
	// OwnerName is what the person the team works for wants agents to
	// call them ("Jane", "Mom"): trimmed, at most 40 characters, empty
	// for none. Stored at boot when no name is stored yet; a stored
	// name is never overwritten. Sourced from KIVALI_OWNER_NAME.
	OwnerName string
}

// cookieSuffixPattern is what KIVALI_COOKIE_SUFFIX may hold.
var cookieSuffixPattern = regexp.MustCompile(`^[a-z0-9_-]{0,24}$`)

// Load reads configuration from the process environment. The model
// settings are read as written; ResolveModels completes them once the
// provider is known.
func Load() (Config, error) {
	cfg := Config{
		Addr:        envOr("ADDR", ":8080"),
		DataDir:     envOr("DATA_DIR", "/data"),
		OwnerEmails: splitCSV(os.Getenv("OWNER_EMAILS")),
		// The public client's id is baked into the build; OAUTH_PUBLIC_CLIENT_ID
		// swaps it (to test a relay before baking, or to run another relay).
		// GOOGLE_OAUTH_CLIENT_ID is an own client and needs its secret.
		GoogleClientID:          envOr("GOOGLE_OAUTH_CLIENT_ID", envOr("OAUTH_PUBLIC_CLIENT_ID", auth.DefaultPublicClientID)),
		googleClientIDBaked:     os.Getenv("GOOGLE_OAUTH_CLIENT_ID") == "",
		GoogleClientSec:         os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		OAuthRedirectURL:        os.Getenv("OAUTH_REDIRECT_URL"),
		OAuthRelayURL:           envOr("OAUTH_RELAY_URL", auth.DefaultRelayURL),
		ExternalURL:             strings.TrimSpace(os.Getenv("KIVALI_EXTERNAL_URL")),
		OAuthAuthURL:            os.Getenv("OAUTH_AUTH_URL"),
		OAuthTokenURL:           os.Getenv("OAUTH_TOKEN_URL"),
		OAuthUserInfoURL:        os.Getenv("OAUTH_USERINFO_URL"),
		OAuthJWKSURL:            os.Getenv("OAUTH_JWKS_URL"),
		OAuthIssuer:             os.Getenv("OAUTH_ISSUER"),
		AgentModel:              os.Getenv("AGENT_MODEL"),
		SummaryModel:            os.Getenv("SUMMARY_MODEL"),
		Env:                     envOr("KIVALI_ENV", "dev"),
		DevMode:                 envBool("DEV_MODE", false),
		DevUser:                 envOr("DEV_USER", "dev@localhost"),
		DevAllowNonLoopback:     envBool("DEV_MODE_ALLOW_NONLOOPBACK", false),
		EgressAllowlistSyncPath: os.Getenv("EGRESS_ALLOWLIST_SYNC_PATH"),
		EgressProxyURL:          os.Getenv("EGRESS_PROXY_URL"),
		AgentpodNamespace:       envOr("AGENTPOD_NAMESPACE", "kivali-dev"),
		AgentpodImage:           envOr("AGENTPOD_IMAGE", "kivali:dev"),
		AgentpodUDSDir:          os.Getenv("AGENTPOD_UDS_DIR"),
		CookieSuffix:            os.Getenv("KIVALI_COOKIE_SUFFIX"),
		SeedOrgName:             strings.TrimSpace(os.Getenv("KIVALI_SEED_ORG_NAME")),
		TeamKind:                os.Getenv("KIVALI_TEAM_KIND"),
	}
	ownerName, err := owner.CleanName(os.Getenv("KIVALI_OWNER_NAME"))
	if err != nil {
		return Config{}, fmt.Errorf("KIVALI_OWNER_NAME: %w", err)
	}
	cfg.OwnerName = ownerName
	if !cookieSuffixPattern.MatchString(cfg.CookieSuffix) {
		return Config{}, fmt.Errorf("KIVALI_COOKIE_SUFFIX %q must be at most 24 of a-z, 0-9, _ and -", cfg.CookieSuffix)
	}
	if err := checkExternalURL(cfg.ExternalURL); err != nil {
		return Config{}, err
	}
	switch cfg.TeamKind {
	case "", "work", "personal":
	default:
		return Config{}, fmt.Errorf("KIVALI_TEAM_KIND %q must be work or personal", cfg.TeamKind)
	}
	if key := os.Getenv("SESSION_KEY"); key != "" {
		cfg.SessionKey = []byte(key)
	}
	return cfg, nil
}

// checkExternalURL accepts an empty KIVALI_EXTERNAL_URL or an https
// origin: https://<host>[:port], optionally with a trailing "/", and
// nothing else (no path, query, fragment or userinfo).
func checkExternalURL(v string) error {
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil || u.Opaque != "" {
		return fmt.Errorf("KIVALI_EXTERNAL_URL %q must be an https origin, https://<host>[:port], with no path, query or userinfo", v)
	}
	return nil
}

// ResolveModels completes the model settings from p: an unset one is
// the provider's default (Defaults.Agent, Defaults.Summary), a set one
// must be a model p knows, and either runs on Current — a deployment
// whose env still names a model the provider has retired runs the
// newest model of that lineage instead, the same way a per-agent pin
// does at boot. Resolved once here so every reader of the default —
// hire, chat, the CoS seed, the picker's highlight — sees the resolved
// id and none has to know a lineage exists.
func (c *Config) ResolveModels(p provider.Provider) error {
	d := p.Defaults()
	for _, m := range []struct {
		env string
		val *string
		def string
	}{
		{"AGENT_MODEL", &c.AgentModel, d.Agent},
		{"SUMMARY_MODEL", &c.SummaryModel, d.Summary},
	} {
		if *m.val == "" {
			*m.val = m.def
		}
		if _, ok := p.Resolve(*m.val); !ok {
			return fmt.Errorf("config: %s=%q is not a model the %s provider knows", m.env, *m.val, p.Name())
		}
		*m.val = p.Current(*m.val)
	}
	return nil
}

// Validate checks required runtime configuration. Every real deploy
// must supply real secrets; DevMode is the one exemption, and it buys
// that exemption by refusing to run anywhere but loopback (enforced in
// main.go). Tests that want to exercise the server in process do so via
// internal/web/web_test.go without going through Load.
func (c Config) Validate() error {
	var missing []string
	// DevMode installs no auth middleware and mints an ephemeral
	// session key at boot, so none of the auth-related vars apply.
	if c.DevMode {
		if len(missing) > 0 {
			return fmt.Errorf("config: required env vars missing: %v", missing)
		}
		return nil
	}
	// Sign-in needs a client. The public one Kivali ships with is the
	// default; a build without it needs GOOGLE_OAUTH_CLIENT_ID. An own
	// client is id plus secret plus the redirect URI registered on it;
	// the public client needs only the relay that holds its secret.
	if c.GoogleClientID == "" {
		missing = append(missing, "GOOGLE_OAUTH_CLIENT_ID (this build ships no public client id)")
	}
	switch {
	case c.GoogleClientSec != "" && c.googleClientIDBaked:
		return errors.New("config: GOOGLE_OAUTH_CLIENT_SECRET is set but GOOGLE_OAUTH_CLIENT_ID is not; an own OAuth client needs both, or neither to use the public client")
	case c.GoogleClientSec == "" && !c.googleClientIDBaked:
		// An env id without its secret would run as a public client under
		// the wrong id and fail at every exchange, silently, at sign-in
		// time. Another public client goes through OAUTH_PUBLIC_CLIENT_ID.
		return errors.New("config: GOOGLE_OAUTH_CLIENT_ID is set but GOOGLE_OAUTH_CLIENT_SECRET is not; an own OAuth client needs both. To use another public client, set OAUTH_PUBLIC_CLIENT_ID (and OAUTH_RELAY_URL) instead")
	case c.GoogleClientSec != "" && c.OAuthRedirectURL == "":
		missing = append(missing, "OAUTH_REDIRECT_URL (required with an own OAuth client: the redirect URI registered on it)")
	case c.GoogleClientSec == "" && c.OAuthRelayURL == "":
		missing = append(missing, "OAUTH_RELAY_URL (the public client's relay)")
	}
	// The relay only bounces to /auth/callback, so a public-client
	// redirect URL set for a proxy must end there.
	if c.GoogleClientSec == "" && c.OAuthRedirectURL != "" {
		if u, err := url.Parse(c.OAuthRedirectURL); err != nil || u.Path != "/auth/callback" {
			return fmt.Errorf("config: OAUTH_REDIRECT_URL %q must be this deployment's /auth/callback when using the public client; the relay returns only to that path", c.OAuthRedirectURL)
		}
	}
	if len(c.SessionKey) == 0 {
		missing = append(missing, "SESSION_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: required env vars missing: %v", missing)
	}
	// Refuse to boot into a state nobody can log in to.
	//
	// OwnerEmails is the whole sign-in list; without it every Google
	// login is refused. The value comes from the kivali-secrets Secret,
	// and a missing or empty key would otherwise lock everyone out
	// silently, discovered only at the next login. Better to fail here,
	// where the message can say what to do.
	if len(c.OwnerEmails) == 0 {
		return fmt.Errorf(
			"config: no OWNER_EMAILS — nobody could log in.\n" +
				"  Set the owner-emails key in the kivali-secrets Secret:\n" +
				"    kubectl -n <ns> patch secret kivali-secrets --type=merge \\\n" +
				"      -p '{\"stringData\":{\"owner-emails\":\"you@example.com\"}}'\n" +
				"  then restart the deployment")
	}
	return nil
}

// envOr reads k, falling back to def when it is unset or empty.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
