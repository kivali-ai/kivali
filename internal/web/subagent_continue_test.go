package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestContinueResumesTheTaskAtOnce: the person's word from the window
// resumes a waiting task without the window grace (the clock does not
// move), and the task runs again to its result. A second word changes
// nothing, another agent's word finds no task, and once the task has
// ended it is not waiting.
func TestContinueResumesTheTaskAtOnce(t *testing.T) {
	run := startWaitingTask(t, submitQuote(t), nil)
	if got := run.svc.continueTask("bob", run.id); got != signInNoTask {
		t.Errorf("another agent's continue = %v, want no task", got)
	}
	if got := run.svc.continueTask("alice", run.id); got != signInContinued {
		t.Fatalf("continue = %v, want continued", got)
	}
	if got := run.svc.continueTask("alice", run.id); got != signInContinued {
		t.Errorf("a second continue = %v, want continued", got)
	}
	if body := run.del.waitFor(1)[0].Content; !strings.Contains(body, `"Acme"`) {
		t.Errorf("delivered = %s", body)
	}
	if run.runs() != 2 {
		t.Errorf("runs = %d, want 2", run.runs())
	}
	if got := run.svc.continueTask("alice", run.id); got != signInNotWaiting {
		t.Errorf("continue on an ended task = %v, want not waiting", got)
	}
}

// TestContinueThenWindowClosedResumesOnce: a window closing after the
// person's word is no second resume.
func TestContinueThenWindowClosedResumesOnce(t *testing.T) {
	run := startWaitingTask(t, submitQuote(t), nil)
	run.svc.continueTask("alice", run.id)
	run.closeWindows()
	run.del.waitFor(1)
	if run.runs() != 2 {
		t.Errorf("runs = %d, want 2", run.runs())
	}
}

// TestAPISubagentContinue: POST .../continue resumes a task waiting for
// a person, 204, and again 204 once it has; a task not waiting is 409;
// a task core does not hold, another agent's, or a path-like id is 404.
func TestAPISubagentContinue(t *testing.T) {
	srv := newTestServer(t)
	for _, slug := range []string{"buyer", "seller"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: "Buyer", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
	srv.SubagentService = &SubagentService{Store: srv.Store, Provider: provider.MockProvider{}}
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	resumed := make(chan struct{})
	srv.SubagentService.registerJob(&subagentJob{
		ID: "w1", Parent: "buyer", Kind: agentpod.SubagentKindWeb, Session: "vendor.example", Site: "vendor.example",
		State: subagentJobWaiting, WaitReason: "sign_in", WaitSince: at, QueuedAt: at, Depth: 1,
		resumed: resumed, done: make(chan struct{}),
	})
	srv.SubagentService.registerJob(&subagentJob{ID: "r1", Parent: "buyer", Kind: agentpod.SubagentKindWeb, State: subagentJobRunning, QueuedAt: at, Depth: 1, done: make(chan struct{})})
	h := authedHandler(t, srv)
	post := func(path string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	if code := post("/api/v1/agents/buyer/subagents/w1/continue"); code != http.StatusNoContent {
		t.Fatalf("continue a waiting task: %d, want 204", code)
	}
	select {
	case <-resumed:
	default:
		t.Fatal("the waiting task was not resumed")
	}
	if code := post("/api/v1/agents/buyer/subagents/w1/continue"); code != http.StatusNoContent {
		t.Errorf("continue again: %d, want 204", code)
	}
	if code := post("/api/v1/agents/buyer/subagents/r1/continue"); code != http.StatusConflict {
		t.Errorf("continue a running task: %d, want 409", code)
	}
	for _, path := range []string{
		"/api/v1/agents/buyer/subagents/nope1234/continue",
		"/api/v1/agents/seller/subagents/w1/continue",
		"/api/v1/agents/buyer/subagents/..%2Fw1/continue",
		"/api/v1/agents/nobody/subagents/w1/continue",
	} {
		if code := post(path); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/buyer/subagents/w1/continue", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a cross-site continue: %d, want 403", rr.Code)
	}
}
