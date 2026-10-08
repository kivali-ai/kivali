package claudeagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeauth"
	"github.com/kivali-ai/kivali/internal/provider"
)

// The Driver is the Claude provider as well as the Claude client: the
// core asks it what a model is called, what it costs, which efforts it
// takes and how the deployment is signed in, and every answer comes
// from catalog.go and this file.
var (
	_ provider.Client      = (*Driver)(nil)
	_ provider.Provider    = (*Driver)(nil)
	_ provider.Credentials = claudeCredentials{}
)

// ProviderName is the Claude provider's name: the prefix a stored id
// may carry ("claude:claude-opus-5-5") and the label on the API.
const ProviderName = "claude"

// Effort levels map 1:1 to the Claude Code CLI's `--effort` flag and
// control how much extended reasoning the model does before answering.
// Ordered low → high. Every Claude model offers all five; stored agent
// and subagent efforts hold these ids, so they never change spelling.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"

	// DefaultEffort is the reasoning level applied when a request leaves
	// Effort empty. High is the deliberate fleet default — agents do
	// real multi-step work where the extra reasoning pays off; callers
	// dial down (medium/low) for cheap mechanical tasks.
	DefaultEffort = EffortHigh
)

// The Claude provider's defaults (Provider.Defaults).
const (
	// DefaultAgentModel is the model an agent runs on when neither its
	// own pin nor AGENT_MODEL says otherwise, and the pin every new
	// hire is created with. Opus by choice: agents do open-ended
	// multi-step work where the tier's judgment pays for itself; the
	// cheaper tiers are for the bounded tasks agents delegate
	// (DefaultSubagentModel) and the summaries Kivali runs on its own
	// behalf (DefaultSummaryModel).
	//
	// Names the family's current row, and catalog_test.go holds it to
	// that: when a newer Opus lands this moves with it, in the same
	// change that retires the old row.
	DefaultAgentModel = "claude-opus-5-5"

	// DefaultSubagentModel is the model a delegated task runs on when
	// the dispatching agent leaves `model` unset. Sonnet by choice:
	// delegated work is bounded and parallel, so the cheaper tier
	// covers most of it and the agent names a heavier model only when
	// a task needs one. Resolved in core at dispatch; the pod keeps
	// its own fallback on the same value for a spec that arrives with
	// an empty model from an older core.
	DefaultSubagentModel = "claude-sonnet-5"

	// DefaultSummaryModel runs the summaries Kivali makes on its own
	// behalf (inbox lines, file summaries).
	DefaultSummaryModel = "claude-haiku-4-5"
)

// effortLevels is the canonical ordered set, with labels.
var effortLevels = []provider.Effort{
	{ID: EffortLow, Label: "Low"},
	{ID: EffortMedium, Label: "Medium"},
	{ID: EffortHigh, Label: "High", Default: true},
	{ID: EffortXHigh, Label: "Extra high"},
	{ID: EffortMax, Label: "Max"},
}

// validEffort reports whether s is one of the recognized effort levels.
func validEffort(s string) bool {
	for _, e := range effortLevels {
		if e.ID == s {
			return true
		}
	}
	return false
}

// normalizeEffort returns a valid effort level for the CLI: the input
// when recognized, else DefaultEffort. Empty and unknown values both
// fall back to the default so a stale or typo'd setting can never reach
// the CLI as an invalid flag (which would fail the spawn).
func normalizeEffort(s string) string {
	if validEffort(s) {
		return s
	}
	return DefaultEffort
}

// efforts is a fresh copy of effortLevels for a ModelInfo.
func efforts() []provider.Effort {
	return append([]provider.Effort(nil), effortLevels...)
}

// stripName removes the "claude:" prefix a stored id may carry.
func stripName(id string) string {
	return strings.TrimPrefix(id, ProviderName+":")
}

// Name is "claude".
func (c *Driver) Name() string { return ProviderName }

// Models are the catalog's selectable rows, cheapest first.
func (c *Driver) Models() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(selectableModels))
	for _, id := range selectableModels {
		m, _ := c.Resolve(id)
		out = append(out, m)
	}
	return out
}

