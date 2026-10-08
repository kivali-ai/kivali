package store

import (
	"errors"
	"testing"
)

func TestArchiveChatCreatesGenerationAndFreshFile(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i, text := range []string{"one", "two", "three"} {
		m := ChatMessage{Role: RoleReceived, Content: text}
		if i%2 == 1 {
			m.Role = RoleSent
		}
		if err := s.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	ts, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if ts == "" {
		t.Fatal("archive returned empty timestamp")
	}

	// Current chat is empty now.
	hist, err := s.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("Read current: %v", err)
	}
	if len(hist) != 0 {
		t.Errorf("current chat should be empty, got %+v", hist)
	}

	// Archive contains the old messages.
	arch, err := s.ReadArchivedChat("alice", ts)
	if err != nil {
		t.Fatalf("ReadArchived: %v", err)
	}
	if len(arch) != 3 {
		t.Errorf("archived len = %d", len(arch))
	}
}

func TestArchiveChatListGenerations(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "r"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.ArchiveChat("alice"); err != nil {
			t.Fatalf("archive %d: %v", i, err)
		}
	}
	chats, err := s.ListArchivedChats("alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(chats) != 3 {
		t.Errorf("len = %d", len(chats))
	}
	// Sorted newest-first — descending timestamps.
	for i := 1; i < len(chats); i++ {
		if chats[i-1].Timestamp < chats[i].Timestamp {
			t.Errorf("not sorted newest-first: %v", chats)
		}
	}
}

func TestArchiveChatMissingAgent(t *testing.T) {
	s := mustStore(t)
	if _, err := s.ArchiveChat("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestReadArchivedChatMissing(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "a", Role: "r"}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.ReadArchivedChat("a", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

// TestArchivedChatExcerpt covers the label the archive UI hangs on
// each generation: what was said TO the agent first, so a list of
// rotation stamps becomes a list you can actually read.
func TestArchivedChatExcerpt(t *testing.T) {
	cases := []struct {
		name string
		msgs []ChatMessage
		want string
	}{{
		name: "prefers the first inbound message",
		msgs: []ChatMessage{
			{Role: RoleSent, Kind: "direct_chat", Content: "Picking up where I left off."},
			{Role: RoleReceived, Kind: "direct_chat", Content: "How did the launch land?"},
			{Role: RoleReceived, Kind: "direct_chat", Content: "and the second question"},
		},
		want: "How did the launch land?",
	}, {
		name: "falls back to the agent's own opening line",
		msgs: []ChatMessage{{Role: RoleSent, Kind: "direct_chat", Content: "Starting the audit."}},
		want: "Starting the audit.",
	}, {
		// Tool traffic is machinery. A generation labelled with a tool
		// call's serialized input says nothing about the conversation.
		name: "skips tool rows",
		msgs: []ChatMessage{
			{Role: RoleSent, Kind: "tool_use", ToolName: "bash", Content: `{"cmd":"ls"}`},
			{Role: RoleReceived, Kind: "tool_result", Content: "file.txt"},
			{Role: RoleSent, Kind: "direct_chat", Content: "Listed the directory."},
		},
		want: "Listed the directory.",
	}, {
		name: "empty generation has no excerpt",
		msgs: nil,
		want: "",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustStore(t)
			if err := s.CreateAgent(Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			for _, m := range tc.msgs {
				if err := s.AppendChatMessage("alice", m); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
			ts, err := s.ArchiveChat("alice")
			if err != nil {
				t.Fatalf("archive: %v", err)
			}
			got, err := s.ArchivedChatExcerpt("alice", ts)
			if err != nil {
				t.Fatalf("excerpt: %v", err)
			}
			if got != tc.want {
				t.Errorf("excerpt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestArchivedChatExcerptMissing(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "a", Role: "r"}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.ArchivedChatExcerpt("a", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestArchiveChatWithNoHistory(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "a", Role: "r"}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ts, err := s.ArchiveChat("a")
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	arch, err := s.ReadArchivedChat("a", ts)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(arch) != 0 {
		t.Errorf("expected empty: %+v", arch)
	}
}
