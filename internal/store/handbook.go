package store

import (
	"errors"
	"os"
	"time"
)

// HandbookFilename is where the org's handbook lives, relative to the
// data directory.
const HandbookFilename = "handbook.md"

// ReadHandbook returns the current handbook body or ErrNotFound.
func (s *FSStore) ReadHandbook() (string, error) {
	b, err := os.ReadFile(s.path(HandbookFilename))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// HandbookUpdatedAt returns when the handbook was last written (the
// file's mtime, UTC), or ErrNotFound when none has been.
func (s *FSStore) HandbookUpdatedAt() (time.Time, error) {
	fi, err := os.Stat(s.path(HandbookFilename))
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime().UTC(), nil
}

// WriteHandbook replaces the handbook atomically.
func (s *FSStore) WriteHandbook(body string) error {
	return writeAtomic(s.path(HandbookFilename), []byte(body), 0o644)
}

// RemoveHandbook deletes the handbook. The restore handler runs it
// before unpacking an archive, so the archive's handbook is the one the
// restored org reads rather than one a fresh deployment's setup wrote.
func (s *FSStore) RemoveHandbook() error {
	if err := os.Remove(s.path(HandbookFilename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
