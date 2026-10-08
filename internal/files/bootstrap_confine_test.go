package files

import (
	"os"
	"path/filepath"
	"testing"
)

// Core's Sync writes into the agent's tree. Anything the agent left
// there as a link must not carry those writes (or moves) elsewhere.

// victimDir is a directory outside the agent's tree with one file in
// it, standing in for another agent's workspace or the credentials.
func victimDir(t *testing.T, tmp string) string {
	t.Helper()
	dir := filepath.Join(tmp, "victim")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertVictimUntouched(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep.txt" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("victim dir now holds %v, want just keep.txt", names)
	}
}

// A farm directory the agent replaced with a link is replaced with a
// real directory again, and the farm is built there, not at the link's
// target.
func TestSyncReplacesSymlinkedFarmDir(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "agents", "alice")
	root := filepath.Join(agentRoot, StoragePrefix)
	victim := victimDir(t, tmp)
	pf := filepath.Join(tmp, "project_files")
	if err := os.MkdirAll(filepath.Join(pf, "abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pf, "abc", "plan.txt"), []byte("p"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"project", "skills", "episodes"} {
		if err := os.Symlink(victim, filepath.Join(root, d)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Sync(BootstrapOptions{
		AgentRoot:        agentRoot,
		ProjectFilesRoot: pf,
		ProjectFiles:     []ProjectFileRef{{SHA: "abc", OriginalName: "keep.txt", CanonicalName: "plan.txt"}},
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	assertVictimUntouched(t, victim)
	for _, d := range []string{"project", "skills", "episodes"} {
		info, err := os.Lstat(filepath.Join(root, d))
		if err != nil || !info.IsDir() {
			t.Errorf("%s should be a real directory again: %v, %v", d, info, err)
		}
	}
	if _, err := os.Readlink(filepath.Join(root, "project", "keep.txt")); err != nil {
		t.Errorf("project link not built in the agent's own project/: %v", err)
	}
}

// A subagents/ the agent replaced with a link does not steer where core
// builds (and cleans up) the overlay.
func TestSubagentOverlayIgnoresSymlinkedSubagentsDir(t *testing.T) {
	tmp := t.TempDir()
	victim := victimDir(t, tmp)
	parentRoot := filepath.Join(tmp, "alice", StoragePrefix)
	if err := os.MkdirAll(parentRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// The link target already holds an entry named like an overlay
	// link, which a Remove through the link would delete.
	if err := os.MkdirAll(filepath.Join(victim, "ab12"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "ab12", "project"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(parentRoot, "subagents")); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(parentRoot, "subagents", "ab12")
	if err := BuildSubagentOverlay(SubagentOverlay{Root: overlay}); err != nil {
		t.Fatalf("BuildSubagentOverlay: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(victim, "ab12", "project")); err != nil || string(b) != "x" {
		t.Errorf("victim entry changed: %q, %v", b, err)
	}
	info, err := os.Lstat(filepath.Join(parentRoot, "subagents"))
	if err != nil || !info.IsDir() {
		t.Fatalf("subagents/ should be a real directory again: %v, %v", info, err)
	}
	if got, err := os.Readlink(filepath.Join(overlay, "project")); err != nil || got != "../../project" {
		t.Errorf("overlay project link = %q, %v", got, err)
	}
}
