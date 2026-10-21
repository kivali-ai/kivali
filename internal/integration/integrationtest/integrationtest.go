// Package integrationtest builds integration bundles for tests: a
// manifest that passes the bundle check, and a zip of it with a tiny
// server. Tests change the manifest map before zipping it to make the
// bundle they need.
package integrationtest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io/fs"
	"sort"
	"testing"
)

// MainPy is the server the bundles carry: enough to be a Python file.
const MainPy = "print('hello from a test integration')\n"

// Manifest is a manifest for name at version that passes the bundle
// check: one sensitive connection field (token) and one that is not
// (portal), three actions, one host, and an access form with a list
// field the integration lists the options of (blogs, required), a
// switch (write) and a choice with fixed options (region).
func Manifest(name, version string) map[string]any {
	return map[string]any{
		"manifest_version": "0.3",
		"name":             name,
		"version":          version,
		"display_name":     "Test " + name,
		"description":      "Blog posts in a test service.",
		"author":           map[string]any{"name": "Integrations engineer (agent)"},
		"server": map[string]any{
			"type":        "python",
			"entry_point": "server/main.py",
			"mcp_config": map[string]any{
				"command": "python3",
				"args":    []any{"server/main.py"},
				"env":     map[string]any{"TOKEN": "${user_config.token}", "PORTAL": "${user_config.portal}"},
			},
		},
		"user_config": map[string]any{
			"token":  map[string]any{"type": "string", "title": "Private app token", "description": "From the service's settings.", "sensitive": true, "required": true},
			"portal": map[string]any{"type": "string", "title": "Portal", "description": "The portal's short name."},
		},
		"tools": []any{
			map[string]any{"name": "create_post", "description": "Create a blog post as a draft."},
			map[string]any{"name": "list_blogs", "description": "List the blogs the connection can see."},
			map[string]any{"name": "whoami", "description": "Name the connected account."},
		},
		"kivali": map[string]any{
			"connection": map[string]any{"method": "token"},
			"hosts":      []any{"api.example.com"},
			"access": map[string]any{
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"blogs":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "title": "Blogs", "description": "The blogs the agent may write in."},
						"write":  map[string]any{"type": "boolean", "title": "May publish and edit"},
						"region": map[string]any{"type": "string", "enum": []any{"eu", "us"}, "title": "Region"},
					},
					"required": []any{"blogs"},
				},
				"choices": map[string]any{"blogs": "list_blogs"},
			},
			"verify": "whoami",
		},
	}
}

// ManifestJSON is m as manifest.json bytes. A map's keys come out
// sorted, so a test that needs an order writes the bytes itself.
func ManifestJSON(tb testing.TB, m any) []byte {
	tb.Helper()
	if raw, ok := m.([]byte); ok {
		return raw
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// File is one entry of a zip Zip writes beyond the defaults.
type File struct {
	Body string
	Mode fs.FileMode
}

// Zip is a bundle: manifest.json (m, a map or the bytes themselves),
// server/main.py, and files by path, all inside the folder top when it
// is not empty. A files entry with an empty body and mode replaces a
// default with nothing: files["server/main.py"] = File{} leaves the
// server out.
func Zip(tb testing.TB, top string, m any, files map[string]File) []byte {
	tb.Helper()
	all := map[string]File{
		"manifest.json":  {Body: string(ManifestJSON(tb, m))},
		"server/main.py": {Body: MainPy},
	}
	for p, f := range files {
		if f == (File{}) {
			delete(all, p)
			continue
		}
		all[p] = f
	}
	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range paths {
		f := all[p]
		name := p
		if top != "" {
			name = top + "/" + p
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := w.Write([]byte(f.Body)); err != nil {
			tb.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}
