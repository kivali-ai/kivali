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
// signing in with the device flow (browser offered too), its token
// refreshed before it expires. needsSignIn makes it the page of the
// same connection after the vendor refused a refresh.
func goldenIntegrationPageSignIn(needsSignIn bool) IntegrationPage {
	expires := integrationLater.Add(8 * time.Hour)
	sign := &ConnectionSignIn{Ways: []string{"device", "browser"}, SignedIn: true, Method: "device", SignedInAt: &integrationNow, ExpiresAt: &expires}
	status := IntegrationStatusConnected
	if needsSignIn {
		refused := "the vendor refused the sign-in (bad_refresh_token): The refresh token passed is incorrect or expired."
		sign = &ConnectionSignIn{Ways: []string{"device", "browser"}, SignedIn: false, Error: &refused}
		status = IntegrationStatusNotConnected
	}
	conn := ConnectionState{ID: "a1b2c3d4", Label: "", CreatedAt: integrationNow, Fields: goldenSignInConnectionFields(), SignIn: sign}
	return IntegrationPage{
		Summary: IntegrationSummary{
			Name: "github", DisplayName: "GitHub", Description: "Issues and pull requests on GitHub.",
			Version: "1.0.0", Status: status, Enabled: true, Agents: 0,
		},
		Author:           "Kivali Developer (agent)",
		Hosts:            []string{"api.github.com"},
		Packages:         []string{},
		Actions:          []IntegrationAction{{Name: "list_issues", Description: "List a repository's open issues."}},
		ConnectionFields: goldenSignInConnectionFields(),
		ConnectionMethod: "device",
		Scopes:           []string{"repo", "read:org"},
		SetupNotesMD:     "Register a GitHub App under **Settings, Developer settings**, tick *Enable Device Flow*, then paste its client id and a client secret here.",
		Connection:       &conn,
		Connections:      []ConnectionState{conn},
		Multiple:         false,
		ConnectionTest:   false,
		Access:           []AccessRow{},
		AgentsWithout:    []PersonRef{{Slug: "dana", Name: "Dana"}},
		Versions: []IntegrationVersionSummary{
			{Version: "1.0.0", State: IntegrationVersionStateApproved, PublishedAt: integrationEarlier, PublishedBy: PersonRef{Slug: "kivali-developer", Name: "Kivali Developer"}, ApprovedAt: &integrationEarlier, NotesMD: ""},
		},
		CanRemove: true,
	}
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
