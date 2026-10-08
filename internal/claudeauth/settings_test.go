package claudeauth

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

func envOf(t *testing.T, b []byte) (map[string]any, map[string]string) {
	t.Helper()
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	env := map[string]string{}
	if e, ok := all["env"].(map[string]any); ok {
		for k, v := range e {
			env[k], _ = v.(string)
		}
	}
	return all, env
}

var (
	foundryKey = map[string]string{"resource": "my-resource", "auth": "api_key", "api_key": "k1"}
	foundrySP  = map[string]string{"resource": "other", "auth": "service_principal", "tenant_id": "t", "client_id": "c", "client_secret": "s"}
)

// Applying Microsoft Foundry keeps every other setting and env
// variable, and replaces whatever provider was there before (a Bedrock
// sign-in here).
func TestApplyFoundryMergesAndReplacesTheProvider(t *testing.T) {
	before := `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls)"]},
  "env": {
    "CLAUDE_CODE_USE_BEDROCK": "1",
    "AWS_REGION": "us-east-1",
    "AWS_BEARER_TOKEN_BEDROCK": "old",
    "DISABLE_TELEMETRY": "1",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5"
  }
}`
	out, err := ApplySetup([]byte(before), SetupMicrosoftFoundry, foundryKey)
	if err != nil {
		t.Fatal(err)
	}
	all, env := envOf(t, out)
	if all["model"] != "opus" || all["permissions"] == nil {
		t.Errorf("other keys lost: %s", out)
	}
	want := map[string]string{
		"CLAUDE_CODE_USE_FOUNDRY":        "1",
		"ANTHROPIC_FOUNDRY_RESOURCE":     "my-resource",
		"ANTHROPIC_FOUNDRY_API_KEY":      "k1",
		"DISABLE_TELEMETRY":              "1",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5",
	}
	if !maps.Equal(env, want) {
		t.Errorf("env %v, want %v", env, want)
	}

	// Switching to a service principal drops the API key.
	out, err = ApplySetup(out, SetupMicrosoftFoundry, foundrySP)
	if err != nil {
		t.Fatal(err)
	}
	_, env = envOf(t, out)
	if _, ok := env["ANTHROPIC_FOUNDRY_API_KEY"]; ok || env["AZURE_TENANT_ID"] != "t" || env["AZURE_CLIENT_ID"] != "c" || env["AZURE_CLIENT_SECRET"] != "s" || env["ANTHROPIC_FOUNDRY_RESOURCE"] != "other" {
		t.Errorf("service principal env %v", env)
	}

	// No settings file yet: just the block. Values for the way not
	// chosen are ignored.
	out, err = ApplySetup(nil, SetupMicrosoftFoundry, map[string]string{"resource": " r ", "auth": "api_key", "api_key": "k", "client_secret": "unused"})
	if err != nil {
		t.Fatal(err)
	}
	all, env = envOf(t, out)
	if len(all) != 1 || !maps.Equal(env, map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1", "ANTHROPIC_FOUNDRY_RESOURCE": "r", "ANTHROPIC_FOUNDRY_API_KEY": "k"}) {
		t.Errorf("fresh %s", out)
	}
}

// A file that is not a settings object is never overwritten.
func TestApplyRefusesAFileItCannotRead(t *testing.T) {
	for _, bad := range []string{"{", "[]", `"x"`, `{"env": "CLAUDE_CODE_USE_BEDROCK=1"}`} {
		if _, err := ApplySetup([]byte(bad), SetupMicrosoftFoundry, foundryKey); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if _, _, err := ClearProvider([]byte(bad)); err == nil {
			t.Errorf("clear %q accepted", bad)
		}
	}
}

