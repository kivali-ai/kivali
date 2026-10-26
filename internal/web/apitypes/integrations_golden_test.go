package apitypes

import (
	"encoding/json"
	"time"
)

// Fixtures for integrations (integrations.go). Every optional field is
// set somewhere, so the TypeScript side sees each shape once.

var (
	integrationEarlier   = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	integrationNow       = time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	integrationLater     = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	integrationVersion2  = "1.3.0"
	integrationVerified  = true
	integrationVerifyMsg = "Connected as Acme Marketing"
	integrationInvalid   = "Blogs lists a choice the form no longer offers"
	integrationCurrent   = "1.2.0"
	integrationValue     = "acme"
)

func goldenIntegrationSummary() IntegrationSummary {
	return IntegrationSummary{
		Name:           "hubspot",
		DisplayName:    "HubSpot",
		Description:    "Blog posts and contacts in HubSpot.",
		Version:        "1.2.0",
		State:          IntegrationStateConnected,
		Enabled:        true,
		Builtin:        false,
		Agents:         2,
		PendingVersion: &integrationVersion2,
	}
}

func goldenIntegrations() Integrations {
	return Integrations{Integrations: []IntegrationSummary{
		goldenIntegrationSummary(),
		{Name: "weather", DisplayName: "Weather", Description: "Forecasts.", Version: "", State: IntegrationStatePending, Enabled: true, Agents: 0},
		{Name: "maps", DisplayName: "Maps", Description: "Places and routes.", Version: "1.0.0", State: IntegrationStateNotIncluded, Enabled: true, Builtin: true, Agents: 0},
		{Name: "git", DisplayName: "Git", Description: "Repositories on a Git host.", Version: "1.0.0", State: IntegrationStateInstalled, Enabled: true, Builtin: true, Agents: 0},
		{Name: "github", DisplayName: "GitHub", Description: "Issues and pull requests on GitHub.", Version: "2.0.0", State: IntegrationStateInstalled, Enabled: true, Builtin: true, Agents: 0},
		{Name: "crm", DisplayName: "CRM", Description: "Contacts.", Version: "2.1.0", State: IntegrationStateConnected, Needs: 1, Enabled: true, Agents: 1},
		{Name: "blog", DisplayName: "Blog", Description: "Posts.", Version: "1.0.0", State: IntegrationStateDisabled, Enabled: false, Agents: 3},
	}}
}

// goldenRequirementsMet are an installed bundle's requirements, every
// one met: Kivali's rows, the connection test among them.
func goldenRequirementsMet() []IntegrationRequirement {
	return []IntegrationRequirement{
		{ID: "version", Title: "Approved version", Words: "Version 1.2.0 is in use.", Met: true},
		{ID: "connection", Title: "Connection", Words: "Its secrets are stored.", Met: true},
		{ID: "connection_test", Title: "Connection test", Words: integrationVerifyMsg, Met: true},
		{ID: "running", Title: "Running", Words: "It is running.", Met: true},
	}
}

func goldenConnectionFields() []ConnectionField {
	return []ConnectionField{
		{Key: "token", Title: "Private app token", Description: "From HubSpot, Settings, Private apps.", Sensitive: true, Required: true, Set: true},
		{Key: "portal", Title: "Portal", Description: "The portal's short name.", Sensitive: false, Required: false, Set: true, Value: &integrationValue},
	}
}

func goldenAccessForm() *AccessForm {
	return &AccessForm{Fields: []AccessField{
		{Key: "blogs", Title: "Blogs", Description: "The blogs the agent may write in.", Kind: AccessFieldKindChoices, Required: true, Options: []string{}, Choices: true},
		{Key: "write", Title: "May publish and edit", Description: "", Kind: AccessFieldKindSwitch, Required: false, Options: []string{}, Choices: false},
		{Key: "delete", Title: "May delete posts", Description: "Deletes posts for good.", Kind: AccessFieldKindSwitch, Required: false, Options: []string{}, Choices: false, Danger: true},
		{Key: "region", Title: "Region", Description: "", Kind: AccessFieldKindChoice, Required: false, Options: []string{"eu", "us"}, Choices: false},
		{Key: "tags", Title: "Tags", Description: "", Kind: AccessFieldKindList, Required: false, Options: []string{}, Choices: false},
		{Key: "note", Title: "Note", Description: "", Kind: AccessFieldKindText, Required: false, Options: []string{}, Choices: false},
	}}
}

