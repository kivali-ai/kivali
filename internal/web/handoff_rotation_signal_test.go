package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestNewChatEmitsRotationSignal locks in the fix for "click New
// chat, stream ends, page shows stale state until manual refresh":
//   - the "done" SSE event payload carries "rotation": true while a
//     rotation is in flight, signalling the client to hold the stream
//     open past done;
//   - a subsequent "rotation_done" event fires after the server has
//     archived chat.jsonl + installed a fresh empty one, signalling
//     the client that a reload will reflect the new disk state.
//
// Implementation: the chat-turn done payload is decorated with
// "rotation": true via hasRotationPending in onAgentpodDone, and
// finalizeAgentpodTurn emits "rotation_done" after finalizeRotation.
//
// The test subscribes to /stream BEFORE POSTing /new-chat so it
// observes the events live — relying on the lingering-hub replay
// would mask the real contract (which is "live subscribers get the
// signals"). Since handleAgentStream now refuses to attach new
// connections to a completed hub, the subscribe-after-POST shape is
// genuinely unsupported anyway.
func TestNewChatEmitsRotationSignal(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := srv.Store.WriteAgentMemory("alice", "# Old\n"); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Content: "hi"},
		{Role: store.RoleSent, Content: "hello"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// Block the rotation's chat-turn until the test signals — gives
	// us a deterministic window to subscribe before any events fire.
	gate := make(chan struct{})
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		<-gate
		return []agentpod.TurnEvent{doneEvent()}
	})

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	noRedirect := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// POST first so a chat hub exists when we subscribe — the gate
	// above keeps the goroutine paused inside the Claude call, so no
	// terminal events are emitted yet. handleAgentStream finds the
	// in-flight (non-completed) hub and attaches us as a live sub.
	postReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/agents/alice/new-chat", nil)
	postReq.AddCookie(authCookie(t, srv))
	postReq.Header.Set("Sec-Fetch-Site", "same-origin")
	postResp, err := noRedirect.Do(postReq)
	if err != nil {
		t.Fatalf("post new-chat: %v", err)
	}
	_ = postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("new-chat code = %d", postResp.StatusCode)
	}
	// Wait for the chat-loop goroutine to claim the hub before we
	// subscribe. Without this, the subscribe could race with the
	// spawn and find no hub.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.ChatHubActive("alice") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !srv.ChatHubActive("alice") {
		t.Fatal("chat hub never became active after POST /new-chat")
	}

	streamCtx, streamCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer streamCancel()
	streamReq, _ := http.NewRequestWithContext(streamCtx, http.MethodGet, ts.URL+"/agents/alice/stream", nil)
	streamReq.AddCookie(authCookie(t, srv))
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("subscribe stream: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 (subscribed to in-flight hub)", streamResp.StatusCode)
	}

	// Release the Claude call now that we're subscribed live.
	close(gate)

	doneData, sawRotationDone := readUntilRotationDone(t, streamResp.Body)

	if doneData == nil {
		t.Fatal("never received a 'done' event on the rotation stream")
	}
	var doneMap map[string]any
	if err := json.Unmarshal(doneData, &doneMap); err != nil {
		t.Fatalf("unmarshal done payload: %v", err)
	}
	if rot, _ := doneMap["rotation"].(bool); !rot {
		t.Errorf("done.rotation = %v, want true", doneMap["rotation"])
	}
	if !sawRotationDone {
		t.Error("never received 'rotation_done' event after 'done' (client would rely on the 15s fallback reload instead)")
	}
}

