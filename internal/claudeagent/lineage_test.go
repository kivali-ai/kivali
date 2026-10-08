package claudeagent

import "testing"

// TestCurrentFollowsTheLineage pins the user-facing rule: a pin on a
// model the picker has retired runs on the newest model of the same
// family, and only the same family. Opus 4.8 became Opus 5.5 when 5.5
// landed; it did not become Fable, which is a different tier at a
// different price.
func TestCurrentFollowsTheLineage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Retired → the family's current row.
		{"claude-opus-4-8", "claude-opus-5-5"},
		{"claude-opus-4-7", "claude-opus-5-5"},
		{"claude-opus-4-6", "claude-opus-5-5"},
		{"claude-opus-5", "claude-opus-5-5"},
		{"claude-fable-5", "claude-fable-5-1"},
		// The suffix goes with the retired row. Sonnet 5 is natively
		// 1M and its SelectAs carries no suffix, so the upgraded pin
		// must not either — "claude-sonnet-5[1m]" is not a catalog
		// string and the CLI would have to strip it.
		{"claude-sonnet-4-6[1m]", "claude-sonnet-5"},
		{"claude-sonnet-4-6", "claude-sonnet-5"},
		{"claude-opus-4-8[1m]", "claude-opus-5-5"},
		// A dated snapshot of a retired model is still that model.
		{"claude-opus-4-8-20260101", "claude-opus-5-5"},

		// Still selectable → untouched, suffixes and snapshots included:
		// those were chosen on purpose.
		{"claude-opus-5-5", "claude-opus-5-5"},
		{"claude-sonnet-5", "claude-sonnet-5"},
		{"claude-haiku-4-5", "claude-haiku-4-5"},
		{"claude-fable-5-1", "claude-fable-5-1"},
		{"claude-opus-5-5[1m]", "claude-opus-5-5[1m]"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},

		// Unknown to the catalog → untouched. A sentinel, a CLI alias,
		// another vendor's id: none of these has a lineage to follow.
		{"", ""},
		{"<synthetic>", "<synthetic>"},
		{"opus", "opus"},
		{"claude-mythos-5-1", "claude-mythos-5-1"},
		{"gpt-4o", "gpt-4o"},
	}
	for _, tc := range cases {
		if got := currentModel(tc.in); got != tc.want {
			t.Errorf("currentModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCurrentNeverCrossesFamiliesOrDowngrades states the two
// invariants as properties of the whole catalog, so they hold for rows
// added after this test was written and not just the ids it names.
func TestCurrentNeverCrossesFamiliesOrDowngrades(t *testing.T) {
	for _, m := range catalog {
		got := currentModel(m.ID)
		if got == m.ID {
			continue
		}
		from, fromV, ok := lineage(m.ID)
		if !ok {
			t.Errorf("%s has no parseable lineage yet Current moved it to %s", m.ID, got)
			continue
		}
		to, toV, ok := lineage(got)
		if !ok || to != from {
			t.Errorf("currentModel(%s) = %s: crossed from the %s family", m.ID, got, from)
		}
		if !newerVersion(toV, fromV) {
			t.Errorf("currentModel(%s) = %s: not newer", m.ID, got)
		}
		if info, ok := lookupModel(got); !ok || !info.Selectable() {
			t.Errorf("currentModel(%s) = %s, which is not selectable", m.ID, got)
		}
		// Idempotent: the boot pass runs every restart, so a second
		// application must be a no-op or pins would drift each boot.
		if again := currentModel(got); again != got {
			t.Errorf("currentModel(currentModel(%s)) = %s, want %s", m.ID, again, got)
		}
	}
}

// TestEveryRetiredRowWithASelectableFamilyMovesForward is the
// guarantee the boot pass relies on: after a restart no active agent is
// left on a retired pin unless its whole family is gone from the
// picker. A retired row that Current leaves in place while a newer
// sibling is on offer would be an agent stuck on a model nobody can
// newly choose.
func TestEveryRetiredRowWithASelectableFamilyMovesForward(t *testing.T) {
	selectable := map[string]bool{}
	for _, m := range catalog {
		if f, _, ok := lineage(m.ID); ok && m.Selectable() {
			selectable[f] = true
		}
	}
	for _, m := range catalog {
		if m.Selectable() {
			continue
		}
		f, _, ok := lineage(m.ID)
		if !ok || !selectable[f] {
			continue
		}
		if got := currentModel(m.ID); got == m.ID {
			t.Errorf("retired %s stays put although the %s family still has a selectable row", m.ID, f)
		}
	}
}

func TestLineage(t *testing.T) {
	cases := []struct {
		in      string
		family  string
		version []int
		ok      bool
	}{
		{"claude-opus-4-8", "opus", []int{4, 8}, true},
		{"claude-opus-5", "opus", []int{5}, true},
		{"claude-opus-5-5", "opus", []int{5, 5}, true},
		{"claude-haiku-4-5", "haiku", []int{4, 5}, true},
		{"claude-fable-5-1", "fable", []int{5, 1}, true},
		{"claude-opus", "", nil, false},
		{"claude-3-5-sonnet", "", nil, false},
		{"claude--5", "", nil, false},
		{"opus-5", "", nil, false},
		{"", "", nil, false},
	}
	for _, tc := range cases {
		f, v, ok := lineage(tc.in)
		if ok != tc.ok || f != tc.family || !equalInts(v, tc.version) {
			t.Errorf("lineage(%q) = (%q, %v, %v), want (%q, %v, %v)", tc.in, f, v, ok, tc.family, tc.version, tc.ok)
		}
	}
}

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		a, b []int
		want bool
	}{
		{[]int{5, 5}, []int{5}, true},
		{[]int{5}, []int{4, 8}, true},
		{[]int{5, 1}, []int{5}, true},
		{[]int{4, 8}, []int{4, 7}, true},
		{[]int{5}, []int{5, 5}, false},
		{[]int{5}, []int{5}, false},
		{[]int{5}, []int{5, 0}, false},
		{[]int{4, 10}, []int{4, 9}, true},
		{nil, nil, false},
	}
	for _, tc := range cases {
		if got := newerVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("newerVersion(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
