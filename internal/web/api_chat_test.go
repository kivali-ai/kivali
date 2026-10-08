package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// chatDo serves one request through the chat routes. A body is sent as
// JSON.
func chatDo(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPIChatRoutes(mux)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestAPIChat(t *testing.T) {
	srv := transcriptServer(t)
	srv.AgentModel = provider.MockModelLarge
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "hi", TS: tsAt(1)},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t1", ToolName: "Bash", ToolInput: "{}", TS: tsAt(2)},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "t1", Content: "ok", TS: tsAt(3)},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "hello", Model: provider.MockModelLarge, Effort: "high", TS: tsAt(4)},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatal(err)
		}
	}
	rr := chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chat", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rr.Code, rr.Body.String())
	}
	c := decodeAPI[apitypes.Chat](t, rr)
	if len(c.Rows) != 3 || c.Rows[1].Kind != apitypes.TranscriptKindToolUse || *c.Rows[1].Output != "ok" {
		t.Errorf("rows = %+v", c.Rows)
	}
	if len(c.Pending) != 0 || c.Running || c.WaitingTasks != 0 || c.Archived {
		t.Errorf("pending/running/waiting/archived = %v/%v/%v/%v", c.Pending, c.Running, c.WaitingTasks, c.Archived)
	}
	if c.CurrentModel != provider.MockModelLarge || c.CurrentEffort != "high" || len(c.Models) != 2 {
		t.Errorf("settings = %q %q %+v", c.CurrentModel, c.CurrentEffort, c.Models)
	}
	if c.Fill.Limit == 0 || c.Fill.Tokens == 0 || c.Fill.Bucket != fillBucket(c.Fill.Pct) || c.Fill.ResolvedModel != provider.MockModelLarge || c.Fill.LongThreshold != c.Fill.Limit*3/4 {
		t.Errorf("fill = %+v", c.Fill)
	}
	// An agent that has left is served, marked archived.
	if err := srv.Store.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	if c := decodeAPI[apitypes.Chat](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/bob/chat", "")); !c.Archived || len(c.Rows) != 0 {
		t.Errorf("archived chat = %+v", c)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/nobody/chat", ""), http.StatusNotFound)
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/ceo/chat", ""), http.StatusBadRequest)
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/_archived/chat", ""), http.StatusNotFound)
}

// Mid-turn: the chat says running and lists what waits.
func TestAPIChatWhileRunning(t *testing.T) {
	srv, _, _, _ := pendingServer(t)
	stageCEO(t, srv, "while you work")
	c := decodeAPI[apitypes.Chat](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chat", ""))
	if !c.Running || len(c.Pending) != 1 || c.Pending[0].Text != "while you work" {
		t.Errorf("running/pending = %v/%+v", c.Running, c.Pending)
	}
}

func TestAPIMessagePost(t *testing.T) {
	srv, _, _, _ := pendingServer(t)
	// No model client: the page handler's plain-text 503 comes back in
	// the API's shape.
	req := func(slug, body string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		srv.wireAPIChatRoutes(mux)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+slug+"/messages", strings.NewReader(body))
		r.Header.Set("content-type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, r)
		return rr
	}
	if e := assertAPIError(t, req("alice", "text=hi"), http.StatusServiceUnavailable); e.Who != whoServer {
		t.Errorf("who = %q", e.Who)
	}
	srv.Claude = &provider.MockClient{}
	rr := req("alice", "text=hi")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rr.Code, rr.Body.String())
	}
	// Not decodeAPI: attachments is absent, by the type's contract, when
	// the message carried none.
	var resp apitypes.MessagePostResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || rr.Header().Get("content-type") != "application/json" {
		t.Fatalf("body %q (%v), content-type %q", rr.Body.String(), err, rr.Header().Get("content-type"))
	}
	if resp.PendingID == "" || resp.TS == 0 {
		t.Errorf("response = %+v, want a pending id (alice is mid-turn)", resp)
	}
	assertAPIError(t, req("alice", "text="), http.StatusBadRequest)
	if e := assertAPIError(t, req("nobody", "text=hi"), http.StatusNotFound); !strings.Contains(e.Error, "nobody") {
		t.Errorf("404 error = %q", e.Error)
	}
	assertAPIError(t, req("ceo", "text=hi"), http.StatusBadRequest)
}

