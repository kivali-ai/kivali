package web

import (
	"context"
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

// TestJSONLTailerYieldsCompleteLines verifies the tailer:
// - returns nothing on a missing file
// - yields complete lines on subsequent reads
// - holds back partial trailing lines until newline arrives
// - drops malformed JSON silently
func TestJSONLTailerYieldsCompleteLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.jsonl")
	tail := &jsonlTailer{path: path}

	// Missing file → no error, no lines.
	got, err := tail.readNew()
	if err != nil {
		t.Fatalf("readNew on missing: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("missing file: want 0 lines, got %d", len(got))
	}

	// Two complete lines.
	if err := os.WriteFile(path, []byte(`{"a":1}`+"\n"+`{"b":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = tail.readNew()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 lines, got %d (%q)", len(got), got)
	}

	// Append a partial line (no newline) → not yielded yet.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"c":3`)
	_ = f.Close()
	got, err = tail.readNew()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("partial line: want 0 yielded, got %d", len(got))
	}

	// Complete the line + add a malformed one → only the complete valid
	// JSON line is yielded.
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("}\n" + "this is not json\n")
	_ = f.Close()
	got, err = tail.readNew()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after completion + garbage: want 1 valid line, got %d (%q)", len(got), got)
	}
	if string(got[0]) != `{"c":3}` {
		t.Errorf("unexpected line: %s", got[0])
	}
}

func TestMetaIsTerminal(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"status":"running"}`, false},
		{`{"status":"completed"}`, true},
		{`{"status":"errored"}`, true},
		{`{"status":""}`, false},
		{`{}`, false},
		{`not json`, false},
	}
	for _, tc := range cases {
		if got := metaIsTerminal([]byte(tc.body)); got != tc.want {
			t.Errorf("metaIsTerminal(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// TestSubagentStreamReplaysAndCloses opens the SSE handler against a
// pre-populated transcript with a terminal meta.json, asserts the
// initial replay emits all chat_message events plus a final done,
// and that the response closes promptly. Exercises the "page opened
// after the subagent already finished" path.
func TestSubagentStreamReplaysAndCloses(t *testing.T) {
	srv := newTestServer(t)
	parent := "alice"
	if err := srv.Store.CreateAgent(store.Agent{Slug: parent, Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	id := "abc123ef"
	dir := filepath.Join(srv.Store.Root(), "agents", parent, "subagents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two chat messages already on disk.
	transcript := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "do the thing", TS: time.Now().UTC()},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "done", TS: time.Now().UTC()},
	}
	chatJSONL := filepath.Join(dir, "chat.jsonl")
	f, err := os.Create(chatJSONL)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, m := range transcript {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"id":"abc123ef","status":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a real http.ResponseRecorder via httptest, but the SSE
	// handler streams to it: since meta is already terminal, the
	// handler does the initial replay and returns immediately
	// without entering the poll loop.
	req := httptest.NewRequest(http.MethodGet, "/agents/"+parent+"/subagents/"+id+"/stream", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"event: chat_message",
		`"do the thing"`,
		`"done"`,
		"event: meta",
		"event: done",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q\n--- body ---\n%s", want, body)
		}
	}
}

// TestSubagentStreamTailsAppends starts a stream against a "running"
// subagent and verifies that lines appended after subscription show
// up in the SSE response. This is the live-tail path.
func TestSubagentStreamTailsAppends(t *testing.T) {
	srv := newTestServer(t)
	parent := "alice"
	if err := srv.Store.CreateAgent(store.Agent{Slug: parent, Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	id := "live5678"
	dir := filepath.Join(srv.Store.Root(), "agents", parent, "subagents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Initial state: one entry, status running.
	chatJSONL := filepath.Join(dir, "chat.jsonl")
	enc := func(path string, msg store.ChatMessage, mode int) {
		t.Helper()
		f, err := os.OpenFile(path, mode, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(msg)
		if _, err := f.Write(append(body, '\n')); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	enc(chatJSONL, store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "begin", TS: time.Now().UTC()}, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"status":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use an httptest.Server so we get a real flushing transport, not
	// the buffered ResponseRecorder which only delivers on handler
	// return.
	httpsrv := httptest.NewServer(srv.Handler())
	defer httpsrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpsrv.URL+"/agents/"+parent+"/subagents/"+id+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(authCookie(t, srv))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// In a goroutine, after a brief delay, append two more lines and
	// flip meta to completed.
	go func() {
		time.Sleep(300 * time.Millisecond)
		enc(chatJSONL, store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u1", ToolName: "file_view", ToolInput: `{"path":"x"}`, TS: time.Now().UTC()}, os.O_APPEND|os.O_WRONLY)
		enc(chatJSONL, store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: "all done", TS: time.Now().UTC()}, os.O_APPEND|os.O_WRONLY)
		_ = os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"status":"completed"}`), 0o644)
	}()

	// Read until done event or timeout. Use a small buffer + scan
	// since SSE is line-delimited.
	buf := make([]byte, 8192)
	var collected strings.Builder
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
			if strings.Contains(collected.String(), "event: done") {
				break
			}
		}
		if err != nil {
			break
		}
	}
	body := collected.String()
	for _, want := range []string{
		`"begin"`,     // initial replay
		`"file_view"`, // appended tool_use
		`"all done"`,  // appended direct_chat
		"event: done",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q\n--- body ---\n%s", want, body)
		}
	}
}
