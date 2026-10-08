package apitypes

import "time"

// ---- /api/v1/agents/{slug}/background ----

// Background is GET /api/v1/agents/{slug}/background: the agent's plan
// for the work it is fanning out, if it wrote one, and every background
// session it has dispatched since its chat began. Sessions are always
// present (possibly empty); a plan without sessions and sessions
// without a plan are both normal.
type Background struct {
	Plan *BackgroundPlan `json:"plan,omitempty"`
	// Sessions are in dispatch order, oldest first, at every depth: a
	// worker a sub-lead dispatched follows it and names it in CallerID.
	// Nothing is capped; the UI folds long lists.
	Sessions []BackgroundSession `json:"sessions"`
}

// PlanStepState is one plan step's state.
type PlanStepState string

const (
	// PlanStepStateDone is a ticked step.
	PlanStepStateDone PlanStepState = "done"
	// PlanStepStateRunning is an unticked step with a session queued or
	// running under it.
	PlanStepStateRunning PlanStepState = "running"
	// PlanStepStateIdle is an unticked step with nothing live under it and
	// no failure as its latest outcome.
	PlanStepStateIdle PlanStepState = "idle"
	// PlanStepStateNeedsHelp is an unticked step whose latest session failed,
	// with nothing live under it. A cancelled session does not count:
	// someone called it off.
	PlanStepStateNeedsHelp PlanStepState = "needs_help"
)

// BackgroundPlan is the agent's plan file, read as a list of steps.
type BackgroundPlan struct {
	// Title is the plan's first heading, or its first line.
	Title string     `json:"title"`
	Steps []PlanStep `json:"steps"`
	// Done and Total count steps.
	Done  int `json:"done"`
	Total int `json:"total"`
	// Running and Finished count sessions, at every depth, as the
	// header's "2 running · 9 finished" reads.
	Running  int `json:"running"`
	Finished int `json:"finished"`
	// NeedsHelp counts steps in state needs_help.
	NeedsHelp int `json:"needs_help"`
}

// PlanStep is one checkbox line of the plan. N is its 1-based position,
// which is the number a session's step refers to.
type PlanStep struct {
	N     int           `json:"n"`
	Title string        `json:"title"`
	State PlanStepState `json:"state"`
}

// SessionKind is what ran a background session. Only subagent exists
// today; agent is reserved for work handed to another durable agent.
type SessionKind string

const (
	SessionKindSubagent SessionKind = "subagent"
	SessionKindAgent    SessionKind = "agent"
)

// SessionState is a background session's state.
type SessionState string

const (
	SessionStateQueued    SessionState = "queued"
	SessionStateRunning   SessionState = "running"
	SessionStateDone      SessionState = "done"
	SessionStateErrored   SessionState = "errored"
	SessionStateCancelled SessionState = "cancelled"
)

// BackgroundSession is one dispatched background task.
type BackgroundSession struct {
	ID    string       `json:"id"`
	Kind  SessionKind  `json:"kind"`
	State SessionState `json:"state"`
	// Title is the description the agent gave the task.
	Title string `json:"title"`
	// Model is the friendly name ("Opus 5.5"); Effort the level.
	Model  string `json:"model"`
	Effort string `json:"effort"`
	// ElapsedS is seconds since the session started (or was dispatched,
	// if it never started), frozen at its end once it has one.
	ElapsedS int64 `json:"elapsed_s"`
	// Step is the plan step's N the session works on, when it can be
	// told (see buildBackground in internal/web). A worker inherits its
	// sub-lead's step. The UI indents a session under its step.
	Step *int `json:"step,omitempty"`
	// Error is the failure in words, on an errored session only.
	Error *string `json:"error,omitempty"`
	// Activity is what a live session is doing now ("running
	// file_view"); absent once it ends.
	Activity *string `json:"activity,omitempty"`
	// CallerID is the session that dispatched this one; absent for one
	// the agent dispatched itself.
	CallerID *string `json:"caller_id,omitempty"`
	// Started is when the session was admitted, or dispatched if it has
	// not been.
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	// URL is the app page for the session's transcript.
	URL string `json:"url"`
}

// ---- /api/v1/agents/{slug}/docs/{kind} ----

// AgentDocKind names one of the About tab's documents. habits is the
// document stored as the agent's operating principles.
type AgentDocKind string

const (
	AgentDocKindRole   AgentDocKind = "role"
	AgentDocKindHabits AgentDocKind = "habits"
	AgentDocKindMemory AgentDocKind = "memory"
)

// AgentDoc is GET and PUT /api/v1/agents/{slug}/docs/{kind}.
type AgentDoc struct {
	Kind    AgentDocKind `json:"kind"`
	Content string       `json:"content"`
	// UpdatedAt is null when the document has never been written.
	UpdatedAt *time.Time    `json:"updated_at" tstype:"string | null,required"`
	Stats     AgentDocStats `json:"stats"`
}

// AgentDocStats is the one-line description's numbers.
type AgentDocStats struct {
	Lines int `json:"lines"`
	Chars int `json:"chars"`
	// Notes is how many entries the agent memory holds; memory only.
	Notes *int `json:"notes,omitempty"`
}

// AgentDocPut is the body of PUT /api/v1/agents/{slug}/docs/{kind}.
type AgentDocPut struct {
	Content string `json:"content"`
}