func TestCheckFoundry(t *testing.T) {
	with := func(m map[string]string, kv ...string) map[string]string {
		out := maps.Clone(m)
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	for _, v := range []map[string]string{foundryKey, foundrySP, with(foundryKey, "resource", "a"), with(foundryKey, "resource", "Res01")} {
		if err := CheckSetup(SetupMicrosoftFoundry, v); err != nil {
			t.Errorf("%v: %v", v, err)
		}
	}
	for _, v := range []map[string]string{
		with(foundryKey, "resource", ""),
		with(foundryKey, "resource", "https://my-resource.services.ai.azure.com/anthropic"),
		with(foundryKey, "resource", "my-resource.services.ai.azure.com"),
		with(foundryKey, "resource", "-res"),
		with(foundryKey, "resource", "res-"),
		with(foundryKey, "resource", "my_resource"),
		with(foundryKey, "api_key", ""),
		with(foundryKey, "api_key", "a key"),
		with(foundryKey, "auth", ""),
		with(foundrySP, "client_id", ""),
	} {
		if err := CheckSetup(SetupMicrosoftFoundry, v); err == nil {
			t.Errorf("%v accepted", v)
		}
		if _, err := ApplySetup(nil, SetupMicrosoftFoundry, v); err == nil {
			t.Errorf("ApplySetup %v accepted", v)
		}
	}
	if err := CheckSetup("nope", foundryKey); err == nil || ValidSetup("nope") || !ValidSetup(SetupMicrosoftFoundry) {
		t.Errorf("unknown setup: %v", err)
	}
}

// Clearing removes each provider's switch and credential and nothing
// else; with none there, it changes nothing.
func TestClearProvider(t *testing.T) {
	in := `{"theme":"dark","env":{"CLAUDE_CODE_USE_VERTEX":"1","ANTHROPIC_VERTEX_PROJECT_ID":"p","CLOUD_ML_REGION":"global",` +
		`"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_RESOURCE":"r","AZURE_CLIENT_SECRET":"s","DISABLE_TELEMETRY":"1"}}`
	out, cleared, err := ClearProvider([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ANTHROPIC_FOUNDRY_RESOURCE", "ANTHROPIC_VERTEX_PROJECT_ID", "AZURE_CLIENT_SECRET", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_VERTEX", "CLOUD_ML_REGION"}; !slices.Equal(cleared, want) {
		t.Errorf("cleared %v, want %v", cleared, want)
	}
	all, env := envOf(t, out)
	if all["theme"] != "dark" || len(env) != 1 || env["DISABLE_TELEMETRY"] != "1" {
		t.Errorf("after clear %s", out)
	}
	// The env object goes once it is empty.
	out, _, _ = ClearProvider([]byte(`{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`))
	if all, _ := envOf(t, out); len(all) != 0 {
		t.Errorf("empty env kept: %s", out)
	}
	for _, none := range []string{"", `{"env":{"DISABLE_TELEMETRY":"1"}}`, `{"model":"opus"}`} {
		out, cleared, err := ClearProvider([]byte(none))
		if err != nil || out != nil || cleared != nil {
			t.Errorf("%q: %s %v %v", none, out, cleared, err)
		}
	}
}

// Every variable a Bedrock or Vertex wizard writes, and every variable
// a setup writes, is a provider key, so Sign in again clears it.
func TestProviderEnvKeysCoverEveryProvider(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_REGION", "AWS_PROFILE", "AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if !slices.Contains(ProviderEnvKeys, k) {
			t.Errorf("%s missing", k)
		}
	}
	values := map[string][]map[string]string{SetupMicrosoftFoundry: {foundryKey, foundrySP}}
	for id, s := range setups {
		if len(values[id]) == 0 {
			t.Errorf("setup %s has no test values", id)
		}
		for _, v := range values[id] {
			for k := range s.env(v) {
				if !slices.Contains(ProviderEnvKeys, k) {
					t.Errorf("%s writes %s, which a new sign-in does not clear", id, k)
				}
			}
		}
	}
}

// What is read back names the setup and never a secret.
func TestReadSetup(t *testing.T) {
	for _, tc := range []struct {
		in   string
		ok   bool
		want map[string]string
	}{
		{`{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_RESOURCE":"my-resource","ANTHROPIC_FOUNDRY_API_KEY":"k"}}`, true, map[string]string{"resource": "my-resource", "auth": "api_key"}},
		{`{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_BASE_URL":"https://res2.services.ai.azure.com/anthropic","AZURE_TENANT_ID":"t","AZURE_CLIENT_ID":"c","AZURE_CLIENT_SECRET":"s"}}`, true,
			map[string]string{"resource": "res2", "auth": "service_principal", "tenant_id": "t", "client_id": "c"}},
		{`{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_RESOURCE":"r"}}`, true, map[string]string{"resource": "r"}},
		{`{"env":{"CLAUDE_CODE_USE_FOUNDRY":"0","ANTHROPIC_FOUNDRY_RESOURCE":"r"}}`, false, nil},
		{`{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`, false, nil},
		{``, false, nil},
		{`nope`, false, nil},
	} {
		id, got, ok := ReadSetup([]byte(tc.in))
		if ok != tc.ok || !maps.Equal(got, tc.want) || (ok && id != SetupMicrosoftFoundry) {
			t.Errorf("%s: %s %v %v, want %v %v", tc.in, id, got, ok, tc.want, tc.ok)
		}
	}
	if id, ok := SetupForProvider("foundry"); !ok || id != SetupMicrosoftFoundry {
		t.Errorf("SetupForProvider(foundry) = %q %v", id, ok)
	}
	if _, ok := SetupForProvider("bedrock"); ok {
		t.Error("bedrock is the CLI's own sign-in")
	}
	if got := SetupDetail(SetupMicrosoftFoundry, map[string]string{"resource": "r"}); got != "r" {
		t.Errorf("detail %q", got)
	}
	if got := MissingModel(SetupMicrosoftFoundry, map[string]string{"resource": "r"}, "claude-sonnet-5"); got != "No deployment named claude-sonnet-5 in r" {
		t.Errorf("missing %q", got)
	}
}

// Outputs of a check, printed by CLI 2.1.292 against a stand-in for the
// Foundry endpoint that answered as Azure does: 404 DeploymentNotFound,
// then 401 for a bad key (with CLAUDE_CODE_MAX_RETRIES=0).
const (
	probeMissing = `{"type":"result","subtype":"success","is_error":true,"api_error_status":404,"num_turns":1,"terminal_reason":"api_error",` +
		`"result":"The model claude-sonnet-5 is not available on your foundry deployment. Try --model to switch to claude-sonnet-4-6, or ask your admin to enable this model."}`
	probeRefused = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"num_turns":1,"terminal_reason":"api_error",` +
		`"result":"Microsoft Foundry authentication failed · refresh your Foundry credential (ANTHROPIC_FOUNDRY_AUTH_TOKEN, ANTHROPIC_FOUNDRY_API_KEY, Azure sign-in for Entra, or your proxy token) and retry · if credentials are current, check access to the Foundry resource · API Error: 401 Access denied due to invalid subscription key or wrong API endpoint."}`
	probeOK = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"OK"}`
)

