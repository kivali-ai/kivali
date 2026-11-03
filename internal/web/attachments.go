package web

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// handleAttachmentDownload serves a blob from the shared attachment
// store. Anyone who knows the SHA can fetch it — visibility is enforced
// by who holds the reference, not by the blob store itself. The
// original bytes go out as a download (setDownloadType).
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
	setDownloadType(w)
	w.Header().Set("content-disposition", `attachment; filename="`+sanitizeDownloadName(att.Name)+`"`)
	if att.Size > 0 {
		// The length lets whatever saves the file show how far it has got.
		w.Header().Set("content-length", strconv.FormatInt(att.Size, 10))
	}
	_, _ = io.Copy(w, rc)
}

// setDownloadType marks a response as a file to save, never a page to
// show. Content-Disposition: attachment alone does not do it: WKWebView,
// which shows a team in Kivali Desktop on macOS, ignores the header and
// displays any type it can (text, markdown, PDF, images) in place of the
// web app. A type it cannot display is saved there as in every browser.
func setDownloadType(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/octet-stream")
	w.Header().Set("x-content-type-options", "nosniff")
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
