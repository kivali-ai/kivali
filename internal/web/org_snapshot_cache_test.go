package web

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// settledAt is a modification time well outside the store's settle window.
var settledAt = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

func settle(t *testing.T, path string) {
	t.Helper()
	if err := os.Chtimes(path, settledAt, settledAt); err != nil {
		t.Fatal(err)
	}
}

func TestChatFactsForReadsOnlyWhenTheChatMoved(t *testing.T) {
	srv := transcriptServer(t)
	a, err := srv.Store.GetAgent("alice")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(srv.Store.Root(), "agents", "alice", "chat.jsonl")
	line := func(kind, content string) string {
		return `{"role":"sent","kind":"` + kind + `","content":"` + content + `","ts":"2026-09-01T00:00:00Z"}` + "\n"
	}
	disrupted := line(store.KindRuntimeDisruption, "a")
	// The same byte length with no disruption in it.
	calm := line("direct_chat", "a"+strings.Repeat("b", len(store.KindRuntimeDisruption)-len("direct_chat")))
	if len(calm) != len(disrupted) {
		t.Fatalf("test lines differ in length: %d vs %d", len(calm), len(disrupted))
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		settle(t, path)
	}

	write(disrupted)
	if f := srv.chatFactsFor(a); f.disruptions != 1 {
		t.Fatalf("disruptions = %d, want 1", f.disruptions)
	}
	// Different content behind the same size and time: the facts come
	// from the cache, which is how we know nothing was read.
	write(calm)
	if f := srv.chatFactsFor(a); f.disruptions != 1 {
		t.Errorf("an unchanged stamp was read again: disruptions = %d", f.disruptions)
	}
	// An append moves the size: read again.
	write(calm + disrupted + disrupted)
	if f := srv.chatFactsFor(a); f.disruptions != 2 {
		t.Errorf("after an append: disruptions = %d, want 2", f.disruptions)
	}
	// A model change re-measures the fill even with the chat unchanged.
	srv.AgentModel = "something-else"
	write(calm)
	if f := srv.chatFactsFor(a); f.disruptions != 0 || f.defaultModel != "something-else" {
		t.Errorf("after a model change: %+v", f)
	}
	// Forgetting (a restore) reads again.
	write(disrupted)
	srv.chatFactsFor(a)
	write(calm)
	srv.forgetSnapshotCache()
	if f := srv.chatFactsFor(a); f.disruptions != 0 {
		t.Errorf("after forgetting: disruptions = %d, want 0", f.disruptions)
	}
}

func TestCEONeedsFollowsTheInbox(t *testing.T) {
	srv := transcriptServer(t)
	chatPath := filepath.Join(srv.Store.Root(), "agents", agent.CEOSlug, "chat.jsonl")

	if n, paths := srv.ceoNeeds(); n != 0 || len(paths) != 0 {
		t.Fatalf("empty inbox: %d %v", n, paths)
	}
	first := seedCEOApprovalRequest(t, srv, "alice", "Buy a scope", "please")
	settle(t, chatPath)
	if n, paths := srv.ceoNeeds(); n != 1 || len(paths) != 1 || paths[0] != first {
		t.Fatalf("one request: %d %v", n, paths)
	}
	if n, _ := srv.ceoNeeds(); n != 1 {
		t.Errorf("cached: %d", n)
	}
	if got := srv.pendingCEOInbox(); got != 1 {
		t.Errorf("pendingCEOInbox = %d", got)
	}

	second := seedCEOApprovalRequest(t, srv, "bob", "Hire a tech", "please")
	settle(t, chatPath)
	want := []string{first, second}
	sort.Strings(want)
	if n, paths := srv.ceoNeeds(); n != 2 || !slices.Equal(paths, want) {
		t.Errorf("two requests: %d %v, want %v", n, paths, want)
	}

	// A redacted request leaves the inbox without the CEO's chat moving.
	if _, err := srv.Store.MoveToRedacted(filepath.Join(srv.Store.Root(), first)); err != nil {
		t.Fatal(err)
	}
	if n, paths := srv.ceoNeeds(); n != 1 || len(paths) != 1 || paths[0] != second {
		t.Errorf("after redaction: %d %v", n, paths)
	}
}
