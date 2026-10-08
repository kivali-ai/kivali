package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// StagedPublish is one record of phase-1 prep work for a publish_*
// tool call: parse done, role-conflict cleared, attachments resolved,
// msg.Date pinned. Persisted at staged_publishes/<StageID>.json so a
// crash or transport flake between stage and commit doesn't lose the
// already-validated work — the bridge retries CommitPublish with the
// same StageID and the server finds this record on disk.
//
// Message is fully resolved: WriteMessage(Message) at commit time is
// the only durable side effect remaining, and the message filename
// is derived deterministically from Message fields (including the
// pinned Date), so writeAtomic over identical content is a no-op on
// retry.
//
// Tool and From are recorded for the rendered tool_result body and
// log lines. CreatedAt drives the janitor sweep (records older than
// the configured TTL are dropped on the assumption the bridge gave up).
type StagedPublish struct {
	StageID   string    `json:"stage_id"`
	Tool      string    `json:"tool"`
	From      string    `json:"from"`
	Message   Message   `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// stagedPublishesDir is the on-disk directory holding StagedPublish
// records. Top-level (not per-agent) so the janitor sweep walks one
// directory instead of every agent's tree.
const stagedPublishesDir = "staged_publishes"

// ErrStagedPublishNotFound signals that a CommitPublish lookup by
// StageID failed because the record is missing — typically expired by
// the janitor, or a stale bridge talking to a fresh core. Callers
// surface this to the bridge so it can re-stage rather than retry.
var ErrStagedPublishNotFound = errors.New("staged publish not found")

// validStageID rejects path-traversal and stray separators. Stage IDs
// are crypto/rand-generated 16-hex-char strings on the happy path;
// this guard is defense against malformed input from the wire.
func validStageID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// stagedPublishPath returns the on-disk path for a StagedPublish.
// validStageID has already vetted id when this is called from a
// post-handshake server path; the function also rejects on its own
// so it's safe to call directly.
func (s *FSStore) stagedPublishPath(id string) (string, error) {
	if !validStageID(id) {
		return "", fmt.Errorf("store: invalid stage id %q", id)
	}
	return s.path(stagedPublishesDir, id+".json"), nil
}

// WriteStagedPublish persists rec atomically at
// staged_publishes/<StageID>.json. Creates the parent directory on
// first call. Caller is responsible for setting rec.StageID,
// rec.CreatedAt, and rec.Message (fully resolved).
func (s *FSStore) WriteStagedPublish(rec StagedPublish) error {
	path, err := s.stagedPublishPath(rec.StageID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("store: mkdir staged_publishes: %w", err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal staged publish: %w", err)
	}
	return writeAtomic(path, b, 0o644)
}

// ReadStagedPublish loads the StagedPublish record by id. Returns
// ErrStagedPublishNotFound when the file is missing (typical case for
// a bridge retry after the janitor already swept the record, or a
// stale wire-level retry after CommitPublish already deleted it).
func (s *FSStore) ReadStagedPublish(id string) (StagedPublish, error) {
	path, err := s.stagedPublishPath(id)
	if err != nil {
		return StagedPublish{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return StagedPublish{}, ErrStagedPublishNotFound
	}
	if err != nil {
		return StagedPublish{}, err
	}
	var rec StagedPublish
	if err := json.Unmarshal(b, &rec); err != nil {
		return StagedPublish{}, fmt.Errorf("store: unmarshal staged publish %s: %w", id, err)
	}
	return rec, nil
}

// DeleteStagedPublish removes the record after a successful commit.
// Missing file is a no-op (idempotent — a stale retry sees the
// already-committed state from the queue and falls through to here).
func (s *FSStore) DeleteStagedPublish(id string) error {
	path, err := s.stagedPublishPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// SweepStagedPublishes deletes staged-publish records older than
// maxAge as measured against now. Returns the count removed and the
// first error encountered (continuing past errors so one bad record
// doesn't block the rest of the sweep). Intended to run on a tick
// from a long-running goroutine; safe to call from anywhere.
//
// maxAge bounds the window for legitimate bridge retries between
// stage and commit. Default 1h is well beyond the few-seconds-typical
// gap; pick larger if your network is uniquely terrible.
func (s *FSStore) SweepStagedPublishes(now time.Time, maxAge time.Duration) (int, error) {
	dir := s.path(stagedPublishesDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var firstErr error
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		rec, rerr := s.ReadStagedPublish(id)
		if rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		if now.Sub(rec.CreatedAt) < maxAge {
			continue
		}
		if derr := s.DeleteStagedPublish(id); derr != nil {
			if firstErr == nil {
				firstErr = derr
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}