// TestPostRotationReloadDoesNotReplayTerminalEvents: after
// rotation_done fires and the client reloads the page, the
// freshly-loaded page's GET /stream MUST NOT be attached to the
// (still-lingering) chat hub and replay its terminal events. Doing so
// would re-fire rotation_done on a page that's already post-rotation,
// triggering another reload, in a cycle that lasts until the linger
// eviction timer fires.
//
// Mechanic under test: handleAgentStream skips completed hubs, so a
// new connection finds either a fresh hub or no hub at all — never
// the dying lingering one.
func TestPostRotationReloadDoesNotReplayTerminalEvents(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := srv.Store.WriteAgentMemory("alice", "# Old\n"); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Content: "hi"},
		{Role: store.RoleSent, Content: "hello"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	installFakeAgentPod(t, srv, "alice")
	// Default fake response is a single done — same as the old
	// "StreamEnd then end_turn" stream.

	// Drive the rotation directly through the handler. The mock stream
	// is instant, so by the time the goroutine's post-completion
	// defer returns, the hub is closed AND marked completed AND still
	// in chatHubs awaiting its eviction-timer cleanup. That is exactly
	// the "post-rotation_done reload arrives" state we want to assert.
	postRR := postNewChat(t, srv, "alice")
	if postRR.Code != http.StatusOK {
		t.Fatalf("new-chat code = %d, body = %s", postRR.Code, postRR.Body.String())
	}
	// Wait for the chat-loop goroutine to drain. ChatHubActive flips
	// false once the hub is marked completed (still lingering, but no
	// longer "active").
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !srv.ChatHubActive("alice") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if srv.ChatHubActive("alice") {
		t.Fatal("chat hub never completed after rotation")
	}

	// Sanity check: the hub IS still in chatHubs (linger window has
	// not yet expired) AND it IS completed. Without those, this test
	// would be exercising the wrong state.
	srv.streamMu.Lock()
	hub := srv.chatHubs["alice"]
	srv.streamMu.Unlock()
	if hub == nil {
		t.Fatal("setup: hub already evicted; can't exercise the lingering-hub path")
	}
	if !hub.isCompleted() {
		t.Fatal("setup: hub is not completed; this isn't the lingering state")
	}

	// The assertion: a fresh GET /stream — the request a page fires
	// right after reloading on rotation_done — must NOT attach to the
	// lingering hub (a 200 replaying "done" + "rotation_done" would
	// loop the reload). handleAgentStream treats the completed hub as
	// absent and returns 204.
	streamReq := httptest.NewRequest(http.MethodGet, "/agents/alice/stream", nil)
	streamRR := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(streamRR, streamReq)
	if streamRR.Code != http.StatusNoContent {
		t.Errorf("post-rotation /stream code = %d, want 204; body = %q",
			streamRR.Code, streamRR.Body.String())
	}
	if body := streamRR.Body.String(); strings.Contains(body, "rotation_done") || strings.Contains(body, "rotation\":true") {
		t.Errorf("post-rotation /stream replayed terminal events to a fresh subscriber; body = %q", body)
	}
}

// readUntilRotationDone walks SSE frames out of an httptest stream
// body, returning the first 'done' payload and whether 'rotation_done'
// was observed afterward. Returns when both are seen or the body
// closes.
func readUntilRotationDone(t *testing.T, body interface{ Read(p []byte) (int, error) }) ([]byte, bool) {
	t.Helper()
	reader := bufio.NewReader(body)
	var (
		doneData        []byte
		sawRotationDone bool
		curEvent        string
	)
	// Run the read loop in its own goroutine so a misbehaving stream
	// (no events at all) doesn't hang the test past its context
	// deadline.
	type result struct {
		done []byte
		rot  bool
	}
	resCh := make(chan result, 1)
	var once sync.Once
	send := func(r result) { once.Do(func() { resCh <- r }) }
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				send(result{done: doneData, rot: sawRotationDone})
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				curEvent = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				payload := strings.TrimPrefix(line, "data: ")
				if curEvent == "done" && doneData == nil {
					doneData = []byte(payload)
				}
				if curEvent == "rotation_done" {
					sawRotationDone = true
				}
			case line == "":
				curEvent = ""
			}
			if doneData != nil && sawRotationDone {
				send(result{done: doneData, rot: sawRotationDone})
				return
			}
		}
	}()
	r := <-resCh
	return r.done, r.rot
}
