package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// A peer publishes a decision about alice; alice's next wake carries a
// wake_update entry behind the message that woke her, the model's
// request carries the same text in its last user message, and the
// entry never becomes a reason to wake again.
func TestWakeAppendsTheNoteBehindTheDelivery(t *testing.T) {
	srv := newChatServer(t)
	for _, a := range []store.Agent{
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Engineer", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	fake := installFakeAgentPod(t, srv, "alice")
	// Reply with text so each turn closes with a RoleSent entry; the
	// spawn-gate assertion at the end needs a genuinely answered chat.
	fake.SetResponse(deltaResponse("ok"))

	// First wake: bootstraps alice's watermark silently, and she holds
	// no assignments, so there is no note at all.
	sendMessage(t, srv, fake, "hello")
	hist, _ := srv.Store.ReadChatHistory("alice")
	for _, m := range hist {
		if m.Kind == store.KindWakeUpdate {
			t.Fatalf("first wake must not carry a note: %+v", m)
		}
	}

	// Bob publishes a decision about alice between her turns: the file
	// lands in his published tree and the graph maintainer is told, as
	// artifact_publish does.
	walks := srv.Store.Graph().Walks()
	pub := files.PublishedDir(srv.Store.Root(), "bob")
	if err := os.MkdirAll(pub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pub, "scope.md"), []byte("---\nid: alice-scope\nkind: decision\nabout: alice\n---\nAlice owns the market model.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.Store.Graph().Published("bob", []string{"scope.md"}); err != nil {
		t.Fatal(err)
	}

	req := sendMessage(t, srv, fake, "second")
	// Neither the message POST, the top of the turn nor its end walked
	// the published trees: the note came from the maintained index.
	if got := srv.Store.Graph().Walks(); got != walks {
		t.Errorf("a message and its turn walked the published trees %d times", got-walks)
	}
	hist, _ = srv.Store.ReadChatHistory("alice")
	trailerIdx, messageIdx := -1, -1
	for i, m := range hist {
		if m.Kind == store.KindWakeUpdate {
			trailerIdx = i
		}
		if m.Kind == "direct_chat" && m.Content == "second" {
			messageIdx = i
		}
	}
	if trailerIdx < 0 || messageIdx < 0 || trailerIdx < messageIdx {
		t.Fatalf("note should follow the waking message: note=%d message=%d hist=%+v", trailerIdx, messageIdx, hist)
	}
	if !strings.Contains(hist[trailerIdx].Content, "bob/alice-scope  decision current v1 · owner bob · about alice") {
		t.Errorf("note content:\n%s", hist[trailerIdx].Content)
	}
	// The model saw it: the last user message carries the CEO's text
	// and the note together.
	last := lastUserText(req)
	if !strings.Contains(last, "second") || !strings.Contains(last, agent.WakeUpdateLead) {
		t.Errorf("last user message should carry both the message and the note:\n%s", last)
	}
	// And the note alone is no reason to wake.
	if v := store.SpawnDecision(hist); v != store.SpawnIdle {
		t.Errorf("after the turn, spawn decision = %v, want idle", v)
	}
}

// Whatever wakes alice, the note behind the deliveries lists what she
// holds, once per wake; the delivery itself carries nothing of the
// sort. The first wake, which sets the graph watermark silently, lists
// it too, and so does every wake after: it is state, not news.
func TestWakeNoteListsOpenAssignmentsOncePerWake(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatal(err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponse(deltaResponse("ok"))
	now := time.Now().UTC()
	if err := srv.Store.WriteAssignment(&assignments.Assignment{
		ID: 1, Title: "Draft the release notes", Status: assignments.StatusOpen, Assignee: "alice", Creator: "chief-of-staff", Created: now, Updated: now,
		Log: []assignments.Entry{{Seq: 1, TS: now, By: "chief-of-staff", Op: assignments.OpCreated}},
	}); err != nil {
		t.Fatal(err)
	}
	const held = "Your open assignments:\n- #1 \"Draft the release notes\" (ready)"
	notes := func(hist []store.ChatMessage) int {
		n := 0
		for _, m := range hist {
			switch {
			case m.Kind == store.KindWakeUpdate:
				n++
				if !strings.Contains(m.Content, held) {
					t.Errorf("note does not list what alice holds:\n%s", m.Content)
				}
			case m.Role == store.RoleReceived && strings.Contains(m.Content, "open assignments"):
				t.Errorf("a delivery repeats what alice holds: %+v", m)
			}
		}
		return n
	}
	req := sendMessage(t, srv, fake, "hello")
	hist, _ := srv.Store.ReadChatHistory("alice")
	if n := notes(hist); n != 1 {
		t.Fatalf("first wake wrote %d notes, want 1: %+v", n, hist)
	}
	last := lastUserText(req)
	if !strings.Contains(last, "hello") || !strings.Contains(last, held) {
		t.Errorf("last user message should carry the message and what alice holds:\n%s", last)
	}
	sendMessage(t, srv, fake, "again")
	hist, _ = srv.Store.ReadChatHistory("alice")
	if n := notes(hist); n != 2 {
		t.Fatalf("second wake: %d notes in the chat, want 2: %+v", n, hist)
	}
}

// A runtime disruption asks for one resume. Once the resumed turn has
// answered, a broadcast wake (release-all, boot recovery) must find
// nothing to do; before the gate learned this, every broadcast
// respawned the agent to answer its own trailer again.
func TestAnsweredResumeDoesNotRespawnOnABroadcastWake(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatal(err)
	}
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponse(deltaResponse("ok"))
	sendMessage(t, srv, fake, "hello")
	// The runtime died mid-turn and the boot scan wrote the marker.
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Kind: store.KindRuntimeDisruption, Content: "died"}); err != nil {
		t.Fatal(err)
	}
	if !srv.spawnChatLoopIfIdle("alice", "release") {
		t.Fatal("a fresh disruption should spawn the resume")
	}
	fake.AwaitTurn(t, 5*time.Second)
	fake.AwaitFinished(t, 5*time.Second)
	waitForChatIdle(t, srv, "alice", 5*time.Second)
	if srv.spawnChatLoopIfIdle("alice", "release") {
		t.Fatal("the resume was answered; a broadcast wake must not respawn")
	}
}

