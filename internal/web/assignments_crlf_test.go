package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Browsers submit a textarea with CRLF line endings, and one that ends
// in a newline ends in "\r\n". updateAssignmentAsCEO normalises before it
// compares, so an untouched description is not written back as an
// amendment (with a wake, or a "note is required" refusal when the
// assignee is someone else).
func TestCEOEditWithCRLFDoesNotAmendAnUnchangedDescription(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	if _, err := srv.tracker().Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Epic", Body: "line one\nline two", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	// Every field as the editor sends it: CRLF endings, a trailing
	// newline, and no note because the CEO changed nothing.
	title, body, assignee := "Epic", "line one\r\nline two\r\n", "alice"
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{
		Title: &title, DescriptionMD: &body, Assignee: &assignee,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("an unchanged description was refused as an amendment: %d %s", rr.Code, rr.Body.String())
	}
	iss, _ := srv.Store.ReadAssignment(1)
	if len(iss.Log) != 1 || iss.Body != "line one\nline two" {
		t.Fatalf("an unchanged description was amended: log=%d body=%q", len(iss.Log), iss.Body)
	}
}
