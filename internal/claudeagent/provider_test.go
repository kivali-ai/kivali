package claudeagent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestCheckEnvironmentRefusesCredentialVariables: agent pods share the
// CLI's sign-in under HOME, not the server's environment, so any
// credential variable the CLI would honour is refused at boot:
// otherwise the server's own calls would bill one way and the agents
// another.
func TestCheckEnvironmentRefusesCredentialVariables(t *testing.T) {
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{name: "tok"}
			err := CheckEnvironment(func(k string) string { return env[k] })
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("CheckEnvironment = %v, want an error naming %s", err, name)
			}
		})
	}
	if err := CheckEnvironment(func(string) string { return "" }); err != nil {
		t.Errorf("CheckEnvironment with none set = %v, want nil", err)
	}
}

// TestProviderModelsAreTheSelectableRows: Models is the picker, in
// catalog order, every row current, labelled, sized and carrying the
// five effort levels with high the default.
func TestProviderModelsAreTheSelectableRows(t *testing.T) {
	d := New(Options{})
	models := d.Models()
	if got, want := strings.Join(idsOf(models), ","), strings.Join(selectableModels, ","); got != want {
		t.Fatalf("Models = %s, want %s", got, want)
	}
	for _, m := range models {
		if !m.Current || m.Label == "" || m.Lineage == "" || m.ContextWindow == 0 {
			t.Errorf("incomplete row %+v", m)
		}
		if got := strings.Join(provider.EffortIDs(m.Efforts), ","); got != "low,medium,high,xhigh,max" {
			t.Errorf("%s efforts = %s", m.ID, got)
		}
		if provider.DefaultEffort(m.Efforts).ID != DefaultEffort {
			t.Errorf("%s default effort = %+v", m.ID, provider.DefaultEffort(m.Efforts))
		}
	}
}

// TestProviderResolveReadsEveryStoredSpelling: a stored id may carry
// the provider prefix, a [1m] suffix or a dated snapshot, or name a
// retired row; each resolves to the row that prices and labels it.
func TestProviderResolveReadsEveryStoredSpelling(t *testing.T) {
	d := New(Options{})
	cases := []struct {
		in, id, label string
		current       bool
		window        int
	}{
		{"claude-opus-5-5", "claude-opus-5-5", "Opus 5.5", true, 1_000_000},
		{"claude:claude-opus-5-5", "claude-opus-5-5", "Opus 5.5", true, 1_000_000},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5", "Haiku 4.5", true, 200_000},
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6", "Sonnet 4.6 · 1M", false, 1_000_000},
		{"claude-opus-4-8", "claude-opus-4-8", "Opus 4.8", false, 1_000_000},
	}
	for _, tc := range cases {
		m, ok := d.Resolve(tc.in)
		if !ok {
			t.Errorf("Resolve(%q) not found", tc.in)
			continue
		}
		if m.ID != tc.id || m.Label != tc.label || m.Current != tc.current || m.ContextWindow != tc.window {
			t.Errorf("Resolve(%q) = %+v, want id %s label %q current %v window %d", tc.in, m, tc.id, tc.label, tc.current, tc.window)
		}
	}
	for _, unknown := range []string{"", "<synthetic>", "gpt-5", "mock:claude-opus-5-5"} {
		if m, ok := d.Resolve(unknown); ok {
			t.Errorf("Resolve(%q) = %+v, want not found", unknown, m)
		}
	}
}

// TestProviderCurrentPriceEffort covers the rest of the Provider
// surface on the real catalog.
func TestProviderCurrentPriceEffort(t *testing.T) {
	d := New(Options{})
	if got := d.Current("claude-opus-4-8"); got != DefaultAgentModel {
		t.Errorf("Current(opus 4.8) = %q, want %q", got, DefaultAgentModel)
	}
	if got := d.Current("claude:claude-opus-5-5"); got != "claude:claude-opus-5-5" {
		t.Errorf("Current on a current id = %q, want it unchanged", got)
	}
	u := provider.TokenUsage{InputTokens: 1_000_000}
	if got := d.Price("claude-opus-5-5", u); got != 4.00 {
		t.Errorf("Price(opus 5.5, 1M in) = %v, want 4", got)
	}
	if got := d.Price("claude:claude-opus-5-5", u); got != 4.00 {
		t.Errorf("Price with prefix = %v, want 4", got)
	}
	if got := d.Price("<synthetic>", u); got != 0 {
		t.Errorf("Price(unknown) = %v, want 0", got)
	}
	if e, ok := d.Effort("claude-haiku-4-5", "xhigh"); !ok || e.Label != "Extra high" {
		t.Errorf("Effort(xhigh) = %+v, %v", e, ok)
	}
	if e, ok := d.Effort("claude-haiku-4-5", "turbo"); ok || e.ID != DefaultEffort {
		t.Errorf("Effort(turbo) = %+v, %v; want the default and false", e, ok)
	}
	if df := d.Defaults(); df.Agent != DefaultAgentModel || df.Subagent != DefaultSubagentModel || df.Summary != DefaultSummaryModel {
		t.Errorf("Defaults = %+v", df)
	}
}

func idsOf(ms []provider.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
