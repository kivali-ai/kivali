package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// mintBackupLink POSTs /api/v1/org/backup/links signed in, from this
// origin, and returns the link.
func mintBackupLink(t *testing.T, srv *Server) apitypes.BackupLink {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/org/backup/links", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mint: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var link apitypes.BackupLink
	if err := json.Unmarshal(rr.Body.Bytes(), &link); err != nil {
		t.Fatalf("mint: %v: %s", err, rr.Body.String())
	}
	return link
}

// getBackupLink GETs a minted link as a navigation would: signed in,
// with no Sec-Fetch-Site (Kivali Desktop fetches it itself, and a
// browser's navigation says none).
func getBackupLink(t *testing.T, srv *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	return rr
}

// A minted link downloads the same backup the POST streams, as a GET
// with no same-origin marker: that is what the web app's navigation and
// Kivali Desktop's own fetch send. Without the GET route a download in
// Kivali Desktop, which replays the URL it is handed, had nothing to
// fetch.
func TestBackupLinkDownloadsTheBackupOnce(t *testing.T) {
	srv := newTestServer(t)
	writeFile(t, filepath.Join(srv.Store.Root(), "agents", "alice", "chat.jsonl"), "hi\n")

	link := mintBackupLink(t, srv)
	if !strings.HasPrefix(link.URL, "/api/v1/org/backup/") || len(link.URL) != len("/api/v1/org/backup/")+32 {
		t.Fatalf("link = %q, want /api/v1/org/backup/<32 hex digits>", link.URL)
	}

	rr := getBackupLink(t, srv, link.URL)
	if rr.Code != http.StatusOK {
		t.Fatalf("download: status = %d, body %s", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("content-disposition"); !strings.HasPrefix(cd, "attachment; filename=kivali-backup-") {
		t.Errorf("content-disposition = %q, want the backup's attachment", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("body is not a zip: %v", err)
	}
	found := false
	for _, f := range zr.File {
		if f.Name == "agents/alice/chat.jsonl" {
			found = true
		}
	}
	if !found {
		t.Errorf("the zip lacks agents/alice/chat.jsonl")
	}

	// Once: a link that has downloaded is gone, so a page that keeps the
	// URL cannot take backups with it later.
	if again := getBackupLink(t, srv, link.URL); again.Code != http.StatusNotFound {
		t.Errorf("second download: status = %d, want 404", again.Code)
	}
}

// A link that was never minted, or that was minted longer ago than its
// time, answers 404: the token is the whole gate, so an expired one must
// not keep working.
func TestBackupLinkExpiresAndRefusesUnknownTokens(t *testing.T) {
	srv := newTestServer(t)
	clk := clock.NewFake()
	srv.Clock = clk

	if rr := getBackupLink(t, srv, backupLinkPath(strings.Repeat("0", 32))); rr.Code != http.StatusNotFound {
		t.Errorf("unknown token: status = %d, want 404", rr.Code)
	}

	link := mintBackupLink(t, srv)
	clk.Advance(backupLinkTTL + time.Second)
	rr := getBackupLink(t, srv, link.URL)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expired link: status = %d, want 404", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "expired") {
		t.Errorf("expired link: body %s, want to say the link expired", rr.Body.String())
	}
}

// Minting takes a same-origin POST, as every write does: a page
// elsewhere cannot mint a link it could then send the browser to.
func TestBackupLinkMintRefusesAnotherSite(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/org/backup/links", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// The store keeps at most backupLinkMax live links, dropping the one
// expiring soonest, so a page that mints in a loop cannot grow memory.
func TestBackupLinksAreBounded(t *testing.T) {
	var links backupLinks
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	first := links.mint(now)
	for i := 0; i < backupLinkMax; i++ {
		links.mint(now.Add(time.Duration(i+1) * time.Second))
	}
	if len(links.live) != backupLinkMax {
		t.Errorf("live = %d, want %d", len(links.live), backupLinkMax)
	}
	if links.take(first, now) {
		t.Errorf("the first link survived minting past the bound")
	}
}
