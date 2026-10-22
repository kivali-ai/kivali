package integration

import (
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
)

// signInManifest is the integrationtest fixture made a device
// connection that also offers browser sign-in, as a GitHub bundle's
// would be.
func signInManifest() map[string]any {
	m := integrationtest.Manifest("github", "1.0.0")
	obj(m, "kivali")["connection"] = map[string]any{
		"method":                 "device",
		"device_endpoint":        "https://github.com/login/device/code",
		"authorization_endpoint": "https://github.com/login/oauth/authorize",
		"token_endpoint":         "https://github.com/login/oauth/access_token",
		"revocation_endpoint":    nil,
		"scopes":                 []any{"repo", "read:org"},
		"pkce":                   true,
		"client":                 "operator",
		"extra_authorization_params": map[string]any{
			"allow_signup": "false",
		},
		"credential_keys": map[string]any{"access_token": "GH_TOKEN"},
	}
	return m
}

// connectionFindings is what the connection section's own check finds
// in m, without the rest of the manifest's.
func connectionFindings(t *testing.T, m map[string]any) []string {
	t.Helper()
	var f Findings
	validateConnection(parseMap(t, m), &f)
	return f.Texts()
}

// Why this test exists: a sign-in connection's manifest is the only
// place core learns where the vendor's endpoints are and what to call
// the token. A well-formed one must pass the section's check whole
// (and be read whole: every knob, null for a missing endpoint), or no
// sign-in integration could be stored once the method is offered.
func TestSignInConnectionPassesAndReadsWhole(t *testing.T) {
	m := signInManifest()
	if got := connectionFindings(t, m); len(got) != 0 {
		t.Fatalf("findings: %v", got)
	}
	c := parseMap(t, m).Kivali.Connection
	if c.DeviceEndpoint != "https://github.com/login/device/code" || c.TokenEndpoint == "" || c.RevocationEndpoint != "" ||
		!slices.Equal(c.Scopes, []string{"repo", "read:org"}) || !c.UsesPKCE() || c.Client != "operator" ||
		c.ExtraAuthorizationParams["allow_signup"] != "false" || c.CredentialKey() != "GH_TOKEN" {
		t.Errorf("connection read as %+v", c)
	}
	if got := c.Ways(); !slices.Equal(got, []string{"device", "browser"}) {
		t.Errorf("ways = %v", got)
	}
	// Loopback http is an in-process vendor's address, for tests.
	obj(m, "kivali", "connection")["token_endpoint"] = "http://127.0.0.1:43211/token"
	if got := connectionFindings(t, m); len(got) != 0 {
		t.Errorf("a loopback http endpoint: %v", got)
	}
}

// Why this test exists: the whole check, for a bundle and for a
// built-in, must take a well-formed sign-in manifest with no finding,
// or no integration that connects by signing in could be stored and
// the owner would never reach its page's Sign in.
func TestValidateTakesSignInConnections(t *testing.T) {
	if got := Validate(parseMap(t, signInManifest()), defaultFiles()).Texts(); len(got) != 0 {
		t.Errorf("bundle findings = %v", got)
	}
	m := builtinManifest()
	m.Kivali.Connection = parseMap(t, signInManifest()).Kivali.Connection
	if got := ValidateBuiltin(m).Texts(); len(got) != 0 {
		t.Errorf("built-in findings = %v", got)
	}
}

