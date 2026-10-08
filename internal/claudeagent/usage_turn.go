package claudeagent

import (
	"sort"

	"github.com/kivali-ai/kivali/internal/provider"
)

// turnUsage reduces what a turn's stream reports about tokens into the
// counts a usage row bills from.
//
// Two sources feed it, and they are wrong in complementary ways. Every
// assistant message the CLI streams carries a usage block, but it is
// the snapshot the API took when the message STARTED: the input and
// cache counts are final (the prompt is known before generation
// begins) while output_tokens is the handful written so far. The CLI
// also emits one assistant event per content block, so a message with
// thinking, text and a tool_use arrives three times, with the same id
// and the same snapshot on each. Summing events therefore multiplies
// input and cache tokens by the blocks per message and reports almost
// no output — measured on prod as 2.4x over on cache writes and 25x
// under on output.
//
// The terminal result frame has the numbers that are right: the turn's
// total with the final output count. But only as one sum across every
// call, plus a per-model gauge that is cumulative over the session
// rather than the turn (see usageGauge).
//
// So calls are deduplicated by message id, latest snapshot wins, and a
// result frame's total SUPERSEDES the snapshots of every call before
// it. Calls with no result frame behind them — the process died, the
// transport never sent one — count from their snapshots, which is
// exact for input and cache tokens and a floor for output.
//
// Not safe for concurrent use; callers hold their own stream mutex.
type turnUsage struct {
	// pending is the deduplicated calls no result frame has covered
	// yet, in first-seen order; byID indexes it by message id.
	pending []usageCall
	byID    map[string]int
	// covered holds the authoritative per-model counts of every frame
	// folded in so far.
	covered modelBuckets
	// lastModel is the model of the most recent call: the bucket for a
	// frame that closes zero calls, which has nowhere better to land.
	lastModel string
}

type usageCall struct {
	id    string
	model string
	usage provider.TokenUsage
}

// Call records the usage snapshot on one streamed assistant message.
//
// id is the API's message id. The same id arriving again — the next
// content block of the same message — replaces the earlier snapshot
// instead of adding to it. An empty id cannot be matched, so a call
// without one counts once, as its own call. model is the id the tokens
// bill to; the caller has already resolved sentinels and blanks.
func (t *turnUsage) Call(id, model string, u provider.TokenUsage) {
	if model != "" {
		t.lastModel = model
	}
	if id != "" {
		if i, ok := t.byID[id]; ok {
			t.pending[i].model = model
			t.pending[i].usage = u
			return
		}
		if t.byID == nil {
			t.byID = make(map[string]int, 4)
		}
		t.byID[id] = len(t.pending)
	}
	t.pending = append(t.pending, usageCall{id: id, model: model, usage: u})
}

// Result folds in one result frame. total is the frame's own usage:
// the sum over the calls it closes, with final output counts. byModel
// is that same total split by answering model when the caller can
// supply it (see usageGauge.Delta), or nil when it cannot.
//
// The frame supersedes every pending call. Its tokens are bucketed
// per model from the best information present: the caller's split
// when given; otherwise the whole total under the one model the calls
// were answered by; otherwise — mixed models and no split — input and
// cache tokens per model exactly as the snapshots reported them, and
// output tokens shared by call count, because the snapshots say
// nothing useful about output and the frame does not say whose it
// was. That last branch is a fair share, not a measurement, and it is
// reached only on the first frame of a process that resumed a session
// AND had more than one model answer in that turn.
func (t *turnUsage) Result(total provider.TokenUsage, byModel []provider.ModelUsage) {
	frame := t.orderLikeCalls(byModel)
	if len(frame) == 0 || (sumModelUsage(frame).IsZero() && !total.IsZero()) {
		frame = t.reconstruct(total)
	}
	for _, mu := range frame {
		if mu.Usage.IsZero() {
			continue
		}
		t.covered.add(mu.Model, mu.Usage)
	}
	t.pending = nil
	t.byID = nil
}

// Total is the turn's token count so far: every covered frame plus the
// snapshots of calls no frame has closed.
func (t *turnUsage) Total() provider.TokenUsage {
	return sumModelUsage(t.all())
}

// Split is Total bucketed by answering model, in first-answered order,
// or nil when a single model answered — the caller's flat {Model,
// Usage} pair already says everything a one-element split would, and
// consumers read nil as "not mixed".
func (t *turnUsage) Split() []provider.ModelUsage {
	all := t.all()
	if len(all) < 2 {
		return nil
	}
	return all
}

// all merges the covered buckets with the pending snapshots.
func (t *turnUsage) all() []provider.ModelUsage {
	b := t.covered.clone()
	for _, c := range t.pending {
		b.add(c.model, c.usage)
	}
	return b.list()
}

// orderLikeCalls returns split reordered so models appear in the order
// the pending calls first answered, then any the calls never named in
// the order given. A split from the CLI's gauge arrives sorted by id;
// the rows should read in the order the models actually spoke.
func (t *turnUsage) orderLikeCalls(split []provider.ModelUsage) []provider.ModelUsage {
	if len(split) < 2 {
		return split
	}
	byModel := make(map[string]provider.ModelUsage, len(split))
	for _, mu := range split {
		byModel[mu.Model] = mu
	}
	out := make([]provider.ModelUsage, 0, len(split))
	taken := make(map[string]bool, len(split))
	for _, c := range t.pending {
		if mu, ok := byModel[c.model]; ok && !taken[c.model] {
			out = append(out, mu)
			taken[c.model] = true
		}
	}
	for _, mu := range split {
		if !taken[mu.Model] {
			out = append(out, mu)
		}
	}
	return out
}

