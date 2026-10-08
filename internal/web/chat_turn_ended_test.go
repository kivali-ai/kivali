package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// streamCode is what GET /agents/{slug}/stream answers. The request is
// already cancelled, so a stream that attaches to a hub returns at once
// (with 200) instead of blocking the test.
func streamCode(t *testing.T, srv *Server, slug string) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/agents/"+slug+"/stream", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr.Code
}

// Between a turn's `done` and the hub completing, finalizeAgentpodTurn
// still runs the graph pass and the flush. The page closes its stream
// on `done` and fetches the chat; were it told the agent is running in
// that window, it would reopen the stream, be replayed the `done`,
// fetch again, and go round until the hub completed — the thinking
// dots, Stop and the transcript flickering at the end of every turn.
func TestTurnEndedHubIsFinishedForThePage(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hub, _ := srv.getOrCreateHub("alice")
	if !getChat(t, srv, "alice").Running {
		t.Fatal("setup: a claimed hub should read running")
	}

	st := &agentpodTurnState{slug: "alice", hub: hub}
	st.emitTerminal("done", map[string]any{"rotation": false})

	if got := streamCode(t, srv, "alice"); got != http.StatusNoContent {
		t.Errorf("stream after done = %d, want 204 (no replay of the done)", got)
	}
	if getChat(t, srv, "alice").Running {
		t.Error("chat after done reads running; the page would reopen the stream and loop")
	}
	// Work coordination is unchanged until the finalize completes the
	// hub: a message arriving now is still staged and flushed by it.
	if !srv.ChatHubActive("alice") {
		t.Error("ChatHubActive went false before the hub completed")
	}
}

// A `done` that a rotation follows keeps the hub live: the page holds
// its stream open for rotation_done, and a page loaded meanwhile must
// still attach to receive it.
func TestRotationDoneKeepsHubLiveForThePage(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hub, _ := srv.getOrCreateHub("alice")
	st := &agentpodTurnState{slug: "alice", hub: hub}
	st.emitTerminal("done", map[string]any{"rotation": true})

	if !hub.liveForPage() {
		t.Error("a rotation's done ended the hub for the page")
	}
	if !getChat(t, srv, "alice").Running {
		t.Error("chat reads idle while the rotation is being finalized")
	}
}
