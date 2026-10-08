package web

import (
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// What an open agent page needs from the server while a chat rotates.
// The page does not reload itself: it follows the stream and fetches
// the chat, so everything it shows comes from these two.
//
//   - Word that a new chat was asked for, on the stream (a chat_marker
//     of kind rotation) and in the chat API (rotating, and the
//     rotation_prompt row).
//   - A rotation_done that arrives only once the fresh chat can be
//     fetched as it will stay, and a generation that names it.

// rotationServer is alice with a chat worth rotating and a fake agent
// pod whose first turn waits for release; later turns end at once.
func rotationServer(t *testing.T) (srv *Server, fake *fakeAgentPod, release func()) {
	t.Helper()
	srv, _ = newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "hi"},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "hello"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	fake = installFakeAgentPod(t, srv, "alice")
	var turns atomic.Int32
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		if turns.Add(1) == 1 {
			<-gate
		}
		return []agentpod.TurnEvent{doneEvent()}
	})
	// Registered after the fake, so it runs first: a test that fails
	// mid-rotation must not leave the fake's Close waiting on the gate.
	t.Cleanup(release)
	return srv, fake, release
}

// chatHubOf is slug's hub, streaming or lingering.
func chatHubOf(t *testing.T, srv *Server, slug string) *chatHub {
	t.Helper()
	srv.streamMu.Lock()
	defer srv.streamMu.Unlock()
	hub := srv.chatHubs[slug]
	if hub == nil {
		t.Fatalf("no chat hub for %s", slug)
	}
	return hub
}

