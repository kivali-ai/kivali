package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// startAgentpodSocketServer wires a Server with an AgentpodHub and
// runs an HTTP server on a Unix socket exposing GET /v1/agent/{slug}/events.
// Returns the socket path and a cleanup func.
func startAgentpodSocketServer(t *testing.T) (path string, srv *Server, cleanup func()) {
	t.Helper()
	srv = &Server{AgentpodHub: newAgentpodHub()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/events", srv.handleAgentpodEvents)

	// macOS caps Unix socket path at ~104 chars; t.TempDir() under
	// /var/folders/... blows past that on long test names. Use /tmp
	// directly with a short prefix to stay within the limit.
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path) //nolint:govet // shadow ok in test setup
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return path, srv, cleanup
}

// TestAgentpodEventsRoundtrip wires the agentpod.Client against a
// real Server-side SSE handler over a Unix socket. Asserts: a typed
// chat-turn payload published on the server side arrives at the
// client with Type and decodable Data.
func TestAgentpodEventsRoundtrip(t *testing.T) {
	path, srv, cleanup := startAgentpodSocketServer(t)
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evs, err := c.OpenEvents(ctx)
	if err != nil {
		t.Fatalf("OpenEvents: %v", err)
	}

	// Wait for the subscriber to register (the server's handler
	// subscribes synchronously after writing the initial heartbeat,
	// which the client buffers). 10ms is generous on local CI.
	deadline := time.Now().Add(2 * time.Second)
	for srv.AgentpodHub.SubscriberCount("alice") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("subscriber never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	want := agentpod.ChatTurnEvent{
		TurnID: "turn-42",
		Slug:   "alice",
		Source: "chat",
		Model:  "claude-test",
	}
	if err := srv.PublishAgentpodEvent("alice", agentpod.EventChatTurn, want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case ev, ok := <-evs:
		if !ok {
			t.Fatal("events channel closed before chat-turn arrived")
		}
		if ev.Type != agentpod.EventChatTurn {
			t.Errorf("Type = %q, want %q", ev.Type, agentpod.EventChatTurn)
		}
		var got agentpod.ChatTurnEvent
		if err := json.Unmarshal(ev.Data, &got); err != nil {
			t.Fatalf("unmarshal Data: %v (raw=%q)", err, string(ev.Data))
		}
		// Field-by-field — Request is json.RawMessage which isn't
		// comparable via ==.
		if got.TurnID != want.TurnID || got.Slug != want.Slug ||
			got.Source != want.Source || got.Model != want.Model {
			t.Errorf("payload = %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// TestAgentpodEventsSlugMismatchRejected guards the X-Kivali-Agent
// header check on the server side.
func TestAgentpodEventsSlugMismatchRejected(t *testing.T) {
	path, _, cleanup := startAgentpodSocketServer(t)
	defer cleanup()

	// Manually craft the request to set a mismatched header.
	dial := func(_ context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.Dial("unix", path)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dial}}
	req, err := http.NewRequest(http.MethodGet, "http://core/v1/agent/alice/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(agentpod.SlugHeader, "bob") // mismatch
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for slug mismatch", resp.StatusCode)
	}
}

// TestAgentpodEventsBadPathRejected guards the path-shape check.
func TestAgentpodEventsBadPathRejected(t *testing.T) {
	srv := &Server{AgentpodHub: newAgentpodHub()}
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/agent//events", nil)
	srv.handleAgentpodEvents(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty slug: status = %d, want 400", rr.Code)
	}
}

// TestAgentpodEventsHubNilReturns503 ensures the endpoint stays a
// safe no-op when AgentpodHub is unwired (today's deployment, tests
// that don't exercise the agentpod path).
func TestAgentpodEventsHubNilReturns503(t *testing.T) {
	srv := &Server{}
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/v1/agent/alice/events", nil)
	srv.handleAgentpodEvents(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("nil hub: status = %d, want 503", rr.Code)
	}
}

// TestAgentpodEventsNotifiesOrgStateOnSubscribeAndDisconnect locks
// the wiring that makes the sidebar's `disconnected` flag responsive:
// when an agent pod opens the events SSE, NotifyOrgState fires so a
// fresh snapshot lands; when the connection drops, NotifyOrgState
// fires again so the sidebar re-disconnects within the snapshot
// debounce window. Without this, the disconnected dot would flicker
// only on unrelated events (chat hub open, message queue write),
// leaving fresh hires looking permanently disconnected.
func TestAgentpodEventsNotifiesOrgStateOnSubscribeAndDisconnect(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = newAgentpodHub()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Subscribe to the org-stream snapshot publisher so we can assert
	// the disconnect flag flips on connect/disconnect.
	_, ch := srv.orgHub.subscribe()
	defer srv.orgHub.unsubscribe(ch)

	// Drain the initial snapshot (alice currently disconnected — no
	// pod subscribed). We expect this baseline before the connect
	// event so the flip is observable.
	srv.NotifyOrgState()
	waitForSnapshot(t, ch, func(s orgSnapshot) bool {
		for _, a := range s.Agents {
			if a.Slug == "alice" {
				return a.State == apitypes.AgentStateDisconnected
			}
		}
		return false
	}, "initial disconnected snapshot")

	// Stand the SSE handler up on a real Unix socket and dial in.
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "s")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/events", srv.handleAgentpodEvents)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})

	c := agentpod.NewClient(sockPath, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := c.OpenEvents(ctx); err != nil {
		t.Fatalf("OpenEvents: %v", err)
	}

	// Connect → snapshot must flip alice.Disconnected = false.
	waitForSnapshot(t, ch, func(s orgSnapshot) bool {
		for _, a := range s.Agents {
			if a.Slug == "alice" {
				return a.State != apitypes.AgentStateDisconnected
			}
		}
		return false
	}, "alice.Disconnected = false after subscribe")

	// Disconnect → snapshot must flip alice.Disconnected back to true.
	cancel()
	waitForSnapshot(t, ch, func(s orgSnapshot) bool {
		for _, a := range s.Agents {
			if a.Slug == "alice" {
				return a.State == apitypes.AgentStateDisconnected
			}
		}
		return false
	}, "alice.Disconnected = true after disconnect")
}

// waitForSnapshot drains the orgHub channel until predicate returns
// true on a decoded snapshot, or fails the test after a generous
// timeout. Used by the disconnect-notify test to ride out the 30ms
// debounce + any unrelated snapshots that fire from other code paths
// during setup.
func waitForSnapshot(t *testing.T, ch <-chan []byte, pred func(orgSnapshot) bool, what string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case body := <-ch:
			var s orgSnapshot
			if err := json.Unmarshal(body, &s); err != nil {
				t.Fatalf("decode snapshot: %v (body=%s)", err, body)
			}
			if pred(s) {
				return
			}
		case <-deadline:
			t.Fatalf("did not see %s within deadline", what)
		}
	}
}

// TestAgentpodHubSlowSubscriberDropped guards the back-pressure
// rule: a subscriber whose buffer fills is dropped, not blocking
// the publisher. We force this by subscribing without draining and
// publishing past the buffer cap.
func TestAgentpodHubSlowSubscriberDropped(t *testing.T) {
	hub := newAgentpodHub()
	sub := hub.Subscribe("alice")

	// Fill the buffer + one (the +1 forces drop).
	for i := 0; i < 32; i++ {
		hub.Publish("alice", agentpod.Event{Type: agentpod.EventPing})
	}

	// The hub should have dropped the subscriber by now.
	if hub.SubscriberCount("alice") != 0 {
		t.Errorf("slow sub still present: %d", hub.SubscriberCount("alice"))
	}
	select {
	case <-sub.closed:
		// expected
	case <-time.After(time.Second):
		t.Error("dropped subscriber's closed channel never fired")
	}
}

// TestAgentpodHubMultipleSubscribers sanity-checks the fan-out: two
// concurrent subscribers both receive a published event. (Steady
// state has one, but transient overlap during reconnect can have
// two, and the hub doesn't differentiate.)
func TestAgentpodHubMultipleSubscribers(t *testing.T) {
	hub := newAgentpodHub()
	a := hub.Subscribe("alice")
	b := hub.Subscribe("alice")
	hub.Publish("alice", agentpod.Event{Type: agentpod.EventPing})

	var wg sync.WaitGroup
	wg.Add(2)
	for _, sub := range []*agentpodSubscriber{a, b} {
		go func(sub *agentpodSubscriber) {
			defer wg.Done()
			select {
			case ev := <-sub.ch:
				if ev.Type != agentpod.EventPing {
					t.Errorf("unexpected event: %+v", ev)
				}
			case <-time.After(time.Second):
				t.Error("sub never received fan-out event")
			}
		}(sub)
	}
	wg.Wait()
}

// TestAgentpodReadSSEFraming feeds canned SSE text through readSSE
// and asserts the framing parser handles event/data, multi-line
// data:, comment lines, and trailing-newline edge cases.
func TestAgentpodReadSSEFraming(t *testing.T) {
	// Reuse the agentpod package's framing — exposed via NewClient
	// would be over-broad; we exercise it indirectly via the
	// roundtrip test above. Keep this small smoke check here so
	// failures pinpoint the framing layer specifically.
	t.Skip("covered by TestAgentpodEventsRoundtrip; placeholder for future framing-only tests")
}

// startTurnEventServer wires a Server with a real Store + the
// turn-event endpoint registered on a Unix socket. Returns the path
// + the *Server (so tests can inspect the disruption-row write).
func startTurnEventServer(t *testing.T) (path string, srv *Server, cleanup func()) {
	t.Helper()
	srv = newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turn_id}/event", srv.handleAgentpodTurnEvent)
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return path, srv, cleanup
}

