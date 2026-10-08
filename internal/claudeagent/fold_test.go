package claudeagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
)

// foldFixture spins a runner on one of the fold scenarios and returns
// it with a live first turn, parked at the tool_result seam.
func foldFixture(t *testing.T, scenario string) (*runner, provider.Stream) {
	t.Helper()
	r, s, _ := foldFixtureClock(t, scenario)
	return r, s
}

// foldFixtureClock is foldFixture for the tests that drive a deadline,
// handing back the fake clock the runner was built on.
func foldFixtureClock(t *testing.T, scenario string) (*runner, provider.Stream, *clock.Fake) {
	t.Helper()
	return foldFixtureClockCtx(t, scenario, context.Background())
}

// foldFixtureClockCtx is foldFixtureClock with the caller's turn
// context, for the tests that cancel the turn (a Stop click).
func foldFixtureClockCtx(t *testing.T, scenario string, turnCtx context.Context) (*runner, provider.Stream, *clock.Fake) {
	t.Helper()
	withFakeCLIEnv(t, scenario)
	opts := helperOpts(t, scenario, &memSessionStore{})
	clk := clock.NewFake()
	opts.clock = clk

	c := New(opts)
	t.Cleanup(func() {
		if c.registry != nil {
			_ = c.registry.Close()
		}
	})

	req := provider.CompleteRequest{
		Agent: "fold-agent",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.ContentBlock{{Type: provider.ContentText, Text: "go"}}},
		},
	}
	reg, err := c.ensureRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	r, err := reg.Acquire(context.Background(), req.Agent, "", req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	s, err := r.RunTurn(turnCtx, req)
	if err != nil {
		t.Fatalf("run turn: %v", err)
	}
	return r, s, clk
}

// TestFoldLandsInRunningTurn is the headline test: a message handed to
// a turn that is ALREADY RUNNING is consumed by that same turn. One
// turn, not two — which is the entire difference between folding and
// the stop-and-reprompt it replaces.
//
// SendFold is called from inside the event drain, on the tool_result
// seam, so there is no timing assumption about when the fold lands.
func TestFoldLandsInRunningTurn(t *testing.T) {
	r, s := foldFixture(t, "fold-landed")

	var foldID string
	var folded []provider.StreamEvent
	for ev := range s.Events() {
		switch ev.Kind {
		case provider.StreamToolResult:
			id, err := s.(provider.Folder).SendFold("also do the other thing")
			if err != nil {
				t.Errorf("SendFold: %v", err)
			}
			foldID = id
		case provider.StreamMessageFolded:
			folded = append(folded, ev)
		}
	}

	if foldID == "" {
		t.Fatal("SendFold returned no uuid; nothing was written to the CLI")
	}
	if len(folded) != 1 {
		t.Fatalf("got %d StreamMessageFolded events, want exactly 1: %+v", len(folded), folded)
	}
	if folded[0].FoldUUID != foldID {
		t.Errorf("fold event uuid = %q, want %q", folded[0].FoldUUID, foldID)
	}
	if !folded[0].FoldLanded {
		t.Error("FoldLanded = false; the running turn named this uuid in user_message_uuids, so it landed")
	}
	if err := s.Err(); err != nil {
		t.Errorf("stream.Err() = %v; a fold must not end or fail the turn", err)
	}

	// The turn was never stopped, so the subprocess is untouched and
	// the per-turn fold bookkeeping is clean for the next turn.
	if r.isDead() {
		t.Error("runner died; folding must not kill the subprocess")
	}
	r.mu.Lock()
	leaked := len(r.pendingFolds)
	r.mu.Unlock()
	if leaked != 0 {
		t.Errorf("pendingFolds leaked %d entries into the next turn", leaked)
	}
}

