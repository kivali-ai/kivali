package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Work's JSON: the board by goal (GET /api/v1/work), one assignment
// (GET /api/v1/assignments/{id}), and the Act menu's reopen, hold and edit.
// Close lives with Home (api_home.go), which shipped it first. Every
// action runs one method (reopenAssignmentAsCEO, holdAssignmentAsCEO,
// updateAssignmentAsCEO), and the goal headers are the snapshot's own
// goals (goalsAndAssignmentReadouts), so the Work board and Home cannot
// drift. Humans do not file work, so there is
// no create.

// wireAPIWorkRoutes registers Work's routes on the API mux.
func (s *Server) wireAPIWorkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/work", s.handleAPIWork)
	mux.HandleFunc("GET /api/v1/assignments/{id}", s.handleAPIAssignment)
	mux.HandleFunc("POST /api/v1/assignments/{id}/reopen", s.handleAPIAssignmentReopen)
	mux.HandleFunc("POST /api/v1/assignments/{id}/hold", s.handleAPIAssignmentHold)
	mux.HandleFunc("POST /api/v1/assignments/{id}/update", s.handleAPIAssignmentUpdate)
}

// ---- derived state ----

// workState is the board's word for where an assignment stands; see
// apitypes.WorkItemState for the rule. A hold outranks a block, and a
// wait on the assignment's own open parts is progress (moving), not a
// block, as in goalsAndAssignmentReadouts.
func workState(set *assignments.Set, iss *assignments.Assignment) apitypes.WorkItemState {
	switch {
	case !iss.Open():
		return apitypes.WorkItemStateClosed
	case set.Held(iss.ID):
		return apitypes.WorkItemStateOnHold
	case len(dependencyWaits(set, iss)) > 0:
		return apitypes.WorkItemStateBlocked
	case set.Ready(iss.ID):
		return apitypes.WorkItemStateReady
	}
	return apitypes.WorkItemStateMoving
}

// personFunc names a slug for the wire (homeView.person).
type personFunc func(slug string) apitypes.PersonRef

// phrase is a person as a sentence names them mid-way: "you" for the
// CEO, else their name.
func phrase(p apitypes.PersonRef) string {
	if p.Slug == agent.CEOSlug {
		return "you"
	}
	return p.Name
}

// holder is who put on the hold covering iss (the actor of the newest
// held entry on the holding assignment) and that assignment's id; ok
// is false when iss is not on hold.
func holder(set *assignments.Set, iss *assignments.Assignment) (by string, holdID int, ok bool) {
	holdID = set.HeldBy(iss.ID)
	if holdID == 0 {
		return "", 0, false
	}
	h, _ := set.Get(holdID)
	for i := len(h.Log) - 1; i >= 0; i-- {
		if h.Log[i].Op == assignments.OpHeld {
			return h.Log[i].By, holdID, true
		}
	}
	return h.Creator, holdID, true
}

// whyStuck says in words why iss cannot move, and who holds it when it
// is on hold. Empty for an assignment that can move.
func whyStuck(set *assignments.Set, iss *assignments.Assignment, state apitypes.WorkItemState, person personFunc) (why *string, heldBy *apitypes.PersonRef) {
	switch state {
	case apitypes.WorkItemStateOnHold:
		by, holdID, _ := holder(set, iss)
		p := person(by)
		text := "On hold by " + phrase(p)
		if holdID != iss.ID {
			text = fmt.Sprintf("On hold under #%d by %s", holdID, phrase(p))
		}
		return &text, &p
	case apitypes.WorkItemStateBlocked:
		// "Waiting on #58 Fix the login redirect, assigned to Engineering lead",
		// the detail canvas's words: what it waits on and who has it.
		waits := dependencyWaits(set, iss)
		parts := make([]string, 0, len(waits))
		for _, id := range waits {
			t, _ := set.Get(id)
			parts = append(parts, fmt.Sprintf("#%d %s, assigned to %s", id, t.Title, phrase(person(t.Assignee))))
		}
		text := "Waiting on " + strings.Join(parts, " and ")
		return &text, nil
	}
	return nil, nil
}

