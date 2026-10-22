package apitypes

import (
	"encoding/json"
	"time"
)

// Integrations: the external services agents can act on. An
// integration is installed from a bundle (a zip the Integrations
// developer publishes, or one you upload), connected with a token, and
// then granted to agents one at a time with an access form the
// integration itself describes. Everything here is operator-facing:
// actions in words, hosts, secrets by title, never the wire.

// ---- /api/v1/org/integrations ----

// Integrations is GET /api/v1/org/integrations: every installed
// integration, sorted by display name.
type Integrations struct {
	Integrations []IntegrationSummary `json:"integrations"`
}

// IntegrationStatus is the one word the list shows for an integration.
type IntegrationStatus string

const (
	// IntegrationStatusPending: no version has been approved yet.
	IntegrationStatusPending IntegrationStatus = "pending"
	// IntegrationStatusNotConnected: approved, on, but no connection.
	IntegrationStatusNotConnected IntegrationStatus = "not_connected"
	// IntegrationStatusConnected: approved, on, with a connection.
	IntegrationStatusConnected IntegrationStatus = "connected"
	// IntegrationStatusOff: switched off; agents cannot use it.
	IntegrationStatusOff IntegrationStatus = "off"
	// IntegrationStatusNotRunning: approved and on, but the integration
	// is not running right now (starting, or stopped after a failure);
	// Test connection and agents' calls wait for it. An integration that
	// ships with Kivali is never not running.
	IntegrationStatusNotRunning IntegrationStatus = "not_running"
	// IntegrationStatusNotIncluded: a built-in integration this version
	// of Kivali does not carry (another version shipped it). Nothing
	// runs it and agents cannot use it; it can be removed.
	IntegrationStatusNotIncluded IntegrationStatus = "not_included"
)

// IntegrationSummary is one row of the list.
type IntegrationSummary struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Version is the approved version in use; empty while no version
	// has been approved.
	Version string            `json:"version"`
	Status  IntegrationStatus `json:"status"`
	Enabled bool              `json:"enabled"`
	Builtin bool              `json:"builtin"`
	// Agents is how many agents have access.
	Agents int `json:"agents"`
	// PendingVersion is a version waiting for your approval, if any.
	PendingVersion *string `json:"pending_version,omitempty"`
}

// ---- /api/v1/org/integrations/{name} ----

// IntegrationPage is GET /api/v1/org/integrations/{name} and the
// answer to every mutation on the integration: everything its page
// draws. Connection values are never in it; a secret field says only
// whether it is set.
type IntegrationPage struct {
	Summary IntegrationSummary `json:"summary"`
	// Author is who built it, as the bundle says.
	Author string `json:"author"`
	// Hosts is every host the integration reaches.
	Hosts   []string            `json:"hosts"`
	Actions []IntegrationAction `json:"actions"`
	// ConnectionFields is the connection form, in the bundle's order,
	// with what the first connection holds.
	ConnectionFields []ConnectionField `json:"connection_fields"`
	// Connection is the first connection (the oldest), absent while
	// there is none: the one connection of an integration that takes
	// one.
	Connection *ConnectionState `json:"connection,omitempty"`
	// Connections is every connection, oldest first; empty while there
	// is none.
	Connections []ConnectionState `json:"connections"`
	// Multiple is true for an integration that takes several
	// connections, each with a label; each agent's access names the one
	// it uses.
	Multiple bool `json:"multiple"`
	// ConnectionTest is true when the version in use has a connection
	// test (its manifest names kivali.verify). Only then does the page
	// offer Test connection, and only then does a connection carry its
	// last result.
	ConnectionTest bool `json:"connection_test"`
	// AccessForm is the access form one agent's grant fills in. Absent
	// when access is all-or-nothing.
	AccessForm *AccessForm `json:"access_form,omitempty"`
	// Access is one row per agent with access, sorted by name.
	Access []AccessRow `json:"access"`
	// AgentsWithout lists the active agents with no access, sorted by
	// name, so a row can be added for one.
	AgentsWithout []PersonRef `json:"agents_without"`
	// Versions is every version stored, newest first.
	Versions []IntegrationVersionSummary `json:"versions"`
	// CanRemove is false for a built-in integration this version of
	// Kivali carries.
	CanRemove bool `json:"can_remove"`
}

