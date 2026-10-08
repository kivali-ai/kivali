package tracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
)

type fixture struct {
	t   *testing.T
	s   *store.FSStore
	m   *messaging.Messenger
	svc *Service
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.Agent{
		{Slug: "ceo", Role: "CEO"},
		{Slug: "cos", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "cos"},
		{Slug: "bob", Role: "Engineer", ReportsTo: "cos"},
	} {
		if err := s.CreateAgent(a, "# role\n"); err != nil {
			t.Fatalf("seed %s: %v", a.Slug, err)
		}
	}
	f := &fixture{t: t, s: s, m: messaging.New(s, nil), now: time.Date(2026, 9, 21, 18, 0, 0, 0, time.UTC)}
	f.svc = New(s, f.m, messaging.DeliveryHooks{})
	f.svc.Now = func() time.Time { f.now = f.now.Add(time.Second); return f.now }
	return f
}

func (f *fixture) queued(slug string) []string {
	f.t.Helper()
	q, err := f.s.ReadMessageQueue()
	if err != nil {
		f.t.Fatal(err)
	}
	return q.Agents[slug].Inbox
}

func (f *fixture) chat(slug string) []store.ChatMessage {
	f.t.Helper()
	hist, err := f.s.ReadChatHistory(slug)
	if err != nil {
		f.t.Fatal(err)
	}
	return hist
}

func (f *fixture) message(rel string) store.Message {
	f.t.Helper()
	m, err := f.s.ReadMessage(filepath.Join(f.s.Root(), rel))
	if err != nil {
		f.t.Fatalf("read %s: %v", rel, err)
	}
	return m
}

func (f *fixture) noPending() {
	f.t.Helper()
	p, err := f.s.ListPendingWakes()
	if err != nil {
		f.t.Fatal(err)
	}
	if len(p) != 0 {
		f.t.Fatalf("pending wakes left: %+v", p)
	}
}

func TestAgentChangeQueuesForRelease(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Draft the release notes", Body: "Do it.", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if ch.Assignment.ID != 1 {
		t.Fatalf("id = %d", ch.Assignment.ID)
	}
	if _, err := f.s.ReadAssignment(1); err != nil {
		t.Fatalf("assignment file: %v", err)
	}
	// The wake waits in alice's release queue; nothing reached her chat.
	q := f.queued("alice")
	if len(q) != 1 || !strings.Contains(q[0], "-assignment_event-cos--to--alice.md") {
		t.Fatalf("alice queue = %v", q)
	}
	if got := f.chat("alice"); len(got) != 0 {
		t.Fatalf("alice chat = %d entries before release", len(got))
	}
	m := f.message(q[0])
	if m.Type != store.MsgAssignmentEvent || m.Assignment == nil || m.Assignment.ID != 1 || m.Assignment.Op != "created" {
		t.Fatalf("queued message = %+v", m)
	}
	if m.Title != "#1 assigned to you: Draft the release notes" {
		t.Fatalf("title = %q", m.Title)
	}
	f.noPending()

	// Release delivers the event and nothing else: what alice holds is
	// the wake note's business (agent.BuildWakeUpdate), once per wake.
	if _, err := f.m.ReleaseAll(ctx, nil, f.svc.Hooks); err != nil {
		t.Fatal(err)
	}
	hist := f.chat("alice")
	if len(hist) != 1 || hist[0].Kind != "inbox_delivery" || hist[0].Role != store.RoleReceived {
		t.Fatalf("alice chat after release = %+v", hist)
	}
	for _, want := range []string{
		"The assignment tracker reports a change to assignment #1, made by cos.",
		"cos assigned you #1 \"Draft the release notes\".",
		"It is ready: nothing blocks it.",
		"Do it.",
	} {
		if !strings.Contains(hist[0].Content, want) {
			t.Errorf("delivered content missing %q:\n%s", want, hist[0].Content)
		}
	}
	if strings.Contains(hist[0].Content, "open assignments") {
		t.Errorf("the delivery repeats what alice holds:\n%s", hist[0].Content)
	}
}

func TestCEOChangeDeliversAtOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, "ceo", assignments.CreateInput{Title: "From the top", Body: "Now.", Assignee: "bob"}); err != nil {
		t.Fatal(err)
	}
	if q := f.queued("bob"); len(q) != 0 {
		t.Fatalf("CEO change was queued: %v", q)
	}
	hist := f.chat("bob")
	if len(hist) != 1 || !strings.Contains(hist[0].Content, "ceo assigned you #1 \"From the top\".") {
		t.Fatalf("bob chat = %+v", hist)
	}
	f.noPending()
}

func TestQuestionToTheCEOLandsInTheirInbox(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Work", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	qc, err := f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "Budget?", Body: "How much may I spend?", Assignee: "ceo", Parent: 1})
	if err != nil {
		t.Fatal(err)
	}
	q := qc.Assignment
	hist := f.chat("ceo")
	if len(hist) != 1 || hist[0].Kind != "ceo_inbox" {
		t.Fatalf("ceo chat = %+v", hist)
	}
	m := f.message(hist[0].MessageRef)
	if m.Assignment == nil || m.Assignment.ID != q.ID || !m.DeliversToCEO() {
		t.Fatalf("ceo-bound event = %+v", m)
	}
	// The CEO answers by closing; alice's wake is instant and says #1
	// is ready again.
	if _, err := f.svc.Close(ctx, "ceo", q.ID, assignments.ResolutionDone, "$40k."); err != nil {
		t.Fatal(err)
	}
	ah := f.chat("alice")
	// The assignment itself is still queued (cos's change); only the
	// CEO's answer is in her chat.
	if len(ah) != 1 || !strings.Contains(ah[0].Content, "Outcome: $40k.") || !strings.Contains(ah[0].Content, "#1 \"Work\" is now ready") {
		t.Fatalf("alice chat = %+v", ah)
	}
	f.noPending()
}

func TestDropPrunesTheQueuedAssignment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Never mind", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	queuedPath := f.queued("alice")[0]
	if _, err := f.svc.Close(ctx, "cos", ch.Assignment.ID, assignments.ResolutionDropped, "Overtaken."); err != nil {
		t.Fatal(err)
	}
	// The assignment is gone from the queue and relocated, and alice,
	// who never saw it, is not told to stop work she never started:
	// her queue is empty and nothing is pending.
	if q := f.queued("alice"); len(q) != 0 {
		t.Fatalf("alice queue after drop = %v; she never knew about the assignment", q)
	}
	if _, err := os.Stat(filepath.Join(f.s.Root(), queuedPath)); !os.IsNotExist(err) {
		t.Fatalf("pruned assignment still in the live ledger: %v", err)
	}
	f.noPending()
}

// Once the assignment has reached the assignee, a drop is news to
// them and the wake says so.
func TestDropAfterReleaseTellsTheAssigneeToStop(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Never mind", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ReleaseAll(ctx, nil, f.svc.Hooks); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Close(ctx, "cos", ch.Assignment.ID, assignments.ResolutionDropped, "Overtaken."); err != nil {
		t.Fatal(err)
	}
	q := f.queued("alice")
	if len(q) != 1 {
		t.Fatalf("alice queue after drop = %v", q)
	}
	if m := f.message(q[0]); m.Assignment.Op != "closed" || !strings.Contains(m.Body, "Stop work on it.") {
		t.Fatalf("queued drop = %+v", m)
	}
}

// A done close prunes a still-queued assignment the same way a drop
// does: the assignment is closed, so the assignment is history whichever
// way it ended.
func TestDoneClosePrunesTheQueuedAssignment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Did it myself", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	queuedPath := f.queued("alice")[0]
	if _, err := f.svc.Close(ctx, assignments.CEO, ch.Assignment.ID, assignments.ResolutionDone, "Done by the CEO."); err != nil {
		t.Fatal(err)
	}
	if q := f.queued("alice"); len(q) != 0 {
		t.Fatalf("alice queue after a done close = %v", q)
	}
	if _, err := os.Stat(filepath.Join(f.s.Root(), queuedPath)); !os.IsNotExist(err) {
		t.Fatalf("pruned assignment still in the live ledger: %v", err)
	}
	// The creator still hears about it, at once, since the CEO closed it.
	if hist := f.chat("cos"); len(hist) != 1 || !strings.Contains(hist[0].Content, "ceo closed #1 \"Did it myself\" as done.") {
		t.Fatalf("cos chat = %+v", hist)
	}
	f.noPending()
}

