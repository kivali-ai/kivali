package claudeagent

import "strings"

// contextSuffix1M is the Claude Code CLI marker that requests the 1M
// (1,000,000-token) context window for a pinned model name, e.g.
// "claude-sonnet-4-6[1m]".
//
// Why we carry it as a suffix rather than a separate model ID: our
// primary transport is the Claude Code SDK/CLI, which we drive with
// `--model <model>`, and the CLI exposes the large window for a pinned
// model name through this suffix (the same thing its `/model` picker
// lists as "… 1M"). The CLI strips it before talking to the provider.
//
// The suffix is NOT a blanket requirement, and treating it as one was a
// long-standing mistake here. The CLI's baked-in model catalog carries a
// per-model `context: {window, native_1m}`, and for every model released
// from Opus 4.7 onward — Opus 4.7/4.8/5/5.5, Sonnet 5, Fable 5/5.1 — the
// window is 1e6 with native_1m set: the BARE name already gets the full
// window and the suffix buys nothing. It is load-bearing only on the
// models that are natively 200k, which the picker does not offer at
// all: Sonnet 4.6 is retired, and a pin still carrying its suffix is
// honored until boot moves it to Sonnet 5 (see Current). See
// NativeContextWindow.
//
// The raw Messages API does not understand the suffix at all (1M is the
// default there for every 1M-capable model, no beta header), so the HTTP
// transport strips it via baseModel before sending. Pricing and any
// other table keyed on the bare model ID must strip it too.
const contextSuffix1M = "[1m]"

// nativeContextWindow is the context window a BARE model ID resolves to
// on the Claude Code CLI — the window you get with no [1m] suffix.
//
// Source: the CLI's own hand-maintained model catalog (`context.window`
// / `context.native_1m`), read out of the installed binary rather than
// inferred from the docs, because the two disagree in a way that
// matters. The Messages API serves 1M by default to every 1M-capable
// model including Sonnet 4.6; the CLI does not — its catalog pins
// Sonnet 4.6 at 200000 and offers 1M only via the suffix. Kivali runs on
// the CLI, so the CLI's numbers are the ones that govern here.
//
// Anything absent from this map is treated as 200k, which is the safe
// direction to be wrong: under-reporting capacity makes the context-fill
// ring warn early, while over-reporting lets it read "calm" while the
// session is actually compacting.
var nativeContextWindow = contextWindowsFromCatalog()

// contextWindowsFromCatalog projects the catalog's NativeContext field
// into the lookup this file already used. A model absent from the
// catalog still falls back to 200k, which is unchanged behaviour.
func contextWindowsFromCatalog() map[string]int {
	m := make(map[string]int, len(catalog))
	for _, info := range catalog {
		if info.NativeContext > 0 {
			m[info.ID] = info.NativeContext
		}
	}
	return m
}

// contextWindow returns the token capacity a model string resolves to on
// the CLI transport: 1M when the [1m] suffix asks for it explicitly, else
// the model's native window, else 200k for anything unrecognized.
//
// Both halves are needed. Dropping the suffix check would mis-size a
// grandfathered "claude-sonnet-4-6[1m]" pin at 200k; dropping the native
// lookup would mis-size a bare "claude-opus-5" at 200k, which is the
// same 5x under-report in the other direction. The native lookup goes
// through catalogKey, so a dated snapshot ID the CLI reports back
// resolves to its model's real window rather than the 200k fallback.
func contextWindow(model string) int {
	if wants1M(model) {
		return 1_000_000
	}
	if w, ok := nativeContextWindow[catalogKey(model)]; ok {
		return w
	}
	return 200_000
}

// baseModel removes the [1m] context-window suffix, yielding the bare
// model ID that pricing tables and the raw Messages API expect. A
// model with no suffix is returned unchanged.
func baseModel(model string) string {
	return strings.TrimSuffix(model, contextSuffix1M)
}

// catalogKey reduces any model string a transport may hand us to the
// bare ID the catalog is keyed on: it drops the [1m] context-window
// suffix AND a trailing dated snapshot suffix ("-YYYYMMDD").
//
// Every table keyed on a model — pricing, native context windows,
// lookupModel — is keyed on the bare ID declared in catalog.go
// ("claude-haiku-4-5"). What we ask for is that bare ID, but what comes
// back is not always: on the SDK transport the CLI echoes the API's own
// answer on each assistant message, and for some models that is the
// dated snapshot ("claude-haiku-4-5-20251001"). The stream readers
// record what was reported — the truthful account of which snapshot
// actually answered — so the normalization belongs here, on the way
// into the table, rather than in what we write down. usage.jsonl is
// re-priced on read, so normalizing the lookup retroactively prices
// every row already on disk.
//
// Deliberately NOT folded into baseModel. baseModel is what the CLI is
// asked for and feeds isSentinelModel: the API accepts a dated ID, and
// serving a different snapshot than the caller pinned would be a
// silent substitution, while a sentinel such as
// "<synthetic>" must keep its brackets intact to stay recognizable.
//
// The date is matched by shape — a hyphen then exactly eight digits at
// the very end — so this covers every model the API may serve under a
// dated ID, not only the ones we have already seen. Both suffix orders
// are handled, since the CLI could reasonably emit either.
//
// The same goes for the ids a cloud provider serves a model under, when
// the CLI is signed in to one: Amazon Bedrock's
// ("us.anthropic.claude-opus-5-5", "anthropic.claude-haiku-4-5-20251001-v1:0")
// and Google Vertex AI's ("claude-haiku-4-5@20251001") read as the
// catalog's own id, so their usage rows price and label.
func catalogKey(model string) string {
	for {
		next := trimSnapshotDate(trimCloudForm(baseModel(model)))
		if next == model {
			return model
		}
		model = next
	}
}

