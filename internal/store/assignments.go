package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
)

// The assignment tracker on disk. Core is the only writer; agents
// reach it through the assignment_* tools and the CEO through the Work
// board. See docs/developers/assignments.md §Storage.
//
//	assignments/000042.md          one file per assignment: front matter
//	                               is the record, the body is the description
//	assignments/pending/<seq>.json the wakes one change owes, written with
//	                            the assignment and deleted once routed,
//	                            so a crash between the two is repaired
//	                            at boot
//
// The files are the only state. Ids and log sequence numbers derive
// from them, so there is no counter to lose or to disagree with them.
const (
	AssignmentsDirName        = "assignments"
	assignmentsPendingDirName = "pending"
)

func assignmentFilename(id int) string { return fmt.Sprintf("%06d.md", id) }

// LockAssignments blocks until the caller owns the assignment
// read-modify-write lock. Every change to an assignment file is read →
// rules → write under it, so two changes cannot allocate the same id
// or seq. Pair with UnlockAssignments.
func (s *FSStore) LockAssignments() { s.assignmentsMu.Lock() }

// UnlockAssignments releases the lock acquired via LockAssignments.
func (s *FSStore) UnlockAssignments() { s.assignmentsMu.Unlock() }

// ReadAssignment reads one assignment, or ErrNotFound.
func (s *FSStore) ReadAssignment(id int) (*assignments.Assignment, error) {
	if id < 1 {
		return nil, ErrNotFound
	}
	p := s.path(AssignmentsDirName, assignmentFilename(id))
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	iss, err := assignments.Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", AssignmentsDirName, assignmentFilename(id), err)
	}
	if iss.ID != id {
		return nil, fmt.Errorf("%s/%s: file carries id %d", AssignmentsDirName, assignmentFilename(id), iss.ID)
	}
	return iss, nil
}

// ListAssignments reads every assignment, ascending by id. A file that does not
// parse fails the whole read rather than being skipped: a skipped
// assignment would silently unblock whatever waited on it, and its id
// would be handed to the next create, which would overwrite it. The
// error names the file so the CEO can fix it by hand.
func (s *FSStore) ListAssignments() ([]*assignments.Assignment, error) {
	dir := s.path(AssignmentsDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*assignments.Assignment
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSuffix(name, ".md"))
		if err != nil {
			return nil, fmt.Errorf("%s/%s: not an assignment file name", AssignmentsDirName, name)
		}
		iss, err := s.ReadAssignment(id)
		if err != nil {
			return nil, fmt.Errorf("assignment tracker paused until this file is fixed by hand: %w", err)
		}
		out = append(out, iss)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ReadAssignmentSet reads every assignment into a Set.
func (s *FSStore) ReadAssignmentSet() (*assignments.Set, error) {
	list, err := s.ListAssignments()
	if err != nil {
		return nil, err
	}
	return assignments.Build(list), nil
}

// WriteAssignment persists one assignment. The record is validated by Marshal
// before anything touches disk, and the write is atomic. A successful
// write fires onAssignmentWrite (if set), so every in-process subscriber —
// today the org snapshot — learns of the change without the caller
// having to notify.
func (s *FSStore) WriteAssignment(iss *assignments.Assignment) error {
	data, err := assignments.Marshal(iss)
	if err != nil {
		return err
	}
	dir := s.path(AssignmentsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, assignmentFilename(iss.ID)), data, 0o644); err != nil {
		return err
	}
	if s.onAssignmentWrite != nil {
		s.onAssignmentWrite()
	}
	return nil
}

// ClearAssignmentNudges records that the wake note has told the assignee,
// for each assignment, that it has children and no acceptance items
// (assignments.Set.Nudges). No log entry: the nudge is advice, not a change
// to the record. An assignment that cannot be read or is no longer nudged is
// skipped; every id is tried and the first write error is returned.
func (s *FSStore) ClearAssignmentNudges(ids []int) error {
	s.LockAssignments()
	defer s.UnlockAssignments()
	var first error
	for _, id := range ids {
		iss, err := s.ReadAssignment(id)
		if err != nil || !iss.Nudge {
			continue
		}
		iss.Nudge = false
		if err := s.WriteAssignment(iss); err != nil && first == nil {
			first = fmt.Errorf("clear nudge on %s: %w", iss.Ref(), err)
		}
	}
	return first
}

// SnapshotAssignmentText keeps the prior text of an amended assignment in the
// content-addressed attachment store and returns its SHA, which is
// what the log entry cites. Same hash the pure package computed, since
// both are sha256 of the same bytes.
func (s *FSStore) SnapshotAssignmentText(id int, text string) (string, error) {
	att, err := s.AddAttachmentFromText(fmt.Sprintf("assignment-%d-prior.md", id), text)
	if err != nil {
		return "", err
	}
	return att.SHA, nil
}

// PendingWakes is what one change owes and has not yet routed. Seq is
// the change's first log entry, which names the file.
type PendingWakes struct {
	Seq        int64              `json:"seq"`
	Assignment int                `json:"assignment"`
	By         string             `json:"by"`
	At         time.Time          `json:"at"`
	Wakes      []assignments.Wake `json:"wakes"`
}

func pendingWakesFilename(seq int64) string { return fmt.Sprintf("%012d.json", seq) }

// WritePendingWakes records the wakes a change owes, before routing.
func (s *FSStore) WritePendingWakes(p PendingWakes) error {
	dir := s.path(AssignmentsDirName, assignmentsPendingDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, pendingWakesFilename(p.Seq)), b, 0o644)
}

// ListPendingWakes returns every unrouted record, ascending by seq.
func (s *FSStore) ListPendingWakes() ([]PendingWakes, error) {
	dir := s.path(AssignmentsDirName, assignmentsPendingDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []PendingWakes
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var p PendingWakes
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("%s/%s/%s: %w", AssignmentsDirName, assignmentsPendingDirName, e.Name(), err)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// DeletePendingWakes removes a routed record. Missing is fine.
func (s *FSStore) DeletePendingWakes(seq int64) error {
	err := os.Remove(s.path(AssignmentsDirName, assignmentsPendingDirName, pendingWakesFilename(seq)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