// Deliveries that arrive while a spawn holds the hub are buffered
// against a turn. If that turn never starts (here: no pod is
// listening), the buffer must still reach chat.jsonl rather than wait
// for some later turn's teardown and land out of order.
func TestAbandonedSpawnFlushesBufferedDeliveries(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "first"}); err != nil {
		t.Fatal(err)
	}
	// What deliverToAgent would have done during the claim window.
	srv.streamMu.Lock()
	if srv.pendingDeliveries == nil {
		srv.pendingDeliveries = map[string][]bufferedDelivery{}
	}
	srv.pendingDeliveries["alice"] = []bufferedDelivery{{id: "x", msg: store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "late"}}}
	srv.streamMu.Unlock()
	// No fake pod is installed, so the publish finds no subscriber.
	if srv.spawnChatLoopIfIdle("alice", "chat") {
		t.Fatal("no subscriber; spawn should report false")
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	found := false
	for _, m := range hist {
		if m.Content == "late" {
			found = true
		}
	}
	if !found {
		t.Errorf("buffered delivery never reached chat.jsonl: %+v", hist)
	}
	srv.streamMu.Lock()
	left := len(srv.pendingDeliveries["alice"])
	srv.streamMu.Unlock()
	if left != 0 {
		t.Errorf("%d deliveries still stranded in the buffer", left)
	}
}

// The note reaches the live chat and the archived chat as a
// wake_update row, through the same API the web app reads.
func TestWakeUpdateEntryRendersLiveAndArchived(t *testing.T) {
	srv := newChatServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "hello"},
		{Role: store.RoleReceived, Kind: store.KindWakeUpdate, Content: agent.WakeUpdateLead + "\n\nThe handbook changed."},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "reply"},
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "more"},
		{Role: store.RoleReceived, Kind: store.KindWakeUpdate, Content: agent.WakeUpdateLead + "\n\nYour open assignments:\n- #1 \"Work\" (ready)"},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "reply"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatal(err)
		}
	}
	wakeRows := func(rows []apitypes.TranscriptRow) int {
		n := 0
		for _, r := range rows {
			if r.Kind == apitypes.TranscriptKindWakeUpdate {
				n++
			}
		}
		return n
	}
	live := apiDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chat", "", nil)
	if live.Code != http.StatusOK {
		t.Fatalf("live chat: code %d", live.Code)
	}
	if n := wakeRows(decodeAPI[apitypes.Chat](t, live).Rows); n != 2 {
		t.Errorf("live chat: %d wake_update rows, want 2", n)
	}
	ts, err := srv.Store.ArchiveChat("alice")
	if err != nil {
		t.Fatal(err)
	}
	archived := apiDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chats/"+ts, "", nil)
	if archived.Code != http.StatusOK {
		t.Fatalf("archived chat: code %d", archived.Code)
	}
	if n := wakeRows(decodeAPI[apitypes.PastChatDetail](t, archived).Rows); n != 2 {
		t.Errorf("archived chat: %d wake_update rows, want 2", n)
	}
}

func lastUserText(req provider.CompleteRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != provider.RoleUser {
			continue
		}
		var parts []string
		for _, c := range req.Messages[i].Content {
			if c.Type == provider.ContentText {
				parts = append(parts, c.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