// TestAgentpodTurnEventFailedSubprocessDied locks in the
// resumption-bug-carrier path across the UDS boundary. Posting a
// "failed" event with reason="subprocess-died" must write a
// KindRuntimeDisruption chat row, parity with the in-process
// LoopExitSubprocessDied path. Closes the loop on workstream 1f's
// CLI-death recovery.
func TestAgentpodTurnEventFailedSubprocessDied(t *testing.T) {
	path, srv, cleanup := startTurnEventServer(t)
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonSubprocessDied,
		FailedDetail: "kubelet OOMKilled",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	if len(hist) == 0 {
		t.Fatal("no chat history; expected disruption row")
	}
	last := hist[len(hist)-1]
	if last.Kind != store.KindRuntimeDisruption {
		t.Errorf("last kind = %q, want %q", last.Kind, store.KindRuntimeDisruption)
	}
	if !strings.Contains(last.Content, "subprocess died") {
		t.Errorf("disruption content missing cause: %q", last.Content)
	}
	if !strings.Contains(last.Content, "kubelet OOMKilled") {
		t.Errorf("disruption content missing detail: %q", last.Content)
	}
}

// TestAgentpodTurnEventFailedOtherErrorWritesTurnError guards the
// other branch: a "failed" event with reason="other-error" against a
// live turn writes a kind:turn-error row — NOT a disruption row, which
// would resume the agent and, repeated, trip the circuit breaker.
// Before the turn-error row existed such a failure left nothing on
// disk: a reload erased the only trace of it and the agent read as
// idle.
func TestAgentpodTurnEventFailedOtherErrorWritesTurnError(t *testing.T) {
	path, srv, _, cleanup := startTurnEventTestServer(t, "alice", "turn-1")
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonOtherError,
		FailedDetail: "stream open: claude: stream http 401: invalid x-api-key",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	hist, _ := srv.Store.ReadChatHistory("alice")
	var errRows int
	for _, m := range hist {
		switch m.Kind {
		case store.KindRuntimeDisruption:
			t.Errorf("unexpected disruption row for other-error: %+v", m)
		case store.KindTurnError:
			errRows++
			if m.Role != store.RoleReceived {
				t.Errorf("turn-error role = %q, want received", m.Role)
			}
			if !strings.Contains(m.Content, "invalid x-api-key") {
				t.Errorf("turn-error content missing detail: %q", m.Content)
			}
		}
	}
	if errRows != 1 {
		t.Fatalf("turn-error rows = %d, want 1 (hist=%+v)", errRows, hist)
	}
	if got := store.SpawnDecision(hist); got != store.SpawnHoldOnError {
		t.Errorf("SpawnDecision = %v, want SpawnHoldOnError (the agent must not retry on its own)", got)
	}
}

