package apitypes

import "time"

// The Org page: usage, the organization's name and logo, the
// handbook, project files, skills, network, and backup/restore. Every
// list mutation answers with the list as it now stands, so the page
// replaces what it shows instead of patching it.

// ---- /api/v1/org/usage ----

// Usage is GET /api/v1/org/usage.
//
// All spend is US dollars priced from each call's token counts at list
// rates, the same pricing Home's spend readouts use, so the two pages
// agree. A call on a model with no price adds nothing to spend; its
// tokens are counted in UsageWindow.Unpriced instead.
type Usage struct {
	Tiles UsageTiles `json:"tiles"`
	// Daily is spend per UTC day for the last 30 days, oldest first,
	// ending today. Always 30 entries: a day with no calls is 0.
	Daily []UsageDay `json:"daily"`
	// ByAgent is every agent with a call in the last 30 days, most
	// 30-day spend first (ties by slug). Calls made for no agent (inbox
	// and file summaries) are one row with an empty slug.
	ByAgent []UsageAgent `json:"by_agent"`
	Windows UsageWindows `json:"windows"`
}

// UsageTiles are the three spend tiles.
type UsageTiles struct {
	// Today is since 00:00 UTC, matching Home's spend today.
	Today UsageTile `json:"today"`
	// D7 and D30 are the last 7 and 30 days before now.
	D7  UsageTile `json:"d7"`
	D30 UsageTile `json:"d30"`
}

// UsageTile is one tile's figures.
type UsageTile struct {
	Spend float64 `json:"spend"`
	Calls int     `json:"calls"`
	// CacheHitPct is the share of input-side tokens served from the
	// prompt cache: cache reads over fresh input plus cache reads plus
	// cache writes, 0..100, rounded. 0 with no calls.
	CacheHitPct int `json:"cache_hit_pct"`
}

// UsageDay is one bar of "Spend per day".
type UsageDay struct {
	// Date is the UTC day, YYYY-MM-DD.
	Date  string  `json:"date"`
	Spend float64 `json:"spend"`
}

// UsageAgent is one row of "Who spent it" and the By agent table.
type UsageAgent struct {
	// Slug is empty for calls made for no agent.
	Slug string `json:"slug"`
	// Name is the agent's display name, the slug for an agent no
	// longer on file, "You" for the CEO, and "Kivali" for calls made
	// for no agent.
	Name string  `json:"name"`
	D7   float64 `json:"d7"`
	D30  float64 `json:"d30"`
}

// UsageWindows is the folded "Calls and tokens" table.
type UsageWindows struct {
	H24 UsageWindow `json:"h24"`
	D7  UsageWindow `json:"d7"`
	D30 UsageWindow `json:"d30"`
}

// UsageWindow is one column of the calls and tokens table.
type UsageWindow struct {
	Calls int `json:"calls"`
	// TokensIn is everything sent to the model: fresh input plus both
	// cache legs. TokensOut is what it wrote back.
	TokensIn    int     `json:"tokens_in"`
	TokensOut   int     `json:"tokens_out"`
	CacheRead   int     `json:"cache_read"`
	CacheCreate int     `json:"cache_create"`
	Spend       float64 `json:"spend"`
	// Unpriced is the tokens (in plus out) on calls whose model has no
	// price, which Spend leaves out; UnpricedCalls is how many calls
	// those are.
	Unpriced      int `json:"unpriced"`
	UnpricedCalls int `json:"unpriced_calls"`
}

// ---- /api/v1/org ----

// Org is GET /api/v1/org, and the answer to PUT /api/v1/org and the
// logo upload and removal.
type Org struct {
	// Name is empty until someone sets one.
	Name    string `json:"name"`
	HasLogo bool   `json:"has_logo"`
	// LogoURL is the 512 px logo, present when HasLogo.
	LogoURL *string `json:"logo_url,omitempty"`
	// Kind is what the team is for; empty when never set.
	Kind TeamKind `json:"kind"`
	// OwnerName is what the person this team works for asked agents to
	// call them ("Jane", "Mom"); empty when they chose nothing.
	OwnerName string `json:"owner_name"`
}

