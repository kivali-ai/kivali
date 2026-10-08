package files

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSyncLinksEpisodesBesideTranscripts pins the agent-visible shape
// of episodic memory: /files/episodes/<ts>.md is a read-only link to
// chats/<ts>/episode.md, present exactly when the writer has produced
// the digest, and refreshed by the same Sync that maintains
// past-chats/.
func TestSyncLinksEpisodesBesideTranscripts(t *testing.T) {
	agentRoot := t.TempDir()
	chats := filepath.Join(agentRoot, "chats")
	const digested, pending = "20260901T000000.000000000Z", "20260902T000000.000000000Z"
	for _, ts := range []string{digested, pending} {
		if err := os.MkdirAll(filepath.Join(chats, ts), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chats, ts, "chat.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(chats, digested, "episode.md"), []byte("---\ntitle: One\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := BootstrapOptions{AgentRoot: agentRoot, ChatsRoot: chats}
	if err := Sync(opts); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	root := StorageRoot(agentRoot)

	link := filepath.Join(root, "episodes", digested+".md")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("episodes/%s.md not linked: %v", digested, err)
	}
	if want := filepath.Join(chats, digested, "episode.md"); target != want {
		t.Errorf("link target = %q, want %q", target, want)
	}
	if got, err := os.ReadFile(link); err != nil || string(got) != "---\ntitle: One\n---\nbody\n" {
		t.Errorf("reading through the link = (%q, %v)", got, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "episodes", pending+".md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a generation without a digest must not be linked (err=%v)", err)
	}
	// past-chats/ is unaffected: both transcripts are linked.
	for _, ts := range []string{digested, pending} {
		if _, err := os.Lstat(filepath.Join(root, "past-chats", ts+".jsonl")); err != nil {
			t.Errorf("past-chats/%s.jsonl missing: %v", ts, err)
		}
	}

	// The writer lands the second digest; the next Sync links it.
	if err := os.WriteFile(filepath.Join(chats, pending, "episode.md"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Sync(opts); err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "episodes", pending+".md")); err != nil || string(got) != "two" {
		t.Errorf("episodes/%s.md after second Sync = (%q, %v)", pending, got, err)
	}

	// A digest removed (regenerating the corpus) drops its link.
	if err := os.Remove(filepath.Join(chats, digested, "episode.md")); err != nil {
		t.Fatal(err)
	}
	if err := Sync(opts); err != nil {
		t.Fatalf("Sync 3: %v", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale episode link survived Sync (err=%v)", err)
	}

	// Read-only from the agent's side: episodes are written by core.
	b := &Backend{Root: root}
	if err := b.Create("/files/episodes/forged.md", "x"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Create under /files/episodes/: err=%v, want ErrReadOnly", err)
	}
}
