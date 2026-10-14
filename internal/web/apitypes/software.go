package apitypes

// Software: the Debian packages installed into agents' shells. The
// Org page holds the list every agent gets; the agent's About tab shows
// that list beside the agent's own. A write adds or removes names on
// the list as it stands, and answers with the list as it now stands.

// ---- /api/v1/org/software ----

// OrgSoftware is GET /api/v1/org/software, and the answer to POST
// /api/v1/org/software and DELETE /api/v1/org/software/{name}: the
// packages every agent's shell gets. Always sorted, never null.
type OrgSoftware struct {
	Packages []string `json:"packages"`
	// Invalid names the listed packages that are not Debian package
	// names (a list edited by hand): they are not installed, and DELETE
	// removes them like any other. Never null.
	Invalid []string `json:"invalid"`
}

// SoftwareAdd is the body of POST /api/v1/org/software and of POST
// /api/v1/agents/{slug}/software: names to add to the list, keeping
// what is there. Names are trimmed, lowercased and de-duplicated; a
// name that is not a Debian package name, or an empty list, is a 400.
// DELETE .../software/{name} removes one name; a name not on the list
// is a 404.
type SoftwareAdd struct {
	Packages []string `json:"packages"`
}

// ---- /api/v1/agents/{slug}/software ----

// AgentSoftware is GET /api/v1/agents/{slug}/software, and the answer
// to its POST and DELETE.
type AgentSoftware struct {
	// Org is the org-wide list, which this agent gets too. Read-only here.
	Org []string `json:"org"`
	// Own is the agent's own list, the part POST and DELETE change.
	Own []string `json:"own"`
	// Invalid names the packages on either list that are not Debian
	// package names: they are not installed. Never null.
	Invalid []string `json:"invalid"`
}
