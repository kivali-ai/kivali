package apitypes

import "time"

// ---- /api/v1/home ----

// Home is GET /api/v1/home: everything the Home screen draws, top to
// bottom. Goals and Readouts are the snapshot's own values (one
// computation serves both), so In flight reads the same here as on
// the stream.
type Home struct {
	Goals    []Goal      `json:"goals"`
	Readouts Readouts    `json:"readouts"`
	Needs    []NeedItem  `json:"needs"`
	Queue    []QueueItem `json:"queue"`
	// AutoRelease is the slider's detent, as in the snapshot.
	AutoRelease AutoRelease `json:"auto_release"`
	// HistoryTotal is how many threads GET /api/v1/home/history pages
	// through.
	HistoryTotal int `json:"history_total"`
}

// PersonRef names an agent, or you (slug "ceo", name "You"). Name is
// the agent's role title, or its slug when it has none or is no
// longer on the team.
type PersonRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// AgentRef is an agent with the state the needs-help row shows.
type AgentRef struct {
	Slug  string     `json:"slug"`
	Name  string     `json:"name"`
	State AgentState `json:"state"`
}

// AttachmentRef is a file on a message. URL downloads it; SizeBytes
// is 0 when the stored file cannot be found.
type AttachmentRef struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
}

// AssignmentRef is the assignment a row is about. Title is empty when the
// tracker no longer knows the id.
type AssignmentRef struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// NeedKind is what a Needs-you row asks of you.
type NeedKind string

const (
	// NeedKindApproval: approve or deny a request.
	NeedKindApproval NeedKind = "approval"
	// NeedKindNotification: acknowledge.
	NeedKindNotification NeedKind = "notification"
	// NeedKindAssignment: an assignment is yours; hand it back with a
	// resolution and outcome (POST /api/v1/assignments/{id}/close).
	NeedKindAssignment NeedKind = "assignment"
	// NeedKindNeedsHelp: an agent has stopped; open its chat.
	NeedKindNeedsHelp NeedKind = "needs_help"
	// NeedKindProposal: an approval that changes the org; review it on
	// the proposal page (ReviewPath).
	NeedKindProposal NeedKind = "proposal"
)

// ProposalKind is which change a proposal makes.
type ProposalKind string

const (
	ProposalKindHire           ProposalKind = "hire"
	ProposalKindRoleUpdate     ProposalKind = "role_update"
	ProposalKindHandbookUpdate ProposalKind = "handbook_update"
	ProposalKindOffboard       ProposalKind = "offboard"
	ProposalKindReorg          ProposalKind = "reorg"
)

// NeedItem is one row of Needs you, newest first; needs-help rows
// follow the requests, in tree order.
type NeedItem struct {
	// ID is what the actions take: the request's message path for
	// approval, notification and proposal rows (the `path` field of
	// POST /api/v1/needs/*), "assignment:<id>" for an assignment, and
	// "agent:<slug>" for a needs-help row.
	ID           string          `json:"id"`
	Kind         NeedKind        `json:"kind"`
	ProposalKind *ProposalKind   `json:"proposal_kind,omitempty"`
	Title        string          `json:"title"`
	From         PersonRef       `json:"from"`
	At           time.Time       `json:"at"`
	BodyMD       string          `json:"body_md"`
	Attachments  []AttachmentRef `json:"attachments"`
	Assignment   *AssignmentRef  `json:"assignment,omitempty"`
	// ReviewPath is the proposal page for a proposal row.
	ReviewPath *string `json:"review_path,omitempty"`
	// Agent is the agent a needs-help row is about.
	Agent *AgentRef `json:"agent,omitempty"`
	// RawURL opens the message file; absent on a needs-help row.
	RawURL *string `json:"raw_url,omitempty"`
}

// QueueKind is a queued message's kind badge.
type QueueKind string

const (
	// QueueKindAssignment is the tracker waking an agent about an
	// assignment. It cannot be bounced; edit the assignment instead.
	QueueKindAssignment QueueKind = "assignment"
	// QueueKindNotice is a message one agent sent others.
	QueueKindNotice QueueKind = "notice"
)