func TestAPIStop(t *testing.T) {
	srv := transcriptServer(t)
	rr := chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/stop", "")
	if got := decodeAPI[apitypes.StopResponse](t, rr); rr.Code != http.StatusOK || got.Stopped {
		t.Errorf("idle stop = %d %+v, want 200 stopped false", rr.Code, got)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/nobody/stop", ""), http.StatusNotFound)
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/ceo/stop", ""), http.StatusBadRequest)

	running, _, _, _ := pendingServer(t)
	rr = chatDo(t, running, http.MethodPost, "/api/v1/agents/alice/stop", "")
	if got := decodeAPI[apitypes.StopResponse](t, rr); !got.Stopped {
		t.Errorf("running stop = %+v, want stopped", got)
	}
	hist, _ := running.Store.ReadChatHistory("alice")
	if len(hist) != 1 || hist[0].Kind != store.KindUserInterruption {
		t.Errorf("chat after stop = %+v, want the interruption marker", hist)
	}
}

func TestAPIModelAndEffort(t *testing.T) {
	srv := transcriptServer(t)
	srv.AgentModel = provider.MockModelLarge
	model := provider.MockModelSmall
	rr := chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/model", `{"model":"`+model+`"}`)
	if got := decodeAPI[apitypes.ChatSettings](t, rr); rr.Code != http.StatusOK || got.CurrentModel != model {
		t.Errorf("model = %d %+v, want %s", rr.Code, got, model)
	}
	if a, _ := srv.Store.GetAgent("alice"); a.Model != model {
		t.Errorf("stored model = %q", a.Model)
	}
	rr = chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/model", `{"model":"default"}`)
	if got := decodeAPI[apitypes.ChatSettings](t, rr); got.CurrentModel != provider.MockModelLarge {
		t.Errorf("default model = %+v, want the fleet default", got)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/model", `{"model":"gpt-9"}`), http.StatusBadRequest)

	rr = chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/effort", `{"effort":"low"}`)
	if got := decodeAPI[apitypes.ChatSettings](t, rr); got.CurrentEffort != "low" {
		t.Errorf("effort = %+v", got)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/effort", `{"effort":"ludicrous"}`), http.StatusBadRequest)
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/effort", `{"level":"low"}`), http.StatusBadRequest)
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/nobody/effort", `{"effort":"low"}`), http.StatusNotFound)

	// A form body is refused: the API takes JSON only.
	mux := http.NewServeMux()
	srv.wireAPIChatRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/alice/effort", strings.NewReader("effort=low"))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	form := httptest.NewRecorder()
	mux.ServeHTTP(form, req)
	assertAPIError(t, form, http.StatusUnsupportedMediaType)

	// An agent that has left cannot be changed.
	if err := srv.Store.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/bob/effort", `{"effort":"low"}`), http.StatusConflict)
}

func TestAPINewChat(t *testing.T) {
	srv := transcriptServer(t)
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/alice/new-chat", ""), http.StatusServiceUnavailable)
	srv.Claude = &provider.MockClient{}
	rr := chatDo(t, srv, http.MethodPost, "/api/v1/agents/bob/new-chat", "")
	if got := decodeAPI[apitypes.NewChatResponse](t, rr); rr.Code != http.StatusOK || got.Started {
		t.Errorf("empty chat = %d %+v, want started false", rr.Code, got)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/ceo/new-chat", ""), http.StatusBadRequest)
	assertAPIError(t, chatDo(t, srv, http.MethodPost, "/api/v1/agents/nobody/new-chat", ""), http.StatusNotFound)

	busy, _, _, _ := pendingServer(t)
	busy.Claude = &provider.MockClient{}
	if err := busy.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, chatDo(t, busy, http.MethodPost, "/api/v1/agents/alice/new-chat", ""), http.StatusConflict)
	if _, ok, _ := busy.Store.ReadPendingRotation("alice"); ok {
		t.Error("a refused new chat left a rotation marker")
	}
}

func TestAPISubagent(t *testing.T) {
	srv := transcriptServer(t)
	writeSubagentMetaFile(t, srv, "aaaa1111", subagentMeta{ID: "aaaa1111", Description: "survey", Model: provider.MockModelRetired,
		Effort: "low", Status: "completed", UpdatedAt: "2026-09-28T12:05:00Z"})
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "go", TS: tsAt(10)},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "done", TS: tsAt(20)},
	} {
		if err := srv.Store.AppendSubagentMessage("alice", "aaaa1111", m); err != nil {
			t.Fatal(err)
		}
	}
	rr := chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/subagents/aaaa1111", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeAPI[apitypes.SubagentTranscript](t, rr)
	m := got.Meta
	if m.ID != "aaaa1111" || m.Parent != "alice" || m.State != apitypes.SubagentTaskStateDone || m.Description != "survey" ||
		m.Model != "Mock Large 0" || m.Effort != "low" || m.Started != msAt(10) || m.Error != nil {
		t.Errorf("meta = %+v", m)
	}
	if m.Ended == nil || *m.Ended != msAt(300) {
		t.Errorf("ended = %v, want meta.json's updated_at", m.Ended)
	}
	if len(got.Rows) != 2 || got.Rows[1].From.Slug != "aaaa1111" || got.Rows[0].From.Slug != "alice" {
		t.Errorf("rows = %+v", got.Rows)
	}
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/subagents/ffff0000", ""), http.StatusNotFound)
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/nobody/subagents/aaaa1111", ""), http.StatusNotFound)
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/subagents/.hidden", ""), http.StatusNotFound)
	// An encoded slash stays inside the {id} segment and reaches the
	// handler as "a/b"; a literal ".." never gets this far (the mux
	// redirects it).
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/subagents/a%2Fb", ""), http.StatusNotFound)

	// The task stays readable after alice leaves: her files, tasks
	// included, moved under agents/_archived.
	if err := srv.Store.ArchiveAgent("alice"); err != nil {
		t.Fatal(err)
	}
	rr = chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/subagents/aaaa1111", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("archived parent: code = %d: %s", rr.Code, rr.Body.String())
	}
	if got := decodeAPI[apitypes.SubagentTranscript](t, rr); got.Meta.Parent != "alice" || got.Meta.State != apitypes.SubagentTaskStateDone || len(got.Rows) != 2 {
		t.Errorf("archived parent: %+v", got.Meta)
	}
}

