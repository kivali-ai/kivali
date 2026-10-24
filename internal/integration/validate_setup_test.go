package integration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
)

// setupManifest is the integrationtest fixture made a setup connection,
// as a GitHub built-in's would be: a flow that sends the browser to
// github.com, asks for the account and a key it uses once, and keeps
// the app's id and private key. user_config is empty, since the
// connection's values are what setup keeps.
func setupManifest() map[string]any {
	m := integrationtest.Manifest("github", "2.0.0")
	m["user_config"] = map[string]any{}
	obj(m, "server", "mcp_config")["env"] = map[string]any{"APP_ID": "${user_config.app_id}"}
	obj(m, "kivali")["connection"] = map[string]any{
		"method": "setup",
		"setup": map[string]any{
			"summary": "Creates a private GitHub App on your account and installs it on the repositories you choose.",
			"hosts":   []any{"github.com"},
			"fields": map[string]any{
				"account":             map[string]any{"words": "Your personal account, or an organization's name"},
				"personal_access_key": map[string]any{"title": "Key", "words": "From the key page", "secret": true, "setup_only": true},
			},
			"keeps": map[string]any{
				"app_id":      "The app's id",
				"private_key": "The app's private key, which Kivali signs with",
			},
		},
	}
	return m
}

// registeredManifest is the integrationtest fixture made a browser
// connection whose client Kivali registers itself: a discover address
// in place of endpoints, and no client id or secret to paste.
func registeredManifest() map[string]any {
	m := integrationtest.Manifest("zoominfo", "1.0.0")
	obj(m, "kivali")["connection"] = map[string]any{
		"method":          "browser",
		"client":          "registered",
		"discover":        "https://mcp.zoominfo.com/mcp",
		"hosts":           []any{"mcp.zoominfo.com", "okta-login.zoominfo.com"},
		"credential_keys": map[string]any{"access_token": "ZI_TOKEN"},
	}
	return m
}