func TestClassifyProbe(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr string
		code                 int
		state, detail        string
	}{
		{"answered", probeOK, "", 0, ModelOK, ""},
		{"no deployment", probeMissing, "", 1, ModelMissing, ""},
		{"selected model", `{"is_error":true,"result":"There's an issue with the selected model (claude-opus-5-5). It may not exist or you may not have access to it."}`, "", 1, ModelMissing, ""},
		{"azure code", `{"is_error":true,"result":"API Error: 400 {\"error\":{\"code\":\"DeploymentNotFound\"}}"}`, "", 1, ModelMissing, ""},
		{"refused", probeRefused, "", 1, ModelError, "the provider refused the sign-in (HTTP 401)"},
		{"server error", `{"is_error":true,"api_error_status":529,"result":"API Error: 529 Overloaded. This is a server-side issue."}`, "", 1, ModelError, "529 Overloaded. This is a server-side issue."},
		{"no json", "", "Error: connect ECONNREFUSED\n  at x", 1, ModelError, "Error: connect ECONNREFUSED"},
		{"silent", "", "", 1, ModelError, "no answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, detail := ClassifyProbe([]byte(tc.stdout), []byte(tc.stderr), tc.code)
			if state != tc.state || detail != tc.detail {
				t.Fatalf("%q %q, want %q %q", state, detail, tc.state, tc.detail)
			}
		})
	}
	long := strings.Repeat("é", 300)
	if _, d := ClassifyProbe(nil, []byte(long), 1); len(d) > 204 || !strings.HasSuffix(d, "…") {
		t.Errorf("long detail %d bytes", len(d))
	}
}

func TestProbeArgsStayTiny(t *testing.T) {
	a := strings.Join(ProbeArgs("claude-haiku-4-5"), " ")
	for _, want := range []string{"-p ", "--model claude-haiku-4-5", "--max-turns 1", "--no-session-persistence", "--output-format json"} {
		if !strings.Contains(a, want) {
			t.Errorf("%q lacks %q", a, want)
		}
	}
}
