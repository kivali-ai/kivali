package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// The subagent stream starts where the page left off. These tests pin
// the three pieces of that contract: the page puts its row count on
// the pill, the stream skips that many rows (or the rows a reconnect
// reports via Last-Event-ID), and every chat_message carries the id
// the reconnect will report. Without it, a stream that replays
// chat.jsonl from byte 0 slips plain chat rows past the client's
// dedupe, and the prompt is painted a second time at the bottom of a
// long transcript.

// writeSubagentTranscript lays down agents/<parent>/subagents/<id>/
// with the given rows and meta body, creating the parent agent so the
// transcript page can render it.
func writeSubagentTranscript(t *testing.T, srv *Server, parent, id string, rows []store.ChatMessage, meta string) {
	t.Helper()
	if _, err := srv.Store.GetAgent(parent); err != nil {
		if err := srv.Store.CreateAgent(store.Agent{Slug: parent, Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
			t.Fatalf("create agent: %v", err)
		}
	}
	dir := filepath.Join(srv.Store.Root(), "agents", parent, "subagents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "chat.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, m := range rows {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

func threeRowTranscript() []store.ChatMessage {
	ts := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	return []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "row-one prompt", TS: ts},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu1", ToolName: "file_view", ToolInput: `{"path":"row-two"}`, TS: ts.Add(time.Second)},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu1", Content: "row-three output", TS: ts.Add(2 * time.Second)},
	}
}

func getSubagentStream(t *testing.T, srv *Server, path string, lastEventID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d", path, rr.Code)
	}
	return rr.Body.String()
}

// TestJSONLTailerSkipsRowsTheClientHas: skip is spent on the first
// rows read and never again, so rows appended later flow through.
func TestJSONLTailerSkipsRowsTheClientHas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.jsonl")
	if err := os.WriteFile(path, []byte(`{"a":1}`+"\n"+`{"b":2}`+"\n"+`{"c":3}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := &jsonlTailer{path: path, skip: 2}
	got, err := tail.readNew()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0]) != `{"c":3}` {
		t.Fatalf("skip 2 of 3: want the third row only, got %q", got)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"d":4}` + "\n")
	_ = f.Close()
	got, err = tail.readNew()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0]) != `{"d":4}` {
		t.Fatalf("after skip is spent: want the appended row, got %q", got)
	}
}

// TestSubagentStreamStartsFromRenderedRows: ?from=2 on a three-row
// transcript sends the third row only, and stamps it with id 3 — its
// position in the file, which is what a reconnect will report.
func TestSubagentStreamStartsFromRenderedRows(t *testing.T) {
	srv := newTestServer(t)
	writeSubagentTranscript(t, srv, "alice", "abc123ef", threeRowTranscript(), `{"id":"abc123ef","status":"completed"}`)

	body := getSubagentStream(t, srv, "/agents/alice/subagents/abc123ef/stream?from=2", "")
	for _, absent := range []string{"row-one", "row-two"} {
		if strings.Contains(body, absent) {
			t.Errorf("stream replayed a row the page already rendered (%q)\n--- body ---\n%s", absent, body)
		}
	}
	for _, want := range []string{
		"id: 3\nevent: chat_message\n",
		"row-three",
		"event: meta",
		"event: done",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Count(body, "event: chat_message") != 1 {
		t.Errorf("want exactly one chat_message, got %d\n--- body ---\n%s", strings.Count(body, "event: chat_message"), body)
	}
}

// TestSubagentStreamReconnectResumesFromLastEventID: a reconnecting
// EventSource re-requests the same ?from URL but adds the id of the
// last row it received, and that later position wins. A header that
// does not parse falls back to ?from.
func TestSubagentStreamReconnectResumesFromLastEventID(t *testing.T) {
	srv := newTestServer(t)
	writeSubagentTranscript(t, srv, "alice", "abc123ef", threeRowTranscript(), `{"id":"abc123ef","status":"completed"}`)

	body := getSubagentStream(t, srv, "/agents/alice/subagents/abc123ef/stream?from=1", "3")
	if strings.Contains(body, "event: chat_message") {
		t.Errorf("reconnect at id 3 of 3 replayed rows\n--- body ---\n%s", body)
	}
	if !strings.Contains(body, "event: done") {
		t.Errorf("reconnect missing done\n--- body ---\n%s", body)
	}

	body = getSubagentStream(t, srv, "/agents/alice/subagents/abc123ef/stream?from=2", "not-a-number")
	if strings.Contains(body, "row-two") || !strings.Contains(body, "row-three") {
		t.Errorf("garbage Last-Event-ID should fall back to ?from=2\n--- body ---\n%s", body)
	}
}

// TestSubagentStreamWithoutPositionReplaysEverything: a client that
// says nothing about what it has gets the whole file, as before.
func TestSubagentStreamWithoutPositionReplaysEverything(t *testing.T) {
	srv := newTestServer(t)
	writeSubagentTranscript(t, srv, "alice", "abc123ef", threeRowTranscript(), `{"id":"abc123ef","status":"completed"}`)

	body := getSubagentStream(t, srv, "/agents/alice/subagents/abc123ef/stream", "")
	if n := strings.Count(body, "event: chat_message"); n != 3 {
		t.Errorf("want 3 chat_message events, got %d\n--- body ---\n%s", n, body)
	}
	for i, want := range []string{"id: 1\n", "id: 2\n", "id: 3\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("row %d missing its id %q\n--- body ---\n%s", i+1, want, body)
		}
	}
}
