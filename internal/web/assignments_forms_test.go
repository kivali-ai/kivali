package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The editor sends the log position it was built from. A save against
// a record that moved meanwhile (here: the assignee added a blocker) is
// refused rather than diffed against a stale picture and silently
// undone.
func TestStaleEditIsRefused(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Epic", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Dep", Assignee: "bob"}); err != nil {
		t.Fatal(err)
	}
	seq := getAssignment(t, srv, 1).Seq
	if seq != 1 {
		t.Fatalf("assignment seq = %d, want 1", seq)
	}
	// alice adds a blocker while the CEO has the editor open.
	if _, err := tr.Update(ctx, "alice", 1, assignments.UpdateInput{AddBlockedBy: []int{2}}); err != nil {
		t.Fatal(err)
	}
	title, assignee := "Epic, renamed", "alice"
	none := []int{}
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{
		Title: &title, Assignee: &assignee, WaitsOn: &none, Note: "clearer", Seq: &seq,
	})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "changed while you were editing") {
		t.Fatalf("stale save: code %d body %s", rr.Code, rr.Body.String())
	}
	iss, _ := srv.Store.ReadAssignment(1)
	if iss.Title != "Epic" || len(iss.BlockedBy) != 1 || len(iss.Log) != 2 {
		t.Fatalf("a stale save changed the record: %+v", iss)
	}
	// With the current seq the same save goes through, and only the
	// title moves.
	seq = getAssignment(t, srv, 1).Seq
	waits := []int{2}
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{
		Title: &title, Assignee: &assignee, WaitsOn: &waits, Note: "clearer", Seq: &seq,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("fresh save: code %d body %s", rr.Code, rr.Body.String())
	}
	iss, _ = srv.Store.ReadAssignment(1)
	if iss.Title != "Epic, renamed" || len(iss.BlockedBy) != 1 || len(iss.Log) != 3 {
		t.Fatalf("fresh save: %+v", iss)
	}
}

// An assignment whose assignee has been archived keeps that assignee:
// the detail still names them, and an unrelated save that sends the
// assignee back unchanged does not reassign it.
func TestArchivedAssigneeSurvivesAnUnrelatedEdit(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	if _, err := srv.tracker().Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Epic", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.ArchiveAgent("alice"); err != nil {
		t.Fatal(err)
	}
	a := getAssignment(t, srv, 1)
	if a.Facts.Assignee.Slug != "alice" {
		t.Fatalf("archived assignee missing from the detail: %+v", a.Facts.Assignee)
	}
	title, assignee := "Epic, clarified", "alice"
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{
		Title: &title, Assignee: &assignee, Note: "clearer", Seq: &a.Seq,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("save: code %d body %s", rr.Code, rr.Body.String())
	}
	iss, _ := srv.Store.ReadAssignment(1)
	if iss.Assignee != "alice" || len(iss.Log) != 2 || iss.Log[1].Op != assignments.OpAmended {
		t.Fatalf("an unrelated save reassigned the assignment: %+v", iss)
	}
}

// Closing an assignment with an open part is the tracker's refusal, a
// conflict that says why, and changes nothing.
func TestCloseWithAnOpenPartIsRefused(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Epic", Assignee: agent.CEOSlug}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Part", Assignee: "bob", Parent: 1}); err != nil {
		t.Fatal(err)
	}
	rr := homePostJSON(t, srv, "/api/v1/assignments/1/close", apitypes.AssignmentCloseRequest{Resolution: apitypes.AssignmentResolutionDropped, Outcome: "All of this, written out."})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "has open parts") {
		t.Fatalf("close with an open child: code %d body %s", rr.Code, rr.Body.String())
	}
	if iss, _ := srv.Store.ReadAssignment(1); !iss.Open() {
		t.Fatal("a refused close closed the assignment")
	}
}

// An assignment assigned to the CEO, answered, then reopened by its filer
// has two assignment events in the CEO's inbox; only the newest asks
// for anything.
func TestReopenAfterCloseLeavesOneNeedsActionCard(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "alice", assignments.CreateInput{Title: "Budget?", Assignee: agent.CEOSlug}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Close(ctx, agent.CEOSlug, 1, assignments.ResolutionDone, "$40k."); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Reopen(ctx, "alice", 1, "Per quarter or per year?"); err != nil {
		t.Fatal(err)
	}
	view, err := srv.buildCEOInbox(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Needs) != 1 || view.Needs[0].Request.Assignment == nil || view.Needs[0].Request.Assignment.Op != string(assignments.OpReopened) {
		t.Fatalf("needs = %+v, want the one reopen card", view.Needs)
	}
	if srv.pendingCEOInbox() != 1 {
		t.Fatalf("sidebar count = %d", srv.pendingCEOInbox())
	}
}

// A delivery is the message and nothing else. What the recipient
// holds is the wake note's business (TestWakeNoteListsOpenAssignmentsOncePerWake):
// once per wake, never on a delivery, so an agent released five
// messages, or handed one mid-turn, is not told five times.
func TestDeliveriesDoNotRepeatWhatTheAgentHolds(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	if _, err := srv.tracker().Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Work", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	notice := func(to string, when time.Time) {
		t.Helper()
		msg := store.Message{Type: store.MsgNotice, Title: "FYI", From: "bob", To: store.Recipients{to}, Date: when, Body: "Heads up."}
		path, err := srv.Store.WriteMessage(msg)
		if err != nil {
			t.Fatal(err)
		}
		msg.Path = path
		if _, err := srv.Messenger.Route(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	notice("alice", time.Now().UTC().Add(time.Minute))
	notice("alice", time.Now().UTC().Add(2*time.Minute))
	if _, err := srv.Messenger.ReleaseAll(ctx, nil, srv.deliveryHooks()); err != nil {
		t.Fatal(err)
	}
	alice := readChat(t, srv, "alice")
	// The assignment event and the two notices, none carrying the list.
	if len(alice) != 3 {
		t.Fatalf("alice chat = %+v", alice)
	}
	for _, m := range alice {
		if m.Kind != "inbox_delivery" || strings.Contains(m.Content, "open assignments") {
			t.Errorf("delivery repeats what alice holds: %+v", m)
		}
	}
}
