// Package seed holds the default content Kivali ships with — the team
// handbook for each kind of team and the roles of the two agents
// Kivali hires itself: the Chief of Staff and the Kivali Developer.
//
// The handbook is the team's: its About, Values, Communicating and
// Working together sections (internal/handbook holds the shape). How
// the runtime works is Kivali OS (internal/agent), which is not
// seeded: it ships in the binary and is read from there.
//
// They call the person "the owner" and never carry their name, which
// reaches agents through the system prompt's owner section
// (internal/owner). The ...For functions pick a kind's text. Setup
// seeds the handbook, which the person can edit afterwards in the app.
// The two roles are Kivali's (KivaliRole): every boot writes them from
// this binary, and nothing else changes them.
package seed

import (
	_ "embed"

	"github.com/kivali-ai/kivali/internal/graph"
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

// ChiefOfStaffRoleFor is the Chief of Staff's role for a team of kind:
// cos_role.md without the business wording on a personal team, byte
// for byte otherwise.
func ChiefOfStaffRoleFor(kind string) string {
	if kind == kindPersonal {
		return owner.PersonalWording(ChiefOfStaffRole)
	}
	return ChiefOfStaffRole
}

// KivaliRole is the role Kivali ships for the agent slug on a team of
// kind, when the agent is one of the two Kivali hires itself: the Chief
// of Staff (ChiefOfStaffRoleFor) and the Kivali Developer
// (KivaliDeveloperRole). Their roles are Kivali's, not the team's:
// boot and a restore write them as the agents' role.md, and every
// other write door refuses them. ok is false for any other agent,
// whose role is the team's.
func KivaliRole(slug, kind string) (role string, ok bool) {
	switch slug {
	case graph.ChiefOfStaffSlug:
		return ChiefOfStaffRoleFor(kind), true
	case graph.KivaliDeveloperSlug:
		return KivaliDeveloperRole, true
	}
	return "", false
}

// KivaliRoleSlugs are the agents KivaliRole has a role for.
var KivaliRoleSlugs = []string{graph.ChiefOfStaffSlug, graph.KivaliDeveloperSlug}

// PersonalFirstMessage is a personal team's Chief of Staff's first
// message: ask the CEO a few questions, then draft the handbook's
// About section, drawing on project files when there are any. It is
// sent as the CEO's own message.
// Edit internal/seed/personal_first_message.md to change it.
//
//go:embed personal_first_message.md
var PersonalFirstMessage string

// WorkFirstMessage is a work team's Chief of Staff's first message
// when setup is not asked to write the handbook from the project
// files: ask the CEO a few questions about the business, leaving out
// what the handbook and files already answer, then draft the
// handbook's About section. It is sent as the CEO's own message.
// Edit internal/seed/work_first_message.md to change it.
//
//go:embed work_first_message.md
var WorkFirstMessage string

// ChiefOfStaffRole is the Chief of Staff's role, before
// ChiefOfStaffRoleFor fits it to the team's kind. Setup writes it as
// the agent's role.md, and every boot writes it again (KivaliRole).
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

// KivaliDeveloperRole is the Kivali Developer's role: the agent Kivali
// hires under the Chief of Staff with the team, which builds the
// integrations that connect agents to outside services. Setup writes
// it as the agent's role.md, and every boot writes it again
// (KivaliRole).
// Edit internal/seed/kivali_developer_role.md to change it.
//
//go:embed kivali_developer_role.md
var KivaliDeveloperRole string
