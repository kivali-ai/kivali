// Package seed holds the default content Kivali ships with — the team
// handbook for each kind of team, the Chief of Staff role template and
// the Integrations engineer's role, the one role a hire can take from
// Kivali.
//
// The handbook is the team's: its About, Values, Communicating and
// Working together sections (internal/handbook holds the shape). How
// the runtime works is Kivali OS (internal/agent), which is not
// seeded: it ships in the binary and is read from there.
//
// They call the person "the owner" and never carry their name, which
// reaches agents through the system prompt's owner section
// (internal/owner). The ...For functions pick a kind's text. Setup
// seeds them; the person can edit either afterwards in the app.
package seed

import (
	_ "embed"

	"github.com/kivali-ai/kivali/internal/owner"
)

// Handbook is the default handbook a work team starts from: the one
// every team takes on when its handbook is first written.
// Edit internal/seed/handbook.md to change it.
//
//go:embed handbook.md
var Handbook string

// PersonalHandbook is the default handbook a personal team starts
// from: the same four sections, written for a person's life rather
// than a business.
// Edit internal/seed/handbook_personal.md to change it.
//
//go:embed handbook_personal.md
var PersonalHandbook string

// kindPersonal is store.TeamKindPersonal, the kind a personal team's
// branding holds.
const kindPersonal = "personal"

// HandbookFor is the default handbook for a team of kind:
// handbook_personal.md for "personal"; handbook.md byte for byte for
// anything else (work, or a team that never set a kind). Neither
// carries the person's name: the system prompt's owner section does
// (owner.Term.Section).
func HandbookFor(kind string) string {
	if kind == kindPersonal {
		return PersonalHandbook
	}
	return Handbook
}

// ChiefOfStaffRoleFor is the Chief of Staff's default role for a team
// of kind: cos_role.md without the business wording on a personal team,
// byte for byte otherwise.
func ChiefOfStaffRoleFor(kind string) string {
	if kind == kindPersonal {
		return owner.PersonalWording(ChiefOfStaffRole)
	}
	return ChiefOfStaffRole
}

// PersonalFirstMessage is a personal team's Chief of Staff's first
// message: ask the CEO a few questions, then draft the handbook's
// About section, drawing on project files when there are any. It is
// sent as the CEO's own message.
// Edit internal/seed/personal_first_message.md to change it.
//
//go:embed personal_first_message.md
var PersonalFirstMessage string

// ChiefOfStaffRole is the default role template for the Chief of Staff
// agent. It becomes the role.md for CoS at seed time.
// Edit internal/seed/cos_role.md to change it.
//
//go:embed cos_role.md
var ChiefOfStaffRole string

// HandbookFromFilesMessage is the Chief of Staff's first message
// when the CEO asks setup to write the handbook from the project
// files: draft the About section and propose it, or ask the CEO what
// the files leave out. It is sent as the CEO's own message, so it
// speaks to the Chief of Staff directly.
// Edit internal/seed/handbook_from_files.md to change it.
//
//go:embed handbook_from_files.md
var HandbookFromFilesMessage string

// IntegrationsEngineerRole is the Integrations engineer's role: the
// one role Kivali ships for a hire, which builds the integrations that
// connect agents to outside services. The Chief of Staff hires it with
// propose_hire's template (IntegrationsEngineerTemplate) and writes no
// role of its own; ApplyHire marks the agent it creates as the
// engineer, which is what gives it the integration_* tools.
// Edit internal/seed/integrations_engineer_role.md to change it.
//
//go:embed integrations_engineer_role.md
var IntegrationsEngineerRole string

// IntegrationsEngineerTemplate is the template propose_hire names for
// the Integrations engineer.
const IntegrationsEngineerTemplate = "integrations-engineer"

// HireTemplates is every template a hire can name, for propose_hire's
// schema.
var HireTemplates = []string{IntegrationsEngineerTemplate}

// HireTemplate is the role a hire template names: its title in words
// for the owner and the role.md the agent gets. ok is false for a name
// Kivali does not ship.
func HireTemplate(name string) (title, role string, ok bool) {
	if name == IntegrationsEngineerTemplate {
		return "Integrations engineer", IntegrationsEngineerRole, true
	}
	return "", "", false
}
