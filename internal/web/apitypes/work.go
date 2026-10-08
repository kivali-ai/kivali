package apitypes

import "time"

// ---- /api/v1/work ----

// WorkItemState is where one assignment stands on the Work board and
// on its detail page. Derived from the tracker every read, never
// stored:
//
//   - closed: closed, as done or dropped.
//   - on_hold: open and on hold, by its own hold or an enclosing one.
//   - blocked: open, not on hold, and waiting on another open
//     assignment it names (a dependency outside its own parts).
//   - moving: open, not on hold, waiting on nothing but its own open
//     parts: the work is under way through them.
//   - ready: open, not on hold, waiting on nothing; whoever holds it
//     can act on it now.
//
// A hold outranks a block, and waiting on one's own parts is progress,
// not blockage, as in Goal.Blocked.
type WorkItemState string

const (
	WorkItemStateReady   WorkItemState = "ready"
	WorkItemStateMoving  WorkItemState = "moving"
	WorkItemStateBlocked WorkItemState = "blocked"
	WorkItemStateOnHold  WorkItemState = "on_hold"
	WorkItemStateClosed  WorkItemState = "closed"
)

// WorkBoard is GET /api/v1/work: the board by goal.
type WorkBoard struct {
	Readouts WorkReadouts `json:"readouts"`
	// Goals are the open goals, in the snapshot's order (ascending by
	// id), each with its three columns.
	Goals []WorkGoal `json:"goals"`
	// ClosedGoals are the goals (top-level assignments with parts or
	// Done-when conditions) closed within the last seven days, most
	// recently closed first.
	ClosedGoals []ClosedGoal `json:"closed_goals"`
}

// WorkReadouts are the numbers across the top of the board. Open,
// ready, blocked and on hold count every open assignment, goals
// included; blocked leaves out assignments on hold, as the Home
// readout does. ClosedWeek counts every assignment closed in the last
// seven days, as the snapshot's readout does: goals and top-level
// assignments too, so it is at least the sum of the goals' LookBack
// lengths and usually more.
type WorkReadouts struct {
	Open       int `json:"open"`
	Ready      int `json:"ready"`
	Blocked    int `json:"blocked"`
	OnHold     int `json:"on_hold"`
	ClosedWeek int `json:"closed_week"`
}

// WorkGoal is one goal's block: the header, then three columns over
// the goal's parts at any depth. The client groups each column's
// items by owner.
type WorkGoal struct {
	ID    int       `json:"id"`
	Title string    `json:"title"`
	Owner PersonRef `json:"owner"`
	// Done and Total are the snapshot goal's (Goal.Done, Goal.Total).
	Done  int `json:"done"`
	Total int `json:"total"`
	// State is the goal's own state (never closed here).
	State WorkItemState `json:"state"`
	// LookBack is the parts closed in the last seven days, most
	// recently closed first.
	LookBack []WorkItem `json:"look_back"`
	// Current is the open parts that are moving, blocked or on hold,
	// ascending by id.
	Current []WorkItem `json:"current"`
	// LookForward is the open parts that are ready, ascending by id.
	LookForward []WorkItem `json:"look_forward"`
	// Unclaimed is the goal's own Done-when conditions that no part
	// names yet, in declared order. A part's own unclaimed conditions
	// show on its row (WorkItem.Acceptance), not here.
	Unclaimed []UnclaimedCondition `json:"unclaimed"`
}

