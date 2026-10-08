package web

import (
	"errors"
	"log"
	"net/http"

	"github.com/kivali-ai/kivali/internal/agent"
)

// Pending message actions for the Kivali web app. The logic lives in
// chat_pending.go; these handlers map its errors onto the API's
// {error, who} shape. wireAPIRoutes mounts them.
//
//	DELETE /api/v1/agents/{slug}/pending/{id}            204 | 404 | 409
//	POST   /api/v1/agents/{slug}/pending/{id}/restore    200 | 404 | 410
//	POST   /api/v1/agents/{slug}/pending/{id}/send-now   202 | 404 | 409 | 502

// wireAPIPendingRoutes registers the pending message routes on the API
// mux.
func (s *Server) wireAPIPendingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /api/v1/agents/{slug}/pending/{id}", s.handleAPIPendingDelete)
	mux.HandleFunc("POST /api/v1/agents/{slug}/pending/{id}/restore", s.handleAPIPendingRestore)
	mux.HandleFunc("POST /api/v1/agents/{slug}/pending/{id}/send-now", s.handleAPIPendingSendNow)
}

// pendingTarget reads the slug and id from the path. The CEO has no
// chat of its own, so nothing is ever pending for it.
func pendingTarget(r *http.Request) (slug, id string, ok bool) {
	slug, id = r.PathValue("slug"), r.PathValue("id")
	return slug, id, slug != "" && id != "" && slug != agent.CEOSlug
}

func (s *Server) handleAPIPendingDelete(w http.ResponseWriter, r *http.Request) {
	slug, id, ok := pendingTarget(r)
	if !ok {
		writePendingError(w, errPendingNotFound)
		return
	}
	if err := s.deletePending(slug, id); err != nil {
		writePendingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIPendingRestore(w http.ResponseWriter, r *http.Request) {
	slug, id, ok := pendingTarget(r)
	if !ok {
		writePendingError(w, errPendingNotFound)
		return
	}
	resp, err := s.restorePending(slug, id)
	if err != nil {
		writePendingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAPIPendingSendNow(w http.ResponseWriter, r *http.Request) {
	slug, id, ok := pendingTarget(r)
	if !ok {
		writePendingError(w, errPendingNotFound)
		return
	}
	if err := s.sendPendingNow(slug, id); err != nil {
		writePendingError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct{}{})
}

// writePendingError maps a chat_pending.go error to its status and
// the person who can act on it.
func writePendingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPendingNotFound):
		writeAPIError(w, http.StatusNotFound, err.Error(), "no one; it was delivered or deleted, and the chat shows which")
	case errors.Is(err, errPendingOffered):
		writeAPIError(w, http.StatusConflict, err.Error(), "use Send now, or wait for the turn")
	case errors.Is(err, errPendingExpired):
		writeAPIError(w, http.StatusGone, err.Error(), "you, by sending the message again")
	case errors.Is(err, errPendingRotating):
		writeAPIError(w, http.StatusConflict, err.Error(), "no one; wait, the message is delivered first in the new chat")
	case errors.Is(err, errPendingNothingToPause):
		writeAPIError(w, http.StatusConflict, err.Error(), "no one; wait, the message is delivered when the agent's current work ends")
	case errors.Is(err, errPendingPauseFailed):
		writeAPIError(w, http.StatusBadGateway, err.Error(), "you, by waiting for the turn or pressing Stop")
	default:
		log.Printf("pending: %v", err)
		writeAPIError(w, http.StatusInternalServerError, "could not deliver the message", whoDevelopers)
	}
}
