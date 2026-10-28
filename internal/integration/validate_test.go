package integration

import (
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/integration/integrationtest"
)

// defaultFiles are the files integrationtest.Zip writes.
func defaultFiles() []BundleFile {
	return []BundleFile{
		{Path: "manifest.json", Size: 100, Mode: 0o644},
		{Path: "server/main.py", Size: int64(len(integrationtest.MainPy)), Mode: 0o644},
	}
}

// parseMap parses a manifest map the way a bundle's bytes are parsed.
func parseMap(t *testing.T, m map[string]any) *Manifest {
	t.Helper()
	mf, err := ParseManifest(integrationtest.ManifestJSON(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

func obj(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

// The reference manifest in testdata (the brief's example, with every
// kind of access field) is what a bundle that passes looks like. If a
// rule starts refusing something it should not, or the parser drops a
// field, this fails first: a finding appears, or a field reads empty.
func TestGoldenManifestPassesAndReadsWhole(t *testing.T) {
	raw, err := os.ReadFile("testdata/valid_manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f := Validate(m, defaultFiles()); len(f) != 0 {
		t.Fatalf("findings on the golden manifest: %v", f.Texts())
	}
	if m.Name != "hubspot" || m.Version != "1.2.0" || m.Author.Name != "Kivali Developer (agent)" ||
		m.Server.MCPConfig.Env["HUBSPOT_TOKEN"] != "${user_config.token}" || len(m.Tools) != 3 ||
		m.Kivali.Verify != "whoami" || m.Kivali.Access.Choices["blogs"] != "list_blogs" {
		t.Errorf("manifest read as %+v", m)
	}
	// A tool's access is read, and a tool without one reads empty.
	if m.Tools[0].Access != "write" || m.Tools[1].Access != "" {
		t.Errorf("tools read as %+v", m.Tools)
	}
	// user_config keeps the bundle's order, which is the connection
	// form's order.
	if len(m.UserConfig) != 2 || m.UserConfig[0].Key != "token" || m.UserConfig[1].Key != "portal" ||
		!m.UserConfig[0].Sensitive || !m.UserConfig[0].Required || m.UserConfig[1].Sensitive {
		t.Errorf("user_config = %+v", m.UserConfig)
	}
}

// Every rule of the check, one case each: the manifest the
// integrationtest fixture builds, changed in one place. A rule that
// stops firing shows up as a missing finding; one that fires at the
// wrong level shows up here too. want is a piece of the finding's
// text.
func TestValidateRules(t *testing.T) {
	cases := []struct {
		name   string
		change func(m map[string]any)
		files  func([]BundleFile) []BundleFile
		level  Level
		want   string
	}{
		{"manifest_version missing", func(m map[string]any) { delete(m, "manifest_version") }, nil, LevelError, "manifest_version is missing"},
		{"manifest_version 1.x", func(m map[string]any) { m["manifest_version"] = "1.0" }, nil, LevelError, "manifest_version 1.0"},
		{"name missing", func(m map[string]any) { delete(m, "name") }, nil, LevelError, "name is missing"},
		{"name uppercase", func(m map[string]any) { m["name"] = "HubSpot" }, nil, LevelError, `name "HubSpot" must be`},
		{"name trailing hyphen", func(m map[string]any) { m["name"] = "crm-" }, nil, LevelError, "must be 1 to 32"},
		{"name too long", func(m map[string]any) { m["name"] = strings.Repeat("a", 33) }, nil, LevelError, "must be 1 to 32"},
		{"name reserved", func(m map[string]any) { m["name"] = "web" }, nil, LevelError, "Kivali's own actions start with web_"},
		{"name reserved first segment", func(m map[string]any) { m["name"] = "file-sync" }, nil, LevelError, "prefix file_sync_"},
		{"version missing", func(m map[string]any) { delete(m, "version") }, nil, LevelError, "version is missing"},
		{"version not semver", func(m map[string]any) { m["version"] = "1.2" }, nil, LevelError, `version "1.2" is not MAJOR.MINOR.PATCH`},
		{"version build metadata", func(m map[string]any) { m["version"] = "1.2.0+abc" }, nil, LevelError, "is not MAJOR.MINOR.PATCH"},
		{"display_name empty", func(m map[string]any) { m["display_name"] = " " }, nil, LevelError, "display_name is missing"},
		{"description empty", func(m map[string]any) { delete(m, "description") }, nil, LevelWarning, "description is empty"},
		{"server.type node", func(m map[string]any) { obj(m, "server")["type"] = "node" }, nil, LevelError, "Kivali runs Python integrations; server.type must be python"},
		{"entry_point missing", func(m map[string]any) { delete(obj(m, "server"), "entry_point") }, nil, LevelError, "server.entry_point is missing"},
		{"entry_point not in bundle", func(m map[string]any) { obj(m, "server")["entry_point"] = "server/app.py" }, nil, LevelError, "server/app.py is not a file in the bundle"},
		{"command node", func(m map[string]any) { obj(m, "server", "mcp_config")["command"] = "node" }, nil, LevelError, "command must be python3 or python"},
		{"args empty", func(m map[string]any) { obj(m, "server", "mcp_config")["args"] = []any{} }, nil, LevelError, "args is empty"},
		{"env names an undeclared field", func(m map[string]any) { obj(m, "server", "mcp_config", "env")["X"] = "${user_config.nope}" }, nil, LevelError, "names user_config.nope, which user_config does not declare"},
		{"env other placeholder", func(m map[string]any) { obj(m, "server", "mcp_config", "env")["HOME"] = "${HOME}/x" }, nil, LevelError, "${HOME} is not a connection field"},
		{"env dirname placeholder", func(m map[string]any) { obj(m, "server", "mcp_config", "env")["D"] = "${__dirname}" }, nil, LevelError, "${__dirname} is not a connection field"},
		{"user_config directory type", func(m map[string]any) { obj(m, "user_config", "portal")["type"] = "directory" }, nil, LevelError, "type directory is not supported"},
		{"user_config file type", func(m map[string]any) { obj(m, "user_config", "portal")["type"] = "file" }, nil, LevelError, "type file is not supported"},
		{"user_config type missing", func(m map[string]any) { delete(obj(m, "user_config", "portal"), "type") }, nil, LevelError, "user_config.portal: type is missing"},
		{"user_config title missing", func(m map[string]any) { delete(obj(m, "user_config", "portal"), "title") }, nil, LevelError, "user_config.portal: title is missing"},
		{"user_config sensitive not a bool", func(m map[string]any) { obj(m, "user_config", "token")["sensitive"] = "yes" }, nil, LevelError, "user_config.token: sensitive must be true or false"},
		{"user_config required not a bool", func(m map[string]any) { obj(m, "user_config", "token")["required"] = 1 }, nil, LevelError, "user_config.token: required must be true or false"},
		{"user_config bad key", func(m map[string]any) {
			obj(m, "user_config")["Api-Key"] = map[string]any{"type": "string", "title": "Key"}
		}, nil, LevelError, "user_config.Api-Key: the key must be"},
		{"tools empty", func(m map[string]any) { m["tools"] = []any{} }, nil, LevelError, "tools is empty"},
		{"tool bad name", func(m map[string]any) {
			m["tools"] = append(m["tools"].([]any), map[string]any{"name": "Make-Post", "description": "x"})
		}, nil, LevelError, "tools[3] (Make-Post): the name must be"},
		{"tool duplicate", func(m map[string]any) {
			m["tools"] = append(m["tools"].([]any), map[string]any{"name": "whoami", "description": "x"})
		}, nil, LevelError, "tools[3] (whoami) is listed twice"},
		{"tool no description", func(m map[string]any) { delete(m["tools"].([]any)[2].(map[string]any), "description") }, nil, LevelWarning, "tools[2] (whoami) has no description"},
		// A tool's access is what the page groups it under. A key the
		// form does not have, a text or choice field (which no access
		// turns on), or any key without a form would draw a heading that
		// names nothing the owner can grant.
		{"tool access not a field", func(m map[string]any) { m["tools"].([]any)[0].(map[string]any)["access"] = "owners" }, nil, LevelError, "tools[0] (create_post): access names owners, which is not a field of the access form"},
		{"tool access on a choice field", func(m map[string]any) { m["tools"].([]any)[0].(map[string]any)["access"] = "region" }, nil, LevelError, "tools[0] (create_post): access names region, a text or choice field"},
		{"tool access without a form", func(m map[string]any) {
			delete(obj(m, "kivali"), "access")
			m["tools"].([]any)[0].(map[string]any)["access"] = "write"
		}, nil, LevelError, "tools[0] (create_post): access names write, and the integration has no kivali.access"},
		{"connection device", func(m map[string]any) { obj(m, "kivali", "connection")["method"] = "device" }, nil, LevelError, "kivali.connection.device_endpoint is missing; a device connection needs it"},
		{"connection browser", func(m map[string]any) { obj(m, "kivali", "connection")["method"] = "browser" }, nil, LevelError, "kivali.connection.authorization_endpoint is missing; a browser connection needs it"},
		{"connection missing", func(m map[string]any) { delete(obj(m, "kivali"), "connection") }, nil, LevelError, "kivali.connection.method is missing"},
		{"connection unknown", func(m map[string]any) { obj(m, "kivali", "connection")["method"] = "oauth" }, nil, LevelError, "kivali.connection.method oauth is not one Kivali knows; it is token, device, browser or setup"},
		{"hosts empty", func(m map[string]any) { obj(m, "kivali")["hosts"] = []any{} }, nil, LevelError, "kivali.hosts is empty"},
		{"host star", func(m map[string]any) { obj(m, "kivali")["hosts"] = []any{"*"} }, nil, LevelError, "* would reach every host"},
		{"host scheme", func(m map[string]any) { obj(m, "kivali")["hosts"] = []any{"https://api.example.com"} }, nil, LevelError, "is not a host name"},
		{"host port", func(m map[string]any) { obj(m, "kivali")["hosts"] = []any{"api.example.com:443"} }, nil, LevelError, "is not a host name"},
		{"host path", func(m map[string]any) { obj(m, "kivali")["hosts"] = []any{"api.example.com/v1"} }, nil, LevelError, "is not a host name"},
		{"host space", func(m map[string]any) { obj(m, "kivali")["hosts"] = []any{"api example.com"} }, nil, LevelError, "is not a host name"},
		{"host uppercase", func(m map[string]any) { obj(m, "kivali")["hosts"] = []any{"API.example.com"} }, nil, LevelError, "must be lowercase"},
		// Each package name reaches apt-get as root, as an argument: an
		// option, a removal (a trailing '-') or a pinned version must be
		// refused before the owner is ever asked.
		{"package option", func(m map[string]any) { obj(m, "kivali")["packages"] = []any{"gh", "-y"} }, nil, LevelError, `kivali.packages: "-y" is not a Debian package name`},
		{"package removal", func(m map[string]any) { obj(m, "kivali")["packages"] = []any{"gh-"} }, nil, LevelError, `kivali.packages: "gh-" is not a Debian package name`},
		{"package pinned", func(m map[string]any) { obj(m, "kivali")["packages"] = []any{"gh=2.46.0"} }, nil, LevelError, `"gh=2.46.0" is not a Debian package name`},
		{"package uppercase", func(m map[string]any) { obj(m, "kivali")["packages"] = []any{"GH"} }, nil, LevelError, `"GH" is not a Debian package name`},
		{"package empty", func(m map[string]any) { obj(m, "kivali")["packages"] = []any{""} }, nil, LevelError, `"" is not a Debian package name`},
		{"package duplicate", func(m map[string]any) { obj(m, "kivali")["packages"] = []any{"gh", "jq", "gh"} }, nil, LevelError, "kivali.packages: gh is listed twice"},
		{"access schema missing", func(m map[string]any) { delete(obj(m, "kivali", "access"), "schema") }, nil, LevelError, "kivali.access.schema must be a JSON Schema of an object"},
		{"access schema not object", func(m map[string]any) { obj(m, "kivali", "access", "schema")["type"] = "array" }, nil, LevelError, "kivali.access.schema: type must be object"},
		{"access schema combinator", func(m map[string]any) { obj(m, "kivali", "access", "schema")["oneOf"] = []any{} }, nil, LevelError, "kivali.access.schema: oneOf is not supported"},
		{"access nested object", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties")["limits"] = map[string]any{"type": "object", "title": "Limits"}
		}, nil, LevelError, "properties.limits: type object is not supported"},
		{"access number", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties")["max"] = map[string]any{"type": "number", "title": "Max"}
		}, nil, LevelError, "properties.max: type number is not supported"},
		{"access property oneOf", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties", "region")["oneOf"] = []any{}
		}, nil, LevelError, "properties.region: oneOf is not supported"},
		{"access array of numbers", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties", "blogs")["items"] = map[string]any{"type": "number"}
		}, nil, LevelError, "properties.blogs.items: type must be string"},
		{"access array without items", func(m map[string]any) {
			delete(obj(m, "kivali", "access", "schema", "properties", "blogs"), "items")
		}, nil, LevelError, "properties.blogs: an array field needs items"},
		{"access danger on a list", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties", "blogs")["x-kivali-danger"] = true
		}, nil, LevelError, "properties.blogs: x-kivali-danger marks a switch; it goes on a boolean field"},
		{"access danger not a bool", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties", "write")["x-kivali-danger"] = "yes"
		}, nil, LevelError, "properties.write: x-kivali-danger must be true or false"},
		{"access section not text", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties", "write")["x-kivali-section"] = 3
		}, nil, LevelError, "properties.write: x-kivali-section must be a heading, as text"},
		{"access section blank", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties", "blogs")["x-kivali-section"] = " "
		}, nil, LevelError, "properties.blogs: x-kivali-section must be a heading, as text"},
		{"access enum of numbers", func(m map[string]any) {
			obj(m, "kivali", "access", "schema", "properties", "region")["enum"] = []any{1, 2}
		}, nil, LevelError, "properties.region.enum must be a non-empty list of text"},
		{"access required not a property", func(m map[string]any) {
			obj(m, "kivali", "access", "schema")["required"] = []any{"blogs", "owner"}
		}, nil, LevelError, "required names owner, which is not a property"},
		{"access property without title", func(m map[string]any) {
			delete(obj(m, "kivali", "access", "schema", "properties", "write"), "title")
		}, nil, LevelWarning, "properties.write has no title; the form shows write"},
		{"choices unknown key", func(m map[string]any) { obj(m, "kivali", "access", "choices")["owners"] = "list_blogs" }, nil, LevelError, "kivali.access.choices.owners: the access form has no field owners"},
		{"choices on a switch", func(m map[string]any) { obj(m, "kivali", "access", "choices")["write"] = "list_blogs" }, nil, LevelError, "kivali.access.choices.write: only a text or list field"},
		{"choices on a field with an enum", func(m map[string]any) { obj(m, "kivali", "access", "choices")["region"] = "list_blogs" }, nil, LevelError, "kivali.access.choices.region: the field has fixed options"},
		{"choices unknown tool", func(m map[string]any) { obj(m, "kivali", "access", "choices")["blogs"] = "list_everything" }, nil, LevelError, "names list_everything, which is not in tools"},
		{"verify unknown tool", func(m map[string]any) { obj(m, "kivali")["verify"] = "ping" }, nil, LevelError, "kivali.verify names ping, which is not in tools"},
		{"bundle symlink", nil, func(f []BundleFile) []BundleFile {
			return append(f, BundleFile{Path: "server/link.py", Mode: fs.ModeSymlink | 0o777})
		}, LevelError, "bundle: server/link.py is a symlink"},
		{"bundle special file", nil, func(f []BundleFile) []BundleFile {
			return append(f, BundleFile{Path: "server/fifo", Mode: fs.ModeNamedPipe | 0o644})
		}, LevelError, "bundle: server/fifo is not a regular file"},
		{"bundle dotdot", nil, func(f []BundleFile) []BundleFile {
			return append(f, BundleFile{Path: "../evil.py", Mode: 0o644})
		}, LevelError, "bundle: ../evil.py leaves the bundle"},
		{"bundle absolute", nil, func(f []BundleFile) []BundleFile {
			return append(f, BundleFile{Path: "/etc/evil", Mode: 0o644})
		}, LevelError, "bundle: /etc/evil is an absolute path"},
		{"bundle too big", nil, func(f []BundleFile) []BundleFile {
			return append(f, BundleFile{Path: "data.bin", Size: MaxBundleBytes, Mode: 0o644})
		}, LevelError, "more than the 64 MiB a bundle may hold"},
		{"bundle too many files", nil, func(f []BundleFile) []BundleFile {
			for i := range MaxBundleFiles {
				f = append(f, BundleFile{Path: "f" + strings.Repeat("x", i%7), Size: 1, Mode: 0o644})
			}
			return f
		}, LevelError, "more than the 5000 a bundle may hold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := integrationtest.Manifest("crm", "1.0.0")
			if c.change != nil {
				c.change(m)
			}
			files := defaultFiles()
			if c.files != nil {
				files = c.files(files)
			}
			findings := Validate(parseMap(t, m), files)
			for _, f := range findings {
				if strings.Contains(f.Text, c.want) {
					if f.Level != c.level {
						t.Errorf("%q is a %s, want a %s", f.Text, f.Level, c.level)
					}
					if c.level == LevelWarning && findings.HasErrors() {
						t.Errorf("a warning case has errors: %v", findings.Texts())
					}
					return
				}
			}
			t.Errorf("no finding contains %q; got %v", c.want, findings.Texts())
		})
	}
}

