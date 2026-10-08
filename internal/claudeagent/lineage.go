// Package claudeagent (lineage.go) — which model a pin should run on today.

package claudeagent

import (
	"strconv"
	"strings"
)

// currentModel returns the model a pin should run on today; the Driver's
// Provider.Current answers with it.
//
// A pin names a lineage as much as a model: an operator who picked
// "claude-opus-4-8" wanted the Opus tier, and when Kivali adds Opus 5.5
// and retires 4.8 from the picker, that agent belongs on 5.5 — not left
// on a model nobody can newly choose, and not moved to Fable, which is
// a different tier at a different price. So a pin whose catalog row has
// been retired resolves to the newest selectable row in the same
// family, and only in the same family.
//
// Returned unchanged: a pin that is still selectable (including one
// carrying a [1m] suffix or a dated snapshot the operator chose on
// purpose), a model the catalog does not know at all (a sentinel, a CLI
// alias, another provider's id), and a retired model whose family has
// nothing newer on offer. Those would be guesses, not upgrades, and the
// picker's retired-model option already reports them honestly.
//
// Applied through Provider.Current in three places, all at the edge
// where a pin enters the system rather than on every read:
// store.UpgradeModelPins rewrites each active agent's agent.yaml at boot
// (main.go) and after a restore (the web restore handler), and
// Config.ResolveModels resolves AGENT_MODEL / SUMMARY_MODEL once. Runtime
// readers (the runner's --model, the context ring, the chips) see the
// resolved value and never call this.
func currentModel(model string) string {
	info, ok := lookupModel(model)
	if !ok || info.Selectable() {
		return model
	}
	family, version, ok := lineage(info.ID)
	if !ok {
		return model
	}
	var (
		best        catalogRow
		bestVersion []int
		found       bool
	)
	for _, m := range catalog {
		if !m.Selectable() {
			continue
		}
		f, v, ok := lineage(m.ID)
		if !ok || f != family {
			continue
		}
		if !found || newerVersion(v, bestVersion) {
			best, bestVersion, found = m, v, true
		}
	}
	// Never a downgrade: a retired row that is newer than everything
	// its family still offers stays where it is.
	if !found || !newerVersion(bestVersion, version) {
		return model
	}
	return best.SelectAs
}

// lineage splits a bare catalog id into family and version:
// "claude-opus-4-8" → ("opus", [4 8]), "claude-opus-5" → ("opus", [5]).
// It reads the same "claude-<family>-<major>[-<minor>…]" shape
// friendlyModelName renders from; anything else reports !ok rather
// than a partial answer, so an off-pattern id is never upgraded from
// or to.
func lineage(id string) (family string, version []int, ok bool) {
	rest, ok := strings.CutPrefix(id, "claude-")
	if !ok {
		return "", nil, false
	}
	parts := strings.Split(rest, "-")
	if len(parts) < 2 || parts[0] == "" {
		return "", nil, false
	}
	for _, p := range parts[1:] {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return "", nil, false
		}
		version = append(version, n)
	}
	return parts[0], version, true
}

// newerVersion reports whether a is strictly newer than b, part by
// part, with a missing part reading as 0 — so 5 < 5.5 and 4.8 < 5.
func newerVersion(a, b []int) bool {
	for i := range max(len(a), len(b)) {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}
