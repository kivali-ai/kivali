package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleMessage serves the raw markdown (frontmatter + body) of a
// message under data/messages/. The path comes in as a multi-segment
// wildcard matching on everything after /messages/, e.g.
// "release-0003/20260419T172715Z-ceo_notification-alice--to--ceo.md".
//
// Query flags:
//
//	?dl=1   sets Content-Disposition: attachment so the browser
//	        downloads the file instead of rendering it inline.
//
// This exists primarily so the CEO can one-click download/copy cowork
// requests from the queue view; it also works as a general raw-message
// viewer for auditing.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("path")
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	// Traversal guard: clean the path and require it to stay inside
	// "messages/". Since PathValue can contain arbitrary segments, a
	// clean-and-prefix check is the straightforward way.
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") || strings.ContainsRune(clean, 0) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	// Full host path under the store.
	abs := filepath.Join(s.Store.Root(), "messages", clean)
	// Re-verify the resolved absolute path is still under the
	// messages/ root.
	docRoot := filepath.Join(s.Store.Root(), "messages")
	if abs != docRoot && !strings.HasPrefix(abs, docRoot+string(filepath.Separator)) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	// Read the raw bytes directly — frontmatter + body — so the
	// user gets the complete on-disk record. message.Body alone
	// would render as a blank page whenever a CEO approved/denied
	// without typing a message (valid state: empty body, meaningful
	// frontmatter). Raw file view is the forensics tool; give them
	// everything that's actually persisted.
	raw, err := os.ReadFile(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Still parse the message to populate the content-type and
	// header hints — we want a 404 for malformed files rather
	// than streaming garbage.
	message, perr := s.Store.ReadMessage(abs)
	if perr != nil {
		http.NotFound(w, r)
		return
	}
	download := r.URL.Query().Get("dl") == "1"
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	baseName := sanitizeDownloadName(filepath.Base(clean))
	w.Header().Set("content-type", "text/markdown; charset=utf-8")
	w.Header().Set("content-disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, baseName))
	w.Header().Set("x-message-title", message.Title)
	w.Header().Set("x-message-type", string(message.Type))
	_, _ = w.Write(raw)
}
