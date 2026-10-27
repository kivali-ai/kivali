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
	// NeedKindWebSignIn: a web task waits for you to sign in through
	// its browser window; open the browser (Subagent), sign in, close
	// the window.
	NeedKindWebSignIn NeedKind = "web_sign_in"
	// NeedKindWebUpload: a web task waits, before it starts, for you to
	// approve uploading files to a site (WebUpload). Approve or deny,
	// with a reason the task's agent reads, through
	// POST /api/v1/needs/approve and /deny with the row's ID as path.
	NeedKindWebUpload NeedKind = "web_upload"
	// NeedKindWebBudget: a web task was refused an action that would
	// have sent more data out than web tasks may (WebBudget).
	// Acknowledge it through POST /api/v1/needs/ack with the row's ID
	// as path.
	NeedKindWebBudget NeedKind = "web_budget"
	// NeedKindIntegrationRequirement: one of an installed integration's
	// requirements is not met (the body says what is), such as a sign-in
	// the service no longer accepts. Open the integration's page
	// (ReviewPath), where its Fix is. The row leaves once the
	// requirement is met, or the integration is disabled or
	// uninstalled.
	NeedKindIntegrationRequirement NeedKind = "integration_requirement"
	// NeedKindIntegrationHold: an agent's call of an integration waits
	// for you to approve it before it runs (IntegrationHold). Approve
	// or deny, with a note the agent reads, through
	// POST /api/v1/needs/approve and /deny with the row's ID as path.
	// Nobody answering within a day is a denial.
	NeedKindIntegrationHold NeedKind = "integration_hold"
)

// IntegrationHoldRef is the call an integration_hold row asks you to
// approve: the integration (its page is /org/integrations/{integration})
// and its display name, the agent that called it, the action without
// the integration's prefix, what it does in a few words (Title), each
// field exactly as it will go, who it reaches, and Taint: what the
// agent read from outside your domain before asking, in words, empty
// when it read nothing from outside. Fields, Recipients and Taint are
// text, shown as text and never rendered: a field may quote an outside
// message.
type IntegrationHoldRef struct {
	Integration string      `json:"integration"`
	Display     string      `json:"display"`
	Agent       string      `json:"agent"`
	Action      string      `json:"action"`
	Title       string      `json:"title"`
	Fields      []HoldField `json:"fields"`
	Recipients  []string    `json:"recipients"`
	Taint       string      `json:"taint"`
}

// HoldField is one line of what a held call does: a label ("Subject")
// and the value exactly as it will go.
type HoldField struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// WebUploadRef is the upload a web_upload row asks you to approve: the
// task (its page is /agents/{parent}/subagents/{id}), the site it is
// held to, where it starts, the files it may upload (paths under the
// agent's /files/) and the task's prompt, the agent's words.
type WebUploadRef struct {
	Parent      string   `json:"parent"`
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Site        string   `json:"site"`
	StartURL    string   `json:"start_url"`
	Files       []string `json:"files"`
	Prompt      string   `json:"prompt"`
}

// WebBudgetLimit is the limit a refused web action went over.
type WebBudgetLimit string

const (
	// WebBudgetLimitAction: one action may send 2 KB.
	WebBudgetLimitAction WebBudgetLimit = "action"
	// WebBudgetLimitTask: one task may send 8 KB.
	WebBudgetLimitTask WebBudgetLimit = "task"
	// WebBudgetLimitDay: an agent's web tasks may send 32 KB a day.
	WebBudgetLimitDay WebBudgetLimit = "day"
)

// WebBudgetRef is what a web_budget row reports: the task, the site
// the data would have gone to (empty when core cannot tell), the size
// of the refused action in bytes and the limit it went over.
type WebBudgetRef struct {
	Parent      string         `json:"parent"`
	ID          string         `json:"id"`
	Description string         `json:"description"`
	Site        string         `json:"site"`
	Bytes       int            `json:"bytes"`
	Limit       WebBudgetLimit `json:"limit"`
}

// SubagentRef is the web task a web_sign_in row is about: the task's
// page is /agents/{parent}/subagents/{id}, its browser window the
// same path with /browser.
type SubagentRef struct {
	Parent      string          `json:"parent"`
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Site        string          `json:"site"`
	Reason      WebSignInReason `json:"reason"`
	// PageURL and PageTitle are the page the task's window shows, as
	// the browser reported them; empty when core knows no page. Page
	// text: shown as text, never used as a link.
	PageURL   string `json:"page_url"`
	PageTitle string `json:"page_title"`
}

// ProposalKind is which change a proposal makes.
type ProposalKind string

const (
	ProposalKindHire            ProposalKind = "hire"
	ProposalKindRoleUpdate      ProposalKind = "role_update"
	ProposalKindHandbookUpdate  ProposalKind = "handbook_update"
	ProposalKindOffboard        ProposalKind = "offboard"
	ProposalKindReorg           ProposalKind = "reorg"
	ProposalKindSoftwareInstall ProposalKind = "software_install"
	// ProposalKindIntegrationInstall asks you to approve a version of an
	// integration; its review page is the version's install screen under
	// Org, Integrations.
	ProposalKindIntegrationInstall ProposalKind = "integration_install"
)

// NeedItem is one row of Needs you, newest first; needs-help rows
// follow the requests, in tree order.
type NeedItem struct {
	// ID is what the actions take: the request's message path for
	// approval, notification and proposal rows (the `path` field of
	// POST /api/v1/needs/*), "assignment:<id>" for an assignment,
	// "agent:<slug>" for a needs-help row, and
	// "web_sign_in:<agent>/<task>", "web_upload:<agent>/<task>" and
	// "web_budget:<agent>/<task>" for a web task's rows, the last two
	// being the path POST /api/v1/needs/* takes for them,
	// "integration_requirement:<integration>/<requirement>" for an
	// installed integration's requirement that is not met, and
	// "integration_hold:<id>" for an agent's call waiting for your
	// approval, the path POST /api/v1/needs/approve and /deny take.
	ID           string          `json:"id"`
	Kind         NeedKind        `json:"kind"`
	ProposalKind *ProposalKind   `json:"proposal_kind,omitempty"`
	Title        string          `json:"title"`
	From         PersonRef       `json:"from"`
	At           time.Time       `json:"at"`
	BodyMD       string          `json:"body_md"`
	Attachments  []AttachmentRef `json:"attachments"`
	Assignment   *AssignmentRef  `json:"assignment,omitempty"`
	// ReviewPath is the proposal page for a proposal row, and the
	// integration's page for an integration_requirement row.
	ReviewPath *string `json:"review_path,omitempty"`
	// Packages are the Debian packages an Install software request
	// asks for. That request is an approval row, answered in place,
	// with ProposalKind software_install; empty on every other row.
	// Never null.
	Packages []SoftwarePackage `json:"packages"`
	// Agent is the agent a needs-help row is about.
	Agent *AgentRef `json:"agent,omitempty"`
	// Subagent is the web task a web_sign_in row is about.
	Subagent *SubagentRef `json:"subagent,omitempty"`
	// WebUpload is the upload a web_upload row asks you to approve.
	WebUpload *WebUploadRef `json:"web_upload,omitempty"`
	// WebBudget is what a web_budget row reports.
	WebBudget *WebBudgetRef `json:"web_budget,omitempty"`
	// IntegrationHold is the call an integration_hold row asks you to
	// approve.
	IntegrationHold *IntegrationHoldRef `json:"integration_hold,omitempty"`
	// RawURL opens the message file; absent on a needs-help row, a web
	// task's rows, an integration_requirement row and an
	// integration_hold row.
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
