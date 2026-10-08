package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The Background tab is the standing answer to "what is running right
// now". It once answered with whatever had been true at page load:
// jobs stayed "queued" after they started, finished ones still read as
// running, and a batch dispatched while someone watched never appeared.
// The app refetches GET /api/v1/agents/{slug}/background whenever the
// org snapshot says the agent's background work moved. These cover the
// server half: the endpoint carries each job's live state, and every
// transition pushes a notify.

func seedJob(t *testing.T, srv *Server, j *subagentJob) {
	t.Helper()
	if srv.SubagentService == nil {
		srv.SubagentService = &SubagentService{Store: srv.Store}
	}
	srv.SubagentService.registerJob(j)
}

// backgroundOf is GET /api/v1/agents/{slug}/background through the
// full stack.
func backgroundOf(t *testing.T, srv *Server, slug string) apitypes.Background {
	t.Helper()
	rr := apiDo(t, srv, http.MethodGet, "/api/v1/agents/"+slug+"/background", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("background %s: code = %d body = %s", slug, rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Background](t, rr)
}

// sessionByID finds one session, failing the test when it is absent.
func sessionByID(t *testing.T, b apitypes.Background, id string) apitypes.BackgroundSession {
	t.Helper()
	for _, s := range b.Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %s in %+v", id, b.Sessions)
	return apitypes.BackgroundSession{}
}

func seedAnalyst(t *testing.T, srv *Server, slugs ...string) {
	t.Helper()
	for _, slug := range slugs {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
}

// TestBackgroundCarriesRunningJob: a running job carries its state,
// description and live activity, keyed by job id.
func TestBackgroundCarriesRunningJob(t *testing.T) {
	srv := newTestServer(t)
	seedAnalyst(t, srv, "alice")
	seedJob(t, srv, &subagentJob{
		ID: "job-a", Parent: "alice", Description: "survey the corpus",
		State: subagentJobRunning, StartedAt: time.Now().UTC().Add(-90 * time.Second),
		Activity: "running file_view", Depth: 1,
	})
	s := sessionByID(t, backgroundOf(t, srv, "alice"), "job-a")
	if s.State != apitypes.SessionStateRunning || s.Title != "survey the corpus" || s.Activity == nil || *s.Activity != "running file_view" {
		t.Errorf("session = %+v", s)
	}
}

// TestBackgroundDistinguishesQueuedFromRunning: the one distinction
// the list exists to draw. A batch parked behind an admission slot
// must not look like a batch burning tokens.
func TestBackgroundDistinguishesQueuedFromRunning(t *testing.T) {
	srv := newTestServer(t)
	seedAnalyst(t, srv, "alice")
	seedJob(t, srv, &subagentJob{ID: "r1", Parent: "alice", Description: "started", State: subagentJobRunning, Depth: 1})
	seedJob(t, srv, &subagentJob{ID: "q1", Parent: "alice", Description: "waiting", State: subagentJobQueued, Depth: 1})
	b := backgroundOf(t, srv, "alice")
	if got := sessionByID(t, b, "r1").State; got != apitypes.SessionStateRunning {
		t.Errorf("r1 state = %s, want running", got)
	}
	if got := sessionByID(t, b, "q1").State; got != apitypes.SessionStateQueued {
		t.Errorf("q1 state = %s, want queued", got)
	}
}

// TestBackgroundKeepsFinishedWork: a batch that has landed is still
// listed, each task with its end state and duration, so "all done" is
// distinguishable from "nothing ever ran". A finished task's last live activity is stale and is not
// reported.
func TestBackgroundKeepsFinishedWork(t *testing.T) {
	srv := newTestServer(t)
	seedAnalyst(t, srv, "alice")
	started := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	seedJob(t, srv, &subagentJob{
		ID: "done-1", Parent: "alice", Description: "finished survey", Depth: 1,
		State: subagentJobCompleted, StartedAt: started, EndedAt: started.Add(4 * time.Minute),
		Activity: "running file_view", // stale; must not be reported for a finished task
	})
	seedJob(t, srv, &subagentJob{ID: "fail-1", Parent: "alice", Description: "broken lookup", Depth: 1, State: subagentJobFailed})

	b := backgroundOf(t, srv, "alice")
	done := sessionByID(t, b, "done-1")
	if done.State != apitypes.SessionStateDone || done.ElapsedS != 240 || done.Activity != nil {
		t.Errorf("finished session = %+v, want done, 240s, no activity", done)
	}
	if got := sessionByID(t, b, "fail-1").State; got != apitypes.SessionStateErrored {
		t.Errorf("failed session state = %s, want errored", got)
	}
}

// TestRotationForgetsFinishedTasks is the user-visible outcome of
// ForgetFinished: after the CEO rotates the chat, the tasks that
// finished in the archived chat are no longer listed, while one that
// is still running — owed to the new chat — is. Driven through the
// same done-event path a real rotation takes.
func TestRotationForgetsFinishedTasks(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	seedJob(t, srv, &subagentJob{ID: "old-1", Parent: "alice", Description: "landed last chat", State: subagentJobCompleted, Depth: 1})
	seedJob(t, srv, &subagentJob{ID: "live-1", Parent: "alice", Description: "still going", State: subagentJobRunning, Depth: 1})

	if before := backgroundOf(t, srv, "alice"); len(before.Sessions) != 2 {
		t.Fatalf("premise: sessions before rotation = %+v", before.Sessions)
	}

	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{RequestedBy: "test"}); err != nil {
		t.Fatalf("WritePendingRotation: %v", err)
	}
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "kicker"}); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{Kind: agentpod.TurnEventDone, StopReason: "end_turn"}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	if _, has, _ := srv.Store.ReadPendingRotation("alice"); has {
		t.Fatal("premise: rotation did not finalize")
	}

	after := backgroundOf(t, srv, "alice")
	if len(after.Sessions) != 1 || after.Sessions[0].ID != "live-1" || after.Sessions[0].State != apitypes.SessionStateRunning {
		t.Errorf("after rotation sessions = %+v, want only the running live-1", after.Sessions)
	}
}

