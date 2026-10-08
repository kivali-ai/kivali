package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestStopCancelsBackgroundWorkWithNoLiveTurn: an agent that
// dispatched background subagents ends its own turn, so by the time
// anyone reaches for Stop its hub is completed (lingering for late SSE
// replay) or already evicted. Stop still reaches the cancellation in
// both cases.
func TestStopCancelsBackgroundWorkWithNoLiveTurn(t *testing.T) {
	cases := []struct {
		name string
		// hub models what the parent left behind when its turn ended.
		hub func() *chatHub
	}{
		{
			name: "hub completed — turn ended, lingering for replay",
			hub: func() *chatHub {
				h := &chatHub{hub: newHub(), slug: "alice"}
				h.markCompleted()
				return h
			},
		},
		{
			name: "hub evicted — nothing left of the turn at all",
			hub:  func() *chatHub { return nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if h := tc.hub(); h != nil {
				srv.chatHubs = map[string]*chatHub{"alice": h}
			}

			// The production cancel unwinds DriveSubagent, whose
			// goroutine then writes the terminal state. Here we only
			// need to prove the handler reaches them.
			cancelled := 0
			srv.SubagentService = &SubagentService{}
			for _, id := range []string{"job-a", "job-b"} {
				srv.SubagentService.registerJob(&subagentJob{
					ID: id, Parent: "alice", State: subagentJobRunning,
					cancel: func() { cancelled++ },
				})
			}

			req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
			rr := httptest.NewRecorder()
			authedHandler(t, srv).ServeHTTP(rr, req)

			if cancelled != 2 {
				t.Errorf("cancelled %d of 2 background jobs — Stop did not reach the batch", cancelled)
			}
			if rr.Code != http.StatusAccepted {
				t.Errorf("code = %d, want 202: Stop acted (it cancelled background work), "+
					"so it must not report the no-op 204", rr.Code)
			}
			// The marker rule is unchanged: no in-flight loop produced
			// an interruption, and writing one quarantines the agent.
			hist, _ := srv.Store.ReadChatHistory("alice")
			for _, m := range hist {
				if m.Kind == store.KindUserInterruption {
					t.Errorf("user-interruption entry written with no live turn: %+v", m)
				}
			}
		})
	}
}

// TestStopCancelsQueuedBackgroundWork: a queued job has no turn at the
// pod yet, so only its context cancel can stop it. If Stop skipped
// those they would start afterwards — the CEO says stop and more work
// begins.
func TestStopCancelsQueuedBackgroundWork(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cancelled := 0
	srv.SubagentService = &SubagentService{}
	srv.SubagentService.registerJob(&subagentJob{
		ID: "queued-1", Parent: "alice", State: subagentJobQueued,
		cancel: func() { cancelled++ },
	})

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)

	if cancelled != 1 {
		t.Errorf("queued job not cancelled (cancelled=%d); it would start after Stop", cancelled)
	}
	if rr.Code != http.StatusAccepted {
		t.Errorf("code = %d, want 202", rr.Code)
	}
}

// TestStopLeavesFinishedJobsAlone: a terminal job must not be
// re-cancelled. Its goroutine is gone and its result may already be in
// flight to the parent.
func TestStopLeavesFinishedJobsAlone(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	touched := 0
	srv.SubagentService = &SubagentService{}
	srv.SubagentService.registerJob(&subagentJob{
		ID: "done-1", Parent: "alice", State: subagentJobCompleted,
		cancel: func() { touched++ },
	})

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)

	if touched != 0 {
		t.Errorf("cancelled a finished job %d time(s)", touched)
	}
	// Nothing live and nothing outstanding: still the no-op answer.
	if rr.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204 (genuinely idle)", rr.Code)
	}
}

// TestStopStillInterruptsALiveTurn guards the path that already
// worked. Widening Stop to reach background jobs must not cost it the
// marker + interrupt it writes for an agent that IS mid-turn.
func TestStopStillInterruptsALiveTurn(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.chatHubs = map[string]*chatHub{"alice": hub}

	req := httptest.NewRequest(http.MethodPost, "/agents/alice/stop", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("code = %d, want 202", rr.Code)
	}
	hub.mu.Lock()
	interrupted := hub.interruptRequested
	hub.mu.Unlock()
	if !interrupted {
		t.Error("live hub not interrupted")
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	found := false
	for _, m := range hist {
		if m.Kind == store.KindUserInterruption {
			found = true
		}
	}
	if !found {
		t.Error("no user-interruption entry for a Stop on a live turn")
	}
}

// TestCancelAllSubagentsIsPerParent: Stop on one agent must not reach
// into another agent's batch.
func TestCancelAllSubagentsIsPerParent(t *testing.T) {
	svc := &SubagentService{}
	aliceCancels, bobCancels := 0, 0
	svc.registerJob(&subagentJob{ID: "a1", Parent: "alice", State: subagentJobRunning, cancel: func() { aliceCancels++ }})
	svc.registerJob(&subagentJob{ID: "b1", Parent: "bob", State: subagentJobRunning, cancel: func() { bobCancels++ }})

	if n := svc.CancelAllSubagents("alice"); n != 1 {
		t.Errorf("CancelAllSubagents(alice) = %d, want 1", n)
	}
	if aliceCancels != 1 || bobCancels != 0 {
		t.Errorf("alice=%d bob=%d — Stop crossed an ownership boundary", aliceCancels, bobCancels)
	}
}

// TestCancelAllSubagentsNilServiceIsSafe: the handler calls this on
// every Stop, including deployments with no subagent service wired.
func TestCancelAllSubagentsNilServiceIsSafe(t *testing.T) {
	var svc *SubagentService
	if n := svc.CancelAllSubagents("alice"); n != 0 {
		t.Errorf("nil service returned %d", n)
	}
}

// TestFinishJobNotifiesWorkingChanged: /org/stream is event-driven
// with no periodic fallback, and a cancelled job delivers no result —
// so without this push the "waiting on N tasks" bubble and the sidebar
// dot survive the Stop that cancelled the work they describe.
func TestFinishJobNotifiesWorkingChanged(t *testing.T) {
	notified := 0
	svc := &SubagentService{
		Provider:             provider.MockProvider{},
		NotifyWorkingChanged: func() { notified++ },
		EmitToParent:         func(string, string, any) {},
		DeliverToParent:      func(string, store.ChatMessage) error { return nil },
	}
	job := &subagentJob{ID: "j1", Parent: "alice", State: subagentJobRunning, done: make(chan struct{})}
	svc.registerJob(job)

	svc.finishJob(job, "tool-1", 0, "", context.Canceled)

	if notified == 0 {
		t.Error("finishJob did not push an org-state notify; the waiting indicator " +
			"would stay up after the job it describes is gone")
	}
	if n := svc.OutstandingSubagents("alice"); n != 0 {
		t.Errorf("outstanding = %d after finishJob, want 0", n)
	}
}