// OrgPut is the body of PUT /api/v1/org. An empty name clears it.
// Kind, when present, changes the team's kind; it must be work or
// personal. OwnerName, when present, changes what agents call the
// person (trimmed, at most 40 characters); empty clears it.
type OrgPut struct {
	Name      string    `json:"name"`
	Kind      *TeamKind `json:"kind,omitempty"`
	OwnerName *string   `json:"owner_name,omitempty"`
}

// ---- /api/v1/org/handbook ----

// Handbook is GET /api/v1/org/handbook and the answer to PUT.
type Handbook struct {
	Content string `json:"content"`
	// Sections is Content split at `##` headings, as DocDiff's
	// splitSections does: text before the first heading is "Opening"
	// (left out when blank), and the heading line itself is not in
	// Lines.
	Sections []DocSection `json:"sections"`
	// Draft is true when no handbook has been saved: Content is the
	// starting draft Kivali ships with.
	Draft bool `json:"draft"`
	// UpdatedAt is when it was last saved; absent for a draft.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// DocSection is one `##` section of a markdown document.
type DocSection struct {
	Title string `json:"title"`
	// Lines is the section's text line for line, blank lines included,
	// so joining them rebuilds it.
	Lines []string `json:"lines"`
	// LineCount is how many of Lines have text: what "N lines" on a
	// section counts.
	LineCount int `json:"line_count"`
}

// HandbookPut is the body of PUT /api/v1/org/handbook.
type HandbookPut struct {
	Content string `json:"content"`
}

// ---- /api/v1/org/files ----

// ProjectFiles is GET /api/v1/org/files and the answer to every files
// mutation. Oldest upload first.
type ProjectFiles struct {
	Files []ProjectFile `json:"files"`
}

// ProjectFile is one uploaded project file.
type ProjectFile struct {
	SHA        string    `json:"sha"`
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	UploadedAt time.Time `json:"uploaded_at"`
	// Extracted is true when agents can read the file as text (or, for
	// an image or PDF, as itself); false when nothing could be pulled
	// out of it.
	Extracted bool `json:"extracted"`
	// Kind is the knowledge-graph kind chosen at upload; empty reads as
	// reference.
	Kind string `json:"kind"`
	// Summary is a one-sentence description written shortly after
	// upload; empty until then or when the file has no text.
	Summary string `json:"summary"`
	// URL downloads the original upload.
	URL string `json:"url"`
}

// FilesBulkDelete is the body of POST /api/v1/org/files/bulk-delete.
type FilesBulkDelete struct {
	SHAs []string `json:"shas"`
}

// ---- /api/v1/org/skills ----

// Skills is GET /api/v1/org/skills and the answer to every skills
// mutation, by name.
type Skills struct {
	Skills []Skill `json:"skills"`
}

// Skill is one skill in the org's library.
type Skill struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
	// Builtin skills ship with Kivali: they can be switched off, never
	// replaced or deleted.
	Builtin     bool      `json:"builtin"`
	Description string    `json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SkillDowngrade is the 409 body of a skill upload whose version is
// not newer than the installed one. Send the upload again with the
// form field confirm_downgrade=true to replace it anyway.
type SkillDowngrade struct {
	Error      string `json:"error"`
	Who        string `json:"who"`
	OldVersion string `json:"old_version"`
	NewVersion string `json:"new_version"`
}

// ---- /api/v1/org/network ----

// Network is GET /api/v1/org/network and the answer to every network
// mutation: the hosts agents may reach, sorted.
type Network struct {
	Hosts []NetworkHost `json:"hosts"`
}

// NetworkHost is one allowed host. A host covers itself and every
// subdomain; a leading "*." means the same. Past that, "*" and "[a-z]"
// match within one label ("*-aiplatform.googleapis.com").
type NetworkHost struct {
	Host string `json:"host"`
}

// NetworkAdd is the body of POST /api/v1/org/network.
type NetworkAdd struct {
	Host string `json:"host"`
}
