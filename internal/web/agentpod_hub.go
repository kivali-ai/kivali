package web

import (
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
)

// podStartGrace is how long after a pod was provisioned its agent counts
// as starting rather than disconnected while no subscriber has dialed
// in: scheduling, start-up and the events SSE take seconds, and a fresh
// hire showing "not connected" in that window reads as a fault. As long
// as SpawnRecovered's wait (spawnRecoveredSubscribeTimeout).
const podStartGrace = 2 * time.Minute

// agentpodHub fans agentpod events from publishers (web's chat
// dispatcher, cache invalidator, etc.) out to one-or-more subscribers
// per agent slug. Each subscriber is the Server-side end of an
// agent runtime's events SSE stream.
//
// In steady state each slug has exactly one subscriber (one agent
// Pod per agent). During an agent-Pod restart, briefly there can be
// two — the dropping old connection and the freshly-connected new
// one. Both get any events published in that window; idempotency on
// the agent-runtime side handles double-delivery.
//
// Subscribers are buffered. A slow subscriber whose buffer fills is
// dropped — the agent runtime reconnects with backoff and re-warms
// from core (cache + state are non-authoritative anyway), so dropping
// is recoverable. We do NOT block publishers on a slow subscriber:
// a stuck agent must not jam the chat-turn dispatch path for the
// whole org.
type agentpodHub struct {
	mu   sync.Mutex
	subs map[string]map[*agentpodSubscriber]struct{}
	// starting holds when each slug's pod was provisioned, until its
	// first subscriber dials in (Starting).
	starting map[string]time.Time
	// cancelGrace stops each slug's grace timer: closed when a
	// subscriber dials in or the slug is marked starting again.
	cancelGrace map[string]chan struct{}
	// graceClock and onGraceExpired are set by SetGraceExpiry: when a
	// slug's grace runs out with no subscriber, onGraceExpired runs so
	// screens move from starting to disconnected. Unset, no timer runs.
	graceClock     clock.Clock
	onGraceExpired func()
}

// agentpodSubscriber is one connected agent-runtime SSE consumer.
// The hub writes events on Ch; the SSE handler reads them and
// frames them on the wire.
type agentpodSubscriber struct {
	slug string
	ch   chan agentpod.Event
	// closed is closed by the hub when it drops the subscriber for
	// being slow; the SSE handler reads it to know "stop trying."
	closed chan struct{}
}

// newAgentpodHub returns an empty hub. Internal constructor used by
// tests; main.go calls NewAgentpodHub instead so the type can stay
// package-private while still being constructable cross-package.
func newAgentpodHub() *agentpodHub {
	return &agentpodHub{subs: map[string]map[*agentpodSubscriber]struct{}{}, starting: map[string]time.Time{}, cancelGrace: map[string]chan struct{}{}}
}

// SetGraceExpiry arms a timer on c for every MarkStarting: when the
// slug's podStartGrace runs out with no subscriber, expired runs (the
// server's NotifyOrgState), so the agent's state is recomputed as
// disconnected instead of waiting for some unrelated change.
func (h *agentpodHub) SetGraceExpiry(c clock.Clock, expired func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.graceClock, h.onGraceExpired = c, expired
}

// MarkStarting records that slug's pod was just provisioned: until a
// subscriber dials in, or podStartGrace passes, the agent is starting.
func (h *agentpodHub) MarkStarting(slug string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starting[slug] = now
	h.stopGraceLocked(slug)
	if h.graceClock == nil || h.onGraceExpired == nil {
		return
	}
	cancel := make(chan struct{})
	h.cancelGrace[slug] = cancel
	timer := h.graceClock.NewTimer(podStartGrace - h.graceClock.Now().Sub(now))
	expired := h.onGraceExpired
	go func() {
		defer timer.Stop()
		select {
		case <-timer.C():
		case <-cancel:
			return
		}
		h.mu.Lock()
		current := h.cancelGrace[slug] == cancel
		if current {
			delete(h.cancelGrace, slug)
		}
		h.mu.Unlock()
		if current {
			expired()
		}
	}()
}

// stopGraceLocked cancels slug's grace timer, if one runs. h.mu held.
func (h *agentpodHub) stopGraceLocked(slug string) {
	if c, ok := h.cancelGrace[slug]; ok {
		close(c)
		delete(h.cancelGrace, slug)
	}
}

// Starting reports whether slug's pod was provisioned less than
// podStartGrace ago and no subscriber has dialed in since.
func (h *agentpodHub) Starting(slug string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.starting[slug]
	return ok && len(h.subs[slug]) == 0 && now.Sub(t) < podStartGrace
}

// NewAgentpodHub is the cross-package constructor for the agent-pod
// events hub. Returns the package-private type — main.go can store
// it in a typed variable and pass it to Server.AgentpodHub without
// needing the type name spelled out, which lets the implementation
// stay unexported.
func NewAgentpodHub() *agentpodHub { //nolint:revive // unexported return type is intentional; see doc comment
	return newAgentpodHub()
}

// Subscribe registers a new subscriber for slug and returns it.
// The buffer cap (16) is sized to absorb burst publishes during a
// reconnect window without dropping; sustained backlog → drop.
func (h *agentpodHub) Subscribe(slug string) *agentpodSubscriber {
	sub := &agentpodSubscriber{
		slug:   slug,
		ch:     make(chan agentpod.Event, 16),
		closed: make(chan struct{}),
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[slug] == nil {
		h.subs[slug] = map[*agentpodSubscriber]struct{}{}
	}
	h.subs[slug][sub] = struct{}{}
	delete(h.starting, slug)
	h.stopGraceLocked(slug)
	return sub
}

// Unsubscribe drops sub from the hub. Safe to call multiple times.
// The subscriber's channel is NOT closed here — closeSubscriber
// (only the hub or the handler that owns the subscriber should
// close) handles it.
func (h *agentpodHub) Unsubscribe(sub *agentpodSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.subs[sub.slug]; ok {
		delete(set, sub)
		if len(set) == 0 {
			delete(h.subs, sub.slug)
		}
	}
	// The closed signal is fired by the publisher path on slow-drop;
	// here Unsubscribe is the clean exit case (handler returning),
	// so we just remove from the index.
}

// Publish delivers ev to every subscriber for slug. Slow subscribers
// (full buffer) are dropped from the hub: their `closed` channel is
// fired and they're removed from the index. The publisher does not
// block.
func (h *agentpodHub) Publish(slug string, ev agentpod.Event) {
	h.mu.Lock()
	subs := make([]*agentpodSubscriber, 0, len(h.subs[slug]))
	for sub := range h.subs[slug] {
		subs = append(subs, sub)
	}
	h.mu.Unlock()
	for _, sub := range subs {
		select {
		case sub.ch <- ev:
		default:
			// Slow subscriber — drop. Mark closed and remove.
			h.dropLocked(sub)
		}
	}
}

// dropLocked closes a subscriber's channels and removes it from the
// hub. Acquires h.mu internally so the publish path can call it
// without holding the lock during the channel send.
func (h *agentpodHub) dropLocked(sub *agentpodSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.subs[sub.slug]; ok {
		if _, present := set[sub]; present {
			delete(set, sub)
			if len(set) == 0 {
				delete(h.subs, sub.slug)
			}
			// Fire-once: close the closed channel exactly once.
			select {
			case <-sub.closed:
			default:
				close(sub.closed)
			}
		}
	}
}

// SubscriberCount reports the number of live subscribers for slug.
// Mainly for tests + observability.
func (h *agentpodHub) SubscriberCount(slug string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[slug])
}
