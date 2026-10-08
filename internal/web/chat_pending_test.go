package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Pending messages: a CEO message sent while the agent is mid-turn is
// shown as pending until the model has it, can be deleted until it is
// handed to the turn, restored within ten seconds, or sent now by
// pausing the turn. See docs/developers/context-serialization.md.

// pendingEvent is one replayed hub event, decoded.
type pendingEvent struct {
	kind    string
	payload map[string]any
}

// hubEvents replays every event the hub has broadcast, in order.
func hubEvents(t *testing.T, hub *chatHub) []pendingEvent {
	t.Helper()
	replay, _, cancel := hub.subscribe()
	close(cancel)
	out := make([]pendingEvent, 0, len(replay))
	for _, ev := range replay {
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatalf("payload of %s: %v", ev.Kind, err)
		}
		out = append(out, pendingEvent{kind: ev.Kind, payload: m})
	}
	return out
}

// eventSeq is the kinds of the replayed events that are in keep, in
// order, each suffixed with the id or content that names its row so
// the order of events FOR THE SAME message is visible.
func eventSeq(t *testing.T, hub *chatHub, keep ...string) []string {
	t.Helper()
	want := map[string]bool{}
	for _, k := range keep {
		want[k] = true
	}
	var out []string
	for _, ev := range hubEvents(t, hub) {
		if !want[ev.kind] {
			continue
		}
		label := ev.kind
		switch {
		case ev.payload["id"] != nil:
			label += ":" + ev.payload["id"].(string)
		case ev.payload["content"] != nil:
			label += ":" + ev.payload["content"].(string)
		}
		out = append(out, label)
	}
	return out
}

// pendingServer is the mid-turn fixture: alice has a streaming hub and
// an agent-pod turn (turn-1), a fake clock, and an agent-pod hub whose
// published events the returned subscriber sees.
func pendingServer(t *testing.T) (*Server, *chatHub, *agentpodSubscriber, *clock.Fake) {
	t.Helper()
	_, srv, hub, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	t.Cleanup(cleanup)
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")
	clk := clock.NewFakeAt(time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	srv.Clock = clk
	return srv, hub, sub, clk
}

// stageCEO stages a CEO direct chat the way the POST handler does.
func stageCEO(t *testing.T, srv *Server, text string) string {
	t.Helper()
	id, err := srv.deliverOrStage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: text,
	}, true)
	if err != nil {
		t.Fatalf("stage %q: %v", text, err)
	}
	if id == "" {
		t.Fatalf("stage %q: went straight to chat.jsonl; want staged", text)
	}
	return id
}

// holdFolds makes the running turn refuse fold offers, the state in
// which a pending message stays deletable (a wind-up already asked for,
// a rotation, or no fold-capable transport).
func holdFolds(srv *Server) {
	srv.streamMu.Lock()
	srv.agentpodTurns["alice"].preemptRequested = true
	srv.streamMu.Unlock()
}

func turnState(srv *Server) *agentpodTurnState {
	srv.streamMu.Lock()
	defer srv.streamMu.Unlock()
	return srv.agentpodTurns["alice"]
}

// receivedChat is the content of every received direct_chat and
// paused-to-deliver row plus the agent's text rows, in file order.
func chatRows(t *testing.T, srv *Server) []string {
	t.Helper()
	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("read chat: %v", err)
	}
	var out []string
	for _, m := range hist {
		switch {
		case m.Kind == store.KindPausedToDeliver:
			out = append(out, "marker:"+m.Content)
		case m.Kind == "direct_chat" && m.Role == store.RoleSent:
			out = append(out, "agent:"+m.Content)
		case m.Kind == "direct_chat":
			out = append(out, "ceo:"+m.Content)
		}
	}
	return out
}

func pendingMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	srv.wireAPIPendingRoutes(mux)
	return mux
}

func pendingDo(t *testing.T, srv *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	pendingMux(srv).ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	return rr
}

func errorBody(t *testing.T, rr *httptest.ResponseRecorder) apitypes.ErrorBody {
	t.Helper()
	var b apitypes.ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("error body %q: %v", rr.Body.String(), err)
	}
	if b.Error == "" || b.Who == "" {
		t.Errorf("error body %q lacks error or who", rr.Body.String())
	}
	return b
}

func postMessage(t *testing.T, srv *Server, text string) apitypes.MessagePostResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages", strings.NewReader("text="+text))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST messages: %d %s", rr.Code, rr.Body.String())
	}
	var resp apitypes.MessagePostResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return resp
}

