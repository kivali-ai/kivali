package config

import (
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ADDR", "DATA_DIR",
		"SESSION_KEY", "OWNER_EMAILS",
		"ANTHROPIC_API_KEY", "GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET",
		"OAUTH_REDIRECT_URL", "OAUTH_RELAY_URL", "OAUTH_AUTH_URL", "OAUTH_TOKEN_URL", "OAUTH_USERINFO_URL",
		"OAUTH_PUBLIC_CLIENT_ID", "OAUTH_JWKS_URL", "OAUTH_ISSUER",
		"AGENT_MODEL", "SUMMARY_MODEL",
		"KIVALI_ENV",
		"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"AGENTPOD_NAMESPACE", "AGENTPOD_IMAGE", "AGENTPOD_UDS_DIR",
		"DEV_MODE", "DEV_USER", "DEV_MODE_ALLOW_NONLOOPBACK",
		"KIVALI_COOKIE_SUFFIX", "KIVALI_SEED_ORG_NAME", "KIVALI_TEAM_KIND", "KIVALI_OWNER_NAME",
		"KIVALI_EXTERNAL_URL",
	} {
		t.Setenv(k, "")
	}
}

// KIVALI_EXTERNAL_URL is an https origin or nothing; anything else
// refuses to boot.
func TestLoadExternalURL(t *testing.T) {
	for v, ok := range map[string]bool{
		"":                                         true,
		"https://dana-imac.tailnet.ts.net":         true,
		"https://dana-imac.tailnet.ts.net/":        true,
		"https://kivali.example.com:8443":          true,
		"http://dana-imac.tailnet.ts.net":          false,
		"https://kivali.example.com/auth":          false,
		"https://kivali.example.com/?a=b":          false,
		"https://kivali.example.com/#x":            false,
		"https://user@kivali.example.com":          false,
		"https://":                                 false,
		"kivali.example.com":                       false,
		"https://kivali.example.com/auth/callback": false,
	} {
		t.Run(v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("KIVALI_EXTERNAL_URL", v)
			cfg, err := Load()
			if ok && (err != nil || cfg.ExternalURL != v) {
				t.Fatalf("Load = %q, %v; want %q", cfg.ExternalURL, err, v)
			}
			if !ok && err == nil {
				t.Fatalf("Load accepted KIVALI_EXTERNAL_URL=%q", v)
			}
		})
	}
}

func TestLoadCookieSuffix(t *testing.T) {
	for suffix, ok := range map[string]bool{
		"":                          true,
		"acme":                      true,
		"team_2-b":                  true,
		"abcdefghijklmnopqrstuvwx":  true,
		"abcdefghijklmnopqrstuvwxy": false,
		"Acme":                      false,
		"a b":                       false,
		"a;b":                       false,
		"a.b":                       false,
	} {
		t.Run(suffix, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("KIVALI_COOKIE_SUFFIX", suffix)
			cfg, err := Load()
			if ok && (err != nil || cfg.CookieSuffix != suffix) {
				t.Fatalf("Load = %+v, %v; want suffix %q", cfg.CookieSuffix, err, suffix)
			}
			if !ok && err == nil {
				t.Fatalf("Load accepted KIVALI_COOKIE_SUFFIX=%q", suffix)
			}
		})
	}
}

func TestLoadTeamKindAndSeedName(t *testing.T) {
	for kind, ok := range map[string]bool{"": true, "work": true, "personal": true, "Work": false, "family": false} {
		t.Run(kind, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("KIVALI_TEAM_KIND", kind)
			cfg, err := Load()
			if ok && (err != nil || cfg.TeamKind != kind) {
				t.Fatalf("Load = %q, %v; want %q", cfg.TeamKind, err, kind)
			}
			if !ok && err == nil {
				t.Fatalf("Load accepted KIVALI_TEAM_KIND=%q", kind)
			}
		})
	}
	clearEnv(t)
	t.Setenv("KIVALI_SEED_ORG_NAME", "  Plainsong  ")
	cfg, err := Load()
	if err != nil || cfg.SeedOrgName != "Plainsong" {
		t.Fatalf("SeedOrgName = %q, %v", cfg.SeedOrgName, err)
	}
}