func goldenIntegrationActions() []IntegrationAction {
	return []IntegrationAction{
		{Name: "create_post", Description: "Create a blog post as a draft."},
		{Name: "list_blogs", Description: "List the blogs the connection can see."},
		{Name: "whoami", Description: "Name the connected account."},
	}
}

// goldenConnectionState is HubSpot's one connection, which the page
// does not ask to name.
func goldenConnectionState() *ConnectionState {
	return &ConnectionState{
		ID:              "c7f3a1b2",
		CreatedAt:       integrationNow,
		Fields:          goldenConnectionFields(),
		VerifiedAt:      &integrationLater,
		Verified:        &integrationVerified,
		VerifyMessage:   &integrationVerifyMsg,
		ReconnectNeeded: false,
	}
}

// goldenGitConnectionFields is Git's connection form with what one
// connection holds: its host, its token set.
func goldenGitConnectionFields(host string) []ConnectionField {
	return []ConnectionField{
		{Key: "host", Title: "Host", Description: "github.com, gitlab.com or any HTTPS git host, without https://", Sensitive: false, Required: true, Set: true, Value: &host},
		{Key: "token", Title: "Access token", Description: "A token that can read the repositories agents will use.", Sensitive: true, Required: true, Set: true},
	}
}

// integrationRetired is the reason an access row set for a connection
// boot set aside grants nothing.
var integrationRetired = "set for the connection gitlab.example.com, which Git no longer has"