// The POST answers pending_id when the message waits behind a turn, and
// the stream announces it with the same id.
func TestMessagePostReturnsPendingIDWhenStaged(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	srv.Claude = &provider.MockClient{}

	resp := postMessage(t, srv, "while+you+work")
	if resp.PendingID == "" || resp.TS == 0 {
		t.Fatalf("response = %+v, want pending_id and ts", resp)
	}
	if rows := chatRows(t, srv); len(rows) != 0 {
		t.Errorf("chat.jsonl = %v; a pending message must not be in it", rows)
	}
	ev, ok := hubEventByKind(hub, "pending_message")
	if !ok {
		t.Fatal("no pending_message event")
	}
	if ev["id"] != resp.PendingID || ev["text"] != "while you work" || ev["queued_at"] != float64(resp.TS) {
		t.Errorf("pending_message = %v, want id %s, the text, queued_at = ts %d", ev, resp.PendingID, resp.TS)
	}
	if atts, ok := ev["attachments"].([]any); !ok || len(atts) != 0 {
		t.Errorf("pending_message.attachments = %v, want []", ev["attachments"])
	}
	// Offered to the running turn at once, so no longer deletable.
	if got := eventSeq(t, hub, "pending_message", "pending_offered"); strings.Join(got, ",") !=
		"pending_message:"+resp.PendingID+",pending_offered:"+resp.PendingID {
		t.Errorf("events = %v, want pending_message then pending_offered", got)
	}
}

// Idle: written straight in, no pending_id key at all.
func TestMessagePostOmitsPendingIDWhenDelivered(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	srv.Claude = &provider.MockClient{}
	hub.markCompleted()
	srv.streamMu.Lock()
	delete(srv.agentpodTurns, "alice")
	srv.streamMu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages", strings.NewReader("text=hello"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "pending_id") || strings.Contains(rr.Body.String(), "attachments") {
		t.Errorf("idle response = %s, want only ts", rr.Body.String())
	}
	if rows := chatRows(t, srv); len(rows) != 1 || rows[0] != "ceo:hello" {
		t.Errorf("chat.jsonl = %v, want [ceo:hello]", rows)
	}
}