// IntegrationAction is one tool the integration gives agents, in words.
type IntegrationAction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ConnectionField is one field of the connection form, from the
// bundle's user_config.
type ConnectionField struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Sensitive fields are secrets: kept in a Kubernetes Secret, never
	// shown again. Set says whether a value is stored.
	Sensitive bool `json:"sensitive"`
	Required  bool `json:"required"`
	Set       bool `json:"set"`
	// Value is the stored value of a field that is not sensitive;
	// absent for a sensitive field and for one not set.
	Value *string `json:"value,omitempty"`
}

// ConnectionState is one connection's standing.
type ConnectionState struct {
	ID string `json:"id"`
	// Label is what the owner calls the connection ("Company GitLab"),
	// or, for an integration that takes several, the one the
	// integration derives from its fields when the owner gave none
	// (Git: its host). The page of an integration that takes one
	// connection does not ask for a label, so it is usually empty there.
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	// Fields is the connection form with what this connection holds:
	// the value of each field that is not sensitive, and which secrets
	// are set.
	Fields []ConnectionField `json:"fields"`
	// VerifiedAt and Verified say what Test connection last found;
	// absent until it has run, and while the version in use has no
	// connection test. VerifyMessage is the integration's own words
	// about the result.
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
	Verified      *bool      `json:"verified,omitempty"`
	VerifyMessage *string    `json:"verify_message,omitempty"`
	// ReconnectNeeded is true when the connection's secrets are not in
	// this cluster (a backup restored into another install, or the
	// Secret removed by hand): enter them again.
	ReconnectNeeded bool `json:"reconnect_needed"`
}

// ConnectionWrite is the body of PUT
// /api/v1/org/integrations/{name}/connection, POST .../connections and
// PUT .../connections/{id}: field values by key, and the label. A
// sensitive field left out keeps its stored value; an empty string
// clears it. A field that is not sensitive is replaced by what is
// sent. A required field with no value, stored or sent, is a 400.
type ConnectionWrite struct {
	// Label is what the owner calls the connection. Required for an
	// integration that takes several connections (a built-in may give
	// one: Git's is the host); empty keeps the stored label. Unique
	// within the integration, ignoring case (409).
	Label  string            `json:"label,omitempty"`
	Fields map[string]string `json:"fields"`
}

// AccessForm is the access form the integration describes for one
// agent: a flat list of fields.
type AccessForm struct {
	Fields []AccessField `json:"fields"`
}

// AccessFieldKind is what kind of value an access field takes.
type AccessFieldKind string

const (
	// AccessFieldKindText is one string.
	AccessFieldKindText AccessFieldKind = "text"
	// AccessFieldKindChoice is one string from Options.
	AccessFieldKindChoice AccessFieldKind = "choice"
	// AccessFieldKindSwitch is a boolean.
	AccessFieldKindSwitch AccessFieldKind = "switch"
	// AccessFieldKindList is a list of strings, free text.
	AccessFieldKindList AccessFieldKind = "list"
	// AccessFieldKindChoices is a list of strings from Options.
	AccessFieldKindChoices AccessFieldKind = "choices"
)

// AccessField is one field of the access form.
type AccessField struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Kind        AccessFieldKind `json:"kind"`
	Required    bool            `json:"required"`
	// Options are the fixed choices of a choice or choices field, from
	// the bundle. Empty for the other kinds, and for a field whose
	// options the integration lists itself (see Choices).
	Options []string `json:"options"`
	// Choices is true when the integration lists this field's options
	// live, through GET .../choices/{key}.
	Choices bool `json:"choices"`
	// Danger is true for a switch whose "on" the owner should choose
	// with care (the manifest's x-kivali-danger): the form draws it in
	// a warning tone with its description in view.
	Danger bool `json:"danger"`
}

