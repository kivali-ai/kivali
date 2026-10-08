package web

import (
	"sync"
	"time"
)

// chatHubLingerDuration is how long a completed chatHub stays in
// Server.chatHubs before its eviction timer fires. The slot is held
// only so getOrCreateHub can identity-check (and evict) it when a
// fresh spawn comes in during the window — it is NOT used to feed
// terminal events to late /stream subscribers. handleAgentStream
// explicitly skips completed hubs so a freshly-reloaded page (e.g.
// post-rotation_done) cannot pick up the dying hub's replay buffer
// and re-fire the terminal events that triggered its reload, which
// would loop indefinitely until the timer expired.
const chatHubLingerDuration = 15 * time.Second

// chatHubQuarantineThreshold is the consecutive-runtime-disruption
// count at which spawnChatLoopIfIdle stops auto-respawning. The
// scenario: agent OOMs mid-turn, boot scan writes a runtime-disruption
// entry and respawns, agent OOMs again, repeat. After this many
// consecutive disruptions without any productive entry between them,
// something is wrong (memory leak, poison content, broken downstream)
// and we hold off rather than burn cycles. The CEO clears it by
// engaging via direct chat (any non-system, non-tool entry resets the
// count via store.ConsecutiveRuntimeDisruptions).
const chatHubQuarantineThreshold = 3

// chatHub is the per-agent wrapper around hub for in-flight direct-chat
// tool loops. The broadcast / subscribe / close / cancel / done / emit
// machinery comes from the embedded *hub; this file adds the two chat-
// specific replay-buffer prunes (checkpoint, trimDeltas) that keep
// mid-flight refreshes from painting duplicate bubbles.
//
// Lifecycle: Server.getOrCreateHub creates the chatHub and marks the
// caller as originator. The originator runs the tool loop in a
// detached goroutine (so client disconnect doesn't kill the work),
// emitting events via hub.broadcast. When the tool loop exits, the
// originator (a) flushes pending deliveries to disk, (b) marks the
// hub completed, (c) calls hub.close (signalling done to existing
// subscribers), and (d) schedules eviction from chatHubs after
// chatHubLingerDuration as a backstop so getOrCreateHub can identity-
// check (and evict) the slot when a fresh spawn comes in. The slot is
// NOT used to serve replay to new /stream connections — handleAgentStream
// skips completed hubs to avoid re-firing terminal events on a page
// that just reloaded in response to one. New wakes during the linger
// window evict the completed hub via getOrCreateHub and start fresh.
type chatHub struct {
	*hub
	slug string

	// spawnReceivedTS is the timestamp of the most recent "real"
	// (non-tool-plumbing) received chat entry at the moment this hub
	// was claimed by the originator. The post-completion defer in
	// spawnChatLoopIfIdle compares the latest received TS against
	// this: if a NEWER received entry slipped in while the loop was
	// streaming (e.g. CEO's approval was instant-delivered mid-
	// inference), a follow-up loop fires to respond to it. Equal (or
	// zero) means "nothing new during the loop", and spinning in
	// empty-stream tests / error paths is avoided.
	spawnReceivedTS time.Time

	// spawnSource records what triggered this loop: "release" (a
	// CEO-paced messaging primitive — Release-all, single Release,
	// Bounce, CEO-reply) vs. "chat" (a direct CEO message into the
	// agent's chat). Becomes the turn's source: the usage row's purpose
	// and the source of any follow-up loop.
	spawnSource string

	// completed flips true when the post-loop defer has finished
	// flushing pending deliveries and called hub.close(). While this
	// is true the hub stays in Server.chatHubs to allow late
	// subscribers to replay buffered events, but ChatHubActive,
	// deliverToAgent's busy check, and getOrCreateHub all treat the
	// slug as idle so new work isn't blocked. Guarded by hub.mu.
	completed bool

	// turnEnded flips true when the turn has sent its last word to the
	// page: a `done` or `error` that no rotation_done follows. The page
	// answers either by closing the stream and fetching the chat, so
	// from then on the hub is finished as far as the page is concerned,
	// although finalizeAgentpodTurn still has work to do (the graph
	// pass, the flush) before it marks the hub completed. Were the page
	// still told the agent is running in that window, it would reopen
	// the stream, have the `done` replayed, fetch again, and go round
	// until the hub completed. Guarded by hub.mu.
	turnEnded bool

	// interruptRequested flips true when handleAgentStop is called
	// against this hub. Used as a state indicator (not a write
	// trigger) — the user-interruption marker is written
	// synchronously by handleAgentStop at click time, not deferred.
	// The flag is consulted by:
	//   - requestInterrupt: gates the synchronous marker write so
	//     repeat clicks don't write duplicates.
	//   - peekInterruptRequested: lets the disconnect-grace synthetic
	//     failure path classify a missing terminator as cancelled
	//     vs. transport-failure.
	// Guarded by hub.mu.
	interruptRequested bool

	// ordered holds events queued while streamMu was held — the
	// pending_* events and the paused-to-deliver marker — in the order
	// of the buffer changes they report. flushOrdered broadcasts them
	// after streamMu is released: broadcast can block on a slow
	// subscriber, and holding streamMu across it would stall every
	// delivery and hub claim for every agent. Guarded by hub.mu.
	ordered []queuedEvent
	// orderedMu serialises flushOrdered so two drainers cannot
	// interleave their batches out of queue order.
	orderedMu sync.Mutex
}

