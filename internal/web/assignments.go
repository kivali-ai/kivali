package web

import (
	"context"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
)

// The CEO's side of the assignment tracker: the changes that make the
// CEO an actor with every permission. A change
// made here delivers to the agent at once, like a CEO reply to any
// message; the release gate paces agents, not the CEO. See
// docs/developers/assignments.md.

// assignmentEdit is one CEO edit to an open assignment: a nil field is
// left alone, and a field equal to the record is no change. Behind
// POST /api/v1/assignments/{id}/update.
type assignmentEdit struct {
	// Seq is the log position the editor read the record at; nil
	// skips the check.
	Seq        *int64
	Title      *string
	Body       *string
	Assignee   *string // blank is no change
	Parent     *int    // 0 clears the parent
	BlockedBy  *[]int
	Acceptance *[]string
	Satisfies  *[]string
	Note       string
}

// updateAssignmentAsCEO applies an edit as the CEO. Only the fields that
// differ from the record are sent to the rules, so an untouched field
// never costs a log entry or a wake; an edit that changes nothing is
// neither a change nor a refusal. The caller checks for a tracker
// first and republishes the org snapshot after.
func (s *Server) updateAssignmentAsCEO(ctx context.Context, id int, e assignmentEdit) error {
	cur, err := s.Store.ReadAssignment(id)
	if err != nil {
		return err
	}
	// The editor carries the log position it was built from. A record
	// that moved since (an agent added a blocker, the creator amended)
	// would otherwise be diffed against a stale picture and the CEO's
	// save would quietly undo the move.
	if e.Seq != nil && *e.Seq != cur.LastSeq() {
		// A hand-edited record can have no log; LastSeq is then 0.
		if len(cur.Log) == 0 {
			return &assignments.Refusal{Reason: fmt.Sprintf("%s changed while you were editing; this form shows the record as it stands now, so make your change again", cur.Ref())}
		}
		last := cur.Log[len(cur.Log)-1]
		return &assignments.Refusal{Reason: fmt.Sprintf("%s changed while you were editing (%s by %s); this form shows the record as it stands now, so make your change again", cur.Ref(), last.Op, last.By)}
	}
	in := assignments.UpdateInput{Note: e.Note}
	if e.Title != nil {
		if t := strings.TrimSpace(*e.Title); t != cur.Title {
			in.Title = &t
		}
	}
	if e.Body != nil {
		if b := assignments.NormalizeText(*e.Body); b != cur.Body {
			in.Body = &b
		}
	}
	if e.Assignee != nil {
		if a := strings.TrimSpace(*e.Assignee); a != "" && a != cur.Assignee {
			in.Assignee = &a
		}
	}
	if e.Parent != nil && *e.Parent != cur.Parent {
		p := *e.Parent
		in.Parent = &p
	}
	if e.BlockedBy != nil {
		have := map[int]bool{}
		for _, b := range cur.BlockedBy {
			have[b] = true
		}
		wantSet := map[int]bool{}
		for _, b := range *e.BlockedBy {
			if wantSet[b] {
				continue
			}
			wantSet[b] = true
			if !have[b] {
				in.AddBlockedBy = append(in.AddBlockedBy, b)
			}
		}
		for _, b := range cur.BlockedBy {
			if !wantSet[b] {
				in.RemoveBlockedBy = append(in.RemoveBlockedBy, b)
			}
		}
	}
	if e.Acceptance != nil {
		in.AddAcceptance, in.RemoveAcceptance = diffItems(cur.Acceptance, assignments.NormalizeItems(*e.Acceptance))
	}
	if e.Satisfies != nil {
		in.AddSatisfies, in.RemoveSatisfies = diffItems(cur.Satisfies, assignments.NormalizeItems(*e.Satisfies))
	}
	if in.Title == nil && in.Body == nil && in.Assignee == nil && in.Parent == nil && len(in.AddBlockedBy) == 0 && len(in.RemoveBlockedBy) == 0 &&
		len(in.AddAcceptance) == 0 && len(in.RemoveAcceptance) == 0 && len(in.AddSatisfies) == 0 && len(in.RemoveSatisfies) == 0 {
		return nil
	}
	_, err = s.tracker().Update(ctx, agent.CEOSlug, id, in)
	return err
}

// closeAssignmentAsCEO closes assignment id as the CEO with resolution
// (done or dropped) and outcome. Behind
// POST /api/v1/assignments/{id}/close, which checks for a tracker first
// and republishes the org snapshot after.
func (s *Server) closeAssignmentAsCEO(ctx context.Context, id int, resolution, outcome string) error {
	res := assignments.Resolution(strings.TrimSpace(resolution))
	_, err := s.tracker().Close(ctx, agent.CEOSlug, id, res, outcome)
	return err
}

// reopenAssignmentAsCEO returns a closed assignment to open as the
// CEO. Behind POST /api/v1/assignments/{id}/reopen.
func (s *Server) reopenAssignmentAsCEO(ctx context.Context, id int, note string) error {
	_, err := s.tracker().Reopen(ctx, agent.CEOSlug, id, note)
	return err
}

// holdAssignmentAsCEO puts an assignment on hold (hold true) or
// resumes it, as the CEO. Behind POST /api/v1/assignments/{id}/hold.
func (s *Server) holdAssignmentAsCEO(ctx context.Context, id int, hold bool, note string) error {
	_, err := s.tracker().Update(ctx, agent.CEOSlug, id, assignments.UpdateInput{Hold: &hold, Note: note})
	return err
}

// diffItems turns the list a form posted into the adds and removes
// the rules take, the way blocked_by is diffed: only what changed is
// sent, so an untouched list costs no log entry.
func diffItems(have, want []string) (add, remove []string) {
	haveSet := map[string]bool{}
	for _, n := range have {
		haveSet[n] = true
	}
	wantSet := map[string]bool{}
	for _, n := range want {
		wantSet[n] = true
		if !haveSet[n] {
			add = append(add, n)
		}
	}
	for _, n := range have {
		if !wantSet[n] {
			remove = append(remove, n)
		}
	}
	return add, remove
}

// ReconcileAssignments routes whatever wakes a crash left unrouted and
// refuses to start on a tracker that cannot be read, so a corrupt
// assignment file is named at boot rather than on the first tool call.
// Called once from main.go after the store is up.
func (s *Server) ReconcileAssignments(ctx context.Context) error {
	tr := s.tracker()
	if tr == nil {
		return nil
	}
	if _, err := tr.Set(); err != nil {
		return err
	}
	return tr.Reconcile(ctx)
}

// assignmentEventCEOItem fills the CEO inbox card for an assignment
// event: the assignment it is about and whether the CEO still has to act on it (an
// assignment to them, still open and still theirs). Everything else
// the tracker tells the CEO is history the moment it arrives.
func (s *Server) assignmentEventCEOItem(set *assignments.Set, m store.Message) (iss *assignments.Assignment, needsAction bool) {
	if m.Assignment == nil || set == nil {
		return nil, false
	}
	iss, ok := set.Get(m.Assignment.ID)
	if !ok {
		return nil, false
	}
	assignment := m.Assignment.Op == string(assignments.OpCreated) || m.Assignment.Op == string(assignments.OpAssigned) || m.Assignment.Op == string(assignments.OpReopened)
	return iss, assignment && iss.Open() && iss.Assignee == agent.CEOSlug
}