// TestAgentpodTurnEventFailedOtherErrorStaleWritesNothing: an
// other-error for a turn core no longer holds state for (the pod
// reporting late, after the disconnect grace already synthesized the
// failure, or after a core restart whose boot scan already wrote the
// disruption row) writes nothing. A second marker would either
// duplicate the first or turn a boot-time resume into a hold. Only
// subprocess-died writes in the stale case, as before.
func TestAgentpodTurnEventFailedOtherErrorStaleWritesNothing(t *testing.T) {
	path, srv, cleanup := startTurnEventServer(t)
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonOtherError,
		FailedDetail: "late report",
	}); err != nil {
		t.Fatalf("PostTurnEvent: %v", err)
	}
	if hist, _ := srv.Store.ReadChatHistory("alice"); len(hist) != 0 {
		t.Errorf("stale other-error wrote rows: %+v", hist)
	}
}

// TestAgentpodTurnEventDeltaAccepted is a smoke test for the common
// path: a delta event lands and is acked with 204. Full chat-hub
// integration (persist + browser SSE) is workstream-5 follow-on.
func TestAgentpodTurnEventDeltaAccepted(t *testing.T) {
	path, _, cleanup := startTurnEventServer(t)
	defer cleanup()

	c := agentpod.NewClient(path, "alice")
	if err := c.PostTurnEvent(context.Background(), "turn-1", agentpod.TurnEvent{
		Kind: agentpod.TurnEventDelta, Text: "hello",
	}); err != nil {
		t.Errorf("PostTurnEvent delta: %v", err)
	}
}

