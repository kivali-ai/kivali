package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestStartAgentpodSocketServesRoutes locks in the second-listener
// shape: StartAgentpodSocket binds a hostPath-style UDS path and
// serves the same handler set the control socket does. End-to-end
// check via the agentpod.Client GET /v1/skills request.
func TestStartAgentpodSocketServesRoutes(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()

	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")

	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket: %v", err)
	}
	t.Cleanup(stop)

	c := agentpod.NewClient(path, "alice")
	skills, err := c.ListSkills(context.Background())
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	if skills == nil {
		t.Errorf("ListSkills returned nil; expected empty slice")
	}
}

// TestStartAgentpodSocketEmptyPathNoOp guards the disabled path:
// passing "" returns a no-op closer so main.go's wiring can call it
// unconditionally without branching the call site.
func TestStartAgentpodSocketEmptyPathNoOp(t *testing.T) {
	srv := newTestServer(t)
	stop, err := srv.StartAgentpodSocket("")
	if err != nil {
		t.Fatalf("StartAgentpodSocket(\"\"): %v", err)
	}
	if stop == nil {
		t.Fatal("nil closer on empty path")
	}
	stop() // must not panic
}

// TestStartAgentpodSocketServesEvents proves the events SSE route is
// wired alongside the read-only routes — same surface as the control
// socket. A subscribe arrives within a short window after open;
// cancel cleanly tears down.
func TestStartAgentpodSocketServesEvents(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")

	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket: %v", err)
	}
	t.Cleanup(stop)

	c := agentpod.NewClient(path, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evs, err := c.OpenEvents(ctx)
	if err != nil {
		t.Fatalf("OpenEvents: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for srv.AgentpodHub.SubscriberCount("alice") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber never registered on hostPath listener")
		}
		time.Sleep(5 * time.Millisecond)
	}
	want := agentpod.ChatTurnEvent{
		TurnID: "turn-1", Slug: "alice", Source: "chat", Model: "claude-test",
	}
	if err := srv.PublishAgentpodEvent("alice", agentpod.EventChatTurn, want); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case ev, ok := <-evs:
		if !ok {
			t.Fatal("events channel closed before delivery")
		}
		if ev.Type != agentpod.EventChatTurn {
			t.Errorf("type = %q, want %q", ev.Type, agentpod.EventChatTurn)
		}
		var got agentpod.ChatTurnEvent
		if err := json.Unmarshal(ev.Data, &got); err != nil {
			t.Errorf("decode: %v", err)
		}
		if got.TurnID != want.TurnID {
			t.Errorf("TurnID = %q, want %q", got.TurnID, want.TurnID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event never arrived through hostPath listener")
	}
}

// TestStartAgentpodSocketStaleFileRemoved proves the bind path
// removes any stale socket file from a crashed prior run before
// Listen — otherwise the listen would EADDRINUSE on every restart.
func TestStartAgentpodSocketStaleFileRemoved(t *testing.T) {
	srv := newTestServer(t)
	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")

	// Plant a stale socket-like file at the bind path.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("plant stale: %v", err)
	}

	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket should evict stale file: %v", err)
	}
	t.Cleanup(stop)

	// Sanity check: a client can connect — meaning the listener is up
	// on the same path the stale file occupied.
	c := agentpod.NewClient(path, "alice")
	if _, err := c.ListSkills(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("ListSkills after stale-file evict: %v", err)
	}
}

// TestJSONBodyCap locks in the OOM-prevention bound: a JSON body
// larger than agentpodMaxJSONBody is rejected at decode time before
// the server tries to buffer it all. Without the cap, a compromised
// or buggy agent runtime could OOM Kivali web with a multi-GB body.
func TestJSONBodyCap(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")
	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket: %v", err)
	}
	t.Cleanup(stop)

	// Build a body that exceeds the cap.
	huge := bytes.Repeat([]byte("a"), agentpodMaxJSONBody+1024)
	envelope := append([]byte(`{"body":"`), huge...)
	envelope = append(envelope, []byte(`"}`)...)

	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		// The whole 16MiB has to cross the socket before the cap can
		// reject it, and -race makes every hop through the stack far
		// dearer: measured ~23x here — 0.58s plain against 13.2-13.6s
		// instrumented, over five runs. A 5s deadline would be a
		// guaranteed failure under `make race-test`, which gates a
		// release.
		//
		// The deadline is not tuned to that cost. It exists only so a
		// wedged server fails here, with the request that wedged it,
		// instead of hanging until go test's package timeout fires
		// and names nothing. Set it clear of any plausible machine
		// and leave it alone. The 2-5s deadlines on the other clients
		// in this file are fine: their bodies are a few hundred bytes,
		// so their cost does not scale with agentpodMaxJSONBody.
		Timeout: 90 * time.Second,
	}
	req, _ := http.NewRequest(http.MethodPost, "http://x/v1/agent/alice/role", bytes.NewReader(envelope))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agentpod.SlugHeader, "alice")
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	// http.MaxBytesReader returns an error on overflow which the
	// decoder wraps; the handler responds with 4xx (BadRequest in
	// most paths). The exact code is less important than NOT 200.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		t.Errorf("oversized body got %d, want 4xx (cap should reject before the handler accepts)", resp.StatusCode)
	}
}

