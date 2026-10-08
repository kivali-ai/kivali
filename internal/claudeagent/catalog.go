// Package claudeagent (catalog.go) — the one place a Claude model is declared.
//
// This package is the Claude driver, and its Provider answers every
// question the core asks about a model from this table: whether it can
// be selected, what it costs, how much context it holds, its efforts.
//
// It is one table so the picker list, the pricing and the context
// window cannot disagree: separate tables would let a model be offered
// in the picker and priced at $0 (Cost returns 0 for an unknown id, so
// the spend would vanish from the rollup), or offered with no context
// window and sized at the 200k fallback, with nothing failing. Adding a
// model means adding one row, and catalog_test.go asserts every entry
// is complete.
//
// ORDER IS THE UI. Entries run cheapest → most expensive, which is the
// order operators see in the picker and agents see in the `subagent`
// tool's model enum. That ordering is the only signal either of them
// gets about relative cost, so keep it true when adding a row.
//
// ONE SELECTABLE ROW PER LINEAGE. A pin names a lineage as much as a
// model: an operator who picked Opus 4.8 wanted the Opus tier, not that
// snapshot of it. So the picker offers the newest model of each family
// — Haiku, Sonnet, Opus, Fable — and nothing older, and a pin on a row
// that has been retired moves to the family's current row at the next
// boot or restore (Provider.Current, applied by store.UpgradeModelPins;
// Config.ResolveModels does the same for AGENT_MODEL). It never crosses
// families: Opus 4.8 becomes Opus 5.5, not Fable, which is a different
// tier at twice the price. catalog_test.go asserts the one-per-lineage
// rule so that shipping a successor without retiring its predecessor
// fails loudly instead of leaving two Opus rows in the picker, one of
// which silently runs as the other.
//
// ADDING A NEW VERSION OF A LINEAGE: add the row with SelectAs set, and
// clear SelectAs on its predecessor. Every agent pinned to the
// predecessor is on the new row after the next restart. The pod image
// must ship a CLI that knows the new id — the CLI validates --model
// against a list compiled into its binary — so bump CLAUDE_CODE_VERSION
// in the Dockerfile in the same change.
//
// ADDING A NEW LINEAGE: one entry with SelectAs set. Leave SelectAs
// empty to price a model nobody may newly choose.
//
// RETIRING A MODEL: clear SelectAs, keep the row. Deleting it would
// silently re-value every historical usage row on that model at $0 and
// under-report the fleet's past spend — usage.jsonl stores the model id
// per turn and is re-priced on read. A row may only be deleted once no
// stored usage record can still name it.

package claudeagent

// catalogRow is everything Kivali knows about one model.
type catalogRow struct {
	// ID is the bare model id, and the key everything else resolves
	// to. Pricing and context lookups normalize through catalogKey
	// before matching, so this carries neither a [1m] suffix nor a
	// dated snapshot suffix ("-YYYYMMDD"). Keying a row on a snapshot
	// id would recreate, from this side, exactly the mismatch
	// catalogKey exists to close; catalog_test.go asserts it.
	//
	// The shape "claude-<family>-<major>[-<minor>]" is load-bearing:
	// Current parses the family and version out of it to find a
	// retired pin's successor. An id that does not fit is priced and
	// sized like any other but never upgraded from or to.
	ID string

	// SelectAs is the exact string offered to operators in the picker
	// and to agents in the subagent tool's enum — which is not always
	// ID. Sonnet 4.6 is natively 200k, so while it was on offer it was
	// offered ONLY as "claude-sonnet-4-6[1m]": a bare 4.6 sitting among
	// 1M-native peers would have been a silent 5x context downgrade for
	// whoever picked it. Every model on offer today is natively 1M or
	// has no 1M form at all, so SelectAs currently equals ID
	// everywhere; the field stays because the next 200k-native model
	// will need it again.
	//
	// Empty means retired: still priced, no longer selectable, and a
	// pin on it moves to the family's current row at boot.
	SelectAs string

	// NativeContext is the model's own window, before any [1m] suffix
	// asks for more. Zero means unknown and resolves to the 200k
	// fallback — the safe direction to be wrong, since under-reporting
	// capacity makes the context-fill ring warn early rather than read
	// "calm" while the session is actually compacting.
	NativeContext int

	// Pricing is the published per-MTok rate. See pricing.go for what
	// the four fields mean and which cache TTL they assume.
	Pricing modelPricing
}

// Selectable reports whether this model may be newly chosen.
func (m catalogRow) Selectable() bool { return m.SelectAs != "" }

