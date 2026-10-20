package store

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// tailState is what a reader that follows a chat with a ChatTail holds:
// the rows taken so far, plus this read's unterminated final row.
type tailState struct{ rows []string }

func (s *tailState) read(t *testing.T, ct *ChatTail) (contents []string, r ChatTailRead) {
	t.Helper()
	r, err := ct.Read()
	if r.Reset {
		s.rows = nil
	}
	if err != nil {
		t.Fatalf("tail read: %v", err)
	}
	for _, m := range r.Rows {
		s.rows = append(s.rows, m.Content)
	}
	contents = append([]string(nil), s.rows...)
	if r.Tail != nil {
		contents = append(contents, r.Tail.Content)
	}
	return contents, r
}

// Home's CEO inbox and the subagent activity line keep what earlier
// reads returned and parse only what was appended since. If any step
// of a chat's life read differently from a whole read, they would show
// rows the chat does not hold (a cut fragment, a row twice) or miss one
// it does (a row that had lost only its newline). This walks a chat
// through plain appends, an unterminated whole row and a cut append,
// comparing the tail with ReadChatHistory after each. The whole-read
// rules themselves are TestChatSurvivesACutAppend and
// TestChatKeepsAWholeRowMissingItsNewline.
func TestChatTailReadsWhatAWholeReadDoes(t *testing.T) {
	s := newStoreForMemoryTest(t)
	path := s.path("agents", "alice", "chat.jsonl")
	ct := s.ChatHistoryTail("alice")
	var st tailState
	appendRow := func(c string) {
		t.Helper()
		if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(step string) {
		t.Helper()
		got, r := st.read(t, ct)
		want := chatContents(t, s, "alice")
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: tail holds %q, a whole read %q", step, got, want)
		}
		// Starting over would read the same rows, but every read would
		// parse the whole chat again: the cost the tail exists to avoid.
		if r.Reset {
			t.Fatalf("%s: the tail started over on a chat that only grew", step)
		}
	}

	appendRow("one")
	appendRow("two")
	check("first read")
	check("nothing appended")
	appendRow("three")
	check("one append")

	whole := `{"role":"received","content":"four"}`
	cutAppend(t, path, whole, len(whole))
	check("a whole row missing its newline")
	appendRow("five")
	check("the append after it")

	frag := `{"role":"received","content":"` + strings.Repeat("x", 9000) + `"}`
	cutAppend(t, path, frag, 4096)
	check("a cut append")
	appendRow("six")
	check("the append that removes the fragment")
}

// A New chat moves an agent's chat.jsonl into its archive and starts
// an empty one. A reader still holding the old chat's rows would go on
// showing them as current, so the tail reports the reset and reads the
// new file from its top.
func TestChatTailStartsOverAfterARotation(t *testing.T) {
	s := newStoreForMemoryTest(t)
	ct := s.ChatHistoryTail("alice")
	var st tailState
	for _, c := range []string{"one", "two"} {
		if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	st.read(t, ct)
	if _, err := s.ArchiveChat("alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "fresh"}); err != nil {
		t.Fatal(err)
	}
	got, r := st.read(t, ct)
	if !r.Reset || strings.Join(got, ",") != "fresh" {
		t.Fatalf("after a rotation: reset %v, rows %q; want a reset and only the new chat", r.Reset, got)
	}
}

// A corrupt line in the middle of a chat fails every whole read of it
// (TestChatCorruptionInTheMiddleIsAnError). The tail must fail the same
// way, and keep failing after later appends, rather than step over the
// line and go on as if the chat were whole.
func TestChatTailKeepsFailingOnACorruptLine(t *testing.T) {
	s := newStoreForMemoryTest(t)
	path := s.path("agents", "alice", "chat.jsonl")
	ct := s.ChatHistoryTail("alice")
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ct.Read(); err != nil {
		t.Fatal(err)
	}
	cutAppend(t, path, "{garbage\n", 9)
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "two"}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := ct.Read(); err == nil {
			t.Fatalf("read %d after a corrupt line: no error", i+1)
		}
	}
}

// A chat that is not there reads as ErrNotFound, the way ReadChatHistory
// reports it, and one that goes away after being read also resets what
// the reader holds.
func TestChatTailReportsAMissingChat(t *testing.T) {
	s := newStoreForMemoryTest(t)
	path := s.path("agents", "alice", "chat.jsonl")
	ct := s.ChatHistoryTail("alice")
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ct.Read(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r, err := ct.Read()
	if !errors.Is(err, ErrNotFound) || !r.Reset {
		t.Fatalf("read of a removed chat: reset %v, err %v; want a reset and ErrNotFound", r.Reset, err)
	}
}
