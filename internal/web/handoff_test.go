package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// postNewChat asks for a new chat through
// POST /api/v1/agents/{slug}/new-chat, the full stack included.
func postNewChat(t *testing.T, srv *Server, slug string) *httptest.ResponseRecorder {
	t.Helper()
	return apiDo(t, srv, http.MethodPost, "/api/v1/agents/"+slug+"/new-chat", "", nil)
}

func TestNewChatQueuesRotationAndArchivesOnCompletion(t *testing.T) {
	// The rotation flow is async: POST /new-chat appends a rotation
	// prompt to chat.jsonl, spawns the chat loop, and returns
	// immediately. When the loop completes, its post-completion hook
	// archives the prior chat and promotes the assistant's reply to
	// agent_memory.md. This test locks in that contract end to end.
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := srv.Store.WriteAgentMemory("alice", "# Old briefing\nstale notes\n"); err != nil {
		t.Fatalf("write briefing: %v", err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Content: "hi"},
		{Role: store.RoleSent, Content: "hello"},
		{Role: store.RoleReceived, Content: "how's Q3?"},
		{Role: store.RoleSent, Content: "Q3 is on track"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// The rotation flow asks the agent to curate their memory with
	// the normal agent_memory_* tools. Script a single
	// agent_memory_append tool call (the agent pod would call
	// memoryToolDispatchResponse internally to mutate live memory)
	// followed by a done so the turn wraps up cleanly.
	newMemoryBody := "# Updated briefing\n\nI know the Q3 plan.\n"
	toolInput, _ := json.Marshal(map[string]string{"text": newMemoryBody})
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		return memoryToolDispatchResponse(srv, "alice", "agent_memory_append", "tu-rotate-1", toolInput)
	})

	rr := postNewChat(t, srv, "alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Wait for the chat-loop goroutine to finish, including the
	// post-completion rotation hook. The defer order in
	// spawnChatLoopIfIdle runs finalizeRotation BEFORE removeHub so
	// "hub idle" is an authoritative "everything is done" signal.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.ChatHubActive("alice") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	live, err := srv.Store.ReadAgentMemory("alice")
	if err != nil {
		t.Fatalf("read briefing: %v", err)
	}
	if !strings.Contains(live, "Q3 plan") {
		t.Errorf("live briefing = %q", live)
	}

	// Fresh chat is empty (archive moved the prior content).
	fresh, _ := srv.Store.ReadChatHistory("alice")
	if len(fresh) != 0 {
		t.Errorf("fresh chat should be empty post-rotation, got %+v", fresh)
	}

	// Archive has the prior 4 messages + the rotation prompt + the
	// agent_memory_append tool_use + tool_result (>=7 total).
	pastChats, _ := srv.Store.ListArchivedChats("alice")
	if len(pastChats) != 1 {
		t.Fatalf("past chats = %+v", pastChats)
	}
	arch, err := srv.Store.ReadArchivedChat("alice", pastChats[0].Timestamp)
	if err != nil {
		t.Fatalf("ReadArchived: %v", err)
	}
	if len(arch) < 6 {
		t.Errorf("archived len = %d, want >=6 (4 prior + rotation prompt + agent_memory_append tool_use/result)", len(arch))
	}
	var sawPrompt, sawMemoryEdit bool
	for _, m := range arch {
		if m.Kind == "rotation_prompt" {
			sawPrompt = true
		}
		if m.Kind == "tool_use" && m.ToolName == "agent_memory_append" {
			sawMemoryEdit = true
		}
	}
	if !sawPrompt {
		t.Error("archive missing rotation_prompt entry")
	}
	if !sawMemoryEdit {
		t.Error("archive missing agent_memory_append — the memory edit the agent was asked to perform")
	}
}

// TestNewChatPreservesDeliveriesArrivingMidRotation locks in the
// "never drop a buffered message" invariant: if a delivery arrives
// via deliverToAgent while a rotation's chat loop is streaming the
// new memory, it must NOT be lost when the rotation archives the
// chat. It should land as entry [0] of the fresh post-rotation
// chat.jsonl, and a follow-up loop should spawn to respond to it.
//
// It also pins HOW it waits: the running turn is neither offered the
// message as a fold nor asked to wind up. The fake pod ignores fold
// offers, so without the observer below a fold would go unanswered and
// this test would pass while a real pod folded the message into the
// reconcile — answered in the transcript about to be archived, absent
// from the fresh chat.
func TestNewChatPreservesDeliveriesArrivingMidRotation(t *testing.T) {
	release := make(chan struct{})
	var callCount atomic.Int32
	srv, _ := newTurnServer(t)
	appendInput, _ := json.Marshal(map[string]string{"text": "# Updated memory\nnew content"})
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	// A second subscriber sees everything core publishes to the pod.
	observer := srv.AgentpodHub.Subscribe("alice")
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		switch callCount.Add(1) {
		case 1:
			// Rotation call: block until the test injects a delivery,
			// then fire an agent_memory_append tool call (the normal
			// memory-curation path the rotation prompt asks for) and
			// terminate. memoryToolDispatchResponse runs the actual
			// dispatch so live memory is updated before done.
			<-release
			return memoryToolDispatchResponse(srv, "alice", "agent_memory_append", "tu-rotate-1", appendInput)
		default:
			// Post-rotation follow-up chat loop (spawned by the buffer
			// flush). This is the call whose reply we actually assert
			// on below.
			return deltaResponse("post-rotation-reply")
		}
	})
	_ = srv.Store.WriteAgentMemory("alice", "# old memory")
	// Seed a prior exchange so the rotation has something to archive.
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Content: "earlier", TS: time.Unix(100, 0)},
		{Role: store.RoleSent, Content: "response", TS: time.Unix(101, 0)},
	} {
		_ = srv.Store.AppendChatMessage("alice", m)
	}

	// Kick off rotation via POST new-chat.
	rr := postNewChat(t, srv, "alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("new-chat code = %d", rr.Code)
	}
	waitFor(t, 2*time.Second, func() bool { return srv.ChatHubActive("alice") })

	// Inject a delivery WHILE the rotation's Claude call is blocked.
	// This is the exact race we're guarding against: the CEO
	// approves something / a peer task_request lands just as an
	// agent is mid-rotation.
	_ = srv.deliverToAgent("alice", store.ChatMessage{
		Role:    store.RoleReceived,
		Content: "mid-rotation-delivery",
		Kind:    "inbox_delivery",
	})

	// The reconcile turn was left alone: no fold offer, no cancel.
	// Drained before the observer is dropped, and dropped before the
	// turn is released so the later session-reset and follow-up
	// publishes are not held against a subscriber nobody reads.
	evs := drainAgentpodEvents(observer)
	srv.AgentpodHub.Unsubscribe(observer)
	if folds := foldedDeliveries(t, evs); len(folds) != 0 {
		t.Errorf("rotation turn was offered folds %v; a message must wait for the fresh chat", folds)
	}
	if ids := cancelledTurnIDs(t, evs); len(ids) != 0 {
		t.Errorf("rotation turn was asked to wind up (%v); the reconcile must run to its end", ids)
	}

	// Release the rotation's stream. Rotation finalizes (archives
	// chat, promotes memory), then the buffered delivery MUST be
	// flushed into the fresh chat.jsonl.
	close(release)

	// Wait for the post-rotation follow-up to produce its reply.
	waitFor(t, 3*time.Second, func() bool {
		hist, _ := srv.Store.ReadChatHistory("alice")
		for _, m := range hist {
			if strings.Contains(m.Content, "post-rotation-reply") {
				return true
			}
		}
		return false
	})
	waitForChatIdle(t, srv, "alice", 2*time.Second)

	// Fresh chat.jsonl must contain the buffered delivery — NOT
	// dropped — and the follow-up's reply.
	fresh, _ := srv.Store.ReadChatHistory("alice")
	var sawDelivery, sawReply bool
	for _, m := range fresh {
		if strings.Contains(m.Content, "mid-rotation-delivery") {
			sawDelivery = true
		}
		if strings.Contains(m.Content, "post-rotation-reply") {
			sawReply = true
		}
	}
	if !sawDelivery {
		t.Errorf("mid-rotation delivery was DROPPED — it must survive rotation and land in the fresh chat. got: %+v", fresh)
	}
	if !sawReply {
		t.Errorf("follow-up loop did not spawn after rotation flushed the buffer; got: %+v", fresh)
	}
	if callCount.Load() < 2 {
		t.Errorf("expected rotation call + follow-up call; got %d", callCount.Load())
	}
	// And it is in the fresh chat ONLY: the archive holds the reconcile,
	// never the message that waited for it.
	gens, _ := srv.Store.ListArchivedChats("alice")
	if len(gens) != 1 {
		t.Fatalf("archived generations = %d, want 1", len(gens))
	}
	archived, _ := srv.Store.ReadArchivedChat("alice", gens[0].Timestamp)
	for _, m := range archived {
		if strings.Contains(m.Content, "mid-rotation-delivery") {
			t.Errorf("mid-rotation delivery was archived with the old chat: %+v", m)
		}
	}
}

