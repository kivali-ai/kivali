package integration

import "testing"

// The server's environment is the manifest's env with each connection
// field put in, and an empty string for a field with no value. A
// placeholder left in would reach the server as literal text, and a
// value put in under the wrong key would hand one field's secret to
// another variable.
func TestServerEnvPutsInTheConnectionsValues(t *testing.T) {
	m := &Manifest{Server: Server{MCPConfig: MCPConfig{Env: map[string]string{
		"TOKEN":  "${user_config.token}",
		"URL":    "https://${user_config.portal}.example.com/${user_config.portal}",
		"PLAIN":  "fixed",
		"ABSENT": "${user_config.region}",
	}}}}
	got := ServerEnv(m, map[string]string{"token": "s3cret", "portal": "acme"})
	want := map[string]string{"TOKEN": "s3cret", "URL": "https://acme.example.com/acme", "PLAIN": "fixed", "ABSENT": ""}
	if len(got) != len(want) {
		t.Fatalf("env = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if env := ServerEnv(nil, nil); len(env) != 0 {
		t.Errorf("no manifest: %v", env)
	}
}
