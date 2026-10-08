// Package claudeauth reads the Claude Code CLI's own account of how it
// is signed in: the JSON `claude auth status --json` prints. The CLI is
// signed in by its own sign-in (a Claude subscription, an
// Anthropic Console account, Amazon Bedrock or Google Vertex AI) or, for
// a provider the CLI has no sign-in for (Microsoft Foundry), by the env
// block in its settings file (setups.go). Kivali asks the CLI rather than probing its files, so
// every method the CLI supports reads the same way.
//
// The server (through the Claude driver) and the desktop supervisor
// (through an exec into the server container) both parse with this
// package, so they describe a sign-in in the same words. It imports
// nothing from this module.
package claudeauth

import (
	"encoding/json"
	"errors"
	"strings"
)

// Args are the arguments, after the CLI binary, that print the status.
// The CLI exits 0 when signed in and 1 when not, printing the JSON
// either way.
func Args() []string { return []string{"auth", "status", "--json"} }

// Status is the part of `claude auth status --json` Kivali reads.
type Status struct {
	// LoggedIn is the CLI's own verdict: it has a credential it will
	// use for model calls.
	LoggedIn bool `json:"loggedIn"`
	// AuthMethod is "claude.ai" (a Claude subscription or Console
	// login), "oauth_token", "api_key", "api_key_helper",
	// "third_party" (a cloud provider) or "none".
	AuthMethod string `json:"authMethod"`
	// APIProvider is "firstParty" (Anthropic), "bedrock", "vertex",
	// "foundry", or another provider the CLI supports. The CLI reports a
	// cloud provider as logged in whenever its switch is set; whether
	// the credential works shows only on a model call.
	APIProvider string `json:"apiProvider"`
	// Email, OrgName and SubscriptionType are set for a claude.ai
	// login. SubscriptionType ("pro", "max", "team", "enterprise") is
	// empty for a Console account.
	Email            string `json:"email"`
	OrgName          string `json:"orgName"`
	SubscriptionType string `json:"subscriptionType"`
}

// Parse reads the CLI's output. The CLI prints null for an unknown
// email, organization or subscription; those read as empty.
func Parse(out []byte) (Status, error) {
	var raw struct {
		LoggedIn         *bool   `json:"loggedIn"`
		AuthMethod       string  `json:"authMethod"`
		APIProvider      string  `json:"apiProvider"`
		Email            *string `json:"email"`
		OrgName          *string `json:"orgName"`
		SubscriptionType *string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Status{}, errors.New("claude auth status: the output is not JSON")
	}
	if raw.LoggedIn == nil {
		return Status{}, errors.New("claude auth status: the output has no loggedIn")
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	return Status{
		LoggedIn:         *raw.LoggedIn,
		AuthMethod:       raw.AuthMethod,
		APIProvider:      raw.APIProvider,
		Email:            deref(raw.Email),
		OrgName:          deref(raw.OrgName),
		SubscriptionType: deref(raw.SubscriptionType),
	}, nil
}

// Billing names, in words, what the CLI's model calls are billed to:
// "Claude Max", "Anthropic Console", "Amazon Bedrock", "Google Vertex
// AI", "Microsoft Foundry". Empty when not signed in, or for a provider
// Kivali does not name.
func (s Status) Billing() string {
	if !s.LoggedIn {
		return ""
	}
	switch s.APIProvider {
	case "bedrock":
		return "Amazon Bedrock"
	case "vertex":
		return "Google Vertex AI"
	case "foundry":
		return "Microsoft Foundry"
	case "firstParty", "":
	default:
		return ""
	}
	if s.SubscriptionType != "" {
		return "Claude " + titleWord(s.SubscriptionType)
	}
	switch s.AuthMethod {
	case "claude.ai", "oauth_token":
		return "Anthropic Console"
	case "api_key", "api_key_helper":
		return "Anthropic API key"
	}
	return ""
}

// titleWord upper-cases the first letter of an ASCII word ("max" is
// "Max").
func titleWord(w string) string {
	if w == "" || w[0] < 'a' || w[0] > 'z' {
		return w
	}
	return string(w[0]-'a'+'A') + w[1:]
}