// TestFoldMissedIsAbsorbedIntoTheSameTurn: a message folded in near
// the END of a turn — after the last tool round, so there is no seam —
// is answered by the CLI in a continuation turn of its own (measured
// against the real binary with no tool round in the turn). The
// runner keeps that continuation on the same turn rather than dropping
// its frames as stray events and letting core re-deliver the text.
//
// So the assertions here are: ONE stream, the continuation's text
// carried on it, and the fold reported as LANDED — because it was.
func TestFoldMissedIsAbsorbedIntoTheSameTurn(t *testing.T) {
	_, s := foldFixture(t, "fold-missed")

	var foldID string
	var folded []provider.StreamEvent
	var deltas []string
	ends := 0
	for ev := range s.Events() {
		switch ev.Kind {
		case provider.StreamToolResult:
			id, err := s.(provider.Folder).SendFold("too late for this turn")
			if err != nil {
				t.Errorf("SendFold: %v", err)
			}
			foldID = id
		case provider.StreamMessageFolded:
			folded = append(folded, ev)
		case provider.StreamDelta:
			deltas = append(deltas, ev.Text)
		case provider.StreamEnd:
			ends++
		}
	}

	if len(folded) != 1 {
		t.Fatalf("got %d StreamMessageFolded events, want exactly 1: %+v", len(folded), folded)
	}
	if folded[0].FoldUUID != foldID {
		t.Errorf("fold event uuid = %q, want %q", folded[0].FoldUUID, foldID)
	}
	if !folded[0].FoldLanded {
		t.Error("FoldLanded = false, but the CLI's continuation started answering the message")
	}
	if ends != 1 {
		t.Errorf("got %d StreamEnd events, want 1: the continuation is part of this turn, not a second one", ends)
	}
	joined := strings.Join(deltas, "|")
	if !strings.Contains(joined, "answered in my own turn") {
		t.Errorf("continuation text missing from the stream; deltas = %q", joined)
	}
	if err := s.Err(); err != nil {
		t.Errorf("stream.Err() = %v; absorbing a continuation is not a failure", err)
	}
}

// TestFoldMissedContinuationCostIsTheGauge: two result frames, two CLI
// invocations, both billed by Anthropic — and total_cost_usd on the
// second frame already contains the first, because it is the CLI's
// session-cumulative gauge. The turn's cost is the last gauge, counted
// once; summing the frames would bill the first invocation twice.
func TestFoldMissedContinuationCostIsTheGauge(t *testing.T) {
	_, s := foldFixture(t, "fold-missed")
	for ev := range s.Events() {
		if ev.Kind == provider.StreamToolResult {
			if _, err := s.(provider.Folder).SendFold("too late for this turn"); err != nil {
				t.Errorf("SendFold: %v", err)
			}
		}
	}
	final := s.Final()
	if final == nil {
		t.Fatal("Final() = nil")
	}
	// The fake's frames read fakeResultCostUSD then 2*fakeResultCostUSD.
	if want := 2 * fakeResultCostUSD; final.CostUSD != want {
		t.Errorf("CostUSD = %v, want %v (the gauge after both frames, not their sum)", final.CostUSD, want)
	}
	// Tokens are the other way round: each frame's usage is its own
	// total, so the absorbed turn's usage is the two frames added —
	// and the per-model gauge, cumulative like the cost, has been
	// differenced back to the same figure rather than double-counted.
	if want := fakeFoldFrame1Usage.Add(fakeFoldFrame2Usage); final.Usage != want {
		t.Errorf("Usage = %+v, want %+v (both frames' own totals, added)", final.Usage, want)
	}
	if final.ByModel != nil {
		t.Errorf("ByModel = %+v, want nil: one model answered both frames", final.ByModel)
	}
}