// AccessRow is one agent's access.
type AccessRow struct {
	Agent PersonRef `json:"agent"`
	// Access is the filled form, as the integration sees it.
	Access json.RawMessage `json:"access"`
	// Summary is the access in words, for the list and the agent's
	// own page.
	Summary string    `json:"summary"`
	SetAt   time.Time `json:"set_at"`
	// ConnectionID is the connection the access was set for, and the
	// one the agent's calls use; ConnectionLabel is its label, empty
	// when it has none or is no longer there.
	ConnectionID    string `json:"connection_id"`
	ConnectionLabel string `json:"connection_label"`
	// Invalid says why this row no longer fits the access form of the
	// version in use (a version approved or rolled back to changed
	// it), or that its connection is gone. An invalid row grants
	// nothing until it is set again.
	Invalid *string `json:"invalid,omitempty"`
}

// AccessWrite is the body of PUT
// /api/v1/org/integrations/{name}/access/{slug}: the filled access
// form, and the connection it is for. For an integration without a
// form, send {}.
type AccessWrite struct {
	Access json.RawMessage `json:"access"`
	// ConnectionID is the connection the agent's calls use. Required
	// when the integration has more than one connection; with one, it
	// may be left out. Missing or unknown where it is needed is a 400.
	ConnectionID string `json:"connection_id,omitempty"`
}

// AccessChoices is GET /api/v1/org/integrations/{name}/choices/{key}:
// the options the integration lists for a field, through the
// connection (?connection=<id> names one of several; without it, the
// first).
type AccessChoices struct {
	Options []string `json:"options"`
}

// IntegrationVersionState is where a version stands.
type IntegrationVersionState string

const (
	// IntegrationVersionStatePending waits for your approval.
	IntegrationVersionStatePending IntegrationVersionState = "pending"
	// IntegrationVersionStateApproved is the version in use.
	IntegrationVersionStateApproved IntegrationVersionState = "approved"
	// IntegrationVersionStateRejected was turned down and never ran.
	IntegrationVersionStateRejected IntegrationVersionState = "rejected"
	// IntegrationVersionStateSuperseded was in use before a later
	// approval; it can be rolled back to.
	IntegrationVersionStateSuperseded IntegrationVersionState = "superseded"
)

