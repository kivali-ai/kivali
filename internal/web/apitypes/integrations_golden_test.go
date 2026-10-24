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
		Status:         IntegrationStatusConnected,
		Enabled:        true,
		Builtin:        false,
		Agents:         2,
		PendingVersion: &integrationVersion2,
	}
}

func goldenIntegrations() Integrations {
	return Integrations{Integrations: []IntegrationSummary{
		goldenIntegrationSummary(),
		{Name: "weather", DisplayName: "Weather", Description: "Forecasts.", Version: "", Status: IntegrationStatusPending, Enabled: true, Agents: 0},
		{Name: "maps", DisplayName: "Maps", Description: "Places and routes.", Version: "1.0.0", Status: IntegrationStatusNotIncluded, Enabled: true, Builtin: true, Agents: 0},
	}}
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
		Label:           "",
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

// goldenIntegrationPageMultiple is Git's page with two connections,
// GitHub and a company GitLab (whose secrets are needed again), and
// one agent's access on each. Git has no connection test.
func goldenIntegrationPageMultiple() IntegrationPage {
	return IntegrationPage{
		Summary: IntegrationSummary{
			Name: "git", DisplayName: "Git", Description: "Clones, fetches and pushes repositories over HTTPS.",
			Version: "1.2.0", Status: IntegrationStatusConnected, Enabled: true, Builtin: true, Agents: 2,
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
			ID: "a1b2c3d4", Label: "GitHub", CreatedAt: integrationEarlier, Fields: goldenGitConnectionFields("github.com"),
		},
		Connections: []ConnectionState{
			{ID: "a1b2c3d4", Label: "GitHub", CreatedAt: integrationEarlier, Fields: goldenGitConnectionFields("github.com")},
			{ID: "e5f6a7b8", Label: "Company GitLab", CreatedAt: integrationNow, Fields: goldenGitConnectionFields("gitlab.example.com"), ReconnectNeeded: true},
		},
		Multiple:       true,
		ConnectionTest: false,
		AccessForm: &AccessForm{Fields: []AccessField{
			{Key: "repositories", Title: "Repositories", Description: "owner/name, or a pattern such as acme/*", Kind: AccessFieldKindList, Required: true, Options: []string{}, Choices: false},
			{Key: "push", Title: "May push", Description: "", Kind: AccessFieldKindSwitch, Required: false, Options: []string{}, Choices: false},
		}},
		Access: []AccessRow{
			{Agent: PersonRef{Slug: "dana", Name: "Dana"}, Access: json.RawMessage(`{"repositories":["acme/*"]}`), Summary: "Repositories: acme/*", SetAt: integrationNow, ConnectionID: "a1b2c3d4", ConnectionLabel: "GitHub"},
			{Agent: PersonRef{Slug: "lee", Name: "Lee"}, Access: json.RawMessage(`{"repositories":["eng/*"],"push":true}`), Summary: "Repositories: eng/* · May push: no branches or tags", SetAt: integrationNow, ConnectionID: "e5f6a7b8", ConnectionLabel: "Company GitLab"},
		},
		AgentsWithout: []PersonRef{{Slug: "chief-of-staff", Name: "Chief of Staff"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.2.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali", Name: "Kivali"}, ApprovedAt: &integrationEarlier, NotesMD: "Ships with Kivali."},
		},
		CanRemove: false,
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
		Connections:      []ConnectionState{*goldenConnectionState()},
		Multiple:         false,
		ConnectionTest:   true,
		AccessForm:       goldenAccessForm(),
		Access: []AccessRow{
			{Agent: PersonRef{Slug: "dana", Name: "Dana"}, Access: json.RawMessage(`{"blogs":["Marketing"],"write":true}`), Summary: "Blogs: Marketing · May publish and edit", SetAt: integrationNow, ConnectionID: "c7f3a1b2"},
			{Agent: PersonRef{Slug: "lee", Name: "Lee"}, Access: json.RawMessage(`{"blogs":["Old"]}`), Summary: "Blogs: Old", SetAt: integrationNow, ConnectionID: "c7f3a1b2", Invalid: &integrationInvalid},
		},
		AgentsWithout: []PersonRef{{Slug: "chief-of-staff", Name: "Chief of Staff"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.3.0", State: IntegrationVersionStatePending, PublishedAt: integrationLater, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, NotesMD: "Adds contacts."},
			{Version: "1.2.0", State: IntegrationVersionStateApproved, PublishedAt: integrationNow, PublishedBy: PersonRef{Slug: "ceo", Name: "Evan"}, ApprovedAt: &integrationLater, NotesMD: ""},
			{Version: "1.1.1", State: IntegrationVersionStateRejected, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, NotesMD: "Reaches a reporting host."},
			{Version: "1.1.0", State: IntegrationVersionStateSuperseded, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, ApprovedAt: &integrationEarlier, NotesMD: "Blog posts only."},
		},
		CanRemove: true,
	}
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
	status := IntegrationStatusConnected
	if needsSignIn {
		refused := "the vendor refused the sign-in (bad_refresh_token): The refresh token passed is incorrect or expired."
		sign = &ConnectionSignIn{Ways: []string{"device", "browser"}, SignedIn: false, Error: &refused, RedirectURL: &redirect}
		status = IntegrationStatusNotConnected
	}
	conn := ConnectionState{ID: "a1b2c3d4", Label: "", CreatedAt: integrationNow, Fields: goldenSignInConnectionFields(), SignIn: sign}
	return IntegrationPage{
		Summary: IntegrationSummary{
			Name: "github", DisplayName: "GitHub", Description: "Issues and pull requests on GitHub.",
			Version: "1.0.0", Status: status, Enabled: true, Agents: 0,
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
		Connections:       []ConnectionState{conn},
		Multiple:          false,
		ConnectionTest:    false,
		Access:            []AccessRow{},
		AgentsWithout:     []PersonRef{{Slug: "dana", Name: "Dana"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.0.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, ApprovedAt: &integrationEarlier, NotesMD: ""},
		},
		CanRemove: true,
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
	page.Summary.Status = IntegrationStatusNotConnected
	page.ConnectionMethod = "browser"
	page.SignInAt = map[string]string{"browser": "github.com"}
	page.SignInRedirectURL = nil
	page.Connection = &conn
	page.Connections = []ConnectionState{conn}
	return page
}

// goldenShippedIntegrationVersion is the install screen of a version
// of an integration that ships with Kivali as a bundle: built in, with
// a bundle's runtime, hosts and size.
func goldenShippedIntegrationVersion() IntegrationVersion {
	return IntegrationVersion{
		Name:             "notes",
		DisplayName:      "Notes",
		Description:      "Notes in a notes service.",
		Version:          "1.0.0",
		State:            IntegrationVersionStateApproved,
		Author:           "Kivali",
		PublishedAt:      integrationEarlier,
		PublishedBy:      PersonRef{Slug: "kivali", Name: "Kivali"},
		ApprovedAt:       &integrationEarlier,
		NotesMD:          "Ships with Kivali.",
		Runtime:          "python",
		ConnectionMethod: "token",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Hosts:            []string{"api.notes.example.com"},
		Packages:         []string{},
		ConnectionFields: []ConnectionField{{Key: "token", Title: "API token", Description: "", Sensitive: true, Required: true, Set: false}},
		Actions:          []IntegrationAction{{Name: "create_note", Description: "Create a note."}},
		AccessForm:       nil,
		BundleBytes:      4096,
		FileCount:        3,
		Current:          true,
		Builtin:          true,
		Shipped:          true,
		Changes:          []string{},
		Findings:         []string{},
	}
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
			{Name: "hubspot", DisplayName: "HubSpot", Status: IntegrationStatusConnected, Summary: "Blogs: Marketing · May publish and edit"},
			{Name: "crm", DisplayName: "CRM", Status: IntegrationStatusOff, Summary: "Full access", Invalid: &integrationInvalid},
			{Name: "git", DisplayName: "Git", Status: IntegrationStatusConnected, Summary: "Repositories: eng/*", ConnectionLabel: "Company GitLab"},
		},
		Available: []AgentIntegration{
			{Name: "weather", DisplayName: "Weather", Status: IntegrationStatusPending, Summary: ""},
		},
	}
}

// goldenDeviceSignIn is a device sign-in just started, with the
// service's page that has the code filled in.
func goldenDeviceSignIn() DeviceSignIn {
	complete := "https://github.com/login/device?user_code=WDJB-MJHT"
	return DeviceSignIn{
		UserCode: "WDJB-MJHT", VerificationURL: "https://github.com/login/device",
		VerificationURLComplete: &complete, ExpiresAt: integrationLater.Add(15 * time.Minute),
	}
}

// goldenSignInFailed is a device sign-in the owner turned down on the
// service's page.
func goldenSignInFailed() SignInProgress {
	words := "the vendor refused the sign-in (access_denied): The user has denied your application access."
	return SignInProgress{State: SignInStateFailed, Error: &words}
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

// goldenIntegrationSetup is what a GitHub built-in's setup flow may
// do: make a private GitHub App on the owner's account from GitHub's
// own pages, asking for the account first, and keep the app's keys.
func goldenIntegrationSetup() *IntegrationSetup {
	return &IntegrationSetup{
		Summary: "Creates a private GitHub App on your account that may write contents, issues and pull requests, and installs it on the repositories you choose.",
		Hosts:   []string{"github.com"},
		Fields: []SetupField{
			{Key: "account", Title: "Account", Words: "Your personal account, or an organization's name", Secret: false, SetupOnly: false},
			{Key: "personal_access_key", Title: "Personal access key", Words: "From the service's key page; used once, never kept", Secret: true, SetupOnly: true},
		},
		Keeps: []SetupKeep{
			{Key: "app_id", Words: "The app's id"},
			{Key: "private_key", Words: "The app's private key, which Kivali signs with to get a token for each call"},
			{Key: "installation_id", Words: "Where the app is installed"},
		},
		Then: "",
	}
}

// goldenIntegrationVersionSetup is the install screen of a built-in
// whose connection is made by its own setup flow: no connection
// fields to enter, and the flow's declarations to read instead.
func goldenIntegrationVersionSetup() IntegrationVersion {
	v := goldenBuiltinIntegrationVersion()
	v.Name, v.DisplayName, v.Description, v.Version = "github", "GitHub", "Issues and pull requests on GitHub.", "2.0.0"
	v.ConnectionMethod = "setup"
	v.ConnectionFields = []ConnectionField{}
	v.Setup = goldenIntegrationSetup()
	v.Actions = []IntegrationAction{{Name: "list_issues", Description: "List a repository's open issues."}}
	return v
}

// goldenIntegrationPageSetup is the page of the same built-in before
// its setup has run: no connection yet, and the flow's declarations.
func goldenIntegrationPageSetup() IntegrationPage {
	return IntegrationPage{
		Summary: IntegrationSummary{
			Name: "github", DisplayName: "GitHub", Description: "Issues and pull requests on GitHub.",
			Version: "2.0.0", Status: IntegrationStatusNotConnected, Enabled: true, Builtin: true, Agents: 0,
		},
		Author:           "Kivali",
		Hosts:            []string{},
		Packages:         []string{},
		Actions:          []IntegrationAction{{Name: "list_issues", Description: "List a repository's open issues."}},
		ConnectionFields: []ConnectionField{},
		ConnectionMethod: "setup",
		Scopes:           []string{},
		SignInHosts:      []string{},
		Setup:            goldenIntegrationSetup(),
		Connections:      []ConnectionState{},
		Multiple:         false,
		ConnectionTest:   true,
		Access:           []AccessRow{},
		AgentsWithout:    []PersonRef{{Slug: "dana", Name: "Dana"}},
		Versions: []IntegrationVersionSummary{
			{Version: "2.0.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali", Name: "Kivali"}, ApprovedAt: &integrationEarlier, NotesMD: "Ships with Kivali."},
		},
		CanRemove: false,
	}
}

// goldenBrowserSignIn is a browser sign-in just started: the vendor's
// sign-in page with the client id, the redirect address, the scopes,
// the state and the PKCE challenge, and the flow id the page sends the
// browser there by.
func goldenBrowserSignIn() BrowserSignIn {
	return BrowserSignIn{Flow: "9f86d081884c7d659a2feaa0c55ad015", URL: "https://app.hubspot.com/oauth/authorize?client_id=7fff1e36-2d40-4ae1-bbb1-5266d59564fb&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256&redirect_uri=https%3A%2F%2Fkivali.example.com%2Fconnect%2Fcallback&response_type=code&scope=content&state=3q2-7wEBAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhs"}
}
