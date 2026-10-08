package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildSubagentOverlayCreatesPrivateDirAndSymlinks proves the
// overlay shape from docs/developers/files-and-publishing.md §"Subagent overlay":
// real artifacts/private/, plus relative symlinks to the parent's
// project / skills / attachments / artifacts/public.
func TestBuildSubagentOverlayCreatesPrivateDirAndSymlinks(t *testing.T) {
	parentRoot := t.TempDir()
	// Stub parent-side targets so resolution doesn't fail.
	for _, p := range []string{"project", "skills", "attachments", "artifacts/public"} {
		if err := os.MkdirAll(filepath.Join(parentRoot, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	overlayRoot := filepath.Join(parentRoot, "subagents", "ab12")
	if err := BuildSubagentOverlay(SubagentOverlay{Root: overlayRoot}); err != nil {
		t.Fatalf("BuildSubagentOverlay: %v", err)
	}

	// artifacts/private is a real dir.
	privDir := filepath.Join(overlayRoot, "artifacts", "private")
	if info, err := os.Lstat(privDir); err != nil || !info.IsDir() {
		t.Errorf("artifacts/private should be a real dir; err=%v info=%+v", err, info)
	}

	// Relative symlinks point at the parent's tree. artifacts/shared is
	// here because graph_node prints a peer's node at
	// /files/artifacts/shared/<owner>/<path> and the subagent has that
	// tool; the Backend's DefaultReadOnlyRoots keeps it read-only.
	cases := map[string]string{
		"project":          "../../project",
		"skills":           "../../skills",
		"attachments":      "../../attachments",
		"artifacts/public": "../../../artifacts/public",
		"artifacts/shared": "../../../artifacts/shared",
	}
	for name, want := range cases {
		got, err := os.Readlink(filepath.Join(overlayRoot, name))
		if err != nil {
			t.Errorf("Readlink %s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("symlink %s target = %q, want %q", name, got, want)
		}
	}

	// Symlinks resolve to the parent's actual dirs.
	resolved, err := filepath.EvalSymlinks(filepath.Join(overlayRoot, "project"))
	if err != nil {
		t.Fatalf("EvalSymlinks project: %v", err)
	}
	wantResolved, _ := filepath.EvalSymlinks(filepath.Join(parentRoot, "project"))
	if resolved != wantResolved {
		t.Errorf("project resolves to %q, want %q", resolved, wantResolved)
	}

	// Idempotent: re-running on an existing overlay leaves it intact
	// AND preserves any artifacts/private/ content.
	if err := os.WriteFile(filepath.Join(privDir, "note.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BuildSubagentOverlay(SubagentOverlay{Root: overlayRoot}); err != nil {
		t.Fatalf("BuildSubagentOverlay second call: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(privDir, "note.md"))
	if err != nil || string(got) != "hi" {
		t.Errorf("note.md should survive idempotent rebuild: got=%q err=%v", got, err)
	}
}

// TestBuildSubagentOverlayRequiresRoot guards the empty-Root error
// path so a programming error doesn't silently produce an unusable
// overlay at the cwd.
func TestBuildSubagentOverlayRequiresRoot(t *testing.T) {
	if err := BuildSubagentOverlay(SubagentOverlay{}); err == nil {
		t.Error("BuildSubagentOverlay with empty Root should error")
	}
}

// TestSubagentOverlaySharedWorkspaceIsWritable proves the one link in
// the overlay that is not a read-only mirror actually behaves like it.
//
// Two things could silently break the sub-lead pattern: the symlink
// resolving somewhere other than the parent's background/ (so siblings get
// private directories that merely look shared), and background/ drifting
// into DefaultReadOnlyRoots (so every write fails and the assembling
// agent finds an empty directory). Both are asserted here because
// neither produces an error anyone would notice at the call site.
func TestSubagentOverlaySharedWorkspaceIsWritable(t *testing.T) {
	parentRoot := t.TempDir()
	for _, p := range []string{"project", "skills", "attachments", "artifacts/public", SharedWorkspaceDir} {
		if err := os.MkdirAll(filepath.Join(parentRoot, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	overlayRoot := filepath.Join(parentRoot, "subagents", "ab12")
	if err := BuildSubagentOverlay(SubagentOverlay{Root: overlayRoot}); err != nil {
		t.Fatalf("BuildSubagentOverlay: %v", err)
	}

	// The subagent writes through its own /files/background/ ...
	sub := &Backend{Root: overlayRoot, WriteRoots: []string{parentRoot}}
	if err := sub.Create(SharedWorkspaceDir+"/"+PlanFileName, "# plan\n- [ ] do the thing\n"); err != nil {
		t.Fatalf("subagent write to shared workspace: %v", err)
	}

	// ... and the parent reads the same bytes at its own path, which
	// is the whole point: one directory, not two.
	landed := filepath.Join(parentRoot, SharedWorkspaceDir, PlanFileName)
	b, err := os.ReadFile(landed)
	if err != nil {
		t.Fatalf("parent-side read of %s: %v", landed, err)
	}
	if string(b) != "# plan\n- [ ] do the thing\n" {
		t.Errorf("parent sees %q, want the subagent's bytes", string(b))
	}

	// And a sibling subagent sees it too.
	siblingRoot := filepath.Join(parentRoot, "subagents", "cd34")
	if err := BuildSubagentOverlay(SubagentOverlay{Root: siblingRoot}); err != nil {
		t.Fatalf("BuildSubagentOverlay sibling: %v", err)
	}
	sibling := &Backend{Root: siblingRoot, WriteRoots: []string{parentRoot}}
	got, err := sibling.View(SharedWorkspaceDir+"/"+PlanFileName, ViewOptions{})
	if err != nil {
		t.Fatalf("sibling read of shared plan: %v", err)
	}
	if !strings.Contains(got, "do the thing") {
		t.Errorf("sibling read %q, want the plan body", got)
	}
}
