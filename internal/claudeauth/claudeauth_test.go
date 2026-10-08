package claudeauth

import "testing"

// Outputs of `claude auth status --json`. loggedOut, apiKey, bedrock
// and vertex were printed by CLI 2.1.280 (the version the image pins)
// against an empty config directory, the last three with
// ANTHROPIC_API_KEY, CLAUDE_CODE_USE_BEDROCK and CLAUDE_CODE_USE_VERTEX
// set; subscription and console carry the fields that version adds for
// a claude.ai login (email, orgId, orgName, subscriptionType).
const (
	loggedOut = `{
  "loggedIn": false,
  "authMethod": "none",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/data/claude-home/.claude/projects",
  "configDirectory": "/data/claude-home/.claude"
}`
	subscription = `{
  "loggedIn": true,
  "authMethod": "claude.ai",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/data/claude-home/.claude/projects",
  "configDirectory": "/data/claude-home/.claude",
  "email": "owner@example.com",
  "orgId": "6f1c2d4e-0000-4000-8000-000000000000",
  "orgName": "owner@example.com's Organization",
  "subscriptionType": "max"
}`
	console = `{
  "loggedIn": true,
  "authMethod": "claude.ai",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/data/claude-home/.claude/projects",
  "configDirectory": "/data/claude-home/.claude",
  "apiKeySource": "/login managed key",
  "email": "billing@example.com",
  "orgId": "6f1c2d4e-0000-4000-8000-000000000001",
  "orgName": "Example Inc",
  "subscriptionType": null
}`
	apiKey = `{
  "loggedIn": true,
  "authMethod": "api_key",
  "apiProvider": "firstParty",
  "analyticsDisabled": false,
  "projectsDirectory": "/data/claude-home/.claude/projects",
  "configDirectory": "/data/claude-home/.claude",
  "apiKeySource": "ANTHROPIC_API_KEY"
}`
	bedrock = `{
  "loggedIn": true,
  "authMethod": "third_party",
  "apiProvider": "bedrock",
  "analyticsDisabled": true,
  "projectsDirectory": "/data/claude-home/.claude/projects",
  "configDirectory": "/data/claude-home/.claude"
}`
	vertex = `{
  "loggedIn": true,
  "authMethod": "third_party",
  "apiProvider": "vertex",
  "analyticsDisabled": true,
  "projectsDirectory": "/data/claude-home/.claude/projects",
  "configDirectory": "/data/claude-home/.claude"
}`
	// foundry was printed by CLI 2.1.292 with the Foundry env block in
	// the settings file (SetFoundry's output) and nothing else.
	foundry = `{
  "loggedIn": true,
  "authMethod": "third_party",
  "apiProvider": "foundry",
  "analyticsDisabled": true,
  "projectsDirectory": "/data/claude-home/.claude/projects",
  "configDirectory": "/data/claude-home/.claude"
}`
)

func TestParseAndBilling(t *testing.T) {
	for _, tc := range []struct {
		name, out      string
		loggedIn       bool
		email, billing string
	}{
		{"logged out", loggedOut, false, "", ""},
		{"subscription", subscription, true, "owner@example.com", "Claude Max"},
		{"console", console, true, "billing@example.com", "Anthropic Console"},
		{"api key", apiKey, true, "", "Anthropic API key"},
		{"bedrock", bedrock, true, "", "Amazon Bedrock"},
		{"vertex", vertex, true, "", "Google Vertex AI"},
		{"foundry", foundry, true, "", "Microsoft Foundry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Parse([]byte(tc.out))
			if err != nil {
				t.Fatal(err)
			}
			if st.LoggedIn != tc.loggedIn || st.Email != tc.email || st.Billing() != tc.billing {
				t.Fatalf("got %+v billing %q; want loggedIn %v email %q billing %q", st, st.Billing(), tc.loggedIn, tc.email, tc.billing)
			}
		})
	}
}

func TestBillingNamesEachSubscription(t *testing.T) {
	for sub, want := range map[string]string{"pro": "Claude Pro", "max": "Claude Max", "team": "Claude Team", "enterprise": "Claude Enterprise"} {
		st := Status{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "firstParty", SubscriptionType: sub}
		if got := st.Billing(); got != want {
			t.Errorf("%s: %q, want %q", sub, got, want)
		}
	}
	// A provider Kivali does not name, and a status that is not signed
	// in, have no billing words.
	if got := (Status{LoggedIn: true, AuthMethod: "third_party", APIProvider: "gateway"}).Billing(); got != "" {
		t.Errorf("other provider: %q", got)
	}
	if got := (Status{AuthMethod: "claude.ai", SubscriptionType: "max"}).Billing(); got != "" {
		t.Errorf("not signed in: %q", got)
	}
}

func TestParseRefusesOtherOutput(t *testing.T) {
	for _, out := range []string{"", "Not logged in. Run claude auth login to authenticate.", `{"authMethod":"none"}`, `[]`} {
		if _, err := Parse([]byte(out)); err == nil {
			t.Errorf("%q parsed", out)
		}
	}
}
