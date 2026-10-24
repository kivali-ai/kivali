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

// IntegrationState is where an integration stands in its lifecycle:
// available, setting up, installed or off; or, before that, waiting
// for its first approval, or not in this version of Kivali.
type IntegrationState string

const (
	// IntegrationStateAvailable: ready to install. A built-in while this
	// version of Kivali carries it, a bundle once a version is approved.
	// Install runs its setup flow.
	IntegrationStateAvailable IntegrationState = "available"
	// IntegrationStateSettingUp: its setup flow runs (IntegrationPage's
	// Flow). A flow that fails or is cancelled keeps nothing, and the
	// integration is available again.
	IntegrationStateSettingUp IntegrationState = "setting_up"
	// IntegrationStateInstalled: the connection exists, and agents can
	// be given access. Its requirements say whether it works.
	IntegrationStateInstalled IntegrationState = "installed"
	// IntegrationStateOff: switched off. Agents cannot use it; the
	// connection and their access are kept.
	IntegrationStateOff IntegrationState = "off"
	// IntegrationStatePending: no version has been approved yet.
	IntegrationStatePending IntegrationState = "pending"
	// IntegrationStateNotIncluded: a built-in integration this version
	// of Kivali does not carry (another version shipped it). Nothing
	// runs it and agents cannot use it; it can be uninstalled.
	IntegrationStateNotIncluded IntegrationState = "not_included"
)

// IntegrationSummary is one row of the list.
type IntegrationSummary struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	// Version is the approved version in use; empty while no version
	// has been approved.
	Version string           `json:"version"`
	State   IntegrationState `json:"state"`
	// Needs is how many of its requirements are not met, each a row of
	// Needs you; 0 for an integration that is not installed or is off.
	Needs   int  `json:"needs"`
	Enabled bool `json:"enabled"`
	Builtin bool `json:"builtin"`
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
	Hosts []string `json:"hosts"`
	// Packages is the Debian packages its pod installs each time it
	// starts (kivali.packages); empty for none.
	Packages []string            `json:"packages"`
	Actions  []IntegrationAction `json:"actions"`
	// ConnectionFields is the connection form, in the bundle's order,
	// with what the connection holds. A connection made by
	// signing in adds the client id and the client secret.
	ConnectionFields []ConnectionField `json:"connection_fields"`
	// ConnectionMethod is how the connection is made: token, device,
	// browser or setup. Empty with no version at all.
	ConnectionMethod string `json:"connection_method"`
	// SignInClient is whose app a sign-in uses: operator, an app you
	// register and whose client id and secret the connection form
	// takes; registered, a client Kivali registers itself with the
	// service, so the form takes neither; setup, the app the
	// integration's setup flow makes. Empty for a connection that signs
	// nothing in.
	SignInClient string `json:"sign_in_client"`
	// Scopes is what a sign-in connection asks the vendor for; empty
	// otherwise.
	Scopes []string `json:"scopes"`
	// SetupNotesMD is the manifest's kivali.setup_notes: what to do on
	// the vendor's side, Markdown, shown above the connection form.
	// Empty when the manifest has none. The literal {{redirect_url}} in
	// it is replaced with the redirect address.
	SetupNotesMD string `json:"setup_notes_md"`
	// Setup is what a setup connection's flow may do, as the manifest
	// declares it; absent for the other methods.
	Setup *IntegrationSetup `json:"setup,omitempty"`
	// SignInHosts are the hosts of the manifest's OAuth endpoints, each
	// once, sorted: where a sign-in sends the owner's browser and where
	// Kivali takes the tokens from. Empty for a token connection.
	SignInHosts []string `json:"sign_in_hosts"`
	// SignInAt is, for each way the owner may sign in, the host of the
	// page the sign-in goes to: browser, the authorization endpoint's;
	// device, the device endpoint's. Absent for a token connection.
	SignInAt map[string]string `json:"sign_in_at,omitempty"`
	// SignInRedirectURL is the redirect address to register with the
	// vendor for browser sign-in, as each connection's sign-in carries
	// it, here before any connection is saved: the owner registers it
	// with the vendor before they have a client id to save. Present when
	// the manifest offers browser sign-in.
	SignInRedirectURL *string `json:"sign_in_redirect_url,omitempty"`
	// Connection is the integration's connection, absent while there is
	// none.
	Connection *ConnectionState `json:"connection,omitempty"`
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
	// Running is whether the integration can be called now: a built-in
	// always while this version of Kivali carries it, a bundle while its
	// server runs. Test connection and the options an access form lists
	// wait for it.
	Running bool `json:"running"`
	// Requirements are an installed or switched-off integration's
	// health, Kivali's rows first (an approved version, the connection,
	// a sign-in still accepted, the connection test, the integration
	// running), then the integration's own. Empty in any other state.
	Requirements []IntegrationRequirement `json:"requirements"`
	// Flow is its setup flow under way, or the one that last ended,
	// which Kivali keeps for ten minutes: installing (flow setup) or a
	// requirement's fix. Absent with none.
	Flow *SetupFlow `json:"flow,omitempty"`
	// CanUninstall is true when DELETE /api/v1/org/integrations/{name}
	// applies: to an uploaded integration whatever its state, and to a
	// built-in that is installed, off or not in this version of Kivali.
	CanUninstall bool `json:"can_uninstall"`
}

