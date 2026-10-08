package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
)

func sampleAssignment(id int) *assignments.Assignment {
	ts := time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)
	return &assignments.Assignment{
		ID: id, Title: "Assignment " + assignments.Ref(id), Status: assignments.StatusOpen,
		Assignee: "alice", Creator: "cos", Created: ts, Updated: ts,
		Log:  []assignments.Entry{{Seq: int64(id), TS: ts, By: "cos", Op: assignments.OpCreated, To: "alice"}},
		Body: "spec " + assignments.Ref(id),
	}
}

func TestAssignmentWriteReadList(t *testing.T) {
	s := mustStore(t)
	if got, err := s.ListAssignments(); err != nil || got != nil {
		t.Fatalf("empty list = %v, %v", got, err)
	}
	if _, err := s.ReadAssignment(1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing assignment err = %v", err)
	}
	for _, id := range []int{3, 1, 2} {
		if err := s.WriteAssignment(sampleAssignment(id)); err != nil {
			t.Fatalf("write %d: %v", id, err)
		}
	}
	got, err := s.ListAssignments()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != 1 || got[2].ID != 3 {
		t.Fatalf("list ids = %v", got)
	}
	one, err := s.ReadAssignment(2)
	if err != nil {
		t.Fatal(err)
	}
	if one.Body != "spec #2" || one.Assignee != "alice" {
		t.Fatalf("read back = %+v", one)
	}
	set, err := s.ReadAssignmentSet()
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 3 || set.MaxID() != 3 || set.MaxSeq() != 3 {
		t.Fatalf("set = %d assignments, max id %d, max seq %d", set.Len(), set.MaxID(), set.MaxSeq())
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "assignments", "000002.md")); err != nil {
		t.Fatalf("file layout: %v", err)
	}
}

func TestAssignmentWriteRefusesInvalidAndListRefusesCorrupt(t *testing.T) {
	s := mustStore(t)
	bad := sampleAssignment(1)
	bad.Status = "blocked"
	if err := s.WriteAssignment(bad); err == nil {
		t.Fatal("invalid record written")
	}
	if err := s.WriteAssignment(sampleAssignment(1)); err != nil {
		t.Fatal(err)
	}
	// A hand-corrupted file pauses the tracker rather than being
	// skipped: skipping would hand its id to the next create.
	if err := os.WriteFile(filepath.Join(s.Root(), "assignments", "000002.md"), []byte("---\nid: 2\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.ListAssignments()
	if err == nil || !strings.Contains(err.Error(), "000002.md") || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("corrupt file err = %v", err)
	}
	// A file whose name disagrees with its id is refused too.
	if err := os.Rename(filepath.Join(s.Root(), "assignments", "000002.md"), filepath.Join(s.Root(), "assignments", "000009.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAssignment(9); err == nil {
		t.Fatal("id mismatch accepted")
	}
}

func TestPendingWakesLifecycle(t *testing.T) {
	s := mustStore(t)
	if got, err := s.ListPendingWakes(); err != nil || got != nil {
		t.Fatalf("empty = %v, %v", got, err)
	}
	at := time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)
	for _, seq := range []int64{20, 5} {
		p := PendingWakes{Seq: seq, Assignment: 1, By: "cos", At: at, Wakes: []assignments.Wake{{To: "alice", Op: assignments.OpCreated, Title: "t", Body: "b"}}}
		if err := s.WritePendingWakes(p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListPendingWakes()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 5 || got[1].Seq != 20 || got[1].Wakes[0].To != "alice" {
		t.Fatalf("pending = %+v", got)
	}
	if err := s.DeletePendingWakes(5); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePendingWakes(5); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	got, _ = s.ListPendingWakes()
	if len(got) != 1 || got[0].Seq != 20 {
		t.Fatalf("after delete = %+v", got)
	}
}

func TestSnapshotAssignmentTextHashMatchesTheLogEntry(t *testing.T) {
	s := mustStore(t)
	text := "Title\n\nprior body"
	sha, err := s.SnapshotAssignmentText(4, text)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(text))
	if sha != hex.EncodeToString(sum[:]) {
		t.Fatalf("attachment sha %s is not sha256 of the text", sha)
	}
}

func TestAssignmentEventMessageRoundTrip(t *testing.T) {
	s := mustStore(t)
	m := Message{
		Type: MsgAssignmentEvent, Title: "#4 assigned to you: T", From: "cos", To: Recipients{"alice"},
		Date:       time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC),
		Assignment: &AssignmentRef{ID: 4, Seq: 9, Op: "created"},
		Body:       "cos assigned you #4.",
	}
	path, err := s.WriteMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	back, err := s.ReadMessage(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Assignment == nil || back.Assignment.ID != 4 || back.Assignment.Seq != 9 || back.Assignment.Op != "created" {
		t.Fatalf("assignment ref = %+v", back.Assignment)
	}
	if back.Type.TakesReplies() {
		t.Fatal("an assignment event takes replies")
	}
	if back.DeliversToCEO() {
		t.Fatal("agent-bound assignment event reads as CEO-bound")
	}
	m.To = Recipients{"ceo"}
	if !m.DeliversToCEO() {
		t.Fatal("CEO-bound assignment event not recognised")
	}
	if !strings.Contains(path, "-assignment_event-cos--to--alice.md") {
		t.Fatalf("path = %s", path)
	}
}
