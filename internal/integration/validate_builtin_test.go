package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// builtinManifest is a built-in's manifest: the bundle's shape with no
// server and no hosts, a live-listed field whose name is no tool, and
// a verify name.
func builtinManifest() *Manifest {
	return &Manifest{
		ManifestVersion: "0.3",
		Name:            "notes",
		Version:         "1.0.0",
		DisplayName:     "Notes",
		Description:     "Notes kept by a service.",
		Author:          Author{Name: "Kivali"},
		UserConfig:      UserConfig{{Key: "token", Type: "string", Title: "Token", Sensitive: true, Required: true}},
		Tools:           []Tool{{Name: "find", Description: "Find a note."}},
		Kivali: Kivali{
			Connection: Connection{Method: "token"},
			Access: &Access{
				Schema:  json.RawMessage(`{"type":"object","properties":{"books":{"type":"array","items":{"type":"string"},"title":"Books"}}}`),
				Choices: map[string]string{"books": "books"},
			},
			Verify: "find",
		},
	}
}

// Why this test exists: a built-in's manifest is checked by the
// bundle's rules except the ones a built-in has no use for. Without
// ValidateBuiltin, a built-in with no server and no hosts would fail
// the bundle's check, and with a check that skipped everything a
// built-in could ship a manifest the Org page cannot draw. A built-in
// that declared hosts, software or a server would show the owner a
// host list, packages or a runtime that nothing enforces: it runs
// inside Kivali, which has no pod to install software into.
func TestValidateBuiltin(t *testing.T) {
	if f := ValidateBuiltin(builtinManifest()); len(f) != 0 {
		t.Fatalf("findings on a good built-in manifest: %v", f.Texts())
	}
	// A built-in runs inside Kivali with whichever connection an
	// agent's access names, so it may take several.
	multiple := builtinManifest()
	multiple.Kivali.Connection.Multiple = true
	if f := ValidateBuiltin(multiple); len(f) != 0 {
		t.Errorf("findings on a built-in that takes several connections: %v", f.Texts())
	}
	// The same manifest is refused as a bundle: no server, no hosts,
	// and a choices name that is not a tool.
	f := Validate(builtinManifest(), nil)
	for _, want := range []string{"server.type", "kivali.hosts is empty", "kivali.access.choices.books names books, which is not in tools"} {
		if !strings.Contains(strings.Join(f.Texts(), "\n"), want) {
			t.Errorf("as a bundle, missing %q in %v", want, f.Texts())
		}
	}

	cases := []struct {
		name   string
		change func(m *Manifest)
		want   string
	}{
		{"bad name", func(m *Manifest) { m.Name = "Notes" }, "name \"Notes\" must be"},
		{"reserved prefix", func(m *Manifest) { m.Name = "file-notes" }, "choose another name"},
		{"bad version", func(m *Manifest) { m.Version = "1.0" }, "1.0"},
		{"no display name", func(m *Manifest) { m.DisplayName = "" }, "display_name is missing"},
		{"no tools", func(m *Manifest) { m.Tools = nil }, "tools is empty"},
		{"bad connection field", func(m *Manifest) { m.UserConfig[0].Type = "object" }, "type object is not supported"},
		{"sign-in connection", func(m *Manifest) { m.Kivali.Connection.Method = "browser" }, "sign-in connections are not available"},
		{"hosts", func(m *Manifest) { m.Kivali.Hosts = []string{"api.example.com"} }, "a built-in integration declares no hosts; it reaches the hosts its connection names"},
		{"packages", func(m *Manifest) { m.Kivali.Packages = []string{"gh"} }, "kivali.packages: a built-in integration runs inside Kivali and installs no software"},
		{"a server", func(m *Manifest) { m.Server = Server{Type: "python", EntryPoint: "server/main.py"} }, "a built-in integration has no server section"},
		{"a server's env only", func(m *Manifest) { m.Server.MCPConfig.Env = map[string]string{"T": "x"} }, "a built-in integration has no server section"},
		{"bad access form", func(m *Manifest) {
			m.Kivali.Access.Schema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","title":"N"}}}`)
			m.Kivali.Access.Choices = nil
		}, "n"},
		{"bad choices name", func(m *Manifest) { m.Kivali.Access.Choices["books"] = "List Books" }, "kivali.access.choices.books"},
		{"bad verify name", func(m *Manifest) { m.Kivali.Verify = "Who Am I" }, "kivali.verify"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := builtinManifest()
			c.change(m)
			f := ValidateBuiltin(m)
			if !f.HasErrors() || !strings.Contains(strings.Join(f.Texts(), "\n"), c.want) {
				t.Errorf("findings = %v, want an error with %q", f.Texts(), c.want)
			}
		})
	}
}

// Why this test exists: a built-in's manifest is written to the store
// as this type marshals and read back with ParseManifest. A field lost
// on the way (the connection form's order, the access form, a
// sensitive flag, whether it takes several connections) would change
// what the Org page draws for it.
func TestBuiltinManifestRoundTrips(t *testing.T) {
	bm := builtinManifest()
	bm.Kivali.Connection.Multiple = true
	raw, err := json.Marshal(bm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"server"`) {
		t.Errorf("a manifest with no server writes one: %s", raw)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f := ValidateBuiltin(m); len(f) != 0 {
		t.Errorf("findings after a round trip: %v", f.Texts())
	}
	if !m.UserConfig[0].Sensitive || m.Kivali.Access.Choices["books"] != "books" || m.Kivali.Verify != "find" || !m.Kivali.Connection.Multiple {
		t.Errorf("read back as %+v", m)
	}
}
