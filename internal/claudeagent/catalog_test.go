package claudeagent

import (
	"strings"
	"testing"
)

// These are the anti-drift tests. One table only helps if a
// half-filled row fails loudly, so each of these pins one way a row
// could go wrong silently.

// TestEveryModelIsPriced catches the failure that lost money: a model
// in the picker with no pricing row. Cost returns 0 for an unknown id,
// so every turn on it reported $0 and simply vanished from the spend
// rollup.
func TestEveryModelIsPriced(t *testing.T) {
	for _, m := range catalog {
		if m.ID == "" {
			t.Error("catalog entry with an empty ID")
			continue
		}
		p := m.Pricing
		if p.InputPerMTok <= 0 || p.OutputPerMTok <= 0 {
			t.Errorf("%s: input/output rate is zero — every turn on it would be priced at $0", m.ID)
		}
		if p.CacheReadPerMTok <= 0 || p.CacheWritePerMTok <= 0 {
			t.Errorf("%s: cache rates are zero. On a long-running agent that re-reads a warm prefix "+
				"every turn, cache reads are most of what the model costs", m.ID)
		}
	}
}

// TestSelectableModelsResolveBackToTheirEntry guards the one place
// SelectAs may differ from ID. Pricing and context lookups strip the
// [1m] suffix and match on ID, so a SelectAs that does not resolve back
// is a model an operator can pick and nothing can price.
func TestSelectableModelsResolveBackToTheirEntry(t *testing.T) {
	for _, m := range catalog {
		if !m.Selectable() {
			continue
		}
		got, ok := lookupModel(m.SelectAs)
		if !ok {
			t.Errorf("%s is selectable as %q, which resolves to no catalog entry", m.ID, m.SelectAs)
			continue
		}
		if got.ID != m.ID {
			t.Errorf("%s is selectable as %q, which resolves to %s", m.ID, m.SelectAs, got.ID)
		}
		if _, priced := defaultPricing[m.ID]; !priced {
			t.Errorf("%s is selectable but absent from the derived pricing table", m.ID)
		}
	}
}

// TestSelectableModelsAreOrderedCheapestFirst pins the ordering,
// because the ordering IS the cost signal. It is the only thing the
// picker and the subagent tool's enum tell anyone about relative price,
// and a row appended in the wrong place quietly inverts that advice.
func TestSelectableModelsAreOrderedCheapestFirst(t *testing.T) {
	var prev catalogRow
	var havePrev bool
	for _, m := range catalog {
		if !m.Selectable() {
			continue
		}
		if havePrev && m.Pricing.InputPerMTok < prev.Pricing.InputPerMTok {
			t.Errorf("%s ($%.2f) is listed after %s ($%.2f): selectable models run cheapest first",
				m.ID, m.Pricing.InputPerMTok, prev.ID, prev.Pricing.InputPerMTok)
		}
		prev, havePrev = m, true
	}
}

// TestRetiredModelsKeepTheirPricing states the invariant that makes
// retirement safe. usage.jsonl stores a model id per turn and is
// re-priced on read, so deleting a row re-values every historical turn
// on that model at $0.
func TestRetiredModelsKeepTheirPricing(t *testing.T) {
	var retired int
	for _, m := range catalog {
		if m.Selectable() {
			continue
		}
		retired++
		if _, ok := defaultPricing[m.ID]; !ok {
			t.Errorf("retired model %s has no pricing: its historical usage rows would re-price to $0", m.ID)
		}
	}
	if retired == 0 {
		t.Skip("no retired models in the catalog yet")
	}
}

// TestCatalogHasNoDuplicateIDs guards the quiet one. Two rows with the
// same ID build a map where the later silently wins, so the rates a
// reader sees first are not the rates in force.
func TestCatalogHasNoDuplicateIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range catalog {
		if seen[m.ID] {
			t.Errorf("duplicate catalog entry for %s — one of them is dead code and it is not obvious which", m.ID)
		}
		seen[m.ID] = true
	}
	if len(defaultPricing) != len(catalog) {
		t.Errorf("pricing table has %d rows for %d catalog entries", len(defaultPricing), len(catalog))
	}
}

