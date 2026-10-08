package web

import (
	"fmt"
	"net/http"
	"time"
)

// orgStreamHeartbeat is how often we emit an SSE comment to keep
// middleboxes (cloud LBs, nginx default 60s idle timeout) from killing
// an otherwise-idle connection. The wire cost is one byte per beat per
// direction.
const orgStreamHeartbeat = 25 * time.Second

// orgStreamRetryMs is the EventSource auto-reconnect backoff hint sent
// at the top of the stream. 1500ms gives the server a beat to come
// back after a restart without retry storms; the browser's default
// (3000ms) is fine too but explicit is clearer.
const orgStreamRetryMs = 1500

// handleOrgStream subscribes the browser to the org-status SSE feed.
// First event: a `snapshot` carrying the current state so a fresh page
// doesn't have to wait for a transition. Subsequent events: full
// snapshots emitted only when state actually changes (orgHub dedupes
// on byte equality). Heartbeats every 25s keep the connection warm.
func (s *Server) handleOrgStream(w http.ResponseWriter, r *http.Request) {
	if s.orgHub == nil {
		http.Error(w, "org hub not initialized", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no") // disable nginx buffering when fronted

	// retry: directive sets the EventSource auto-reconnect interval.
	// A predictable jitter-friendly value (1500ms) avoids thundering-
	// herd reconnects after a server restart at scale.
	_, _ = fmt.Fprintf(w, "retry: %d\n\n", orgStreamRetryMs)
	flusher.Flush()

	initial, ch := s.orgHub.subscribe()
	defer s.orgHub.unsubscribe(ch)

	// If the hub hasn't published yet (server just started, nothing
	// has changed state), build the snapshot inline so this connection
	// gets data immediately. Cheap; no Notify storm.
	if initial == nil {
		initial = s.buildOrgSnapshot()
	}
	writeRawSSE(w, flusher, "snapshot", initial)

	hb := s.clk().NewTicker(orgStreamHeartbeat)
	defer hb.Stop()

	for {
		select {
		case snap, ok := <-ch:
			if !ok {
				return
			}
			writeRawSSE(w, flusher, "snapshot", snap)
		case <-hb.C():
			// SSE comment line. The client never sees this in event
			// callbacks; it exists purely to keep the TCP path warm
			// past intermediate idle timeouts.
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