// A bundle that wraps a CLI declares the CLI's package. If the rule
// refused real Debian names, such a bundle could never be published;
// if the parser dropped the field, the pod would start without the
// software and every call that runs it would fail.
func TestValidPackagesPassAndRead(t *testing.T) {
	raw := integrationtest.Manifest("crm", "1.0.0")
	obj(raw, "kivali")["packages"] = []any{"gh", "poppler-utils", "libc++-dev"}
	m := parseMap(t, raw)
	if f := Validate(m, defaultFiles()); len(f) != 0 {
		t.Fatalf("findings: %v", f.Texts())
	}
	if got := strings.Join(m.Kivali.Packages, " "); got != "gh poppler-utils libc++-dev" {
		t.Errorf("packages read as %q", got)
	}
}

// The fixture every later test builds on passes the check with no
// finding at all, so a test that changes one thing sees only that
// thing's finding.
func TestFixtureManifestPasses(t *testing.T) {
	m := parseMap(t, integrationtest.Manifest("crm", "1.0.0"))
	if f := Validate(m, defaultFiles()); len(f) != 0 {
		t.Fatalf("fixture findings: %v", f.Texts())
	}
}

// A manifest Validate never saw (nil, after a bundle without one) is
// one error, not a panic.
func TestValidateNilManifest(t *testing.T) {
	if f := Validate(nil, nil); !f.HasErrors() {
		t.Errorf("findings = %v", f.Texts())
	}
}

