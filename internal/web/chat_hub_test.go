package web

import (
	"sync"
	"testing"
	"time"
)

// newTestHub returns a fresh chatHub wired up the same way
// getOrCreateHub does, for use in unit tests.
func newTestHub() *chatHub {
	return &chatHub{hub: newHub(), slug: "t"}
}

// drainReplay counts what subscribe() returned as its snapshot —
// helpful for asserting checkpoint / trimDeltas semantics without
// touching the live channel.
func drainReplay(hub *chatHub) []hubEvent {
	replay, _, cancel := hub.subscribe()
	close(cancel)
	return replay
}

// TestHubBroadcastAndSubscribe verifies that events appended via
// broadcast end up in the replay buffer and get delivered live to
// existing subscribers.
func TestHubBroadcastAndSubscribe(t *testing.T) {
	hub := newTestHub()
	_, live, cancel := hub.subscribe()
	defer close(cancel)

	hub.broadcast("delta", map[string]string{"text": "a"})
	hub.broadcast("delta", map[string]string{"text": "b"})

	// Live subscriber should receive both events in order.
	for i, want := range []string{"a", "b"} {
		select {
		case ev := <-live:
			if ev.Kind != "delta" {
				t.Errorf("event %d kind = %q, want delta", i, ev.Kind)
			}
			if want == "a" && string(ev.Payload) == "" {
				t.Errorf("event %d payload empty", i)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("event %d: timed out waiting for live delivery", i)
		}
	}
}

// TestHubSubscribeGetsReplaySnapshot confirms that a subscriber who
// joins after events have already been broadcast gets a full replay.
// This is the "refresh mid-stream" path.
func TestHubSubscribeGetsReplaySnapshot(t *testing.T) {
	hub := newTestHub()
	hub.broadcast("delta", map[string]string{"text": "1"})
	hub.broadcast("tool_use_start", map[string]any{"tool_use_id": "x"})
	hub.broadcast("delta", map[string]string{"text": "2"})

	replay, _, cancel := hub.subscribe()
	defer close(cancel)

	if len(replay) != 3 {
		t.Fatalf("replay = %d events, want 3", len(replay))
	}
	kinds := []string{replay[0].Kind, replay[1].Kind, replay[2].Kind}
	want := []string{"delta", "tool_use_start", "delta"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("replay[%d].Kind = %q, want %q", i, kinds[i], want[i])
		}
	}
}

// TestHubCheckpointEmptiesReplay covers the "post-iteration trim"
// path that prevents duplicate bubbles on refresh: after
// checkpoint(), new subscribers should see no replay.
func TestHubCheckpointEmptiesReplay(t *testing.T) {
	hub := newTestHub()
	hub.broadcast("delta", map[string]string{"text": "a"})
	hub.broadcast("tool_use_start", map[string]any{"id": "x"})
	hub.broadcast("tool_result", map[string]any{"id": "x"})

	if got := len(drainReplay(hub)); got != 3 {
		t.Fatalf("pre-checkpoint replay len = %d, want 3", got)
	}
	hub.Checkpoint()
	if got := len(drainReplay(hub)); got != 0 {
		t.Errorf("post-checkpoint replay len = %d, want 0", got)
	}
	// New events after checkpoint land normally in the fresh buffer.
	hub.broadcast("delta", map[string]string{"text": "b"})
	if got := len(drainReplay(hub)); got != 1 {
		t.Errorf("post-checkpoint new-event replay len = %d, want 1", got)
	}
}

// TestHubTrimDeltasKeepsToolEvents verifies the selective trim that
// runs after every assistant-text persist. Delta events must drop
// (they'd re-create a duplicate bubble on refresh) while tool events
// stay so in-flight chip state still replays correctly.
func TestHubTrimDeltasKeepsToolEvents(t *testing.T) {
	hub := newTestHub()
	hub.broadcast("delta", map[string]string{"text": "part1"})
	hub.broadcast("tool_use_start", map[string]any{"id": "a"})
	hub.broadcast("delta", map[string]string{"text": "part2"})
	hub.broadcast("tool_use", map[string]any{"id": "a"})
	hub.broadcast("tool_result", map[string]any{"id": "a"})

	hub.TrimDeltas()
	replay := drainReplay(hub)
	if len(replay) != 3 {
		t.Fatalf("post-trimDeltas replay len = %d, want 3 (tool events only)", len(replay))
	}
	for _, ev := range replay {
		if ev.Kind == "delta" {
			t.Errorf("delta event survived trimDeltas: %+v", ev)
		}
	}
	wantKinds := []string{"tool_use_start", "tool_use", "tool_result"}
	for i, k := range wantKinds {
		if replay[i].Kind != k {
			t.Errorf("replay[%d].Kind = %q, want %q", i, replay[i].Kind, k)
		}
	}
}