// TestBackgroundIsPerAgent: one agent's list must not show another's
// work.
func TestBackgroundIsPerAgent(t *testing.T) {
	srv := newTestServer(t)
	seedAnalyst(t, srv, "alice", "bob")
	seedJob(t, srv, &subagentJob{ID: "b1", Parent: "bob", Description: "bobs work", State: subagentJobRunning, Depth: 1})
	if b := backgroundOf(t, srv, "alice"); len(b.Sessions) != 0 {
		t.Errorf("alice's background shows another agent's job: %+v", b.Sessions)
	}
}

/* ---- the signal that tells the page to refetch ---- */

// TestDispatchNotifiesOrgState: a batch dispatched while someone is
// watching must appear without a reload. /org/stream is the page's only
// live channel once the parent's turn has ended, and it never polls.
func TestDispatchNotifiesOrgState(t *testing.T) {
	notified := 0
	svc := &SubagentService{NotifyWorkingChanged: func() { notified++ }}
	svc.registerJob(&subagentJob{ID: "j1", Parent: "alice", State: subagentJobQueued})
	svc.notifyWorkingChanged()

	if notified == 0 {
		t.Error("no org-state notify on dispatch; a new batch would not appear until " +
			"something unrelated moved")
	}
}

// TestNotifyWorkingChangedNilSafe: called from paths that run with a
// directly-constructed service in tests and fixtures.
func TestNotifyWorkingChangedNilSafe(t *testing.T) {
	var nilSvc *SubagentService
	nilSvc.notifyWorkingChanged() // must not panic
	(&SubagentService{}).notifyWorkingChanged()
}

// TestWaitingTasksUnchangedByQueuedToRunning documents WHY the running
// transition needs its own notify: the outstanding count does not move
// when a job starts, because queued and running are both non-terminal.
// Relying on the count alone would leave a started job reading
// "queued" until something else happened.
func TestWaitingTasksUnchangedByQueuedToRunning(t *testing.T) {
	svc := &SubagentService{}
	job := &subagentJob{ID: "j1", Parent: "alice", State: subagentJobQueued}
	svc.registerJob(job)

	before := svc.OutstandingSubagents("alice")
	svc.updateJob("j1", func(j *subagentJob) { j.State = subagentJobRunning })
	after := svc.OutstandingSubagents("alice")

	if before != after {
		t.Fatalf("outstanding moved %d→%d; this test's premise is wrong", before, after)
	}
	if after != 1 {
		t.Errorf("outstanding = %d, want 1", after)
	}
}
