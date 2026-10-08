package claudeagent

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

func TestBaseModelAndWants1M(t *testing.T) {
	cases := []struct {
		in       string
		wantBase string
		want1M   bool
	}{
		{"claude-opus-4-8", "claude-opus-4-8", false},
		{"claude-opus-4-8[1m]", "claude-opus-4-8", true},
		{"claude-sonnet-4-6[1m]", "claude-sonnet-4-6", true},
		{"claude-haiku-4-5", "claude-haiku-4-5", false},
		{"", "", false},
		// Suffix only counts at the end — a [1m] mid-string is not a flag.
		{"claude-[1m]-weird", "claude-[1m]-weird", false},
	}
	for _, tc := range cases {
		if got := baseModel(tc.in); got != tc.wantBase {
			t.Errorf("baseModel(%q) = %q, want %q", tc.in, got, tc.wantBase)
		}
		if got := wants1M(tc.in); got != tc.want1M {
			t.Errorf("wants1M(%q) = %v, want %v", tc.in, got, tc.want1M)
		}
	}
	// baseModel must NOT learn about dated snapshots: it also builds the
	// raw Messages API request, where stripping the snapshot the caller
	// pinned would silently serve a different model.
	if got := baseModel("claude-haiku-4-5-20251001"); got != "claude-haiku-4-5-20251001" {
		t.Errorf("baseModel dropped a snapshot date: %q", got)
	}
}

