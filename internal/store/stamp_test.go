package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// longAgo is a modification time well outside stampSettle.
var longAgo = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

func touch(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countOrFail(t *testing.T, s *FSStore) int {
	t.Helper()
	n, err := s.CountMessages()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFileStampMatches(t *testing.T) {
	s := newStampStore(t)
	path := s.path("agents", "alice", "chat.jsonl")

	missing := s.ChatHistoryStamp("alice")
	if !missing.Matches(s.ChatHistoryStamp("alice")) {
		t.Errorf("a missing file's stamp should match itself")
	}

	writeFile(t, path, "one\n")
	if missing.Matches(s.ChatHistoryStamp("alice")) {
		t.Errorf("creating the file kept the stamp")
	}
	// Just written: not settled, so never trusted, even unchanged.
	fresh := s.ChatHistoryStamp("alice")
	if fresh.Matches(s.ChatHistoryStamp("alice")) {
		t.Errorf("an unsettled stamp was trusted")
	}

	touch(t, path, longAgo)
	settled := s.ChatHistoryStamp("alice")
	if !settled.Matches(s.ChatHistoryStamp("alice")) {
		t.Errorf("a settled, unchanged file did not match")
	}
	// An append changes the size even when the time lands on the old tick.
	writeFile(t, path, "one\ntwo\n")
	touch(t, path, longAgo)
	if settled.Matches(s.ChatHistoryStamp("alice")) {
		t.Errorf("a grown file matched")
	}
}

func TestCountMessages(t *testing.T) {
	s := newStampStore(t)
	if n := countOrFail(t, s); n != 0 {
		t.Fatalf("no messages/ yet: %d", n)
	}
	root := s.path("messages")
	writeFile(t, filepath.Join(root, "2026-09-01", "a.md"), "a")
	writeFile(t, filepath.Join(root, "2026-09-01", "b.md"), "b")
	writeFile(t, filepath.Join(root, "2026-09-01", "notes.txt"), "not a message")
	writeFile(t, filepath.Join(root, "2026-09-02", "nested", "c.md"), "c")
	writeFile(t, filepath.Join(root, ".redacted", "2026-09-01", "gone.md"), "skipped")
	writeFile(t, filepath.Join(root, "2026-09-02", ".archived", "old.md"), "skipped")
	if n := countOrFail(t, s); n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}

	// Settled directories that did not change are not listed again.
	for _, d := range []string{root, filepath.Join(root, "2026-09-01"), filepath.Join(root, "2026-09-02"), filepath.Join(root, "2026-09-02", "nested")} {
		touch(t, d, longAgo)
	}
	countOrFail(t, s) // remembers the settled stamps
	before := s.msgCounts.listed
	if n := countOrFail(t, s); n != 3 || s.msgCounts.listed != before {
		t.Errorf("unchanged tree: count %d, %d listings", n, s.msgCounts.listed-before)
	}

	// A new message moves its bucket's stamp: that bucket alone is listed.
	bucket := filepath.Join(root, "2026-09-01")
	writeFile(t, filepath.Join(bucket, "d.md"), "d")
	before = s.msgCounts.listed
	if n := countOrFail(t, s); n != 4 || s.msgCounts.listed != before+1 {
		t.Errorf("after a new message: count %d, %d listings", n, s.msgCounts.listed-before)
	}

	// A new bucket, and a removed one.
	writeFile(t, filepath.Join(root, "2026-09-03", "e.md"), "e")
	if n := countOrFail(t, s); n != 5 {
		t.Errorf("after a new bucket: %d, want 5", n)
	}
	if err := os.RemoveAll(filepath.Join(root, "2026-09-02")); err != nil {
		t.Fatal(err)
	}
	if n := countOrFail(t, s); n != 4 {
		t.Errorf("after removing a bucket: %d, want 4", n)
	}
	if _, ok := s.msgCounts.dirs[filepath.Join(root, "2026-09-02", "nested")]; ok {
		t.Errorf("a removed directory stayed remembered")
	}

	// ForgetReadCaches: the next count lists everything again.
	touch(t, root, longAgo)
	touch(t, bucket, longAgo)
	countOrFail(t, s)
	s.ForgetReadCaches()
	before = s.msgCounts.listed
	if n := countOrFail(t, s); n != 4 || s.msgCounts.listed == before {
		t.Errorf("after forgetting: count %d, %d listings", n, s.msgCounts.listed-before)
	}
}

func newStampStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