// WorkItem is one assignment on the board.
type WorkItem struct {
	ID    int           `json:"id"`
	Title string        `json:"title"`
	State WorkItemState `json:"state"`
	// Owner is the assignee.
	Owner PersonRef `json:"owner"`
	// WaitingOn is what a blocked assignment waits on: the open
	// assignments it names, ascending, each with its state for the
	// reference chip. Empty otherwise.
	WaitingOn []AssignmentLink `json:"waiting_on"`
	// HeldBy is who put the hold on, when the item is on hold.
	HeldBy *PersonRef `json:"held_by,omitempty"`
	// Why says in words why a blocked or on-hold item cannot move:
	// "Waiting on #58 Fix the login redirect, assigned to Engineering lead",
	// "On hold by you", "On hold under #7 by you".
	Why *string `json:"why,omitempty"`
	// Acceptance counts the item's own Done-when conditions by state,
	// for the row's meter. Absent when it declares none.
	Acceptance *ConditionProgress `json:"acceptance,omitempty"`
	// ClosedAt and Resolution are set on closed items.
	ClosedAt   *time.Time            `json:"closed_at,omitempty"`
	Resolution *AssignmentResolution `json:"resolution,omitempty"`
}

// UnclaimedCondition is a Done-when condition nothing opened names.
type UnclaimedCondition struct {
	Name string `json:"name"`
}

// ClosedGoal is a goal closed in the last seven days.
type ClosedGoal struct {
	ID         int                  `json:"id"`
	Title      string               `json:"title"`
	Owner      PersonRef            `json:"owner"`
	ClosedAt   time.Time            `json:"closed_at"`
	Resolution AssignmentResolution `json:"resolution"`
}

// ---- /api/v1/assignments/{id} ----

// Assignment is GET /api/v1/assignments/{id}: one assignment with
// everything its detail page shows.
type Assignment struct {
	ID    int           `json:"id"`
	Title string        `json:"title"`
	State WorkItemState `json:"state"`
	// Why says in words why it cannot move (blocked or on hold).
	Why *string `json:"why,omitempty"`
	// HeldBy is who put the hold on, when it is on hold.
	HeldBy *PersonRef `json:"held_by,omitempty"`
	// HeldHere is true when the hold is this assignment's own (Resume
	// applies to it) rather than an enclosing assignment's.
	HeldHere bool `json:"held_here"`
	// Seq is the newest log entry's sequence number. Send it back on
	// POST …/update so an edit against a record that changed meanwhile
	// is refused rather than undoing the change.
	Seq           int64           `json:"seq"`
	Facts         AssignmentFacts `json:"facts"`
	DescriptionMD string          `json:"description_md"`
	// Conditions are the Done-when conditions, in declared order.
	// Empty when it declares none.
	Conditions []AssignmentCondition `json:"conditions"`
	Progress   ConditionProgress     `json:"progress"`
	// Outcome is set once it is closed.
	Outcome *AssignmentOutcome `json:"outcome,omitempty"`
	// Log is every change, oldest first.
	Log []AssignmentLogEntry `json:"log"`
	// Parts are its children, ascending by id.
	Parts []AssignmentLink `json:"parts"`
}

// AssignmentFacts are the eight facts at the top of the detail page.
type AssignmentFacts struct {
	Assignee PersonRef       `json:"assignee"`
	OpenedBy PersonRef       `json:"opened_by"`
	Opened   time.Time       `json:"opened"`
	Updated  time.Time       `json:"updated"`
	PartOf   *AssignmentLink `json:"part_of,omitempty"`
	// WaitsOn is every assignment it names as a dependency, open or
	// closed, ascending.
	WaitsOn []AssignmentLink `json:"waits_on"`
	// HoldsUp is the open assignments waiting on it: those naming it
	// as a dependency, and its parent while it is open.
	HoldsUp []AssignmentLink `json:"holds_up"`
	// CountsToward is the parent's Done-when conditions this
	// assignment delivers.
	CountsToward []CountsToward `json:"counts_toward"`
}

// AssignmentLink names another assignment with its state and owner.
type AssignmentLink struct {
	ID    int           `json:"id"`
	Title string        `json:"title"`
	State WorkItemState `json:"state"`
	Owner PersonRef     `json:"owner"`
}

// CountsToward is one Done-when condition of the parent (ID, Title)
// that this assignment delivers when it closes as done.
type CountsToward struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Condition string `json:"condition"`
}

