package integration

import (
	"slices"
	"strings"
	"testing"
)

// Why this test exists: a sign-in connection's server gets the access
// token from kivali/credential, never the client secret. An env value
// that names ${user_config.client_secret} reads empty at run time, so a
// bundle built on it would fail in a way its author cannot see; and an
// env key equal to the access token's name would be overwritten by the
// token. Both are refused at the check; the client id is not secret
// and stays allowed.
func TestSignInServerEnvRules(t *testing.T) {
	m := signInManifest()
	env := obj(m, "server", "mcp_config", "env")
	env["SECRET"] = "${user_config.client_secret}"
	env["GH_TOKEN"] = "${user_config.token}"
	env["CLIENT"] = "${user_config.client_id}"
	got := Validate(parseMap(t, m), defaultFiles()).Texts()
	for _, want := range []string{
		"server.mcp_config.env.SECRET: the client secret is Kivali's; the server asks kivali/credential for the access token",
		"server.mcp_config.env.GH_TOKEN: Kivali gives the access token under that name, so the env cannot set it",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	if slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "env.CLIENT") }) {
		t.Errorf("the client id was refused: %v", got)
	}
}
