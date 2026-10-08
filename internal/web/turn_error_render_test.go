package web

import (
	"encoding/json"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// The reload half of the turn-error marker and the sidebar's red dot.
// chat.js paints the same shapes live; these pin what a fresh page load
// shows, which is what the CEO sees when they come back to a stopped
// agent an hour later.

func seedErroredAgent(t *testing.T, srv *Server) {
	t.Helper()
	// Reports to the CEO directly so the sidebar tree renders the node
	// (a parent that is not an active agent is never walked).
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "go"},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "starting", Model: "mock-large-0"},
		{Role: store.RoleReceived, Kind: store.KindTurnError, Content: "The previous turn was stopped by an error and did not complete: Claude AI usage limit reached|1759100000. The Kivali server is still running; resume your prior work per the handbook."},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// TestOrgSnapshotErroredFlag: a trailing turn-error row sets Errored
// in the org snapshot, and a message received after it clears it —
// the same release rule as Stop.
func TestOrgSnapshotErroredFlag(t *testing.T) {
	srv := newTestServer(t)
	seedErroredAgent(t, srv)

	find := func() agentLiveness {
		t.Helper()
		var snap orgSnapshot
		if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		for _, a := range snap.Agents {
			if a.Slug == "alice" {
				return a
			}
		}
		t.Fatal("alice not in snapshot")
		return agentLiveness{}
	}
	alice := find()
	if alice.State != "needs_help" {
		t.Errorf("alice = %+v; want state needs_help", alice)
	}

	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "the limit reset, carry on",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if alice = find(); alice.State == "needs_help" {
		t.Errorf("alice still needs help after a new message; the hold must release on the next received entry")
	}
}
