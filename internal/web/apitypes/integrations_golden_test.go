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

func goldenIntegrationPage() IntegrationPage {
	return IntegrationPage{
		Summary:          goldenIntegrationSummary(),
		Author:           "Kivali Developer (agent)",
		Hosts:            []string{"api.hubapi.com"},
		Actions:          goldenIntegrationActions(),
		ConnectionFields: goldenConnectionFields(),
		Connection: &ConnectionState{
			ID:              "c7f3a1b2",
			CreatedAt:       integrationNow,
			VerifiedAt:      &integrationLater,
			Verified:        &integrationVerified,
			VerifyMessage:   &integrationVerifyMsg,
			ReconnectNeeded: false,
		},
		AccessForm: goldenAccessForm(),
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
		Hosts:            []string{"api.hubapi.com"},
		ConnectionFields: goldenConnectionFields(),
		Actions:          goldenIntegrationActions(),
		AccessForm:       goldenAccessForm(),
		BundleBytes:      184320,
		FileCount:        12,
		Current:          false,
		CurrentVersion:   &integrationCurrent,
		Changes:          []string{"Hosts added: api.hubapi.com", "Access form fields added: Tags, Note"},
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
		Hosts:            []string{},
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
		},
		Available: []AgentIntegration{
			{Name: "weather", DisplayName: "Weather", Status: IntegrationStatusPending, Summary: ""},
		},
	}
}