// catalog is every model Kivali can price, cheapest first. See the
// package comment above before editing.
var catalog = []catalogRow{
	{
		ID:            "claude-haiku-4-5",
		SelectAs:      "claude-haiku-4-5",
		NativeContext: 200_000,
		Pricing: modelPricing{
			InputPerMTok:      1.00,
			OutputPerMTok:     5.00,
			CacheWritePerMTok: 1.25,
			CacheReadPerMTok:  0.10,
		},
	},
	{
		// Sonnet 5 is $2/$10 at the standard rate, below Sonnet
		// 4.6's $3/$15.
		ID:            "claude-sonnet-5",
		SelectAs:      "claude-sonnet-5",
		NativeContext: 1_000_000,
		Pricing: modelPricing{
			InputPerMTok:      2.00,
			OutputPerMTok:     10.00,
			CacheWritePerMTok: 2.50,
			CacheReadPerMTok:  0.20,
		},
	},
	{
		// Opus 5.5 is the first Opus to cost LESS than the one before
		// it: $4/$20 against the $5/$25 every Opus from 4.5 to 5 has
		// charged, and cache reads at $0.20 — 0.05x its input rate,
		// half the 0.1x multiplier the rest of the table follows and
		// the second row after Fable 5.1 to break it. Do not derive
		// that column from InputPerMTok.
		//
		// Two things the picker cannot express. Reasoning cannot be
		// turned off — effort is the only dial, and the API's own
		// default is medium where every earlier model defaulted to
		// high; Kivali pins effort explicitly on every spawn, so the
		// fleet default (high) still applies. And it runs broader
		// safety classifiers than Opus 5, so a turn can end in a
		// refusal where 5 would have answered.
		ID:            "claude-opus-5-5",
		SelectAs:      "claude-opus-5-5",
		NativeContext: 1_000_000,
		Pricing: modelPricing{
			InputPerMTok:      4.00,
			OutputPerMTok:     20.00,
			CacheWritePerMTok: 5.00,
			CacheReadPerMTok:  0.20,
		},
	},
	{
		// Fable is the only lineage here that costs MORE than Opus —
		// 2.5x on input and output against Opus 5.5. It sits last in
		// the picker and last here, so "the biggest one on the list"
		// stops meaning "the one at Opus prices".
		//
		// Three further things the picker cannot express: it requires
		// an org data-retention setting of 30 days (under
		// zero-retention the provider rejects the request outright);
		// reasoning is always on, so an agent pinned to it spends
		// thinking tokens every turn regardless of effort; and it
		// reads cache at $0.25/MTok — 0.025x its input rate, not the
		// 0.1x multiplier most rows follow and a quarter of what Fable
		// 5 charged. On a long-running agent that re-reads a warm
		// prefix every turn that is most of what the model costs, so
		// copying the Fable 5 row wholesale would over-report 5.1
		// spend by up to 4x on the cache-read line.
		ID:            "claude-fable-5-1",
		SelectAs:      "claude-fable-5-1",
		NativeContext: 1_000_000,
		Pricing: modelPricing{
			InputPerMTok:      10.00,
			OutputPerMTok:     50.00,
			CacheWritePerMTok: 12.50,
			CacheReadPerMTok:  0.25,
		},
	},

	// --- Retired: priced, not selectable -------------------------------
	// These outlive the picker on purpose. See the package comment. A
	// pin on any of them moves to its family's current row at boot.
	{
		// The last 200k-native model Kivali offered, and therefore the
		// only row whose SelectAs ever differed from its ID (it was
		// offered as "claude-sonnet-4-6[1m]"). A pin still carrying the
		// suffix is honored until boot moves it to Sonnet 5, which is
		// natively 1M and needs no suffix.
		ID:            "claude-sonnet-4-6",
		NativeContext: 200_000,
		Pricing: modelPricing{
			InputPerMTok:      3.00,
			OutputPerMTok:     15.00,
			CacheWritePerMTok: 3.75,
			CacheReadPerMTok:  0.30,
		},
	},
	{
		ID:            "claude-opus-4-8",
		NativeContext: 1_000_000,
		Pricing: modelPricing{
			InputPerMTok:      5.00,
			OutputPerMTok:     25.00,
			CacheWritePerMTok: 6.25,
			CacheReadPerMTok:  0.50,
		},
	},
	{
		ID:            "claude-opus-5",
		NativeContext: 1_000_000,
		Pricing: modelPricing{
			InputPerMTok:      5.00,
			OutputPerMTok:     25.00,
			CacheWritePerMTok: 6.25,
			CacheReadPerMTok:  0.50,
		},
	},
	{
		// Same list price as 5.1 on every axis but cache reads, which
		// it charges at 4x. There was never a cost reason to pin a new
		// agent to 5 once 5.1 shipped, so it left the picker with
		// nothing lost.
		ID:            "claude-fable-5",
		NativeContext: 1_000_000,
		Pricing: modelPricing{
			InputPerMTok:      10.00,
			OutputPerMTok:     50.00,
			CacheWritePerMTok: 12.50,
			CacheReadPerMTok:  1.00,
		},
	},
	{
		ID:            "claude-opus-4-7",
		NativeContext: 1_000_000,
		Pricing: modelPricing{
			InputPerMTok:      5.00,
			OutputPerMTok:     25.00,
			CacheWritePerMTok: 6.25,
			CacheReadPerMTok:  0.50,
		},
	},
	{
		// No native-window entry existed for 4.6 before the merge, so
		// it resolved to the 200k fallback. Recorded explicitly now
		// rather than left to the fallback, since a retired model's
		// stored turns still get their context ring sized on read.
		ID:            "claude-opus-4-6",
		NativeContext: 200_000,
		Pricing: modelPricing{
			InputPerMTok:      5.00,
			OutputPerMTok:     25.00,
			CacheWritePerMTok: 6.25,
			CacheReadPerMTok:  0.50,
		},
	},
}

// selectableModels are the ids an operator may pick per agent and an
// agent may name per subagent task, cheapest first.
//
// Derived, not maintained. The picker and the `subagent` tool schema
// both read this, so neither can offer something the catalog does not
// price.
var selectableModels = selectableFromCatalog()

// SelectableModelIDs are the model ids Kivali runs the CLI with, cheapest
// first: on a provider where --model names a deployment, the names its
// deployments need.
func SelectableModelIDs() []string { return append([]string(nil), selectableModels...) }

func selectableFromCatalog() []string {
	out := make([]string, 0, len(catalog))
	for _, m := range catalog {
		if m.Selectable() {
			out = append(out, m.SelectAs)
		}
	}
	return out
}

// lookupModel returns the catalog entry for a model id, stripping any
// [1m] suffix and dated snapshot suffix first. Reports false for
// anything unknown.
func lookupModel(model string) (catalogRow, bool) {
	base := catalogKey(model)
	for _, m := range catalog {
		if m.ID == base {
			return m, true
		}
	}
	return catalogRow{}, false
}
