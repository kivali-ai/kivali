package web

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

// pendingDelivery is one pending message on the release preview page.
// Path is the message's relPath so Comment / Bounce can target it.
//
// Recipients is every agent the message is queued for — one slug for
// everything but a notice, which can address several. It renders as
// one card either way: the CEO decides about the message, not about
// each of its deliveries, so Release delivers to all of them and
// Bounce cancels the whole thing back to the sender.
//
// View is the shared InboxView the CEO inbox uses, reused here for
// visual consistency (icon, label, attachment chips).
//
// Due is when the message releases on its own (autoReleaseDue); zero
// when it has no deadline and waits for the CEO.
type pendingDelivery struct {
	Path       string
	Recipients []string
	Message    store.Message
	View       agent.InboxView
	Due        time.Time
}

// errBadMessagePath refuses a path that is not a message file under
// messages/.
var errBadMessagePath = errors.New("bad path")

// errNoEngine refuses a release, bounce or reply on a deployment with
// no runtime to deliver through.
var errNoEngine = errors.New("no engine configured")

// bounceOne's refusals: a bounce says why, and an assignment event has no
// sender to say it to.
var (
	errBounceNeedsComment    = errors.New("bounce requires a comment explaining why")
	errBounceAssignmentEvent = errors.New("an assignment event cannot be bounced; change the assignment on the Work board instead")
)

// cleanMessagePath is a posted message path, cleaned, when it names a
// file under messages/.
func cleanMessagePath(p string) (string, bool) {
	clean := filepath.Clean(p)
	if !strings.HasPrefix(clean, "messages/") || strings.Contains(clean, "..") {
		return "", false
	}
	return clean, true
}

// releaseOne releases one queued message to every agent it is queued
// for, with note (trimmed; empty for none) appended as the CEO's
// annotation. Shared by POST /release and POST /api/v1/queue/release.
//
// Rides on Messenger.ReleaseSelected with a single-path slice so
// quick-release and multi-select share one drain primitive (atomic
// under one queue-lock, broadcast wake). A path no longer queued is
// messaging.ErrNotQueued: ReleaseSelected reports it via notFound
// rather than as an error so a partially-stale batch still drains the
// valid paths. The queue write notifies /org/stream itself
// (Store.SetOnMessageQueueWrite).
func (s *Server) releaseOne(ctx context.Context, path, note string) error {
	relPath, ok := cleanMessagePath(path)
	if !ok {
		return errBadMessagePath
	}
	if s.Runtime == nil {
		return errNoEngine
	}
	var comments map[string]string
	if note = strings.TrimSpace(note); note != "" {
		comments = map[string]string{relPath: note}
	}
	delivered, notFound, warnings, err := s.Messenger.ReleaseSelected(ctx, []string{relPath}, comments, s.deliveryHooks())
	if err != nil {
		return err
	}
	if len(notFound) > 0 {
		return messaging.ErrNotQueued
	}
	if len(delivered) == 0 {
		// Path was in the queue but read/append failed mid-drain.
		// Surface the warning so the CEO sees what broke.
		msg := "release failed"
		if len(warnings) > 0 {
			msg = warnings[0]
		}
		return errors.New(msg)
	}
	for _, warning := range warnings {
		log.Printf("release %s: %s", relPath, warning)
	}
	return nil
}

// bounceOne cancels one queued message and hands comment back to its
// sender. Shared by POST /bounce and POST /api/v1/queue/bounce; see
// handleBounce for why the bounce is delivered at once.
func (s *Server) bounceOne(ctx context.Context, path, comment string) error {
	relPath, ok := cleanMessagePath(path)
	if !ok {
		return errBadMessagePath
	}
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return errBounceNeedsComment
	}
	if s.Runtime == nil {
		return errNoEngine
	}
	// An assignment_event is the tracker reporting a change already made;
	// there is no sender to hand a comment back to. The CEO edits the
	// assignment instead, and the queued wake follows the assignment (a drop or
	// reassign pulls it). See docs/developers/assignments.md §Wakes.
	if m, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), relPath)); err == nil && m.Type == store.MsgAssignmentEvent {
		return errBounceAssignmentEvent
	}
	return s.Messenger.Bounce(ctx, relPath, comment, s.deliveryHooks())
}

// releaseMany releases paths (every queued message when paths is
// empty) with per-path comments, and reports how many messages went
// out. Serves POST /api/v1/queue/release-all.
//
// A list drains through ReleaseSelected, which counts what it
// delivered; paths no longer queued are skipped. Release-everything
// drains through ReleaseAll, which does not count, so the number there
// is the queue's distinct paths read just before the drain: what the
// Queue header showed.
func (s *Server) releaseMany(ctx context.Context, paths []string, comments map[string]string) (int, error) {
	if s.Runtime == nil {
		return 0, errNoEngine
	}
	var (
		warnings []string
		released int
		err      error
	)
	if len(paths) > 0 {
		var delivered []string
		delivered, _, warnings, err = s.Messenger.ReleaseSelected(ctx, paths, comments, s.deliveryHooks())
		released = len(delivered)
	} else {
		if q, qerr := s.Store.ReadMessageQueue(); qerr == nil {
			released = len(pendingPathsOf(q))
		}
		warnings, err = s.Messenger.ReleaseAll(ctx, comments, s.deliveryHooks())
	}
	if err != nil {
		return 0, err
	}
	for _, warning := range warnings {
		// Log non-fatal delivery warnings (dead-letter, missing
		// recipient). The user-visible signal is the inbox state
		// reconciling after the snapshot lands.
		log.Printf("release-all: %s", warning)
	}
	s.NotifyOrgState()
	return released, nil
}

// cleanReleaseAll applies the release-all input rules of
// POST /api/v1/queue/release-all: paths and comment
// keys that are not under messages/ are dropped, comments are trimmed
// and blank ones dropped, and an empty result is nil.
func cleanReleaseAll(rawPaths []string, rawComments map[string]string) ([]string, map[string]string) {
	var paths []string
	if len(rawPaths) > 0 {
		paths = make([]string, 0, len(rawPaths))
		for _, p := range rawPaths {
			if clean, ok := cleanMessagePath(p); ok {
				paths = append(paths, clean)
			}
		}
		if len(paths) == 0 {
			paths = nil
		}
	}
	var comments map[string]string
	if len(rawComments) > 0 {
		comments = make(map[string]string, len(rawComments))
		for path, comment := range rawComments {
			clean, ok := cleanMessagePath(path)
			if !ok {
				continue
			}
			trimmed := strings.TrimSpace(comment)
			if trimmed == "" {
				continue
			}
			comments[clean] = trimmed
		}
		if len(comments) == 0 {
			comments = nil
		}
	}
	return paths, comments
}
