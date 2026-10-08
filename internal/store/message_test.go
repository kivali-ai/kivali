package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDocumentRoundtrip(t *testing.T) {
	s := mustStore(t)
	d := Message{
		Type:  MsgNotice,
		Title: "Draft Q3 analysis",
		From:  "chief-of-staff",
		To:    Recipients{"market-analyst"},
		Date:  time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC),
		Body:  "please do the thing\n",
	}
	path, err := s.WriteMessage(d)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(path, "2026-04-18") {
		t.Errorf("path = %q, want 2026-04-18 in it", path)
	}
	got, err := s.ReadMessage(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Title != d.Title || got.Type != d.Type || got.From != d.From || got.To.Primary() != d.To.Primary() {
		t.Errorf("meta mismatch: got %+v", got)
	}
	if strings.TrimSpace(got.Body) != strings.TrimSpace(d.Body) {
		t.Errorf("body = %q, want %q", got.Body, d.Body)
	}
}

func TestDocumentRoundtripWithAttachments(t *testing.T) {
	s := mustStore(t)
	d := Message{
		Type:  MsgNotice,
		Title: "Draft Q3 analysis",
		From:  "chief-of-staff",
		To:    Recipients{"market-analyst"},
		Date:  time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC),
		Body:  "please read attached spec\n",
		Attachments: []MessageAttachment{
			{SHA: "abc123", Name: "spec.md"},
			{SHA: "def456", Name: "inputs.csv"},
		},
	}
	path, err := s.WriteMessage(d)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.ReadMessage(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(got.Attachments))
	}
	if got.Attachments[0].SHA != "abc123" || got.Attachments[0].Name != "spec.md" {
		t.Errorf("att[0] = %+v", got.Attachments[0])
	}
	if got.Attachments[1].Name != "inputs.csv" {
		t.Errorf("att[1] = %+v", got.Attachments[1])
	}
}

func TestDocumentRequiredFields(t *testing.T) {
	s := mustStore(t)
	cases := []Message{
		{Title: "t", From: "a", To: Recipients{"b"}},
		{Type: MsgNotice, From: "a", To: Recipients{"b"}},
		{Type: MsgNotice, Title: "t", To: Recipients{"b"}},
		{Type: MsgNotice, Title: "t", From: "a"},
	}
	for i, d := range cases {
		if _, err := s.WriteMessage(d); err == nil {
			t.Errorf("case %d: expected error for %+v", i, d)
		}
	}
}