// KIVALI_OWNER_NAME is optional, trimmed, at most 40 characters, on
// one line.
func TestLoadOwnerName(t *testing.T) {
	for in, want := range map[string]string{"": "", "  Mom  ": "Mom", strings.Repeat("x", 40): strings.Repeat("x", 40)} {
		clearEnv(t)
		t.Setenv("KIVALI_OWNER_NAME", in)
		cfg, err := Load()
		if err != nil || cfg.OwnerName != want {
			t.Errorf("KIVALI_OWNER_NAME=%q: OwnerName = %q, %v; want %q", in, cfg.OwnerName, err, want)
		}
	}
	for _, bad := range []string{strings.Repeat("x", 41), "Mom\nDad"} {
		clearEnv(t)
		t.Setenv("KIVALI_OWNER_NAME", bad)
		if _, err := Load(); err == nil {
			t.Errorf("Load accepted KIVALI_OWNER_NAME=%q", bad)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
}

func TestValidateProdMissing(t *testing.T) {
	clearEnv(t)
	cfg, _ := Load()
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should fail in prod with no secrets")
	}
}

func TestValidateProdOK(t *testing.T) {
	clearEnv(t)
	t.Setenv("OWNER_EMAILS", "owner@example.com")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "sec")
	t.Setenv("OAUTH_REDIRECT_URL", "https://example.com/auth/callback")
	t.Setenv("SESSION_KEY", "012345678901234567890123456789ab")
	cfg, _ := Load()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// OAUTH_REDIRECT_URL is required in prod — Google rejects callbacks
// without an exact-match redirect, and forgetting it means a confusing
// runtime failure instead of a clean startup error.
func TestValidateProdRequiresRedirectURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "sec")
	t.Setenv("SESSION_KEY", "012345678901234567890123456789ab")
	cfg, _ := Load()
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should fail without OAUTH_REDIRECT_URL")
	}
}

func TestEnvDefaultsToDev(t *testing.T) {
	clearEnv(t)
	cfg, _ := Load()
	if cfg.Env != "dev" {
		t.Errorf("Env default = %q, want %q", cfg.Env, "dev")
	}
}

func TestEnvHonorsOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("KIVALI_ENV", "prod")
	cfg, _ := Load()
	if cfg.Env != "prod" {
		t.Errorf("Env = %q, want %q", cfg.Env, "prod")
	}
}

func TestOwnerEmailsParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv("OWNER_EMAILS", " owner@example.com , another@example.com ,, ")
	cfg, _ := Load()
	if got, want := len(cfg.OwnerEmails), 2; got != want {
		t.Fatalf("len(OwnerEmails) = %d, want %d (got %v)", got, want, cfg.OwnerEmails)
	}
	if cfg.OwnerEmails[0] != "owner@example.com" || cfg.OwnerEmails[1] != "another@example.com" {
		t.Errorf("OwnerEmails = %v", cfg.OwnerEmails)
	}
}

func TestOwnerEmailsEmpty(t *testing.T) {
	clearEnv(t)
	cfg, _ := Load()
	if cfg.OwnerEmails != nil {
		t.Errorf("OwnerEmails default = %v, want nil", cfg.OwnerEmails)
	}
}

// AgentpodUDSDir defaults to empty (the manifest sets it). Image
// defaults to kivali:dev so out-of-cluster boots aren't a no-op for
// developers experimenting locally. Namespace defaults to kivali-dev.
func TestAgentpodDefaults(t *testing.T) {
	clearEnv(t)
	cfg, _ := Load()
	if cfg.AgentpodUDSDir != "" {
		t.Errorf("AgentpodUDSDir default = %q, want empty", cfg.AgentpodUDSDir)
	}
	if cfg.AgentpodImage != "kivali:dev" {
		t.Errorf("AgentpodImage default = %q, want kivali:dev", cfg.AgentpodImage)
	}
	if cfg.AgentpodNamespace != "kivali-dev" {
		t.Errorf("AgentpodNamespace default = %q, want kivali-dev", cfg.AgentpodNamespace)
	}
}

func TestAgentpodHonorsOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENTPOD_NAMESPACE", "kivali-team")
	t.Setenv("AGENTPOD_UDS_DIR", "/var/run/kivali/uds")
	t.Setenv("AGENTPOD_IMAGE", "kivali:v1.2.3")
	cfg, _ := Load()
	if cfg.AgentpodNamespace != "kivali-team" {
		t.Errorf("AgentpodNamespace = %q", cfg.AgentpodNamespace)
	}
	if cfg.AgentpodUDSDir != "/var/run/kivali/uds" {
		t.Errorf("AgentpodUDSDir = %q", cfg.AgentpodUDSDir)
	}
	if cfg.AgentpodImage != "kivali:v1.2.3" {
		t.Errorf("AgentpodImage = %q", cfg.AgentpodImage)
	}
}

// DEV_MODE is the only path that skips the auth-related requirements,
// so these tests pin both halves: that it actually skips them, and
// that turning it off restores every one of them. A regression in
// either direction is a security bug, not a UX bug.
func TestDevModeSkipsAuthRequirements(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEV_MODE", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.DevMode {
		t.Fatal("DevMode = false, want true")
	}
	// No Google client, no redirect URL, no session key — and yet valid.
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate under DEV_MODE = %v, want nil", err)
	}
}

func TestDevModeOffStillRequiresEverything(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEV_MODE", "false")
	cfg, _ := Load()
	if cfg.DevMode {
		t.Fatal("DevMode = true, want false")
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want an error naming the missing auth vars")
	}
	// With nothing set, the public client the build carries is the
	// default, so only the session key is missing; the id, the secret
	// and the redirect belong to an own client only.
	if !strings.Contains(err.Error(), "SESSION_KEY") {
		t.Errorf("Validate error %q missing SESSION_KEY", err)
	}
	for _, unwanted := range []string{"GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET", "OAUTH_REDIRECT_URL"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("Validate error %q names %q, which the public client does not need", err, unwanted)
		}
	}
	if cfg.GoogleClientID == "" {
		t.Error("GoogleClientID should default to the baked public client id")
	}
}

// The public client needs no secret and no registered redirect: the
// relay holds the secret, and each sign-in derives its own callback.
// OAUTH_PUBLIC_CLIENT_ID stands in for the baked id here.
func TestValidatePublicClientNeedsOnlyIDAndSessionKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("OWNER_EMAILS", "owner@example.com")
	t.Setenv("OAUTH_PUBLIC_CLIENT_ID", "public-id")
	t.Setenv("SESSION_KEY", "012345678901234567890123456789ab")
	cfg, _ := Load()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil for the public client", err)
	}
	if cfg.GoogleClientID != "public-id" || cfg.GoogleClientSec != "" {
		t.Errorf("client = %q/%q, want the public id and no secret", cfg.GoogleClientID, cfg.GoogleClientSec)
	}
	if cfg.OAuthRelayURL == "" {
		t.Error("OAuthRelayURL should default to the relay Kivali ships with")
	}
}

// An own client id without its secret would run as a mis-keyed public
// client and fail at every exchange; refused by name instead.
func TestValidateRefusesOwnIDWithoutSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("OWNER_EMAILS", "owner@example.com")
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "own-id")
	t.Setenv("SESSION_KEY", "012345678901234567890123456789ab")
	cfg, _ := Load()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_SECRET") || !strings.Contains(err.Error(), "OAUTH_PUBLIC_CLIENT_ID") {
		t.Errorf("Validate = %v, want an error naming the missing secret and the public-client override", err)
	}
}

// With the public client, a configured redirect must end at
// /auth/callback, the only path the relay returns to.
func TestValidatePublicRedirectPath(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://org.example.com/auth/callback":        true,
		"https://org.example.com/kivali/auth/callback": false,
		"https://org.example.com/":                     false,
	} {
		clearEnv(t)
		t.Setenv("OWNER_EMAILS", "owner@example.com")
		t.Setenv("OAUTH_PUBLIC_CLIENT_ID", "public-id")
		t.Setenv("OAUTH_REDIRECT_URL", raw)
		t.Setenv("SESSION_KEY", "012345678901234567890123456789ab")
		cfg, _ := Load()
		err := cfg.Validate()
		if ok && err != nil {
			t.Errorf("%s: Validate = %v, want nil", raw, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "/auth/callback")) {
			t.Errorf("%s: Validate = %v, want an error naming /auth/callback", raw, err)
		}
	}
}

