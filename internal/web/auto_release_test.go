package web

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// autoReleaseHarness is a turn server on a fake clock with one
// recipient, bob, and a channel that sees every message-queue write.
// The queue write is the one observable every route and every release
// goes through, so it is what the tests wait on instead of sleeping.
//
// The scheduler is NOT started here; tests that want the loop start
// it, which is what lets the pass be asserted on its own.
type autoReleaseHarness struct {
	srv    *Server
	fake   *clock.Fake
	writes chan struct{}
}

func newAutoReleaseHarness(t *testing.T) *autoReleaseHarness {
	t.Helper()
	srv, _ := newTurnServer(t)
	// Delivery is what is under test, not the reply loop:
	// spawnChatLoopIfIdle short-circuits when Claude is nil.
	srv.Claude = nil
	h := &autoReleaseHarness{srv: srv, fake: clock.NewFake(), writes: make(chan struct{}, 64)}
	srv.Clock = h.fake
	if err := srv.Store.CreateAgent(store.Agent{Slug: "bob", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	srv.Store.SetOnMessageQueueWrite(func() {
		srv.onMessageQueueWrite()
		h.writes <- struct{}{}
	})
	return h
}

// enqueue writes a notice sent at sentAt and queues it for to. Goes
// through WriteMessageQueue so the post-write hook fires exactly as a
// real publish's commit does; the hook's write signal is consumed
// here so a test's next awaitWrite is the release, not the enqueue.
func (h *autoReleaseHarness) enqueue(t *testing.T, title, to string, sentAt time.Time) string {
	t.Helper()
	abs, err := h.srv.Store.WriteMessage(store.Message{
		Type: store.MsgNotice, Title: title, From: "alice", To: store.Recipients{to},
		Date: sentAt, Body: "body of " + title,
	})
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	rel, _ := filepath.Rel(h.srv.Store.Root(), abs)
	h.srv.Store.LockMessageQueue()
	q, _ := h.srv.Store.ReadMessageQueue()
	rt := q.Agents[to]
	rt.Inbox = append(rt.Inbox, rel)
	q.Agents[to] = rt
	err = h.srv.Store.WriteMessageQueue(q)
	h.srv.Store.UnlockMessageQueue()
	if err != nil {
		t.Fatalf("WriteMessageQueue: %v", err)
	}
	h.awaitWrite(t, "enqueue")
	return rel
}

func (h *autoReleaseHarness) setDelay(t *testing.T, a store.AutoRelease) {
	t.Helper()
	if err := h.srv.Store.WriteAutoRelease(a); err != nil {
		t.Fatalf("WriteAutoRelease: %v", err)
	}
}

// awaitWrite blocks until the next message-queue write.
func (h *autoReleaseHarness) awaitWrite(t *testing.T, what string) {
	t.Helper()
	select {
	case <-h.writes:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no message-queue write within 5s", what)
	}
}

// delivered lists the message paths bob has received as inbox
// deliveries, in chat order.
func (h *autoReleaseHarness) delivered(t *testing.T) []string {
	t.Helper()
	hist, err := h.srv.Store.ReadChatHistory("bob")
	if err != nil {
		t.Fatalf("read bob chat: %v", err)
	}
	var out []string
	for _, m := range hist {
		if m.Kind == "inbox_delivery" {
			out = append(out, m.MessageRef)
		}
	}
	return out
}

func (h *autoReleaseHarness) startLoop(t *testing.T) {
	t.Helper()
	stop := h.srv.StartAutoRelease()
	t.Cleanup(stop)
}

// A pass releases exactly what is overdue, measured from the send
// time, and reports when the next message is due so the loop can
// sleep until then rather than poll.
func TestAutoReleasePassReleasesOverdueOnly(t *testing.T) {
	h := newAutoReleaseHarness(t)
	now := h.fake.Now()
	h.setDelay(t, store.AutoRelease{Enabled: true, Delay: 2 * time.Minute})
	old := h.enqueue(t, "old", "bob", now.Add(-3*time.Minute))
	fresh := h.enqueue(t, "fresh", "bob", now.Add(-time.Minute))

	wait, ok := h.srv.autoReleasePass()
	if !ok || wait != time.Minute {
		t.Fatalf("pass = (%s, %v), want (1m0s, true): the fresh message is due in a minute", wait, ok)
	}
	if got := h.delivered(t); len(got) != 1 || got[0] != old {
		t.Fatalf("delivered %v, want only %s", got, old)
	}
	if got := queuedFor(t, h.srv, "bob"); len(got) != 1 || got[0] != fresh {
		t.Fatalf("queued %v, want only %s", got, fresh)
	}

	// Off: nothing is due and nothing is waiting, however old.
	h.setDelay(t, store.AutoRelease{})
	h.fake.Advance(time.Hour)
	if wait, ok := h.srv.autoReleasePass(); ok || wait != 0 {
		t.Fatalf("pass while off = (%s, %v), want (0, false)", wait, ok)
	}
	if got := queuedFor(t, h.srv, "bob"); len(got) != 1 {
		t.Fatalf("off released something: queued %v", got)
	}
}

// The loop arms one timer at the earliest deadline and releases when
// the clock reaches it — not before, and without anything else
// touching the queue in between.
func TestAutoReleaseFiresAtTheDeadline(t *testing.T) {
	h := newAutoReleaseHarness(t)
	h.setDelay(t, store.AutoRelease{Enabled: true, Delay: 2 * time.Minute})
	h.startLoop(t)

	rel := h.enqueue(t, "q3", "bob", h.fake.Now())
	h.fake.BlockUntil(1)
	if ds := h.fake.Deadlines(); len(ds) != 1 || !ds[0].Equal(h.fake.Now().Add(2*time.Minute)) {
		t.Fatalf("armed %v, want one timer at now+2m (%s)", ds, h.fake.Now().Add(2*time.Minute))
	}

	h.fake.Advance(2*time.Minute - time.Millisecond)
	if got := h.delivered(t); len(got) != 0 {
		t.Fatalf("released before the deadline: %v", got)
	}

	h.fake.Advance(time.Millisecond)
	h.awaitWrite(t, "release at the deadline")
	if got := h.delivered(t); len(got) != 1 || got[0] != rel {
		t.Fatalf("delivered %v, want %s", got, rel)
	}
	if got := queuedFor(t, h.srv, "bob"); len(got) != 0 {
		t.Fatalf("still queued after release: %v", got)
	}
	// Nothing left to wait for: the loop disarms rather than polling.
	waitForWaiters(t, h.fake, 0)
}

// Moving the slider governs what is sent from then on and nothing
// before it. A message that landed while the slider was off stays
// queued after it moves to a delay — however old it is, and however
// long the clock then runs — for the CEO to release by hand; only
// what lands after the move is on the clock.
func TestAutoReleaseSliderMoveLeavesTheQueuedAlone(t *testing.T) {
	h := newAutoReleaseHarness(t)
	h.startLoop(t)
	stale := h.enqueue(t, "stale", "bob", h.fake.Now().Add(-10*time.Minute))
	if got := h.delivered(t); len(got) != 0 {
		t.Fatalf("released while off: %v", got)
	}

	rr := queuePost(t, h.srv, "/api/v1/auto-release", apitypes.AutoReleaseRequest{Value: "5m"})
	if rr.Code != http.StatusOK {
		t.Fatalf("slider post: %d %s", rr.Code, rr.Body.String())
	}
	fresh := h.enqueue(t, "fresh", "bob", h.fake.Now())
	h.fake.BlockUntil(1)
	if ds := h.fake.Deadlines(); len(ds) != 1 || !ds[0].Equal(h.fake.Now().Add(5*time.Minute)) {
		t.Fatalf("armed %v, want one timer at now+5m for the fresh message only", ds)
	}
	if got := h.delivered(t); len(got) != 0 {
		t.Fatalf("the slider move released something: %v", got)
	}

	h.fake.Advance(5 * time.Minute)
	h.awaitWrite(t, "release of the fresh message")
	if got := h.delivered(t); len(got) != 1 || got[0] != fresh {
		t.Fatalf("delivered %v, want only %s", got, fresh)
	}
	if got := queuedFor(t, h.srv, "bob"); len(got) != 1 || got[0] != stale {
		t.Fatalf("queued %v, want the stale message still waiting for the CEO", got)
	}
	waitForWaiters(t, h.fake, 0)
}

// A shorter delay does not hurry what is already counting down: a
// message keeps the deadline it was given when it was sent, and only
// what is sent after the move gets the new one.
func TestAutoReleaseShorterDelayKeepsExistingDeadlines(t *testing.T) {
	h := newAutoReleaseHarness(t)
	h.startLoop(t)
	if rr := queuePost(t, h.srv, "/api/v1/auto-release", apitypes.AutoReleaseRequest{Value: "20m"}); rr.Code != http.StatusOK {
		t.Fatalf("slider post: %d %s", rr.Code, rr.Body.String())
	}
	slow := h.enqueue(t, "slow", "bob", h.fake.Now())
	h.fake.BlockUntil(1)
	dueAt := h.fake.Now().Add(20 * time.Minute)
	if ds := h.fake.Deadlines(); len(ds) != 1 || !ds[0].Equal(dueAt) {
		t.Fatalf("armed %v, want one timer at now+20m", ds)
	}

	h.fake.Advance(time.Minute)
	if rr := queuePost(t, h.srv, "/api/v1/auto-release", apitypes.AutoReleaseRequest{Value: "now"}); rr.Code != http.StatusOK {
		t.Fatalf("slider post: %d %s", rr.Code, rr.Body.String())
	}
	quick := h.enqueue(t, "quick", "bob", h.fake.Now())
	h.awaitWrite(t, "release of the quick message")
	if got := h.delivered(t); len(got) != 1 || got[0] != quick {
		t.Fatalf("delivered %v, want only %s", got, quick)
	}
	if got := queuedFor(t, h.srv, "bob"); len(got) != 1 || got[0] != slow {
		t.Fatalf("queued %v, want the slow message still on its own clock", got)
	}
	// Its deadline is where it was.
	h.fake.BlockUntil(1)
	if ds := h.fake.Deadlines(); len(ds) != 1 || !ds[0].Equal(dueAt) {
		t.Fatalf("armed %v, want the slow message's original deadline %s", ds, dueAt)
	}
	h.fake.Advance(19 * time.Minute)
	h.awaitWrite(t, "release of the slow message")
	if got := h.delivered(t); len(got) != 2 || got[1] != slow {
		t.Fatalf("delivered %v, want %s last", got, slow)
	}
}

// Sliding to off cancels a pending deadline; the clock can then run
// as far as it likes without a release.
func TestAutoReleaseOffDisarms(t *testing.T) {
	h := newAutoReleaseHarness(t)
	h.setDelay(t, store.AutoRelease{Enabled: true, Delay: 2 * time.Minute})
	h.startLoop(t)
	h.enqueue(t, "q3", "bob", h.fake.Now())
	h.fake.BlockUntil(1)

	rr := queuePost(t, h.srv, "/api/v1/auto-release", apitypes.AutoReleaseRequest{Value: "off"})
	if rr.Code != http.StatusOK {
		t.Fatalf("slider post: %d", rr.Code)
	}
	waitForWaiters(t, h.fake, 0)
	h.fake.Advance(time.Hour)
	if got := h.delivered(t); len(got) != 0 {
		t.Fatalf("released while off: %v", got)
	}
	if got := queuedFor(t, h.srv, "bob"); len(got) != 1 {
		t.Fatalf("queue changed while off: %v", got)
	}
}

// A restart must not lose deadlines: they live in the message dates on
// disk, and the first pass at boot releases whatever came due while
// the server was down.
func TestAutoReleaseBootPassReleasesWhatWasOverdue(t *testing.T) {
	h := newAutoReleaseHarness(t)
	h.setDelay(t, store.AutoRelease{Enabled: true, Delay: 2 * time.Minute})
	rel := h.enqueue(t, "missed", "bob", h.fake.Now().Add(-5*time.Minute))

	h.startLoop(t)
	h.awaitWrite(t, "boot pass")
	if got := h.delivered(t); len(got) != 1 || got[0] != rel {
		t.Fatalf("delivered %v, want %s", got, rel)
	}
}

// A pointer queued for an agent who no longer exists is not due: the
// drain would leave it in place, and a pass that kept asking for it
// would re-run that no-op release on every queue write, forever.
func TestAutoReleaseSkipsPointersNobodyCanReceive(t *testing.T) {
	h := newAutoReleaseHarness(t)
	h.setDelay(t, store.AutoRelease{Enabled: true})
	h.enqueue(t, "orphan", "ghost", h.fake.Now().Add(-time.Hour))

	if wait, ok := h.srv.autoReleasePass(); ok || wait != 0 {
		t.Fatalf("pass = (%s, %v), want (0, false)", wait, ok)
	}
	select {
	case <-h.writes:
		t.Fatal("a pass with nothing deliverable wrote the queue")
	default:
	}
	if got := queuedFor(t, h.srv, "ghost"); len(got) != 1 {
		t.Fatalf("orphan pointer changed: %v", got)
	}
}

// Without an engine there is nothing to deliver with, so the start is
// a no-op stop — the same gate every release handler has.
func TestStartAutoReleaseWithoutEngineIsNoop(t *testing.T) {
	srv := newTestServer(t)
	stop := srv.StartAutoRelease()
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return")
	}
}

// waitForWaiters blocks until the fake clock has exactly n armed
// timers. Fake.BlockUntil waits for *at least* n, which cannot express
// "the timer was stopped", so poll Waiters for the exact count.
func waitForWaiters(t *testing.T, f *clock.Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.Waiters() == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("fake clock has %d armed timers, want %d", f.Waiters(), n)
}