// TestSlugHeaderEmptyRejected locks in the slug binding: the
// X-Kivali-Agent header must be present AND match the URL slug, so a
// pod that can dial the hostPath UDS cannot address requests to any
// other slug's path by sending an empty header.
func TestSlugHeaderEmptyRejected(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}

	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")

	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket: %v", err)
	}
	t.Cleanup(stop)

	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		Timeout: 2 * time.Second,
	}

	// 1. POST /v1/agent/alice/role with NO header → 403.
	body, _ := json.Marshal(map[string]string{"body": "evil role"})
	req, _ := http.NewRequest(http.MethodPost, "http://x/v1/agent/alice/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("no-header POST role: status=%d, want 403", resp.StatusCode)
	}

	// 2. POST /v1/agent/alice/role with mismatched header → 403.
	req2, _ := http.NewRequest(http.MethodPost, "http://x/v1/agent/alice/role", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set(agentpod.SlugHeader, "bob")
	resp2, err := httpc.Do(req2)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("mismatched-header POST role: status=%d, want 403", resp2.StatusCode)
	}
}

// TestReadAgentRoleGateNeedsCoSCaller locks in the CoS-only gate:
// even if a non-CoS agent calls the state-dispatch endpoint at
// /v1/agent/<other>/state/dispatch, the read_agent_role dispatcher
// refuses unless the BOUND CALLER is chief-of-staff. The slug header
// binds the URL slug to the verified caller, so this test exercises
// the full chain.
func TestReadAgentRoleGateNeedsCoSCaller(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "alice's role"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Researcher", ReportsTo: "ceo"}, "bob's role"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")
	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket: %v", err)
	}
	t.Cleanup(stop)

	// Alice tries to read bob's role via the state dispatcher. With
	// the matching slug header (URL = header = bound caller), this
	// reaches the dispatcher; the dispatcher's IsChiefOfStaff check
	// derives from URL slug == "alice", which is false → tool refuses.
	c := agentpod.NewClient(path, "alice")
	resp, err := c.StateDispatch(context.Background(), "read_agent_role", []byte(`{"slug":"bob"}`))
	if err != nil {
		t.Fatalf("StateDispatch: %v", err)
	}
	if !resp.IsError {
		t.Errorf("non-CoS read_agent_role returned IsError=false; should refuse. body=%q", resp.Body)
	}
}

// TestSlugRegexRejectsTraversal locks in the slug-format check on
// the URL parser: even with a valid slug-header, a path like
// /v1/agent/../role must be rejected before the slug ever reaches
// store.path("agents", slug). Without ValidateSlug at the parser,
// slug=".." would resolve to the data root after Clean.
func TestSlugRegexRejectsTraversal(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()

	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")
	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket: %v", err)
	}
	t.Cleanup(stop)

	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		Timeout: 2 * time.Second,
	}

	// Each shape should be rejected at the URL parser before any
	// downstream handler runs. The actual response code is whatever
	// the http.ServeMux falls back to (404) when no route matches.
	for _, badSlug := range []string{"..", "_archived", "../etc", "ALICE", "alice/bob", "alice%2Fbob"} {
		body, _ := json.Marshal(map[string]string{"body": "x"})
		url := "http://x/v1/agent/" + badSlug + "/role"
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(agentpod.SlugHeader, badSlug)
		resp, err := httpc.Do(req)
		if err != nil {
			// Some malformed paths fail at request build / dial — that's also a rejection.
			continue
		}
		// The response must not be 2xx; the role write must not have fired.
		if resp.StatusCode/100 == 2 {
			t.Errorf("slug=%q got 2xx (write reached the handler): %s", badSlug, resp.Status)
		}
		_ = resp.Body.Close()
	}
}

// TestRouteMessageStampsFrom locks in the From-spoofing fix: when
// a caller posts a message with From=ceo (a spoof) addressed to a
// nonexistent recipient, the server's dead-letter goes back to the
// REAL sender (the URL/header slug, alice), not to the spoofed
// "ceo". Without the override, the dead-letter would land in ceo's
// queue, hiding the spoofed publish from the real culprit and
// trickling alarming "your message bounced" notifications to the
// CEO for things they never sent.
func TestRouteMessageStampsFrom(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentpodHub = NewAgentpodHub()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)

	dir, err := os.MkdirTemp("/tmp", "wos-agentpod-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "core.sock")
	stop, err := srv.StartAgentpodSocket(path)
	if err != nil {
		t.Fatalf("StartAgentpodSocket: %v", err)
	}
	t.Cleanup(stop)

	// Spoofed publish: claim From=ceo, send to a nonexistent
	// recipient. The dead-letter should go to alice (real sender),
	// not ceo (spoof).
	msg := store.Message{
		Type: store.MsgNotice, Title: "spoof",
		From: "ceo", To: store.Recipients{"ghost"}, Body: "spoofed",
	}
	abs, err := srv.Store.WriteMessage(msg)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	msg.Path = abs

	body, _ := json.Marshal(msg)
	httpc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		Timeout: 5 * time.Second,
	}
	req, _ := http.NewRequest(http.MethodPost, "http://x/v1/agent/alice/route-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agentpod.SlugHeader, "alice")
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d, want 204", resp.StatusCode)
	}

	// Dead-letter went to alice (real sender), not ceo (spoofed).
	q, err := srv.Store.ReadMessageQueue()
	if err != nil {
		t.Fatalf("ReadMessageQueue: %v", err)
	}
	if n := len(q.Agents["ceo"].Inbox); n != 0 {
		t.Errorf("ceo inbox got %d dead-letters; the From spoof landed at the spoofed identity", n)
	}
	if n := len(q.Agents["alice"].Inbox); n != 1 {
		t.Errorf("alice inbox got %d dead-letters, want 1 (real sender should receive the bounce)", n)
	}
}
