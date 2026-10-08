package web

import (
	"bytes"
	"sync"
)

// orgHub is the singleton broadcaster for org liveness. Unlike
// chatHub / turnHub (which carry a full event log so a mid-flight
// refresh replays everything), orgHub holds only the latest snapshot.
// Late subscribers don't need history — they need "what's true right
// now."
//
// publish() stores a new snapshot and fans it out to subscribers, but
// only if the bytes actually changed. Idle connections cost nothing.
//
// Subscriber channels are buffered with capacity 1 and non-blocking
// sends. A slow consumer drops the previous queued snapshot and gets
// the newest — never blocks the producer, never replays superseded
// state.
type orgHub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	latest []byte
}

func newOrgHub() *orgHub {
	return &orgHub{subs: map[chan []byte]struct{}{}}
}

// publish stores snapshot as latest and broadcasts to subscribers iff
// it differs from the prior latest. No-op on byte-equal.
func (h *orgHub) publish(snapshot []byte) {
	h.mu.Lock()
	if bytes.Equal(snapshot, h.latest) {
		h.mu.Unlock()
		return
	}
	h.latest = snapshot
	subs := make([]chan []byte, 0, len(h.subs))
	for c := range h.subs {
		subs = append(subs, c)
	}
	h.mu.Unlock()
	for _, c := range subs {
		// Non-blocking send. If the buffer is full, the consumer is
		// slow — replace its queued (now-stale) snapshot with the
		// newest one. The snapshot is the whole truth, so dropping
		// intermediates is correct.
		select {
		case c <- snapshot:
		default:
			select {
			case <-c:
			default:
			}
			select {
			case c <- snapshot:
			default:
			}
		}
	}
}

// subscribe returns the current snapshot (may be nil before the first
// publish) and a channel that delivers future snapshots. Caller MUST
// call unsubscribe when done to release the slot.
func (h *orgHub) subscribe() (initial []byte, ch chan []byte) {
	ch = make(chan []byte, 1)
	h.mu.Lock()
	if h.latest != nil {
		initial = append([]byte(nil), h.latest...)
	}
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return
}

func (h *orgHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}
