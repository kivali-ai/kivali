package web

import (
	"errors"
	"net/http"

	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// handleAPIAutoRelease serves POST /api/v1/auto-release: body
// {"value": "now|30s|2m|5m|20m|off"}. The same setting the inbox
// slider writes (setAutoRelease), so every instance of the control in
// either UI follows through the snapshot's inbox.auto_release.
func (s *Server) handleAPIAutoRelease(w http.ResponseWriter, r *http.Request) {
	var req apitypes.AutoReleaseRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if err := s.setAutoRelease(string(req.Value)); err != nil {
		if errors.Is(err, errUnknownDetent) {
			writeAPIError(w, http.StatusBadRequest, "auto-release must be one of now, 30s, 2m, 5m, 20m or off", whoDevelopers)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "the auto-release setting could not be saved", whoDevelopers)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.AutoReleaseResponse{
		Value: apitypes.AutoRelease(autoReleaseKey(s.Store.ReadAutoRelease())),
	})
}