func TestAPIPastChats(t *testing.T) {
	srv := transcriptServer(t)
	// Two rotations: gen1 then gen2, each with the memory snapshot taken
	// when it was asked for; the live files are what gen2 left.
	gens := []string{"20260901T090000.000000000Z", "20260915T090000.000000000Z"}
	chats := [][]store.ChatMessage{
		{{Role: store.RoleReceived, Kind: "direct_chat", Content: "first", TS: tsAt(1)}, {Role: store.RoleSent, Kind: "direct_chat", Content: "ok", TS: tsAt(2)}},
		{{Role: store.RoleReceived, Kind: "direct_chat", Content: "second", TS: tsAt(3)},
			{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t", ToolName: "Bash", TS: tsAt(4)},
			{Role: store.RoleSent, Kind: "direct_chat", Content: "fine", TS: tsAt(5)}},
	}
	before := []string{"# Memory\n- old fact\n", "# Memory\n- old fact\n- learned in gen1\n"}
	for i, g := range gens {
		for _, m := range chats[i] {
			if err := srv.Store.AppendChatMessage("alice", m); err != nil {
				t.Fatal(err)
			}
		}
		if err := srv.Store.ArchiveChatAs("alice", g); err != nil {
			t.Fatal(err)
		}
		if err := srv.Store.WriteArchivedAgentMemory("alice", g, before[i]); err != nil {
			t.Fatal(err)
		}
		if err := srv.Store.WriteArchivedAgentHabits("alice", g, "habit v"+string(rune('1'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.Store.WriteEpisode("alice", gens[0], "---\nts: x\ntitle: The first chat\ntouched: -\n---\nWe met.\n"); err != nil {
		t.Fatal(err)
	}

	list := decodeAPI[apitypes.PastChats](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chats", ""))
	if len(list.Chats) != 2 || list.Chats[0].TS != gens[1] || list.Chats[1].TS != gens[0] {
		t.Fatalf("chats = %+v, want newest first", list.Chats)
	}
	newest, oldest := list.Chats[0], list.Chats[1]
	if oldest.Title != "The first chat" || newest.Title != "second" {
		t.Errorf("titles = %q, %q: the episode title, else the opening line", oldest.Title, newest.Title)
	}
	if newest.Messages != 2 || newest.Range != (apitypes.ChatRange{From: msAt(3), To: msAt(5)}) || newest.Tokens == 0 {
		t.Errorf("newest = %+v", newest)
	}

	d := decodeAPI[apitypes.PastChatDetail](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chats/"+gens[0], ""))
	s := d.Summary
	if s.TS != gens[0] || s.DigestMD != "We met." || s.Title != "The first chat" {
		t.Errorf("summary = %+v", s)
	}
	if len(s.MemoryAdded) != 1 || s.MemoryAdded[0] != "learned in gen1" {
		t.Errorf("memory_added = %v, want the line gen2's snapshot has and gen1's lacks", s.MemoryAdded)
	}
	if s.HabitsDiff != (apitypes.HabitsDiff{Before: "habit v1", After: "habit v2"}) {
		t.Errorf("habits_diff = %+v", s.HabitsDiff)
	}
	if len(d.Rows) != 2 || d.PrevTS != nil || d.NextTS == nil || *d.NextTS != gens[1] {
		t.Errorf("rows/prev/next = %d/%v/%v", len(d.Rows), d.PrevTS, d.NextTS)
	}

	// The newest generation compares against the live files.
	if err := srv.Store.WriteAgentMemory("alice", "# Memory\n- learned in gen1\n- learned in gen2\n"); err != nil {
		t.Fatal(err)
	}
	d = decodeAPI[apitypes.PastChatDetail](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chats/"+gens[1], ""))
	if len(d.Summary.MemoryAdded) != 1 || d.Summary.MemoryAdded[0] != "learned in gen2" || d.Summary.DigestMD != "" {
		t.Errorf("newest summary = %+v", d.Summary)
	}
	if d.PrevTS == nil || *d.PrevTS != gens[0] || d.NextTS != nil {
		t.Errorf("prev/next = %v/%v", d.PrevTS, d.NextTS)
	}

	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/alice/chats/20200101T000000.000000000Z", ""), http.StatusNotFound)
	assertAPIError(t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/nobody/chats", ""), http.StatusNotFound)
	if empty := decodeAPI[apitypes.PastChats](t, chatDo(t, srv, http.MethodGet, "/api/v1/agents/bob/chats", "")); len(empty.Chats) != 0 {
		t.Errorf("bob's chats = %+v", empty.Chats)
	}
}

func TestEpisodeDigestAndAddedLines(t *testing.T) {
	if got := episodeDigest("---\nts: a\ntitle: t\n---\nbody\n"); got != "body" {
		t.Errorf("digest = %q", got)
	}
	if got := episodeDigest("no frontmatter\n"); got != "no frontmatter" {
		t.Errorf("digest = %q", got)
	}
	got := addedLines("# A\n- one\n", "# A\n## B\n- one\n* two\n\n- two\nthree")
	if strings.Join(got, "|") != "two|three" {
		t.Errorf("added = %v", got)
	}
	if got := addedLines("x", "x"); got == nil || len(got) != 0 {
		t.Errorf("added = %#v, want empty", got)
	}
}
