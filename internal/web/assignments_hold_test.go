package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The CEO pauses and resumes an assignment; everything under it and
// the board show the hold.
func TestCEOHoldPausesAndResumes(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Epic", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Create(ctx, "alice", assignments.CreateInput{Title: "Part", Assignee: "bob", Parent: 1}); err != nil {
		t.Fatal(err)
	}

	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/hold", apitypes.AssignmentHoldRequest{Held: true})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("hold without a note: code %d body %s", rr.Code, rr.Body.String())
	}
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/hold", apitypes.AssignmentHoldRequest{Held: true, Note: "Pause."})
	if rr.Code != http.StatusOK {
		t.Fatalf("hold: code %d body %s", rr.Code, rr.Body.String())
	}
	if iss, _ := srv.Store.ReadAssignment(1); !iss.Held {
		t.Fatalf("assignment not held: %+v", iss)
	}
	epic := getAssignment(t, srv, 1)
	if epic.State != apitypes.WorkItemStateOnHold || !epic.HeldHere || !strings.Contains(epic.Log[len(epic.Log)-1].Text, "put it on hold, with everything under it") {
		t.Fatalf("held assignment: state %s held_here %v log %+v", epic.State, epic.HeldHere, epic.Log)
	}
	part := getAssignment(t, srv, 2)
	if part.State != apitypes.WorkItemStateOnHold || part.HeldHere || part.HeldBy == nil {
		t.Fatalf("part under a hold: state %s held_here %v held_by %+v", part.State, part.HeldHere, part.HeldBy)
	}
	if r := getWork(t, srv).Readouts; r.OnHold != 2 {
		t.Fatalf("board readouts under a hold: %+v", r)
	}

	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/hold", apitypes.AssignmentHoldRequest{Held: false, Note: "Go."})
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: code %d body %s", rr.Code, rr.Body.String())
	}
	if iss, _ := srv.Store.ReadAssignment(1); iss.Held || iss.Log[len(iss.Log)-1].Op != assignments.OpResumed {
		t.Fatalf("assignment not resumed: %+v", iss)
	}
}
