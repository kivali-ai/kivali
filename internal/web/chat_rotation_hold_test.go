package web

import (
	"context"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestDeliveryDuringRotationIsHeldForTheFreshChat is the rotation
// rule: a message that arrives while the agent is reconciling memory
// for a rotation is neither folded into that turn nor allowed to wind
// it up. It waits; the turn finishes and archives; the message opens
// the fresh chat.
//
// Without the hold the pod folds the message into the reconcile turn —
// from the pod's side it is a chat turn like any other — so the model
// answers it inside a transcript that is about to be archived and the
// fresh chat never sees it. A fold that missed would instead cancel
// the reconcile.
func TestDeliveryDuringRotationIsHeldForTheFreshChat(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	// The chat being rotated, ending on the reconcile prompt the
	// running turn is answering.
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "earlier"},
		{Role: store.RoleSent, Content: "response"},
		{Role: store.RoleReceived, Kind: "rotation_prompt", Content: "reconcile your memory"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{
		PriorMemory: "# old", Timestamp: store.NewArchiveTimestamp(), RequestedBy: "ceo",
	}); err != nil {
		t.Fatalf("WritePendingRotation: %v", err)
	}

	for _, body := range []string{"first", "second"} {
		if err := srv.deliverToAgent("alice", store.ChatMessage{
			Role: store.RoleReceived, Kind: "direct_chat", Content: body,
		}); err != nil {
			t.Fatalf("deliver %s: %v", body, err)
		}
	}

	// Nothing reached the pod: no fold offer, no cancel.
	evs := drainAgentpodEvents(sub)
	if folds := foldedDeliveries(t, evs); len(folds) != 0 {
		t.Errorf("rotation turn was offered folds %v; want none", folds)
	}
	if ids := cancelledTurnIDs(t, evs); len(ids) != 0 {
		t.Errorf("rotation turn was asked to wind up (%v); want none", ids)
	}
	srv.streamMu.Lock()
	buffered := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if buffered != 2 {
		t.Fatalf("buffered deliveries = %d, want 2", buffered)
	}
	before, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range before {
		if m.Content == "first" || m.Content == "second" {
			t.Errorf("held message %q reached chat.jsonl before the rotation finished", m.Content)
		}
	}

	// The reconcile turn ends: archive, then flush.
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDone, StopReason: "end_turn",
	}); err != nil {
		t.Fatalf("done: %v", err)
	}

	gens, err := srv.Store.ListArchivedChats("alice")
	if err != nil || len(gens) != 1 {
		t.Fatalf("archived generations = %v (%v); want exactly one", gens, err)
	}
	archived, err := srv.Store.ReadArchivedChat("alice", gens[0].Timestamp)
	if err != nil {
		t.Fatalf("ReadArchivedChat: %v", err)
	}
	var sawPrompt bool
	for _, m := range archived {
		if m.Kind == "rotation_prompt" {
			sawPrompt = true
		}
		if m.Content == "first" || m.Content == "second" {
			t.Errorf("held message %q was archived with the old chat", m.Content)
		}
	}
	if !sawPrompt {
		t.Error("archive is missing the rotation prompt; the wrong transcript was archived")
	}
	fresh, _ := srv.Store.ReadChatHistory("alice")
	var got []string
	for _, m := range fresh {
		got = append(got, m.Content)
	}
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("fresh chat = %v, want [first second]: the held messages in arrival order and nothing else", got)
	}
	srv.streamMu.Lock()
	left := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if left != 0 {
		t.Errorf("%d deliveries still buffered after the rotation finalized", left)
	}
}

// TestDeliveryToIdleAgentWithRotationMarkerWritesThrough pins the edge
// of the hold: the marker alone is not a reason to stage. With no turn
// in flight nothing would flush the buffer promptly — the rotation is
// either about to start, and its turn reads the message from disk, or
// was interrupted by a restart, and the recovered turn does — and a
// message held in memory against a pod that may never come back is a
// message lost on the next restart.
func TestDeliveryToIdleAgentWithRotationMarkerWritesThrough(t *testing.T) {
	_, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")

	// Retire the turn: completed hub, no agentpod turn state.
	hub.markCompleted()
	srv.streamMu.Lock()
	delete(srv.agentpodTurns, "alice")
	srv.streamMu.Unlock()
	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{RequestedBy: "ceo"}); err != nil {
		t.Fatalf("WritePendingRotation: %v", err)
	}

	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "hello",
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if evs := drainAgentpodEvents(sub); len(evs) != 0 {
		t.Errorf("idle agent published %d pod events; want none", len(evs))
	}
	srv.streamMu.Lock()
	buffered := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if buffered != 0 {
		t.Errorf("buffered deliveries = %d, want 0: nothing is running to flush them", buffered)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	var got []string
	for _, m := range hist {
		if m.Role == store.RoleReceived {
			got = append(got, m.Content)
		}
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("chat.jsonl received rows = %v, want [hello] written directly", got)
	}
}