// workItem is one assignment as a board row.
func workItem(set *assignments.Set, iss *assignments.Assignment, person personFunc) apitypes.WorkItem {
	state := workState(set, iss)
	it := apitypes.WorkItem{
		ID: iss.ID, Title: iss.Title, State: state, Owner: person(iss.Assignee),
		WaitingOn: []apitypes.AssignmentLink{},
	}
	it.Why, it.HeldBy = whyStuck(set, iss, state, person)
	if state == apitypes.WorkItemStateBlocked {
		for _, id := range dependencyWaits(set, iss) {
			t, _ := set.Get(id)
			it.WaitingOn = append(it.WaitingOn, assignmentLink(set, t, person))
		}
	}
	if p, ok := set.Progress(iss.ID); ok {
		it.Acceptance = &apitypes.ConditionProgress{Satisfied: p.Satisfied, Claimed: p.Claimed, Unclaimed: p.Unclaimed}
	}
	if !iss.Open() {
		res := apitypes.AssignmentResolution(iss.Resolution)
		it.Resolution = &res
		if iss.Closed != nil {
			at := *iss.Closed
			it.ClosedAt = &at
		}
	}
	return it
}

// descendants lists every assignment under id at any depth, open or closed,
// ascending.
func descendants(set *assignments.Set, id int) []int {
	var out []int
	seen := map[int]bool{id: true}
	stack := set.Parts(id)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
		stack = append(stack, set.Parts(n)...)
	}
	sort.Ints(out)
	return out
}

// isGoal reports whether a top-level assignment is a goal: it has
// Done-when conditions or parts (goalsAndAssignmentReadouts' rule).
func isGoal(set *assignments.Set, iss *assignments.Assignment) bool {
	return iss.Parent == 0 && (len(iss.Acceptance) > 0 || len(set.Parts(iss.ID)) > 0)
}

// ---- GET /api/v1/work ----

// buildWorkBoard derives the board from one assignment set.
//
// Goals and their done/total come from goalsAndAssignmentReadouts, as in
// the snapshot. Each goal's parts at any depth fall into one column:
// closed within closedWeekWindow → look back (older closes are left
// off); moving, blocked or on hold → current; ready → look forward.
// Unclaimed is the goal's own Done-when conditions nothing names.
func buildWorkBoard(set *assignments.Set, now time.Time, person personFunc) apitypes.WorkBoard {
	goals, blocked, closedWeek := goalsAndAssignmentReadouts(set, now, person)
	board := apitypes.WorkBoard{
		Readouts:    apitypes.WorkReadouts{Blocked: blocked, ClosedWeek: closedWeek},
		Goals:       make([]apitypes.WorkGoal, 0, len(goals)),
		ClosedGoals: []apitypes.ClosedGoal{},
	}
	cut := now.Add(-closedWeekWindow)
	for _, id := range set.IDs() {
		iss, _ := set.Get(id)
		if iss.Open() {
			board.Readouts.Open++
			switch {
			case set.Held(id):
				board.Readouts.OnHold++
			case set.Ready(id):
				board.Readouts.Ready++
			}
			continue
		}
		if isGoal(set, iss) && iss.Closed != nil && !iss.Closed.Before(cut) {
			board.ClosedGoals = append(board.ClosedGoals, apitypes.ClosedGoal{
				ID: iss.ID, Title: iss.Title, Owner: person(iss.Assignee),
				ClosedAt: *iss.Closed, Resolution: apitypes.AssignmentResolution(iss.Resolution),
			})
		}
	}
	sort.SliceStable(board.ClosedGoals, func(i, j int) bool {
		return board.ClosedGoals[i].ClosedAt.After(board.ClosedGoals[j].ClosedAt)
	})

	for _, g := range goals {
		iss, _ := set.Get(g.ID)
		wg := apitypes.WorkGoal{
			ID: g.ID, Title: g.Title, Owner: person(g.Owner), Done: g.Done, Total: g.Total,
			State:    workState(set, iss),
			LookBack: []apitypes.WorkItem{}, Current: []apitypes.WorkItem{},
			LookForward: []apitypes.WorkItem{}, Unclaimed: []apitypes.UnclaimedCondition{},
		}
		for _, id := range descendants(set, g.ID) {
			part, _ := set.Get(id)
			it := workItem(set, part, person)
			switch it.State {
			case apitypes.WorkItemStateClosed:
				if part.Closed != nil && !part.Closed.Before(cut) {
					wg.LookBack = append(wg.LookBack, it)
				}
			case apitypes.WorkItemStateReady:
				wg.LookForward = append(wg.LookForward, it)
			default:
				wg.Current = append(wg.Current, it)
			}
		}
		sort.SliceStable(wg.LookBack, func(i, j int) bool {
			return wg.LookBack[i].ClosedAt.After(*wg.LookBack[j].ClosedAt)
		})
		for _, item := range set.Items(g.ID) {
			if item.State() == assignments.ConditionUnclaimed {
				wg.Unclaimed = append(wg.Unclaimed, apitypes.UnclaimedCondition{Name: item.Name})
			}
		}
		board.Goals = append(board.Goals, wg)
	}
	return board
}