// TestCatalogIDsAreNotDatedSnapshots guards the mismatch from the
// other side. Lookups normalize a reported id through catalogKey, which
// strips "-YYYYMMDD"; a catalog row keyed on a snapshot id would then be
// unreachable — the normalized "claude-haiku-4-5" would never match a
// "claude-haiku-4-5-20251001" key, and every turn on that model would go
// unpriced exactly as before. catalog rows are keyed on bare ids.
func TestCatalogIDsAreNotDatedSnapshots(t *testing.T) {
	for _, m := range catalog {
		if key := catalogKey(m.ID); key != m.ID {
			t.Errorf("catalog id %q carries a suffix lookups strip (normalizes to %q): "+
				"the row would be unreachable and its turns would price at $0", m.ID, key)
		}
	}
}

// TestSelectableModelsMatchesCatalog pins the derivation itself, since
// selectableModels is what the picker and the subagent schema both
// read.
func TestSelectableModelsMatchesCatalog(t *testing.T) {
	var want []string
	for _, m := range catalog {
		if m.Selectable() {
			want = append(want, m.SelectAs)
		}
	}
	if strings.Join(selectableModels, ",") != strings.Join(want, ",") {
		t.Errorf("selectableModels = %v, catalog says %v", selectableModels, want)
	}
	if len(selectableModels) == 0 {
		t.Fatal("nothing is selectable — the picker and the subagent tool would both be empty")
	}
}

// TestContextWindowsComeFromTheCatalog checks the third table that used
// to be maintained by hand. A missing window is not an error — it falls
// back to 200k — but it must be the catalog's answer, not a stale map's.
func TestContextWindowsComeFromTheCatalog(t *testing.T) {
	for _, m := range catalog {
		if m.NativeContext == 0 {
			continue
		}
		if got := contextWindow(m.ID); got != m.NativeContext {
			t.Errorf("%s: contextWindow = %d, catalog says %d", m.ID, got, m.NativeContext)
		}
	}
	// And the suffix still wins over a smaller native window, which is
	// the whole reason Sonnet 4.6 is offered only in its [1m] form.
	if got := contextWindow("claude-sonnet-4-6" + contextSuffix1M); got != 1_000_000 {
		t.Errorf("suffixed sonnet-4-6 sized at %d, want 1M", got)
	}
}

// TestDefaultSubagentModelIsSelectable pins the subagent default to
// the catalog. Core resolves an omitted per-task model to this id and
// stamps it on the job, the pod spec, meta.json and every UI chip — so
// a retired default would run every unlabelled task on a model the
// picker does not offer and nothing would say so.
func TestDefaultSubagentModelIsSelectable(t *testing.T) {
	for _, m := range selectableModels {
		if m == DefaultSubagentModel {
			return
		}
	}
	t.Errorf("DefaultSubagentModel %q is not among the selectable models %v", DefaultSubagentModel, selectableModels)
}

// TestDefaultAgentModelIsSelectable pins the fleet default the same
// way. Every hire is pinned to this id verbatim and config resolves
// AGENT_MODEL through Current, so a retired default here would be one
// the boot pass immediately rewrites on every new agent — a sign the
// constant was forgotten when its lineage moved on.
func TestDefaultAgentModelIsSelectable(t *testing.T) {
	for _, m := range selectableModels {
		if m == DefaultAgentModel {
			return
		}
	}
	t.Errorf("DefaultAgentModel %q is not among the selectable models %v", DefaultAgentModel, selectableModels)
}

// TestOneSelectableModelPerLineage pins the rule Current depends on.
// A pin moves to "the newest selectable row in its family", which is
// only a sensible destination when there is exactly one. Two
// selectable Opus rows would mean a picker offering a model that boot
// then silently swaps for its sibling — so shipping a successor
// without retiring its predecessor fails here, in the same change.
func TestOneSelectableModelPerLineage(t *testing.T) {
	seen := map[string]string{}
	for _, m := range catalog {
		if !m.Selectable() {
			continue
		}
		family, _, ok := lineage(m.ID)
		if !ok {
			t.Errorf("%s is selectable but has no parseable lineage: Current could never move a pin off it", m.ID)
			continue
		}
		if prev, dup := seen[family]; dup {
			t.Errorf("%s and %s are both selectable: retire the older one, its pins move to the newer at boot", prev, m.ID)
		}
		seen[family] = m.ID
	}
}