// Why this test exists: a setup connection's manifest is the only place
// core learns what the flow may do, and it must pass whole (and be read
// whole, in the order declared) or no setup integration could ship. A
// bundle's setup flow is held to the same declarations as a built-in's
// and runs over MCP, so the same manifest passes as a bundle's too: a
// refusal left for bundles would keep the Kivali Developer from ever
// publishing one.
func TestSetupConnectionPassesForABuiltinAndReadsWhole(t *testing.T) {
	m := setupManifest()
	if got := builtinConnectionFindings(t, m); len(got) != 0 {
		t.Fatalf("findings: %v", got)
	}
	c := parseMap(t, m).Kivali.Connection
	s := c.Setup
	if s == nil || !strings.HasPrefix(s.Summary, "Creates a private GitHub App") || !slices.Equal(s.Hosts, []string{"github.com"}) || s.Then != "" {
		t.Fatalf("setup read as %+v", s)
	}
	if len(s.Fields) != 2 || s.Fields[0].Key != "account" || s.Fields[0].Secret || s.Fields[0].SetupOnly ||
		s.Fields[1].Key != "personal_access_key" || s.Fields[1].Title != "Key" || !s.Fields[1].Secret || !s.Fields[1].SetupOnly {
		t.Errorf("fields read as %+v", s.Fields)
	}
	if !slices.Equal(s.Keeps.Keys(), []string{"app_id", "private_key"}) || s.Keeps[1].Words != "The app's private key, which Kivali signs with" || !s.Keeps.Has("app_id") || s.Keeps.Has("account") {
		t.Errorf("keeps read as %+v", s.Keeps)
	}
	if c.SignIn() || c.OAuth() || c.ThenMethod() != "" || c.Registered() {
		t.Errorf("a setup connection that keeps the connection itself reads as signing in: %+v", c)
	}
	if got := SetupFieldTitle(s.Fields[0]); got != "Account" {
		t.Errorf("a field with no title is called %q", got)
	}
	if bundle := connectionFindings(t, m); len(bundle) != 0 {
		t.Errorf("as a bundle: %v", bundle)
	}
	// The whole check, as a bundle: no finding, so an env that names a
	// kept key passes as one naming a form field does. As a built-in (no server): no finding, and kivali.hosts is
	// required, since a setup built-in reaches its service from Kivali
	// and the install screen lists where. A keep of client_id without a
	// hand-over passes: GitHub's manifest flow mints one. The ordered
	// fields are written back as they were read.
	obj(m, "kivali", "connection", "setup", "keeps")["client_id"] = "The app's client id"
	if got := Validate(parseMap(t, m), defaultFiles()).Texts(); len(got) != 0 {
		t.Errorf("bundle findings = %v", got)
	}
	delete(m, "server")
	mb := parseMap(t, m)
	if got := ValidateBuiltin(mb).Texts(); len(got) != 0 {
		t.Errorf("built-in findings = %v", got)
	}
	delete(obj(m, "kivali"), "hosts")
	if got := ValidateBuiltin(parseMap(t, m)).Texts(); len(got) != 1 || !strings.Contains(got[0], "kivali.hosts is empty; a built-in with a setup flow names every host") {
		t.Errorf("a setup built-in without hosts: %v", got)
	}
	delete(obj(m, "kivali", "connection", "setup", "keeps"), "client_id")
	obj(m, "kivali")["hosts"] = []any{"api.example.com"}
	if !ConnectionKeySecret(mb, "app_id") || ConnectionKeySecret(mb, "account") || ConnectionKeySecret(parseMap(t, signInManifest()), "client_id") || !ConnectionKeySecret(parseMap(t, signInManifest()), "client_secret") {
		t.Error("ConnectionKeySecret: a kept key and a sensitive field are secret; a plain field and the client id are not")
	}
	out, err := json.Marshal(mb.Kivali.Connection.Setup)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"summary":"Creates a private GitHub App on your account and installs it on the repositories you choose.","hosts":["github.com"],"fields":{"account":{"words":"Your personal account, or an organization's name"},"personal_access_key":{"title":"Key","words":"From the key page","secret":true,"setup_only":true}},"keeps":{"app_id":"The app's id","client_id":"The app's client id","private_key":"The app's private key, which Kivali signs with"}}`
	if string(out) != want {
		t.Errorf("setup written as\n%s\nwant\n%s", out, want)
	}
	if got := ConnectionKeys(mb); !slices.Equal(got, []string{"app_id", "client_id", "private_key"}) {
		t.Errorf("connection keys = %v", got)
	}
	if got := ConnectionFields(mb); len(got) != 0 {
		t.Errorf("a setup connection's form = %+v", got)
	}
}

// Why this test exists: a setup flow that hands over to a sign-in
// (setup.then) is both: core holds the tokens, so the connection reads
// as OAuth and needs the sign-in's endpoints and credential name, and
// setup must keep the app's client id and secret for it, which are
// otherwise refused as keeps. A manifest that got the hand-over half
// wrong would leave core a sign-in with no app or no endpoint to run.
func TestSetupThenHandsOverToASignIn(t *testing.T) {
	m := setupManifest()
	conn := obj(m, "kivali", "connection")
	setup := obj(m, "kivali", "connection", "setup")
	setup["then"] = "browser"
	got := builtinConnectionFindings(t, m)
	for _, want := range []string{
		"kivali.connection.authorization_endpoint is missing; a browser connection needs it",
		"kivali.connection.token_endpoint is missing; a browser connection needs it",
		"kivali.connection.credential_keys.access_token is missing",
		"kivali.connection.setup.keeps.client_id is missing; a setup that hands over to a browser sign-in keeps the app's client id and secret for it",
		"kivali.connection.setup.keeps.client_secret is missing",
	} {
		if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, want) }) {
			t.Errorf("no finding contains %q; got %v", want, got)
		}
	}
	conn["authorization_endpoint"] = "https://app.hubspot.com/oauth/authorize"
	conn["token_endpoint"] = "https://api.hubapi.com/oauth/v1/token"
	conn["credential_keys"] = map[string]any{"access_token": "HUBSPOT_TOKEN"}
	obj(m, "kivali", "connection", "setup", "keeps")["client_id"] = "The app's client id"
	obj(m, "kivali", "connection", "setup", "keeps")["client_secret"] = "The app's client secret"
	if got := builtinConnectionFindings(t, m); len(got) != 0 {
		t.Fatalf("findings: %v", got)
	}
	c := parseMap(t, m).Kivali.Connection
	if c.SignIn() || !c.OAuth() || c.ThenMethod() != MethodBrowser {
		t.Errorf("a setup connection with then reads as %+v", c)
	}
	if got := c.SignInHosts(); !slices.Equal(got, []string{"api.hubapi.com", "app.hubspot.com"}) {
		t.Errorf("sign-in hosts = %v", got)
	}
	// The fixture's keeps are a Go map, written sorted.
	if got := ConnectionKeys(parseMap(t, m)); !slices.Equal(got, []string{"app_id", "client_id", "client_secret", "private_key"}) {
		t.Errorf("connection keys = %v", got)
	}
}

// Why this test exists: each rule of the setup section keeps out a
// manifest whose flow core could not hold to its declarations: a host
// with a wildcard or in another case would not match what a Go step
// names; a field or kept key with no words leaves the install screen
// nothing to show; a key both used once and kept contradicts itself; a
// kept token key would collide with core's own; user_config on a setup
// connection would draw a form nothing reads; endpoints without a
// hand-over would be sign-in settings nothing runs. A field or a keep
// may name client_id or client_secret with or without a hand-over: a
// flow may ask the owner for an app's credentials (HubSpot's fallback)
// or keep the ones a service minted (GitHub's), and the install screen
// shows either. One case per rule, each the valid manifest changed in
// one place.
func TestSetupConnectionRules(t *testing.T) {
	setup := func(m map[string]any) map[string]any { return obj(m, "kivali", "connection", "setup") }
	cases := []struct {
		name   string
		change func(m map[string]any)
		want   string
	}{
		{"no setup section", func(m map[string]any) { delete(obj(m, "kivali", "connection"), "setup") }, "kivali.connection.setup is missing; a setup connection declares what its flow may do"},
		{"no summary", func(m map[string]any) { setup(m)["summary"] = " " }, "kivali.connection.setup.summary is missing"},
		{"host wildcard", func(m map[string]any) { setup(m)["hosts"] = []any{"*"} }, "kivali.connection.setup.hosts: * would send the browser anywhere"},
		{"host pattern", func(m map[string]any) { setup(m)["hosts"] = []any{"*.github.com"} }, `kivali.connection.setup.hosts: "*.github.com" is not a host name`},
		{"host with scheme", func(m map[string]any) { setup(m)["hosts"] = []any{"https://github.com"} }, `kivali.connection.setup.hosts: "https://github.com" is not a host name`},
		{"host in capitals", func(m map[string]any) { setup(m)["hosts"] = []any{"GitHub.com"} }, "kivali.connection.setup.hosts: GitHub.com must be lowercase"},
		{"host twice", func(m map[string]any) { setup(m)["hosts"] = []any{"github.com", "github.com"} }, "kivali.connection.setup.hosts: github.com is listed twice"},
		{"field key", func(m map[string]any) {
			obj(m, "kivali", "connection", "setup", "fields")["Account-Name"] = map[string]any{"words": "x"}
		}, "kivali.connection.setup.fields.Account-Name: the key must be lowercase letters, digits and underscores"},
		{"field without words", func(m map[string]any) {
			obj(m, "kivali", "connection", "setup", "fields")["account"] = map[string]any{}
		}, "kivali.connection.setup.fields.account: words are missing"},
		{"field secret not a boolean", func(m map[string]any) {
			obj(m, "kivali", "connection", "setup", "fields")["account"] = map[string]any{"words": "x", "secret": "yes"}
		}, "kivali.connection.setup.fields.account: secret must be true or false"},
		{"field under a token key", func(m map[string]any) {
			obj(m, "kivali", "connection", "setup", "fields")["oauth_access_token"] = map[string]any{"words": "x"}
		}, "kivali.connection.setup.fields.oauth_access_token: Kivali keeps a sign-in's tokens under that key"},
		{"setup-only field also kept", func(m map[string]any) {
			obj(m, "kivali", "connection", "setup", "keeps")["personal_access_key"] = "The key"
		}, "kivali.connection.setup.fields.personal_access_key is setup_only and also kept"},
		{"keep key", func(m map[string]any) { obj(m, "kivali", "connection", "setup", "keeps")["App Id"] = "x" }, "kivali.connection.setup.keeps.App Id: the key must be lowercase letters"},
		{"keep without words", func(m map[string]any) { obj(m, "kivali", "connection", "setup", "keeps")["app_id"] = "" }, "kivali.connection.setup.keeps.app_id: words are missing"},
		{"keep under a token key", func(m map[string]any) { obj(m, "kivali", "connection", "setup", "keeps")["oauth_refresh_token"] = "x" }, "kivali.connection.setup.keeps.oauth_refresh_token: Kivali keeps a sign-in's tokens under that key"},
		{"then unknown", func(m map[string]any) { setup(m)["then"] = "token" }, "kivali.connection.setup.then token is not a sign-in Kivali runs; it is browser or device"},
		{"user_config on a setup connection", func(m map[string]any) {
			m["user_config"] = map[string]any{"token": map[string]any{"type": "string", "title": "Token"}}
		}, "user_config.token: a setup connection's fields are kivali.connection.setup.fields"},
		{"client on a setup connection", func(m map[string]any) { obj(m, "kivali", "connection")["client"] = "operator" }, "kivali.connection.client: a setup connection's app is the one setup makes"},
		{"endpoint without a hand-over", func(m map[string]any) {
			obj(m, "kivali", "connection")["token_endpoint"] = "https://api.example.com/token"
		}, "kivali.connection.token_endpoint: a setup connection has endpoints only when setup.then hands over to a sign-in"},
		{"setup on a token connection", func(m map[string]any) { obj(m, "kivali", "connection")["method"] = "token" }, "kivali.connection.setup: only a setup connection has one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := setupManifest()
			c.change(m)
			got := builtinConnectionFindings(t, m)
			if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, c.want) }) {
				t.Errorf("no finding contains %q; got %v", c.want, got)
			}
		})
	}
}

// Why this test exists: a registered client is the one sign-in the
// owner registers nothing for, so its manifest gives a discover address
// and no endpoints, and its form takes no client id or secret; what it
// does give is the hosts the discovered endpoints may be on, since the
// install screen promises where the browser is sent and core can hold
// the metadata to that only when the manifest says. A manifest that
// gave endpoints beside discover could send the sign-in somewhere other
// than the server it discovered; one with no discover gives core
// nothing to read; one with no hosts leaves the promise empty; and a
// registered device or token connection names a flow core does not run
// that way. For a bundle it is not in this version of Kivali.
func TestRegisteredClient(t *testing.T) {
	m := registeredManifest()
	if got := builtinConnectionFindings(t, m); len(got) != 0 {
		t.Fatalf("findings: %v", got)
	}
	c := parseMap(t, m).Kivali.Connection
	if !c.Registered() || !c.SignIn() || !c.OAuth() || c.Discover != "https://mcp.zoominfo.com/mcp" {
		t.Errorf("read as %+v", c)
	}
	if got := c.SignInHosts(); !slices.Equal(got, []string{"mcp.zoominfo.com", "okta-login.zoominfo.com"}) {
		t.Errorf("sign-in hosts = %v", got)
	}
	if got := ConnectionFields(parseMap(t, m)); len(got) != len(parseMap(t, m).UserConfig) {
		t.Errorf("a registered client's form adds fields: %+v", got)
	}
	bundle := connectionFindings(t, m)
	if len(bundle) != 1 || !strings.Contains(bundle[0], "kivali.connection.client registered is not in this version of Kivali for a bundle") {
		t.Errorf("as a bundle: %v", bundle)
	}
	conn := func(m map[string]any) map[string]any { return obj(m, "kivali", "connection") }
	cases := []struct {
		name   string
		change func(m map[string]any)
		want   string
	}{
		{"no discover", func(m map[string]any) { delete(conn(m), "discover") }, "kivali.connection.discover is missing; a registered client needs the address"},
		{"discover over http", func(m map[string]any) { conn(m)["discover"] = "http://mcp.zoominfo.com/mcp" }, `kivali.connection.discover: "http://mcp.zoominfo.com/mcp" must be an https:// address`},
		{"an endpoint beside discover", func(m map[string]any) {
			conn(m)["authorization_endpoint"] = "https://evil.example/authorize"
		}, "kivali.connection.authorization_endpoint: Kivali reads a registered client's endpoints from discover, so the manifest gives none"},
		{"registered device", func(m map[string]any) { conn(m)["method"] = "device" }, "kivali.connection.client registered: only a browser connection has Kivali register itself"},
		{"no hosts", func(m map[string]any) { delete(conn(m), "hosts") }, "kivali.connection.hosts is missing; a registered client names every host the discovered endpoints may be on"},
		{"host pattern", func(m map[string]any) { conn(m)["hosts"] = []any{"*.zoominfo.com"} }, `kivali.connection.hosts: "*.zoominfo.com" is not a host name`},
		{"discover on an operator client", func(m map[string]any) {
			conn(m)["client"] = "operator"
			conn(m)["authorization_endpoint"] = "https://app.example/authorize"
			conn(m)["token_endpoint"] = "https://api.example/token"
		}, "kivali.connection.discover: only a registered client has one"},
		{"hosts on an operator client", func(m map[string]any) {
			delete(conn(m), "discover")
			conn(m)["client"] = "operator"
			conn(m)["authorization_endpoint"] = "https://app.example/authorize"
			conn(m)["token_endpoint"] = "https://api.example/token"
		}, "kivali.connection.hosts: only a registered client has them"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := registeredManifest()
			c.change(m)
			got := builtinConnectionFindings(t, m)
			if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, c.want) }) {
				t.Errorf("no finding contains %q; got %v", c.want, got)
			}
		})
	}
}

// Why this test exists: the install screen of a version that would
// replace the one in use says what the new setup flow may do that the
// old one could not: a host it newly sends the browser to, a field it
// newly asks for (a secret marked), a key it newly keeps. Without these
// lines a version could widen its flow's reach and the owner would see
// only "No change".
func TestDiffNamesSetupChanges(t *testing.T) {
	from := parseMap(t, setupManifest())
	to := setupManifest()
	obj(to, "kivali", "connection", "setup")["hosts"] = []any{"github.com", "api.github.com"}
	obj(to, "kivali", "connection", "setup", "fields")["token"] = map[string]any{"words": "x", "secret": true}
	delete(obj(to, "kivali", "connection", "setup", "fields"), "account")
	obj(to, "kivali", "connection", "setup", "keeps")["installation_id"] = "Where the app is installed"
	got := strings.Join(Diff(from, parseMap(t, to)), "\n")
	for _, want := range []string{
		"Setup sends your browser to, added: api.github.com",
		"Setup asks for, added: Token (secret)",
		"Setup asks for, removed: Account",
		"Setup keeps, added: installation_id",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}
	if got := Diff(from, from); len(got) != 0 {
		t.Errorf("a manifest against itself = %v", got)
	}
}
