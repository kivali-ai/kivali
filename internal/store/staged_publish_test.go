package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestStagedPublishRoundtrip locks in the basic on-disk shape of a
// staged-publish record: WriteStagedPublish persists the JSON,
// ReadStagedPublish returns the same fields, DeleteStagedPublish
// removes the file, and a second Read after delete surfaces
// ErrStagedPublishNotFound.
func TestStagedPublishRoundtrip(t *testing.T) {
	s := mustStore(t)
	rec := StagedPublish{
		StageID: "abcdef0123456789",
		Tool:    "publish_notice",
		From:    "alice",
		Message: Message{
			Type:  MsgNotice,
			Title: "Q3 analysis",
			From:  "alice",
			To:    Recipients{"bob"},
			Date:  time.Date(2026, 5, 11, 23, 16, 0, 0, time.UTC),
			Body:  "Focus on APAC.\n",
		},
		CreatedAt: time.Date(2026, 5, 11, 23, 16, 0, 0, time.UTC),
	}
	if err := s.WriteStagedPublish(rec); err != nil {
		t.Fatalf("WriteStagedPublish: %v", err)
	}
	got, err := s.ReadStagedPublish(rec.StageID)
	if err != nil {
		t.Fatalf("ReadStagedPublish: %v", err)
	}
	if got.StageID != rec.StageID || got.Tool != rec.Tool || got.From != rec.From {
		t.Errorf("envelope round-trip mismatch: %+v vs %+v", got, rec)
	}
	if got.Message.Title != rec.Message.Title || got.Message.To.Primary() != rec.Message.To.Primary() {
		t.Errorf("message round-trip mismatch: %+v vs %+v", got.Message, rec.Message)
	}
	if !got.Message.Date.Equal(rec.Message.Date) {
		t.Errorf("Date drift: got %v want %v", got.Message.Date, rec.Message.Date)
	}
	if err := s.DeleteStagedPublish(rec.StageID); err != nil {
		t.Errorf("DeleteStagedPublish: %v", err)
	}
	if _, err := s.ReadStagedPublish(rec.StageID); !errors.Is(err, ErrStagedPublishNotFound) {
		t.Errorf("post-delete Read: err = %v, want ErrStagedPublishNotFound", err)
	}
	// Delete is idempotent — second delete is a no-op.
	if err := s.DeleteStagedPublish(rec.StageID); err != nil {
		t.Errorf("second DeleteStagedPublish should be no-op, got %v", err)
	}
}

// TestStagedPublishRejectsBadIDs confirms the path-traversal guard
// in validStageID: dots / slashes / empty ids are rejected before any
// filesystem access happens.
func TestStagedPublishRejectsBadIDs(t *testing.T) {
	s := mustStore(t)
	for _, bad := range []string{"", "../etc/passwd", "foo/bar", "a.b"} {
		if _, err := s.ReadStagedPublish(bad); err == nil || strings.Contains(err.Error(), "not found") {
			t.Errorf("ReadStagedPublish(%q) accepted bad id: err=%v", bad, err)
		}
		if err := s.WriteStagedPublish(StagedPublish{StageID: bad}); err == nil {
			t.Errorf("WriteStagedPublish(%q) accepted bad id", bad)
		}
	}
}

// TestSweepStagedPublishesDropsExpired exercises the janitor's
// maxAge cutoff: records older than maxAge are removed, newer records
// are preserved, and the sweep returns the right count.
func TestSweepStagedPublishesDropsExpired(t *testing.T) {
	s := mustStore(t)
	now := time.Date(2026, 5, 11, 23, 16, 0, 0, time.UTC)
	// Two records past the cutoff, one inside, one right on the edge
	// (boundary handling: maxAge=1h, age==1h should be removed
	// because the check is Sub < maxAge).
	cases := []struct {
		id           string
		created      time.Time
		shouldRemove bool
	}{
		{id: "older1", created: now.Add(-2 * time.Hour), shouldRemove: true},
		{id: "older2", created: now.Add(-90 * time.Minute), shouldRemove: true},
		{id: "fresh", created: now.Add(-10 * time.Minute), shouldRemove: false},
		{id: "edge", created: now.Add(-1 * time.Hour), shouldRemove: true},
	}
	for _, c := range cases {
		rec := StagedPublish{StageID: c.id, Tool: "publish_notice", From: "alice", CreatedAt: c.created}
		if err := s.WriteStagedPublish(rec); err != nil {
			t.Fatalf("WriteStagedPublish %s: %v", c.id, err)
		}
	}
	removed, err := s.SweepStagedPublishes(now, time.Hour)
	if err != nil {
		t.Fatalf("SweepStagedPublishes: %v", err)
	}
	wantRemoved := 0
	for _, c := range cases {
		if c.shouldRemove {
			wantRemoved++
		}
	}
	if removed != wantRemoved {
		t.Errorf("removed = %d, want %d", removed, wantRemoved)
	}
	for _, c := range cases {
		_, err := s.ReadStagedPublish(c.id)
		if c.shouldRemove {
			if !errors.Is(err, ErrStagedPublishNotFound) {
				t.Errorf("%s should be removed, got %v", c.id, err)
			}
		} else {
			if err != nil {
				t.Errorf("%s should be kept, got %v", c.id, err)
			}
		}
	}
}

// TestSweepStagedPublishesEmptyDirNoError confirms a missing
// staged_publishes/ dir is a no-op, not an error. The janitor's
// first sweep on a fresh deploy must not fail.
func TestSweepStagedPublishesEmptyDirNoError(t *testing.T) {
	s := mustStore(t)
	removed, err := s.SweepStagedPublishes(time.Now(), time.Hour)
	if err != nil {
		t.Errorf("sweep on empty dir: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d on empty dir, want 0", removed)
	}
}

// TestMessageQueueCommittedStageIDsRingCap locks in the FIFO eviction
// behavior of the bounded slice: once CommittedStageIDsCap is full,
// subsequent appends drop the oldest entry.
func TestMessageQueueCommittedStageIDsRingCap(t *testing.T) {
	q := MessageQueue{}
	// Fill to cap + 1 so we observe one eviction.
	for i := 0; i < CommittedStageIDsCap+1; i++ {
		q.RecordCommittedStageID(stageIDForIdx(i))
	}
	if len(q.CommittedStageIDs) != CommittedStageIDsCap {
		t.Errorf("len after cap+1 inserts = %d, want %d", len(q.CommittedStageIDs), CommittedStageIDsCap)
	}
	if q.HasCommittedStageID(stageIDForIdx(0)) {
		t.Errorf("oldest entry should have been evicted")
	}
	if !q.HasCommittedStageID(stageIDForIdx(CommittedStageIDsCap)) {
		t.Errorf("newest entry should be present")
	}
	// Re-recording an existing id is a no-op (no growth).
	before := len(q.CommittedStageIDs)
	q.RecordCommittedStageID(stageIDForIdx(CommittedStageIDsCap))
	if len(q.CommittedStageIDs) != before {
		t.Errorf("re-record changed length to %d, want %d", len(q.CommittedStageIDs), before)
	}
}

func stageIDForIdx(i int) string {
	return "stage-" + nDigit(i, 6)
}

func nDigit(i, n int) string {
	out := make([]byte, n)
	for k := n - 1; k >= 0; k-- {
		out[k] = byte('0' + i%10)
		i /= 10
	}
	return string(out)
}
