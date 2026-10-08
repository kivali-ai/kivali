package web

import (
	"context"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestSpawnChatLoopIfIdleNoopWhenNothingToRespondTo asserts that
// spawning a chat loop on a fresh agent with no inbox entries is a
// no-op — we don't burn Claude tokens on an empty context.
func TestSpawnChatLoopIfIdleNoopWhenNothingToRespondTo(t *testing.T) {
	srv, mc := newTurnServer(t)
	mc.HandlesLoop = true
	var callCount int
	var mu sync.Mutex
	mc.StreamFn = func(ctx context.Context, req provider.CompleteRequest) (provider.Stream, error) {
		mu.Lock()
		callCount++
		mu.Unlock()
		return provider.NewMockStream([]provider.StreamEvent{{Kind: provider.StreamEnd}}, &provider.CompleteResponse{}), nil
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "B", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	started := srv.spawnChatLoopIfIdle("bob", "chat")
	if started {
		t.Error("should not start a loop when agent has no pending response")
	}
	if callCount != 0 {
		t.Errorf("Stream should not have been called; callCount = %d", callCount)
	}
}

// TestChatHubActiveReportsPresence locks in the cross-path guard
// helper that the runtime uses (via Config.SkipAgent = srv.ChatHubActive)
// to avoid spawning a parallel Claude stream when the web chat path
// is already running one. Were it an always-false stub, two Claude
// subprocesses would run inference on the same chat history and
// produce near-duplicate tool calls.
func TestChatHubActiveReportsPresence(t *testing.T) {
	srv, _ := newTurnServer(t)
	if srv.ChatHubActive("bob") {
		t.Error("no hub yet — should be false")
	}
	hub, isOriginator := srv.getOrCreateHub("bob")
	if !isOriginator {
		t.Fatal("first caller should be originator")
	}
	defer func() {
		hub.close()
		srv.removeHub("bob")
	}()
	if !srv.ChatHubActive("bob") {
		t.Error("hub claimed for bob; ChatHubActive should return true")
	}
	if srv.ChatHubActive("alice") {
		t.Error("no hub for alice; should be false")
	}
}