// TestCatalogKeyStripsDatedSnapshot pins the normalization that lets a
// reported id find its catalog row. On the SDK transport the CLI echoes
// back the API's own answer, which for some models is the dated
// snapshot ("claude-haiku-4-5-20251001") even when the request named the
// bare id. Unstripped, it missed every catalog-keyed table and 532 prod
// rows over five months reported as unpriced tokens.
func TestCatalogKeyStripsDatedSnapshot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5"},
		{"claude-haiku-4-5-20251001[1m]", "claude-haiku-4-5"},
		// Not a model we price, but the shape rule is general: any
		// model the API serves under a dated id normalizes, now and in
		// future. Whether a row exists for it is the catalog's call.
		{"claude-opus-4-5-20251101", "claude-opus-4-5"},
		// Untouched shapes.
		{"claude-haiku-4-5", "claude-haiku-4-5"},
		{"claude-opus-4-8[1m]", "claude-opus-4-8"},
		{"", ""},
		// Wrong digit count is not a date, in either direction.
		{"claude-haiku-4-5-2025100", "claude-haiku-4-5-2025100"},
		{"claude-haiku-4-5-202510011", "claude-haiku-4-5-202510011"},
		// Digits must be the whole tail, and the separator must be a
		// hyphen.
		{"claude-haiku-4-5-2025100x", "claude-haiku-4-5-2025100x"},
		{"claude-haiku-4-5.20251001", "claude-haiku-4-5.20251001"},
		// Mid-string dates stay put; only a trailing one is a snapshot.
		{"claude-20251001-haiku", "claude-20251001-haiku"},
		// Never strips down to nothing — an id that is only a date is
		// not a snapshot of anything, and "" would match an empty key.
		{"-20251001", "-20251001"},
		// Sentinels survive intact, including one with a dated-looking
		// tail: the brackets are what isSentinelModel matches on.
		{"<synthetic>", "<synthetic>"},
		{"<synthetic-20251001>", "<synthetic-20251001>"},
	}
	for _, tc := range cases {
		if got := catalogKey(tc.in); got != tc.want {
			t.Errorf("catalogKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// And normalizing must not turn a sentinel into a real-looking id.
	for _, m := range []string{"<synthetic>", "<synthetic-20251001>"} {
		if !isSentinelModel(m) {
			t.Errorf("isSentinelModel(%q) = false, want true", m)
		}
	}
}

// TestCatalogKeyReadsCloudProviderIDs: signed in to Amazon Bedrock or
// Google Vertex AI, the CLI reports the provider's own id for a model;
// each shape reads as the catalog id, so the usage row prices and
// labels. Shapes from the providers' model tables and the CLI's docs.
func TestCatalogKeyReadsCloudProviderIDs(t *testing.T) {
	cases := []struct{ in, want string }{
		// Bedrock cross-region inference profiles, each prefix.
		{"us.anthropic.claude-opus-5-5", "claude-opus-5-5"},
		{"eu.anthropic.claude-opus-5-5", "claude-opus-5-5"},
		{"apac.anthropic.claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"global.anthropic.claude-opus-5-5", "claude-opus-5-5"},
		{"us-gov.anthropic.claude-opus-4-8", "claude-opus-4-8"},
		{"jp.anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5"},
		// Bedrock foundation model ids, with and without a version.
		{"anthropic.claude-haiku-4-5-20251001-v1:0", "claude-haiku-4-5"},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", "claude-sonnet-4-5"},
		{"anthropic.claude-opus-5-5-v1", "claude-opus-5-5"},
		{"us.anthropic.claude-opus-4-6-v1[1m]", "claude-opus-4-6"},
		// Bedrock Mantle.
		{"anthropic.claude-sonnet-5", "claude-sonnet-5"},
		// Vertex AI.
		{"claude-haiku-4-5@20251001", "claude-haiku-4-5"},
		{"claude-opus-5-5@20260101", "claude-opus-5-5"},
		{"claude-sonnet-4-5@20250929[1m]", "claude-sonnet-4-5"},
		// Not provider shapes: left alone.
		{"arn:aws:bedrock:us-east-2:123456789012:application-inference-profile/opus", "arn:aws:bedrock:us-east-2:123456789012:application-inference-profile/opus"},
		{"two.labels.anthropic.claude-opus-5-5", "two.labels.anthropic.claude-opus-5-5"},
		{"claude-opus-5-5-vx", "claude-opus-5-5-vx"},
		{"@20251001", "@20251001"},
	}
	for _, tc := range cases {
		if got := catalogKey(tc.in); got != tc.want {
			t.Errorf("catalogKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	d := New(Options{})
	for _, id := range []string{"us.anthropic.claude-opus-5-5", "claude-haiku-4-5@20251001", "anthropic.claude-haiku-4-5-20251001-v1:0"} {
		m, ok := d.Resolve(id)
		if !ok || m.Label == "" || m.Label == id {
			t.Errorf("Resolve(%q) = %+v, %v; want a labelled catalog row", id, m, ok)
		}
		if d.Price(id, provider.TokenUsage{InputTokens: 1_000_000}) <= 0 {
			t.Errorf("Price(%q) = 0; want the catalog rate", id)
		}
	}
}

// TestContextWindowResolvesDatedSnapshot is the second table the
// mismatch reached. A dated id missed nativeContextWindow too and sized
// at the 200k fallback, which is the same under-report that makes the
// context-fill ring warn early on a model that is nowhere near full.
func TestContextWindowResolvesDatedSnapshot(t *testing.T) {
	bare, ok := lookupModel("claude-haiku-4-5")
	if !ok || bare.NativeContext == 0 {
		t.Skip("haiku-4-5 has no native window in the catalog")
	}
	if got := contextWindow("claude-haiku-4-5-20251001"); got != bare.NativeContext {
		t.Errorf("contextWindow(dated haiku) = %d, want %d", got, bare.NativeContext)
	}
	// lookupModel resolves it too, so anything else reading the catalog
	// through it gets the right row.
	if got, ok := lookupModel("claude-haiku-4-5-20251001"); !ok || got.ID != "claude-haiku-4-5" {
		t.Errorf("lookupModel(dated haiku) = (%q, %v), want (claude-haiku-4-5, true)", got.ID, ok)
	}
}

func TestFriendlyModelName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"claude-opus-4-8", "Opus 4.8"},
		{"claude-opus-4-7", "Opus 4.7"},
		{"claude-sonnet-4-6", "Sonnet 4.6"},
		{"claude-haiku-4-5", "Haiku 4.5"},
		{"claude-opus-4-8[1m]", "Opus 4.8 · 1M"},
		{"claude-sonnet-4-6[1m]", "Sonnet 4.6 · 1M"},
		// Fable 5 is the first single-part version we ship ("5", not
		// "4-8"), so it exercises the len(parts)>=2 branch at its
		// minimum. A future family with a bare version ("claude-foo")
		// would fall through to the raw-ID path instead.
		{"claude-fable-5", "Fable 5"},
		{"claude-fable-5[1m]", "Fable 5 · 1M"},
		// Fable 5.1 rejoins the two-part shape, but off a family whose
		// previous member was one-part — so "claude-fable-5-1" must
		// render "Fable 5.1" and not be mistaken for the bare "Fable 5"
		// by a prefix match anywhere downstream.
		{"claude-fable-5-1", "Fable 5.1"},
		// Opus 5.5 is the same two-part shape after a one-part sibling,
		// and the fleet default — the label most chips show.
		{"claude-opus-5-5", "Opus 5.5"},
		{"", ""},
		// Unknown / off-pattern IDs degrade to the raw ID (1M marker still
		// prettified) rather than dropping the label.
		{"gpt-4o", "gpt-4o"},
		{"weird[1m]", "weird · 1M"},
	}
	for _, tc := range cases {
		if got := friendlyModelName(tc.in); got != tc.want {
			t.Errorf("friendlyModelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestContextWindow pins the two-part rule: a "[1m]" suffix means 1M,
// and a natively-1M model is 1M with a bare ID too. The native half is
// what stops a bare model ID from being sized at a fifth of its real
// capacity, and getting it wrong is invisible in every test that only
// uses suffixed IDs.
func TestContextWindow(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		// Natively 1M — bare name, no suffix needed.
		{"claude-fable-5-1", 1_000_000},
		{"claude-fable-5", 1_000_000},
		{"claude-opus-5-5", 1_000_000},
		{"claude-opus-5", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4-7", 1_000_000},
		// Natively 200k — the suffix is the only way up, and is still
		// honored for grandfathered pins.
		{"claude-sonnet-4-6", 200_000},
		{"claude-sonnet-4-6[1m]", 1_000_000},
		// 200k with no 1M form at all.
		{"claude-haiku-4-5", 200_000},
		// A suffix on a natively-1M model is redundant, not wrong.
		{"claude-opus-5[1m]", 1_000_000},
		// Unknown models fall to 200k: under-reporting capacity makes
		// the fill ring warn early, which is the safe direction.
		{"gpt-4o", 200_000},
		{"", 200_000},
	}
	for _, tc := range cases {
		if got := contextWindow(tc.in); got != tc.want {
			t.Errorf("contextWindow(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeEffort(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", DefaultEffort},      // unset → fleet default (high)
		{"bogus", DefaultEffort}, // typo/stale → default, never a bad flag
		{"HIGH", DefaultEffort},  // case-sensitive: not a match → default
		{"low", "low"},
		{"medium", "medium"},
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
	}
	for _, tc := range cases {
		if got := normalizeEffort(tc.in); got != tc.want {
			t.Errorf("normalizeEffort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if DefaultEffort != EffortHigh {
		t.Errorf("DefaultEffort = %q, want high (the fleet default)", DefaultEffort)
	}
	if !validEffort("max") || validEffort("turbo") {
		t.Error("ValidEffort gate is wrong")
	}
}
