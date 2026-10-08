package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

func mustStore(t *testing.T) *store.FSStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}

// seedArchivedChat writes a chat.jsonl into the agent's archive
// directory at a given ts, with the supplied entries. Mimics what
// ArchiveChat produces at the end of a rotation.
func seedArchivedChat(t *testing.T, s *store.FSStore, slug, ts string, entries []store.ChatMessage) {
	t.Helper()
	// Pre-reset chat.jsonl under agents/<slug>/chat.jsonl with the
	// entries, then archive — this gives us a real archived chat at
	// the target ts. CreateAgent may return "already exists" on a
	// second call in the same test; that's expected.
	_ = s.CreateAgent(store.Agent{Slug: slug, Role: "R"}, "k")
	for _, m := range entries {
		if err := s.AppendChatMessage(slug, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	actualTS, err := s.ArchiveChat(slug)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	_ = actualTS
	_ = ts // we don't control the archive TS; caller references by position instead
}

func callSearch(t *testing.T, s *store.FSStore, slug string, in map[string]any) *ToolResult {
	t.Helper()
	raw, _ := json.Marshal(in)
	tool := SearchPastChatsTool(NewStorePastChatsReader(s, slug))
	res, err := tool.Handler(context.Background(), raw)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return res
}

func TestSearchPastChatsNoArchives(t *testing.T) {
	s := mustStore(t)
	_ = s.CreateAgent(store.Agent{Slug: "alice", Role: "R"}, "k")
	res := callSearch(t, s, "alice", map[string]any{"query": "anything"})
	if res.IsError {
		t.Errorf("empty archive should NOT be an error; got IsError=true content=%v", res.Content)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0], "No past chats") {
		t.Errorf("expected 'no past chats' message, got %v", res.Content)
	}
}

func TestSearchPastChatsFindsMatchAcrossChats(t *testing.T) {
	s := mustStore(t)
	seedArchivedChat(t, s, "alice", "chat1", []store.ChatMessage{
		{Role: "received", Content: "let's discuss the solar inverter monitoring approach"},
		{Role: "sent", Content: "solar inverters are a good first product"},
	})
	seedArchivedChat(t, s, "alice", "chat2", []store.ChatMessage{
		{Role: "received", Content: "today we switch topics to Q3 planning"},
		{Role: "sent", Content: "Q3 plan looks solid, no mention of inverters"},
	})
	seedArchivedChat(t, s, "alice", "chat3", []store.ChatMessage{
		{Role: "received", Content: "revisit the solar inverter question"},
	})

	res := callSearch(t, s, "alice", map[string]any{"query": "solar inverter"})
	if res.IsError {
		t.Fatalf("IsError=true content=%v", res.Content)
	}
	body := res.Content[0]
	// Must match in chat1 (2 hits) and chat3 (1 hit), not chat2.
	for _, want := range []string{"solar inverter monitoring", "first product", "revisit the solar inverter"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected snippet containing %q; got:\n%s", want, body)
		}
	}
	// chat2's content must NOT appear in the output body.
	if strings.Contains(body, "Q3 plan") {
		t.Errorf("chat2 leaked into results — grep should only hit chats with matches\n%s", body)
	}
	// Header reports the count and chat-with-hits ratio.
	if !strings.Contains(body, "Found 3 match") {
		t.Errorf("expected 'Found 3 match...' header; got:\n%s", body)
	}
	if !strings.Contains(body, "across 2 of 3 past chat") {
		t.Errorf("expected 'across 2 of 3 past chat(s)' header; got:\n%s", body)
	}
}

func TestSearchPastChatsCaseInsensitive(t *testing.T) {
	s := mustStore(t)
	seedArchivedChat(t, s, "alice", "chat1", []store.ChatMessage{
		{Role: "received", Content: "The Chief Scientist needs to handle this"},
	})
	res := callSearch(t, s, "alice", map[string]any{"query": "chief scientist"})
	if res.IsError {
		t.Fatalf("IsError=true content=%v", res.Content)
	}
	if !strings.Contains(res.Content[0], "Chief Scientist") {
		t.Errorf("case-insensitive match missed; got:\n%s", res.Content[0])
	}
}

func TestSearchPastChatsMaxResultsCaps(t *testing.T) {
	s := mustStore(t)
	seedArchivedChat(t, s, "alice", "chat1", []store.ChatMessage{
		{Role: "received", Content: "alpha"},
		{Role: "sent", Content: "alpha"},
		{Role: "received", Content: "alpha"},
		{Role: "sent", Content: "alpha"},
		{Role: "received", Content: "alpha"},
	})
	res := callSearch(t, s, "alice", map[string]any{"query": "alpha", "max_results": 2})
	if res.IsError {
		t.Fatalf("IsError=true: %v", res.Content)
	}
	if !strings.Contains(res.Content[0], "Found 2 match") {
		t.Errorf("max_results cap not honored; got:\n%s", res.Content[0])
	}
}

func TestSearchPastChatsEmptyQueryRejected(t *testing.T) {
	s := mustStore(t)
	_ = s.CreateAgent(store.Agent{Slug: "alice", Role: "R"}, "k")
	res := callSearch(t, s, "alice", map[string]any{"query": "  "})
	if !res.IsError {
		t.Errorf("empty query should be an error")
	}
}

func TestSearchPastChatsSnippetHasContext(t *testing.T) {
	s := mustStore(t)
	// Long content on both sides of the match so snippet uses context.
	seedArchivedChat(t, s, "alice", "chat1", []store.ChatMessage{
		{Role: "received", Content: strings.Repeat("before ", 20) + "NEEDLE " + strings.Repeat("after ", 20)},
	})
	res := callSearch(t, s, "alice", map[string]any{"query": "NEEDLE"})
	body := res.Content[0]
	// Must have at least some "before" text and some "after" text in the snippet.
	if !strings.Contains(body, "before") || !strings.Contains(body, "after") {
		t.Errorf("snippet missing context; got:\n%s", body)
	}
	// And ellipses, since we truncated both ends.
	if !strings.Contains(body, "…") {
		t.Errorf("expected ellipses around truncated snippet; got:\n%s", body)
	}
}