func (s *Server) handleAPIWork(w http.ResponseWriter, _ *http.Request) {
	set, err := s.currentAssignmentSet()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the assignments could not be read", whoServer)
		return
	}
	writeJSON(w, http.StatusOK, buildWorkBoard(set, s.clk().Now(), s.newHomeView().person))
}

// ---- GET /api/v1/assignments/{id} ----

// conditionStateWire is a Done-when condition's state as the API
// names it. The tracker says "met" to the model; the wire and the
// design system's meter keep "satisfied".
func conditionStateWire(s assignments.ConditionState) apitypes.ConditionState {
	switch s {
	case assignments.ConditionMet:
		return apitypes.ConditionStateSatisfied
	case assignments.ConditionClaimed:
		return apitypes.ConditionStateClaimed
	}
	return apitypes.ConditionStateUnclaimed
}

// assignmentLink names another assignment with its state and owner.
func assignmentLink(set *assignments.Set, iss *assignments.Assignment, person personFunc) apitypes.AssignmentLink {
	return apitypes.AssignmentLink{ID: iss.ID, Title: iss.Title, State: workState(set, iss), Owner: person(iss.Assignee)}
}

// buildAssignment is one assignment with everything its detail screen
// (/assignments/{id} in the web app) shows.
func buildAssignment(set *assignments.Set, iss *assignments.Assignment, person personFunc) apitypes.Assignment {
	state := workState(set, iss)
	a := apitypes.Assignment{
		ID: iss.ID, Title: iss.Title, State: state, HeldHere: iss.Held, Seq: iss.LastSeq(),
		Facts: apitypes.AssignmentFacts{
			Assignee: person(iss.Assignee), OpenedBy: person(iss.Creator),
			Opened: iss.Created, Updated: iss.Updated,
			WaitsOn: []apitypes.AssignmentLink{}, HoldsUp: []apitypes.AssignmentLink{},
			CountsToward: []apitypes.CountsToward{},
		},
		DescriptionMD: iss.Body,
		Conditions:    []apitypes.AssignmentCondition{},
		Log:           make([]apitypes.AssignmentLogEntry, 0, len(iss.Log)),
		Parts:         []apitypes.AssignmentLink{},
	}
	a.Why, a.HeldBy = whyStuck(set, iss, state, person)
	if iss.Parent != 0 {
		if p, ok := set.Get(iss.Parent); ok {
			link := assignmentLink(set, p, person)
			a.Facts.PartOf = &link
			for _, name := range iss.Satisfies {
				a.Facts.CountsToward = append(a.Facts.CountsToward, apitypes.CountsToward{ID: p.ID, Title: p.Title, Condition: name})
			}
		}
	}
	for _, b := range iss.BlockedBy {
		if bi, ok := set.Get(b); ok {
			a.Facts.WaitsOn = append(a.Facts.WaitsOn, assignmentLink(set, bi, person))
		}
	}
	for _, b := range set.Blocks(iss.ID) {
		bi, _ := set.Get(b)
		a.Facts.HoldsUp = append(a.Facts.HoldsUp, assignmentLink(set, bi, person))
	}
	for _, c := range set.Parts(iss.ID) {
		ci, _ := set.Get(c)
		a.Parts = append(a.Parts, assignmentLink(set, ci, person))
	}
	for _, item := range set.Items(iss.ID) {
		cond := apitypes.AssignmentCondition{
			Name: item.Name, State: conditionStateWire(item.State()), ClaimedBy: []apitypes.AssignmentRef{},
		}
		for _, c := range item.ClaimedBy {
			ci, _ := set.Get(c)
			cond.ClaimedBy = append(cond.ClaimedBy, apitypes.AssignmentRef{ID: c, Title: ci.Title})
		}
		// The first done close meets it (docs/developers/assignments.md), so the
		// earliest close among the parts that name it.
		for _, c := range item.SatisfiedBy {
			ci, _ := set.Get(c)
			if ci.Closed == nil {
				continue
			}
			if cond.MetBy == nil || ci.Closed.Before(cond.MetBy.At) {
				cond.MetBy = &apitypes.MetBy{ID: c, Title: ci.Title, At: *ci.Closed}
			}
		}
		a.Conditions = append(a.Conditions, cond)
	}
	if p, ok := set.Progress(iss.ID); ok {
		a.Progress = apitypes.ConditionProgress{Satisfied: p.Satisfied, Claimed: p.Claimed, Unclaimed: p.Unclaimed}
	}
	if !iss.Open() && iss.Closed != nil {
		out := &apitypes.AssignmentOutcome{
			Resolution: apitypes.AssignmentResolution(iss.Resolution), Text: iss.Outcome, At: *iss.Closed, Unmet: []string{},
		}
		if n := len(iss.Log); n > 0 && iss.Log[n-1].Op == assignments.OpClosed {
			out.Unmet = append(out.Unmet, iss.Log[n-1].Unmet...)
		}
		a.Outcome = out
	}
	for _, e := range iss.Log {
		a.Log = append(a.Log, logEntry(set, e, person))
	}
	return a
}

