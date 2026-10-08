// Package apitypes is the wire contract between the Go server and the
// Kivali web app: every JSON body under /api/v1/ and the payload of the
// /org/stream `snapshot` event.
//
// tygo reads this package and writes web/src/api/types.gen.ts (`make
// api-types`), so the TypeScript the screens compile against is derived
// from these declarations and nothing else. Rules:
//
//   - Pure data. No imports beyond the standard library, no methods
//     that carry business logic, no types from internal/store or
//     internal/provider; the handlers map into these.
//   - Every field has an explicit snake_case json tag.
//   - No nil slices in a response. An empty list is `[]`, never null or
//     absent, so the client never needs an existence check. NoNilSlices
//     enforces it on the golden fixtures and in handler tests.
//   - A field that is legitimately absent is a pointer with omitempty,
//     which tygo renders as an optional property.
package apitypes

import "time"

// ErrorBody is every error response under /api/: what happened, then
// who can fix it.
type ErrorBody struct {
	Error string `json:"error"`
	Who   string `json:"who"`
}

// ---- /api/v1/me ----

// Me is GET /api/v1/me: who is signed in and which org they are in.
type Me struct {
	User    MeUser `json:"user"`
	Org     MeOrg  `json:"org"`
	Version string `json:"version"`
	DevMode bool   `json:"dev_mode"`
}

// MeUser is the signed-in person. The session carries only an email,
// so Name and Initials are derived from its local part.
type MeUser struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Initials string `json:"initials"`
}

// MeOrg is the current org's identity. Name is empty until the owner
// sets one; HasLogo says whether /branding/* serves an uploaded mark.
type MeOrg struct {
	Name    string `json:"name"`
	HasLogo bool   `json:"has_logo"`
	// OwnerName is what the person this team works for asked agents to
	// call them; absent when they chose nothing, and always on the
	// public GET /api/v1/login.
	OwnerName string `json:"owner_name,omitempty"`
}

// ---- org snapshot (SSE /org/stream and GET /api/v1/snapshot) ----

// AgentState is one agent's run state as the new UI shows it, derived
// from the liveness flags that travel beside it.
type AgentState string

const (
	AgentStateRunning      AgentState = "running"
	AgentStateWaiting      AgentState = "waiting"
	AgentStateIdle         AgentState = "idle"
	AgentStateNeedsHelp    AgentState = "needs_help"
	AgentStateQuarantined  AgentState = "quarantined"
	AgentStateDisconnected AgentState = "disconnected"
)

// OrgSnapshot is the payload of every /org/stream `snapshot` event and
// of GET /api/v1/snapshot. The server publishes a new one only when the
// marshalled bytes change, so every field must marshal stably: agents
// in tree order, lists sorted.
type OrgSnapshot struct {
	// Agents is every active agent except the CEO, in tree order: a
	// depth-first walk of the reporting lines, siblings by slug.
	Agents  []SnapshotAgent `json:"agents"`
	Inbox   SnapshotInbox   `json:"inbox"`
	Release SnapshotRelease `json:"release"`
	// MessagesTotal is the count of messages on disk; a change is the
	// client's cue to refetch history.
	MessagesTotal int `json:"messages_total"`
	// AssignmentsVersion changes on every tracker write in this process. It
	// is a counter, not a timestamp: compare for inequality only.
	AssignmentsVersion int64 `json:"assignments_version"`
	// Goals are the open top-level assignments that have parts or
	// Done-when conditions, ascending by id.
	Goals    []Goal   `json:"goals"`
	Readouts Readouts `json:"readouts"`
}

// SnapshotAgent is one agent's liveness: State, derived on the server
// from flags that stay there (web.lifecycle), and the count of
// background jobs it waits on.
type SnapshotAgent struct {
	Slug string `json:"slug"`
	// Name is the label the sidebar tree shows: the role title, or the
	// slug when the agent has none.
	Name      string `json:"name"`
	RoleTitle string `json:"role_title"`
	// Icon is the role icon its avatar shows; empty for initials only.
	Icon string `json:"icon"`
	// ReportsTo is the manager's slug; "ceo" for the CEO's direct
	// reports.
	ReportsTo string `json:"reports_to"`
	// Depth is the tree depth: 1 for the CEO's direct reports.
	Depth int        `json:"depth"`
	State AgentState `json:"state"`
	// ContextPct is how full the current chat's context window is,
	// 0..100. 0 when the agent has no chat history.
	ContextPct int `json:"context_pct"`
	// WaitingTasks is how many background subagent jobs this agent
	// still owns; the sidebar says so beside a waiting agent.
	WaitingTasks int `json:"waiting_tasks,omitempty"`
}

// SnapshotInbox is the CEO-facing aggregate: what needs a decision and
// what is queued for release.
type SnapshotInbox struct {
	Unactioned int `json:"unactioned"`
	// PendingPaths is every queued message path, de-duplicated and
	// sorted.
	PendingPaths []string `json:"pending_paths"`
	// CEOPaths is the request path of every unactioned item in the
	// CEO's inbox, sorted.
	CEOPaths []string `json:"ceo_paths"`
	// AutoRelease is the slider's detent: now, 30s, 2m, 5m, 20m or off.
	AutoRelease AutoRelease `json:"auto_release"`
}

// SnapshotRelease is the release engine's state.
type SnapshotRelease struct {
	Running      bool `json:"running"`
	ActiveAgents int  `json:"active_agents"`
	PendingDocs  int  `json:"pending_docs"`
}

