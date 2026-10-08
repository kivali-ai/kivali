package store

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestProjectFileAddAndList(t *testing.T) {
	s := mustStore(t)
	pf, err := s.AddProjectFile("biz-plan.md", strings.NewReader("# Biz Plan\n"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if pf.SHA == "" {
		t.Error("SHA empty")
	}
	if pf.OriginalName != "biz-plan.md" {
		t.Errorf("OriginalName = %q", pf.OriginalName)
	}
	if pf.OriginalExt != ".md" {
		t.Errorf("OriginalExt = %q", pf.OriginalExt)
	}
	if pf.Size == 0 {
		t.Error("Size zero")
	}
	list, err := s.ListProjectFiles()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
}

func TestProjectFileOpenOriginal(t *testing.T) {
	s := mustStore(t)
	body := "hello world"
	pf, err := s.AddProjectFile("x.txt", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	r, err := s.OpenOriginal(pf.SHA)
	if err != nil {
		t.Fatalf("OpenOriginal: %v", err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(b) != body {
		t.Errorf("body = %q, want %q", b, body)
	}
}

func TestProjectFileDuplicateUploadSameBytes(t *testing.T) {
	s := mustStore(t)
	a, err := s.AddProjectFile("x.txt", strings.NewReader("same"))
	if err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	b, err := s.AddProjectFile("x.txt", strings.NewReader("same"))
	if err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	if a.SHA != b.SHA {
		t.Errorf("shas differ: %q vs %q", a.SHA, b.SHA)
	}
	list, _ := s.ListProjectFiles()
	if len(list) != 1 {
		t.Errorf("list = %d, want 1", len(list))
	}
}

func TestSetCanonical(t *testing.T) {
	s := mustStore(t)
	pf, err := s.AddProjectFile("x.docx", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.SetCanonical(pf.SHA, "canonical.pdf", "application/pdf"); err != nil {
		t.Fatalf("SetCanonical: %v", err)
	}
	got, err := s.GetProjectFile(pf.SHA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CanonicalName != "canonical.pdf" || got.MIME != "application/pdf" {
		t.Errorf("canonical = %q / %q", got.CanonicalName, got.MIME)
	}
}

func TestOpenCanonicalMissing(t *testing.T) {
	s := mustStore(t)
	pf, err := s.AddProjectFile("x.txt", strings.NewReader("hi"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := s.OpenCanonical(pf.SHA); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRemoveProjectFile(t *testing.T) {
	s := mustStore(t)
	pf, err := s.AddProjectFile("x.txt", strings.NewReader("bye"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.RemoveProjectFile(pf.SHA); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if list, _ := s.ListProjectFiles(); len(list) != 0 {
		t.Errorf("list = %d", len(list))
	}
	if _, err := s.GetProjectFile(pf.SHA); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
}
