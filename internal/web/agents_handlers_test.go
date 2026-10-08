package web

// Per-handler unit tests for the agent-mutation routes. Each handler gets at least: happy path
// (200/303 + state changed as expected), validation failure (4xx
// without state change), and missing-agent (404). The e2e suite in
// messaging_e2e_test.go covers the larger flows; these tests pin the
// per-handler contract so a refactor of, say, agent-stop's lock
// discipline can't slip through unnoticed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// postFormString posts a urlencoded body string. Sibling
// `postFormString(t, srv, path, map)` already exists in bootstrap_test.go;
// this wrapper takes the body verbatim so per-handler tests can
// supply minimal urlencoded payloads inline.
func postFormString(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

/* ---------- PUT /api/v1/agents/{slug}/docs/memory ---------------- */

// putAgentMemory saves an agent's memory through the docs API.
func putAgentMemory(t *testing.T, srv *Server, slug, content string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	return apiDo(t, srv, http.MethodPut, "/api/v1/agents/"+slug+"/docs/memory", string(body), nil)
}

func TestAgentSetMemoryWrites(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := putAgentMemory(t, srv, "alice", "remember the APAC constraint")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := srv.Store.ReadAgentMemory("alice")
	if err != nil {
		t.Fatalf("read memory: %v", err)
	}
	if !strings.Contains(got, "remember the APAC constraint") {
		t.Errorf("memory missing saved body: %q", got)
	}
}

func TestAgentSetMemoryAllowsEmptyBodyToClear(t *testing.T) {
	srv := newTestServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k")
	if err := srv.Store.WriteAgentMemory("alice", "old content"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	if rr := putAgentMemory(t, srv, "alice", ""); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if got, _ := srv.Store.ReadAgentMemory("alice"); got != "" {
		t.Errorf("memory should be cleared, got %q", got)
	}
}

func TestAgentSetMemoryRejectsCEOSlug(t *testing.T) {
	srv := newTestServer(t)
	if rr := putAgentMemory(t, srv, agent.CEOSlug, "x"); rr.Code != http.StatusBadRequest {
		t.Errorf("CEO slug should be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAgentSetMemoryReturns404ForUnknownAgent(t *testing.T) {
	srv := newTestServer(t)
	if rr := putAgentMemory(t, srv, "ghost", "x"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown agent should 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

/* ---------- model and effort: agent_setmodel_test.go ------------- */

/* ---------- offboarding ------------------------------------------ */
//
// The archive path now hangs off a CEO approval; ceo_test.go covers it
// end to end. hire_test.go asserts the retired Fire surface is gone.

/* ---------- handleAgentStop -------------------------------------- */

func TestAgentStopReturns204WhenNoLoopRunning(t *testing.T) {
	srv := newTestServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "k")
	rr := postFormString(t, srv, "/agents/alice/stop", "")
	if rr.Code != http.StatusNoContent {
		t.Errorf("idle agent stop = %d, want 204", rr.Code)
	}
}

// Stop's cancellation flows over UDS via publishCancelTurn →
// EventCancelTurn; TestStopPublishesCancelTurnEvent in
// interrupt_test.go covers it.

func TestAgentStopRejectsCEOSlug(t *testing.T) {
	srv := newTestServer(t)
	rr := postFormString(t, srv, "/agents/"+agent.CEOSlug+"/stop", "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("stop on CEO slug = %d, want 400", rr.Code)
	}
}