func getChat(t *testing.T, srv *Server, slug string) apitypes.Chat {
	t.Helper()
	rr := apiDo(t, srv, http.MethodGet, "/api/v1/agents/"+slug+"/chat", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET chat = %d: %s", rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Chat](t, rr)
}

// rotationPromptTS is the ts of the rotation prompt in slug's chat, in
// the unit the wire carries.
func rotationPromptTS(t *testing.T, srv *Server, slug string) int64 {
	t.Helper()
	hist, err := srv.Store.ReadChatHistory(slug)
	if err != nil {
		t.Fatalf("read chat: %v", err)
	}
	for _, m := range hist {
		if m.Kind == "rotation_prompt" {
			return m.TS.UnixMilli()
		}
	}
	t.Fatal("no rotation prompt in the chat")
	return 0
}

// TestNewChatIsAnnouncedToTheOpenPage: asking for a new chat is visible
// at once to a page that is already open, which only follows the stream,
// and to one that loads mid-rotation, which only reads the chat API.
// Both name the same row (the prompt's ts), so a page that gets both
// paints it once.
func TestNewChatIsAnnouncedToTheOpenPage(t *testing.T) {
	srv, fake, release := rotationServer(t)

	if c := getChat(t, srv, "alice"); c.Rotating || c.Generation != "" {
		t.Errorf("before: rotating/generation = %v/%q, want false and the first chat's empty generation", c.Rotating, c.Generation)
	}
	if rr := postNewChat(t, srv, "alice"); rr.Code != http.StatusOK {
		t.Fatalf("new-chat = %d: %s", rr.Code, rr.Body.String())
	}
	promptTS := rotationPromptTS(t, srv, "alice")

	// The stream: in the hub's replay by the time the POST has answered,
	// so the page that asked finds it when it opens the stream.
	marker, ok := hubEventByKind(chatHubOf(t, srv, "alice"), "chat_marker")
	if !ok {
		t.Fatal("the rotation turn's stream carries no chat_marker: an open page is never told a new chat was asked for")
	}
	if marker["kind"] != string(apitypes.MarkerKindRotation) {
		t.Errorf("chat_marker kind = %v, want %q", marker["kind"], apitypes.MarkerKindRotation)
	}
	if ts, _ := marker["ts"].(float64); int64(ts) != promptTS {
		t.Errorf("chat_marker ts = %v, want the rotation prompt's %d", marker["ts"], promptTS)
	}
	if content, _ := marker["content"].(string); content != "" {
		t.Errorf("chat_marker content = %q, want empty: the prompt is for the model", content)
	}

	// The chat API, mid-rotation.
	c := getChat(t, srv, "alice")
	if !c.Rotating {
		t.Error("rotating = false while the agent folds the chat into memory")
	}
	if c.Generation != "" {
		t.Errorf("generation = %q before the archive, want the old chat's", c.Generation)
	}
	if last := c.Rows[len(c.Rows)-1]; last.Kind != apitypes.TranscriptKindRotationPrompt || last.TS != promptTS {
		t.Errorf("last row = %s at %d, want rotation_prompt at %d", last.Kind, last.TS, promptTS)
	}

	release()
	fake.AwaitFinished(t, 5*time.Second)

	// The fresh chat.
	gens, err := srv.Store.ListArchivedChats("alice")
	if err != nil || len(gens) != 1 {
		t.Fatalf("archived generations = %v (%v), want exactly one", gens, err)
	}
	c = getChat(t, srv, "alice")
	if c.Rotating {
		t.Error("rotating = true after the rotation finished")
	}
	if c.Generation != gens[0].Timestamp {
		t.Errorf("generation = %q, want %q: the archive id of the chat before this one", c.Generation, gens[0].Timestamp)
	}
	if len(c.Rows) != 0 || c.Running {
		t.Errorf("fresh chat rows/running = %d/%v, want an empty idle chat", len(c.Rows), c.Running)
	}
}

// TestRotationDoneFollowsTheSettledChat: rotation_done is the hub's
// last event, sent once the hub is completed and the messages held
// during the rotation are in the fresh chat.
//
// The page fetches the chat the moment it hears rotation_done. Sent
// before the hub completed (as it was, while the page answered it with
// a full reload that took longer than the finalize), that fetch says
// the agent is still running, the page reopens the stream, and the hub
// replays the archived turn onto the screen that was just cleared.
func TestRotationDoneFollowsTheSettledChat(t *testing.T) {
	srv, fake, release := rotationServer(t)
	if rr := postNewChat(t, srv, "alice"); rr.Code != http.StatusOK {
		t.Fatalf("new-chat = %d: %s", rr.Code, rr.Body.String())
	}
	hub := chatHubOf(t, srv, "alice")

	// A page following the stream, noting the hub's state as it hears
	// rotation_done.
	_, live, cancel := hub.subscribe()
	defer close(cancel)
	completed := make(chan bool, 1)
	go func() {
		for ev := range live {
			if ev.Kind == "rotation_done" {
				completed <- hub.isCompleted()
				return
			}
		}
		close(completed)
	}()

	// A message sent while the chat is being folded waits for the fresh one.
	stageCEO(t, srv, "for the new chat")

	release()
	fake.AwaitFinished(t, 5*time.Second)
	// The held message woke the agent in its fresh chat.
	fake.AwaitFinished(t, 5*time.Second)

	kinds := hubEventKinds(hub)
	done := slices.Index(kinds, "done")
	delivered := slices.Index(kinds, "pending_delivered")
	rotated := slices.Index(kinds, "rotation_done")
	if done < 0 || delivered < 0 || rotated < 0 {
		t.Fatalf("stream = %v, want done, pending_delivered and rotation_done", kinds)
	}
	if done >= delivered || delivered >= rotated || rotated != len(kinds)-1 {
		t.Errorf("stream = %v, want done, then the held message's pending_delivered, then rotation_done last", kinds)
	}
	if wasCompleted, heard := <-completed; !heard {
		t.Error("the page following the stream never heard rotation_done")
	} else if !wasCompleted {
		t.Error("rotation_done reached the page before the hub was completed: a fetch now still says running")
	}

	gens, err := srv.Store.ListArchivedChats("alice")
	if err != nil || len(gens) != 1 {
		t.Fatalf("archived generations = %v (%v), want exactly one", gens, err)
	}
	c := getChat(t, srv, "alice")
	if c.Generation != gens[0].Timestamp || c.Rotating {
		t.Errorf("generation/rotating = %q/%v, want %q and false", c.Generation, c.Rotating, gens[0].Timestamp)
	}
	var said []string
	for _, r := range c.Rows {
		if r.Kind == apitypes.TranscriptKindMessage && r.BodyMD != nil {
			said = append(said, *r.BodyMD)
		}
	}
	if !slices.Equal(said, []string{"for the new chat"}) {
		t.Errorf("fresh chat says %v, want only the message held during the rotation", said)
	}
	if len(c.Pending) != 0 {
		t.Errorf("pending = %+v, want none: the held message was delivered", c.Pending)
	}
}

// TestFailedRotationTurnStillAnnouncesTheArchive: a rotation is
// finalized whichever way its turn ends, so a turn that fails says so
// in its error event the way done does. The page closes the stream on
// an error; told nothing, it would settle on the chat that is about to
// be archived and never hear rotation_done.
func TestFailedRotationTurnStillAnnouncesTheArchive(t *testing.T) {
	srv, fake, _ := rotationServer(t)
	fake.SetResponse([]agentpod.TurnEvent{{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonOtherError,
		FailedDetail: "the model could not be reached",
	}})
	if rr := postNewChat(t, srv, "alice"); rr.Code != http.StatusOK {
		t.Fatalf("new-chat = %d: %s", rr.Code, rr.Body.String())
	}
	fake.AwaitFinished(t, 5*time.Second)

	hub := chatHubOf(t, srv, "alice")
	failed, ok := hubEventByKind(hub, "error")
	if !ok {
		t.Fatalf("stream = %v, want an error event", hubEventKinds(hub))
	}
	if failed["rotation"] != true {
		t.Errorf("error.rotation = %v, want true (rotation pending)", failed["rotation"])
	}
	kinds := hubEventKinds(hub)
	if at := slices.Index(kinds, "rotation_done"); at != len(kinds)-1 || at < slices.Index(kinds, "error") {
		t.Errorf("stream = %v, want rotation_done last, after the error", kinds)
	}
	if gens, err := srv.Store.ListArchivedChats("alice"); err != nil || len(gens) != 1 {
		t.Errorf("archived generations = %v (%v), want exactly one", gens, err)
	}
	if c := getChat(t, srv, "alice"); len(c.Rows) != 0 || c.Rotating {
		t.Errorf("fresh chat rows/rotating = %d/%v, want an empty chat", len(c.Rows), c.Rotating)
	}
}

// TestErrorEventSaysNoRotationWhenNoneIsPending pins the other value,
// which is what the page closes the stream on.
func TestErrorEventSaysNoRotationWhenNoneIsPending(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	srv.finishFailedChatTurn(turnState(srv), agentpod.FailedReasonOtherError, "boom")
	failed, ok := hubEventByKind(hub, "error")
	if !ok {
		t.Fatalf("stream = %v, want an error event", hubEventKinds(hub))
	}
	if failed["rotation"] != false {
		t.Errorf("error.rotation = %v, want false (no rotation queued)", failed["rotation"])
	}
	if slices.Contains(hubEventKinds(hub), "rotation_done") {
		t.Errorf("stream = %v, want no rotation_done", hubEventKinds(hub))
	}
}