// quoted renders names as the log prints them: “a”, “b”.
func quoted(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "“" + n + "”"
	}
	return strings.Join(parts, ", ")
}

// logEntry is one log entry in words, in the detail screen's vocabulary.
func logEntry(set *assignments.Set, e assignments.Entry, person personFunc) apitypes.AssignmentLogEntry {
	out := apitypes.AssignmentLogEntry{At: e.TS, By: person(e.By)}
	if e.Note != "" {
		note := e.Note
		out.Note = &note
	}
	ref := func(id int) *apitypes.AssignmentRef {
		r := &apitypes.AssignmentRef{ID: id}
		if t, ok := set.Get(id); ok {
			r.Title = t.Title
		}
		return r
	}
	switch e.Op {
	case assignments.OpCreated:
		out.Text = "opened it, assigned to " + phrase(person(e.To))
	case assignments.OpAssigned:
		out.Text = fmt.Sprintf("reassigned it from %s to %s", phrase(person(e.From)), phrase(person(e.To)))
	case assignments.OpAmended:
		out.Text = "edited the " + strings.ReplaceAll(e.Fields, ",", " and ")
		if e.Prior != "" {
			out.BeforeRef = &apitypes.BeforeRef{Kind: "attachment", Ref: e.Prior, URL: "/attachments/" + e.Prior}
		}
	case assignments.OpParent:
		if e.Ref != 0 {
			out.Text = fmt.Sprintf("made it part of #%d", e.Ref)
			out.Ref = ref(e.Ref)
		} else {
			out.Text = "made it top-level"
		}
	case assignments.OpBlocked:
		out.Text = fmt.Sprintf("made it wait on #%d", e.Ref)
		out.Ref = ref(e.Ref)
	case assignments.OpUnblocked:
		out.Text = fmt.Sprintf("stopped it waiting on #%d", e.Ref)
		out.Ref = ref(e.Ref)
	case assignments.OpClosed:
		out.Text = "closed it as " + string(e.Resolution)
		if n := len(e.Unmet); n > 0 {
			noun := "condition"
			if n != 1 {
				noun = "conditions"
			}
			out.Text += fmt.Sprintf(", with %d Done-when %s unmet: %s", n, noun, quoted(e.Unmet))
		}
	case assignments.OpReopened:
		out.Text = "reopened it"
	case assignments.OpCreator:
		out.Text = fmt.Sprintf("handed who opened it from %s to %s", phrase(person(e.From)), phrase(person(e.To)))
	case assignments.OpHeld:
		out.Text = "put it on hold, with everything under it"
	case assignments.OpResumed:
		out.Text = "resumed it, with everything under it"
	case assignments.OpAcceptance:
		if len(e.Items) > 0 {
			out.Text = "set Done when to " + quoted(e.Items)
		} else {
			out.Text = "cleared Done when"
		}
	case assignments.OpSatisfies:
		if len(e.Items) > 0 {
			out.Text = "set what it counts toward to " + quoted(e.Items)
		} else {
			out.Text = "cleared what it counts toward"
		}
	default:
		out.Text = string(e.Op)
	}
	return out
}

// apiAssignmentID reads {id} from the path, writing the 404 itself when it
// is not an assignment id.
func apiAssignmentID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeAPIError(w, http.StatusNotFound, "there is no such assignment", whoNoOne)
		return 0, false
	}
	return id, true
}

