// Package roleicon is the set of role icons an agent's avatar can
// show. The Chief of Staff picks one for every hire (propose_hire's
// `icon`) from these names and descriptions, with the role in hand;
// the agent keeps it, and the web app draws exactly that icon.
//
// The web app holds the drawings (web/src/ds/AgentAvatar/roleIcons.ts,
// Lucide icons). The apitypes golden test writes this list to
// internal/web/apitypes/testdata/role_icons.json, and a web test checks
// that every name here has a drawing.
package roleicon

import (
	"fmt"
	"strings"
)

// Icon is one role icon: its name, as the avatar takes it, and what it
// shows and suits, for the one choosing.
type Icon struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Icons is every role icon, in the order the hiring agent reads them.
var Icons = []Icon{
	{"compass", "a compass: coordinating, running operations, planning, chief of staff"},
	{"briefcase", "a briefcase: general business, executive, consulting"},
	{"target", "a target: product management, goals, strategy"},
	{"lightbulb", "a light bulb: ideas, innovation, brainstorming"},
	{"rocket", "a rocket: launches, growth, a new venture"},
	{"code", "angle brackets: software engineering, programming"},
	{"terminal", "a command prompt: devops, systems administration, automation"},
	{"server", "a server rack: hosting, backend, infrastructure, platform"},
	{"database", "a database cylinder: data engineering, records"},
	{"chart-line", "a line chart: analytics, metrics, reporting"},
	{"bug", "a bug: quality assurance, testing"},
	{"cpu", "a chip: hardware, embedded systems"},
	{"circuit-board", "a circuit board: electronics"},
	{"pen-tool", "a pen nib: design, UX, product design"},
	{"palette", "a paint palette: brand, art direction, creative work"},
	{"file-text", "a page of text: writing, documentation, editing"},
	{"languages", "two scripts: translation, localization"},
	{"megaphone", "a megaphone: marketing, announcements"},
	{"newspaper", "a newspaper: press, public relations, news"},
	{"camera", "a camera: photography, visual content"},
	{"video", "a video camera: video production"},
	{"music", "a music note: music, audio, podcasts"},
	{"handshake", "a handshake: sales, partnerships, business development"},
	{"headset", "a headset: customer support, help desk"},
	{"mail", "an envelope: email, correspondence, managing an inbox"},
	{"calendar", "a calendar: scheduling, personal or executive assistant"},
	{"users", "a group of people: people operations, HR, team culture"},
	{"user-search", "a person under a magnifier: recruiting, hiring"},
	{"calculator", "a calculator: finance, accounting, bookkeeping, budgets"},
	{"receipt", "a receipt: expenses, invoices, billing"},
	{"coins", "a stack of coins: fundraising, investors, treasury"},
	{"piggy-bank", "a piggy bank: personal finance, savings, a household budget"},
	{"gavel", "a gavel: legal work, contracts, counsel"},
	{"scale", "balance scales: policy, ethics, weighing options, advising"},
	{"shield-check", "a shield with a check: compliance, risk, audit, governance"},
	{"shield", "a shield: security, safety, protection"},
	{"lock", "a padlock: privacy, access, secrets"},
	{"search", "a magnifying glass: research, investigation, sourcing"},
	{"book-open", "an open book: reading, knowledge, a library"},
	{"graduation-cap", "a graduation cap: teaching, tutoring, coaching"},
	{"microscope", "a microscope: science, lab work, analysis"},
	{"flask-conical", "a flask: chemistry, experiments, research and development"},
	{"shopping-cart", "a shopping cart: purchasing, procurement, shopping"},
	{"truck", "a truck: logistics, shipping, delivery"},
	{"package", "a parcel: inventory, warehousing, fulfilment"},
	{"factory", "a factory: manufacturing, production"},
	{"wrench", "a wrench: maintenance, repairs, field service"},
	{"globe", "a globe: international work, world affairs"},
	{"plane", "an aeroplane: travel, trips, itineraries"},
	{"map", "a folded map: navigation, a territory, local knowledge"},
	{"house", "a house: the household, home, family, chores"},
	{"utensils", "a fork and knife: cooking, meals, nutrition"},
	{"dumbbell", "a dumbbell: fitness, training"},
	{"heart-pulse", "a heart with a pulse: health, wellness"},
	{"sprout", "a sprout: gardening, plants, landscaping"},
	{"leaf", "a leaf: environment, sustainability, conservation"},
	{"tractor", "a tractor: farming, agriculture"},
	{"beef", "a cut of meat: livestock, animal husbandry"},
	{"fish", "a fish: fishing, marine life, aquariums"},
	{"cloud-sun", "a sun behind a cloud: weather, climate"},
	{"vote", "a ballot box: civic life, elections, politics"},
	{"landmark", "a columned building: government, the public sector, banking"},
	{"radio", "a radio: communications, broadcasting"},
	{"user", "a person: a general role that none of the others fits"},
}

// Valid reports whether name is a role icon.
func Valid(name string) bool {
	for _, i := range Icons {
		if i.Name == name {
			return true
		}
	}
	return false
}

// Check returns nil for a role icon, else an error naming the choices.
func Check(name string) error {
	if Valid(name) {
		return nil
	}
	names := make([]string, len(Icons))
	for i, ic := range Icons {
		names[i] = ic.Name
	}
	return fmt.Errorf("%q is not a role icon; pick one of: %s", name, strings.Join(names, ", "))
}

// Menu is the list for a tool description: one "name: description"
// line per icon.
func Menu() string {
	var b strings.Builder
	for _, i := range Icons {
		b.WriteString("- ")
		b.WriteString(i.Name)
		b.WriteString(": ")
		b.WriteString(i.Description)
		b.WriteString("\n")
	}
	return b.String()
}
