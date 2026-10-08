package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// TestAgentToAgentRoundTripEndToEnd exercises the full multi-release
// round trip through the real HTTP handlers and real runtime, with
// only the Claude transport mocked. It catches wiring regressions
// that per-layer unit tests miss:
//
//  1. release 1: CoS has a pending received message (simulated CEO
//     chat). CoS runs and calls assignment_create for alice. The tracker
//     queues the assignment in alice's release inbox.
//  2. release 2: the assignment is delivered to alice's chat.jsonl as
//     an inbox_delivery. Alice runs and calls assignment_close. The tracker
//     queues the close in CoS's release inbox.
//  3. release 3: the close is delivered to CoS's chat.jsonl. The
//     round trip is closed and the assignment is closed on disk.
//
// Asserts on persisted state at each step — chat histories, release
// state inboxes, the assignment file.
func TestAgentToAgentRoundTripEndToEnd(t *testing.T) {
	srv, _ := newTurnServer(t)

	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "# cos role"); err != nil {
		t.Fatalf("seed cos: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# alice role"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	// Simulate the CEO dropping a message into CoS's chat (the
	// starting condition). This makes CoS "ready" on release 1.
	if err := srv.Store.AppendChatMessage("chief-of-staff", store.ChatMessage{
		Role:    store.RoleReceived,
		Content: "please ask alice to draft the Q3 report",
		Kind:    "direct_chat",
		TS:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed cos chat: %v", err)
	}

	// Each agent gets a fakeAgentPod that scripts a single chat-turn
	// per release: one tool_use_start + tool_use_end, the dispatched
	// tool_result, a delta, and done. The assignment tools run through the
	// same in-process dispatcher the state endpoint uses over UDS.
	createInput := []byte(`{"title":"Draft Q3 report","description":"Pull numbers for Q3 and write a 2-page summary.","assignee":"alice"}`)

	cosFake := installFakeAgentPod(t, srv, "chief-of-staff")
	cosFake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		if _, err := srv.Store.ReadAssignment(1); err == nil {
			// Already filed on an earlier round; nothing more to do.
			return []agentpod.TurnEvent{
				{Kind: agentpod.TurnEventDelta, Text: "Waiting on alice."},
				doneEvent(),
			}
		}
		out := assignmentToolDispatchResponse(t, srv, "chief-of-staff", mcp.AssignmentCreateToolName, "tu-cos-1", createInput)
		out = append(out,
			agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "Filed for alice."},
			doneEvent(),
		)
		return out
	})
	aliceFake := installFakeAgentPod(t, srv, "alice")
	aliceFake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		// Release-all wakes every agent, so alice's turn also fires on
		// the round that only queued work for her. She closes the
		// assignment once it has reached her.
		iss, err := srv.Store.ReadAssignment(1)
		if err != nil || !iss.Open() || iss.Assignee != "alice" {
			return []agentpod.TurnEvent{
				{Kind: agentpod.TurnEventDelta, Text: "Nothing to do yet."},
				doneEvent(),
			}
		}
		closeInput := []byte(`{"id":1,"resolution":"done","outcome":"First pass in artifacts/public/q3-draft.md."}`)
		out := assignmentToolDispatchResponse(t, srv, "alice", mcp.AssignmentCloseToolName, "tu-alice-1", closeInput)
		out = append(out,
			agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "Posted."},
			doneEvent(),
		)
		return out
	})

	// ---- release 1 — CoS files the assignment ---------------------------
	executeAndWait(t, srv, 2*time.Second)

	cosHist, _ := srv.Store.ReadChatHistory("chief-of-staff")
	if !hasSentContaining(cosHist, mcp.AssignmentCreateToolName) {
		t.Fatalf("expected CoS's assistant response to reference assignment_create; got %+v", cosHist)
	}
	ts, _ := srv.Store.ReadMessageQueue()
	if len(ts.Agents["alice"].Inbox) != 1 {
		t.Fatalf("alice turn-inbox should have 1 wake; got %v", ts.Agents["alice"].Inbox)
	}
	if wake := ts.Agents["alice"].Inbox[0]; !strings.Contains(wake, "assignment_event") {
		t.Errorf("queued path should be an assignment event; got %q", wake)
	}

	// ---- release 2 — alice receives the assignment, closes it -----
	executeAndWait(t, srv, 2*time.Second)

	aliceHist, _ := srv.Store.ReadChatHistory("alice")
	if !hasKind(aliceHist, "inbox_delivery") {
		t.Errorf("alice's chat should include the assignment as inbox_delivery; got %+v", aliceHist)
	}
	if !hasReceivedContaining(aliceHist, "chief-of-staff assigned you #1") {
		t.Errorf("alice's delivery should say who assigned what; got %+v", aliceHist)
	}
	if !hasSentContaining(aliceHist, mcp.AssignmentCloseToolName) {
		t.Errorf("alice's assistant response should reference assignment_close; got %+v", aliceHist)
	}
	ts, _ = srv.Store.ReadMessageQueue()
	if len(ts.Agents["chief-of-staff"].Inbox) != 1 {
		t.Fatalf("cos turn-inbox should have 1 wake after alice closed; got %v", ts.Agents["chief-of-staff"].Inbox)
	}

	// ---- release 3 — CoS receives the close ------------------------
	executeAndWait(t, srv, 2*time.Second)

	cosHist, _ = srv.Store.ReadChatHistory("chief-of-staff")
	if !hasReceivedContaining(cosHist, "alice closed #1") {
		t.Errorf("cos's chat should carry alice's close as inbox_delivery; got %+v", cosHist)
	}
	ts, _ = srv.Store.ReadMessageQueue()
	if len(ts.Agents["chief-of-staff"].Inbox) != 0 {
		t.Errorf("cos inbox should be drained after delivery; got %v", ts.Agents["chief-of-staff"].Inbox)
	}
	iss, err := srv.Store.ReadAssignment(1)
	if err != nil || iss.Open() || iss.Outcome == "" {
		t.Fatalf("assignment on disk = %+v, %v; want closed with an outcome", iss, err)
	}
}