// TestFoldInterruptedWhileAbsorbingEndsTheTurnWarm: a Stop click that
// lands while the turn is held open for a continuation the CLI has not
// started. There is nothing running to interrupt and no result frame
// is coming, so the runner ends the turn itself as a cancel, reports
// the fold missed (cancel_queued handed the message back), and keeps
// the subprocess alive — the generic escalation would have found no
// result and killed it.
func TestFoldInterruptedWhileAbsorbingEndsTheTurnWarm(t *testing.T) {
	turnCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, s, clk := foldFixtureClockCtx(t, "fold-orphaned", turnCtx)

	type outcome struct {
		folded []provider.StreamEvent
		ends   int
	}
	done := make(chan outcome, 1)
	sent := make(chan string, 1)
	sendErr := make(chan error, 1)
	go func() {
		var got outcome
		for ev := range s.Events() {
			switch ev.Kind {
			case provider.StreamToolResult:
				id, err := s.(provider.Folder).SendFold("stop will land before any continuation")
				sendErr <- err
				sent <- id
			case provider.StreamMessageFolded:
				got.folded = append(got.folded, ev)
			case provider.StreamEnd:
				got.ends++
			}
		}
		done <- got
	}()
	if err := <-sendErr; err != nil {
		t.Fatalf("SendFold: %v", err)
	}
	foldID := <-sent

	// The absorb timer registering on the clock is the signal that the
	// result has been routed and the turn is now held open.
	clk.BlockUntil(1)
	cancel()

	got := <-done
	if len(got.folded) != 1 || got.folded[0].FoldUUID != foldID || got.folded[0].FoldLanded {
		t.Fatalf("fold events = %+v, want one miss for %s", got.folded, foldID)
	}
	if got.ends != 1 {
		t.Errorf("got %d StreamEnd events, want 1", got.ends)
	}
	if err := s.Err(); !errors.Is(err, provider.ErrUserCancelled) {
		t.Errorf("stream.Err() = %v, want ErrUserCancelled", err)
	}
	select {
	case <-r.dead:
		t.Error("the subprocess was killed for a Stop that had nothing running to stop")
	default:
	}
	r.mu.Lock()
	absorbing, active := len(r.absorbing), r.active
	r.mu.Unlock()
	if absorbing != 0 || active != nil {
		t.Errorf("runner still holds absorb state (%d) or an active turn (%v)", absorbing, active != nil)
	}
}

// TestFoldOrphanedReportsMissAfterDeadline is the bound on absorption.
// The CLI always started the continuation in measurement, but "hold
// the turn open until a frame that may never arrive" is not a state to
// leave unbounded. When the deadline passes the runner reports the
// miss and core's fallback re-delivers.
func TestFoldOrphanedReportsMissAfterDeadline(t *testing.T) {
	_, s, clk := foldFixtureClock(t, "fold-orphaned")

	type outcome struct {
		folded []provider.StreamEvent
		ends   int
	}
	done := make(chan outcome, 1)
	sent := make(chan string, 1)
	sendErr := make(chan error, 1)
	go func() {
		var got outcome
		for ev := range s.Events() {
			switch ev.Kind {
			case provider.StreamToolResult:
				// The seam, same as every other fold test: the fake
				// is parked on stdin here, so the turn is live.
				id, err := s.(provider.Folder).SendFold("nothing will ever run this")
				sendErr <- err
				sent <- id
			case provider.StreamMessageFolded:
				got.folded = append(got.folded, ev)
			case provider.StreamEnd:
				got.ends++
			}
		}
		done <- got
	}()

	if err := <-sendErr; err != nil {
		t.Fatalf("SendFold: %v", err)
	}
	foldID := <-sent

	// The absorb timer is the only thing left waiting on the clock by
	// the time the fake's lone result frame has been routed.
	clk.BlockUntil(1)
	clk.Advance(foldAbsorbDeadline)

	got := <-done
	if len(got.folded) != 1 {
		t.Fatalf("got %d StreamMessageFolded events, want exactly 1: %+v", len(got.folded), got.folded)
	}
	if got.folded[0].FoldUUID != foldID {
		t.Errorf("fold event uuid = %q, want %q", got.folded[0].FoldUUID, foldID)
	}
	if got.folded[0].FoldLanded {
		t.Error("FoldLanded = true, but no continuation ever claimed the message")
	}
	if got.ends != 1 {
		t.Errorf("got %d StreamEnd events, want 1", got.ends)
	}
}