func postMessageJSON(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

// A JSON body {"text": ...} is taken like the form: staged behind a
// running turn with a pending_id, on both the page route and the API
// alias; written straight in when idle. A bad body is a 400 in the
// API's error shape.
func TestMessagePostAcceptsJSON(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	srv.Claude = &provider.MockClient{}

	for _, path := range []string{"/agents/alice/messages", "/api/v1/agents/alice/messages"} {
		rr := postMessageJSON(t, srv, path, `{"text":"  while you work  "}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: code %d %s", path, rr.Code, rr.Body.String())
		}
		var resp apitypes.MessagePostResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decode %q: %v", path, rr.Body.String(), err)
		}
		if resp.PendingID == "" || resp.TS == 0 {
			t.Errorf("%s: response = %+v, want pending_id and ts", path, resp)
		}
	}
	ev, ok := hubEventByKind(hub, "pending_message")
	if !ok || ev["text"] != "while you work" {
		t.Errorf("pending_message = %v, want the trimmed text", ev)
	}
	if rows := chatRows(t, srv); len(rows) != 0 {
		t.Errorf("chat.jsonl = %v; pending messages must not be in it", rows)
	}

	e := errorBody(t, postMessageJSON(t, srv, "/api/v1/agents/alice/messages", `{"text":"x","attachment":"y"}`))
	if !strings.Contains(e.Error, "not valid") {
		t.Errorf("unknown field: error = %q", e.Error)
	}
	if rr := postMessageJSON(t, srv, "/agents/alice/messages", `{"text":"  "}`); rr.Code != http.StatusBadRequest {
		t.Errorf("blank text: code %d, want 400", rr.Code)
	}

	hub.markCompleted()
	srv.streamMu.Lock()
	delete(srv.agentpodTurns, "alice")
	srv.streamMu.Unlock()
	rr := postMessageJSON(t, srv, "/agents/alice/messages", `{"text":"hello"}`)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "pending_id") {
		t.Fatalf("idle: code %d %s, want 200 without pending_id", rr.Code, rr.Body.String())
	}
	// hello is written, and the two staged messages land with it once
	// the turn is gone; the refused bodies left nothing behind.
	rows := chatRows(t, srv)
	if len(rows) != 3 || !slices.Contains(rows, "ceo:hello") {
		t.Errorf("chat.jsonl = %v, want ceo:hello and the two staged messages", rows)
	}
}

// A fold that lands retires the pending bubble BEFORE the chat_message
// that paints the row, on the same hub.
func TestPendingDeliveredPrecedesChatMessageOnFold(t *testing.T) {
	srv, hub, sub, _ := pendingServer(t)
	a := stageCEO(t, srv, "first")
	b := stageCEO(t, srv, "second")
	folds := foldedDeliveries(t, drainAgentpodEvents(sub))
	if len(folds) != 2 || folds[0][1] != a || folds[1][1] != b {
		t.Fatalf("fold offers = %v, want [%s %s]", folds, a, b)
	}

	srv.onAgentpodFolded(turnState(srv), agentpod.TurnEvent{Kind: agentpod.TurnEventFolded, DeliveryID: a, Landed: true})

	got := eventSeq(t, hub, "pending_message", "pending_offered", "pending_delivered", "chat_message")
	want := []string{
		"pending_message:" + a, "pending_offered:" + a,
		"pending_message:" + b, "pending_offered:" + b,
		"pending_delivered:" + a, "chat_message:first",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events =\n %v\nwant\n %v", got, want)
	}
	ev, _ := hubEventByKind(hub, "pending_delivered")
	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) != 1 || ev["ts"] != float64(hist[0].TS.UnixMilli()) {
		t.Errorf("pending_delivered.ts = %v, want the row's ts", ev["ts"])
	}
	if p := srv.pendingMessagesFor("alice"); len(p) != 1 || p[0].ID != b || p[0].Deletable {
		t.Errorf("pendingMessagesFor = %+v, want only %s, not deletable", p, b)
	}
}

// At the end of a turn every visible buffered row is retired on the
// dying hub; invisible system deliveries flush silently as before.
func TestPendingDeliveredAtTurnEnd(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	holdFolds(srv)
	a := stageCEO(t, srv, "from the ceo")
	if err := srv.deliverToAgent("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "inbox_delivery", Content: "from a peer",
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if p := srv.pendingMessagesFor("alice"); len(p) != 1 || p[0].ID != a || !p[0].Deletable {
		t.Fatalf("pendingMessagesFor = %+v, want only the CEO's, deletable", p)
	}

	srv.onAgentpodFailed(turnState(srv), agentpod.TurnEvent{Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonCancelled})

	got := eventSeq(t, hub, "pending_message", "pending_offered", "pending_delivered", "pending_deleted")
	want := []string{"pending_message:" + a, "pending_delivered:" + a}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", got, want)
	}
	if rows := chatRows(t, srv); len(rows) != 1 || rows[0] != "ceo:from the ceo" {
		t.Errorf("chat rows = %v", rows)
	}
	if p := srv.pendingMessagesFor("alice"); p == nil || len(p) != 0 {
		t.Errorf("pendingMessagesFor after flush = %#v, want empty non-nil", p)
	}
}

func TestPendingMessagesForNeverNil(t *testing.T) {
	srv := newTestServer(t)
	if p := srv.pendingMessagesFor("nobody"); p == nil {
		t.Error("pendingMessagesFor returned nil")
	}
}

func TestPendingDelete(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	holdFolds(srv)
	id := stageCEO(t, srv, "never mind")

	rr := pendingDo(t, srv, http.MethodDelete, "/api/v1/agents/alice/pending/"+id)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if p := srv.pendingMessagesFor("alice"); len(p) != 0 {
		t.Errorf("still pending after delete: %+v", p)
	}
	if got := eventSeq(t, hub, "pending_deleted"); len(got) != 1 || got[0] != "pending_deleted:"+id {
		t.Errorf("events = %v, want pending_deleted:%s", got, id)
	}

	// Gone: a second delete, and a delete of an id nobody issued.
	for _, path := range []string{"/api/v1/agents/alice/pending/" + id, "/api/v1/agents/alice/pending/nope", "/api/v1/agents/ceo/pending/" + id} {
		rr = pendingDo(t, srv, http.MethodDelete, path)
		if rr.Code != http.StatusNotFound {
			t.Errorf("DELETE %s: %d, want 404", path, rr.Code)
		}
		errorBody(t, rr)
	}

	// The turn ends: nothing left to deliver.
	srv.onAgentpodFailed(turnState(srv), agentpod.TurnEvent{Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonCancelled})
	if rows := chatRows(t, srv); len(rows) != 0 {
		t.Errorf("deleted message reached chat.jsonl: %v", rows)
	}
}

// Once offered, only the agent can consume it: 409 with the exact body.
func TestPendingDeleteAfterOfferConflicts(t *testing.T) {
	srv, _, _, _ := pendingServer(t)
	id := stageCEO(t, srv, "too late")

	rr := pendingDo(t, srv, http.MethodDelete, "/api/v1/agents/alice/pending/"+id)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete offered: %d, want 409", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"error":"already with the agent","who":"use Send now, or wait for the turn"}` {
		t.Errorf("body = %s", got)
	}
	if p := srv.pendingMessagesFor("alice"); len(p) != 1 {
		t.Errorf("offered message was removed: %+v", p)
	}
}

// Restore within the window puts it back where it was and announces it
// again; after the window it is 410.
func TestPendingRestoreWindow(t *testing.T) {
	srv, hub, _, clk := pendingServer(t)
	holdFolds(srv)
	a := stageCEO(t, srv, "one")
	b := stageCEO(t, srv, "two")
	c := stageCEO(t, srv, "three")

	if rr := pendingDo(t, srv, http.MethodDelete, "/api/v1/agents/alice/pending/"+b); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	clk.Advance(pendingRestoreWindow - time.Millisecond)
	rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+b+"/restore")
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	var resp apitypes.PendingRestoreResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != b || resp.Text != "two" || !resp.Deletable || resp.DeliveredTS != nil {
		t.Errorf("restore = %+v, want %s back in the queue, deletable", resp, b)
	}
	apitypes.NoNilSlices(t, resp)
	var order []string
	for _, p := range srv.pendingMessagesFor("alice") {
		order = append(order, p.ID)
	}
	if strings.Join(order, ",") != strings.Join([]string{a, b, c}, ",") {
		t.Errorf("queue after restore = %v, want original order", order)
	}
	if got := eventSeq(t, hub, "pending_message", "pending_deleted"); strings.Join(got, ",") != strings.Join([]string{
		"pending_message:" + a, "pending_message:" + b, "pending_message:" + c,
		"pending_deleted:" + b, "pending_message:" + b,
	}, ",") {
		t.Errorf("events = %v", got)
	}

	// A restored message can be restored only once.
	if rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+b+"/restore"); rr.Code != http.StatusNotFound {
		t.Errorf("second restore: %d, want 404", rr.Code)
	}

	// The window is ten seconds, measured on the injected clock.
	if rr := pendingDo(t, srv, http.MethodDelete, "/api/v1/agents/alice/pending/"+c); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	clk.Advance(pendingRestoreWindow)
	rr = pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+c+"/restore")
	if rr.Code != http.StatusGone {
		t.Fatalf("late restore: %d, want 410", rr.Code)
	}
	errorBody(t, rr)
	if rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/nope/restore"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown restore: %d, want 404", rr.Code)
	}
}