// IntegrationVersionSummary is one version in the page's list.
type IntegrationVersionSummary struct {
	Version     string                  `json:"version"`
	State       IntegrationVersionState `json:"state"`
	PublishedAt time.Time               `json:"published_at"`
	// PublishedBy is the agent that published it, or you for an
	// upload.
	PublishedBy PersonRef  `json:"published_by"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	// Notes are the publisher's version notes, Markdown.
	NotesMD string `json:"notes_md"`
}

// ---- /api/v1/org/integrations/{name}/versions/{version} ----

// IntegrationVersion is GET
// /api/v1/org/integrations/{name}/versions/{version}: the install
// screen. It says, in words, what the version is, who built it, every
// host it reaches, every secret the connection needs, every action,
// what the access form asks, and the bundle's size. Approving runs it;
// nothing runs before. It is also the answer of POST
// .../versions/{version}/approve and .../reject (a pending version),
// and of POST .../versions/{version}/rollback, which makes a
// superseded version the one in use again in place of the current one
// (409 for any other state).
type IntegrationVersion struct {
	Name        string                  `json:"name"`
	DisplayName string                  `json:"display_name"`
	Description string                  `json:"description"`
	Version     string                  `json:"version"`
	State       IntegrationVersionState `json:"state"`
	Author      string                  `json:"author"`
	PublishedAt time.Time               `json:"published_at"`
	PublishedBy PersonRef               `json:"published_by"`
	ApprovedAt  *time.Time              `json:"approved_at,omitempty"`
	NotesMD     string                  `json:"notes_md"`
	// Runtime is what runs the bundle: python; empty for a built-in
	// integration, which runs inside Kivali.
	Runtime string `json:"runtime"`
	// ConnectionMethod is how the connection is made: token.
	ConnectionMethod string              `json:"connection_method"`
	Hosts            []string            `json:"hosts"`
	ConnectionFields []ConnectionField   `json:"connection_fields"`
	Actions          []IntegrationAction `json:"actions"`
	AccessForm       *AccessForm         `json:"access_form,omitempty"`
	BundleBytes      int64               `json:"bundle_bytes"`
	FileCount        int                 `json:"file_count"`
	// Current is true when this is the version in use now.
	Current bool `json:"current"`
	// Builtin is true for a version of an integration that ships with
	// Kivali: it has no bundle, needs no approval and comes with
	// Kivali's updates, so it is never approved, rejected or rolled
	// back to here (409).
	Builtin bool `json:"builtin"`
	// CurrentVersion is the version in use now, when there is one and
	// it is not this one, so the screen can say what approving replaces.
	CurrentVersion *string `json:"current_version,omitempty"`
	// Changes is what this version changes from CurrentVersion, in
	// words, one line each: hosts, connection fields, actions and
	// access form fields added or removed ("Hosts added:
	// api.example.com"). Empty without a CurrentVersion, and when none
	// of those differ.
	Changes []string `json:"changes"`
	// Findings are what the bundle check reported for this version:
	// warnings only, since a bundle with an error is never stored.
	Findings []string `json:"findings"`
}

// IntegrationUploadRefused is the 400 body of POST
// /api/v1/org/integrations when the bundle does not pass the check:
// every finding, in words.
type IntegrationUploadRefused struct {
	Error    string   `json:"error"`
	Who      string   `json:"who"`
	Findings []string `json:"findings"`
}

// ---- /api/v1/org/integrations/{name}/audit ----

// IntegrationAudit is GET /api/v1/org/integrations/{name}/audit: the
// latest calls, newest first.
type IntegrationAudit struct {
	Rows []IntegrationAuditRow `json:"rows"`
}

// IntegrationAuditRow is one call through the integration, or your
// roll back to another version.
type IntegrationAuditRow struct {
	At time.Time `json:"at"`
	// Agent is who called; you, for Test connection, the options an
	// access form lists and a roll back.
	Agent PersonRef `json:"agent"`
	// Action is the action called, without the integration's prefix;
	// rollback, with you as the agent, for a roll back.
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	// Reason says why a call was refused, or what failed; empty when
	// the call went through. For a roll back it names the versions:
	// "from version 1.1.0 to version 1.0.0".
	Reason     string `json:"reason"`
	DurationMS int64  `json:"duration_ms"`
}

// ---- /api/v1/agents/{slug}/integrations ----

// AgentIntegrations is GET /api/v1/agents/{slug}/integrations: the
// agent's Integrations tab. Editing happens on the integration's page
// under Org.
type AgentIntegrations struct {
	// Connected is every integration this agent has access to, with
	// its access in words.
	Connected []AgentIntegration `json:"connected"`
	// Available is every other installed integration.
	Available []AgentIntegration `json:"available"`
}

// AgentIntegration is one integration as the agent's tab lists it.
type AgentIntegration struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// Status is the integration's. Under Connected, for an integration
	// that takes several connections, connected or not connected is the
	// connection's the agent's access names: not connected while that
	// one needs its secrets again.
	Status IntegrationStatus `json:"status"`
	// Summary is the agent's access in words; empty under Available.
	Summary string `json:"summary"`
	// ConnectionLabel is the label of the connection the agent's access
	// names, for an integration that takes several connections: the row
	// reads "Git (Company GitLab)". Empty otherwise.
	ConnectionLabel string `json:"connection_label,omitempty"`
	// Invalid is set when the agent's access no longer fits the
	// integration's access form and grants nothing until set again.
	Invalid *string `json:"invalid,omitempty"`
}

// ---- /api/v1/org/integrations/{name}/logs ----

// IntegrationLogs is GET /api/v1/org/integrations/{name}/logs: the
// integration's latest log lines, oldest first, with connection secrets
// already replaced.
type IntegrationLogs struct {
	Lines []IntegrationLogLine `json:"lines"`
}

// IntegrationLogLine is one line the integration, or Kivali about it,
// wrote.
type IntegrationLogLine struct {
	At time.Time `json:"at"`
	// Level is the integration's own level (info, warning, error), or
	// stderr for a line it wrote to its error output.
	Level   string `json:"level"`
	Message string `json:"message"`
}