// ConditionState is where one Done-when condition stands: satisfied
// when a part naming it closed as done, claimed while an open part
// names it, unclaimed when nothing opened names it.
type ConditionState string

const (
	ConditionStateSatisfied ConditionState = "satisfied"
	ConditionStateClaimed   ConditionState = "claimed"
	ConditionStateUnclaimed ConditionState = "unclaimed"
)

// AssignmentCondition is one Done-when condition.
type AssignmentCondition struct {
	Name  string         `json:"name"`
	State ConditionState `json:"state"`
	// MetBy is the part whose done close met it: the earliest such
	// close when several parts name it.
	MetBy *MetBy `json:"met_by,omitempty"`
	// ClaimedBy is the open parts naming it, ascending.
	ClaimedBy []AssignmentRef `json:"claimed_by"`
}

// MetBy is the assignment that met a condition and when it closed.
type MetBy struct {
	ID    int       `json:"id"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
}

// ConditionProgress counts the conditions by state; all zero when it
// declares none.
type ConditionProgress struct {
	Satisfied int `json:"satisfied"`
	Claimed   int `json:"claimed"`
	Unclaimed int `json:"unclaimed"`
}

// AssignmentOutcome is how it ended.
type AssignmentOutcome struct {
	Resolution AssignmentResolution `json:"resolution"`
	Text       string               `json:"text"`
	At         time.Time            `json:"at"`
	// Unmet names the Done-when conditions still unmet when it closed
	// as done. Empty otherwise.
	Unmet []string `json:"unmet"`
}

// AssignmentLogEntry is one change, in words. Text starts after the
// actor ("reassigned it from Buyer to Tester"); Note is the reason the
// actor gave.
type AssignmentLogEntry struct {
	At   time.Time `json:"at"`
	By   PersonRef `json:"by"`
	Text string    `json:"text"`
	Note *string   `json:"note,omitempty"`
	// Ref is the assignment the change points at (a new parent, a
	// dependency added or removed).
	Ref *AssignmentRef `json:"ref,omitempty"`
	// BeforeRef is the text as it was before an edit.
	BeforeRef *BeforeRef `json:"before_ref,omitempty"`
}

// BeforeRef points at a stored earlier text. Kind is "attachment":
// Ref is its content hash and URL opens it.
type BeforeRef struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	URL  string `json:"url"`
}

// ---- POST /api/v1/assignments/{id}/reopen, /hold, /update ----

// AssignmentReopenRequest reopens a closed assignment. Note says what is
// wrong with the outcome; its assignee reads it first. Required.
type AssignmentReopenRequest struct {
	Note string `json:"note"`
}

// AssignmentHoldRequest puts an assignment and everything under it on hold
// (Held true) or resumes it (false). Note says why; everyone it wakes
// reads it first. Required.
type AssignmentHoldRequest struct {
	Held bool   `json:"held"`
	Note string `json:"note"`
}

// AssignmentUpdateRequest edits an open assignment. Absent fields are left
// alone; a field equal to the record is no change. Note is required
// when the change wakes someone else (a reassign, or an edit to
// someone else's assignment). Seq, when sent, must be Assignment.Seq
// as read: an edit against a record that has changed since is refused.
type AssignmentUpdateRequest struct {
	Title         *string `json:"title,omitempty"`
	DescriptionMD *string `json:"description_md,omitempty"`
	Assignee      *string `json:"assignee,omitempty"`
	// Parent is the assignment it is part of; 0 makes it top-level.
	Parent *int `json:"parent,omitempty"`
	// WaitsOn replaces the list of assignments it waits on.
	WaitsOn *[]int `json:"waits_on,omitempty"`
	// Conditions replaces its Done-when conditions.
	Conditions *[]string `json:"conditions,omitempty"`
	// CountsToward replaces the parent's conditions it delivers.
	CountsToward *[]string `json:"counts_toward,omitempty"`
	Note         string    `json:"note,omitempty"`
	Seq          *int64    `json:"seq,omitempty"`
}
