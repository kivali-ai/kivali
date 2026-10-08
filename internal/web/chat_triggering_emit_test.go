package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// emitTriggeringChatMessages paints the message the agent is about to
// answer, so a browser that did not send it is not watching a reply to
// nothing. That covers every triggering kind, not only direct_chat: a
// background task's result — the one trigger no browser ever types —
// shows up live, not only after a refresh.

func triggeringKinds(t *testing.T, hist []store.ChatMessage) []string {
	t.Helper()
	hub := &chatHub{hub: newHub(), slug: "alice"}
	emitTriggeringChatMessages(hub, provider.MockProvider{}, t.TempDir(), hist)
	replay, _, _ := hub.subscribe()
	var kinds []string
	for _, ev := range replay {
		if ev.Kind != "chat_message" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatalf("payload: %v", err)
		}
		kinds = append(kinds, m["kind"].(string))
	}
	return kinds
}

func recvAt(kind string, min int) store.ChatMessage {
	return store.ChatMessage{
		Role: store.RoleReceived, Kind: kind, Content: kind + " body",
		TS: time.Date(2026, 9, 16, 18, min, 0, 0, time.UTC),
	}
}

// TestTriggeringEmitIncludesSubagentResult: a subagent result is
// painted live.
func TestTriggeringEmitIncludesSubagentResult(t *testing.T) {
	hist := []store.ChatMessage{
		{Role: store.RoleSent, Kind: "direct_chat", Content: "dispatching 2 tasks",
			TS: time.Date(2026, 9, 16, 18, 19, 0, 0, time.UTC)},
		recvAt(store.KindSubagentResult, 20),
		recvAt(store.KindSubagentResult, 22),
	}
	got := triggeringKinds(t, hist)
	want := []string{store.KindSubagentResult, store.KindSubagentResult}
	if len(got) != len(want) {
		t.Fatalf("emitted %v, want %v — the page would show the agent replying to "+
			"messages it never painted", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("emit[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTriggeringEmitOrderingIsOldestFirst: results land in file order,
// so a two-task batch reads top-to-bottom the way it does after a
// refresh.
func TestTriggeringEmitOrderingIsOldestFirst(t *testing.T) {
	hub := &chatHub{hub: newHub(), slug: "alice"}
	emitTriggeringChatMessages(hub, provider.MockProvider{}, t.TempDir(), []store.ChatMessage{
		{Role: store.RoleSent, Kind: "direct_chat", TS: time.Date(2026, 9, 16, 18, 19, 0, 0, time.UTC)},
		{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "first",
			TS: time.Date(2026, 9, 16, 18, 20, 0, 0, time.UTC)},
		{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "second",
			TS: time.Date(2026, 9, 16, 18, 22, 0, 0, time.UTC)},
	})
	replay, _, _ := hub.subscribe()
	var bodies []string
	for _, ev := range replay {
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err == nil {
			bodies = append(bodies, m["content"].(string))
		}
	}
	if len(bodies) != 2 || bodies[0] != "first" || bodies[1] != "second" {
		t.Errorf("bodies = %v, want [first second]", bodies)
	}
}

// TestTriggeringEmitStopsAtTheAgentsOwnTurn: walking back must stop at
// the agent's last output. Results it already answered are history, and
// re-emitting them would duplicate bubbles the page already has.
func TestTriggeringEmitStopsAtTheAgentsOwnTurn(t *testing.T) {
	hist := []store.ChatMessage{
		recvAt(store.KindSubagentResult, 10), // already answered
		{Role: store.RoleSent, Kind: "direct_chat", Content: "answered it",
			TS: time.Date(2026, 9, 16, 18, 11, 0, 0, time.UTC)},
		recvAt(store.KindSubagentResult, 20), // the new trigger
	}
	if got := triggeringKinds(t, hist); len(got) != 1 {
		t.Errorf("emitted %d messages, want 1 — entries before the agent's own "+
			"turn are already-answered history", len(got))
	}
}

// TestTriggeringEmitSkipsKindsThatRenderDifferently: inbox_delivery
// renders from the structured InboxView the transcript builds, and
// tool_result is plumbing inside a turn. Emitting either as a plain
// chat_message would paint a bubble that does not match the refreshed
// page.
func TestTriggeringEmitSkipsKindsThatRenderDifferently(t *testing.T) {
	hist := []store.ChatMessage{
		{Role: store.RoleSent, Kind: "direct_chat", TS: time.Date(2026, 9, 16, 18, 0, 0, 0, time.UTC)},
		recvAt("inbox_delivery", 10),
		recvAt("tool_result", 11),
		recvAt(store.KindSubagentResult, 12),
	}
	got := triggeringKinds(t, hist)
	for _, k := range got {
		if k == "inbox_delivery" || k == "tool_result" {
			t.Errorf("emitted %q, which the page renders by another path", k)
		}
	}
	if len(got) != 1 {
		t.Errorf("emitted %v, want only the subagent_result", got)
	}
}

// TestTriggeringEmitStillCoversDirectChat guards the original case.
func TestTriggeringEmitStillCoversDirectChat(t *testing.T) {
	hist := []store.ChatMessage{recvAt("direct_chat", 30)}
	if got := triggeringKinds(t, hist); len(got) != 1 || got[0] != "direct_chat" {
		t.Errorf("emitted %v, want [direct_chat]", got)
	}
}

// TestTriggeringEmitCarriesTheTaskResultRow: the page paints a task's
// result from the event's row, so it must be the row the chat API
// serves for the same message. A page that painted anything else would
// change under the person at the next fetch.
func TestTriggeringEmitCarriesTheTaskResultRow(t *testing.T) {
	srv := transcriptServer(t)
	writeSubagentMetaFile(t, srv, "aaaa1111", subagentMeta{ID: "aaaa1111", Description: "survey", Model: "claude-sonnet-4-6", Effort: "low", Status: "completed"})
	if err := srv.Store.AppendSubagentMessage("alice", "aaaa1111", store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: "Three vendors."}); err != nil {
		t.Fatal(err)
	}
	hist := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "and one more thing", TS: tsAt(1),
			Attachments: []store.MessageAttachment{{SHA: "abc123", Name: "quote.pdf"}}},
		{Role: store.RoleReceived, Kind: store.KindSubagentResult, Content: "Background task aaaa1111 (survey) finished.\n\nThree vendors.\n", TS: tsAt(2)},
	}
	hub := &chatHub{hub: newHub(), slug: "alice"}
	emitTriggeringChatMessages(hub, srv.Provider, srv.Store.Root(), hist)
	replay, _, _ := hub.subscribe()
	var events []apitypes.ChatMessageEvent
	for _, ev := range replay {
		if ev.Kind != "chat_message" {
			continue
		}
		var m apitypes.ChatMessageEvent
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatalf("payload: %v", err)
		}
		events = append(events, m)
	}
	if len(events) != 2 {
		t.Fatalf("emitted %d chat_message events, want 2", len(events))
	}

	typed := events[0]
	if typed.Row != nil || typed.Role != apitypes.MessageRoleReceived || typed.Content != "and one more thing" || typed.TS != msAt(1) ||
		len(typed.Attachments) != 1 || typed.Attachments[0] != (apitypes.PendingAttachment{SHA: "abc123", Name: "quote.pdf"}) {
		t.Errorf("direct_chat event = %+v", typed)
	}

	result := events[1]
	if result.Kind != store.KindSubagentResult || result.TS != msAt(2) {
		t.Errorf("result event = %+v", result)
	}
	got, err := json.Marshal(deref(t, "row", result.Row))
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(oneRow(t, srv.transcriptRows("alice", hist[1:]), apitypes.TranscriptKindTaskResult))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the event's row is not the chat API's:\n event %s\n   api %s", got, want)
	}
}