func TestDocumentFilenameCollisionFree(t *testing.T) {
	s := mustStore(t)
	// same second but different nanoseconds should produce unique names
	base := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		d := Message{
			Type:  MsgNotice,
			Title: "t",
			From:  "a",
			To:    Recipients{"b"},
			Date:  base.Add(time.Duration(i)),
			Body:  "x",
		}
		if _, err := s.WriteMessage(d); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	messages, err := s.ListMessages(MessageFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(messages) != 10 {
		t.Errorf("got %d messages, want 10", len(messages))
	}
}

func TestListDocumentsFilter(t *testing.T) {
	s := mustStore(t)
	now := time.Now().UTC()
	messages := []Message{
		{Type: MsgNotice, Title: "a", From: "cos", To: Recipients{"alice"}, Date: now, Body: "x"},
		{Type: MsgNotice, Title: "b", From: "cos", To: Recipients{"bob"}, Date: now.Add(time.Second), Body: "x"},
		{Type: MsgCEONotification, Title: "c", From: "alice", To: Recipients{"cos"}, Date: now.Add(2 * time.Second), Body: "x"},
	}
	for _, d := range messages {
		if _, err := s.WriteMessage(d); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	all, err := s.ListMessages(MessageFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all = %d", len(all))
	}
	byTo, err := s.ListMessages(MessageFilter{To: "alice"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(byTo) != 1 || byTo[0].Title != "a" {
		t.Errorf("byTo = %+v", byTo)
	}
	byType, err := s.ListMessages(MessageFilter{Type: MsgCEONotification})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(byType) != 1 || byType[0].Title != "c" {
		t.Errorf("byType = %+v", byType)
	}
	byFrom, err := s.ListMessages(MessageFilter{From: "cos"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(byFrom) != 2 {
		t.Errorf("byFrom = %d", len(byFrom))
	}
}

// TestMessageDateBucketUTC locks in the bucketing rule: WriteMessage
// uses the UTC date of d.Date, NOT the local-TZ date. A message
// published at 2026-04-27T23:30:00Z buckets to 2026-04-27 even when
// the writer's local TZ would push it into the next day.
func TestMessageDateBucketUTC(t *testing.T) {
	s := mustStore(t)
	// Construct in a non-UTC zone (Asia/Tokyo, +09:00). 2026-04-27T23:30Z
	// is 2026-04-28T08:30 in Tokyo — bucket must still be the UTC day.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("Asia/Tokyo unavailable in this build: %v", err)
	}
	when := time.Date(2026, 4, 27, 23, 30, 0, 0, time.UTC).In(tokyo)
	if got := MessageDateBucket(when); got != "2026-04-27" {
		t.Errorf("MessageDateBucket(%s) = %q, want 2026-04-27", when, got)
	}
	d := Message{
		Type:  MsgNotice,
		Title: "t",
		From:  "a",
		To:    Recipients{"b"},
		Date:  when,
		Body:  "x",
	}
	path, err := s.WriteMessage(d)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(path, "2026-04-27") {
		t.Errorf("path = %q, want UTC bucket 2026-04-27", path)
	}
	if strings.Contains(path, "2026-04-28") {
		t.Errorf("path = %q, must not bucket by local-TZ date 2026-04-28", path)
	}
}

// TestMoveToRedacted exercises the redact-relocation primitive: the
// file moves from messages/<bucket>/foo.md to
// messages/.redacted/<bucket>/foo.md, ListMessages stops finding it
// (dot-prefix subdir is skipped), and the file remains readable at its
// new path so the audit trail survives.
func TestMoveToRedacted(t *testing.T) {
	s := mustStore(t)
	when := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	abs, err := s.WriteMessage(Message{
		Type: MsgNotice, Title: "t1", From: "alice", To: Recipients{"bob"},
		Date: when, Body: "body",
	})
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	newPath, err := s.MoveToRedacted(abs)
	if err != nil {
		t.Fatalf("MoveToRedacted: %v", err)
	}
	wantDir := filepath.Join(s.Root(), "messages", ".redacted", "2026-04-18")
	if filepath.Dir(newPath) != wantDir {
		t.Errorf("redacted path dir = %q, want %q", filepath.Dir(newPath), wantDir)
	}

	// Old path is gone.
	if _, err := s.ReadMessage(abs); err == nil {
		t.Errorf("ReadMessage at old path should fail; file was supposed to move")
	}
	// New path round-trips.
	got, err := s.ReadMessage(newPath)
	if err != nil {
		t.Fatalf("ReadMessage at new path: %v", err)
	}
	if got.Title != "t1" {
		t.Errorf("redacted file body lost; got title %q", got.Title)
	}

	// ListMessages must skip the .redacted subdir — that's the whole
	// point. Dot-prefix rule is shared with archive/reset-backup
	// scopes, so this guards the layering as much as the redact case.
	all, err := s.ListMessages(MessageFilter{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("ListMessages must not return redacted files; got %+v", all)
	}
}

// TestMoveToRedactedRejectsOutsideMessages: defense-in-depth on the
// path argument. Caller must pass a path under messages/; anything
// else is a programmer error and should fail loud rather than silently
// move a file out of an unrelated tree.
func TestMoveToRedactedRejectsOutsideMessages(t *testing.T) {
	s := mustStore(t)
	bad := filepath.Join(s.Root(), "agents", "alice", "role.md")
	if _, err := s.MoveToRedacted(bad); err == nil {
		t.Errorf("expected error moving non-messages/ path; got nil")
	}
}