// reconstruct buckets a frame total by model from the pending
// snapshots alone. See Result for the three cases.
func (t *turnUsage) reconstruct(total provider.TokenUsage) []provider.ModelUsage {
	var order []string
	calls := make(map[string]int, 2)
	for _, c := range t.pending {
		if calls[c.model] == 0 {
			order = append(order, c.model)
		}
		calls[c.model]++
	}
	switch len(order) {
	case 0:
		return []provider.ModelUsage{{Model: t.lastModel, Usage: total}}
	case 1:
		return []provider.ModelUsage{{Model: order[0], Usage: total}}
	}
	out := make([]provider.ModelUsage, 0, len(order))
	remaining := total.OutputTokens
	for i, m := range order {
		var u provider.TokenUsage
		for _, c := range t.pending {
			if c.model == m {
				u.InputTokens += c.usage.InputTokens
				u.CacheReadTokens += c.usage.CacheReadTokens
				u.CacheCreateTokens += c.usage.CacheCreateTokens
			}
		}
		share := total.OutputTokens * calls[m] / len(t.pending)
		if i == len(order)-1 {
			share = remaining
		}
		remaining -= share
		u.OutputTokens = share
		out = append(out, provider.ModelUsage{Model: m, Usage: u})
	}
	return out
}

func sumModelUsage(split []provider.ModelUsage) provider.TokenUsage {
	var sum provider.TokenUsage
	for _, mu := range split {
		sum = sum.Add(mu.Usage)
	}
	return sum
}

// modelBuckets accumulates token counts per model, preserving the
// order models were first seen so an emitted split reads in the order
// the models actually answered.
type modelBuckets struct {
	order []string
	byID  map[string]provider.TokenUsage
}

// add folds u into the bucket for model. An empty model is a bucket
// like any other: dropping it would make the split disagree with the
// total, and a silent shortfall in a billing number is worse than a
// row with a blank label.
func (m *modelBuckets) add(model string, u provider.TokenUsage) {
	if m.byID == nil {
		m.byID = make(map[string]provider.TokenUsage, 2)
	}
	if _, seen := m.byID[model]; !seen {
		m.order = append(m.order, model)
	}
	m.byID[model] = m.byID[model].Add(u)
}

func (m *modelBuckets) list() []provider.ModelUsage {
	out := make([]provider.ModelUsage, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, provider.ModelUsage{Model: id, Usage: m.byID[id]})
	}
	return out
}

func (m *modelBuckets) clone() modelBuckets {
	var c modelBuckets
	for _, mu := range m.list() {
		c.add(mu.Model, mu.Usage)
	}
	return c
}

// usageGauge turns the CLI's per-model usage gauge into per-frame
// deltas.
//
// The result frame's modelUsage is cumulative over the whole SESSION:
// not the turn, and not even the process. A process started with
// --resume reloads the session's history into it, so its first frame
// already contains every earlier turn's tokens (measured against CLI
// 2.1.281: a resumed process's first frame read the previous process's
// final gauge plus its own turn). Only the difference between two
// consecutive readings on one process is a turn's own usage.
//
// A gauge therefore knows its baseline in exactly two cases: the
// process began a NEW session, so the gauge started at zero and the
// first reading is itself a delta; or it has already taken a reading.
// Delta returns nil otherwise, and the caller lets turnUsage.Result
// reconstruct the split from the calls it saw.
//
// The zero value is a gauge with an unknown baseline.
type usageGauge struct {
	last  map[string]provider.TokenUsage
	known bool
}

// newUsageGauge returns a gauge for a process that began a fresh
// session (freshSession true, baseline zero) or resumed one (false,
// baseline unknown until the first reading).
func newUsageGauge(freshSession bool) usageGauge {
	return usageGauge{known: freshSession}
}

// Delta takes one frame's reading and returns each model's change since
// the previous reading, sorted by model id, or nil when the baseline is
// unknown. A reading that moved backwards means the gauge was reset
// under us; that reading becomes the new baseline and the delta is
// reported unknown rather than negative.
//
// Every non-nil reading advances the gauge, including frames the caller
// goes on to drop — an unmodelled turn still spent tokens, and the next
// delta has to be measured from after it. A nil reading (a frame with
// no modelUsage at all) leaves the gauge untouched and returns nil.
func (g *usageGauge) Delta(reading map[string]provider.TokenUsage) []provider.ModelUsage {
	if reading == nil {
		return nil
	}
	prev, known := g.last, g.known
	g.last = make(map[string]provider.TokenUsage, len(reading))
	for m, u := range reading {
		g.last[m] = u
	}
	g.known = true
	if !known {
		return nil
	}
	models := make([]string, 0, len(reading))
	for m := range reading {
		models = append(models, m)
	}
	sort.Strings(models)
	out := make([]provider.ModelUsage, 0, len(models))
	for _, m := range models {
		d := subUsage(reading[m], prev[m])
		if d.InputTokens < 0 || d.OutputTokens < 0 || d.CacheReadTokens < 0 || d.CacheCreateTokens < 0 {
			return nil
		}
		if d.IsZero() {
			continue
		}
		out = append(out, provider.ModelUsage{Model: m, Usage: d})
	}
	return out
}

func subUsage(u, o provider.TokenUsage) provider.TokenUsage {
	return provider.TokenUsage{
		InputTokens:       u.InputTokens - o.InputTokens,
		OutputTokens:      u.OutputTokens - o.OutputTokens,
		CacheReadTokens:   u.CacheReadTokens - o.CacheReadTokens,
		CacheCreateTokens: u.CacheCreateTokens - o.CacheCreateTokens,
	}
}
