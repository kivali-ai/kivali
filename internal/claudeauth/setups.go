package claudeauth

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// A Setup is a way to sign the CLI in that it has no interactive
// sign-in for: a provider configured purely by the env block in the
// CLI's settings file. Kivali collects its values and writes that
// block (ApplySetup). Everything about a setup is here, behind its id:
// which values it takes and how they are checked, the env variables
// they become, what of them may be shown again, and how a model the
// provider lacks is described. Callers pass ids and values through and
// never name a setup's variables.
type setup struct {
	// apiProvider is the "apiProvider" `claude auth status` reports
	// once the setup is in force.
	apiProvider string
	// secrets are the value keys never read back or shown.
	secrets []string
	// validate checks the values (only those the chosen options use).
	validate func(v map[string]string) error
	// env is the settings env block the values become.
	env func(v map[string]string) map[string]string
	// read finds the setup in force in env, with its values minus
	// secrets.
	read func(env map[string]string) (map[string]string, bool)
	// detail is what names the setup's target in the billing line
	// ("Microsoft Foundry · <detail>").
	detail func(v map[string]string) string
	// missing words a model the provider has no deployment for.
	missing func(v map[string]string, model string) string
}

// SetupMicrosoftFoundry is Microsoft Foundry: the resource, and its API
// key or an Azure service principal (Entra ID, through the Azure SDK's
// default credential chain). Kivali runs the CLI with catalog model ids
// for --model, which on Foundry name deployments, so the resource needs
// a deployment named after each.
const SetupMicrosoftFoundry = "microsoft-foundry"

// resourceName is an Azure resource's name: letters, digits and
// hyphens, starting and ending with a letter or digit. A URL or a host
// name is not one; the CLI refuses those for ANTHROPIC_FOUNDRY_RESOURCE.
var resourceName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,62}[A-Za-z0-9])?$`)

var setups = map[string]setup{
	SetupMicrosoftFoundry: {
		apiProvider: "foundry",
		secrets:     []string{"api_key", "client_secret"},
		validate: func(v map[string]string) error {
			switch {
			case v["resource"] == "":
				return errors.New("enter the Foundry resource name")
			case !resourceName.MatchString(v["resource"]):
				return errors.New("enter the resource name only, like my-resource, not a URL")
			}
			switch v["auth"] {
			case "api_key":
				if v["api_key"] == "" {
					return errors.New("enter the API key")
				}
			case "service_principal":
				if v["tenant_id"] == "" || v["client_id"] == "" || v["client_secret"] == "" {
					return errors.New("a service principal needs its tenant ID, client ID and client secret")
				}
			default:
				return errors.New("choose an API key or a service principal")
			}
			return nil
		},
		env: func(v map[string]string) map[string]string {
			env := map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1", "ANTHROPIC_FOUNDRY_RESOURCE": v["resource"]}
			if v["auth"] == "api_key" {
				env["ANTHROPIC_FOUNDRY_API_KEY"] = v["api_key"]
			} else {
				env["AZURE_TENANT_ID"] = v["tenant_id"]
				env["AZURE_CLIENT_ID"] = v["client_id"]
				env["AZURE_CLIENT_SECRET"] = v["client_secret"]
			}
			return env
		},
		read: func(env map[string]string) (map[string]string, bool) {
			if s := env["CLAUDE_CODE_USE_FOUNDRY"]; s == "" || s == "0" || strings.EqualFold(s, "false") {
				return nil, false
			}
			v := map[string]string{"resource": env["ANTHROPIC_FOUNDRY_RESOURCE"]}
			if v["resource"] == "" {
				// https://<resource>.services.ai.azure.com/anthropic
				host := strings.SplitN(strings.TrimPrefix(env["ANTHROPIC_FOUNDRY_BASE_URL"], "https://"), "/", 2)[0]
				if name, ok := strings.CutSuffix(host, ".services.ai.azure.com"); ok && resourceName.MatchString(name) {
					v["resource"] = name
				}
			}
			switch {
			case env["ANTHROPIC_FOUNDRY_API_KEY"] != "":
				v["auth"] = "api_key"
			case env["AZURE_CLIENT_SECRET"] != "":
				v["auth"] = "service_principal"
				v["tenant_id"] = env["AZURE_TENANT_ID"]
				v["client_id"] = env["AZURE_CLIENT_ID"]
			}
			for k, s := range v {
				if s == "" {
					delete(v, k)
				}
			}
			return v, true
		},
		detail: func(v map[string]string) string { return v["resource"] },
		missing: func(v map[string]string, model string) string {
			return fmt.Sprintf("No deployment named %s in %s", model, v["resource"])
		},
	},
}

// ValidSetup reports whether id names a setup.
func ValidSetup(id string) bool { _, ok := setups[id]; return ok }

// cleanValues trims every value and drops the empty ones.
func cleanValues(values map[string]string) map[string]string {
	v := map[string]string{}
	for k, s := range values {
		if s = strings.TrimSpace(s); s != "" {
			v[k] = s
		}
	}
	return v
}

// CheckSetup validates values for setup id: the sentence for the person
// when they are wrong. No value may hold whitespace inside.
func CheckSetup(id string, values map[string]string) error {
	s, ok := setups[id]
	if !ok {
		return fmt.Errorf("unknown sign-in setup %q", id)
	}
	v := cleanValues(values)
	for _, x := range v {
		if strings.ContainsAny(x, " \t\r\n") {
			return errors.New("a value has a space in it")
		}
	}
	return s.validate(v)
}

// ApplySetup returns settings (a settings.json's bytes; empty for none)
// with every provider variable replaced by setup id's env block for
// values. Every other key, and every other env variable, is kept.
func ApplySetup(settings []byte, id string, values map[string]string) ([]byte, error) {
	if err := CheckSetup(id, values); err != nil {
		return nil, err
	}
	f, err := readSettings(settings)
	if err != nil {
		return nil, err
	}
	f.clear()
	for k, x := range setups[id].env(cleanValues(values)) {
		f.env[k] = x
	}
	return f.bytes()
}

// ReadSetup finds the setup in force in settings: its id and its values
// without secrets. ok is false when none is.
func ReadSetup(settings []byte) (id string, values map[string]string, ok bool) {
	f, err := readSettings(settings)
	if err != nil {
		return "", nil, false
	}
	env := f.strings()
	for id, s := range setups {
		if v, ok := s.read(env); ok {
			for _, k := range s.secrets {
				delete(v, k)
			}
			return id, v, true
		}
	}
	return "", nil, false
}

// SetupForProvider is the setup a status's APIProvider means, if any:
// the one whose settings name its target in the billing line.
func SetupForProvider(apiProvider string) (string, bool) {
	for id, s := range setups {
		if s.apiProvider == apiProvider {
			return id, true
		}
	}
	return "", false
}

// SetupDetail names the target of setup id's saved values for the
// billing line ("my-resource"); empty when there is none.
func SetupDetail(id string, values map[string]string) string {
	s, ok := setups[id]
	if !ok || s.detail == nil {
		return ""
	}
	return s.detail(values)
}

// MissingModel words model missing from setup id's provider, for the
// person ("No deployment named claude-sonnet-5 in my-resource").
func MissingModel(id string, values map[string]string, model string) string {
	s, ok := setups[id]
	if !ok || s.missing == nil {
		return model + " is not available"
	}
	return s.missing(cleanValues(values), model)
}
