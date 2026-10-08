package store

import (
	"errors"
	"testing"
)

func seedChat(t *testing.T, s *FSStore, slug string, texts ...string) {
	t.Helper()
	for i, text := range texts {
		m := ChatMessage{Role: RoleReceived, Content: text}
		if i%2 == 1 {
			m.Role = RoleSent
		}
		if err := s.AppendChatMessage(slug, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func TestEpisodeWriteReadHas(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// An episode names an archived generation; there is nothing to
	// attach it to before the rotation.
	if err := s.WriteEpisode("alice", "20260101T000000.000000000Z", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WriteEpisode before archive: err=%v, want ErrNotFound", err)
	}
	seedChat(t, s, "alice", "hi", "hello")
	ts, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if s.HasEpisode("alice", ts) {
		t.Fatal("HasEpisode true before any write")
	}
	if _, err := s.ReadEpisode("alice", ts); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadEpisode before write: err=%v, want ErrNotFound", err)
	}
	body := "---\nts: " + ts + "\ntitle: Greeting\ntouched: -\n---\nAsked: hi.\n"
	if err := s.WriteEpisode("alice", ts, body); err != nil {
		t.Fatalf("WriteEpisode: %v", err)
	}
	got, err := s.ReadEpisode("alice", ts)
	if err != nil || got != body {
		t.Fatalf("ReadEpisode = (%q, %v), want the body", got, err)
	}
	if !s.HasEpisode("alice", ts) {
		t.Fatal("HasEpisode false after write")
	}
}

// TestListArchivedChatsWithoutEpisode pins the writer's backfill
// contract: oldest first, digested generations excluded, and empty
// transcripts never listed (they would otherwise be re-scanned on
// every boot with nothing to digest).
func TestListArchivedChatsWithoutEpisode(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedChat(t, s, "alice", "first chat", "ok")
	oldest, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive 1: %v", err)
	}
	// Nothing said since the last rotation: this generation's
	// transcript is empty.
	empty, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive 2: %v", err)
	}
	seedChat(t, s, "alice", "third chat", "ok")
	digested, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive 3: %v", err)
	}
	if err := s.WriteEpisode("alice", digested, "done"); err != nil {
		t.Fatalf("WriteEpisode: %v", err)
	}
	seedChat(t, s, "alice", "fourth chat", "ok")
	newest, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive 4: %v", err)
	}

	missing, err := s.ListArchivedChatsWithoutEpisode("alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(missing) != 2 || missing[0].Timestamp != oldest || missing[1].Timestamp != newest {
		t.Fatalf("missing = %+v, want [%s, %s] (oldest first; %s empty and %s digested excluded)",
			missing, oldest, newest, empty, digested)
	}

	// An agent with no archive at all is an empty list, not an error.
	if err := s.CreateAgent(Agent{Slug: "bob", Role: "Eng"}, "k"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	if missing, err := s.ListArchivedChatsWithoutEpisode("bob"); err != nil || len(missing) != 0 {
		t.Fatalf("bob: (%v, %v), want empty", missing, err)
	}
}

// TestArchiveChatAsIsIdempotentForAFixedGeneration is the
// crash-recovery contract: the rotation flow archives under a
// generation minted before the memory-update turn, and a Kivali
// restart between archive and marker-clear re-runs finalize with the
// same name. The second run must neither fail nor move the fresh
// chat's new messages over the transcript already archived.
func TestArchiveChatAsIsIdempotentForAFixedGeneration(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedChat(t, s, "alice", "before", "rotation")
	ts := NewArchiveTimestamp()
	if err := s.ArchiveChatAs("alice", ts); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	seedChat(t, s, "alice", "after the crash")

	if err := s.ArchiveChatAs("alice", ts); err != nil {
		t.Fatalf("second archive under the same generation: %v", err)
	}
	archived, err := s.ReadArchivedChat("alice", ts)
	if err != nil {
		t.Fatalf("read archived: %v", err)
	}
	if len(archived) != 2 || archived[0].Content != "before" {
		t.Fatalf("archived transcript = %+v, want the original two messages untouched", archived)
	}
	live, err := s.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if len(live) != 1 || live[0].Content != "after the crash" {
		t.Fatalf("live chat = %+v, want the post-crash message preserved", live)
	}
	gens, _ := s.ListArchivedChats("alice")
	if len(gens) != 1 {
		t.Fatalf("generations = %+v, want exactly one", gens)
	}

	// A generation name is a directory name, never a path.
	if err := s.ArchiveChatAs("alice", "../escape"); err == nil {
		t.Fatal("ArchiveChatAs accepted a path as a generation name")
	}
	if err := s.ArchiveChatAs("alice", ""); err == nil {
		t.Fatal("ArchiveChatAs accepted an empty generation name")
	}
}
