package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// A backup is taken with POST /api/v1/org/backup (backup.go), which
// only a page on this origin may send. A download, though, is a
// navigation: the web app sends the browser to a URL and the browser
// (or Kivali Desktop, which fetches every download of a team's window
// itself, as the window's person) saves what comes back. A navigation
// is a GET, so the page first mints a link here, then goes to it.
//
// A link is a one-time token: it works once, within backupLinkTTL, and
// only with a session that may take backups anyway. It cannot be
// guessed, so a page elsewhere cannot start a backup by pointing an
// image or a frame at it.

// backupLinkTTL is how long a minted link stays usable.
const backupLinkTTL = 2 * time.Minute

// backupLinkMax bounds the links outstanding at once; minting past it
// drops the one that expires soonest.
const backupLinkMax = 16

// backupLinks holds the links minted and not yet used, by token, with
// when each stops working. The zero value is ready.
type backupLinks struct {
	mu   sync.Mutex
	live map[string]time.Time
}

// mint adds a link that works until now+backupLinkTTL and returns its
// token.
func (l *backupLinks) mint(now time.Time) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	token := hex.EncodeToString(b[:])
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.live == nil {
		l.live = map[string]time.Time{}
	}
	for t, until := range l.live {
		if !until.After(now) {
			delete(l.live, t)
		}
	}
	for len(l.live) >= backupLinkMax {
		oldest, at := "", time.Time{}
		for t, until := range l.live {
			if oldest == "" || until.Before(at) {
				oldest, at = t, until
			}
		}
		delete(l.live, oldest)
	}
	l.live[token] = now.Add(backupLinkTTL)
	return token
}

// take uses a link up: true when token was minted, not yet used and
// still within its time.
func (l *backupLinks) take(token string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.live[token]
	if !ok {
		return false
	}
	delete(l.live, token)
	return until.After(now)
}

// backupLinkPath is where a minted link downloads: the backup route
// with the token as its last segment.
func backupLinkPath(token string) string {
	return "/api/v1/org/backup/" + token
}

// handleBackupLinkMint answers POST /api/v1/org/backup/links with a
// link that downloads the backup once (apitypes.BackupLink).
func (s *Server) handleBackupLinkMint(w http.ResponseWriter, r *http.Request) {
	token := s.backupLinks.mint(s.Clock.Now())
	writeJSON(w, http.StatusOK, apitypes.BackupLink{URL: backupLinkPath(token)})
}

// handleBackupLinkDownload answers GET /api/v1/org/backup/{token}: the
// backup, streamed as POST /api/v1/org/backup streams it, when the
// token is a live link; 404 otherwise, a used or expired link included.
func (s *Server) handleBackupLinkDownload(w http.ResponseWriter, r *http.Request) {
	if !s.backupLinks.take(r.PathValue("token"), s.Clock.Now()) {
		writeAPIError(w, http.StatusNotFound, "That backup link has been used or has expired", "you, by choosing Download a backup again")
		return
	}
	s.handleBackupDownload(w, r)
}