// IntegrationRequirement is one row of an integration's health: met or
// not, in words, with the flow that fixes it.
type IntegrationRequirement struct {
	// ID names the row, stable across reads. Kivali's own are version,
	// connection, sign_in, connection_test and running.
	ID string `json:"id"`
	// Title is the row in a few words ("Signed in").
	Title string `json:"title"`
	// Words say what is, met or not.
	Words string `json:"words"`
	Met   bool   `json:"met"`
	// Fix is the flow its Fix button runs (POST .../setup with
	// {"flow": fix}): setup to make the connection again, sign_in to
	// sign in again, or a flow the integration names. Empty for a row
	// with nothing to run.
	Fix string `json:"fix"`
}

// IntegrationAction is one tool the integration gives agents, in words.
type IntegrationAction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// IntegrationSetup is what a setup connection's flow may do, from the
// manifest's kivali.connection.setup: the install screen lists it
// before anything runs, and Kivali holds every step of the flow to it.
type IntegrationSetup struct {
	// Summary is what setup does, in words.
	Summary string `json:"summary"`
	// Hosts is every host setup may send your browser to.
	Hosts []string `json:"hosts"`
	// Fields is every field setup may ask you for, in order.
	Fields []SetupField `json:"fields"`
	// Keeps is every value setup may keep in the connection's Secret,
	// in order.
	Keeps []SetupKeep `json:"keeps"`
	// Then is the sign-in setup hands over to once it has made the
	// app: browser or device; empty when setup keeps the connection
	// itself.
	Then string `json:"then"`
}

// SetupField is one field a setup flow may ask you for.
type SetupField struct {
	Key string `json:"key"`
	// Title is what the field is called on the form.
	Title string `json:"title"`
	// Words say what to enter and where it comes from.
	Words string `json:"words"`
	// Secret fields are entered as secrets and never shown again.
	Secret bool `json:"secret"`
	// SetupOnly fields are used during setup and never kept.
	SetupOnly bool `json:"setup_only"`
}

// SetupKeep is one value a setup flow may keep, in words.
type SetupKeep struct {
	Key   string `json:"key"`
	Words string `json:"words"`
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
	ID        string    `json:"id"`
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
	// SignIn is the sign-in state of a connection made by signing in;
	// absent for a token connection.
	SignIn *ConnectionSignIn `json:"sign_in,omitempty"`
}

// ConnectionSignIn is where a connection made by signing in stands.
type ConnectionSignIn struct {
	// Ways is how the owner may sign in from where the request came
	// from, the manifest's method first: device, browser. Browser only
	// where a browser sign-in can finish: at a loopback address, or at
	// the team's external address outside a Kivali Desktop window. The
	// page offers the first.
	Ways []string `json:"ways"`
	// BrowserElsewhere is true when Ways is empty because browser
	// sign-in, the connection's only way, cannot finish from where the
	// request came from: a Kivali Desktop window for a team elsewhere,
	// or an address the team does not sign in at. The page says to sign
	// in from a browser at OpenAt.
	BrowserElsewhere bool `json:"browser_elsewhere,omitempty"`
	// OpenAt is the team's external address (https://<host>), where a
	// browser can sign the connection in, when BrowserElsewhere and the
	// team has one.
	OpenAt *string `json:"open_at,omitempty"`
	// SignedIn is true while the connection holds tokens that work.
	SignedIn bool `json:"signed_in"`
	// Method is the way the owner signed in; empty while not signed in.
	Method     string     `json:"method,omitempty"`
	SignedInAt *time.Time `json:"signed_in_at,omitempty"`
	// ExpiresAt is when the access token expires and Kivali refreshes
	// it; absent when it does not expire.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Error says why the connection needs the owner to sign in again.
	Error *string `json:"error,omitempty"`
	// RedirectURL is the exact address to register with the vendor for
	// browser sign-in, from the address the request reached Kivali at:
	// <scheme>://<host>/connect/callback. Present when browser is among
	// Ways.
	RedirectURL *string `json:"redirect_url,omitempty"`
}