// Goal is an open top-level assignment with its rolled-up progress.
type Goal struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	// Owner is the goal's assignee.
	Owner string `json:"owner"`
	// Done and Total count Done-when conditions when the goal declares
	// any; otherwise its parts, leaving dropped parts out of both.
	Done  int `json:"done"`
	Total int `json:"total"`
	// Blocked lists every dependency wait inside the goal: an open
	// assignment in it (the goal or a part at any depth) that waits on
	// another open assignment. Waiting on its own parts is progress,
	// not blockage, so those edges are left out.
	Blocked []GoalBlocker `json:"blocked"`
	// Workers are the distinct assignees of the goal's open parts at
	// any depth, sorted.
	Workers []string `json:"workers"`
}

// GoalBlocker is one dependency wait: assignment ID waits on
// assignment On, whose title is OnTitle and whose assignee is
// OnAssignee (absent when On has no assignee).
type GoalBlocker struct {
	ID         int        `json:"id"`
	On         int        `json:"on"`
	OnTitle    string     `json:"on_title"`
	OnAssignee *PersonRef `json:"on_assignee,omitempty"`
}

// Readouts are Home's In flight numbers.
type Readouts struct {
	// Working is how many agents are running a turn or waiting on
	// their own background work.
	Working int `json:"working"`
	// Blocked is how many open assignments, not on hold, wait on
	// another open assignment (the same edges Goal.Blocked lists).
	Blocked int `json:"blocked"`
	// ClosedWeek is how many assignments closed in the last seven days.
	ClosedWeek int `json:"closed_week"`
	// SpendToday is US dollars since 00:00 UTC; Spend7d over the last
	// seven days. Both priced from token counts at list rates; usage on
	// a model with no price is left out.
	SpendToday float64 `json:"spend_today"`
	Spend7d    float64 `json:"spend_7d"`
}

// ---- /api/v1/agents ----

// AgentSummary is one agent in a list: the snapshot's identity fields
// plus what it runs on.
type AgentSummary struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	RoleTitle string `json:"role_title"`
	Icon      string `json:"icon"`
	ReportsTo string `json:"reports_to"`
	Depth     int    `json:"depth"`
	Model     string `json:"model"`
	// ModelLabel is Model in words, from the provider ("Opus 5.5"); the
	// id itself when the provider does not know it.
	ModelLabel string     `json:"model_label"`
	Effort     string     `json:"effort"`
	ContextPct int        `json:"context_pct"`
	State      AgentState `json:"state"`
	Created    time.Time  `json:"created"`
	// ArchivedAt is set only on archived agents.
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
}

// AgentsResponse is GET /api/v1/agents. Agents is in tree order;
// Archived is by slug, with depth 0 and state idle.
type AgentsResponse struct {
	Agents   []AgentSummary `json:"agents"`
	Archived []AgentSummary `json:"archived"`
}

// ModelOption is one entry of the model picker, as the org's provider
// describes it. Legacy marks the agent's current pin when the picker no
// longer offers it; a legacy pin the provider does not know carries
// only its id (as id and label) and no efforts.
type ModelOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Legacy marks the agent's own pin when the picker no longer
	// offers it.
	Legacy bool `json:"legacy"`
	// Current is true for a model a person may newly pick.
	Current bool `json:"current"`
	// ContextWindow is the tokens the model holds.
	ContextWindow int `json:"context_window"`
	// Efforts are the reasoning levels the model offers, low to high,
	// one of them the default. Empty: the model has no reasoning
	// control, and the effort chip hides.
	Efforts []EffortOption `json:"efforts"`
	// Provider names the provider that runs the model ("claude").
	Provider string `json:"provider"`
}

// EffortOption is one reasoning level a model offers. An agent's or a
// subagent's effort field holds the ID.
type EffortOption struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

// ContextFill is the chat header's context gauge.
type ContextFill struct {
	Pct    int `json:"pct"`
	Tokens int `json:"tokens"`
	Limit  int `json:"limit"`
}

// AgentCounts are the numbers on the agent's tabs.
type AgentCounts struct {
	// Background is how many background tasks the agent has running.
	Background int `json:"background"`
	PastChats  int `json:"past_chats"`
}

// AgentDetail is GET /api/v1/agents/{slug}.
type AgentDetail struct {
	AgentSummary `tstype:",extends,required"`
	// RoleLine is the first line of the agent's role document, or its
	// role title when the document is empty.
	RoleLine string        `json:"role_line"`
	Models   []ModelOption `json:"models"`
	Context  ContextFill   `json:"context"`
	Counts   AgentCounts   `json:"counts"`
	Archived bool          `json:"archived"`
}

// ---- /api/v1/auto-release ----

// AutoRelease is a detent on the auto-release slider.
type AutoRelease string

const (
	AutoReleaseNow AutoRelease = "now"
	AutoRelease30s AutoRelease = "30s"
	AutoRelease2m  AutoRelease = "2m"
	AutoRelease5m  AutoRelease = "5m"
	AutoRelease20m AutoRelease = "20m"
	AutoReleaseOff AutoRelease = "off"
)

// AutoReleaseRequest is the body of POST /api/v1/auto-release.
type AutoReleaseRequest struct {
	Value AutoRelease `json:"value"`
}

// AutoReleaseResponse echoes the setting now in force.
type AutoReleaseResponse struct {
	Value AutoRelease `json:"value"`
}