// queuedEvent is one event waiting in chatHub.ordered.
type queuedEvent struct {
	kind    string
	payload any
}

// queueOrdered appends an event for flushOrdered to broadcast. Called
// with streamMu held, so queue order is buffer-change order. nil-safe.
func (h *chatHub) queueOrdered(kind string, payload any) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.ordered = append(h.ordered, queuedEvent{kind: kind, payload: payload})
	h.mu.Unlock()
}

// flushOrdered broadcasts every queued event in queue order. Call it
// after releasing streamMu. When it returns, every event queued before
// the call has been broadcast — by this caller or by a concurrent one
// it waited for — so an event emitted directly afterwards (the
// chat_message after a pending_delivered) is always later on the wire.
// nil-safe.
func (h *chatHub) flushOrdered() {
	if h == nil {
		return
	}
	h.orderedMu.Lock()
	defer h.orderedMu.Unlock()
	for {
		h.mu.Lock()
		q := h.ordered
		h.ordered = nil
		h.mu.Unlock()
		if len(q) == 0 {
			return
		}
		for _, ev := range q {
			h.Emit(ev.kind, ev.payload)
		}
	}
}

// requestInterrupt records a Stop click against this hub. Returns
// firstRequest=true on the first click (caller writes the marker
// and dispatches cancel); false on repeat clicks (caller still
// dispatches cancel — to re-arm against whatever's currently
// in-flight — but skips the marker write to avoid duplicates).
// alreadyCompleted=true when the hub has already finished its loop
// (caller returns 204; nothing to stop).
func (h *chatHub) requestInterrupt() (firstRequest bool, alreadyCompleted bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.completed {
		return false, true
	}
	if h.interruptRequested {
		return false, false
	}
	h.interruptRequested = true
	return true, false
}

// peekInterruptRequested reads the flag without clearing it. Used
// by the disconnect-grace synthetic-failure path to decide whether
// a no-terminator-from-runtime should surface as cancelled (Stop
// was clicked) or as an error.
func (h *chatHub) peekInterruptRequested() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.interruptRequested
}

// markCompleted flips the completed flag under hub.mu. Reads via
// isCompleted are also under hub.mu so the flag's visibility is
// consistent with the events log subscribers see during replay.
func (h *chatHub) markCompleted() {
	h.mu.Lock()
	h.completed = true
	h.mu.Unlock()
}

// isCompleted reports whether markCompleted has been called.
func (h *chatHub) isCompleted() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.completed
}

// markTurnEnded records that the turn's terminal event went out.
func (h *chatHub) markTurnEnded() {
	h.mu.Lock()
	h.turnEnded = true
	h.mu.Unlock()
}

