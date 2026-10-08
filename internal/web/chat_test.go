package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// newChatServer wires a Server with a stub Claude client (so the
// "Claude not configured" 503 short-circuit doesn't fire) and an
// always-on AgentpodHub. Tests that drive a chat turn install a
// fakeAgentPod for their slug and script TurnEvents on it; tests that
// only check spawn-gate / 204 short-circuits don't need the fake.
func newChatServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.Claude = &provider.MockClient{}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{
		AgentModel: "mock-large-0",
	})
	srv.AgentpodHub = newAgentpodHub()
	return srv
}

// waitForChatIdle blocks until no chat loop is running for slug, or
// the timeout hits. Used by tests after a POST-triggered spawn so we
// can assert on the fully-persisted chat state without racing the
// background goroutine.
func waitForChatIdle(t *testing.T, srv *Server, slug string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !srv.ChatHubActive(slug) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("chat loop never settled for %s within %v", slug, timeout)
}

// waitFor polls cond until it returns true or timeout elapses. Fails
// the test on timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition never became true within %v", timeout)
}

func TestChatMessagePostAppends(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	installFakeAgentPod(t, srv, "alice")
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages",
		strings.NewReader("text=hello+alice"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	// The POST echoes the assigned ts so the browser can dedup its
	// optimistic bubble against the chat_message SSE event.
	var postResp struct {
		TS int64 `json:"ts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("decode post response: %v body=%s", err, rr.Body.String())
	}
	if postResp.TS == 0 {
		t.Errorf("expected non-zero ts in response, body=%s", rr.Body.String())
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) < 1 || hist[0].Role != store.RoleReceived || hist[0].Content != "hello alice" {
		t.Errorf("hist = %+v", hist)
	}
	// The POST also kicks off the fake agent pod's reply loop; drain
	// it before t.Cleanup so the async usage.jsonl write doesn't race
	// with TempDir removal.
	waitForChatIdle(t, srv, "alice", 2*time.Second)
}

func TestChatMessageMultipartWithAttachment(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	installFakeAgentPod(t, srv, "alice")
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("text", "here is the draft")
	fw, _ := w.CreateFormFile("attachment", "draft.md")
	_, _ = io.WriteString(fw, "# Draft\nbody\n")
	_ = w.Close()

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages", &buf)
	req.Header.Set("content-type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Attachments []store.MessageAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if len(resp.Attachments) != 1 || resp.Attachments[0].Name != "draft.md" || resp.Attachments[0].SHA == "" {
		t.Errorf("resp.Attachments = %+v", resp.Attachments)
	}

	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) != 1 {
		t.Fatalf("hist len = %d", len(hist))
	}
	if len(hist[0].Attachments) != 1 || hist[0].Attachments[0].SHA != resp.Attachments[0].SHA {
		t.Errorf("chat attachment not persisted: %+v", hist[0])
	}

	text, err := srv.Store.ReadAttachmentText(resp.Attachments[0].SHA)
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if !strings.Contains(text, "# Draft") {
		t.Errorf("canonical text = %q", text)
	}
	// Drain the async fake-agent-pod reply (which now writes
	// usage.jsonl on done) before cleanup.
	waitForChatIdle(t, srv, "alice", 2*time.Second)
}

func TestChatMessageRejectsEmpty(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages",
		strings.NewReader("text=%20%20"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rr.Code)
	}
}

func TestChatMessageCEORejected(t *testing.T) {
	srv := newChatServer(t)
	req := httptest.NewRequest(http.MethodPost, "/agents/ceo/messages",
		strings.NewReader("text=hi"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rr.Code)
	}
}

func TestChatMessagePostTriggersLoopAndPersists(t *testing.T) {
	// POSTing a chat message should spawn the response loop
	// synchronously (the HTTP response returns before the loop
	// finishes, but the spawn is kicked off before we 204). The fake
	// agent pod consumes the chat-turn event, POSTs back the scripted
	// deltas + done, and core persists the assistant reply to
	// chat.jsonl. This test locks in the contract that /stream is
	// subscribe-only and POST drives the work.
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	deltas := []string{"hel", "lo ", "there"}
	fake.SetResponseFunc(func(ct agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		req := requestFromTurn(t, ct)
		if req.Agent != "alice" {
			t.Errorf("agent = %q", req.Agent)
		}
		if req.Purpose != "chat" {
			t.Errorf("purpose = %q", req.Purpose)
		}
		if len(req.Tools) == 0 {
			t.Error("expected tools to be enabled in direct chat")
		}
		out := make([]agentpod.TurnEvent, 0, len(deltas)+1)
		for _, d := range deltas {
			out = append(out, agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: d})
		}
		out = append(out, doneEventWithUsage(10, 3))
		return out
	})

	// POST the user's message — this appends RoleReceived and kicks
	// off the response goroutine.
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages",
		strings.NewReader("text=hi"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("post code = %d, body = %s", rr.Code, rr.Body.String())
	}
	waitForChatIdle(t, srv, "alice", 2*time.Second)

	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) < 2 {
		t.Fatalf("expected user + assistant messages; hist = %+v", hist)
	}
	if hist[0].Role != store.RoleReceived || hist[0].Content != "hi" {
		t.Errorf("user message malformed: %+v", hist[0])
	}
	if hist[1].Role != store.RoleSent || hist[1].Content != "hello there" {
		t.Errorf("assistant message malformed: %+v", hist[1])
	}
	if hist[1].Kind != "direct_chat" {
		t.Errorf("kind = %q", hist[1].Kind)
	}
}

// TestChatMessagePostBroadcastsInboundMessage locks in the multi-device
// fix: when a message is sent from one device, a second browser watching
// the agent page (reconnected to /stream by the /org/stream reconciler
// when the agent flips to "thinking") must paint the user's bubble, not
// just the thinking indicator. That requires the spawn to emit a
// chat_message event into the hub's replay buffer for the inbound
// message. This test captures the hub replay at the moment the pod
// begins the turn — before any Checkpoint/TrimDeltas could prune it —
// and asserts the inbound message is present.
func TestChatMessagePostBroadcastsInboundMessage(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	gotMsg := make(chan map[string]any, 1)
	fake.SetResponseFunc(func(ct agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		// By the time the pod consumes the chat-turn the spawn has
		// already emitted the inbound message into the hub (the emit
		// runs before publishAgentpodChatTurn). Subscribe and scan the
		// replay buffer for it.
		srv.streamMu.Lock()
		hub := srv.chatHubs["alice"]
		srv.streamMu.Unlock()
		if hub != nil {
			replay, _, cancel := hub.subscribe()
			close(cancel)
			for _, ev := range replay {
				if ev.Kind == "chat_message" {
					var m map[string]any
					_ = json.Unmarshal(ev.Payload, &m)
					select {
					case gotMsg <- m:
					default:
					}
					break
				}
			}
		}
		return []agentpod.TurnEvent{doneEventWithUsage(1, 1)}
	})

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages",
		strings.NewReader("text=hello+from+phone"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("post code = %d, body = %s", rr.Code, rr.Body.String())
	}

	select {
	case m := <-gotMsg:
		if got, _ := m["content"].(string); got != "hello from phone" {
			t.Errorf("chat_message content = %q, want %q", got, "hello from phone")
		}
		if role, _ := m["role"].(string); role != "received" {
			t.Errorf("chat_message role = %q, want received", role)
		}
		if kind, _ := m["kind"].(string); kind != "direct_chat" {
			t.Errorf("chat_message kind = %q, want direct_chat", kind)
		}
		if ts, _ := m["ts"].(float64); ts == 0 {
			t.Errorf("chat_message ts missing/zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no chat_message event emitted for the inbound message")
	}
	waitForChatIdle(t, srv, "alice", 2*time.Second)
}

// TestChatLoopBuffersDeliveriesDuringLoop exercises a delivery that
// races a running response loop, and the buffering that handles it:
//
//  1. A response loop is running for an agent.
//  2. A received entry (instant CEO delivery) arrives via
//     deliverToAgent.
//  3. deliverToAgent sees the active hub and HOLDS the message in
//     pendingDeliveries. Crucially, nothing appears in chat.jsonl
//     yet — file order stays strictly append-canonical.
//  4. Loop #1 finishes. The defer flushes the buffer (landing the
//     delivery AFTER the prior reply in file order) and spawns a
//     follow-up loop.
//  5. The follow-up responds to the delivery.
//
// This test locks in three invariants at once:
//   - Chat.jsonl is NEVER interleaved (delivery doesn't appear
//     between loop #1's tool_use and final reply).
//   - UI order = file order = model order (all strictly append).
//   - A buffered delivery triggers a follow-up response.
func TestChatLoopBuffersDeliveriesDuringLoop(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")

	var callCount atomic.Int32
	// Buffered so the closure (runs on the fake pod's goroutine) hands
	// the captured request to the main goroutine without a data race.
	followupReqCh := make(chan provider.CompleteRequest, 1)
	releaseFirst := make(chan struct{})
	fake.SetResponseFunc(func(ct agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		if callCount.Add(1) == 1 {
			// Block the first call until the test is ready.
			<-releaseFirst
			return deltaResponse("first-reply")
		}
		// Follow-up call — capture the req so we can verify that the
		// mid-flight received was reordered to the tail.
		followupReqCh <- requestFromTurn(t, ct)
		return deltaResponse("second-reply")
	})

	// Kick off loop #1 by POSTing a user message.
	postBody := strings.NewReader("text=first-question")
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages", postBody)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST code = %d", rr.Code)
	}

	// Wait for loop #1 to actually be running (hub active).
	waitFor(t, 2*time.Second, func() bool { return srv.ChatHubActive("alice") })

	// Simulate an instant CEO delivery landing mid-flight. Routes
	// through deliverToAgent — which sees the active hub and holds
	// the message in pendingDeliveries. Nothing lands in chat.jsonl
	// yet. When loop #1 finishes, the defer flushes + spawns a
	// follow-up that responds.
	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Content: "second-question", Kind: "inbox_delivery",
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	srv.spawnChatLoopIfIdle("alice", "chat") // noop, hub active

	// Release loop #1.
	close(releaseFirst)

	// Wait for the follow-up loop's reply to land — this is the
	// terminal side effect of the race fix. Polling ChatHubActive
	// alone has a race: there's a tiny idle window between loop #1's
	// removeHub and loop #2's spawn, and a fast test poll can hit it
	// before loop #2 starts. Wait for "second-reply" to appear so we
	// know the whole chain ran.
	waitFor(t, 3*time.Second, func() bool {
		hist, _ := srv.Store.ReadChatHistory("alice")
		for _, m := range hist {
			if m.Role == store.RoleSent && strings.Contains(m.Content, "second-reply") {
				return true
			}
		}
		return false
	})
	waitForChatIdle(t, srv, "alice", 2*time.Second)

	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) < 4 {
		t.Fatalf("expected at least 4 entries (q1, reply1, q2, reply2); got %d: %+v", len(hist), hist)
	}
	if hist[len(hist)-1].Role != store.RoleSent {
		t.Errorf("chat should end on assistant reply; got %+v", hist[len(hist)-1])
	}
	// Follow-up loop must have run; the fake returns "second-reply".
	found := false
	for _, m := range hist {
		if m.Role == store.RoleSent && strings.Contains(m.Content, "second-reply") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("follow-up reply 'second-reply' not found in chat; got %+v", hist)
	}
	if callCount.Load() < 2 {
		t.Errorf("fake should have been called at least twice (first + follow-up); got %d", callCount.Load())
	}

	// Critical invariant: in the follow-up req, the mid-flight
	// "second-question" must be the LAST user-style message. Without
	// the reorder, it would sit buried between the prior tool flow
	// and the prior assistant reply, and the model would read the
	// reply as already addressing it.
	var followupReq provider.CompleteRequest
	select {
	case followupReq = <-followupReqCh:
	default:
		t.Fatal("follow-up request not captured")
	}
	var latestUserText string
	for i := len(followupReq.Messages) - 1; i >= 0; i-- {
		m := followupReq.Messages[i]
		if m.Role != provider.RoleUser {
			continue
		}
		for _, c := range m.Content {
			if c.Type == provider.ContentText && c.Text != "" {
				latestUserText = c.Text
				break
			}
		}
		if latestUserText != "" {
			break
		}
	}
	if !strings.Contains(latestUserText, "second-question") {
		t.Errorf("latest user text in follow-up req should be 'second-question' (mid-flight delivery reordered to tail); got %q", latestUserText)
	}

	// UI invariant: chat.jsonl on disk must already reflect the
	// canonical order. The UI renders the file directly, so if we
	// reordered only at request-build time the CEO would see one
	// order in the UI and the model would see another — exactly the
	// misleading swap we want to prevent.
	final, _ := srv.Store.ReadChatHistory("alice")
	// Expected file order: first-question, first-reply, then the
	// mid-flight second-question AFTER the reply, then the follow-
	// up's second-reply.
	var order []string
	for _, m := range final {
		if m.Kind == "direct_chat" || m.Kind == "inbox_delivery" {
			order = append(order, m.Content)
		}
	}
	if len(order) < 4 {
		t.Fatalf("expected at least 4 real entries in chat.jsonl (q1, r1, q2, r2); got %d: %v", len(order), order)
	}
	// Verify q2 appears AFTER r1 in file order.
	q2Idx, r1Idx := -1, -1
	for i, c := range order {
		if strings.Contains(c, "second-question") {
			q2Idx = i
		}
		if strings.Contains(c, "first-reply") {
			r1Idx = i
		}
	}
	if q2Idx < 0 || r1Idx < 0 {
		t.Fatalf("expected both first-reply and second-question in chat.jsonl; got %v", order)
	}
	if q2Idx <= r1Idx {
		t.Errorf("chat.jsonl file order wrong: second-question at %d must be AFTER first-reply at %d (UI and model must see the same order)", q2Idx, r1Idx)
	}
}

// TestChatLoopBuffersMultipleDeliveriesInArrivalOrder locks in the
// batching guarantee: if two deliveries arrive during one response,
// both are buffered and both land in chat.jsonl in arrival order
// when the loop ends. The follow-up loop sees both as user releases
// (via chatHistoryToMessages projection) and responds to them
// together rather than one-at-a-time.
func TestChatLoopBuffersMultipleDeliveriesInArrivalOrder(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "R"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")

	release := make(chan struct{})
	var callCount atomic.Int32
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		if callCount.Add(1) == 1 {
			<-release
			return deltaResponse("reply-1")
		}
		return deltaResponse("reply-2")
	})

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages", strings.NewReader("text=q1"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	authedHandler(t, srv).ServeHTTP(httptest.NewRecorder(), req)
	waitFor(t, 2*time.Second, func() bool { return srv.ChatHubActive("alice") })

	// Two deliveries arrive while loop #1 is blocked. Both must be
	// held — neither should appear in chat.jsonl until loop #1 ends.
	_ = srv.deliverToAgent("alice", store.ChatMessage{Role: store.RoleReceived, Content: "q2", Kind: "inbox_delivery"})
	_ = srv.deliverToAgent("alice", store.ChatMessage{Role: store.RoleReceived, Content: "q3", Kind: "inbox_delivery"})

	// Invariant: during the loop, chat.jsonl does NOT contain q2/q3.
	midHist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range midHist {
		if strings.Contains(m.Content, "q2") || strings.Contains(m.Content, "q3") {
			t.Errorf("mid-flight delivery leaked into chat.jsonl while loop was running: %+v", m)
		}
	}

	close(release)
	waitFor(t, 3*time.Second, func() bool {
		hist, _ := srv.Store.ReadChatHistory("alice")
		for _, m := range hist {
			if m.Role == store.RoleSent && strings.Contains(m.Content, "reply-2") {
				return true
			}
		}
		return false
	})
	waitForChatIdle(t, srv, "alice", 2*time.Second)

	hist, _ := srv.Store.ReadChatHistory("alice")
	q2Idx, q3Idx, reply1Idx := -1, -1, -1
	for i, m := range hist {
		switch {
		case strings.Contains(m.Content, "q2"):
			q2Idx = i
		case strings.Contains(m.Content, "q3"):
			q3Idx = i
		case strings.Contains(m.Content, "reply-1"):
			reply1Idx = i
		}
	}
	if reply1Idx < 0 || q2Idx < 0 || q3Idx < 0 {
		t.Fatalf("missing expected entries; hist = %+v", hist)
	}
	// Both buffered deliveries must appear AFTER loop #1's reply...
	if q2Idx <= reply1Idx {
		t.Errorf("q2 at %d must be after reply-1 at %d", q2Idx, reply1Idx)
	}
	// ...and in arrival order (q2 before q3).
	if q3Idx <= q2Idx {
		t.Errorf("q3 at %d must be after q2 at %d (arrival order preserved)", q3Idx, q2Idx)
	}
}

// TestChatLoopFollowupGatedByTimestamp locks in the anti-spin rule:
// if a loop finishes and the last received entry has the SAME (or
// older) timestamp as when the loop was spawned — meaning nothing
// new arrived mid-flight — a follow-up must NOT fire. Guards
// against empty-stream tests and error-mid-response spin.
func TestChatLoopFollowupGatedByTimestamp(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")

	var callCount atomic.Int32
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		callCount.Add(1)
		return []agentpod.TurnEvent{doneEvent()}
	})

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages", strings.NewReader("text=hi"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST code = %d", rr.Code)
	}
	waitForChatIdle(t, srv, "alice", 2*time.Second)
	// Give any errant follow-up a chance to fire.
	time.Sleep(100 * time.Millisecond)
	if callCount.Load() > 1 {
		t.Errorf("expected exactly 1 chat-turn (no spurious follow-up); got %d", callCount.Load())
	}
}

func TestAgentStreamNoHubReturns204(t *testing.T) {
	// /stream is subscribe-only. With no hub active (no recent
	// POST, no runtime activity), it must 204. Locks in "clicking
	// into an agent page never wakes the agent."
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Even with an unanswered RoleReceived sitting in chat.jsonl,
	// /stream must stay passive. The only legit spawn paths are POST
	// /messages, instant CEO delivery, and the runtime.
	_ = srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Content: "hi"})

	req := httptest.NewRequest(http.MethodGet, "/agents/alice/stream", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rr.Code)
	}
}

func TestChatStreamNoWorkReturns204(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Last message is sent → nothing to respond to.
	_ = srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Content: "hi"})
	_ = srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleSent, Content: "already responded"})

	req := httptest.NewRequest(http.MethodGet, "/agents/alice/stream", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("code = %d", rr.Code)
	}
}

func TestChatMessagePostWhenClaudeNil(t *testing.T) {
	// Claude-nil surfaces at POST /messages, since /stream is
	// subscribe-only: don't silently accept a message into chat.jsonl
	// that nothing can respond to.
	srv := newTestServer(t) // no Claude
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages",
		strings.NewReader("text=hi"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rr.Code)
	}
}

func TestChatPOSTSurvivesClientDisconnect(t *testing.T) {
	// The response loop runs in a detached goroutine — the HTTP
	// request context is not passed in, so a client disconnect
	// mid-processing never cancels the work. This test verifies
	// that the assistant response is still persisted even if the
	// POST request's own context is canceled the moment it returns
	// (nothing should depend on that ctx anyway).
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponse(deltaResponse("partial"))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/agents/alice/messages",
		strings.NewReader("text=hi"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	cancel() // simulate client walking away right after the POST returns.

	waitForChatIdle(t, srv, "alice", 2*time.Second)
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) < 2 {
		t.Fatalf("expected assistant message to persist; hist = %+v", hist)
	}
	if hist[1].Content != "partial" {
		t.Errorf("persisted = %q", hist[1].Content)
	}
}