// ---- /api/v1/org/integrations/{name}/setup ----

// SetupFlowState is where a setup flow stands.
type SetupFlowState string

const (
	// SetupFlowStateRunning: Kivali is working out the next step;
	// Progress says how far it is when it knows.
	SetupFlowStateRunning SetupFlowState = "running"
	// SetupFlowStateForm: waiting for you to fill in Step's fields.
	SetupFlowStateForm SetupFlowState = "form"
	// SetupFlowStateGo: waiting for your browser to go to the service
	// (Step.StartURL) and come back.
	SetupFlowStateGo SetupFlowState = "go"
	// SetupFlowStateWait: waiting for something to happen on the
	// service; Kivali checks again every few seconds while the page is
	// open.
	SetupFlowStateWait SetupFlowState = "wait"
	// SetupFlowStateDone: the connection is made.
	SetupFlowStateDone SetupFlowState = "done"
	// SetupFlowStateFailed: the flow ended without finishing; Error
	// says why.
	SetupFlowStateFailed SetupFlowState = "failed"
)

// SetupFlow is the answer of POST and GET
// /api/v1/org/integrations/{name}/setup and each event of GET
// .../setup/stream: the integration's setup flow under way, or the one
// that last ended, which Kivali keeps for ten minutes after its last
// change. A flow lives in the server's memory: after a restart there
// is none (404), and you start again.
type SetupFlow struct {
	// ID names the flow: GET /connect/start?flow=<id> sends the browser
	// on for a go step, and the service's callback returns to the
	// integration's page with ?setup=<id>.
	ID string `json:"id"`
	// Flow is the flow run: setup, to make the connection, or a
	// requirement's fix.
	Flow  string         `json:"flow"`
	State SetupFlowState `json:"state"`
	// Step is what you do next, while State is form, go or wait.
	Step *SetupFlowStep `json:"step,omitempty"`
	// Progress is where Kivali is while State is running, when the
	// integration says.
	Progress *SetupFlowProgress `json:"progress,omitempty"`
	// Error says why a failed flow ended.
	Error *string `json:"error,omitempty"`
	// ExpiresAt is when Kivali forgets the flow unless it changes
	// before.
	ExpiresAt time.Time `json:"expires_at"`
}

// SetupFlowStepKind is what kind of step you take.
type SetupFlowStepKind string

const (
	// SetupFlowStepKindForm: fill in fields.
	SetupFlowStepKindForm SetupFlowStepKind = "form"
	// SetupFlowStepKindGo: your browser goes to the service and comes back.
	SetupFlowStepKindGo SetupFlowStepKind = "go"
	// SetupFlowStepKindWait: wait while something happens on the service.
	SetupFlowStepKindWait SetupFlowStepKind = "wait"
)

// SetupFlowStep is one step of a setup flow for you to take.
type SetupFlowStep struct {
	Kind SetupFlowStepKind `json:"kind"`
	// Words say what to do.
	Words string `json:"words"`
	// Fields is a form's fields, in order; empty for the other kinds.
	// Answer them with POST .../setup/answer.
	Fields []SetupField `json:"fields"`
	// StartURL is, for a go step, where the page sends the browser:
	// /connect/start?flow=<id>, which Kivali Desktop opens in the
	// system browser.
	StartURL *string `json:"start_url,omitempty"`
	// NoReturn is true for a go step whose service sends the browser
	// nowhere afterwards (a settings page): the page opens start_url in
	// a new tab and stays, and the flow goes on at once.
	NoReturn bool `json:"no_return"`
	// URL is, for a wait step, a page on the service to open meanwhile.
	URL *string `json:"url,omitempty"`
	// Host is the host the step sends you to: a go step's, or a wait
	// step's page's.
	Host *string `json:"host,omitempty"`
	// Code is, for a wait step, a code to enter on its page.
	Code *string `json:"code,omitempty"`
}

