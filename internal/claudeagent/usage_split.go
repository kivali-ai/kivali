package claudeagent

import "github.com/kivali-ai/kivali/internal/provider"

// Per-turn token accounting for the CLI transport.
//
// stream-json reports usage in two places and both need care: each
// assistant event carries the START-of-message snapshot of its call,
// repeated once per content block, and the result frame carries the
// turn's real total plus a session-cumulative per-model gauge. The
// reduction — count each API call once, let the result frame's final
// numbers supersede the snapshots, recover a turn's per-model share
// from the gauge — lives in turnUsage and usageGauge,
// shared by the persistent runner and the per-call stream (which also
// reads subagent runs). This file keeps what is specific to
// attributing a call's model and emitting usage rows.

// attributeModel picks the model ID one call's tokens should be billed
// to, given the ID reported on the assistant message, the last genuine
// ID seen on this stream, and the ID the request asked for.
//
// `reported` is normally the answering model. But Claude Code puts a
// sentinel ("<synthetic>") on the message that closes an errored or
// timed-out turn, and some transports leave the field empty. A sentinel
// is not a model — pricing has no row for it, so tokens bucketed under
// it price at $0 even though Anthropic billed them.
//
// `lastReal` is the previous genuine ID on the same stream, seeded from
// the CLI's "system" init event, so it is populated well before any
// synthetic message can arrive. `requested` is the last resort, for a
// stream whose very first message is already a sentinel.
//
// Returning "" when nothing is usable is deliberate: the reducer still
// counts those tokens under a blank label, which keeps the split's sum
// equal to the accumulated total. A visible blank bucket beats a silent
// shortfall in a billing number.
func attributeModel(reported, lastReal, requested string) string {
	for _, m := range [...]string{reported, lastReal, requested} {
		if m != "" && !isSentinelModel(m) {
			return m
		}
	}
	return ""
}

// usageEvents expands one finished turn into the UsageEvent rows to
// record: one per answering model when the turn was mixed, otherwise a
// single row carrying the flat total.
//
// costUSD is the CLI's total for the whole turn — it is not reported
// per call, so it cannot be attributed per model. It rides entirely on
// the FIRST row and is zero on the rest, which keeps the sum across a
// turn's rows equal to the turn's real cost. Splitting it by token share
// would invent a per-model precision the transport never gave us, and
// repeating it on every row would multiply the total.
func usageEvents(base provider.UsageEvent, total provider.TokenUsage, split []provider.ModelUsage, costUSD float64) []provider.UsageEvent {
	if len(split) == 0 {
		base.InputTokens = total.InputTokens
		base.OutputTokens = total.OutputTokens
		base.CacheReadTokens = total.CacheReadTokens
		base.CacheCreateTokens = total.CacheCreateTokens
		base.CostUSD = costUSD
		return []provider.UsageEvent{base}
	}
	out := make([]provider.UsageEvent, 0, len(split))
	for i, mu := range split {
		ev := base
		ev.Model = mu.Model
		ev.InputTokens = mu.Usage.InputTokens
		ev.OutputTokens = mu.Usage.OutputTokens
		ev.CacheReadTokens = mu.Usage.CacheReadTokens
		ev.CacheCreateTokens = mu.Usage.CacheCreateTokens
		if i == 0 {
			ev.CostUSD = costUSD
		}
		out = append(out, ev)
	}
	return out
}