// Restored after the turn ended: delivered like a fresh message.
func TestPendingRestoreAfterTurnEndDelivers(t *testing.T) {
	srv, _, _, _ := pendingServer(t)
	holdFolds(srv)
	id := stageCEO(t, srv, "back again")
	if rr := pendingDo(t, srv, http.MethodDelete, "/api/v1/agents/alice/pending/"+id); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	srv.onAgentpodFailed(turnState(srv), agentpod.TurnEvent{Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonCancelled})

	rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+id+"/restore")
	if rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	var resp apitypes.PendingRestoreResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if resp.DeliveredTS == nil || len(hist) != 1 || *resp.DeliveredTS != hist[0].TS.UnixMilli() {
		t.Errorf("restore = %+v, chat = %v; want delivered with the row's ts", resp, hist)
	}
}

func TestPendingSendNowRefusals(t *testing.T) {
	srv, _, sub, _ := pendingServer(t)
	id := stageCEO(t, srv, "urgent")
	drainAgentpodEvents(sub)

	if rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/nope/send-now"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown: %d, want 404", rr.Code)
	}

	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{RequestedBy: "ceo"}); err != nil {
		t.Fatalf("rotation marker: %v", err)
	}
	rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+id+"/send-now")
	if rr.Code != http.StatusConflict {
		t.Errorf("rotating: %d, want 409", rr.Code)
	}
	errorBody(t, rr)
	if err := srv.Store.ClearPendingRotation("alice"); err != nil {
		t.Fatalf("clear rotation: %v", err)
	}

	// No agent-pod turn to pause (in-process runtime, or the spawn
	// window before the turn is published).
	st := turnState(srv)
	srv.streamMu.Lock()
	delete(srv.agentpodTurns, "alice")
	srv.streamMu.Unlock()
	rr = pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+id+"/send-now")
	if rr.Code != http.StatusConflict {
		t.Errorf("no turn: %d, want 409", rr.Code)
	}
	errorBody(t, rr)
	srv.streamMu.Lock()
	srv.agentpodTurns["alice"] = st
	srv.streamMu.Unlock()

	if ids := cancelledTurnIDs(t, drainAgentpodEvents(sub)); len(ids) != 0 {
		t.Errorf("refused Send now published cancels %v", ids)
	}
}