// liveForPage reports whether the page should follow this hub: its
// turn is still going, so a stream attaches to it and the chat reads
// running. Neither holds once the terminal event went out
// (markTurnEnded) or the hub completed. Everything that coordinates
// work (ChatHubActive, getOrCreateHub, deliverOrStage) keeps asking
// isCompleted: a message that arrives while the turn finalizes is
// still staged, and the finalize flushes it.
func (h *chatHub) liveForPage() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.completed && !h.turnEnded
}

// Checkpoint clears the replay buffer. Called by the originator at
// safe boundaries — after an iter's assistant text + tool_uses +
// tool_results have been fully persisted to chat.jsonl — so that new
// subscribers (refresh mid-flight) don't receive replay of events
// whose visible effect is already in the chat API's rows.
//
// Live subscribers already consumed these events via broadcast; they
// are unaffected.
//
// Capitalized to satisfy agent.StreamEmitter (the chat-path runner
// lives in internal/agent now).
func (h *chatHub) Checkpoint() {
	h.mu.Lock()
	h.events = h.events[:0]
	h.mu.Unlock()
}

// TrimDeltas removes buffered "delta" events from the replay log.
// Called by the originator immediately after persisting an assistant
// text message to chat.jsonl — from that point on, the text reaches
// any new subscriber via the chat API's rows, so replaying its
// deltas would produce a duplicate bubble with a fresh client-side
// timestamp layered on top of the persisted original. Tool events
// (tool_use_start, tool_use, tool_result, doc_published) remain in
// the buffer so in-flight chip state still replays correctly.
func (h *chatHub) TrimDeltas() {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := make([]hubEvent, 0, len(h.events))
	for _, ev := range h.events {
		if ev.Kind == "delta" {
			continue
		}
		kept = append(kept, ev)
	}
	h.events = kept
}

// getOrCreateHub atomically claims a chatHub for slug or returns the
// existing one. isOriginator is true when this caller created the
// hub (and is responsible for running the tool loop + calling
// hub.close on completion).
//
// Lingering hubs (loop already finished, hanging around in chatHubs
// for late SSE subscribers to replay events from) are evicted on
// claim: the new caller gets a fresh hub as originator, and the old
// hub's pending eviction timer becomes a no-op. Existing subscribers
// of the lingering hub keep their channels — hub.close() already
// fired, so they will exit cleanly on their next read; they don't
// follow the new hub.
func (s *Server) getOrCreateHub(slug string) (h *chatHub, isOriginator bool) {
	s.streamMu.Lock()
	if s.chatHubs == nil {
		s.chatHubs = map[string]*chatHub{}
	}
	if existing, ok := s.chatHubs[slug]; ok {
		if !existing.isCompleted() {
			s.streamMu.Unlock()
			return existing, false
		}
		// Lingering hub — evict so the new spawn gets a clean slate.
		delete(s.chatHubs, slug)
	}
	ch := &chatHub{hub: newHub(), slug: slug}
	s.chatHubs[slug] = ch
	s.streamMu.Unlock()
	// Originator-only notify: a hub already-existed claim doesn't
	// change the org snapshot. Outside the lock so a slow notify
	// doesn't stall concurrent claims.
	s.NotifyOrgState()
	return ch, true
}

// removeHub drops slug's entry so the next request starts a fresh
// chatHub. Safe to call multiple times.
func (s *Server) removeHub(slug string) {
	s.streamMu.Lock()
	_, existed := s.chatHubs[slug]
	if s.chatHubs != nil {
		delete(s.chatHubs, slug)
	}
	s.streamMu.Unlock()
	if existed {
		s.NotifyOrgState()
	}
}

// evictHubIfMatches removes slug's entry from chatHubs only if it is
// still the same hub object passed in. Used by the post-loop linger
// timer: if a fresh spawn already evicted us (via getOrCreateHub) and
// installed a new hub, the timer must not clobber that new hub.
func (s *Server) evictHubIfMatches(slug string, h *chatHub) {
	s.streamMu.Lock()
	cur, ok := s.chatHubs[slug]
	if ok && cur == h {
		delete(s.chatHubs, slug)
	}
	s.streamMu.Unlock()
}