// TestHubCloseClosesLiveChannelsAndDoneSignal verifies that close
// unblocks all subscribers and is idempotent.
func TestHubCloseClosesLiveChannelsAndDoneSignal(t *testing.T) {
	hub := newTestHub()
	_, live1, cancel1 := hub.subscribe()
	defer close(cancel1)
	_, live2, cancel2 := hub.subscribe()
	defer close(cancel2)

	hub.close()

	// Both subscriber channels should be closed.
	select {
	case _, ok := <-live1:
		if ok {
			t.Error("live1 delivered an event after close (expected channel close)")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("live1 channel not closed within 100ms of hub.close")
	}
	select {
	case _, ok := <-live2:
		if ok {
			t.Error("live2 delivered an event after close")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("live2 channel not closed within 100ms of hub.close")
	}

	// done signal should also fire.
	select {
	case <-hub.done:
	case <-time.After(100 * time.Millisecond):
		t.Error("hub.done not closed")
	}

	// Second close call is a no-op (idempotent).
	hub.close() // should not panic
}

// TestHubBroadcastAfterCloseIsNoop — once the hub is closed no more
// events get buffered, and it's safe to call broadcast.
func TestHubBroadcastAfterCloseIsNoop(t *testing.T) {
	hub := newTestHub()
	hub.broadcast("delta", map[string]string{"text": "pre-close"})
	hub.close()
	hub.broadcast("delta", map[string]string{"text": "post-close"}) // should not panic

	// The replay buffer should still reflect what was broadcast
	// before close (we don't require post-close events to be
	// captured, just that the call is safe).
	replay := drainReplay(hub)
	for _, ev := range replay {
		// If the implementation decided to drop or retain the
		// pre-close event is fine; the invariant is "no post-close
		// event was silently appended."
		if string(ev.Payload) == `{"text":"post-close"}` {
			t.Error("post-close event got appended to replay buffer")
		}
	}
}

// TestGetOrCreateHubEvictsLingeringHub locks in the linger contract:
// when a chatHub finishes its loop and is marked completed, it stays
// in chatHubs so late SSE subscribers can replay buffered events,
// but a new wake (next CEO message, follow-up loop, rotation, etc.)
// must claim a fresh hub — not subscribe to the dead one as a
// non-originator. Otherwise the new wake would silently no-op
// because the originator branch is the one that runs RunAgent.
func TestGetOrCreateHubEvictsLingeringHub(t *testing.T) {
	srv := newTestServer(t)

	first, isOriginator := srv.getOrCreateHub("alice")
	if !isOriginator {
		t.Fatal("first claim should be originator")
	}

	// Simulate the post-loop defer: flush done, hub closed, hub
	// marked completed but still in chatHubs awaiting eviction.
	first.markCompleted()
	first.close()

	second, isOriginator := srv.getOrCreateHub("alice")
	if !isOriginator {
		t.Fatal("claim during linger should evict the completed hub and return originator=true")
	}
	if second == first {
		t.Fatal("getOrCreateHub returned the lingering hub instead of a fresh one")
	}
	if second.isCompleted() {
		t.Error("fresh hub should not be marked completed")
	}
}

// TestEvictHubIfMatchesIsIdentityChecked covers the linger-eviction
// timer's safety net: if a fresh spawn already replaced the
// lingering hub before the timer fired, the timer must not clobber
// the new hub.
func TestEvictHubIfMatchesIsIdentityChecked(t *testing.T) {
	srv := newTestServer(t)

	old, _ := srv.getOrCreateHub("alice")
	old.markCompleted()
	old.close()

	// Fresh spawn evicts old, claims new.
	newHub, _ := srv.getOrCreateHub("alice")
	if newHub == old {
		t.Fatal("setup: fresh spawn didn't evict the lingering hub")
	}

	// Old hub's linger timer fires. It must not delete the new hub.
	srv.evictHubIfMatches("alice", old)

	srv.streamMu.Lock()
	cur := srv.chatHubs["alice"]
	srv.streamMu.Unlock()
	if cur != newHub {
		t.Errorf("evictHubIfMatches clobbered the wrong hub: got %p, want %p", cur, newHub)
	}
}

// TestHubConcurrentBroadcastAndSubscribe is a basic sanity check
// that concurrent broadcasts and subscriptions don't race. Run with
// -race to catch mutex misuse.
func TestHubConcurrentBroadcastAndSubscribe(t *testing.T) {
	hub := newTestHub()
	defer hub.close()

	const n = 100
	var wg sync.WaitGroup

	// Producers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				hub.broadcast("delta", map[string]string{"text": "x"})
			}
		}()
	}
	// Late subscribers interleaved with producers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n/4; j++ {
				replay, _, cancel := hub.subscribe()
				close(cancel)
				_ = replay
			}
		}()
	}
	wg.Wait()
}