func (s *Server) handleAPIAssignment(w http.ResponseWriter, r *http.Request) {
	id, ok := apiAssignmentID(w, r)
	if !ok {
		return
	}
	set, err := s.currentAssignmentSet()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the assignments could not be read", whoServer)
		return
	}
	iss, ok := set.Get(id)
	if !ok {
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("there is no assignment #%d", id), whoNoOne)
		return
	}
	writeJSON(w, http.StatusOK, buildAssignment(set, iss, s.newHomeView().person))
}

// ---- POST /api/v1/assignments/{id}/reopen, /hold, /update ----

// apiAssignmentAction runs one CEO change to an assignment: 404 for an
// unknown id, 503 without a tracker, the change, then 409 for a rule's
// refusal (its text says which rule) or 200 {} after republishing the
// org snapshot. body is decoded first; check, when it returns false,
// has written a 400 itself.
func (s *Server) apiAssignmentAction(w http.ResponseWriter, r *http.Request, body any, check func() bool, run func(id int) error) {
	id, ok := apiAssignmentID(w, r)
	if !ok {
		return
	}
	if !decodeJSONOrError(w, r, body) {
		return
	}
	if !check() {
		return
	}
	if s.tracker() == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "no model is connected yet, so assignments cannot change", whoServer)
		return
	}
	if _, err := s.Store.ReadAssignment(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("there is no assignment #%d", id), whoNoOne)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "the assignment could not be read", whoServer)
		return
	}
	if err := run(id); err != nil {
		switch {
		case assignments.IsRefusal(err):
			writeAPIError(w, http.StatusConflict, err.Error(), whoYou)
		case errors.Is(err, store.ErrNotFound):
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("there is no assignment #%d", id), whoNoOne)
		default:
			writeAPIError(w, http.StatusInternalServerError, "the assignment could not be changed", whoServer)
		}
		return
	}
	s.NotifyOrgState()
	writeJSON(w, http.StatusOK, apitypes.Empty{})
}

func (s *Server) handleAPIAssignmentReopen(w http.ResponseWriter, r *http.Request) {
	var req apitypes.AssignmentReopenRequest
	s.apiAssignmentAction(w, r, &req, func() bool {
		if strings.TrimSpace(req.Note) == "" {
			writeAPIError(w, http.StatusBadRequest, "say what is wrong with the outcome", whoYou)
			return false
		}
		return true
	}, func(id int) error {
		return s.reopenAssignmentAsCEO(r.Context(), id, req.Note)
	})
}

func (s *Server) handleAPIAssignmentHold(w http.ResponseWriter, r *http.Request) {
	var req apitypes.AssignmentHoldRequest
	s.apiAssignmentAction(w, r, &req, func() bool {
		if strings.TrimSpace(req.Note) == "" {
			what := "say why it is going on hold"
			if !req.Held {
				what = "say what changed so work can resume"
			}
			writeAPIError(w, http.StatusBadRequest, what, whoYou)
			return false
		}
		return true
	}, func(id int) error {
		return s.holdAssignmentAsCEO(r.Context(), id, req.Held, req.Note)
	})
}

func (s *Server) handleAPIAssignmentUpdate(w http.ResponseWriter, r *http.Request) {
	var req apitypes.AssignmentUpdateRequest
	s.apiAssignmentAction(w, r, &req, func() bool {
		if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
			writeAPIError(w, http.StatusBadRequest, "an assignment needs a title", whoYou)
			return false
		}
		if req.Assignee != nil && strings.TrimSpace(*req.Assignee) == "" {
			writeAPIError(w, http.StatusBadRequest, "an assignment needs an assignee", whoYou)
			return false
		}
		if req.Parent != nil && *req.Parent < 0 {
			writeAPIError(w, http.StatusBadRequest, "part of must be an assignment id, or 0 for none", whoDevelopers)
			return false
		}
		if req.WaitsOn != nil {
			for _, b := range *req.WaitsOn {
				if b < 1 {
					writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("%d is not an assignment id", b), whoDevelopers)
					return false
				}
			}
		}
		return true
	}, func(id int) error {
		return s.updateAssignmentAsCEO(r.Context(), id, assignmentEdit{
			Seq: req.Seq, Title: req.Title, Body: req.DescriptionMD, Assignee: req.Assignee,
			Parent: req.Parent, BlockedBy: req.WaitsOn,
			Acceptance: req.Conditions, Satisfies: req.CountsToward, Note: req.Note,
		})
	})
}
