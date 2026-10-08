package files

import "testing"

// TestIsHardlinkMirrorPath locks in the layout the backup walkers
// trust to skip the per-agent hardlink mirror. If a future refactor
// changes where the mirror lives, this test should be updated AND
// the backup walkers in main/backup.go + internal/web/backup.go
// rechecked, since they share the same skip predicate. The published
// trees are canonical, not mirrors: public/ and whatever an agent's
// old artifacts/ still holds are carried.
func TestIsHardlinkMirrorPath(t *testing.T) {
	mirror := []string{
		"agents/amy/attachments",
		"agents/amy/attachments/abc",
		"agents/amy/attachments/abc/blob.pdf",
	}
	notMirror := []string{
		"",
		".",
		"agents",
		"agents/amy",
		"agents/amy/agent.yaml",
		"agents/amy/chat.jsonl",
		"agents/amy/memory",
		"agents/amy/memory/artifacts",
		"agents/amy/memory/artifacts/shared",
		"agents/amy/memory/artifacts/shared/owner/file.bin",
		"agents/amy/memory/artifacts/public",
		"agents/amy/memory/artifacts/public/x.bin",
		"agents/amy/memory/artifacts/private",
		"agents/amy/memory/artifacts/private/draft.md",
		"agents/amy/files/artifacts/public/x.bin",
		"agents/amy/skills/some/skill.md",
		"attachments",
		"attachments/abc/blob.bin",
		"public",
		"public/amy",
		"public/amy/spec.md",
		"messages/2026-04-18/x.md",
		"project_files/abc/x.bin",
		"claude-home/.claude/.credentials.json",
	}
	for _, p := range mirror {
		if !IsHardlinkMirrorPath(p) {
			t.Errorf("IsHardlinkMirrorPath(%q) = false, want true", p)
		}
	}
	for _, p := range notMirror {
		if IsHardlinkMirrorPath(p) {
			t.Errorf("IsHardlinkMirrorPath(%q) = true, want false", p)
		}
	}
}