func TestNewChatEmptyHistoryNoop(t *testing.T) {
	srv, mc := newTurnServer(t)
	mc.CompleteFn = func(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
		t.Fatal("should not call Claude for empty chat")
		return nil, nil
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := postNewChat(t, srv, "alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	if got := decodeAPI[apitypes.NewChatResponse](t, rr); got.Started {
		t.Error("an empty chat reported a new chat started")
	}
	// Empty chat → no archive made, briefing untouched, no Claude call.
	pastChats, _ := srv.Store.ListArchivedChats("alice")
	if len(pastChats) != 0 {
		t.Errorf("expected no archive for empty chat, got %+v", pastChats)
	}
}

func TestNewChatRejectsCEO(t *testing.T) {
	srv, _ := newTurnServer(t)
	if rr := postNewChat(t, srv, "ceo"); rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rr.Code)
	}
}

func TestNewChatWithoutClaude(t *testing.T) {
	srv := newTestServer(t) // no Claude
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Content: "hi"})

	if rr := postNewChat(t, srv, "alice"); rr.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d", rr.Code)
	}
}

func TestBuildMemoryUpdateInstruction(t *testing.T) {
	cases := []struct {
		name          string
		bytes         int
		wantSizeCheck bool
		wantOver      bool
		wantKB        int
	}{
		{"small", 4 * 1024, false, false, 0},
		{"just-under-soft", memoryNudgeSoftBytes - 1, false, false, 0},
		{"at-soft", memoryNudgeSoftBytes, true, false, 16},
		{"between", 20 * 1024, true, false, 20},
		{"at-hard", memoryNudgeHardBytes, true, true, 32},
		{"well-over-hard", 50 * 1024, true, true, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const ts = "20260918T120000.000000000Z"
			got := buildMemoryUpdateInstruction(tc.bytes, ts)
			if !strings.HasPrefix(got, memoryUpdateInstructionFor(ts)) {
				t.Fatal("base instruction missing")
			}
			// The episode id must be filled in, never left as the
			// template placeholder — an agent citing [[ep:{{ts}}]]
			// has a citation nothing will ever resolve.
			if strings.Contains(got, "{{ts}}") || !strings.Contains(got, "[[ep:"+ts+"]]") {
				t.Fatalf("episode id not rendered into the prompt:\n%s", got)
			}
			hasNudge := strings.Contains(got, "Size check")
			if hasNudge != tc.wantSizeCheck {
				t.Fatalf("size check present = %v, want %v", hasNudge, tc.wantSizeCheck)
			}
			if !tc.wantSizeCheck {
				return
			}
			over := strings.Contains(got, "over budget")
			if over != tc.wantOver {
				t.Fatalf("over budget present = %v, want %v", over, tc.wantOver)
			}
			// Both tiers must prompt the agent to consider org change.
			lower := strings.ToLower(got)
			for _, want := range []string{"new agent", "split"} {
				if !strings.Contains(lower, want) {
					t.Errorf("missing phrase %q in nudge", want)
				}
			}
			if !strings.Contains(got, "~"+strings.TrimSpace(fmt.Sprintf("%d", tc.wantKB))+" KB") {
				t.Errorf("expected ~%d KB in message, got:\n%s", tc.wantKB, got)
			}
		})
	}
}
