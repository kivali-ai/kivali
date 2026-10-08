package claudeagent

import (
	"reflect"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// tu is a call snapshot with the shape the CLI actually streams: final
// prompt-side counts, output barely begun.
func tu(in, out, cacheRead, cacheCreate int) provider.TokenUsage {
	return provider.TokenUsage{InputTokens: in, OutputTokens: out, CacheReadTokens: cacheRead, CacheCreateTokens: cacheCreate}
}

// TestTurnUsageCountsEachMessageOnce guards against over-counting: the
// CLI streams one assistant event per content block, each
// repeating the same usage under the same message id. Three events for
// one message must bill as one call.
func TestTurnUsageCountsEachMessageOnce(t *testing.T) {
	var u turnUsage
	snap := tu(10, 8, 13689, 13376)
	u.Call("msg_1", "claude-haiku-4-5", snap) // thinking block
	u.Call("msg_1", "claude-haiku-4-5", snap) // text block
	u.Call("msg_1", "claude-haiku-4-5", snap) // tool_use block

	if got := u.Total(); got != snap {
		t.Errorf("Total = %+v, want the single snapshot %+v", got, snap)
	}
	if u.Split() != nil {
		t.Errorf("Split = %+v, want nil for one model", u.Split())
	}
}

// TestTurnUsageLatestSnapshotWins: should a later block of the same
// message ever carry different numbers, the last one stands — "keep
// the final usage for it", not the first.
func TestTurnUsageLatestSnapshotWins(t *testing.T) {
	var u turnUsage
	u.Call("msg_1", "m", tu(10, 1, 0, 0))
	u.Call("msg_1", "m", tu(10, 9, 0, 0))
	if got := u.Total(); got != tu(10, 9, 0, 0) {
		t.Errorf("Total = %+v, want the later snapshot", got)
	}
}

// TestTurnUsageCallsWithoutIDEachCount: a transport that omits ids
// gives us nothing to match on, so every event is its own call — the
// shape test fakes without message ids rely on.
func TestTurnUsageCallsWithoutIDEachCount(t *testing.T) {
	var u turnUsage
	u.Call("", "m", tu(100, 10, 0, 0))
	u.Call("", "m", tu(200, 20, 0, 0))
	if got := u.Total(); got != tu(300, 30, 0, 0) {
		t.Errorf("Total = %+v, want both calls summed", got)
	}
}

// TestTurnUsageResultSupersedesSnapshots is the under-count half: the
// snapshots say output is 1+2 tokens, the result frame says 300. The
// frame wins, and the snapshots it closes are not added on top.
func TestTurnUsageResultSupersedesSnapshots(t *testing.T) {
	var u turnUsage
	u.Call("msg_1", "m", tu(10, 1, 100, 5))
	u.Call("msg_1", "m", tu(10, 1, 100, 5))
	u.Call("msg_2", "m", tu(20, 2, 200, 6))
	frame := tu(30, 300, 300, 11)
	u.Result(frame, nil)

	if got := u.Total(); got != frame {
		t.Errorf("Total = %+v, want the frame total %+v", got, frame)
	}
	if u.Split() != nil {
		t.Errorf("Split = %+v, want nil: one model answered", u.Split())
	}
}

// TestTurnUsageResultUsesGivenSplit: when the caller recovered the
// frame's per-model share from the gauge, that is the split, ordered
// as the models first answered rather than as the gauge listed them.
func TestTurnUsageResultUsesGivenSplit(t *testing.T) {
	var u turnUsage
	u.Call("msg_1", "claude-fable-5-1", tu(10, 1, 0, 0))
	u.Call("msg_2", "claude-opus-5", tu(20, 1, 0, 0))
	u.Call("msg_3", "claude-fable-5-1", tu(30, 1, 0, 0))
	// The gauge sorts by id, so opus comes first here.
	u.Result(tu(60, 600, 0, 0), []provider.ModelUsage{
		{Model: "claude-opus-5", Usage: tu(20, 200, 0, 0)},
		{Model: "claude-fable-5-1", Usage: tu(40, 400, 0, 0)},
	})

	want := []provider.ModelUsage{
		{Model: "claude-fable-5-1", Usage: tu(40, 400, 0, 0)},
		{Model: "claude-opus-5", Usage: tu(20, 200, 0, 0)},
	}
	if got := u.Split(); !reflect.DeepEqual(got, want) {
		t.Errorf("Split = %+v, want %+v (first-answered order)", got, want)
	}
	if got := u.Total(); got != tu(60, 600, 0, 0) {
		t.Errorf("Total = %+v, want the split's sum", got)
	}
}

// TestTurnUsageMixedWithoutSplitSharesOutputByCallCount pins the one
// approximate branch: a mixed frame with no gauge delta. Prompt-side
// counts per model come from the snapshots, which are exact; output is
// shared by call count and must still sum to the frame's figure.
func TestTurnUsageMixedWithoutSplitSharesOutputByCallCount(t *testing.T) {
	var u turnUsage
	u.Call("msg_1", "a", tu(10, 1, 100, 0))
	u.Call("msg_2", "a", tu(20, 1, 0, 7))
	u.Call("msg_3", "b", tu(5, 1, 0, 0))
	u.Result(tu(35, 91, 100, 7), nil)

	split := u.Split()
	if len(split) != 2 {
		t.Fatalf("Split = %+v, want 2 buckets", split)
	}
	if split[0].Model != "a" || split[1].Model != "b" {
		t.Fatalf("Split order = %q/%q, want a/b", split[0].Model, split[1].Model)
	}
	// a made two of three calls: 91*2/3 = 60; b takes the remainder so
	// nothing is lost to rounding.
	if got, want := split[0].Usage, tu(30, 60, 100, 7); got != want {
		t.Errorf("a = %+v, want %+v", got, want)
	}
	if got, want := split[1].Usage, tu(5, 31, 0, 0); got != want {
		t.Errorf("b = %+v, want %+v", got, want)
	}
	if got := u.Total(); got != tu(35, 91, 100, 7) {
		t.Errorf("Total = %+v, want the frame total", got)
	}
}

// TestTurnUsageCallsAfterFrameCountFromSnapshots: a call streamed after
// the last frame (the process died before another result) keeps its
// snapshot — exact on prompt tokens, a floor on output — rather than
// vanishing.
func TestTurnUsageCallsAfterFrameCountFromSnapshots(t *testing.T) {
	var u turnUsage
	u.Call("msg_1", "m", tu(10, 1, 0, 0))
	u.Result(tu(10, 100, 0, 0), nil)
	u.Call("msg_2", "m", tu(20, 2, 50, 0))
	if got, want := u.Total(), tu(30, 102, 50, 0); got != want {
		t.Errorf("Total = %+v, want %+v (frame + trailing snapshot)", got, want)
	}
}

// TestTurnUsageFramesAdd: a turn can span several result frames (an
// absorbed continuation). Each frame is its own total, so they add.
func TestTurnUsageFramesAdd(t *testing.T) {
	var u turnUsage
	u.Call("msg_1", "m", tu(10, 1, 0, 0))
	u.Result(tu(10, 100, 0, 0), nil)
	u.Call("msg_2", "m", tu(20, 1, 0, 0))
	u.Result(tu(20, 200, 0, 0), nil)
	if got, want := u.Total(), tu(30, 300, 0, 0); got != want {
		t.Errorf("Total = %+v, want %+v", got, want)
	}
}

// TestTurnUsageFrameWithNoCallsLandsOnLastModel: a frame that closes
// no streamed call (the assistant events were lost, or a fake never
// sent any) still has tokens to book; they go under the model most
// recently seen.
func TestTurnUsageFrameWithNoCallsLandsOnLastModel(t *testing.T) {
	var u turnUsage
	u.Call("msg_1", "claude-opus-5", tu(10, 1, 0, 0))
	u.Result(tu(10, 100, 0, 0), nil)
	u.Result(tu(5, 50, 0, 0), nil)
	if got, want := u.Total(), tu(15, 150, 0, 0); got != want {
		t.Errorf("Total = %+v, want %+v", got, want)
	}
	if u.Split() != nil {
		t.Errorf("Split = %+v, want nil: everything under claude-opus-5", u.Split())
	}
}

// TestTurnUsageBlankModelBucketCounted: a call whose message carried no
// usable model id is still counted — under "" if need be — because a
// bucket set that does not reconstruct the total is a silent shortfall
// in a billing number.
func TestTurnUsageBlankModelBucketCounted(t *testing.T) {
	var u turnUsage
	u.Call("a", "claude-opus-5", tu(100, 0, 0, 0))
	u.Call("b", "", tu(7, 0, 0, 0))
	split := u.Split()
	if len(split) != 2 {
		t.Fatalf("Split = %+v, want 2 buckets", split)
	}
	if sum := split[0].Usage.InputTokens + split[1].Usage.InputTokens; sum != 107 {
		t.Errorf("split sums to %d input tokens, want 107 (nothing dropped)", sum)
	}
}

// TestUsageGaugeFreshSessionFirstReadingIsDelta: a process that began
// a new session started the CLI's gauge at zero, so its first reading
// is already the turn's own usage.
func TestUsageGaugeFreshSessionFirstReadingIsDelta(t *testing.T) {
	g := newUsageGauge(true)
	got := g.Delta(map[string]provider.TokenUsage{"claude-haiku-4-5": tu(26, 293, 68874, 14580)})
	want := []provider.ModelUsage{{Model: "claude-haiku-4-5", Usage: tu(26, 293, 68874, 14580)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Delta = %+v, want %+v", got, want)
	}
}

// TestUsageGaugeResumedSessionNeedsTwoReadings mirrors what CLI 2.1.281
// does on --resume: the first frame's gauge already holds the earlier
// process's spend (36 in / 333 out from two prior turns) plus this
// turn's (18 / 155). Nothing on the frame separates them, so the delta
// is unknown; the NEXT frame's is exact.
func TestUsageGaugeResumedSessionNeedsTwoReadings(t *testing.T) {
	g := newUsageGauge(false)
	if got := g.Delta(map[string]provider.TokenUsage{"m": tu(54, 488, 159350, 12378)}); got != nil {
		t.Errorf("first Delta on a resumed process = %+v, want nil (baseline unknown)", got)
	}
	got := g.Delta(map[string]provider.TokenUsage{"m": tu(72, 641, 219527, 12635)})
	want := []provider.ModelUsage{{Model: "m", Usage: tu(18, 153, 60177, 257)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("second Delta = %+v, want %+v", got, want)
	}
}

// TestUsageGaugeSplitsNewModelAndSkipsIdle: a model that first appears
// on this reading is a pure delta; one whose reading did not move
// contributed nothing to this frame and is left out.
func TestUsageGaugeSplitsNewModelAndSkipsIdle(t *testing.T) {
	g := newUsageGauge(true)
	g.Delta(map[string]provider.TokenUsage{"a": tu(10, 100, 0, 0)})
	got := g.Delta(map[string]provider.TokenUsage{"a": tu(10, 100, 0, 0), "b": tu(5, 50, 0, 0)})
	want := []provider.ModelUsage{{Model: "b", Usage: tu(5, 50, 0, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Delta = %+v, want %+v", got, want)
	}
}

// TestUsageGaugeBackwardsReadingIsUnknown: a gauge that moved down was
// reset under us. Reporting a negative share would subtract from a
// bill; the reading becomes the new baseline and this frame's split is
// unknown instead.
func TestUsageGaugeBackwardsReadingIsUnknown(t *testing.T) {
	g := newUsageGauge(true)
	g.Delta(map[string]provider.TokenUsage{"a": tu(10, 100, 0, 0)})
	if got := g.Delta(map[string]provider.TokenUsage{"a": tu(3, 30, 0, 0)}); got != nil {
		t.Errorf("Delta after a reset = %+v, want nil", got)
	}
	got := g.Delta(map[string]provider.TokenUsage{"a": tu(4, 40, 0, 0)})
	want := []provider.ModelUsage{{Model: "a", Usage: tu(1, 10, 0, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Delta from the new baseline = %+v, want %+v", got, want)
	}
}

// TestUsageGaugeNilReadingLeavesGaugeAlone: a frame with no modelUsage
// (a CLI that omits it, a fake) neither advances the gauge nor pretends to know
// a split.
func TestUsageGaugeNilReadingLeavesGaugeAlone(t *testing.T) {
	g := newUsageGauge(true)
	g.Delta(map[string]provider.TokenUsage{"a": tu(10, 100, 0, 0)})
	if got := g.Delta(nil); got != nil {
		t.Errorf("Delta(nil) = %+v, want nil", got)
	}
	got := g.Delta(map[string]provider.TokenUsage{"a": tu(15, 150, 0, 0)})
	want := []provider.ModelUsage{{Model: "a", Usage: tu(5, 50, 0, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Delta = %+v, want %+v (measured from the last real reading)", got, want)
	}
}

// TestUsageGaugeZeroValueIsUnknown pins the zero value: a stream built
// without a gauge (older callers, struct-literal tests) gets no split
// from it, never a wrong one.
func TestUsageGaugeZeroValueIsUnknown(t *testing.T) {
	var g usageGauge
	if got := g.Delta(map[string]provider.TokenUsage{"a": tu(10, 100, 0, 0)}); got != nil {
		t.Errorf("zero-value Delta = %+v, want nil", got)
	}
}