// SetupFlowProgress is how far a running step has got, in the
// integration's words. Total is 0 when there is no count to show.
type SetupFlowProgress struct {
	Words string `json:"words"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// SetupFlowStart is the body of POST /api/v1/org/integrations/{name}/setup:
// the flow to run. A flow already under way for the integration ends
// first.
type SetupFlowStart struct {
	// Flow is setup, to install the integration or make its connection
	// again; or a requirement's fix: sign_in, or a flow the
	// integration's requirements name. Any other is a 409.
	Flow string `json:"flow"`
}

// SetupFlowAnswer is the body of POST
// /api/v1/org/integrations/{name}/setup/answer: a form step's fields by
// key. A field the form does not ask for is a 400.
type SetupFlowAnswer struct {
	Fields map[string]string `json:"fields"`
}

// ConnectionWrite is the body of PUT
// /api/v1/org/integrations/{name}/connection: field values by key. A
// sensitive field left out keeps its stored value; an empty string
// clears it. A field that is not sensitive is replaced by what is
// sent. A required field with no value, stored or sent, is a 400.
type ConnectionWrite struct {
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
	// Invalid says why this row grants nothing until it is set again:
	// it no longer fits the access form of the version in use (a
	// version approved or rolled back to changed it), or the connection
	// it was set for is gone.
	Invalid *string `json:"invalid,omitempty"`
}

// AccessWrite is the body of PUT
// /api/v1/org/integrations/{name}/access/{slug}: the filled access
// form. For an integration without a form, send {}.
type AccessWrite struct {
	Access json.RawMessage `json:"access"`
}

// AccessChoices is GET /api/v1/org/integrations/{name}/choices/{key}:
// the options the integration lists for a field, through its
// connection.
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
	// Runtime is what runs the bundle, uploaded or shipped: python; empty
	// for a built-in of Go code, which runs inside Kivali.
	Runtime string `json:"runtime"`
	// ConnectionMethod is how the connection is made: token, device,
	// browser or setup.
	ConnectionMethod string `json:"connection_method"`
	// SignInClient is whose app a sign-in uses (IntegrationPage's).
	SignInClient string `json:"sign_in_client"`
	// Scopes is what a sign-in connection asks the vendor for; empty
	// otherwise.
	Scopes []string `json:"scopes"`
	// SetupNotesMD is the manifest's kivali.setup_notes: what to do on
	// the vendor's side, Markdown. Empty when the manifest has none.
	// The literal {{redirect_url}} in it is replaced with the redirect
	// address.
	SetupNotesMD string `json:"setup_notes_md"`
	// Setup is what a setup connection's flow may do; absent for the
	// other methods. Read it before you approve: Kivali sends your
	// browser only to these hosts, asks only for these fields and keeps
	// only these values.
	Setup *IntegrationSetup `json:"setup,omitempty"`
	// SignInHosts are the hosts of the manifest's OAuth endpoints, each
	// once, sorted: where a sign-in sends the owner's browser and where
	// Kivali takes the tokens from. Empty for a token connection.
	SignInHosts []string `json:"sign_in_hosts"`
	Hosts       []string `json:"hosts"`
	// Packages is the Debian packages the pod installs from Debian's
	// repositories each time it starts, before the server runs
	// (kivali.packages); empty for none.
	Packages         []string            `json:"packages"`
	ConnectionFields []ConnectionField   `json:"connection_fields"`
	Actions          []IntegrationAction `json:"actions"`
	AccessForm       *AccessForm         `json:"access_form,omitempty"`
	BundleBytes      int64               `json:"bundle_bytes"`
	FileCount        int                 `json:"file_count"`
	// Current is true when this is the version in use now.
	Current bool `json:"current"`
	// Builtin is true for a version of an integration that ships with
	// Kivali: it needs no approval and comes with Kivali's updates, so
	// it is never approved, rejected or rolled back to here (409).
	Builtin bool `json:"builtin"`
	// CurrentVersion is the version in use now, when there is one and
	// it is not this one, so the screen can say what approving replaces.
	CurrentVersion *string `json:"current_version,omitempty"`
	// Changes is what this version changes from CurrentVersion, in
	// words, one line each: hosts, software, connection fields, actions
	// and access form fields added or removed ("Hosts added:
	// api.example.com", "Software added: gh"). Empty without a CurrentVersion, and when none
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
	// State is the integration's.
	State IntegrationState `json:"state"`
	// Usable is true when the agent's calls can go through now: the
	// integration is installed and running, and its connection has its
	// secrets and, made by signing in, is signed in. Always false under
	// Available.
	Usable bool `json:"usable"`
	// Summary is the agent's access in words; empty under Available.
	Summary string `json:"summary"`
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
	// Level is the integration's own level (info, warning, error, or
	// debug for detail, such as apt-get's output while its software
	// installs), or stderr for a line it wrote to its error output.
	Level   string `json:"level"`
	Message string `json:"message"`
}