// Resolve finds the catalog row for any id a pin, a usage row or the
// CLI may carry: a "claude:" prefix, a [1m] suffix and a dated snapshot
// suffix are all read through. The answer's ID is the id a person
// picks for that row (the row's own id once it is retired), its Label
// and ContextWindow are for the id as asked, so a [1m] pin reads
// "Sonnet 4.6 · 1M" and sizes at 1M.
func (c *Driver) Resolve(id string) (provider.ModelInfo, bool) {
	id = stripName(id)
	row, ok := lookupModel(id)
	if !ok {
		return provider.ModelInfo{}, false
	}
	label := friendlyModelName(catalogKey(id))
	if wants1M(id) {
		label += " · 1M"
	}
	family, _, _ := lineage(row.ID)
	info := provider.ModelInfo{
		ID:            row.ID,
		Label:         label,
		Lineage:       family,
		Current:       row.Selectable(),
		ContextWindow: contextWindow(id),
		Efforts:       efforts(),
	}
	if row.Selectable() {
		info.ID = row.SelectAs
	}
	return info, true
}

// Current is where a pin on id's lineage runs today; see currentModel.
func (c *Driver) Current(id string) string {
	bare := stripName(id)
	if next := currentModel(bare); next != bare {
		return next
	}
	return id
}

// Price is u at id's published rate; 0 for an id the catalog does not
// know.
func (c *Driver) Price(id string, u provider.TokenUsage) float64 {
	return defaultPricing.Cost(stripName(id), u)
}

// Defaults are Opus for agents, Sonnet for subagents, Haiku for
// summaries.
func (c *Driver) Defaults() provider.Defaults {
	return provider.Defaults{Agent: DefaultAgentModel, Subagent: DefaultSubagentModel, Summary: DefaultSummaryModel}
}

// Effort accepts any of the five levels for any model: the CLI takes
// --effort on every model it runs, including one the catalog does not
// know.
func (c *Driver) Effort(_, id string) (provider.Effort, bool) {
	for _, e := range effortLevels {
		if e.ID == id {
			return e, true
		}
	}
	return provider.DefaultEffort(effortLevels), false
}

// Credentials reports how this deployment's CLI is signed in.
func (c *Driver) Credentials() provider.Credentials {
	return claudeCredentials{binary: c.opts.ClaudeBinary}
}

// HomeDirName is the directory under the data volume the CLI's HOME
// lives in (the image sets HOME=/data/claude-home), and the subPath an
// agent pod mounts it from: whatever the CLI's sign-in writes there signs
// in every agent.
const HomeDirName = "claude-home"

// authEnvVars are credential variables the CLI would honour from the
// process environment. Agent pods receive only the CLI's HOME, not the
// server's environment, so a credential here would bill the server one
// way and the agents another.
var authEnvVars = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// CheckEnvironment refuses a server environment that sets a credential
// variable: the one way to sign the CLI in is its own sign-in (run
// `claude`; `/login` switches), kept under HOME, which every agent pod
// shares.
func CheckEnvironment(getenv func(string) string) error {
	var set []string
	for _, v := range authEnvVars {
		if getenv(v) != "" {
			set = append(set, v)
		}
	}
	if len(set) > 0 {
		return errors.New("[" + strings.Join(set, " ") + "] set; sign the CLI in by running `claude` instead, " +
			"because agent pods share the CLI's sign-in but not the server's environment")
	}
	return nil
}

type claudeCredentials struct{ binary string }

// authStatusTimeout bounds one `claude auth status`.
const authStatusTimeout = 15 * time.Second

// Status asks the CLI, run as this process with its environment and
// HOME (the ones every model call runs with), how it is signed in.
func (c claudeCredentials) Status(ctx context.Context) (provider.CredentialStatus, error) {
	st, err := authStatus(ctx, c.binary)
	if err != nil {
		return provider.CredentialStatus{}, err
	}
	return provider.CredentialStatus{Present: st.LoggedIn, Who: st.Email, Billing: st.Billing()}, nil
}

// authStatus runs `claude auth status --json` with binary, as this
// process with its environment and HOME, and parses what it prints.
func authStatus(ctx context.Context, binary string) (claudeauth.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, authStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, claudeauth.Args()...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// Signed out exits 1 with the same JSON on stdout.
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return claudeauth.Status{}, fmt.Errorf("claude auth status: %w", err)
	}
	st, perr := claudeauth.Parse(out)
	if perr != nil {
		if err != nil {
			return claudeauth.Status{}, fmt.Errorf("claude auth status: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return claudeauth.Status{}, perr
	}
	return st, nil
}

func (claudeCredentials) Guidance() string {
	return "Whoever runs this Kivali server can sign it in to Claude."
}
func (claudeCredentials) HomeDir() string { return HomeDirName }