// assignmentToolDispatchResponse scripts one assignment_* call the way a fake
// agent pod would: the tool runs through the same in-process
// dispatcher the state endpoint serves over UDS, and the events a
// pod would post for it follow.
func assignmentToolDispatchResponse(t *testing.T, srv *Server, slug, toolName, toolUseID string, input []byte) []agentpod.TurnEvent {
	t.Helper()
	body, isErr := mcp.DispatchStateToolInProcess(mcp.StateDispatchDeps{
		Store: srv.Store, Slug: slug, Tracker: srv.tracker(),
	}, toolName, json.RawMessage(input))
	return []agentpod.TurnEvent{
		{Kind: agentpod.TurnEventToolUseStart, ToolUseID: toolUseID, ToolName: toolName},
		{Kind: agentpod.TurnEventToolUseEnd, ToolUseID: toolUseID, ToolName: toolName, ToolInput: input},
		{Kind: agentpod.TurnEventToolResult, ToolUseID: toolUseID, ToolResultText: body, ToolResultIsError: isErr},
	}
}

func hasSentContaining(hist []store.ChatMessage, substr string) bool {
	for _, m := range hist {
		if m.Role == store.RoleSent && strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

func hasReceivedContaining(hist []store.ChatMessage, substr string) bool {
	for _, m := range hist {
		if m.Role == store.RoleReceived && strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

// executeAndWait POSTs /release-all and waits for every spawned chat
// loop to settle. Release-all is fire-and-forget on the server: the
// drain runs synchronously inside the handler, then per-agent chat
// loops fire async. We poll until no chat hub is actively running and
// the runtime is idle.
//
// Lingering hubs (loop done, kept in chatHubs for chatHubLingerDuration
// so late SSE subscribers can replay) don't count as in-flight: the
// agent isn't thinking, the buffer is flushed, the visible work is
// complete.
func executeAndWait(t *testing.T, srv *Server, timeout time.Duration) {
	t.Helper()
	rr := queuePost(t, srv, "/api/v1/queue/release-all", apitypes.QueueReleaseAllRequest{})
	if rr.Code != http.StatusOK {
		t.Fatalf("execute code = %d body=%s", rr.Code, rr.Body.String())
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !anyChatHubActive(srv) && (srv.Runtime == nil || !srv.Runtime.Running()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("release-all did not settle within %v", timeout)
}

// anyChatHubActive reports whether any slug has a non-completed
// chat hub. Used as the "is anyone still streaming?" gate in tests.
func anyChatHubActive(s *Server) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	for _, hub := range s.chatHubs {
		if !hub.isCompleted() {
			return true
		}
	}
	return false
}

func hasKind(hist []store.ChatMessage, kind string) bool {
	for _, m := range hist {
		if m.Kind == kind {
			return true
		}
	}
	return false
}
