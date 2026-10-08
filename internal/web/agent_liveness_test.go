package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestAgentChatRunningForTurnEngineRun: an agent running inside the
// runtime shows the thinking indicator in its chat as well as the
// working dot in the sidebar. The chat API's running flag is chatHubs ∪
// the runtime's running set; this locks that in.
func TestAgentChatRunningForTurnEngineRun(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Stub a runtime reporting alice as running. The handler only
	// calls Runtime.RunningAgents() / Runtime.Running(), so a
	// minimal Runtime with a primed running map suffices.
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	primeRunning(srv.Runtime, "alice")

	if c := agentChatView(t, srv, "alice"); !c.Running {
		t.Errorf("chat reads running = false while the engine runs this slug; the thinking indicator would not show")
	}
}

// TestAgentChatNotRunningWhenIdle is the negative case — without an
// engine run or chat hub, the chat does not read as running.
func TestAgentChatNotRunningWhenIdle(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if c := agentChatView(t, srv, "alice"); c.Running {
		t.Errorf("idle agent reads as running")
	}
}

// TestAgentStreamHoldsOpenWhileEngineRuns is the SSE counterpart:
// when only the engine is running this slug (no chat hub), the
// /agents/<slug>/stream handler must hold the connection open and
// emit a `done` event when the engine releases the slug — not
// 204-and-disconnect, which would yank the thinking indicator
// client-side.
func TestAgentStreamHoldsOpenWhileEngineRuns(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Wire OnRunningChange to NotifyOrgState exactly as main.go does
	// — without this, runtime state transitions never reach the
	// org-hub, so the wait-stream never wakes from its subscribe.
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	srv.Runtime.SetOnRunningChange(srv.NotifyOrgState)
	primeRunning(srv.Runtime, "alice")

	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", hs.URL+"/agents/alice/stream", nil)
	req.AddCookie(authCookie(t, srv))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (engine is running this slug)", resp.StatusCode)
	}

	// Release the slug from the runtime — OnRunningChange fires,
	// NotifyOrgState pushes a snapshot, the wait-stream wakes,
	// re-checks gating, sees neither runtime nor hub owns alice,
	// emits `done` and closes.
	primeRunning(srv.Runtime) // empty → alice no longer in running set

	gotDone := make(chan bool, 1)
	go func() {
		buf := make([]byte, 1024)
		var acc []byte
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				if strings.Contains(string(acc), "event: done") {
					gotDone <- true
					return
				}
			}
			if err == io.EOF || err != nil {
				break
			}
		}
		gotDone <- false
	}()

	select {
	case ok := <-gotDone:
		if !ok {
			t.Errorf("expected `event: done` after engine released the slug")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for done event")
	}
}

// primeRunning pokes Runtime.MarkRunning for each slug listed (or
// clears the running set when called with no slugs). Lets tests
// simulate runtime state without spinning a real RunAgent call.
func primeRunning(r *agent.Runtime, slugs ...string) {
	agent.PrimeRunningForTest(r, slugs)
}
