package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncSkillsLinksEntireDirectory verifies that syncSkills creates
// one symlink per skill dir under /files/skills/ — agents see the
// whole bundle (SKILL.md + scripts) rather than a flat file-per-skill
// listing.
func TestSyncSkillsLinksEntireDirectory(t *testing.T) {
	skillsRoot := t.TempDir()
	// Build two valid skills + one partial (no SKILL.md, must be ignored)
	// + one staging dir (.skill-stage-*, must be ignored).
	writeFile(t, filepath.Join(skillsRoot, "alpha", "SKILL.md"), "---\ndescription: a\n---\n")
	writeFile(t, filepath.Join(skillsRoot, "alpha", "scripts", "run.sh"), "#!/bin/bash\n")
	writeFile(t, filepath.Join(skillsRoot, "beta", "SKILL.md"), "---\ndescription: b\n---\n")
	// Partial (no manifest): must not produce a symlink.
	writeFile(t, filepath.Join(skillsRoot, "half-baked", "notes.md"), "incomplete\n")
	// Staging turd: same.
	if err := os.MkdirAll(filepath.Join(skillsRoot, ".skill-stage-xyz"), 0o755); err != nil {
		t.Fatal(err)
	}

	agentRoot := t.TempDir()
	if err := Sync(BootstrapOptions{
		AgentRoot:  agentRoot,
		SkillsRoot: skillsRoot,
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	dst := filepath.Join(agentRoot, "memory", "skills")
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !hasName(names, "alpha") || !hasName(names, "beta") {
		t.Errorf("missing expected skill symlinks; got %v", names)
	}
	if hasName(names, "half-baked") {
		t.Errorf("half-baked skill (no SKILL.md) should be skipped; got %v", names)
	}
	if hasName(names, ".skill-stage-xyz") {
		t.Errorf("staging dir leaked into memory; got %v", names)
	}

	// The symlinks should point at the source skill dirs so the agent
	// reads the full tree through file_view (including scripts/).
	alphaLink := filepath.Join(dst, "alpha")
	target, err := os.Readlink(alphaLink)
	if err != nil {
		t.Fatalf("readlink alpha: %v", err)
	}
	if target != filepath.Join(skillsRoot, "alpha") {
		t.Errorf("symlink target=%q want %q", target, filepath.Join(skillsRoot, "alpha"))
	}

	// And the script under the symlinked tree must be reachable via
	// regular filesystem APIs (so the agent's file_view can read it).
	scriptPath := filepath.Join(dst, "alpha", "scripts", "run.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("script not reachable through symlinked dir: %v", err)
	}
}

// TestSyncSkillsRemovesDeletedSkills verifies a second Sync after a
// skill is removed cleans up the corresponding symlink.
func TestSyncSkillsRemovesDeletedSkills(t *testing.T) {
	skillsRoot := t.TempDir()
	agentRoot := t.TempDir()
	writeFile(t, filepath.Join(skillsRoot, "goner", "SKILL.md"), "---\ndescription: x\n---\n")
	if err := Sync(BootstrapOptions{AgentRoot: agentRoot, SkillsRoot: skillsRoot}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agentRoot, "memory", "skills", "goner")
	if _, err := os.Lstat(dst); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	// Remove the skill from the source.
	if err := os.RemoveAll(filepath.Join(skillsRoot, "goner")); err != nil {
		t.Fatal(err)
	}
	if err := Sync(BootstrapOptions{AgentRoot: agentRoot, SkillsRoot: skillsRoot}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dst); err == nil {
		t.Errorf("stale symlink still present after source deleted")
	} else if !strings.Contains(err.Error(), "no such file") && !os.IsNotExist(err) {
		t.Errorf("unexpected err: %v", err)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
