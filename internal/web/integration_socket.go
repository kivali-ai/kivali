// Package web (integration_socket.go) — the integration pods' socket.
//
// integration.sock, in the integration subdirectory of the UDS dir,
// serves two routes and nothing else, so an integration pod never
// reaches the agent routes on core.sock. Both carry the per-pod token
// (integrationpod.TokenHeader):
//
//   - GET /v1/integration/{name}/events: the shim's one SSE stream.
//     The handshake names the version the pod runs, which must be the
//     approved one, and the hash of the shim's proxy secret, which is
//     registered as the integration's scoped credential.
//   - POST /v1/integration/{name}/rpc: one JSON-RPC message from the
//     pod; a request is answered with its response, anything else with
//     204.

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
)

// StartIntegrationSocket binds the integration pods' socket and serves
// their two routes on it, and only those. Returns the stop function.
func (s *Server) StartIntegrationSocket(path string) (func(), error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/integration/{name}/events", s.handleIntegrationEvents)
	mux.HandleFunc("POST /v1/integration/{name}/rpc", s.handleIntegrationRPC)
	return s.startUDSListenerWith(path, mux)
}

// handleIntegrationEvents serves a shim's events stream: start first,
// then whatever core queues for it, with comment heartbeats.
func (s *Server) handleIntegrationEvents(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.Integrations == nil {
		http.Error(w, "integrations not configured", http.StatusServiceUnavailable)
		return
	}
	if !integration.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	hash := r.Header.Get(integrationpod.ProxyHashHeader)
	if hash != "" && !egresscred.ValidHash(hash) {
		http.Error(w, "the proxy hash is not a lowercase hex SHA-256", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	link, err := s.Integrations.connect(name, r.Header.Get(integrationpod.TokenHeader), r.Header.Get(integrationpod.VersionHeader), hash)
	switch {
	case errors.Is(err, errIntegrationRefused):
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	case err != nil:
		log.Printf("integration %s: connect: %v", name, err)
		http.Error(w, "the connection could not be taken on", http.StatusInternalServerError)
		return
	}
	defer s.Integrations.disconnect(link)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": ok\n\n"); err != nil {
		return
	}
	flusher.Flush()
	hb := s.clk().NewTicker(integrationHeartbeat)
	defer hb.Stop()
	write := func(ev integrationpod.Event) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, ev.Data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-link.closed:
			// What was queued before the close (a shutdown) still goes.
			for {
				select {
				case ev := <-link.events:
					if !write(ev) {
						return
					}
				default:
					return
				}
			}
		case ev := <-link.events:
			if !write(ev) {
				return
			}
		case <-hb.C():
			if _, err := io.WriteString(w, ": hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleIntegrationRPC takes one JSON-RPC message from a pod.
func (s *Server) handleIntegrationRPC(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.Integrations == nil {
		http.Error(w, "integrations not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.Integrations.authorized(name, r.Header.Get(integrationpod.TokenHeader)) {
		http.Error(w, errIntegrationRefused.Error(), http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, integrationMaxBody))
	if err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var msg integrationpod.Message
	if err := json.Unmarshal(body, &msg); err != nil || (!msg.IsRequest() && !msg.IsNotification() && !msg.IsResponse()) {
		http.Error(w, "not a JSON-RPC message", http.StatusBadRequest)
		return
	}
	reply := s.Integrations.onMessage(r.Context(), name, msg)
	if reply == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(reply)
}
