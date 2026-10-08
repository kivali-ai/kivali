package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// workDo serves one request through Work's routes behind the API's
// header and same-origin middleware, as wireAPIRoutes mounts them.
func workDo(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPIWorkRoutes(mux)
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req = httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("content-type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	rr := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rr, req)
	return rr
}

func getWork(t *testing.T, srv *Server) apitypes.WorkBoard {
	t.Helper()
	rr := workDo(t, srv, http.MethodGet, "/api/v1/work", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/work: code %d body %s", rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.WorkBoard](t, rr)
}

func getAssignment(t *testing.T, srv *Server, id int) apitypes.Assignment {
	t.Helper()
	rr := workDo(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/assignments/%d", id), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assignments/%d: code %d body %s", id, rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Assignment](t, rr)
}

// workFixture builds, through the tracker and on a fake clock ending
// at goalsNow, one goal with a part in every column:
//
//	#1  goal "Ship v3" (you), Done when: docs written,
//	    release notes, test plan
//	#2  under #1, closed done ten days ago: off the board
//	#3  top-level "Fix the login redirect" (bob), ready; not a goal
//	#4  under #1, "Write the docs" (alice), closed done two days ago,
//	    meets "docs written"
//	#5  under #1, "Release notes" (bob), claims "release notes", waits
//	    on #3: blocked
//	#6  under #1, "Screenshots" (alice), on hold by you
//	#7  under #1, "Migration" (bob), with open part #8: moving
//	#8  under #7, "Schema" (alice), ready
//	#9  under #1, "Changelog" (alice), ready
//	#10 top-level goal "Old goal" with a condition, closed as done
//	    two days ago
//
// "test plan" is unclaimed.
func workFixture(t *testing.T) (*Server, time.Time) {
	t.Helper()
	srv := assignmentsServer(t)
	fc := clock.NewFakeAt(goalsNow.Add(-10 * 24 * time.Hour))
	srv.Clock = fc
	tr := srv.tracker()
	tr.Now = fc.Now
	ctx := context.Background()
	create := func(in assignments.CreateInput) {
		t.Helper()
		if _, err := tr.Create(ctx, agent.CEOSlug, in); err != nil {
			t.Fatalf("create %q: %v", in.Title, err)
		}
	}
	closeDone := func(id int) {
		t.Helper()
		if err := srv.closeAssignmentAsCEO(ctx, id, "done", "Done."); err != nil {
			t.Fatalf("close #%d: %v", id, err)
		}
	}
	create(assignments.CreateInput{Title: "Ship v3", Assignee: agent.CEOSlug, Acceptance: []string{"docs written", "release notes", "test plan"}})
	create(assignments.CreateInput{Title: "Old sketch", Assignee: "alice", Parent: 1})
	closeDone(2)
	fc.Advance(8 * 24 * time.Hour)
	create(assignments.CreateInput{Title: "Fix the login redirect", Assignee: "bob"})
	create(assignments.CreateInput{Title: "Write the docs", Assignee: "alice", Parent: 1, Satisfies: []string{"docs written"}})
	closed := fc.Now().UTC()
	closeDone(4)
	create(assignments.CreateInput{Title: "Release notes", Assignee: "bob", Parent: 1, Satisfies: []string{"release notes"}, BlockedBy: []int{3}})
	create(assignments.CreateInput{Title: "Screenshots", Assignee: "alice", Parent: 1})
	if err := srv.holdAssignmentAsCEO(ctx, 6, true, "Wait for the fab."); err != nil {
		t.Fatal(err)
	}
	create(assignments.CreateInput{Title: "Migration", Assignee: "bob", Parent: 1})
	create(assignments.CreateInput{Title: "Schema", Assignee: "alice", Parent: 7})
	create(assignments.CreateInput{Title: "Changelog", Assignee: "alice", Parent: 1})
	create(assignments.CreateInput{Title: "Old goal", Assignee: "bob", Acceptance: []string{"shipped"}})
	if err := srv.closeAssignmentAsCEO(ctx, 10, "dropped", "Not needed."); err != nil {
		t.Fatal(err)
	}
	fc.Advance(2 * 24 * time.Hour)
	return srv, closed
}

func ids(items []apitypes.WorkItem) []int {
	out := []int{}
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestAPIWorkBoardByGoal(t *testing.T) {
	srv, closedAt := workFixture(t)
	board := getWork(t, srv)

	wantReadouts := apitypes.WorkReadouts{Open: 7, Ready: 3, Blocked: 1, OnHold: 1, ClosedWeek: 2}
	if board.Readouts != wantReadouts {
		t.Errorf("readouts = %+v, want %+v", board.Readouts, wantReadouts)
	}
	if len(board.Goals) != 1 {
		t.Fatalf("goals = %+v, want only #1", board.Goals)
	}
	g := board.Goals[0]
	if g.ID != 1 || g.Title != "Ship v3" || g.Owner != (apitypes.PersonRef{Slug: "ceo", Name: "You"}) || g.Done != 1 || g.Total != 3 || g.State != apitypes.WorkItemStateMoving {
		t.Errorf("goal header = %+v", g)
	}
	if got := ids(g.LookBack); !reflect.DeepEqual(got, []int{4}) {
		t.Errorf("look_back = %v, want [4] (#2 closed ten days ago)", got)
	}
	if lb := g.LookBack[0]; lb.State != apitypes.WorkItemStateClosed || lb.ClosedAt == nil || !lb.ClosedAt.Equal(closedAt) ||
		lb.Resolution == nil || *lb.Resolution != apitypes.AssignmentResolutionDone || lb.Owner != (apitypes.PersonRef{Slug: "alice", Name: "Analyst"}) {
		t.Errorf("look_back[0] = %+v", lb)
	}
	if got := ids(g.Current); !reflect.DeepEqual(got, []int{5, 6, 7}) {
		t.Errorf("current = %v, want [5 6 7]", got)
	}
	blocked, held, moving := g.Current[0], g.Current[1], g.Current[2]
	engineer := apitypes.PersonRef{Slug: "bob", Name: "Engineer"}
	if blocked.State != apitypes.WorkItemStateBlocked || blocked.Why == nil || *blocked.Why != "Waiting on #3 Fix the login redirect, assigned to Engineer" ||
		!reflect.DeepEqual(blocked.WaitingOn, []apitypes.AssignmentLink{{ID: 3, Title: "Fix the login redirect", State: apitypes.WorkItemStateReady, Owner: engineer}}) || blocked.HeldBy != nil {
		t.Errorf("blocked item = %+v", blocked)
	}
	// #5 claims one of #1's conditions but declares none of its own.
	if blocked.Acceptance != nil {
		t.Errorf("blocked item acceptance = %+v, want none", blocked.Acceptance)
	}
	if held.State != apitypes.WorkItemStateOnHold || held.Why == nil || *held.Why != "On hold by you" ||
		held.HeldBy == nil || held.HeldBy.Slug != agent.CEOSlug || len(held.WaitingOn) != 0 {
		t.Errorf("held item = %+v", held)
	}
	if moving.State != apitypes.WorkItemStateMoving || moving.Why != nil || moving.Owner.Slug != "bob" {
		t.Errorf("moving item = %+v", moving)
	}
	if got := ids(g.LookForward); !reflect.DeepEqual(got, []int{8, 9}) {
		t.Errorf("look_forward = %v, want [8 9]", got)
	}
	if !reflect.DeepEqual(g.Unclaimed, []apitypes.UnclaimedCondition{{Name: "test plan"}}) {
		t.Errorf("unclaimed = %+v", g.Unclaimed)
	}
	if len(board.ClosedGoals) != 1 || board.ClosedGoals[0].ID != 10 || board.ClosedGoals[0].Resolution != apitypes.AssignmentResolutionDropped ||
		board.ClosedGoals[0].Owner.Slug != "bob" {
		t.Errorf("closed_goals = %+v", board.ClosedGoals)
	}
}

// The board and an assignment read the assignment set the snapshot caches:
// no reread until a tracker write bumps the assignments version, and a
// write is seen on the next read.
func TestAPIWorkReadsTheCachedAssignmentSet(t *testing.T) {
	srv, _ := workFixture(t)
	getWork(t, srv)
	cached := srv.assignmentCache.set
	if cached == nil {
		t.Fatal("GET /api/v1/work did not fill the assignment set cache")
	}
	getAssignment(t, srv, 5)
	getWork(t, srv)
	if srv.assignmentCache.set != cached {
		t.Error("the assignment set was reread with no tracker write in between")
	}

	in, err := srv.tracker().Create(context.Background(), agent.CEOSlug, assignments.CreateInput{Title: "Late addition", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if got := getAssignment(t, srv, in.Assignment.ID); got.Title != "Late addition" {
		t.Errorf("after a write: assignment #%d = %q", in.Assignment.ID, got.Title)
	}
	if srv.assignmentCache.set == cached {
		t.Error("a tracker write did not refresh the cached assignment set")
	}
}

func TestAPIWorkBoardEmpty(t *testing.T) {
	srv := assignmentsServer(t)
	board := getWork(t, srv)
	if board.Readouts != (apitypes.WorkReadouts{}) || len(board.Goals) != 0 || len(board.ClosedGoals) != 0 {
		t.Errorf("empty board = %+v", board)
	}
}

func TestAPIWorkBoardHeldUnderAnAncestorSaysWhere(t *testing.T) {
	srv, _ := workFixture(t)
	// Holding #7 covers #8 under it.
	if err := srv.holdAssignmentAsCEO(context.Background(), 7, true, "Pause the migration."); err != nil {
		t.Fatal(err)
	}
	g := getWork(t, srv).Goals[0]
	var schema *apitypes.WorkItem
	for i := range g.Current {
		if g.Current[i].ID == 8 {
			schema = &g.Current[i]
		}
	}
	if schema == nil || schema.State != apitypes.WorkItemStateOnHold || schema.Why == nil || *schema.Why != "On hold under #7 by you" {
		t.Errorf("#8 = %+v, want on hold under #7", schema)
	}
}

func TestAPIAssignmentDetail(t *testing.T) {
	srv, closedAt := workFixture(t)

	goal := getAssignment(t, srv, 1)
	wantConds := []apitypes.AssignmentCondition{
		{Name: "docs written", State: apitypes.ConditionStateSatisfied, MetBy: &apitypes.MetBy{ID: 4, Title: "Write the docs", At: closedAt}, ClaimedBy: []apitypes.AssignmentRef{}},
		{Name: "release notes", State: apitypes.ConditionStateClaimed, ClaimedBy: []apitypes.AssignmentRef{{ID: 5, Title: "Release notes"}}},
		{Name: "test plan", State: apitypes.ConditionStateUnclaimed, ClaimedBy: []apitypes.AssignmentRef{}},
	}
	if len(goal.Conditions) != 3 {
		t.Fatalf("conditions = %+v", goal.Conditions)
	}
	for i, c := range goal.Conditions {
		w := wantConds[i]
		if c.Name != w.Name || c.State != w.State || !reflect.DeepEqual(c.ClaimedBy, w.ClaimedBy) || (c.MetBy == nil) != (w.MetBy == nil) {
			t.Errorf("condition %d = %+v, want %+v", i, c, w)
		}
		if w.MetBy != nil && (c.MetBy.ID != w.MetBy.ID || c.MetBy.Title != w.MetBy.Title || !c.MetBy.At.Equal(w.MetBy.At)) {
			t.Errorf("condition %d met_by = %+v, want %+v", i, c.MetBy, w.MetBy)
		}
	}
	if goal.Progress != (apitypes.ConditionProgress{Satisfied: 1, Claimed: 1, Unclaimed: 1}) {
		t.Errorf("progress = %+v", goal.Progress)
	}
	var parts []int
	for _, p := range goal.Parts {
		parts = append(parts, p.ID)
	}
	if !reflect.DeepEqual(parts, []int{2, 4, 5, 6, 7, 9}) {
		t.Errorf("parts = %v", parts)
	}
	if goal.State != apitypes.WorkItemStateMoving || goal.Facts.PartOf != nil || goal.Outcome != nil {
		t.Errorf("goal = %+v", goal)
	}

	a := getAssignment(t, srv, 5)
	if a.State != apitypes.WorkItemStateBlocked || a.Why == nil || *a.Why != "Waiting on #3 Fix the login redirect, assigned to Engineer" {
		t.Errorf("#5 state %s why %v", a.State, a.Why)
	}
	f := a.Facts
	if f.Assignee.Slug != "bob" || f.OpenedBy != (apitypes.PersonRef{Slug: "ceo", Name: "You"}) || f.Opened.IsZero() || f.Updated.Before(f.Opened) {
		t.Errorf("facts = %+v", f)
	}
	if f.PartOf == nil || f.PartOf.ID != 1 || f.PartOf.State != apitypes.WorkItemStateMoving {
		t.Errorf("part_of = %+v", f.PartOf)
	}
	if len(f.WaitsOn) != 1 || f.WaitsOn[0].ID != 3 || f.WaitsOn[0].State != apitypes.WorkItemStateReady {
		t.Errorf("waits_on = %+v", f.WaitsOn)
	}
	if len(f.HoldsUp) != 1 || f.HoldsUp[0].ID != 1 {
		t.Errorf("holds_up = %+v", f.HoldsUp)
	}
	if !reflect.DeepEqual(f.CountsToward, []apitypes.CountsToward{{ID: 1, Title: "Ship v3", Condition: "release notes"}}) {
		t.Errorf("counts_toward = %+v", f.CountsToward)
	}
	if len(a.Log) != 1 || a.Log[0].Text != "opened it, assigned to Engineer" || a.Log[0].By.Slug != agent.CEOSlug {
		t.Errorf("log = %+v", a.Log)
	}

	done := getAssignment(t, srv, 4)
	if done.State != apitypes.WorkItemStateClosed || done.Outcome == nil || done.Outcome.Text != "Done." ||
		done.Outcome.Resolution != apitypes.AssignmentResolutionDone || !done.Outcome.At.Equal(closedAt) {
		t.Errorf("#4 outcome = %+v", done.Outcome)
	}
	if last := done.Log[len(done.Log)-1]; last.Text != "closed it as done" || last.Note == nil || *last.Note != "Done." {
		t.Errorf("#4 last log = %+v", last)
	}

	held := getAssignment(t, srv, 6)
	if held.State != apitypes.WorkItemStateOnHold || !held.HeldHere || held.HeldBy == nil || held.HeldBy.Slug != agent.CEOSlug {
		t.Errorf("#6 = %+v", held)
	}
	if last := held.Log[len(held.Log)-1]; last.Text != "put it on hold, with everything under it" || last.Note == nil || *last.Note != "Wait for the fab." {
		t.Errorf("#6 last log = %+v", last)
	}

	assertAPIError(t, workDo(t, srv, http.MethodGet, "/api/v1/assignments/99", nil), http.StatusNotFound)
	assertAPIError(t, workDo(t, srv, http.MethodGet, "/api/v1/assignments/abc", nil), http.StatusNotFound)
}

func TestAPIAssignmentUpdate(t *testing.T) {
	srv, _ := workFixture(t)
	seq := getAssignment(t, srv, 9).Seq

	title, body := "Labels", "Front and back."
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/update", apitypes.AssignmentUpdateRequest{
		Title: &title, DescriptionMD: &body, Note: "Both sides.", Seq: &seq,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update: code %d body %s", rr.Code, rr.Body.String())
	}
	a := getAssignment(t, srv, 9)
	if a.Title != "Labels" || a.DescriptionMD != "Front and back." {
		t.Errorf("after update: %q %q", a.Title, a.DescriptionMD)
	}
	last := a.Log[len(a.Log)-1]
	if last.Text != "edited the title and description" || last.BeforeRef == nil || last.BeforeRef.Kind != "attachment" ||
		last.BeforeRef.URL != "/attachments/"+last.BeforeRef.Ref || last.Note == nil || *last.Note != "Both sides." {
		t.Errorf("amend log = %+v", last)
	}

	// Reassign and add Done-when conditions in one edit.
	to := "bob"
	conds := []string{"front", "back"}
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/update", apitypes.AssignmentUpdateRequest{Assignee: &to, Conditions: &conds, Note: "Bob has the printer."})
	if rr.Code != http.StatusOK {
		t.Fatalf("reassign: code %d body %s", rr.Code, rr.Body.String())
	}
	a = getAssignment(t, srv, 9)
	if a.Facts.Assignee.Slug != "bob" || len(a.Conditions) != 2 || a.Progress.Unclaimed != 2 {
		t.Errorf("after reassign: %+v", a)
	}
	// Its row on the board now carries its own conditions' meter.
	for _, it := range getWork(t, srv).Goals[0].LookForward {
		if it.ID == 9 && (it.Acceptance == nil || *it.Acceptance != (apitypes.ConditionProgress{Unclaimed: 2})) {
			t.Errorf("#9 row acceptance = %+v", it.Acceptance)
		}
	}

	// An edit that changes nothing is fine and writes nothing.
	n := len(a.Log)
	if rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/update", apitypes.AssignmentUpdateRequest{Title: &title}); rr.Code != http.StatusOK {
		t.Errorf("no-op update: code %d", rr.Code)
	}
	if got := len(getAssignment(t, srv, 9).Log); got != n {
		t.Errorf("no-op update wrote %d entries", got-n)
	}

	// A stale seq is a conflict; so is a rule's refusal.
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/update", apitypes.AssignmentUpdateRequest{Title: &body, Seq: &seq}), http.StatusConflict)
	closedTitle := "Drawn"
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/4/update", apitypes.AssignmentUpdateRequest{Title: &closedTitle, Note: "x"}), http.StatusConflict)
	// Bad input.
	blank := " "
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/update", apitypes.AssignmentUpdateRequest{Title: &blank}), http.StatusBadRequest)
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/update", map[string]any{"priority": 1}), http.StatusBadRequest)
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/99/update", apitypes.AssignmentUpdateRequest{Title: &title}), http.StatusNotFound)
}

func TestAPIAssignmentReopen(t *testing.T) {
	srv, _ := workFixture(t)
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/4/reopen", apitypes.AssignmentReopenRequest{}), http.StatusBadRequest)
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/4/reopen", apitypes.AssignmentReopenRequest{Note: "The drawing is missing a view."})
	if rr.Code != http.StatusOK {
		t.Fatalf("reopen: code %d body %s", rr.Code, rr.Body.String())
	}
	a := getAssignment(t, srv, 4)
	if a.State != apitypes.WorkItemStateReady || a.Outcome != nil || a.Log[len(a.Log)-1].Text != "reopened it" {
		t.Errorf("after reopen: state %s outcome %+v", a.State, a.Outcome)
	}
	// Reopening an open assignment is the tracker's refusal.
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/4/reopen", apitypes.AssignmentReopenRequest{Note: "again"}), http.StatusConflict)
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/99/reopen", apitypes.AssignmentReopenRequest{Note: "x"}), http.StatusNotFound)
}

func TestAPIAssignmentHold(t *testing.T) {
	srv, _ := workFixture(t)
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/hold", apitypes.AssignmentHoldRequest{Held: true}), http.StatusBadRequest)
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/hold", apitypes.AssignmentHoldRequest{Held: true, Note: "Waiting on branding."})
	if rr.Code != http.StatusOK {
		t.Fatalf("hold: code %d body %s", rr.Code, rr.Body.String())
	}
	if a := getAssignment(t, srv, 9); a.State != apitypes.WorkItemStateOnHold || !a.HeldHere {
		t.Errorf("after hold: %s held_here %v", a.State, a.HeldHere)
	}
	assertAPIError(t, workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/hold", apitypes.AssignmentHoldRequest{Held: true, Note: "again"}), http.StatusConflict)
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/9/hold", apitypes.AssignmentHoldRequest{Held: false, Note: "Branding is in."})
	if rr.Code != http.StatusOK {
		t.Fatalf("resume: code %d body %s", rr.Code, rr.Body.String())
	}
	if a := getAssignment(t, srv, 9); a.State != apitypes.WorkItemStateReady || a.Log[len(a.Log)-1].Text != "resumed it, with everything under it" {
		t.Errorf("after resume: %s", a.State)
	}
	// Refused from another site.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/assignments/9/hold", bytes.NewReader([]byte(`{"held":true,"note":"x"}`)))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	mux := http.NewServeMux()
	srv.wireAPIWorkRoutes(mux)
	rec := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusForbidden)
}

func TestLogEntryWords(t *testing.T) {
	set := assignments.Build([]*assignments.Assignment{openAssignment(7, "Epic", "bob", 0)})
	person := func(slug string) apitypes.PersonRef {
		if slug == agent.CEOSlug {
			return apitypes.PersonRef{Slug: slug, Name: "You"}
		}
		return apitypes.PersonRef{Slug: slug, Name: slug}
	}
	cases := []struct {
		e    assignments.Entry
		want string
		ref  int
	}{
		{assignments.Entry{Op: assignments.OpAssigned, From: "alice", To: agent.CEOSlug}, "reassigned it from alice to you", 0},
		{assignments.Entry{Op: assignments.OpParent, Ref: 7}, "made it part of #7", 7},
		{assignments.Entry{Op: assignments.OpParent}, "made it top-level", 0},
		{assignments.Entry{Op: assignments.OpBlocked, Ref: 7}, "made it wait on #7", 7},
		{assignments.Entry{Op: assignments.OpUnblocked, Ref: 7}, "stopped it waiting on #7", 7},
		{assignments.Entry{Op: assignments.OpClosed, Resolution: assignments.ResolutionDone, Unmet: []string{"a", "b"}}, "closed it as done, with 2 Done-when conditions unmet: “a”, “b”", 0},
		{assignments.Entry{Op: assignments.OpCreator, From: "alice", To: "bob"}, "handed who opened it from alice to bob", 0},
		{assignments.Entry{Op: assignments.OpAcceptance, Items: []string{"x"}}, "set Done when to “x”", 0},
		{assignments.Entry{Op: assignments.OpAcceptance}, "cleared Done when", 0},
		{assignments.Entry{Op: assignments.OpSatisfies, Items: []string{"x"}}, "set what it counts toward to “x”", 0},
		{assignments.Entry{Op: assignments.OpAmended, Fields: "title"}, "edited the title", 0},
	}
	for _, c := range cases {
		got := logEntry(set, c.e, person)
		if got.Text != c.want {
			t.Errorf("%s: text %q, want %q", c.e.Op, got.Text, c.want)
		}
		if (got.Ref != nil) != (c.ref != 0) || (got.Ref != nil && (got.Ref.ID != c.ref || got.Ref.Title != "Epic")) {
			t.Errorf("%s: ref %+v, want #%d", c.e.Op, got.Ref, c.ref)
		}
		if got.BeforeRef != nil {
			t.Errorf("%s: before_ref without a prior", c.e.Op)
		}
	}
}
