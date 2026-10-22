package integration

import (
	"encoding/json"
	"reflect"
	"testing"
)

// withPackages is m with kivali.packages set to pkgs.
func withPackages(m *Manifest, pkgs ...string) *Manifest {
	m.Kivali.Packages = pkgs
	return m
}

// diffManifest is a manifest with these hosts, connection fields
// (user_config's JSON object), actions and access schema properties
// (a JSON object, or "" for access that is all or nothing).
func diffManifest(t *testing.T, hosts []string, userConfig string, tools []string, accessProps string) *Manifest {
	t.Helper()
	m := map[string]any{
		"name":        "crm",
		"version":     "1.0.0",
		"user_config": json.RawMessage(userConfig),
		"kivali":      map[string]any{"hosts": hosts},
	}
	var ts []any
	for _, name := range tools {
		ts = append(ts, map[string]any{"name": name})
	}
	m["tools"] = ts
	if accessProps != "" {
		m["kivali"].(map[string]any)["access"] = map[string]any{
			"schema": json.RawMessage(`{"type":"object","properties":` + accessProps + `}`),
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Why this test exists: the install screen of a version that would
// run in place of the one in use says what it changes, and the owner
// reads those lines instead of comparing two screens by eye. A new
// host, a new secret, a new action or a new access field that Diff
// missed would be let in unread; a field matched by its title instead
// of its key would read as removed and added when only its words
// changed. Software is the same: a package the pod would newly install
// as root, missed here, would be let in unread. Each case is one kind
// of change; going from access that is
// all or nothing to a form names the form's fields; and an unchanged
// manifest, or a nil one, gives no lines at all, which the screen
// shows as no change.
func TestDiff(t *testing.T) {
	h := []string{"api.example.com"}
	uc := `{"token":{"type":"string","title":"Private app token","sensitive":true},"portal":{"type":"string","title":"Portal"}}`
	tools := []string{"create_post", "list_blogs"}
	access := `{"blogs":{"type":"array","items":{"type":"string"},"title":"Blogs"},"write":{"type":"boolean","title":"May publish"}}`
	from := diffManifest(t, h, uc, tools, access)

	cases := []struct {
		name string
		to   func() *Manifest
		want []string
	}{
		{"hosts", func() *Manifest {
			return diffManifest(t, []string{"*.example.com", "uploads.example.com"}, uc, tools, access)
		}, []string{"Hosts added: *.example.com, uploads.example.com", "Hosts removed: api.example.com"}},
		{"software", func() *Manifest {
			return withPackages(diffManifest(t, h, uc, tools, access), "gh", "jq")
		}, []string{"Software added: gh, jq"}},
		{"connection fields, a secret marked", func() *Manifest {
			return diffManifest(t, h, `{"key":{"type":"string","title":"API key","sensitive":true},"portal":{"type":"string","title":"Portal name"},"region":{"type":"string"}}`, tools, access)
		}, []string{"Connection fields added: API key (secret), region", "Connection fields removed: Private app token (secret)"}},
		{"actions, in words", func() *Manifest {
			return diffManifest(t, h, uc, []string{"list_blogs", "delete_post", "find_contact"}, access)
		}, []string{"Actions added: Delete post, Find contact", "Actions removed: Create post"}},
		{"access form fields", func() *Manifest {
			return diffManifest(t, h, uc, tools, `{"blogs":{"type":"array","items":{"type":"string"},"title":"Blogs to write in"},"region":{"type":"string","enum":["eu","us"],"title":"Region"}}`)
		}, []string{"Access form fields added: Region", "Access form fields removed: May publish"}},
		{"everything at once, in order", func() *Manifest {
			return diffManifest(t, nil, `{}`, nil, "")
		}, []string{
			"Hosts removed: api.example.com",
			"Connection fields removed: Private app token (secret), Portal",
			"Actions removed: Create post, List blogs",
			"Access form fields removed: Blogs, May publish",
		}},
		{"nothing changed", func() *Manifest { return diffManifest(t, h, uc, tools, access) }, nil},
	}
	for _, c := range cases {
		if got := Diff(from, c.to()); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Diff =\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
	// From software to other software, and to none.
	gh := withPackages(diffManifest(t, h, uc, tools, access), "gh", "jq")
	if got := Diff(gh, withPackages(diffManifest(t, h, uc, tools, access), "jq", "poppler-utils")); !reflect.DeepEqual(got, []string{"Software added: poppler-utils", "Software removed: gh"}) {
		t.Errorf("software changed: %q", got)
	}
	if got := Diff(gh, from); !reflect.DeepEqual(got, []string{"Software removed: gh, jq"}) {
		t.Errorf("software removed: %q", got)
	}
	plain := diffManifest(t, h, uc, tools, "")
	if got := Diff(plain, from); !reflect.DeepEqual(got, []string{"Access form fields added: Blogs, May publish"}) {
		t.Errorf("all or nothing to a form: %q", got)
	}
	if got := Diff(nil, from); got != nil {
		t.Errorf("Diff(nil, m) = %q", got)
	}
	if got := Diff(from, nil); got != nil {
		t.Errorf("Diff(m, nil) = %q", got)
	}
}
