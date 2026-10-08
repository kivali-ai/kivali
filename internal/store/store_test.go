package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestNewCreatesLayout(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wantDirs := []string{"agents", "agents/_archived", "messages", "project_files"}
	for _, sub := range wantDirs {
		p := filepath.Join(dir, sub)
		if !isDir(p) {
			t.Errorf("missing dir: %s", p)
		}
	}
	if s.Root() != dir {
		t.Errorf("Root = %q, want %q", s.Root(), dir)
	}
}

func TestNewIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir); err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := New(dir); err != nil {
		t.Fatalf("second New: %v", err)
	}
}

func TestHandbookRoundtrip(t *testing.T) {
	s := mustStore(t)
	if _, err := s.ReadHandbook(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on empty store, got %v", err)
	}
	body := "# Handbook\n\nhello world.\n"
	if err := s.WriteHandbook(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.ReadHandbook()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != body {
		t.Errorf("Read = %q, want %q", got, body)
	}
}
