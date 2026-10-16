package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Browser sessions are the saved Chromium profiles an agent's web
// subagents run in: one directory per session under
// agents/<slug>/browser/<session>/, holding profile.tar.zst (the
// packed profile, absent until a browser has been saved) and
// session.json. The directory sits outside the agent's memory/ tree,
// so neither the agent container nor its shell mounts it; only core
// reads and writes it, and the browser pod receives the profile over
// its connection to core.
const (
	browserDirName         = "browser"
	browserProfileFilename = "profile.tar.zst"
	browserSessionFilename = "session.json"
)

// BrowserSession is session.json: what core knows about a session
// apart from the profile's bytes.
type BrowserSession struct {
	// Name is the session's name: "open", or the registrable domain
	// the session is locked to.
	Name string `json:"name"`
	// Site is the registrable domain for a site session, empty for open.
	Site string `json:"site,omitempty"`
	// Created is when the session was first named; LastUsed when a
	// web subagent last ran in it or a browser was last saved.
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
	// ProfileBytes is the packed profile's size, 0 when none is saved.
	ProfileBytes int64 `json:"profile_bytes,omitempty"`
}

// browserSessionName is what a session may be called: a lower-case
// label of letters, digits, dots and dashes, starting with a letter or
// digit. "open" fits; so does a registrable domain. It is a single
// path element by construction.
var browserSessionName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)

// ErrInvalidBrowserSession is returned for a session name that is not
// a plain lower-case label.
var ErrInvalidBrowserSession = errors.New("a session is \"open\" or a site such as vendor.com: lower-case letters, digits, dots and dashes")

// ValidBrowserSessionName reports whether name may name a session.
func ValidBrowserSessionName(name string) error {
	if !browserSessionName.MatchString(name) || name == "." || name == ".." {
		return ErrInvalidBrowserSession
	}
	return nil
}

func (s *FSStore) browserSessionDir(slug, session string) string {
	return s.path("agents", slug, browserDirName, session)
}

// ListBrowserSessions returns an agent's sessions, by name.
func (s *FSStore) ListBrowserSessions(slug string) ([]BrowserSession, error) {
	entries, err := os.ReadDir(s.path("agents", slug, browserDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []BrowserSession
	for _, e := range entries {
		if !e.IsDir() || ValidBrowserSessionName(e.Name()) != nil {
			continue
		}
		bs, ok, err := s.ReadBrowserSession(slug, e.Name())
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, bs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadBrowserSession returns one session's record; ok is false when
// the session does not exist.
func (s *FSStore) ReadBrowserSession(slug, session string) (BrowserSession, bool, error) {
	if err := ValidBrowserSessionName(session); err != nil {
		return BrowserSession{}, false, err
	}
	b, err := os.ReadFile(filepath.Join(s.browserSessionDir(slug, session), browserSessionFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return BrowserSession{}, false, nil
	}
	if err != nil {
		return BrowserSession{}, false, err
	}
	var bs BrowserSession
	if err := json.Unmarshal(b, &bs); err != nil {
		return BrowserSession{}, false, fmt.Errorf("%s/%s/%s: %w", slug, session, browserSessionFilename, err)
	}
	return bs, true, nil
}

// WriteBrowserSession writes a session's record, creating the session.
func (s *FSStore) WriteBrowserSession(slug string, bs BrowserSession) error {
	if err := ValidBrowserSessionName(bs.Name); err != nil {
		return err
	}
	dir := s.browserSessionDir(slug, bs.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(bs, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, browserSessionFilename), b, 0o644)
}

// EnsureBrowserSession returns the session's record, creating it as
// of now when it does not exist. site is the registrable domain for a
// site session, empty for open.
func (s *FSStore) EnsureBrowserSession(slug, session, site string, now time.Time) (BrowserSession, error) {
	bs, ok, err := s.ReadBrowserSession(slug, session)
	if err != nil {
		return BrowserSession{}, err
	}
	if ok {
		return bs, nil
	}
	bs = BrowserSession{Name: session, Site: site, Created: now.UTC(), LastUsed: now.UTC()}
	return bs, s.WriteBrowserSession(slug, bs)
}

// TouchBrowserSession records that the session was used as of now.
func (s *FSStore) TouchBrowserSession(slug, session string, now time.Time) error {
	bs, ok, err := s.ReadBrowserSession(slug, session)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	bs.LastUsed = now.UTC()
	return s.WriteBrowserSession(slug, bs)
}

// WriteBrowserProfile replaces the session's saved profile with the
// bytes read from r, through a temporary file and a rename, and
// records the size on the session. The session must exist.
func (s *FSStore) WriteBrowserProfile(slug, session string, r io.Reader, now time.Time) (int64, error) {
	bs, ok, err := s.ReadBrowserSession(slug, session)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("browser session %s/%s does not exist", slug, session)
	}
	dir := s.browserSessionDir(slug, session)
	tmp, err := os.CreateTemp(dir, ".profile-*")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	n, err := io.Copy(tmp, r)
	if err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, browserProfileFilename)); err != nil {
		return 0, err
	}
	committed = true
	if err := syncDir(dir); err != nil {
		return 0, err
	}
	bs.ProfileBytes = n
	bs.LastUsed = now.UTC()
	return n, s.WriteBrowserSession(slug, bs)
}

// OpenBrowserProfile opens the session's saved profile for reading and
// returns its size. A session with no saved profile returns a nil
// reader and no error.
func (s *FSStore) OpenBrowserProfile(slug, session string) (io.ReadCloser, int64, error) {
	if err := ValidBrowserSessionName(session); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(filepath.Join(s.browserSessionDir(slug, session), browserProfileFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// DeleteBrowserSession removes a session, its profile included. A
// session that does not exist is not an error.
func (s *FSStore) DeleteBrowserSession(slug, session string) error {
	if err := ValidBrowserSessionName(session); err != nil {
		return err
	}
	return os.RemoveAll(s.browserSessionDir(slug, session))
}

// DeleteBrowserSessions removes every session of an agent, as
// offboarding does.
func (s *FSStore) DeleteBrowserSessions(slug string) error {
	return os.RemoveAll(s.path("agents", slug, browserDirName))
}
