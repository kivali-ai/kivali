// Package seed holds the default content Kivali ships with — the
// handbook template and the Chief of Staff role template.
//
// They call the person "the owner" and never carry their name, which
// reaches agents through the system prompt's owner section
// (internal/owner). The ...For functions drop a personal team's
// business wording. Setup seeds them; the person can edit either
// afterwards in the app.
package seed

import (
	_ "embed"
	"strings"

	"github.com/kivali-ai/kivali/internal/owner"
)

// Handbook is the default handbook a work team starts from.
// Edit internal/seed/handbook.md to change it.
//
//go:embed handbook.md
var Handbook string

// handbookPersonalHead is the personal handbook's opening and
// its "About you" section, which stand in for everything in
// handbook.md before the first operating section.
//
//go:embed handbook_personal_head.md
var handbookPersonalHead string

// operatingSectionsStart is the heading that begins the operating
// sections every kind of team shares.
const operatingSectionsStart = "\n## How the org works\n"

// PersonalHandbook is the default handbook a personal team starts from:
// HandbookFor("personal").
var PersonalHandbook = HandbookFor(kindPersonal)

// personalHandbook is handbook_personal_head.md, then handbook.md from
// its first operating section on, so both kinds share one copy of
// those sections.
func personalHandbook(work string) string {
	i := strings.Index(work, operatingSectionsStart)
	if i < 0 {
		panic("seed: handbook.md has no " + strings.TrimSpace(operatingSectionsStart) + " section")
	}
	return handbookPersonalHead + work[i+1:]
}

// kindPersonal is store.TeamKindPersonal, the kind a personal team's
// branding holds.
const kindPersonal = "personal"

// HandbookFor is the default handbook for a team of kind: the personal
// handbook, without the business wording (owner.PersonalWording), for
// "personal"; handbook.md byte for byte for anything else (work, or a
// team that never set a kind). Neither carries the person's name: the
// system prompt's owner section does (owner.Term.Section).
func HandbookFor(kind string) string {
	if kind == kindPersonal {
		return owner.PersonalWording(personalHandbook(Handbook))
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
// message: ask the CEO a few questions, then draft the "About you"
// section, drawing on project files when there are any. It is sent as
// the CEO's own message.
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
// files: draft the company section and propose it, or ask the CEO what
// the files leave out. It is sent as the CEO's own message, so it
// speaks to the Chief of Staff directly.
// Edit internal/seed/handbook_from_files.md to change it.
//
//go:embed handbook_from_files.md
var HandbookFromFilesMessage string
