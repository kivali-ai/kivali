package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// An assignment event takes the same road as every other message, so a
// release that lands on an agent mid-turn is offered to that turn as
// a fold rather than ending it — and the text folded in is the event
// alone; what the agent holds was listed by the wake note when the
// turn started. Nothing reaches chat.jsonl until the pod says the
// turn took it.
func TestAssignmentEventReleasedMidTurnIsFolded(t *testing.T) {
	_, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug}, "k"); err != nil {
		t.Fatal(err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0", MaxTokens: 1024})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)

	ctx := context.Background()
	if _, err := srv.tracker().Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Draft the release notes", Body: "Do it.", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Messenger.ReleaseAll(ctx, nil, srv.deliveryHooks()); err != nil {
		t.Fatal(err)
	}

	evs := drainAgentpodEvents(sub)
	folds := foldedDeliveries(t, evs)
	if len(folds) != 1 || folds[0][0] != "turn-1" {
		t.Fatalf("fold offers = %v, want one for turn-1", folds)
	}
	var text string
	for _, ev := range evs {
		if ev.Type != agentpod.EventFoldMessage {
			continue
		}
		var fm agentpod.FoldMessageEvent
		if err := json.Unmarshal(ev.Data, &fm); err != nil {
			t.Fatal(err)
		}
		text = fm.Text
	}
	for _, want := range []string{"chief-of-staff assigned you #1 \"Draft the release notes\".", "Do it."} {
		if !strings.Contains(text, want) {
			t.Errorf("folded text missing %q:\n%s", want, text)
		}
	}
	// A fold brings the event and nothing else; what alice holds was
	// listed once by the wake note when this turn started.
	if strings.Contains(text, "open assignments") {
		t.Errorf("folded text repeats what alice holds:\n%s", text)
	}
	if ids := cancelledTurnIDs(t, evs); len(ids) != 0 {
		t.Errorf("release cancelled turns %v; a fold keeps the turn running", ids)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Kind == "inbox_delivery" {
			t.Fatalf("wake reached chat.jsonl before the pod took it: %q", m.Content)
		}
	}
}

// A fold that the pod reports as landed — including one absorbed as
// the CLI's continuation right at the end of the turn — is written at
// that moment and never again: the end-of-turn flush only drains what
// is still buffered.
func TestAssignmentEventFoldedAtTurnEndLandsOnce(t *testing.T) {
	_, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug}, "k"); err != nil {
		t.Fatal(err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0", MaxTokens: 1024})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)

	srv.streamMu.Lock()
	st := srv.agentpodTurns["alice"]
	srv.streamMu.Unlock()
	if st == nil {
		t.Fatal("no in-flight turn state")
	}
	// The turn has written its last words when the wake arrives.
	srv.onAgentpodDelta(st, agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "all done here"})

	ctx := context.Background()
	if _, err := srv.tracker().Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "One more", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Messenger.ReleaseAll(ctx, nil, srv.deliveryHooks()); err != nil {
		t.Fatal(err)
	}
	folds := foldedDeliveries(t, drainAgentpodEvents(sub))
	if len(folds) != 1 {
		t.Fatalf("fold offers = %d, want 1", len(folds))
	}
	// The pod absorbed the continuation and reports the fold landed.
	srv.onAgentpodFolded(st, agentpod.TurnEvent{Kind: agentpod.TurnEventFolded, DeliveryID: folds[0][1], Landed: true})
	// Then the turn ends for real.
	srv.onAgentpodDone(st, doneEvent())
	_ = hub

	hist, _ := srv.Store.ReadChatHistory("alice")
	var wakes int
	var order []string
	for _, m := range hist {
		switch {
		case m.Kind == "inbox_delivery" && strings.Contains(m.Content, "#1 \"One more\""):
			wakes++
			order = append(order, "wake")
		case m.Role == store.RoleSent && m.Content == "all done here":
			order = append(order, "reply")
		}
	}
	if wakes != 1 {
		t.Fatalf("wake written %d times, want exactly once:\n%+v", wakes, hist)
	}
	if len(order) != 2 || order[0] != "reply" || order[1] != "wake" {
		t.Errorf("chat.jsonl order = %v, want the reply before the wake it preceded", order)
	}
	srv.streamMu.Lock()
	buffered := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if buffered != 0 {
		t.Errorf("%d deliveries still buffered after the turn ended", buffered)
	}
}
