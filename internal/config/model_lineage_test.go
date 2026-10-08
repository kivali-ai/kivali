package config

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestResolveModelsAlongTheirLineage pins the config half of the
// upgrade rule. A deployment's env is written once and outlives many
// releases, so AGENT_MODEL is usually the name of whatever model was
// current when the cluster was set up. When the provider retires that
// row the value must run as the lineage's current model, exactly as a
// per-agent pin does at boot — and it is resolved here, once, so hire,
// chat, the CoS seed and the picker all read the same id.
func TestResolveModelsAlongTheirLineage(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MODEL", provider.MockModelRetired)
	t.Setenv("SUMMARY_MODEL", provider.MockModelSmall)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ResolveModels(provider.MockProvider{}); err != nil {
		t.Fatalf("ResolveModels: %v", err)
	}
	if cfg.AgentModel != provider.MockModelLarge {
		t.Errorf("AgentModel = %q, want %q (the retired pin's current row)", cfg.AgentModel, provider.MockModelLarge)
	}
	// A model the picker still offers is passed through untouched.
	if cfg.SummaryModel != provider.MockModelSmall {
		t.Errorf("SummaryModel = %q, want %s unchanged", cfg.SummaryModel, provider.MockModelSmall)
	}
}

// TestResolveModelsDefaultsFromTheProvider: with nothing set, the
// models are the provider's own defaults, so no literal in config can
// drift from the driver's catalog.
func TestResolveModelsDefaultsFromTheProvider(t *testing.T) {
	clearEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentModel != "" || cfg.SummaryModel != "" {
		t.Fatalf("Load filled models before the provider is known: %q, %q", cfg.AgentModel, cfg.SummaryModel)
	}
	p := provider.MockProvider{}
	if err := cfg.ResolveModels(p); err != nil {
		t.Fatalf("ResolveModels: %v", err)
	}
	if d := p.Defaults(); cfg.AgentModel != d.Agent || cfg.SummaryModel != d.Summary {
		t.Errorf("models = %q, %q; want the provider's defaults %q, %q", cfg.AgentModel, cfg.SummaryModel, d.Agent, d.Summary)
	}
}

// TestResolveModelsRefusesAnUnknownModel: an env override names a model
// the provider resolves, or the server does not start.
func TestResolveModelsRefusesAnUnknownModel(t *testing.T) {
	clearEnv(t)
	t.Setenv("AGENT_MODEL", "no-such-model")
	cfg, _ := Load()
	err := cfg.ResolveModels(provider.MockProvider{})
	if err == nil || !strings.Contains(err.Error(), "AGENT_MODEL") || !strings.Contains(err.Error(), "no-such-model") {
		t.Fatalf("ResolveModels = %v, want an error naming AGENT_MODEL and the id", err)
	}
}