func TestReassignPrunesOnlyTheOldAssignee(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Hand-off", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	to := "bob"
	if _, err := f.svc.Update(ctx, "cos", ch.Assignment.ID, assignments.UpdateInput{Assignee: &to, Note: "bob owns it"}); err != nil {
		t.Fatal(err)
	}
	aq, bq := f.queued("alice"), f.queued("bob")
	// alice's assignment was still queued, so it is pulled and she is
	// not told to stop work she never knew about.
	if len(aq) != 0 {
		t.Fatalf("alice queue = %v", aq)
	}
	if len(bq) != 1 || !strings.Contains(f.message(bq[0]).Body, "to you (from alice)") {
		t.Fatalf("bob queue = %v", bq)
	}
	f.noPending()

	// Hand it back after bob has seen it: now bob is told to stop.
	if _, err := f.m.ReleaseAll(ctx, nil, f.svc.Hooks); err != nil {
		t.Fatal(err)
	}
	back := "alice"
	if _, err := f.svc.Update(ctx, "cos", ch.Assignment.ID, assignments.UpdateInput{Assignee: &back, Note: "alice after all"}); err != nil {
		t.Fatal(err)
	}
	bq = f.queued("bob")
	if len(bq) != 1 || f.message(bq[0]).Assignment.Op != "assigned" || !strings.Contains(f.message(bq[0]).Body, "from you to alice") {
		t.Fatalf("bob queue after the hand-back = %v", bq)
	}
}

func TestRefusalsPassThrough(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "x", Assignee: "nobody"})
	if !assignments.IsRefusal(err) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.svc.Close(ctx, "alice", 9, assignments.ResolutionDone, "x"); !assignments.IsRefusal(err) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := f.s.ListAssignments(); len(got) != 0 {
		t.Fatal("a refused create wrote a file")
	}
}

func TestReconcileRoutesWhatACrashLeft(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "Crashy", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between the write and the route: put the
	// record back, with one wake to an agent that has since gone.
	p := store.PendingWakes{Seq: 1, Assignment: ch.Assignment.ID, By: "cos", At: f.now, Wakes: []assignments.Wake{
		{To: "alice", Op: assignments.OpCreated, Title: "#1 assigned to you: Crashy", Body: "again"},
		{To: "gone", Op: assignments.OpCreated, Title: "#1 assigned to you: Crashy", Body: "never"},
	}}
	if err := f.s.WritePendingWakes(p); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	f.noPending()
	// The replay wrote the same path as the original, and the queue
	// holds it once.
	if q := f.queued("alice"); len(q) != 1 {
		t.Fatalf("alice queue after reconcile = %v", q)
	}
}

func TestAmendSnapshotsThePriorText(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "T", Body: "v1", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	body := "v2"
	got, err := f.svc.Update(ctx, "cos", ch.Assignment.ID, assignments.UpdateInput{Body: &body, Note: "sharper"})
	if err != nil {
		t.Fatal(err)
	}
	last := got.Assignment.Log[len(got.Assignment.Log)-1]
	if last.Op != assignments.OpAmended || last.Prior == "" {
		t.Fatalf("entry = %+v", last)
	}
	if _, err := os.Stat(filepath.Join(f.s.Root(), "attachments", last.Prior)); err != nil {
		t.Fatalf("prior text not snapshotted under attachments/%s: %v", last.Prior, err)
	}
}