// QueueItem is one queued message: one row however many agents it
// is addressed to. Newest first.
type QueueItem struct {
	// Path is the message path the queue actions take.
	Path        string          `json:"path"`
	Kind        QueueKind       `json:"kind"`
	Title       string          `json:"title"`
	From        PersonRef       `json:"from"`
	To          []PersonRef     `json:"to"`
	BodyMD      string          `json:"body_md"`
	Attachments []AttachmentRef `json:"attachments"`
	Assignment  *AssignmentRef  `json:"assignment,omitempty"`
	QueuedAt    time.Time       `json:"queued_at"`
	// ReleasesAt is when the message releases on its own. Absent when
	// it waits for you (Held).
	ReleasesAt *time.Time `json:"releases_at,omitempty"`
	// Held is true when the message has no release time: it was sent
	// while auto-release was off, or auto-release has been turned off
	// since.
	Held   bool   `json:"held"`
	RawURL string `json:"raw_url"`
}

// ---- /api/v1/home/history ----

// HistoryResponse is GET /api/v1/home/history?before=&limit=: threads
// newest activity first. NextBefore is the cursor for the next page,
// absent on the last one. The cursor is opaque.
type HistoryResponse struct {
	Threads    []HistoryThread `json:"threads"`
	Total      int             `json:"total"`
	NextBefore *string         `json:"next_before,omitempty"`
}

// HistoryThread is a message and every reply to it.
type HistoryThread struct {
	// Path is the first message's path; stable across pages.
	Path         string           `json:"path"`
	LastActivity time.Time        `json:"last_activity"`
	Request      HistoryMessage   `json:"request"`
	Replies      []HistoryMessage `json:"replies"`
	// Decision is the newest decision among the replies: approved,
	// denied or acknowledged. Absent when nobody decided.
	Decision *string `json:"decision,omitempty"`
}

// HistoryMessage is one message in a thread.
type HistoryMessage struct {
	Path string `json:"path"`
	// Type is the message's type as stored (notice, assignment_event,
	// ceo_approval_request, ...); Label is its name in words.
	Type        string          `json:"type"`
	Label       string          `json:"label"`
	Title       string          `json:"title"`
	From        PersonRef       `json:"from"`
	To          []PersonRef     `json:"to"`
	At          time.Time       `json:"at"`
	BodyMD      string          `json:"body_md"`
	Attachments []AttachmentRef `json:"attachments"`
	Assignment  *AssignmentRef  `json:"assignment,omitempty"`
	Decision    *string         `json:"decision,omitempty"`
	RawURL      string          `json:"raw_url"`
}

// ---- /api/v1/queue/* ----

// QueueReleaseRequest is POST /api/v1/queue/release. Note, when set,
// is appended to the message as your note before it goes out.
type QueueReleaseRequest struct {
	Path string `json:"path"`
	Note string `json:"note,omitempty"`
}

// QueueReleaseAllRequest is POST /api/v1/queue/release-all. Without
// Paths every queued message goes; with them, only those (paths no
// longer queued are skipped), and an empty list releases nothing.
// Notes are keyed by path.
type QueueReleaseAllRequest struct {
	Paths []string          `json:"paths,omitempty"`
	Notes map[string]string `json:"notes,omitempty"`
}

// QueueReleaseResponse is how many messages went out.
type QueueReleaseResponse struct {
	Released int `json:"released"`
}

// QueueBounceRequest is POST /api/v1/queue/bounce: the message is
// cancelled and Comment goes back to its sender.
type QueueBounceRequest struct {
	Path    string `json:"path"`
	Comment string `json:"comment"`
}

// ---- /api/v1/needs/* ----

// NeedActionResponse answers POST /api/v1/needs/approve, deny and ack.
// A proposal's answer says what changed, in words, and links where to
// see it; any other request answers with an empty object.
type NeedActionResponse struct {
	Result string `json:"result,omitempty"`
	Link   string `json:"link,omitempty"`
}

// ---- /api/v1/assignments/{id}/close ----

// AssignmentCloseRequest is POST /api/v1/assignments/{id}/close: hand an
// assignment back as done or dropped, with the outcome the asker
// reads first.
type AssignmentCloseRequest struct {
	Resolution AssignmentResolution `json:"resolution"`
	Outcome    string               `json:"outcome"`
}

// AssignmentResolution is how an assignment ended.
type AssignmentResolution string

const (
	AssignmentResolutionDone    AssignmentResolution = "done"
	AssignmentResolutionDropped AssignmentResolution = "dropped"
)

// Empty is the body of a success with nothing to say.
type Empty struct{}