// TestFoldRefusedWithoutCapability pins the feature-detection rule: a
// CLI that does not advertise msg_lifecycle_v1 cannot report the fold
// seam, so we refuse to write rather than fold blind. The caller then
// falls back to ending the turn, which is what it did before folds
// existed.
//
// "tooluse" is any scenario whose init carries no capabilities array.
func TestFoldRefusedWithoutCapability(t *testing.T) {
	_, s := foldFixture(t, "tooluse")

	var sendErr error
	var attempted bool
	for ev := range s.Events() {
		if ev.Kind == provider.StreamToolResult && !attempted {
			attempted = true
			_, sendErr = s.(provider.Folder).SendFold("should not reach the CLI")
		}
		if ev.Kind == provider.StreamMessageFolded {
			t.Error("got a StreamMessageFolded event from a CLI that cannot fold")
		}
	}
	if !attempted {
		t.Fatal("never reached a tool_result seam to attempt the fold from")
	}
	if !errors.Is(sendErr, errFoldUnsupported) {
		t.Fatalf("SendFold error = %v, want errFoldUnsupported", sendErr)
	}
}

// TestFoldRefusedWhileWindingUp: once an interrupt is armed the turn is
// on its way out. Folding into it would race the abort — the CLI could
// consume the message into a turn we are about to cancel, and we would
// then have handed it over AND re-delivered it ourselves.
func TestFoldRefusedWhileWindingUp(t *testing.T) {
	r, s := foldFixture(t, "fold-landed")
	defer func() { _ = s.Close() }()

	// Arm from inside the drain, at the tool_result seam. Reaching the
	// seam proves the init frame has already been routed, so the
	// capability is on record and the refusal under test is the only
	// one that can fire. The fake is parked on stdin at this point, so
	// the turn stays live while we assert; we break out rather than
	// draining to completion because no fold is coming to unblock it.
	var sendErr error
	var armed bool
	for ev := range s.Events() {
		if ev.Kind != provider.StreamToolResult {
			continue
		}
		armed = true
		r.mu.Lock()
		r.interruptArmed = true
		r.mu.Unlock()
		_, sendErr = s.(provider.Folder).SendFold("too late")
		break
	}
	if !armed {
		t.Fatal("never reached a tool_result seam")
	}
	if !errors.Is(sendErr, errFoldWinding) {
		t.Fatalf("SendFold error = %v, want errFoldWinding", sendErr)
	}
	r.mu.Lock()
	pending := len(r.pendingFolds)
	r.mu.Unlock()
	if pending != 0 {
		t.Errorf("a refused fold registered %d pending entries; nothing was written", pending)
	}
}

// TestTurnReleaseCarriesUUID guards the one-line change that made mid-turn
// delivery observable at all. The CLI emits NO command_lifecycle frames
// for a message sent without a uuid, which is precisely why the earlier
// measurement had to fall back to "did the model obey" — a statement
// about the model, not about delivery.
func TestTurnReleaseCarriesUUID(t *testing.T) {
	r, s := foldFixture(t, "fold-landed")

	for ev := range s.Events() {
		if ev.Kind == provider.StreamToolResult {
			if _, err := s.(provider.Folder).SendFold("second message"); err != nil {
				t.Errorf("SendFold: %v", err)
			}
		}
	}

	body, err := os.ReadFile(filepath.Join(r.dir, "stdin-mirror.jsonl"))
	if err != nil {
		t.Fatalf("read stdin mirror: %v", err)
	}
	var userLines int
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if !strings.Contains(line, `"type":"user"`) {
			continue
		}
		userLines++
		if !strings.Contains(line, `"uuid"`) {
			t.Errorf("user message written without a uuid, so the CLI reports no lifecycle for it:\n%s", line)
		}
	}
	if userLines != 2 {
		t.Fatalf("wrote %d user lines to stdin, want 2 (the release and the fold)", userLines)
	}
}
