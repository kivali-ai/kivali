package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// navAccept is what every mainstream browser sends on a top-level
// navigation — a left click, a cmd-click, a middle click.
const navAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"

// TestAttachmentDownloadServesTheOriginal: GET /attachments/<sha> hands
// back the stored bytes as a download, whatever the file type and
// however the browser asked. Markdown included: the web app renders
// documents itself, so the server never answers an attachment with a
// page of its own.
func TestAttachmentDownloadServesTheOriginal(t *testing.T) {
	srv := newTestServer(t)
	for _, name := range []string{"notes.md", "notes.txt", "report.pdf"} {
		t.Run(name, func(t *testing.T) {
			// Distinct bytes per file: the store deduplicates by hash.
			body := "# " + name + "\n\n<script>alert(1)</script>\n"
			att, err := srv.Store.AddAttachment(context.Background(), name, strings.NewReader(body))
			if err != nil {
				t.Fatalf("AddAttachment(%s): %v", name, err)
			}
			req := httptest.NewRequest(http.MethodGet, "/attachments/"+att.SHA, nil)
			req.Header.Set("accept", navAccept)
			req.Header.Set("sec-fetch-mode", "navigate")
			req.Header.Set("sec-fetch-dest", "document")
			rr := httptest.NewRecorder()
			authedHandler(t, srv).ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got := rr.Body.String(); got != body {
				t.Errorf("body = %q, want the original bytes", got)
			}
			if cd := rr.Header().Get("content-disposition"); cd != `attachment; filename="`+name+`"` {
				t.Errorf("content-disposition = %q, want an attachment download named %s", cd, name)
			}
		})
	}
}

func TestAttachmentDownloadUnknownIs404(t *testing.T) {
	srv := newTestServer(t)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/attachments/"+strings.Repeat("0", 64), nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}
