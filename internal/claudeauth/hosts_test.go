package claudeauth

import (
	"reflect"
	"strings"
	"testing"
)

func settingsEnv(kv ...string) []byte {
	var b strings.Builder
	b.WriteString(`{"env":{`)
	for i := 0; i < len(kv); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + kv[i] + `":"` + kv[i+1] + `"`)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

func TestModelHosts(t *testing.T) {
	signedIn := func(provider string) Status { return Status{LoggedIn: true, APIProvider: provider} }
	for _, tc := range []struct {
		name        string
		st          Status
		settings    []byte
		aws         string
		want        []string
		wantMissing bool
	}{
		{
			name: "anthropic",
			st:   Status{LoggedIn: true, APIProvider: "firstParty", AuthMethod: "claude.ai"},
			want: []string{"api.anthropic.com", "platform.claude.com"},
		},
		{
			name:     "anthropic with a base URL",
			st:       signedIn("firstParty"),
			settings: settingsEnv("ANTHROPIC_BASE_URL", "https://llm.example.net/v1"),
			want:     []string{"api.anthropic.com", "llm.example.net", "platform.claude.com"},
		},
		{
			name:     "bedrock",
			st:       signedIn("bedrock"),
			settings: settingsEnv("CLAUDE_CODE_USE_BEDROCK", "1", "AWS_REGION", "us-east-1"),
			want: []string{
				"bedrock-runtime.us-east-1.amazonaws.com", "bedrock.us-east-1.amazonaws.com",
				"sts.amazonaws.com", "sts.us-east-1.amazonaws.com",
			},
		},
		{
			name:     "bedrock with an SSO profile through a session",
			st:       signedIn("bedrock"),
			settings: settingsEnv("AWS_REGION", "eu-west-1", "AWS_PROFILE", "dev"),
			aws:      "[profile dev]\nsso_session = corp\nregion = eu-west-1\n\n[sso-session corp]\nsso_region = us-east-2\nsso_start_url = https://corp.awsapps.com/start\n",
			want: []string{
				"bedrock-runtime.eu-west-1.amazonaws.com", "bedrock.eu-west-1.amazonaws.com",
				"oidc.us-east-2.amazonaws.com", "portal.sso.us-east-2.amazonaws.com",
				"sts.amazonaws.com", "sts.eu-west-1.amazonaws.com",
			},
		},
		{
			name:     "bedrock with a key profile",
			st:       signedIn("bedrock"),
			settings: settingsEnv("AWS_REGION", "us-west-2", "AWS_PROFILE", "keys"),
			aws:      "[profile keys]\nregion = us-west-2\n",
			want: []string{
				"bedrock-runtime.us-west-2.amazonaws.com", "bedrock.us-west-2.amazonaws.com",
				"sts.amazonaws.com", "sts.us-west-2.amazonaws.com",
			},
		},
		{
			name:        "bedrock with no region",
			st:          signedIn("bedrock"),
			settings:    settingsEnv("CLAUDE_CODE_USE_BEDROCK", "1"),
			want:        []string{"sts.amazonaws.com"},
			wantMissing: true,
		},
		{
			name:     "vertex",
			st:       signedIn("vertex"),
			settings: settingsEnv("CLOUD_ML_REGION", "us-east5", "VERTEX_REGION_CLAUDE_HAIKU_4_5", "global"),
			want: []string{
				"aiplatform.googleapis.com", "iamcredentials.googleapis.com", "oauth2.googleapis.com",
				"sts.googleapis.com", "us-east5-aiplatform.googleapis.com", "www.googleapis.com",
			},
		},
		{
			name:     "vertex global",
			st:       signedIn("vertex"),
			settings: settingsEnv("CLOUD_ML_REGION", "global"),
			want: []string{
				"aiplatform.googleapis.com", "iamcredentials.googleapis.com", "oauth2.googleapis.com",
				"sts.googleapis.com", "www.googleapis.com",
			},
		},
		{
			name:     "vertex multi-region",
			st:       signedIn("vertex"),
			settings: settingsEnv("CLOUD_ML_REGION", "eu"),
			want: []string{
				"aiplatform.eu.rep.googleapis.com", "iamcredentials.googleapis.com", "oauth2.googleapis.com",
				"sts.googleapis.com", "www.googleapis.com",
			},
		},
		{
			name:     "vertex with no region",
			st:       signedIn("vertex"),
			settings: settingsEnv("ANTHROPIC_VERTEX_PROJECT_ID", "p"),
			want: []string{
				"iamcredentials.googleapis.com", "oauth2.googleapis.com", "sts.googleapis.com", "www.googleapis.com",
			},
			wantMissing: true,
		},
		{
			name:     "foundry",
			st:       signedIn("foundry"),
			settings: settingsEnv("CLAUDE_CODE_USE_FOUNDRY", "1", "ANTHROPIC_FOUNDRY_RESOURCE", "my-resource"),
			want:     []string{"login.microsoftonline.com", "my-resource.services.ai.azure.com"},
		},
		{
			name:     "foundry by base URL",
			st:       signedIn("foundry"),
			settings: settingsEnv("ANTHROPIC_FOUNDRY_BASE_URL", "https://other.services.ai.azure.com/anthropic"),
			want:     []string{"login.microsoftonline.com", "other.services.ai.azure.com"},
		},
		{
			name:        "foundry with no resource",
			st:          signedIn("foundry"),
			settings:    settingsEnv("CLAUDE_CODE_USE_FOUNDRY", "1"),
			want:        []string{"login.microsoftonline.com"},
			wantMissing: true,
		},
		{
			name:        "region that is not a region",
			st:          signedIn("bedrock"),
			settings:    settingsEnv("AWS_REGION", "evil.example.com/"),
			want:        []string{"sts.amazonaws.com"},
			wantMissing: true,
		},
		{name: "signed out", st: Status{APIProvider: "firstParty"}, wantMissing: true},
		{name: "unknown provider", st: signedIn("mantle"), wantMissing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts, missing := ModelHosts(tc.st, tc.settings, []byte(tc.aws))
			if len(tc.want) == 0 {
				if len(hosts) != 0 {
					t.Errorf("hosts = %v, want none", hosts)
				}
			} else if !reflect.DeepEqual(hosts, tc.want) {
				t.Errorf("hosts = %v\nwant    %v", hosts, tc.want)
			}
			if (len(missing) > 0) != tc.wantMissing {
				t.Errorf("missing = %v, want missing=%v", missing, tc.wantMissing)
			}
		})
	}
}