// The latch path: Send now cancels the parent turn once; at turn end the
// partial reply is kept, then the marker, then every pending message in
// order, and the marker does not hold the agent.
func TestPendingSendNowPausesAndDelivers(t *testing.T) {
	srv, hub, sub, _ := pendingServer(t)
	srv.installAgentpodSubagentTurn("alice", "subagent-turn-9", "sub9", "claude-sonnet-5")
	a := stageCEO(t, srv, "first")
	b := stageCEO(t, srv, "second")
	drainAgentpodEvents(sub)
	st := turnState(srv)
	srv.onAgentpodDelta(st, agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "halfway through"})

	for i := 0; i < 2; i++ {
		rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+b+"/send-now")
		if rr.Code != http.StatusAccepted {
			t.Fatalf("send-now #%d: %d %s", i+1, rr.Code, rr.Body.String())
		}
	}
	if ids := cancelledTurnIDs(t, drainAgentpodEvents(sub)); len(ids) != 1 || ids[0] != "turn-1" {
		t.Fatalf("cancels = %v, want exactly [turn-1] (parent only, once)", ids)
	}

	// The pod reports the cancelled turn.
	srv.onAgentpodFailed(st, agentpod.TurnEvent{Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonCancelled})

	want := []string{"agent:halfway through", "marker:" + pausedToDeliverContent, "ceo:first", "ceo:second"}
	if got := chatRows(t, srv); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("chat.jsonl =\n %v\nwant\n %v", got, want)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	if v := store.SpawnDecision(hist); v != store.SpawnNow {
		t.Errorf("SpawnDecision after the pause = %v, want SpawnNow (the marker must not hold)", v)
	}

	got := eventSeq(t, hub, "chat_marker", "pending_delivered")
	wantEv := []string{"chat_marker:" + pausedToDeliverContent, "pending_delivered:" + a, "pending_delivered:" + b}
	if strings.Join(got, ",") != strings.Join(wantEv, ",") {
		t.Errorf("events = %v, want %v", got, wantEv)
	}
	if ev, _ := hubEventByKind(hub, "chat_marker"); ev["kind"] != apitypes.ChatMarkerPausedToDeliver {
		t.Errorf("chat_marker = %v, want kind %s", ev, apitypes.ChatMarkerPausedToDeliver)
	}
}

// Without a Send now, a cancelled turn behaves as Stop does: no
// marker, and the partial reply is not flushed (Stop's semantics).
func TestCancelledTurnWithoutSendNowWritesNoMarker(t *testing.T) {
	srv, _, _, _ := pendingServer(t)
	holdFolds(srv)
	stageCEO(t, srv, "queued")
	st := turnState(srv)
	srv.onAgentpodDelta(st, agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "partial"})
	srv.onAgentpodFailed(st, agentpod.TurnEvent{Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonCancelled})
	if got := chatRows(t, srv); len(got) != 1 || got[0] != "ceo:queued" {
		t.Errorf("chat.jsonl = %v, want [ceo:queued]", got)
	}
}

// Send now after every message already folded in: the turn still ends,
// but there is nothing to introduce, so no marker is written. The
// partial reply is still kept — the CEO paused, they did not reject.
func TestSendNowWithNothingLeftWritesNoMarker(t *testing.T) {
	srv, _, sub, _ := pendingServer(t)
	id := stageCEO(t, srv, "only")
	drainAgentpodEvents(sub)
	if rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+id+"/send-now"); rr.Code != http.StatusAccepted {
		t.Fatalf("send-now: %d", rr.Code)
	}
	st := turnState(srv)
	srv.onAgentpodFolded(st, agentpod.TurnEvent{Kind: agentpod.TurnEventFolded, DeliveryID: id, Landed: true})
	srv.onAgentpodDelta(st, agentpod.TurnEvent{Kind: agentpod.TurnEventDelta, Text: "answering it"})
	srv.onAgentpodFailed(st, agentpod.TurnEvent{Kind: agentpod.TurnEventFailed, FailedReason: agentpod.FailedReasonCancelled})
	if got := chatRows(t, srv); strings.Join(got, "|") != "ceo:only|agent:answering it" {
		t.Errorf("chat.jsonl = %v, want [ceo:only agent:answering it] and no marker", got)
	}
}

