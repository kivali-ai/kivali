package store

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// TestUpgradeModelPins covers the boot pass that moves a pin along its
// lineage. The resolver is a stub so the test states the contract of
// the pass itself — which agents it touches, what it leaves alone, what
// it reports — independently of which models the catalog happens to
// retire this month.
func TestUpgradeModelPins(t *testing.T) {
	s := mustStore(t)
	for _, a := range []Agent{
		{Slug: "old", Role: "r", Model: "family-old"},
		{Slug: "current", Role: "r", Model: "family-new"},
		{Slug: "inherit", Role: "r"}, // "" = fleet default; config resolves that
		{Slug: "foreign", Role: "r", Model: "other-vendor"},
		{Slug: "gone", Role: "r", Model: "family-old"},
	} {
		if err := s.CreateAgent(a, "k"); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}
	// An archived agent on the old pin: not running, so not rewritten.
	if err := s.ArchiveAgent("gone"); err != nil {
		t.Fatalf("archive: %v", err)
	}

	current := func(m string) string {
		if m == "family-old" {
			return "family-new"
		}
		return m
	}
	moved, err := s.UpgradeModelPins(current)
	if err != nil {
		t.Fatalf("UpgradeModelPins: %v", err)
	}
	if len(moved) != 1 || moved[0] != (ModelPinUpgrade{Slug: "old", From: "family-old", To: "family-new"}) {
		t.Errorf("moved = %+v, want exactly old: family-old → family-new", moved)
	}
	want := map[string]string{
		"old":     "family-new",
		"current": "family-new",
		"inherit": "",
		"foreign": "other-vendor",
	}
	for slug, model := range want {
		a, err := s.GetAgent(slug)
		if err != nil {
			t.Fatalf("GetAgent %s: %v", slug, err)
		}
		if a.Model != model {
			t.Errorf("%s: Model = %q, want %q", slug, a.Model, model)
		}
		// The rewrite goes through SetAgentModel, which preserves the
		// rest of agent.yaml; a pass that rebuilt the record from
		// scratch would drop Role, ReportsTo, CreatedAt.
		if a.Role != "r" {
			t.Errorf("%s: Role = %q after the pass, want r", slug, a.Role)
		}
	}
	archived, err := s.ListArchivedAgents()
	if err != nil {
		t.Fatalf("ListArchivedAgents: %v", err)
	}
	if len(archived) != 1 || archived[0].Model != "family-old" {
		t.Errorf("archived agent = %+v, want its family-old pin untouched", archived)
	}

	// Idempotent: the pass runs on every boot, so the second run must
	// find nothing to do.
	moved, err = s.UpgradeModelPins(current)
	if err != nil {
		t.Fatalf("second UpgradeModelPins: %v", err)
	}
	if len(moved) != 0 {
		t.Errorf("second run moved %+v, want nothing", moved)
	}
}

// TestUpgradeModelPinsWithAProviderResolver is the end-to-end shape of
// the boot pass against a provider's Current: an agent hired on a model
// the provider has since retired boots on the lineage's current model,
// and an agent already on it is untouched. The mock provider stands in
// for the real one; the Claude catalog's own lineage rules are tested in
// internal/claudeagent.
func TestUpgradeModelPinsWithAProviderResolver(t *testing.T) {
	p := provider.MockProvider{}
	retired, current := provider.MockModelRetired, p.Current(provider.MockModelRetired)
	if current == retired {
		t.Fatalf("Current(%q) = %q; expected the pass to have somewhere to move it", retired, current)
	}

	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "hired-on-old", Role: "r", Model: retired}, "k"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := s.CreateAgent(Agent{Slug: "hired-today", Role: "r", Model: current}, "k"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	moved, err := s.UpgradeModelPins(p.Current)
	if err != nil {
		t.Fatalf("UpgradeModelPins: %v", err)
	}
	if len(moved) != 1 || moved[0].Slug != "hired-on-old" || moved[0].To != current {
		t.Errorf("moved = %+v, want hired-on-old → %s", moved, current)
	}
	a, err := s.GetAgent("hired-on-old")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if a.Model != current {
		t.Errorf("hired-on-old runs on %q after boot, want %q", a.Model, current)
	}
}
