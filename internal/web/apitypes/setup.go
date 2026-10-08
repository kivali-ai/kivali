package apitypes

// ---- /api/v1/login (public) ----

// Login is GET /api/v1/login, the one API route served without a
// session: what the sign-in screen needs before anyone has signed in.
// It carries nothing else, since anyone can read it.
type Login struct {
	// Org is the org's name and whether it has a logo. Name is empty
	// until the owner sets one.
	Org MeOrg `json:"org"`
	// AuthReady says whether Google sign-in is configured (both its
	// client id and secret). False is the "Sign-in isn't set up yet"
	// state.
	AuthReady bool `json:"auth_ready"`
	// DevMode is true when the server runs without sign-in, attributing
	// every request to one developer account.
	DevMode bool `json:"dev_mode"`
}

// ---- /api/v1/setup ----

// SetupStep is one of setup's four steps, or done.
type SetupStep string

const (
	SetupStepWelcome SetupStep = "welcome"
	SetupStepOrg     SetupStep = "org"
	SetupStepFiles   SetupStep = "files"
	SetupStepCoS     SetupStep = "cos"
	SetupStepDone    SetupStep = "done"
)

// Setup is GET /api/v1/setup, and the body every setup write answers
// with, so the client can replace its state wholesale.
type Setup struct {
	// Needed is true until a Chief of Staff exists.
	Needed bool `json:"needed"`
	// Step is where setup should resume: done once the Chief of Staff
	// exists; cos while a hire is running or has failed; otherwise
	// welcome when nothing is set, org when files are uploaded but the
	// org has no name, files when the org is named but has no files, and
	// cos when both are there.
	Step SetupStep `json:"step"`
	Org  MeOrg     `json:"org"`
	// Kind is what the team is for; empty when it was never set, which
	// setup treats as work.
	Kind  TeamKind    `json:"kind"`
	Files []SetupFile `json:"files"`
	CoS   SetupCoS    `json:"cos"`
	// Credential says whether a model is connected and how calls are
	// paid for; seeding needs a model.
	Credential SetupCredential `json:"credential"`
	// RestoreAvailable is true while the server holds nothing a backup
	// restore would overwrite.
	RestoreAvailable bool `json:"restore_available"`
}

// TeamKind is what a team is for: work runs a business, personal helps
// one person with their life. Unset is a team created before kinds
// existed, or one nobody chose a kind for; it behaves as work.
type TeamKind string

const (
	TeamKindUnset    TeamKind = ""
	TeamKindWork     TeamKind = "work"
	TeamKindPersonal TeamKind = "personal"
)

// SetupFile is one uploaded project file, oldest first.
type SetupFile struct {
	SHA       string `json:"sha"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	// Extracted is true when the file has a text form the Chief of Staff
	// reads in full; false files are named to it but not read.
	Extracted bool `json:"extracted"`
}

// SetupCoS is the Chief of Staff step's starting point.
type SetupCoS struct {
	Exists bool `json:"exists"`
	// DefaultRoleMD is the role document a new Chief of Staff gets.
	DefaultRoleMD string `json:"default_role_md"`
	// DefaultHandbookMD is the handbook seeding would use: the
	// stored one when there is one, else the built-in default.
	DefaultHandbookMD string `json:"default_handbook_md"`
}

// SetupCredential says whether the model is connected and how calls
// are paid for.
type SetupCredential struct {
	// Ready is true when the server has a model driver; seeding needs
	// one.
	Ready bool `json:"ready"`
	// Provider names the org's model provider ("claude").
	Provider string `json:"provider"`
	// Present is true when the provider reports it is signed in.
	Present bool `json:"present"`
	// Who is the signed-in identity when the provider reports one.
	Who string `json:"who"`
	// Billing names what calls are billed to, in the provider's words
	// ("Claude Max", "Anthropic Console", "Amazon Bedrock", "Google
	// Vertex AI"); empty when not signed in or not reported.
	Billing string `json:"billing"`
	// Guidance is one plain sentence saying what is missing and who can
	// fix it. Empty when Ready.
	Guidance string `json:"guidance"`
}

// SetupOrgRequest is the body of POST /api/v1/setup/org.
type SetupOrgRequest struct {
	Name string `json:"name"`
}

// SeedCoSRequest is the body of POST /api/v1/setup/seed-cos. It
// answers 202 with an empty body and the hire runs in the background;
// GET /api/v1/setup/progress follows it.
type SeedCoSRequest struct {
	// HandbookFromFiles asks the new Chief of Staff, in its first
	// message, to draft the handbook's company section from the
	// project files and propose it, or to ask the CEO what the files
	// leave out. The handbook written at seeding is then the
	// default (or HandbookMD), to be replaced by that proposal.
	HandbookFromFiles bool `json:"handbook_from_files"`
	// RoleMD replaces the default role document. Send it only when the
	// CEO edited it.
	RoleMD *string `json:"role_md,omitempty"`
	// HandbookMD replaces the handbook seeding writes. Send it
	// only when the CEO edited it.
	HandbookMD *string `json:"handbook_md,omitempty"`
}

// SetupProgressState is where the Chief of Staff hire stands.
type SetupProgressState string

const (
	SetupProgressStateIdle    SetupProgressState = "idle"
	SetupProgressStateRunning SetupProgressState = "running"
	SetupProgressStateDone    SetupProgressState = "done"
	SetupProgressStateFailed  SetupProgressState = "failed"
)

// SetupStageState is one stage's state; it maps onto AgentState on the
// hiring panel.
type SetupStageState string

const (
	SetupStageStateDone    SetupStageState = "done"
	SetupStageStateRunning SetupStageState = "running"
	SetupStageStateQueued  SetupStageState = "queued"
	SetupStageStateFailed  SetupStageState = "failed"
)

// SetupStage is one line of the hiring panel. The labels name the
// seed's real steps: "Reading your files", "Writing its first
// briefing" (the one long model call), "Hiring", and, when the
// handbook is to come from the files, "Asking it to draft the
// handbook".
type SetupStage struct {
	Label string          `json:"label"`
	State SetupStageState `json:"state"`
}

// SetupProgress is GET /api/v1/setup/progress. Idle has no stages;
// done after a restart (the Chief of Staff exists but this process did
// not hire it) has none either.
type SetupProgress struct {
	State  SetupProgressState `json:"state"`
	Stages []SetupStage       `json:"stages"`
	// ElapsedS is whole seconds since the hire started, frozen when it
	// finishes.
	ElapsedS int `json:"elapsed_s"`
	// Error and Who are set only on failed: what happened and who can
	// fix it.
	Error string `json:"error,omitempty"`
	Who   string `json:"who,omitempty"`
}
