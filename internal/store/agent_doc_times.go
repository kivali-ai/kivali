package store

import (
	"errors"
	"io/fs"
	"os"
	"time"
)

// When each of an agent's three documents last changed: the About tab
// says "updated 2 days ago" beside each one. The file's modification
// time is the record; nothing else stamps these writes.

// RoleUpdatedAt is when role.md was last written, or ErrNotFound when
// there is none.
func (s *FSStore) RoleUpdatedAt(slug string) (time.Time, error) {
	return s.agentFileModTime(slug, "role.md")
}

// AgentMemoryUpdatedAt is when the agent memory was last written, or
// ErrNotFound before the first write.
func (s *FSStore) AgentMemoryUpdatedAt(slug string) (time.Time, error) {
	return s.agentFileModTime(slug, agentMemoryFilename)
}

// AgentHabitsUpdatedAt is when the habits were last written, or
// ErrNotFound before the first one.
func (s *FSStore) AgentHabitsUpdatedAt(slug string) (time.Time, error) {
	return s.agentFileModTime(slug, AgentMemoryHabitsFilename)
}

func (s *FSStore) agentFileModTime(slug, name string) (time.Time, error) {
	info, err := os.Stat(s.path("agents", slug, name))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime().UTC(), nil
}
