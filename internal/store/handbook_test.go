package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WriteHandbook writes handbook.md, and ReadHandbook and
// HandbookUpdatedAt read it back.
func TestHandbookWriteAndRead(t *testing.T) {
	s := mustStore(t)
	if _, err := s.ReadHandbook(); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadHandbook with none written: %v; want ErrNotFound", err)
	}
	if _, err := s.HandbookUpdatedAt(); !errors.Is(err, ErrNotFound) {
		t.Errorf("HandbookUpdatedAt with none written: %v; want ErrNotFound", err)
	}
	if err := s.WriteHandbook("# rules\n"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Root(), HandbookFilename))
	if err != nil || string(b) != "# rules\n" {
		t.Errorf("%s = %q, %v", HandbookFilename, b, err)
	}
	if got, err := s.ReadHandbook(); err != nil || got != "# rules\n" {
		t.Errorf("ReadHandbook = %q, %v", got, err)
	}
	if _, err := s.HandbookUpdatedAt(); err != nil {
		t.Errorf("HandbookUpdatedAt: %v", err)
	}
}

// A handbook proposal is stored under handbook_update and reads back.
func TestHandbookUpdateFrontmatterRoundTrip(t *testing.T) {
	src := "---\ntype: ceo_approval_request\ntitle: Handbook change\nfrom: chief-of-staff\nto: ceo\ndate: 2026-09-01T00:00:00Z\nhandbook_update:\n  body: |\n    # Rules\n---\n\nwhy\n"
	m, err := ParseMessage("proposal.md", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if m.HandbookUpdate == nil || m.HandbookUpdate.Body != "# Rules" {
		t.Errorf("HandbookUpdate = %+v", m.HandbookUpdate)
	}
	s := mustStore(t)
	m.Path = ""
	path, err := s.WriteMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\nhandbook_update:\n") {
		t.Errorf("written frontmatter:\n%s", b)
	}
	back, err := s.ReadMessage(path)
	if err != nil || back.HandbookUpdate == nil || back.HandbookUpdate.Body != "# Rules" {
		t.Errorf("round trip = %+v, %v", back.HandbookUpdate, err)
	}
}

// RemoveHandbook clears the handbook and is fine with none present.
func TestRemoveHandbook(t *testing.T) {
	s := mustStore(t)
	if err := s.RemoveHandbook(); err != nil {
		t.Fatalf("RemoveHandbook with nothing there: %v", err)
	}
	if err := s.WriteHandbook("new"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveHandbook(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadHandbook(); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadHandbook after RemoveHandbook: %v; want ErrNotFound", err)
	}
}
