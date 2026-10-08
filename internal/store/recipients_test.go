package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRecipientsScalarStaysScalar is the on-disk compatibility
// contract in both directions: a single-recipient message is written
// as a bare `to: alice` — byte-identical to every message written
// before the list existed — and a multi-recipient one as a sequence.
func TestRecipientsScalarStaysScalar(t *testing.T) {
	s := mustStore(t)
	date := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	onePath, err := s.WriteMessage(Message{
		Type: MsgNotice, Title: "One", From: "cos",
		To: Recipients{"alice"}, Date: date, Body: "b",
	})
	if err != nil {
		t.Fatalf("write single: %v", err)
	}
	raw, _ := os.ReadFile(onePath)
	if !strings.Contains(string(raw), "to: alice\n") {
		t.Errorf("single recipient must serialize as a scalar; frontmatter:\n%s", raw)
	}

	manyPath, err := s.WriteMessage(Message{
		Type: MsgNotice, Title: "Many", From: "cos",
		To: Recipients{"alice", "bob"}, Date: date.Add(time.Second), Body: "b",
	})
	if err != nil {
		t.Fatalf("write multi: %v", err)
	}
	raw, _ = os.ReadFile(manyPath)
	if !strings.Contains(string(raw), "- alice") || !strings.Contains(string(raw), "- bob") {
		t.Errorf("multiple recipients must serialize as a sequence; frontmatter:\n%s", raw)
	}

	got, err := s.ReadMessage(manyPath)
	if err != nil {
		t.Fatalf("read multi: %v", err)
	}
	if strings.Join(got.To, ",") != "alice,bob" {
		t.Errorf("to = %v, want [alice bob]", got.To)
	}
}

// TestReadScalarRecipientFile: a message file whose `to` is a bare
// scalar loads, renders, and matches a To filter. The fixture is
// handwritten rather than round-tripped so the test would catch a
// codec change that only happens to be self-consistent.
func TestReadScalarRecipientFile(t *testing.T) {
	s := mustStore(t)
	dir := filepath.Join(s.Root(), "messages", "2026-04-18")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "20260418T120000.000000000Z-notice-alice--to--cos.md")
	const body = `---
type: notice
title: Q3 numbers
from: alice
to: chief-of-staff
date: 2026-04-18T12:00:00Z
---

Done.
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := s.ReadMessage(path)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if len(got.To) != 1 || got.To[0] != "chief-of-staff" {
		t.Errorf("to = %v, want [chief-of-staff]", got.To)
	}
	if got.To.String() != "chief-of-staff" {
		t.Errorf("String() = %q, want the bare slug", got.To.String())
	}
	if got.InReplyTo != "" {
		t.Errorf("a message without in_reply_to should load with no parent; got %q", got.InReplyTo)
	}

	matched, err := s.ListMessages(MessageFilter{To: "chief-of-staff"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(matched) != 1 {
		t.Errorf("a To filter should match a scalar recipient; got %d messages", len(matched))
	}
}

// TestListMessagesToFilterMatchesAnyRecipient: the filter asks "is
// this addressed to me", which for a notice means "am I one of the
// addressees" — not "am I the first one".
func TestListMessagesToFilterMatchesAnyRecipient(t *testing.T) {
	s := mustStore(t)
	if _, err := s.WriteMessage(Message{
		Type: MsgNotice, Title: "N", From: "cos",
		To: Recipients{"alice", "bob", "carol"}, Date: time.Now().UTC(), Body: "b",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, slug := range []string{"alice", "bob", "carol"} {
		got, err := s.ListMessages(MessageFilter{To: slug})
		if err != nil {
			t.Fatalf("list %s: %v", slug, err)
		}
		if len(got) != 1 {
			t.Errorf("filter To=%q matched %d messages, want 1", slug, len(got))
		}
	}
	got, _ := s.ListMessages(MessageFilter{To: "dave"})
	if len(got) != 0 {
		t.Errorf("a non-addressee must not match; got %d", len(got))
	}
}

// TestMessageFilenameBoundsRecipientList: the "--to--" segment stays
// deterministic and bounded however many agents a notice names, so
// MessagePath still re-derives the same path on an idempotent commit.
func TestMessageFilenameBoundsRecipientList(t *testing.T) {
	date := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	many := Recipients{}
	for _, slug := range []string{"market-analyst", "recruiter", "chief-of-staff", "data-engineer", "designer"} {
		many = append(many, slug)
	}
	d := Message{Type: MsgNotice, Title: "T", From: "cos", To: many, Date: date}
	name := messageFilename(d)
	if len(name) > 160 {
		t.Errorf("filename should stay bounded; got %d chars: %s", len(name), name)
	}
	if !strings.Contains(name, "market-analyst_and_4_more") {
		t.Errorf("long recipient lists should summarize by count; got %s", name)
	}
	if again := messageFilename(d); again != name {
		t.Errorf("filename must be deterministic; %s != %s", again, name)
	}

	short := Message{Type: MsgNotice, Title: "T", From: "cos", To: Recipients{"alice", "bob"}, Date: date}
	if n := messageFilename(short); !strings.Contains(n, "alice_bob") {
		t.Errorf("a short list should name every recipient; got %s", n)
	}
}

// TestWriteMessageRejectsEmptyRecipients guards the required-field
// check on the To slice: nil, empty, and a list holding a blank entry
// are all refused.
func TestWriteMessageRejectsEmptyRecipients(t *testing.T) {
	s := mustStore(t)
	cases := []struct {
		name string
		to   Recipients
	}{
		{"nil", nil},
		{"empty", Recipients{}},
		{"blank entry", Recipients{"alice", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.WriteMessage(Message{Type: MsgNotice, Title: "T", From: "cos", To: tc.to, Body: "b"})
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// TestTakesReplies is the one-line statement of the rule the parser
// and the messaging layer both enforce.
func TestTakesReplies(t *testing.T) {
	if MsgNotice.TakesReplies() {
		t.Error("a notice must not be repliable")
	}
	for _, mt := range []MessageType{MsgCEOApprovalRequest, MsgCEONotification, MsgCEOApprovalResponse} {
		if !mt.TakesReplies() {
			t.Errorf("%s should be repliable", mt)
		}
	}
}
