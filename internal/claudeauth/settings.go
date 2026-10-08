package claudeauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The CLI's user settings file is $HOME/.claude/settings.json. Its
// "env" object is applied to every run of the CLI, which is how a
// cloud provider is chosen: Amazon Bedrock's and Google Vertex AI's
// setup wizards write their variables there, and each Setup (setups.go)
// writes its own the same way. Kivali's server and every agent pod
// share that HOME.

// ProviderEnvKeys are the settings env variables that choose a cloud
// provider or carry its credential: each provider's CLAUDE_CODE_USE_*
// switch and the region, project, resource and secret variables that
// go with it, covering what the CLI's Bedrock and Vertex wizards write
// and what every Setup writes (a test checks). A sign-in through
// `claude` (/login) does not remove them, and any of them left behind
// outranks it, so a new sign-in starts by clearing them.
var ProviderEnvKeys = []string{
	// The provider switches.
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"CLAUDE_CODE_USE_MANTLE",
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
	"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH",
	// Amazon Bedrock (what its wizard writes).
	"AWS_REGION",
	"AWS_PROFILE",
	"AWS_BEARER_TOKEN_BEDROCK",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"ANTHROPIC_BEDROCK_BASE_URL",
	// Google Vertex AI (what its wizard writes).
	"ANTHROPIC_VERTEX_PROJECT_ID",
	"CLOUD_ML_REGION",
	"GOOGLE_APPLICATION_CREDENTIALS",
	"ANTHROPIC_VERTEX_BASE_URL",
	// Microsoft Foundry, and the Azure service principal its default
	// credential chain reads.
	"ANTHROPIC_FOUNDRY_RESOURCE",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_FOUNDRY_API_KEY",
	"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
	"AZURE_TENANT_ID",
	"AZURE_CLIENT_ID",
	"AZURE_CLIENT_SECRET",
}

// settingsFile is a settings.json read for editing: every key but env
// kept as written.
type settingsFile struct {
	keys map[string]json.RawMessage
	env  map[string]any
}

func readSettings(b []byte) (settingsFile, error) {
	f := settingsFile{keys: map[string]json.RawMessage{}, env: map[string]any{}}
	if strings.TrimSpace(string(b)) == "" {
		return f, nil
	}
	if err := json.Unmarshal(b, &f.keys); err != nil || f.keys == nil {
		return settingsFile{}, errors.New("settings.json is not a JSON object")
	}
	if raw, ok := f.keys["env"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &f.env); err != nil || f.env == nil {
			return settingsFile{}, errors.New("settings.json: env is not an object")
		}
	}
	return f, nil
}

// strings are the env variables that hold strings, trimmed.
func (f settingsFile) strings() map[string]string {
	out := map[string]string{}
	for k, v := range f.env {
		if s, ok := v.(string); ok {
			out[k] = strings.TrimSpace(s)
		}
	}
	return out
}

func (f settingsFile) bytes() ([]byte, error) {
	if len(f.env) == 0 {
		delete(f.keys, "env")
	} else {
		b, err := json.Marshal(f.env)
		if err != nil {
			return nil, err
		}
		f.keys["env"] = b
	}
	out, err := json.MarshalIndent(f.keys, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// clear removes ProviderEnvKeys from env and returns the ones there
// were, sorted.
func (f settingsFile) clear() []string {
	var cleared []string
	for _, k := range ProviderEnvKeys {
		if _, ok := f.env[k]; ok {
			delete(f.env, k)
			cleared = append(cleared, k)
		}
	}
	sort.Strings(cleared)
	return cleared
}

// ClearProvider returns settings without ProviderEnvKeys, and the keys
// it removed; out is nil when there were none.
func ClearProvider(settings []byte) (out []byte, cleared []string, err error) {
	s, err := readSettings(settings)
	if err != nil {
		return nil, nil, err
	}
	cleared = s.clear()
	if len(cleared) == 0 {
		return nil, nil, nil
	}
	out, err = s.bytes()
	return out, cleared, err
}

// The model check: one tiny `claude -p` per model, whose answer says
// whether the provider serves that model under that name.

// ProbeArgs are the arguments, after the CLI binary, of the check for
// model: one prompt, no tools, one turn, nothing saved, JSON out.
// ProbeEnv goes with them.
func ProbeArgs(model string) []string {
	return []string{"-p", "Reply with OK.", "--model", model, "--tools", "", "--max-turns", "1",
		"--no-session-persistence", "--output-format", "json"}
}

// ProbeEnv is the environment a check runs with: one retry, since the
// CLI otherwise retries a refused credential for minutes, and no
// traffic besides the model call.
func ProbeEnv() []string {
	return []string{"CLAUDE_CODE_MAX_RETRIES=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
}

// Model check states.
const (
	ModelOK      = "ok"
	ModelMissing = "missing"
	ModelError   = "error"
)

// missingMarks are what the CLI's answer holds when the provider has no
// model by that name, besides its api_error_status of 404: its words
// for a missing model ("The model … is not available on your …
// deployment", "There's an issue with the selected model (…). It may
// not exist…") and Azure's own error code.
var missingMarks = []string{
	"is not available on your",
	"issue with the selected model",
	"deploymentnotfound",
	"deployment for this resource does not exist",
}

// ClassifyProbe reads a check's output (`--output-format json`'s
// result): ok when the CLI answered without an error, missing when the
// error is a missing model, and otherwise error with a short reason.
func ClassifyProbe(stdout, stderr []byte, code int) (state, detail string) {
	var res struct {
		IsError   *bool  `json:"is_error"`
		Result    string `json:"result"`
		APIStatus int    `json:"api_error_status"`
	}
	parsed := json.Unmarshal(stdout, &res) == nil && res.IsError != nil
	if parsed && code == 0 && !*res.IsError {
		return ModelOK, ""
	}
	text := res.Result
	if !parsed || strings.TrimSpace(text) == "" {
		text = string(stderr)
	}
	lower := strings.ToLower(text)
	if res.APIStatus == 404 || slices.ContainsFunc(missingMarks, func(m string) bool { return strings.Contains(lower, m) }) {
		return ModelMissing, ""
	}
	if res.APIStatus == 401 || res.APIStatus == 403 {
		return ModelError, fmt.Sprintf("the provider refused the sign-in (HTTP %d)", res.APIStatus)
	}
	// The CLI's API errors end "· API Error: <status> <message>"; the
	// message is the part worth showing.
	if _, after, ok := strings.Cut(text, "API Error: "); ok {
		text = after
	}
	return ModelError, firstLine(text)
}

// firstLine is text's first non-empty line, at most 200 bytes.
func firstLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = strings.ToValidUTF8(l[:200], "") + "…"
			}
			return l
		}
	}
	return "no answer"
}