// Errors come before warnings in what the upload refusal lists, so the
// owner reads what blocks the bundle first.
func TestFindingsTextsPutErrorsFirst(t *testing.T) {
	f := Findings{{LevelWarning, "w"}, {LevelError, "e"}}
	if got := strings.Join(f.Texts(), ","); got != "e,w" {
		t.Errorf("Texts = %s", got)
	}
	if got := f.Warnings(); len(got) != 1 || got[0] != "w" {
		t.Errorf("Warnings = %v", got)
	}
}

// ToolPrefix names an integration's tools; a wrong prefix would put
// its actions under another name than the one the install screen shows.
func TestToolPrefix(t *testing.T) {
	for in, want := range map[string]string{"hubspot": "hubspot_", "my-crm": "my_crm_", "a": "a_"} {
		if got := ToolPrefix(in); got != want {
			t.Errorf("ToolPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// Names are directory names in the store; one that slips through with
// a slash or a dot would be a path, so the rule is pinned at its edges.
func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "7", "hubspot", "my-crm", strings.Repeat("a", 32)} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a_b", "a.b", "a/b", "..", strings.Repeat("a", 33)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

// A ParseManifest error is what the upload refusal shows for a manifest
// that is not JSON; a tolerant parse there would store a bundle whose
// manifest reads empty.
func TestParseManifestRefusesBadJSON(t *testing.T) {
	for _, raw := range []string{`{`, `[]`, `{"name": 5}`, `{"user_config": {"a": {}, "a": {}}}`, `{"user_config": []}`} {
		if _, err := ParseManifest([]byte(raw)); err == nil {
			t.Errorf("ParseManifest(%s) accepted", raw)
		}
	}
	// Fields Kivali does not read are tolerated.
	if _, err := ParseManifest([]byte(`{"name":"a","compatibility":{"platforms":["linux"]},"prompts":[]}`)); err != nil {
		t.Errorf("unknown fields refused: %v", err)
	}
}

// user_config marshals back as an object in its order, so a manifest
// written out again reads the same.
func TestUserConfigRoundTrip(t *testing.T) {
	raw := []byte(`{"zeta":{"type":"string","title":"Z"},"alpha":{"type":"boolean","title":"A","sensitive":true}}`)
	var u UserConfig
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var back UserConfig
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].Key != "zeta" || back[1].Key != "alpha" || !back[1].Sensitive {
		t.Errorf("round trip = %+v (from %s)", back, out)
	}
}