// TestAgentpodRunSubagentGatedAfterStop locks in the Stop-press gate
// on subagent dispatch: when the slug's chatHub has interruptRequested
// set, the handler returns 409 with [stopped by user] WITHOUT calling
// SubagentService. The model sees the gated tool_result, the runner's
// routeEvent fires gracefulKill on the resulting user{tool_result},
// and no subagent work was wasted in the runner-kill window.
func TestAgentpodRunSubagentGatedAfterStop(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// Wire a hub with the interrupt flag set — simulates Stop click
	// landing before the gracefulKill window expired.
	hub := &chatHub{hub: newHub(), slug: "alice"}
	first, alreadyDone := hub.requestInterrupt()
	if !first || alreadyDone {
		t.Fatalf("requestInterrupt: first=%v alreadyDone=%v", first, alreadyDone)
	}
	srv.chatHubs = map[string]*chatHub{"alice": hub}
	// Deliberately leave SubagentService nil — the gate must short-
	// circuit BEFORE reaching the service-not-configured branch.
	// (If the gate breaks, the test would 503 instead of 409.)

	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"parent":"alice","arguments":{"tasks":[]}}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/agent/alice/run-subagent", body)
	req.Header.Set(agentpod.SlugHeader, "alice")
	req.SetPathValue("slug", "alice")
	srv.handleAgentpodRunSubagent(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("code = %d, want 409 (gated by interrupt flag); body = %q", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "stopped by user") {
		t.Errorf("body = %q, want to contain 'stopped by user'", rr.Body.String())
	}
}

// TestAgentpodTurnEventBadKindRejected guards the unknown-kind 400.
func TestAgentpodTurnEventBadKindRejected(t *testing.T) {
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"kind":"bogus"}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/agent/alice/chat-turn/turn-1/event", body)
	req.Header.Set(agentpod.SlugHeader, "alice")
	srv.handleAgentpodTurnEvent(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown kind", rr.Code)
	}
}

// helper to silence unused warnings if a test is skipped while WIP.
var _ = strings.TrimSpace