// Why this test exists: each rule of the connection section keeps a
// manifest out of the store that core could not run safely: an
// endpoint over plain http would carry the client secret and tokens in
// the clear; a missing endpoint or credential name leaves core nothing
// to call or nowhere to put the token; a user_config that declares the
// client id or secret, or the keys the tokens are kept under, would
// collide with what core stores; an extra authorization parameter
// that names one core sets would let a manifest replace the redirect
// address or the state. One case per rule, each the valid manifest
// changed in one place; want is a piece of the finding.
func TestSignInConnectionRules(t *testing.T) {
	conn := func(m map[string]any) map[string]any { return obj(m, "kivali", "connection") }
	cases := []struct {
		name   string
		change func(m map[string]any)
		want   string
	}{
		{"device without device_endpoint", func(m map[string]any) { delete(conn(m), "device_endpoint") }, "kivali.connection.device_endpoint is missing; a device connection needs it"},
		{"device without token_endpoint", func(m map[string]any) { delete(conn(m), "token_endpoint") }, "kivali.connection.token_endpoint is missing; a device connection needs it"},
		{"browser without authorization_endpoint", func(m map[string]any) {
			conn(m)["method"] = "browser"
			delete(conn(m), "authorization_endpoint")
		}, "kivali.connection.authorization_endpoint is missing; a browser connection needs it"},
		{"browser without token_endpoint", func(m map[string]any) {
			conn(m)["method"] = "browser"
			delete(conn(m), "token_endpoint")
		}, "kivali.connection.token_endpoint is missing; a browser connection needs it"},
		{"endpoint over http", func(m map[string]any) { conn(m)["token_endpoint"] = "http://github.com/token" }, `kivali.connection.token_endpoint: "http://github.com/token" must be an https:// address`},
		{"endpoint not absolute", func(m map[string]any) { conn(m)["device_endpoint"] = "/login/device/code" }, "kivali.connection.device_endpoint"},
		{"revocation endpoint over http", func(m map[string]any) { conn(m)["revocation_endpoint"] = "http://example.com/revoke" }, "kivali.connection.revocation_endpoint"},
		{"client kivali", func(m map[string]any) { conn(m)["client"] = "kivali" }, "kivali.connection.client: a client Kivali ships is not in this version of Kivali; the owner registers the app and the client is operator"},
		{"client unknown", func(m map[string]any) { conn(m)["client"] = "vendor" }, "kivali.connection.client vendor is not one Kivali knows"},
		{"credential_keys missing", func(m map[string]any) { delete(conn(m), "credential_keys") }, "kivali.connection.credential_keys.access_token is missing"},
		{"credential_keys lowercase name", func(m map[string]any) { conn(m)["credential_keys"] = map[string]any{"access_token": "gh_token"} }, `"gh_token" must be capital letters`},
		{"credential_keys other key", func(m map[string]any) {
			conn(m)["credential_keys"] = map[string]any{"access_token": "GH_TOKEN", "refresh_token": "GH_REFRESH"}
		}, "kivali.connection.credential_keys.refresh_token: only access_token can be named"},
		{"user_config declares client_id", func(m map[string]any) {
			obj(m, "user_config")["client_id"] = map[string]any{"type": "string", "title": "Client"}
		}, "user_config.client_id: Kivali adds the client id and secret"},
		{"user_config declares client_secret", func(m map[string]any) {
			obj(m, "user_config")["client_secret"] = map[string]any{"type": "string", "title": "Secret", "sensitive": true}
		}, "user_config.client_secret: Kivali adds the client id and secret"},
		{"empty scope", func(m map[string]any) { conn(m)["scopes"] = []any{"repo", " "} }, "kivali.connection.scopes[1] is empty"},
		{"extra param core sets", func(m map[string]any) {
			conn(m)["extra_authorization_params"] = map[string]any{"redirect_uri": "https://evil.example"}
		}, "kivali.connection.extra_authorization_params.redirect_uri: Kivali sets redirect_uri itself"},
		{"extra param state", func(m map[string]any) { conn(m)["extra_authorization_params"] = map[string]any{"state": "x"} }, "extra_authorization_params.state"},
		{"token with an endpoint", func(m map[string]any) {
			obj(m, "kivali")["connection"] = map[string]any{"method": "token", "token_endpoint": "https://example.com/token"}
		}, "kivali.connection.token_endpoint: a token connection has no endpoints"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := signInManifest()
			c.change(m)
			got := connectionFindings(t, m)
			if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, c.want) }) {
				t.Errorf("no finding contains %q; got %v", c.want, got)
			}
		})
	}
}

// Why this test exists: the tokens a sign-in gives are kept in the
// connection's Secret under oauth_access_token and oauth_refresh_token.
// A manifest whose form had a field of either name would let a save
// overwrite the tokens, or the page list the refresh token as a field,
// so the check refuses those keys for every method.
func TestUserConfigRefusesTheTokenKeys(t *testing.T) {
	for _, key := range []string{"oauth_access_token", "oauth_refresh_token"} {
		m := integrationtest.Manifest("crm", "1.0.0")
		obj(m, "user_config")[key] = map[string]any{"type": "string", "title": "Token", "sensitive": true}
		got := Validate(parseMap(t, m), defaultFiles()).Texts()
		if !slices.ContainsFunc(got, func(s string) bool {
			return strings.Contains(s, "user_config."+key+": Kivali keeps a sign-in's tokens under that key")
		}) {
			t.Errorf("%s: findings = %v", key, got)
		}
	}
}

// Why this test exists: the connection form of a sign-in connection is
// the manifest's fields and the two core adds, the client id (not
// secret, so the page shows it again) and the client secret (kept in
// the Secret, never shown). If the form lacked them the owner could
// not enter the app's credentials; if a token connection got them its
// form would ask for something nothing uses. A version's diff names
// them as fields it adds over a token version.
func TestConnectionFields(t *testing.T) {
	token := parseMap(t, integrationtest.Manifest("crm", "1.0.0"))
	if got := ConnectionFields(token); len(got) != len(token.UserConfig) {
		t.Errorf("a token connection's form = %+v", got)
	}
	m := parseMap(t, signInManifest())
	got := ConnectionFields(m)
	n := len(m.UserConfig)
	if len(got) != n+2 || got[n].Key != "client_id" || got[n].Sensitive || !got[n].Required || got[n].Title != "Client id" ||
		got[n+1].Key != "client_secret" || !got[n+1].Sensitive || !got[n+1].Required || got[n+1].Title != "Client secret" {
		t.Errorf("a sign-in connection's form = %+v", got)
	}
	if len(m.UserConfig) != n {
		t.Error("ConnectionFields changed the manifest's user_config")
	}
	diff := strings.Join(Diff(token, m), "\n")
	if !strings.Contains(diff, "Connection fields added: Client id, Client secret (secret)") {
		t.Errorf("diff = %s", diff)
	}
	// A browser connection that also gives a device endpoint offers
	// both, browser first.
	b := m.Kivali.Connection
	b.Method = MethodBrowser
	if w := b.Ways(); !slices.Equal(w, []string{"browser", "device"}) {
		t.Errorf("browser ways = %v", w)
	}
	b.DeviceEndpoint = ""
	if w := b.Ways(); !slices.Equal(w, []string{"browser"}) {
		t.Errorf("browser alone ways = %v", w)
	}
}
