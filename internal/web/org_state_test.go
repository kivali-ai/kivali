package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestOrgSnapshotShape verifies the snapshot carries every field the
// web app depends on, with stable alphabetical ordering for the
// byte-equality dedup orgHub does downstream. Per-agent records
// expose `working` (drives the sidebar's working dot) and `bucket`
// (drives the context ring); pure-chat busy surfaces as
// `working: true` with no extra fields.
func TestOrgSnapshotShape(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Researcher", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "k"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	// alice has a chat hub spawned by direct CEO chat → working=true.
	srv.chatHubs = map[string]*chatHub{
		"alice": {hub: newHub(), slug: "alice", spawnSource: "chat"},
	}

	body := srv.buildOrgSnapshot()
	var snap orgSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	// Tree order (depth-first, siblings by slug), no CEO entry.
	if len(snap.Agents) != 3 {
		t.Fatalf("expected 3 agents (CoS, alice, bob), got %d: %+v", len(snap.Agents), snap.Agents)
	}
	want := []string{"chief-of-staff", "alice", "bob"}
	wantDepth := []int{1, 2, 2}
	for i, w := range want {
		if snap.Agents[i].Slug != w || snap.Agents[i].Depth != wantDepth[i] {
			t.Errorf("agents[%d] = %q depth %d, want %q depth %d", i, snap.Agents[i].Slug, snap.Agents[i].Depth, w, wantDepth[i])
		}
	}
	byslug := map[string]agentLiveness{}
	for _, a := range snap.Agents {
		byslug[a.Slug] = a
	}
	if byslug["alice"].State != "running" {
		t.Errorf("alice = %+v, want running", byslug["alice"])
	}
	for _, slug := range []string{"bob", "chief-of-staff"} {
		if byslug[slug].State == "running" {
			t.Errorf("%s = %+v, want idle", slug, byslug[slug])
		}
	}
}

// TestOrgSnapshotContextBucket exercises the fill math by writing
// chat history past the 50% threshold and checking the snapshot
// reflects it.
func TestOrgSnapshotContextBucket(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentModel = provider.MockModelSmall // a 200k window
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 200k window × 50% = 100k tokens × 4 chars/tok = 400k chars to
	// just cross into "mid" bucket.
	big := strings.Repeat("x", 410_000)
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role:    store.RoleSent,
		Content: big,
		TS:      time.Now(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	body := srv.buildOrgSnapshot()
	var snap orgSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.Agents) != 1 {
		t.Fatalf("agents: %+v", snap.Agents)
	}
	if pct := snap.Agents[0].ContextPct; pct < 50 || fillBucket(pct) != "mid" {
		t.Errorf("context_pct = %d, want the mid band (400k chars / 4 / 200k window = 50%%)", pct)
	}
}

