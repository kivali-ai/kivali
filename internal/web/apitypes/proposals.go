package apitypes

import "time"

// ---- /api/v1/proposals/{path...} ----

// Proposal is GET /api/v1/proposals/{path...}: one proposal to change
// the org (a hire, a role update, a handbook update, an offboard
// or a reorg), everything its review page draws. Path is the request
// message's path, the `path` that POST /api/v1/needs/approve and deny
// take.
type Proposal struct {
	Path     string       `json:"path"`
	Kind     ProposalKind `json:"kind"`
	Title    string       `json:"title"`
	Proposer PersonRef    `json:"proposer"`
	// ProposedAt is when the request was sent.
	ProposedAt time.Time `json:"proposed_at"`
	// ReasonMD is the request's body: why the proposer asks.
	ReasonMD    string          `json:"reason_md"`
	Attachments []AttachmentRef `json:"attachments"`
	Summary     ProposalSummary `json:"summary"`
	// Docs are the documents the proposal writes, in order. Empty for
	// an offboard and a reorg, which change no document.
	Docs []ProposalDoc `json:"docs"`
	// Resolved is present once you have approved or denied it.
	Resolved *ProposalResolution `json:"resolved,omitempty"`
	// OffboardSentence is what happens to an offboarded agent's files,
	// said beside the Approve button. Present on an offboard only.
	OffboardSentence *string `json:"offboard_sentence,omitempty"`
}

// ProposalSummary is the summary card: the agent it is about (a hire,
// a role update, an offboard), the moves of a reorg, and a line per
// fact worth saying.
type ProposalSummary struct {
	Agent *ProposalAgent `json:"agent,omitempty"`
	// Moves is a reorg's reporting-line changes; empty for any other
	// kind.
	Moves []ProposalMove `json:"moves"`
	Facts []ProposalFact `json:"facts"`
}

// ProposalAgent is the agent tile. For a hire it describes the agent
// the approval creates: its role title as its name, and the model and
// effort a new agent starts on.
type ProposalAgent struct {
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	RoleTitle string    `json:"role_title"`
	Icon      string    `json:"icon"`
	ReportsTo PersonRef `json:"reports_to"`
	// Model is the model id; ModelLabel is its name in words
	// ("Opus 5.5").
	Model      string `json:"model"`
	ModelLabel string `json:"model_label"`
	Effort     string `json:"effort"`
}

// ProposalMove is one reporting-line change in a reorg: Slug moves
// under To.
type ProposalMove struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	// From is whom the agent reports to now. Absent when that is
	// already To, as it is once an approved move has landed.
	From *PersonRef `json:"from,omitempty"`
	To   PersonRef  `json:"to"`
	// Failed says why an approved move could not be made.
	Failed *string `json:"failed,omitempty"`
}

// ProposalFact is one "label · value" line on the summary card.
type ProposalFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ProposalDocKey says which document a block is, for its icon.
type ProposalDocKey string

const (
	ProposalDocKeyRole     ProposalDocKey = "role"
	ProposalDocKeyMemory   ProposalDocKey = "memory"
	ProposalDocKeyHandbook ProposalDocKey = "handbook"
)

// ProposalDoc is one document the proposal writes, whole: the client
// splits it into sections at `##` headings and diffs them. Before is
// the document as it stands now, absent for a new document (a hire's)
// and once an approved change has replaced it, when After is what is
// live. Both end in exactly one newline.
type ProposalDoc struct {
	Key   ProposalDocKey `json:"key"`
	Title string         `json:"title"`
	// Meta is the block's mono line: "role.md · 84 lines".
	Meta   string  `json:"meta"`
	Before *string `json:"before,omitempty"`
	After  string  `json:"after"`
	// BeforeIsCurrent is true on a denied proposal: Before is the
	// document as it is now, which may have changed since the proposal
	// was made (no earlier version is kept), so the page should say its
	// diff is against the current file. False while the proposal waits,
	// when the current file is exactly what an approval would replace.
	BeforeIsCurrent bool `json:"before_is_current,omitempty"`
}

// ProposalResolution is how you answered: the same result sentence and
// link POST /api/v1/needs/approve or deny returned.
type ProposalResolution struct {
	Approved bool      `json:"approved"`
	At       time.Time `json:"at"`
	Result   string    `json:"result"`
	Link     string    `json:"link,omitempty"`
}
