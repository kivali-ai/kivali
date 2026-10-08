package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
)

// The graph tools list a peer's file published after the caller's
// wake, and the path they print for it — /files/artifacts/shared/
// <owner>/<path> — resolves at once: it is a mount of the published
// trees themselves, with no per-caller copy to fall behind. Core's
// Backend for the caller mounts it the same way.
func TestGraphToolsPrintPathsThatResolveAtOnce(t *testing.T) {
	s := graphFixture(t)
	// vp publishes something new mid-way through cs's turn.
	p := filepath.Join(files.PublishedDir(s.Root(), "vp"), "new.md")
	if err := os.WriteFile(p, []byte("---\nid: brand-new\n---\nfresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := graphCall(t, s, "cs", GraphNodeToolName, `{"id":"vp/brand-new"}`)
	path := ""
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "read: file_view "); ok {
			path = rest
		}
	}
	if path != "/files/artifacts/shared/vp/new.md" {
		t.Fatalf("read path = %q:\n%s", path, out)
	}
	backend := files.Dispatcher{DataDir: s.Root()}.BackendFor("cs")
	text, err := backend.View(path, files.ViewOptions{})
	if err != nil {
		t.Fatalf("printed path %s does not resolve for the caller: %v", path, err)
	}
	if !strings.Contains(text, "fresh") {
		t.Errorf("printed path serves stale bytes:\n%s", text)
	}
}