// trimCloudForm turns a cloud provider's form of a Claude model id into
// the Anthropic form, and leaves anything else untouched:
//
//   - Amazon Bedrock: an optional cross-region inference profile prefix
//     (one label: "us.", "eu.", "apac.", "global.", "us-gov."...), then
//     "anthropic.", then the id, then an optional version suffix
//     ("-v1:0", "-v1").
//   - Google Vertex AI: the id, then "@" and a snapshot date
//     ("claude-haiku-4-5@20251001").
//
// What remains may still carry a "-YYYYMMDD" date; catalogKey trims it.
func trimCloudForm(model string) string {
	if i := strings.Index(model, "anthropic.claude-"); i >= 0 && validProfilePrefix(model[:i]) {
		model = trimBedrockVersion(model[i+len("anthropic."):])
	}
	if i := strings.LastIndexByte(model, '@'); i > 0 && strings.HasPrefix(model, "claude-") {
		model = model[:i]
	}
	return model
}

// validProfilePrefix reports whether p is empty or one lowercase label
// and a dot: the shape of a Bedrock inference profile prefix.
func validProfilePrefix(p string) bool {
	if p == "" {
		return true
	}
	if !strings.HasSuffix(p, ".") || len(p) == 1 {
		return false
	}
	for _, r := range p[:len(p)-1] {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return true
}

// trimBedrockVersion removes a trailing "-v<n>" or "-v<n>:<m>".
func trimBedrockVersion(model string) string {
	i := strings.LastIndex(model, "-v")
	if i <= 0 {
		return model
	}
	major, minor, hasMinor := strings.Cut(model[i+2:], ":")
	if !allDigits(major) || (hasMinor && !allDigits(minor)) {
		return model
	}
	return model[:i]
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// trimSnapshotDate removes a trailing "-YYYYMMDD" from a model ID and
// leaves anything else untouched. It never returns the empty string: an
// ID that is nothing but a date is not a snapshot of anything, so it
// comes back unchanged rather than collapsing to "" and matching an
// empty table key.
func trimSnapshotDate(model string) string {
	const n = len("-20260916")
	if len(model) <= n || model[len(model)-n] != '-' {
		return model
	}
	for i := len(model) - n + 1; i < len(model); i++ {
		if model[i] < '0' || model[i] > '9' {
			return model
		}
	}
	return model[:len(model)-n]
}

// wants1M reports whether the model string carries the [1m] suffix,
// i.e. whether the 1M context window was explicitly requested.
func wants1M(model string) bool {
	return strings.HasSuffix(model, contextSuffix1M)
}

// isSentinelModel reports whether a model ID is a Claude Code
// placeholder rather than a real, billable model.
//
// When a turn ends in an error or a timeout, the CLI closes it with an
// assistant message carrying "<synthetic>" as its model ID. The tokens
// on that message are real — the request was made and Anthropic billed
// it — but the ID names no model, so it can never appear in
// defaultPricing and pricingTable.Cost values it at $0. Left unchecked
// it also wins the latest-wins model resolution every stream reader
// does, which parks a whole run's accumulated tokens under an unpriced
// ID and reports billed tokens as $0.00.
//
// Matched by shape, not by the literal "<synthetic>". Real model IDs
// are bare kebab-case ("claude-opus-5"), optionally with the [1m]
// suffix; angle brackets are the CLI's own convention for "not a
// model". Shape-matching means a future sentinel ("<interrupted>", say)
// is caught the day it ships rather than silently zeroing another batch
// of turns — the same failure mode defaultPricing's header warns about
// for retired models, except a sentinel hits it permanently.
//
// Only the CLI driver (internal/claudeagent) calls this: it reports the
// sentinel message as a typed error (StreamError, StopError) and bills
// its tokens to the model that answered, so no sentinel id reaches
// common code.
func isSentinelModel(model string) bool {
	m := baseModel(model)
	return len(m) >= 2 && strings.HasPrefix(m, "<") && strings.HasSuffix(m, ">")
}

// friendlyModelName renders a model ID as a short, human-readable label
// for the UI — "claude-opus-4-8" → "Opus 4.8", "claude-sonnet-4-6[1m]"
// → "Sonnet 4.6 · 1M". The kebab IDs are exact and unambiguous but hard
// to scan in a dropdown or a chat-bubble pill, so this trims the
// "claude-" prefix, title-cases the family, joins the version parts with
// a dot, and tacks on a "· 1M" marker when the 1M window was requested.
//
// It's intentionally tolerant: any ID that doesn't fit the
// "claude-<family>-<major>-<minor>…" shape (a new family, a renamed
// scheme, an outright typo) falls back to the raw ID with only the [1m]
// suffix prettified, so the UI degrades to "still readable" rather than
// dropping the label entirely.
func friendlyModelName(model string) string {
	if model == "" {
		return ""
	}
	want1M := wants1M(model)
	base := baseModel(model)

	pretty := base
	if rest, ok := strings.CutPrefix(base, "claude-"); ok {
		parts := strings.Split(rest, "-")
		if len(parts) >= 2 {
			family := parts[0]
			if family != "" {
				family = strings.ToUpper(family[:1]) + family[1:]
			}
			version := strings.Join(parts[1:], ".")
			pretty = strings.TrimSpace(family + " " + version)
		}
	}
	if want1M {
		pretty += " · 1M"
	}
	return pretty
}