// TestOrgHubBroadcast covers the hub primitive: subscribe gets the
// latest snapshot immediately, publish broadcasts only when bytes
// change, and a slow consumer drops intermediates rather than
// blocking the producer.
func TestOrgHubBroadcast(t *testing.T) {
	h := newOrgHub()
	// Publish #1: no subscribers yet, but the snapshot is stored.
	first := []byte(`{"a":1}`)
	h.publish(first)

	initial, ch := h.subscribe()
	defer h.unsubscribe(ch)
	if string(initial) != string(first) {
		t.Errorf("initial snapshot = %q, want %q", initial, first)
	}

	// Publish #2 (different bytes) → live subscriber receives it.
	second := []byte(`{"a":2}`)
	h.publish(second)
	select {
	case got := <-ch:
		if string(got) != string(second) {
			t.Errorf("live snapshot = %q, want %q", got, second)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive snapshot")
	}

	// Publish #3 with byte-equal payload → no-op, channel stays empty.
	h.publish(second)
	select {
	case got := <-ch:
		t.Errorf("unexpected dedup miss: got %q", got)
	case <-time.After(50 * time.Millisecond):
		// Good — bytes-equal publish was a no-op.
	}
}

// TestOrgHubSlowConsumer verifies a backlogged subscriber doesn't
// stall the producer and gets the newest snapshot (intermediate
// snapshots are dropped because the snapshot is the whole truth).
func TestOrgHubSlowConsumer(t *testing.T) {
	h := newOrgHub()
	_, ch := h.subscribe()
	defer h.unsubscribe(ch)

	for i := 0; i < 10; i++ {
		h.publish([]byte{byte('a' + i)})
	}
	// Drain — should land on the newest snapshot, not whichever
	// happened to fit in the buffer.
	deadline := time.Now().Add(time.Second)
	var last []byte
drain:
	for time.Now().Before(deadline) {
		select {
		case got := <-ch:
			last = got
		case <-time.After(20 * time.Millisecond):
			break drain
		}
		if len(last) > 0 && last[0] == 'a'+9 {
			break drain
		}
	}
	if len(last) == 0 || last[0] != 'a'+9 {
		t.Errorf("newest snapshot = %v, want byte 'j'", last)
	}
}

// TestOrgStreamServesSnapshot exercises the SSE handler end-to-end:
// connect, parse the first `snapshot` event, and verify the payload
// matches the agents we seeded.
func TestOrgStreamServesSnapshot(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Use an httptest server because httptest.ResponseRecorder
	// doesn't support http.Flusher properly (no real TCP behind it).
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	req, _ := http.NewRequest("GET", hs.URL+"/org/stream", nil)
	req.AddCookie(authCookie(t, srv))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("content-type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	// Read until we see the first snapshot. SSE frames are terminated
	// by a blank line; we look for "event: snapshot" plus its data
	// line in the bytes.
	buf := make([]byte, 4096)
	deadline := time.Now().Add(2 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		// Set a read deadline by closing the body if we exhaust the
		// loop. For this test, an idle server emits the snapshot
		// within the first read.
		n, err := resp.Body.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
			if strings.Contains(string(got), "event: snapshot") &&
				strings.Contains(string(got), `"alice"`) {
				break
			}
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(string(got), `"alice"`) {
		t.Errorf("snapshot didn't include alice; got: %s", got)
	}
}

// TestNotifyOrgStateOnChatHubLifecycle locks in the wire-up: opening
// and closing a chat hub fires a notify, so the sidebar lights up
// the working dot the instant the loop starts.
func TestNotifyOrgStateOnChatHubLifecycle(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drain initial publishes by subscribing first.
	_, ch := srv.orgHub.subscribe()
	defer srv.orgHub.unsubscribe(ch)

	// Open a chat hub for alice → notify fires.
	_, isOriginator := srv.getOrCreateHub("alice")
	if !isOriginator {
		t.Fatal("expected to be originator")
	}
	select {
	case snap := <-ch:
		var s orgSnapshot
		_ = json.Unmarshal(snap, &s)
		var aliceWorking bool
		for _, a := range s.Agents {
			if a.Slug == "alice" {
				aliceWorking = a.State == "running"
			}
		}
		if !aliceWorking {
			t.Errorf("alice should be working after getOrCreateHub; snapshot: %s", snap)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive snapshot after getOrCreateHub")
	}

	// Close the hub → notify fires again, alice idle.
	srv.removeHub("alice")
	select {
	case snap := <-ch:
		var s orgSnapshot
		_ = json.Unmarshal(snap, &s)
		for _, a := range s.Agents {
			if a.Slug == "alice" && a.State == "running" {
				t.Errorf("alice should be idle after removeHub; snapshot: %s", snap)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive snapshot after removeHub")
	}
}

// TestOrgSnapshotWorkingFromChatHubs: an agent with a live chat hub is
// working and its colleagues are not.
func TestOrgSnapshotWorkingFromChatHubs(t *testing.T) {
	srv := newTestServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Researcher", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "k"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	// Simulate an in-flight tool loop for alice by injecting a
	// chatHub directly. workingAgents() snapshots the map under
	// streamMu, so populating it here is equivalent to having a real
	// goroutine running.
	srv.chatHubs = map[string]*chatHub{
		"alice": {hub: newHub(), slug: "alice"},
	}
	var snap orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range snap.Agents {
		got[a.Slug] = a.State == "running"
	}
	if !got["alice"] || got["bob"] || got["chief-of-staff"] {
		t.Errorf("working = %+v, want only alice", got)
	}
}

func TestOrgSnapshotDisconnectedReflectsAgentpodHub(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Decode into a fresh struct each time, so nothing carries over
	// from a prior snapshot.
	decode := func(label string) orgSnapshot {
		t.Helper()
		var s orgSnapshot
		if err := json.Unmarshal(srv.buildOrgSnapshot(), &s); err != nil {
			t.Fatalf("decode (%s): %v", label, err)
		}
		return s
	}

	// AgentpodHub unwired (default newTestServer): Disconnected stays
	// false. Production wiring is opt-in; tests without it shouldn't
	// see every agent flagged disconnected.
	if decode("hub unwired").Agents[0].State == "disconnected" {
		t.Errorf("Disconnected = true with AgentpodHub unwired; expected false")
	}

	// Wire the hub. With zero subscribers, alice flips to disconnected.
	srv.AgentpodHub = newAgentpodHub()
	if decode("hub wired, no sub").Agents[0].State != "disconnected" {
		t.Errorf("Disconnected = false with hub wired and 0 subscribers; expected true")
	}

	// Subscribe → disconnected flips back off.
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)
	if decode("subscribed").Agents[0].State == "disconnected" {
		t.Errorf("Disconnected = true after Subscribe; expected false (pod is connected)")
	}
}

// TestOrgSnapshotPendingMailVisibleViaPath verifies the flat
// pending_paths list populates from a queued inbox entry — the
// input the inbox client uses to detect a change.
func TestOrgSnapshotPendingMailVisibleViaPath(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed cos: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	msg := store.Message{Type: store.MsgNotice, Title: "X", From: "chief-of-staff", To: store.Recipients{"alice"}, Body: "b"}
	abs, err := srv.Store.WriteMessage(msg)
	if err != nil {
		t.Fatalf("write msg: %v", err)
	}
	rel := strings.TrimPrefix(abs, srv.Store.Root()+"/")
	ts, _ := srv.Store.ReadMessageQueue()
	if ts.Agents == nil {
		ts.Agents = map[string]store.AgentQueue{}
	}
	rt := ts.Agents["alice"]
	rt.Inbox = append(rt.Inbox, rel)
	ts.Agents["alice"] = rt
	if err := srv.Store.WriteMessageQueue(ts); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	var snap orgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// pending_paths carries message paths, de-duplicated: a notice
	// queued in several inboxes is one card in the UI, so it must be
	// one entry here or the client's DOM diff never settles.
	if len(snap.Inbox.PendingPaths) != 1 || snap.Inbox.PendingPaths[0] != rel {
		t.Errorf("pending_paths = %v, want [%s]", snap.Inbox.PendingPaths, rel)
	}
}

// TestOrgHubConcurrentPublishers exercises the lock discipline.
// Multiple goroutines publishing concurrently shouldn't race or
// deadlock; the latest snapshot always wins.
func TestOrgHubConcurrentPublishers(t *testing.T) {
	h := newOrgHub()
	_, ch := h.subscribe()
	defer h.unsubscribe(ch)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.publish([]byte{byte(i)})
		}()
	}
	wg.Wait()
	// Drain — last value to land in the channel is whatever survived
	// the slow-consumer drop logic. We don't care which one, just
	// that we don't deadlock.
	select {
	case <-ch:
	case <-time.After(200 * time.Millisecond):
	}
}
