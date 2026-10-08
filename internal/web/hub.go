package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// hub is the generic broadcast bus underneath chatHub and the
// singleton release hub. Exactly one producer goroutine appends events
// via broadcast; any number of HTTP subscribers consume them — first
// receiving a snapshot of the replay log (so a late/refreshed tab
// sees everything that happened before it arrived), then live events
// until the producer calls close.
//
// It intentionally knows nothing about agents, releases, or SSE wire
// format: those live in chat_hub.go (chatHub wraps *hub to add chat-
// specific replay-buffer pruning) and in handleReleaseAll (which
// sets a singleton on the Server). The only reason this is package-
// private rather than its own sub-package is that the SSE serving
// helper, serveHubSSE, lives in the same file and they're tightly
// coupled.
type hub struct {
	mu     sync.Mutex
	events []hubEvent // ordered log; new subscribers replay these first
	subs   []*hubSub  // live subscribers
	closed bool

	done chan struct{} // closed by close()
}

// hubEvent is one broadcast entry: a named event with a pre-marshaled
// JSON payload.
type hubEvent struct {
	Kind    string
	Payload []byte
}

type hubSub struct {
	ch     chan hubEvent
	cancel chan struct{}
}

// newHub returns an initialized hub ready for broadcast.
func newHub() *hub {
	return &hub{done: make(chan struct{})}
}

// hubMaxEvents caps the per-hub replay log. Without this cap, a
// long-running agent that emits a steady stream of tool events
// during a single chat would grow h.events unboundedly while the
// hub was alive (deltas get trimmed via TrimDeltas, but tool_use /
// tool_result / done frames stay). 1024 is generous for any normal
// chat — ~200 tool calls' worth of frames — and keeps a single
// runaway agent from pinning megabytes of payload bytes in memory.
// On overflow, the oldest events are dropped; late subscribers may
// miss the very first events of a long chat but still see recent
// state.
const hubMaxEvents = 1024

// broadcast appends an event to the log and fans out to live
// subscribers. Slow consumers (channel full) don't stall the
// producer; the event still lands in the log so a later replay
// delivers it. The log is bounded at hubMaxEvents.
func (h *hub) broadcast(kind string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
		kind = "error"
	}
	ev := hubEvent{Kind: kind, Payload: body}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if len(h.events) >= hubMaxEvents {
		// Drop oldest. Copy into a fresh slice so the underlying
		// array doesn't keep references to the dropped payloads.
		drop := len(h.events) - hubMaxEvents + 1
		fresh := make([]hubEvent, len(h.events)-drop, hubMaxEvents)
		copy(fresh, h.events[drop:])
		h.events = fresh
	}
	h.events = append(h.events, ev)
	subs := append([]*hubSub(nil), h.subs...)
	h.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- ev:
		case <-s.cancel:
		}
	}
}

// subscribe returns a snapshot of already-buffered events plus a live
// channel for new ones, and a cancel signal the caller closes when it
// is going away.
func (h *hub) subscribe() (replay []hubEvent, live chan hubEvent, cancel chan struct{}) {
	sub := &hubSub{
		ch:     make(chan hubEvent, 64),
		cancel: make(chan struct{}),
	}
	h.mu.Lock()
	replay = append([]hubEvent(nil), h.events...)
	h.subs = append(h.subs, sub)
	h.mu.Unlock()
	return replay, sub.ch, sub.cancel
}

// close ends the hub and wakes all subscribers. Idempotent.
func (h *hub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := h.subs
	h.subs = nil
	h.mu.Unlock()
	for _, s := range subs {
		close(s.ch)
	}
	close(h.done)
}

// Emit adapts hub.broadcast to the chat-side agent.StreamEmitter
// interface used by the tool loop. Capitalized so chatHub (which
// embeds *hub) satisfies the interface across packages.
func (h *hub) Emit(kind string, payload any) { h.broadcast(kind, payload) }

// chatCtx is the subset of context.Context serveHubSSE needs. Named
// separately so tests can stub without importing context.
type chatCtx interface {
	Done() <-chan struct{}
}

// serveHubSSE releases an HTTP response into an SSE consumer of a hub.
// Replays the hub's event log first (so a page refresh sees everything
// that happened before it arrived), then delivers live events until
// the hub closes or the client disconnects.
//
// Shared between chatHub and the release hub — both want the same
// behavior on the wire.
func serveHubSSE(w http.ResponseWriter, flusher http.Flusher, ctx chatCtx, h *hub) {
	replay, live, cancel := h.subscribe()
	defer close(cancel)
	// Empty comment frame to flush response headers immediately —
	// without this, a subscribe to an in-flight hub that hasn't
	// emitted events yet leaves the HTTP client blocked waiting on
	// headers until the first event lands. EventSource ignores SSE
	// comment lines, so no client-visible effect. Mirrors what
	// serveEngineWaitStream already does.
	_, _ = fmt.Fprint(w, ": subscribed\n\n")
	flusher.Flush()
	for _, ev := range replay {
		writeRawSSE(w, flusher, ev.Kind, ev.Payload)
	}
	for {
		select {
		case ev, ok := <-live:
			if !ok {
				return
			}
			writeRawSSE(w, flusher, ev.Kind, ev.Payload)
		case <-h.done:
			// Drain remaining events buffered on the channel.
			for {
				select {
				case ev, ok := <-live:
					if !ok {
						return
					}
					writeRawSSE(w, flusher, ev.Kind, ev.Payload)
				default:
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// writeRawSSE emits a single SSE event with a pre-marshaled payload.
// Errors on write are ignored: if the client disconnected mid-
// subscription, the subscriber loop exits on the next ctx.Done().
func writeRawSSE(w http.ResponseWriter, f http.Flusher, event string, payload []byte) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	f.Flush()
}
