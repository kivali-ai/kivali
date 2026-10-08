package store

import (
	"os"
	"strings"
	"testing"
	"time"
)

// cutAppend leaves the first n bytes of a further row at the end of the
// file at path, as a power cut part way through an append can: the row's
// first pages written back, the file size moved past them, no newline.
func cutAppend(t *testing.T, path, row string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(row[:n]); err != nil {
		t.Fatal(err)
	}
}

func chatContents(t *testing.T, s *FSStore, slug string) []string {
	t.Helper()
	hist, err := s.ReadChatHistory(slug)
	if err != nil {
		t.Fatalf("read chat: %v", err)
	}
	var out []string
	for _, m := range hist {
		out = append(out, m.Content)
	}
	return out
}

// A row cut short by a power cut is never acknowledged: the chat still
// reads (the rows before it intact), and the next append removes the
// fragment instead of gluing a new row onto it, which would make one
// unreadable line and fail every later read of the whole chat.
func TestChatSurvivesACutAppend(t *testing.T) {
	s := newStoreForMemoryTest(t)
	for _, c := range []string{"one", "two"} {
		if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	path := s.path("agents", "alice", "chat.jsonl")
	row := `{"role":"received","content":"` + strings.Repeat("x", 9000) + `"}`
	cutAppend(t, path, row, 4096)

	if got := chatContents(t, s, "alice"); strings.Join(got, ",") != "one,two" {
		t.Fatalf("chat with a cut append = %q, want the two whole rows", got)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "three"}); err != nil {
		t.Fatal(err)
	}
	if got := chatContents(t, s, "alice"); strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("chat after the next append = %q", got)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "xxxx") || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("the fragment survived the next append:\n%s", b)
	}
}

// A whole row that lost only its newline is kept, not truncated away.
func TestChatKeepsAWholeRowMissingItsNewline(t *testing.T) {
	s := newStoreForMemoryTest(t)
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	path := s.path("agents", "alice", "chat.jsonl")
	row := `{"role":"received","content":"two"}`
	cutAppend(t, path, row, len(row))
	if got := chatContents(t, s, "alice"); strings.Join(got, ",") != "one,two" {
		t.Fatalf("chat = %q, want the unterminated whole row read", got)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "three"}); err != nil {
		t.Fatal(err)
	}
	if got := chatContents(t, s, "alice"); strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("chat = %q", got)
	}
}

// Damage anywhere but a cut final append still fails the read loudly:
// the tolerance is for exactly the unacknowledged tail, nothing else.
func TestChatCorruptionInTheMiddleIsAnError(t *testing.T) {
	s := newStoreForMemoryTest(t)
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	path := s.path("agents", "alice", "chat.jsonl")
	cutAppend(t, path, "{garbage\n", 9)
	if err := s.AppendChatMessage("alice", ChatMessage{Role: RoleReceived, Content: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadChatHistory("alice"); err == nil {
		t.Fatal("a corrupt line in the middle of the chat read without an error")
	}
}

func TestSubagentChatSurvivesACutAppend(t *testing.T) {
	s := newStoreForMemoryTest(t)
	if err := s.AppendSubagentMessage("alice", "t1", ChatMessage{Role: RoleReceived, Content: "one"}); err != nil {
		t.Fatal(err)
	}
	path := s.path("agents", "alice", "subagents", "t1", "chat.jsonl")
	cutAppend(t, path, `{"role":"received","content":"lost"}`, 20)
	if err := s.AppendSubagentMessage("alice", "t1", ChatMessage{Role: RoleReceived, Content: "two"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"two"`) {
		t.Fatalf("subagent chat:\n%s", b)
	}
}

func TestUsageSurvivesACutAppend(t *testing.T) {
	s := newStoreForMemoryTest(t)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		if err := s.AppendUsage(UsageRecord{TS: t0.Add(time.Duration(i) * time.Minute), Model: "m", CostUSD: 1}); err != nil {
			t.Fatal(err)
		}
	}
	cutAppend(t, s.path("usage.jsonl"), `{"ts":"2026-10-01T00:05:00Z","model":"m","cost_usd":1}`, 30)
	recs, err := s.ReadUsageSince(time.Time{})
	if err != nil || len(recs) != 3 {
		t.Fatalf("usage with a cut append: %d records, %v; want 3", len(recs), err)
	}
	if err := s.AppendUsage(UsageRecord{TS: t0.Add(10 * time.Minute), Model: "m", CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	recs, err = s.ReadUsageSince(time.Time{})
	if err != nil || len(recs) != 4 {
		t.Fatalf("usage after the next append: %d records, %v; want 4", len(recs), err)
	}
}