// goldenIntegrationPageGit is Git's page: its one connection, whose
// secrets are needed again, one agent's access and one agent's access
// set for a connection boot set aside. Git has no connection test.
func goldenIntegrationPageGit() IntegrationPage {
	return IntegrationPage{
		Summary: IntegrationSummary{
			Name: "git", DisplayName: "Git", Description: "Clones, fetches and pushes repositories over HTTPS.",
			Version: "1.2.0", State: IntegrationStateConnected, Needs: 1, Enabled: true, Builtin: true, Agents: 2,
		},
		Author:           "Kivali",
		Hosts:            []string{},
		Packages:         []string{},
		Actions:          []IntegrationAction{{Name: "clone", Description: "Clones a repository."}, {Name: "push", Description: "Pushes branches and tags."}},
		ConnectionFields: goldenGitConnectionFields("github.com"),
		ConnectionMethod: "token",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Connection: &ConnectionState{
			ID: "a1b2c3d4", CreatedAt: integrationEarlier, Fields: goldenGitConnectionFields("github.com"), ReconnectNeeded: true,
		},
		ConnectionTest: false,
		AccessForm: &AccessForm{Fields: []AccessField{
			{Key: "repositories", Title: "Repositories", Description: "owner/name, or a pattern such as acme/*", Kind: AccessFieldKindList, Required: true, Options: []string{}, Choices: false},
			{Key: "push", Title: "May push", Description: "", Kind: AccessFieldKindSwitch, Required: false, Options: []string{}, Choices: false},
		}},
		Access: []AccessRow{
			{Agent: PersonRef{Slug: "dana", Name: "Dana"}, Access: json.RawMessage(`{"repositories":["acme/*"]}`), Summary: "Repositories: acme/*", SetAt: integrationNow},
			{Agent: PersonRef{Slug: "lee", Name: "Lee"}, Access: json.RawMessage(`{"repositories":["eng/*"],"push":true}`), Summary: "Repositories: eng/* · May push: no branches or tags", SetAt: integrationNow, Invalid: &integrationRetired},
		},
		AgentsWithout: []PersonRef{{Slug: "chief-of-staff", Name: "Chief of Staff"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.2.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali", Name: "Kivali"}, ApprovedAt: &integrationEarlier, NotesMD: "Ships with Kivali."},
		},
		Running: true,
		Requirements: []IntegrationRequirement{
			{ID: "version", Title: "Approved version", Words: "Version 1.2.0 ships with Kivali.", Met: true},
			{ID: "connection", Title: "Connection", Words: "Its secrets are not stored on this Kivali server, as after a restore from a backup. Enter them again.", Met: false, Fix: "setup"},
		},
		CanDisconnect: true,
		CanUninstall:  false,
	}
}

func goldenIntegrationPage() IntegrationPage {
	return IntegrationPage{
		Summary:          goldenIntegrationSummary(),
		Author:           "Kivali Developer (agent)",
		Hosts:            []string{"api.hubapi.com"},
		Packages:         []string{},
		Actions:          goldenIntegrationActions(),
		ConnectionFields: goldenConnectionFields(),
		ConnectionMethod: "token",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Connection:       goldenConnectionState(),
		ConnectionTest:   true,
		AccessForm:       goldenAccessForm(),
		Access: []AccessRow{
			{Agent: PersonRef{Slug: "dana", Name: "Dana"}, Access: json.RawMessage(`{"blogs":["Marketing"],"write":true}`), Summary: "Blogs: Marketing · May publish and edit", SetAt: integrationNow},
			{Agent: PersonRef{Slug: "lee", Name: "Lee"}, Access: json.RawMessage(`{"blogs":["Old"]}`), Summary: "Blogs: Old", SetAt: integrationNow, Invalid: &integrationInvalid},
		},
		AgentsWithout: []PersonRef{{Slug: "chief-of-staff", Name: "Chief of Staff"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.3.0", State: IntegrationVersionStatePending, PublishedAt: integrationLater, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, NotesMD: "Adds contacts."},
			{Version: "1.2.0", State: IntegrationVersionStateApproved, PublishedAt: integrationNow, PublishedBy: PersonRef{Slug: "ceo", Name: "Evan"}, ApprovedAt: &integrationLater, NotesMD: ""},
			{Version: "1.1.1", State: IntegrationVersionStateRejected, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, NotesMD: "Reaches a reporting host."},
			{Version: "1.1.0", State: IntegrationVersionStateSuperseded, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, ApprovedAt: &integrationEarlier, NotesMD: "Blog posts only."},
		},
		Running:       true,
		Requirements:  goldenRequirementsMet(),
		CanDisconnect: true,
		CanUninstall:  true,
	}
}

// goldenIntegrationPageDisabled is HubSpot’s page disabled: its
// connection and access kept, its requirements without the running
// row, nothing in Needs you.
func goldenIntegrationPageDisabled() IntegrationPage {
	page := goldenIntegrationPage()
	page.Summary.State, page.Summary.Enabled = IntegrationStateDisabled, false
	page.Running = false
	page.Requirements = goldenRequirementsMet()[:3]
	return page
}

// goldenIntegrationPagePending is an uploaded bundle's page before
// any version is approved: nothing installed, its one version waiting.
func goldenIntegrationPagePending() IntegrationPage {
	return IntegrationPage{
		Summary:          IntegrationSummary{Name: "weather", DisplayName: "Weather", Description: "Forecasts.", Version: "", State: IntegrationStatePending, Enabled: true, Agents: 0, PendingVersion: new("1.0.0")},
		Author:           "Kivali Developer (agent)",
		Hosts:            []string{"api.weather.example"},
		Packages:         []string{},
		Actions:          []IntegrationAction{{Name: "forecast", Description: "The forecast for a place."}},
		ConnectionFields: []ConnectionField{{Key: "key", Title: "API key", Description: "", Sensitive: true, Required: true, Set: false}},
		ConnectionMethod: "token",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Access:           []AccessRow{},
		AgentsWithout:    []PersonRef{{Slug: "dana", Name: "Dana"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.0.0", State: IntegrationVersionStatePending, PublishedAt: integrationLater, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, NotesMD: "Forecasts by place."},
		},
		Running:       false,
		Requirements:  []IntegrationRequirement{},
		CanDisconnect: false,
		CanUninstall:  true,
	}
}

// goldenIntegrationPageNotIncluded is the page of a built-in another
// version of Kivali shipped: nothing runs it, and it can be
// uninstalled.
func goldenIntegrationPageNotIncluded() IntegrationPage {
	page := goldenIntegrationPagePending()
	page.Summary = IntegrationSummary{Name: "maps", DisplayName: "Maps", Description: "Places and routes.", Version: "1.0.0", State: IntegrationStateNotIncluded, Enabled: true, Builtin: true}
	page.Author = "Kivali"
	page.Hosts = []string{}
	page.Versions = []IntegrationVersionSummary{
		{Version: "1.0.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali", Name: "Kivali"}, ApprovedAt: &integrationEarlier, NotesMD: "Ships with Kivali."},
	}
	return page
}

func goldenIntegrationVersion() IntegrationVersion {
	return IntegrationVersion{
		Name:             "hubspot",
		DisplayName:      "HubSpot",
		Description:      "Blog posts and contacts in HubSpot.",
		Version:          "1.3.0",
		State:            IntegrationVersionStatePending,
		Author:           "Kivali Developer (agent)",
		PublishedAt:      integrationLater,
		PublishedBy:      PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"},
		NotesMD:          "Adds contacts. Reaches one more host.",
		Runtime:          "python",
		ConnectionMethod: "token",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Hosts:            []string{"api.hubapi.com"},
		Packages:         []string{"gh"},
		ConnectionFields: goldenConnectionFields(),
		Actions:          goldenIntegrationActions(),
		AccessForm:       goldenAccessForm(),
		BundleBytes:      184320,
		FileCount:        12,
		Current:          false,
		CurrentVersion:   &integrationCurrent,
		Changes:          []string{"Hosts added: api.hubapi.com", "Software added: gh", "Access form fields added: Tags, Note"},
		Findings:         []string{"tools[2] (whoami) has no description"},
	}
}

// goldenBuiltinIntegrationVersion is the install screen of a version
// of an integration that ships with Kivali: published by Kivali, in
// use, with no bundle, no runtime and no hosts of its own.
func goldenBuiltinIntegrationVersion() IntegrationVersion {
	return IntegrationVersion{
		Name:             "git",
		DisplayName:      "Git",
		Description:      "Repositories on a Git host.",
		Version:          "1.0.0",
		State:            IntegrationVersionStateApproved,
		Author:           "Kivali",
		PublishedAt:      integrationEarlier,
		PublishedBy:      PersonRef{Slug: "kivali", Name: "Kivali"},
		ApprovedAt:       &integrationEarlier,
		NotesMD:          "Ships with Kivali.",
		Runtime:          "",
		ConnectionMethod: "token",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Hosts:            []string{},
		Packages:         []string{},
		ConnectionFields: []ConnectionField{{Key: "token", Title: "Access token", Description: "", Sensitive: true, Required: true, Set: false}},
		Actions:          []IntegrationAction{{Name: "clone", Description: "Clone a repository."}},
		AccessForm:       nil,
		BundleBytes:      0,
		FileCount:        0,
		Current:          true,
		Builtin:          true,
		Changes:          []string{},
		Findings:         []string{},
	}
}

// goldenIntegrationVersionEarlierBundle is GitHub 1.0.0's install
// screen after the update to 2.0.0: a built-in version an earlier
// Kivali shipped as a bundle, superseded, still holding its bundle's
// size, file count and software, which nothing runs any more.
func goldenIntegrationVersionEarlierBundle() IntegrationVersion {
	v := goldenBuiltinIntegrationVersion()
	current := "2.0.0"
	v.Name, v.DisplayName, v.Description = "github", "GitHub", "Read and open issues, comments and pull requests in GitHub repositories, signed in with your own GitHub App."
	v.State, v.Current, v.CurrentVersion = IntegrationVersionStateSuperseded, false, &current
	v.Runtime, v.ConnectionMethod, v.SignInClient = "python", "browser", "operator"
	v.SignInHosts = []string{"github.com"}
	v.Hosts = []string{"api.github.com", "github.com"}
	v.Packages = []string{"gh"}
	v.ConnectionFields = goldenSignInConnectionFields()
	v.Actions = []IntegrationAction{{Name: "list_issues", Description: "List a repository's issues by state, labels or search words, at most 50. Reads only."}}
	v.BundleBytes, v.FileCount = 4096, 3
	return v
}

// goldenSignInConnectionFields is a sign-in connection's form: the
// manifest's field and the client id and secret core adds.
func goldenSignInConnectionFields() []ConnectionField {
	clientID := "Iv23liAbCdEf"
	return []ConnectionField{
		{Key: "org", Title: "Organization", Description: "The organization agents work in.", Sensitive: false, Required: false, Set: false},
		{Key: "client_id", Title: "Client id", Description: "", Sensitive: false, Required: true, Set: true, Value: &clientID},
		{Key: "client_secret", Title: "Client secret", Description: "", Sensitive: true, Required: true, Set: true},
	}
}

// goldenIntegrationPageSignIn is a GitHub bundle's page, connected by
// signing in with the device flow (browser offered too, with the
// redirect address to register), its token refreshed before it
// expires. needsSignIn makes it the page of the same connection after
// the vendor refused a refresh.
func goldenIntegrationPageSignIn(needsSignIn bool) IntegrationPage {
	expires := integrationLater.Add(8 * time.Hour)
	redirect := "https://kivali.example.com/connect/callback"
	sign := &ConnectionSignIn{Ways: []string{"device", "browser"}, SignedIn: true, Method: "device", SignedInAt: &integrationNow, ExpiresAt: &expires, RedirectURL: &redirect}
	signInRow := IntegrationRequirement{ID: "sign_in", Title: "Sign-in", Words: "Signed in with a code.", Met: true, Fix: "sign_in"}
	needs := 0
	if needsSignIn {
		refused := "the vendor refused the sign-in (bad_refresh_token): The refresh token passed is incorrect or expired."
		sign = &ConnectionSignIn{Ways: []string{"device", "browser"}, SignedIn: false, Error: &refused, RedirectURL: &redirect}
		signInRow.Met, signInRow.Words = false, "The service no longer accepts the sign-in: "+refused+" Agents cannot use GitHub until you sign in again."
		needs = 1
	}
	conn := ConnectionState{ID: "a1b2c3d4", CreatedAt: integrationNow, Fields: goldenSignInConnectionFields(), SignIn: sign}
	return IntegrationPage{
		Summary: IntegrationSummary{
			Name: "github", DisplayName: "GitHub", Description: "Issues and pull requests on GitHub.",
			Version: "1.0.0", State: IntegrationStateConnected, Needs: needs, Enabled: true, Agents: 0,
		},
		Author:            "Kivali Developer (agent)",
		Hosts:             []string{"api.github.com"},
		Packages:          []string{},
		Actions:           []IntegrationAction{{Name: "list_issues", Description: "List a repository's open issues."}},
		ConnectionFields:  goldenSignInConnectionFields(),
		ConnectionMethod:  "device",
		SignInClient:      "operator",
		Scopes:            []string{"repo", "read:org"},
		SignInHosts:       []string{"github.com"},
		SignInAt:          map[string]string{"device": "github.com", "browser": "github.com"},
		SetupNotesMD:      "Register a GitHub App under **Settings, Developer settings**, set its callback URL to " + redirect + ", tick *Enable Device Flow*, then paste its client id and a client secret here.",
		SignInRedirectURL: &redirect,
		Connection:        &conn,
		ConnectionTest:    false,
		Access:            []AccessRow{},
		AgentsWithout:     []PersonRef{{Slug: "dana", Name: "Dana"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.0.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, ApprovedAt: &integrationEarlier, NotesMD: ""},
		},
		Running: true,
		Requirements: []IntegrationRequirement{
			{ID: "version", Title: "Approved version", Words: "Version 1.0.0 is in use.", Met: true},
			{ID: "connection", Title: "Connection", Words: "Its secrets are stored.", Met: true},
			signInRow,
			{ID: "running", Title: "Running", Words: "It is running.", Met: true},
		},
		CanDisconnect: true,
		CanUninstall:  true,
	}
}

// goldenIntegrationPageSignInElsewhere is the same GitHub page, its
// connection saved but never signed in, offering browser sign-in alone,
// as Kivali Desktop's window for a team elsewhere reads it: no way
// works here, so the page names the team's external address to sign in
// from, and shows no redirect address to register.
func goldenIntegrationPageSignInElsewhere() IntegrationPage {
	page := goldenIntegrationPageSignIn(false)
	openAt := "https://kivali.example.com"
	conn := *page.Connection
	conn.SignIn = &ConnectionSignIn{Ways: []string{}, BrowserElsewhere: true, OpenAt: &openAt}
	page.Summary.Needs = 1
	page.Requirements[2] = IntegrationRequirement{ID: "sign_in", Title: "Sign-in", Words: "Not signed in yet.", Met: false, Fix: "sign_in"}
	page.ConnectionMethod = "browser"
	page.SignInAt = map[string]string{"browser": "github.com"}
	page.SignInRedirectURL = nil
	page.Connection = &conn
	return page
}

func goldenIntegrationAudit() IntegrationAudit {
	return IntegrationAudit{Rows: []IntegrationAuditRow{
		{At: integrationLater, Agent: PersonRef{Slug: "dana", Name: "Dana"}, Action: "create_post", Allowed: true, Reason: "", DurationMS: 812},
		{At: integrationNow, Agent: PersonRef{Slug: "lee", Name: "Lee"}, Action: "create_post", Allowed: false, Reason: "Lee has no access to the Marketing blog", DurationMS: 3},
		{At: integrationEarlier, Agent: PersonRef{Slug: "ceo", Name: "Evan"}, Action: "rollback", Allowed: true, Reason: "from version 1.2.0 to version 1.1.0", DurationMS: 0},
	}}
}

func goldenAgentIntegrations() AgentIntegrations {
	return AgentIntegrations{
		Connected: []AgentIntegration{
			{Name: "hubspot", DisplayName: "HubSpot", State: IntegrationStateConnected, Usable: true, Summary: "Blogs: Marketing · May publish and edit"},
			{Name: "crm", DisplayName: "CRM", State: IntegrationStateDisabled, Usable: false, Summary: "Full access", Invalid: &integrationInvalid},
			{Name: "git", DisplayName: "Git", State: IntegrationStateConnected, Usable: false, Summary: "Repositories: eng/*"},
		},
		Available: []AgentIntegration{
			{Name: "weather", DisplayName: "Weather", State: IntegrationStatePending, Summary: ""},
		},
	}
}

// goldenIntegrationVersionSignIn is the install screen of a HubSpot
// bundle that connects by signing in in the browser: the scopes it
// asks for and the hosts its sign-in goes to.
func goldenIntegrationVersionSignIn() IntegrationVersion {
	v := goldenIntegrationVersion()
	v.ConnectionMethod = "browser"
	v.SignInClient = "operator"
	v.Scopes = []string{"content"}
	v.SignInHosts = []string{"api.hubapi.com", "app.hubspot.com"}
	v.ConnectionFields = []ConnectionField{
		{Key: "client_id", Title: "Client id", Description: "", Sensitive: false, Required: true, Set: false},
		{Key: "client_secret", Title: "Client secret", Description: "", Sensitive: true, Required: true, Set: false},
	}
	return v
}

// goldenIntegrationSetup is what GitHub's setup flow may do, as its
// manifest declares it (githubintegration): make a private GitHub App
// on the owner's account or an organization's from GitHub's own pages,
// asking for the account first, and keep the app's values.
func goldenIntegrationSetup() *IntegrationSetup {
	return &IntegrationSetup{
		Summary: "Creates a private GitHub App, on your account or an organization's, that may read contents and write issues and pull requests, and installs it on the repositories you choose. Disconnecting removes the installation; the app stays on GitHub until you delete it under GitHub Apps in the account's settings.",
		Hosts:   []string{"github.com"},
		Fields: []SetupField{
			{Key: "account", Title: "Account", Words: "An organization's name, or leave it blank for your personal account", Secret: false, SetupOnly: false},
		},
		Keeps: []SetupKeep{
			{Key: "app_id", Words: "The app's id", Secret: false},
			{Key: "slug", Words: "The app's name on GitHub", Secret: false},
			{Key: "client_id", Words: "The app's client id", Secret: false},
			{Key: "client_secret", Words: "The app's client secret", Secret: true},
			{Key: "private_key", Words: "The app's private key, which Kivali signs with to get a token for each call", Secret: true},
			{Key: "installation_id", Words: "Where the app is installed", Secret: false},
		},
		Then: "",
	}
}

// goldenGitHubDescription is GitHub's description, as its manifest
// gives it.
const goldenGitHubDescription = "Read and open issues, comments and pull requests in GitHub repositories, as a private GitHub App on your account."

// goldenIntegrationVersionSetup is the install screen of a built-in
// whose connection is made by its own setup flow: no connection
// fields to enter, the flow's declarations to read instead, and the
// hosts its setup and calls reach from Kivali.
func goldenIntegrationVersionSetup() IntegrationVersion {
	v := goldenBuiltinIntegrationVersion()
	v.Name, v.DisplayName, v.Description, v.Version = "github", "GitHub", goldenGitHubDescription, "2.0.0"
	v.ConnectionMethod = "setup"
	v.ConnectionFields = []ConnectionField{}
	v.Hosts = []string{"api.github.com", "github.com"}
	v.Setup = goldenIntegrationSetup()
	v.Actions = []IntegrationAction{{Name: "list_issues", Description: "List a repository's open issues."}}
	return v
}

// goldenIntegrationVersionRegistered is the install screen of a
// browser sign-in whose client Kivali registers itself: no client id or
// secret to paste, the discover host and the hosts the discovered
// endpoints may be on under Signs in at.
func goldenIntegrationVersionRegistered() IntegrationVersion {
	v := goldenIntegrationVersion()
	v.Name, v.DisplayName, v.Description = "zoominfo", "ZoomInfo", "Company and contact data from ZoomInfo."
	v.ConnectionMethod = "browser"
	v.SignInClient = "registered"
	v.Scopes = []string{}
	v.SignInHosts = []string{"mcp.zoominfo.com", "okta-login.zoominfo.com"}
	v.Hosts = []string{"api.zoominfo.com"}
	v.Packages = []string{}
	v.ConnectionFields = []ConnectionField{}
	v.Changes = []string{}
	v.Findings = []string{}
	return v
}

// goldenIntegrationPageSetup is GitHub's page before its setup has run:
// no connection yet, and the flow's declarations.
func goldenIntegrationPageSetup() IntegrationPage {
	setup := goldenIntegrationSetup()
	return IntegrationPage{
		Summary: IntegrationSummary{
			Name: "github", DisplayName: "GitHub", Description: goldenGitHubDescription,
			Version: "2.0.0", State: IntegrationStateInstalled, Enabled: true, Builtin: true, Agents: 0,
		},
		Author:           "Kivali",
		Hosts:            []string{"api.github.com", "github.com"},
		Packages:         []string{},
		Actions:          []IntegrationAction{{Name: "list_issues", Description: "List a repository's open issues."}},
		ConnectionFields: []ConnectionField{},
		ConnectionMethod: "setup",
		SignInClient:     "",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Setup:            setup,
		ConnectionTest:   true,
		Access:           []AccessRow{},
		AgentsWithout:    []PersonRef{{Slug: "dana", Name: "Dana"}},
		Versions: []IntegrationVersionSummary{
			{Version: "2.0.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali", Name: "Kivali"}, ApprovedAt: &integrationEarlier, NotesMD: "Ships with Kivali."},
		},
		Running:       true,
		Requirements:  []IntegrationRequirement{},
		CanDisconnect: false,
		CanUninstall:  false,
	}
}

// goldenIntegrationPageConnecting is the same GitHub page while its
// setup flow builds the app: still installed, with the flow under way.
func goldenIntegrationPageConnecting() IntegrationPage {
	page := goldenIntegrationPageSetup()
	page.Summary.State = IntegrationStateInstalled // connecting: no connection yet
	flow := goldenSetupFlowRunning()
	page.Flow = &flow
	return page
}

// goldenIntegrationPageFix is the same GitHub page once installed,
// one of its own requirements unmet and its Fix flow waiting for the
// owner to approve a permission on GitHub.
func goldenIntegrationPageFix() IntegrationPage {
	page := goldenIntegrationPageSetup()
	conn := ConnectionState{ID: "f00dcafe", CreatedAt: integrationNow, Fields: []ConnectionField{}, VerifiedAt: &integrationLater, Verified: &integrationVerified, VerifyMessage: new("Connected as kivali-acme[bot], the app Kivali Acme, installed on acme")}
	page.Summary.State, page.Summary.Needs = IntegrationStateConnected, 1
	page.Connection = &conn
	page.CanDisconnect = true
	page.Requirements = []IntegrationRequirement{
		{ID: "version", Title: "Approved version", Words: "Version 2.0.0 ships with Kivali.", Met: true},
		{ID: "connection", Title: "Connection", Words: "Its secrets are stored.", Met: true},
		{ID: "connection_test", Title: "Connection test", Words: "Connected as kivali-acme[bot], the app Kivali Acme, installed on acme", Met: true},
		{ID: "key", Title: "The app's key works", Words: "GitHub accepts the key of the app Kivali Acme. Disconnecting GitHub here leaves the app on GitHub, for you to delete in its settings at https://github.com/organizations/acme/settings/apps/kivali-acme.", Met: true},
		{ID: "installed", Title: "Installed on GitHub", Words: "Installed on acme.", Met: true},
		{ID: "repositories", Title: "Repositories it reaches", Words: "Installed on acme, reaching 12 repositories.", Met: true},
		{ID: "permissions", Title: "Permissions current", Words: "The installation lacks permissions this version needs: pull requests: write. Approve them on GitHub.", Met: false, Fix: "permissions"},
	}
	flow := goldenSetupFlowWait()
	flow.Flow = "permissions"
	page.Flow = &flow
	return page
}

// goldenIntegrationPageMisfit is the same GitHub page after an update
// from GitHub 1.0.0, whose connection (the owner's own app, signed in
// with) this version's setup does not take: available, with the one
// row saying to set it up again, and the agents' access kept.
func goldenIntegrationPageMisfit() IntegrationPage {
	page := goldenIntegrationPageSetup()
	page.Summary.Agents = 1
	// The earlier version's connection, which Disconnect drops.
	page.Connection = &ConnectionState{ID: "a1b2c3d4", CreatedAt: integrationEarlier, Fields: []ConnectionField{}}
	page.CanDisconnect = true
	page.Requirements = []IntegrationRequirement{{
		ID: "connection", Title: "Connection", Met: false, Fix: "setup",
		Words: "Set up GitHub again: this version makes its connection by its own setup, and the one an earlier version made does not fit it. Every agent's access is kept.",
	}}
	return page
}

// goldenSetupFlowID is the setup flows' id in the fixtures.
const goldenSetupFlowID = "9f86d081884c7d659a2feaa0c55ad015"

// goldenSetupFlow is a setup flow of GitHub's in state, its second, the rest
// filled in by the caller.
func goldenSetupFlow(state SetupFlowState) SetupFlow {
	return SetupFlow{ID: goldenSetupFlowID, Flow: "setup", State: state, Seq: 2, ExpiresAt: integrationLater.Add(10 * time.Minute)}
}

// goldenSetupFlowRunning is a flow working out its next step, three
// stages of five in.
func goldenSetupFlowRunning() SetupFlow {
	f := goldenSetupFlow(SetupFlowStateRunning)
	f.Progress = &SetupFlowProgress{Words: "Building the app", Done: 3, Total: 5}
	return f
}

// goldenSetupFlowForm is a flow asking for its declared fields, one of
// them a secret used only during setup.
func goldenSetupFlowForm() SetupFlow {
	f := goldenSetupFlow(SetupFlowStateForm)
	f.Step = &SetupFlowStep{Kind: SetupFlowStepKindForm, Words: "Name the account the app goes on.", Fields: []SetupField{
		{Key: "account", Title: "Account", Words: "Your personal account, or an organization's name"},
		{Key: "personal_access_key", Title: "Key", Words: "From the key page", Secret: true, SetupOnly: true},
	}}
	return f
}

// goldenSetupFlowGo is a flow sending the browser to the service and
// back.
func goldenSetupFlowGo() SetupFlow {
	f := goldenSetupFlow(SetupFlowStateGo)
	start, host := "/connect/start?flow="+goldenSetupFlowID, "github.com"
	f.Step = &SetupFlowStep{Kind: SetupFlowStepKindGo, Words: "Create the app on GitHub. GitHub sends you back here.", Fields: []SetupField{}, StartURL: &start, Host: &host}
	return f
}

// goldenSetupFlowGoNoReturn is a flow sending the browser to a
// settings page that sends it nowhere afterwards.
func goldenSetupFlowGoNoReturn() SetupFlow {
	f := goldenSetupFlowGo()
	f.Step.Words = "Choose the repositories the app may reach on GitHub."
	f.Step.NoReturn = true
	return f
}

// goldenSetupFlowWait is a device sign-in waiting for the owner to
// enter the code on the service's page.
func goldenSetupFlowWait() SetupFlow {
	f := goldenSetupFlow(SetupFlowStateWait)
	page, host, code := "https://github.com/login/device?user_code=WDJB-MJHT", "github.com", "WDJB-MJHT"
	f.Step = &SetupFlowStep{
		Kind: SetupFlowStepKindWait, Words: "Enter this code on the service’s page, then approve the sign-in there. Kivali finishes it here once you do.",
		Fields: []SetupField{}, URL: &page, Host: &host, Code: &code,
	}
	return f
}

// goldenSetupFlowFailed is a flow the service refused.
func goldenSetupFlowFailed() SetupFlow {
	f := goldenSetupFlow(SetupFlowStateFailed)
	words := "the vendor refused the sign-in (access_denied): The user has denied your application access."
	f.Error = &words
	return f
}