// A secret without an id of its own would pair an operator's secret
// with the public client id: refused by name.
func TestValidateRefusesSecretWithoutOwnClientID(t *testing.T) {
	clearEnv(t)
	t.Setenv("OWNER_EMAILS", "owner@example.com")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "sec")
	t.Setenv("OAUTH_REDIRECT_URL", "https://example.com/auth/callback")
	t.Setenv("SESSION_KEY", "012345678901234567890123456789ab")
	cfg, _ := Load()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_SECRET") || !strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_ID") {
		t.Errorf("Validate = %v, want an error naming the secret and the missing id", err)
	}
}

// The endpoint overrides are read verbatim.
func TestOAuthEndpointOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("OAUTH_RELAY_URL", "https://relay.example/x")
	t.Setenv("OAUTH_AUTH_URL", "https://idp.example/authorize")
	t.Setenv("OAUTH_TOKEN_URL", "https://idp.example/token")
	t.Setenv("OAUTH_USERINFO_URL", "https://idp.example/userinfo")
	cfg, _ := Load()
	if cfg.OAuthRelayURL != "https://relay.example/x" || cfg.OAuthAuthURL != "https://idp.example/authorize" ||
		cfg.OAuthTokenURL != "https://idp.example/token" || cfg.OAuthUserInfoURL != "https://idp.example/userinfo" {
		t.Errorf("endpoint overrides not read: relay=%q auth=%q token=%q userinfo=%q", cfg.OAuthRelayURL, cfg.OAuthAuthURL, cfg.OAuthTokenURL, cfg.OAuthUserInfoURL)
	}
}

func TestDevModeDefaults(t *testing.T) {
	clearEnv(t)
	cfg, _ := Load()
	if cfg.DevMode {
		t.Error("DevMode defaults to true; must default to false")
	}
	if cfg.DevAllowNonLoopback {
		t.Error("DevAllowNonLoopback defaults to true; must default to false")
	}
	if cfg.DevUser != "dev@localhost" {
		t.Errorf("DevUser = %q, want dev@localhost", cfg.DevUser)
	}
}

// The lockout guard. OWNER_EMAILS is the whole sign-in list; without
// it every Google login is refused. The value comes from the Secret,
// and a missing key would land there silently, so the server refuses
// to boot instead.
func validAuthEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GOOGLE_OAUTH_CLIENT_ID", "id")
	t.Setenv("GOOGLE_OAUTH_CLIENT_SECRET", "sec")
	t.Setenv("OAUTH_REDIRECT_URL", "https://example.com/auth/callback")
	t.Setenv("SESSION_KEY", "012345678901234567890123456789ab")
}

func TestValidateRefusesWithNoWayIn(t *testing.T) {
	clearEnv(t)
	validAuthEnv(t)
	cfg, _ := Load()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate should refuse with no OWNER_EMAILS")
	}
	// The message is the whole point of the guard — an operator
	// reading pod logs has to learn what to do about it.
	if !strings.Contains(err.Error(), "owner-emails") {
		t.Errorf("error should name the Secret key to set; got %v", err)
	}
}

func TestValidateAcceptsOwnerEmails(t *testing.T) {
	clearEnv(t)
	validAuthEnv(t)
	t.Setenv("OWNER_EMAILS", "owner@example.com")
	cfg, _ := Load()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with OWNER_EMAILS set: %v", err)
	}
}

// A whitespace-only OWNER_EMAILS parses to zero entries, so it is a
// lockout wearing a disguise: the Secret key exists, the env var is
// non-empty, and nobody can log in. splitCSV drops the blanks and
// the guard has to fire on the parsed result, not the raw string.
func TestValidateRefusesBlankOwnerEmails(t *testing.T) {
	clearEnv(t)
	validAuthEnv(t)
	t.Setenv("OWNER_EMAILS", " , ,")
	cfg, _ := Load()
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should refuse when OWNER_EMAILS parses to no entries")
	}
}

// DevMode installs no auth middleware at all, so the allowlist is
// irrelevant there. `make run` must not start demanding one.
func TestValidateDevModeSkipsLockoutGuard(t *testing.T) {
	clearEnv(t)
	t.Setenv("DEV_MODE", "true")
	cfg, _ := Load()
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate under DEV_MODE: %v", err)
	}
}