// The marker decision is made under the drain's own streamMu hold, so
// a Send now that found the message pending (and answered 202) cannot
// land between the latch check and the drain.
func TestFlushAndCompleteMarkedDecidesUnderStreamMu(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	holdFolds(srv)
	stageCEO(t, srv, "queued")
	called := false
	n := srv.flushAndCompleteMarked("alice", hub, func() *store.ChatMessage {
		called = true
		if srv.streamMu.TryLock() {
			srv.streamMu.Unlock()
			t.Error("marker hook ran without streamMu held")
		}
		m := pausedToDeliverMarker(srv.clk().Now())
		return &m
	})
	if !called || n != 1 {
		t.Fatalf("called=%v flushed=%d, want the hook called and one row flushed", called, n)
	}
	if got := chatRows(t, srv); strings.Join(got, "|") != "marker:"+pausedToDeliverContent+"|ceo:queued" {
		t.Errorf("chat.jsonl = %v, want the marker then the message", got)
	}

	// Nothing waiting: the hook still runs, but no marker is written.
	called = false
	if n := srv.flushAndCompleteMarked("alice", hub, func() *store.ChatMessage {
		called = true
		m := pausedToDeliverMarker(srv.clk().Now())
		return &m
	}); !called || n != 0 {
		t.Fatalf("empty buffer: called=%v flushed=%d", called, n)
	}
	if got := chatRows(t, srv); len(got) != 2 {
		t.Errorf("chat.jsonl = %v, want no second marker", got)
	}
}

// Queued events go out in queue order, only on flush, and a nil hub is
// a no-op (a delivery staged while only the runtime engine is busy has
// no hub to announce on).
func TestChatHubOrderedQueue(t *testing.T) {
	hub := newTestHub()
	hub.queueOrdered("pending_message", map[string]string{"id": "a"})
	hub.queueOrdered("pending_offered", map[string]string{"id": "a"})
	if n := len(drainReplay(hub)); n != 0 {
		t.Fatalf("%d events broadcast before flush", n)
	}
	hub.flushOrdered()
	hub.Emit("chat_message", map[string]string{"content": "x"})
	if got := strings.Join(eventSeq(t, hub, "pending_message", "pending_offered", "chat_message"), ","); got != "pending_message:a,pending_offered:a,chat_message:x" {
		t.Errorf("events = %s", got)
	}
	var none *chatHub
	none.queueOrdered("pending_message", nil)
	none.flushOrdered()
}

// Offboarding retires the pending bubbles before the hub closes, and
// takes the tombstones with it: nothing of the agent's stays behind.
func TestOffboardRetiresPendingMessages(t *testing.T) {
	srv, hub, _, _ := pendingServer(t)
	holdFolds(srv)
	gone := stageCEO(t, srv, "deleted first")
	id := stageCEO(t, srv, "for someone leaving")
	if rr := pendingDo(t, srv, http.MethodDelete, "/api/v1/agents/alice/pending/"+gone); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	srv.streamMu.Lock()
	_, tombstoned := srv.pendingTombstones["alice"][gone]
	srv.streamMu.Unlock()
	if !tombstoned {
		t.Fatal("delete left no tombstone on the server")
	}

	srv.drainSlugState("alice")
	if got := eventSeq(t, hub, "pending_deleted"); strings.Join(got, ",") != "pending_deleted:"+gone+",pending_deleted:"+id {
		t.Errorf("events = %v, want pending_deleted for %s then %s", got, gone, id)
	}
	if p := srv.pendingMessagesFor("alice"); len(p) != 0 {
		t.Errorf("pending after offboard: %+v", p)
	}
	srv.streamMu.Lock()
	_, kept := srv.pendingTombstones["alice"]
	srv.streamMu.Unlock()
	if kept {
		t.Error("offboard left alice's tombstones on the server")
	}
	if rr := pendingDo(t, srv, http.MethodPost, "/api/v1/agents/alice/pending/"+gone+"/restore"); rr.Code != http.StatusNotFound {
		t.Errorf("restore after offboard: %d, want 404", rr.Code)
	}
}
