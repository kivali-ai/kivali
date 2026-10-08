package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// assignmentsServer is e2eServer plus two agents to hold assignments.
func assignmentsServer(t *testing.T) *Server {
	t.Helper()
	srv := e2eServer(t)
	for _, a := range []store.Agent{
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Engineer", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role\n"); err != nil {
			t.Fatal(err)
		}
	}
	return srv
}

// The CEO edits, closes and reopens an assignment through the API. A
// reassign needs a note; every CEO change reaches its reader at once.
func TestCEOActsOnAnAssignment(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Epic", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	// Edit: only the changed fields are recorded. Reassign needs a note.
	bob := "bob"
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{Assignee: &bob})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "note is required") {
		t.Fatalf("reassign without note: code %d body %s", rr.Code, rr.Body.String())
	}
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{Assignee: &bob, Note: "bob owns it"})
	if rr.Code != http.StatusOK {
		t.Fatalf("reassign: code %d body %s", rr.Code, rr.Body.String())
	}
	iss, _ := srv.Store.ReadAssignment(1)
	if iss.Assignee != "bob" || len(iss.Log) != 2 || iss.Log[1].Op != assignments.OpAssigned {
		t.Fatalf("after reassign: %+v", iss)
	}
	// bob was told at once. alice's assignment was still queued for
	// release, so she never knew she held it: the queued wake is pulled
	// and no "stop work" follows it.
	if h := readChat(t, srv, "alice"); len(h) != 0 {
		t.Fatalf("alice chat = %+v", h)
	}
	if q, _ := srv.Store.ReadMessageQueue(); len(q.Agents["alice"].Inbox) != 0 {
		t.Fatalf("alice queue = %v", q.Agents["alice"].Inbox)
	}
	if h := readChat(t, srv, "bob"); len(h) != 1 || !strings.Contains(h[0].Content, "to you (from alice)") {
		t.Fatalf("bob chat = %+v", h)
	}
	// Close as done. The creator is woken, and because the CEO made
	// the change it reaches them at once.
	rr = homePostJSON(t, srv, "/api/v1/assignments/1/close", apitypes.AssignmentCloseRequest{Resolution: apitypes.AssignmentResolutionDone, Outcome: "Fine."})
	if rr.Code != http.StatusOK {
		t.Fatalf("close: code %d body %s", rr.Code, rr.Body.String())
	}
	if h := readChat(t, srv, "chief-of-staff"); len(h) != 1 || !strings.Contains(h[0].Content, "ceo closed #1 \"Epic\" as done.") {
		t.Fatalf("cos chat = %+v", h)
	}
	if a := getAssignment(t, srv, 1); a.State != apitypes.WorkItemStateClosed || a.Outcome == nil {
		t.Errorf("closed assignment = state %s outcome %+v", a.State, a.Outcome)
	}
	// Reopen with a note.
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/reopen", apitypes.AssignmentReopenRequest{Note: "Not fine."})
	if rr.Code != http.StatusOK {
		t.Fatalf("reopen: code %d body %s", rr.Code, rr.Body.String())
	}
	iss, _ = srv.Store.ReadAssignment(1)
	if !iss.Open() {
		t.Fatal("not reopened")
	}
	assertAPIError(t, workDo(t, srv, http.MethodGet, "/api/v1/assignments/99", nil), http.StatusNotFound)
}

func TestQuestionToTheCEOIsANeedAndClosingAnswersIt(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Work", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Create(ctx, "alice", assignments.CreateInput{Title: "Budget?", Body: "How much?", Assignee: agent.CEOSlug, Parent: 1}); err != nil {
		t.Fatal(err)
	}
	view, err := srv.buildCEOInbox(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Needs) != 1 || view.Needs[0].RequestKind != "assignment" || view.Needs[0].Assignment == nil || view.Needs[0].Assignment.ID != 2 {
		t.Fatalf("needs = %+v", view.Needs)
	}
	if srv.pendingCEOInbox() != 1 {
		t.Fatalf("needs count = %d", srv.pendingCEOInbox())
	}
	// Answering closes it, wakes alice with the answer, and the need
	// moves to history.
	rr := homePostJSON(t, srv, "/api/v1/assignments/2/close", apitypes.AssignmentCloseRequest{Resolution: apitypes.AssignmentResolutionDone, Outcome: "$40k."})
	if rr.Code != http.StatusOK {
		t.Fatalf("close: code %d body %s", rr.Code, rr.Body.String())
	}
	view, _ = srv.buildCEOInbox(1)
	if len(view.Needs) != 0 || len(view.History) != 1 {
		t.Fatalf("after answer: needs %d history %d", len(view.Needs), len(view.History))
	}
	h := readChat(t, srv, "alice")
	if len(h) != 1 || !strings.Contains(h[0].Content, "Outcome: $40k.") || !strings.Contains(h[0].Content, "#1 \"Work\" is now ready") {
		t.Fatalf("alice chat = %+v", h)
	}
	// An informational event to the CEO (their own filed assignment closed)
	// is history from the start: cos closes something the CEO filed.
	if _, err := tr.Create(ctx, agent.CEOSlug, assignments.CreateInput{Title: "For cos", Assignee: "chief-of-staff"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Close(ctx, "chief-of-staff", 3, assignments.ResolutionDone, "done"); err != nil {
		t.Fatal(err)
	}
	view, _ = srv.buildCEOInbox(1)
	if len(view.Needs) != 0 || len(view.History) != 2 {
		t.Fatalf("after informational event: needs %d history %d", len(view.Needs), len(view.History))
	}
}
