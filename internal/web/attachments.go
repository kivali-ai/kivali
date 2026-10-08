package web

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// handleAttachmentDownload serves a blob from the shared attachment
// store. Anyone who knows the SHA can fetch it — visibility is enforced
// by who holds the reference, not by the blob store itself. The
// original bytes go out with Content-Disposition: attachment.
func (s *Server) handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" {
		http.Error(w, "sha required", http.StatusBadRequest)
		return
	}
	att, err := s.Store.GetAttachment(sha)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rc, err := s.Store.OpenAttachmentOriginal(sha)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	mime := att.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("content-type", mime)
	w.Header().Set("content-disposition", `attachment; filename="`+sanitizeDownloadName(att.Name)+`"`)
	_, _ = io.Copy(w, rc)
}

// sanitizeDownloadName strips control characters and quotes from an
// attachment name so it's safe to embed in a Content-Disposition header.
func sanitizeDownloadName(s string) string {
	s = strings.ReplaceAll(s, `"`, "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	if s == "" {
		return "attachment"
	}
	return s
}
