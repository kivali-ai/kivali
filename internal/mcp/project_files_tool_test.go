package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestListProjectFilesEmpty(t *testing.T) {
	s := mustStore(t)
	tool := ListProjectFilesTool(NewStoreProjectFilesLister(s))
	res, err := tool.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Errorf("empty catalog shouldn't be an error; got %v", res.Content)
	}
	if !strings.Contains(res.Content[0], "No project files uploaded yet") {
		t.Errorf("unexpected empty message: %s", res.Content[0])
	}
}

func TestListProjectFilesShowsSummaries(t *testing.T) {
	s := mustStore(t)
	// Seed three files with varying summary states.
	pf1, err := s.AddProjectFile("plan.md", strings.NewReader("# Q3 plan\nbody"))
	if err != nil {
		t.Fatalf("add plan: %v", err)
	}
	if err := s.SetCanonical(pf1.SHA, "original.md", "text/markdown"); err != nil {
		t.Fatalf("canonical plan: %v", err)
	}
	if err := s.SetProjectFileSummary(pf1.SHA, "Q3 go-to-market plan covering pricing and channel strategy."); err != nil {
		t.Fatalf("summary plan: %v", err)
	}

	pf2, err := s.AddProjectFile("nosummary.md", strings.NewReader("just uploaded, summarizer hasn't run"))
	if err != nil {
		t.Fatalf("add nosummary: %v", err)
	}
	if err := s.SetCanonical(pf2.SHA, "original.md", "text/markdown"); err != nil {
		t.Fatalf("canonical nosummary: %v", err)
	}
	// NO SetProjectFileSummary — simulates race between upload and Haiku.

	// A PNG with a summary — images are first-class in the listing
	// now. Canonical is empty (we don't extract text from images),
	// but the MIME type routes it into the main section with an
	// [image] marker.
	pf3, err := s.AddProjectFile("diagram.png", strings.NewReader("binary-bytes"))
	if err != nil {
		t.Fatalf("add image: %v", err)
	}
	// No SetCanonical — images never have canonical text. Summary
	// is attached directly (Haiku vision run on the original bytes).
	if err := s.SetProjectFileSummary(pf3.SHA, "Block diagram of the fence-break detector's analog front end."); err != nil {
		t.Fatalf("summary image: %v", err)
	}
	// Note: MIME on ProjectFile is set via SetCanonical; for a raw
	// PNG without SetCanonical it stays empty. Set it directly via
	// the internal path used by the real upload flow.
	if err := s.SetCanonical(pf3.SHA, "", "image/png"); err != nil {
		t.Fatalf("set image mime: %v", err)
	}

	// An unknown binary with no summary — unviewable section.
	pf4, err := s.AddProjectFile("blob.bin", strings.NewReader("\x00\x01\x02"))
	if err != nil {
		t.Fatalf("add blob: %v", err)
	}
	_ = pf4

	tool := ListProjectFilesTool(NewStoreProjectFilesLister(s))
	res, err := tool.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	body := res.Content[0]

	// Summary-bearing file renders its summary.
	if !strings.Contains(body, "Q3 go-to-market plan") {
		t.Errorf("summary missing from listing; got:\n%s", body)
	}
	// File without a summary yet renders "(no summary yet)".
	if !strings.Contains(body, "nosummary.md") || !strings.Contains(body, "(no summary yet)") {
		t.Errorf("missing-summary placeholder missing; got:\n%s", body)
	}
	// Image with a summary appears in the main listing with an [image] marker.
	if !strings.Contains(body, "diagram.png") {
		t.Errorf("image file missing from main listing; got:\n%s", body)
	}
	if !strings.Contains(body, "[image]") {
		t.Errorf("image marker missing; got:\n%s", body)
	}
	if !strings.Contains(body, "Block diagram of the fence-break detector") {
		t.Errorf("image summary missing; got:\n%s", body)
	}
	// Unknown binary goes to the 'other binary files' section.
	if !strings.Contains(body, "Opaque files") {
		t.Errorf("unviewable-binary section header missing; got:\n%s", body)
	}
	if !strings.Contains(body, "blob.bin") {
		t.Errorf("blob.bin missing from unviewable section; got:\n%s", body)
	}
	// diagram.png must NOT be in the "Other binary" section (it's
	// an image with a summary, first-class).
	lines := strings.Split(body, "\n")
	inUnviewable := false
	for _, ln := range lines {
		if strings.Contains(ln, "Opaque files") {
			inUnviewable = true
			continue
		}
		if inUnviewable && strings.Contains(ln, "diagram.png") {
			t.Errorf("diagram.png leaked into the 'other binary' section; should be first-class")
		}
	}
	// The browse pointer to file_view is in the preamble.
	if !strings.Contains(body, "file_view /files/project/<name>") {
		t.Errorf("browse pointer missing; got:\n%s", body)
	}
}