func TestReassignAllForOffboard(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, title := range []string{"A", "B"} {
		if _, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: title, Assignee: "alice"}); err != nil {
			t.Fatal(err)
		}
	}
	done, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "C", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Close(ctx, "alice", done.Assignment.ID, assignments.ResolutionDone, "ok"); err != nil {
		t.Fatal(err)
	}
	// alice also filed asks of her own: one bob still holds, one bob
	// already closed.
	asked, err := f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "D", Assignee: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	answered, err := f.svc.Create(ctx, "alice", assignments.CreateInput{Title: "E", Assignee: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Close(ctx, "bob", answered.Assignment.ID, assignments.ResolutionDone, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ReleaseAll(ctx, nil, f.svc.Hooks); err != nil {
		t.Fatal(err)
	}
	bobBefore := len(f.chat("bob"))
	cosBefore := len(f.chat("cos"))

	n, err := f.svc.ReassignAll(ctx, "alice", "cos", "alice was offboarded")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("moved %d, want 2 held + 1 filed", n)
	}
	set, _ := f.svc.Set()
	if got := set.Query(assignments.Filter{Assignee: "cos"}); len(got) != 2 {
		t.Fatalf("cos now holds %d", len(got))
	}
	// The open ask alice filed is cos's to follow now; the closed one
	// keeps its history as it was.
	if iss, _ := set.Get(asked.Assignment.ID); iss.Creator != "cos" || iss.Log[len(iss.Log)-1].Op != assignments.OpCreator {
		t.Fatalf("open ask after offboard = %+v", iss)
	}
	if iss, _ := set.Get(answered.Assignment.ID); iss.Creator != "alice" {
		t.Fatalf("closed ask should keep its creator: %+v", iss)
	}
	// The CEO made the changes, so cos was told at once: twice for the
	// assignments it now holds, once for the ask it now follows. bob, who
	// holds that ask, has nothing new to do and is not woken.
	hist := f.chat("cos")[cosBefore:]
	if len(hist) != 3 || !strings.Contains(hist[0].Content, "alice was offboarded") || !strings.Contains(hist[2].Content, "made you the creator of #4 \"D\"") {
		t.Fatalf("cos chat = %+v", hist)
	}
	if got := len(f.chat("bob")); got != bobBefore {
		t.Fatalf("bob was woken %d time(s) for a creator move", got-bobBefore)
	}
	f.noPending()

	// Later bob closes the ask: the wake goes to its new creator, not
	// into the void.
	if _, err := f.svc.Close(ctx, "bob", asked.Assignment.ID, assignments.ResolutionDone, "shipped"); err != nil {
		t.Fatal(err)
	}
	if q := f.queued("cos"); len(q) != 1 || f.message(q[0]).Assignment.Op != "closed" {
		t.Fatalf("cos queue after bob's close = %v", q)
	}
}

// Reopening an assignment whose assignee has since been archived does not
// hand it back to a ghost: the reopener holds it, with a log entry
// saying why, and hands it on with assignment_update.
func TestReopenOntoAnArchivedAssigneeHandsItToTheReopener(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ch, err := f.svc.Create(ctx, "cos", assignments.CreateInput{Title: "v3", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Close(ctx, "alice", ch.Assignment.ID, assignments.ResolutionDone, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ArchiveAgent("alice"); err != nil {
		t.Fatal(err)
	}
	re, err := f.svc.Reopen(ctx, "cos", ch.Assignment.ID, "the phase check fails")
	if err != nil {
		t.Fatal(err)
	}
	if re.Assignment.Assignee != "cos" {
		t.Fatalf("assignee after reopen = %s, want the reopener", re.Assignment.Assignee)
	}
	last := re.Assignment.Log[len(re.Assignment.Log)-1]
	if last.Op != assignments.OpAssigned || last.From != "alice" || last.To != "cos" || !strings.Contains(last.Note, "no longer an active agent") {
		t.Fatalf("last log entry = %+v", last)
	}
	if len(re.Wakes) != 0 {
		t.Fatalf("wakes = %+v; the reopener holds it, nobody else is told", re.Wakes)
	}
	f.noPending()
}
