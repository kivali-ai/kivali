package web

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

var goalsNow = time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)

func openAssignment(id int, title, assignee string, parent int, blockedBy ...int) *assignments.Assignment {
	return &assignments.Assignment{ID: id, Title: title, Status: assignments.StatusOpen, Assignee: assignee, Creator: "chief-of-staff", Parent: parent, BlockedBy: blockedBy}
}

func closedAssignment(id int, title, assignee string, parent int, res assignments.Resolution, at time.Time) *assignments.Assignment {
	iss := openAssignment(id, title, assignee, parent)
	iss.Status, iss.Resolution, iss.Closed = assignments.StatusClosed, res, &at
	return iss
}

func TestGoalsAndAssignmentReadouts(t *testing.T) {
	// #1 goal counted by parts: #2 done, #3 dropped (left out), #4 and
	// #5 open. #5 waits on #20, outside the goal; #6 (under #4) waits
	// on #5, inside it. #4 also waits on its own part #6, which is
	// progress, not a blocker.
	// #10 goal with Done-when conditions: one met by #11, one claimed
	// by open #12, one unclaimed.
	// #20 is a top-level assignment with no parts and no conditions:
	// not a goal. #21 is held and blocked: not counted as blocked.
	// #30 closed nine days ago: not this week.
	goal := openAssignment(1, "Ship v3", "engineering-lead", 0)
	withItems := openAssignment(10, "Hire a CFO", "chief-of-staff", 0)
	withItems.Acceptance = []string{"shortlist", "offer", "onboarding"}
	met := closedAssignment(11, "Build the shortlist", "recruiter", 10, assignments.ResolutionDone, goalsNow.Add(-time.Hour))
	met.Satisfies = []string{"shortlist"}
	claimed := openAssignment(12, "Make the offer", "chief-of-staff", 10)
	claimed.Satisfies = []string{"offer"}
	held := openAssignment(21, "Paused thing", "engineering-lead", 0, 20)
	held.Held = true

	set := assignments.Build([]*assignments.Assignment{
		goal,
		closedAssignment(2, "Pick a vendor", "buyer", 1, assignments.ResolutionDone, goalsNow.Add(-48*time.Hour)),
		closedAssignment(3, "Dead end", "buyer", 1, assignments.ResolutionDropped, goalsNow.Add(-72*time.Hour)),
		openAssignment(4, "Schema", "dev", 1),
		openAssignment(5, "Run the tests", "tester", 1, 20),
		openAssignment(6, "Migration", "dev", 4, 5),
		withItems, met, claimed,
		openAssignment(20, "Fix the login redirect", "buyer", 0),
		held,
		closedAssignment(30, "Old", "buyer", 0, assignments.ResolutionDone, goalsNow.Add(-9*24*time.Hour)),
	})
	// Each blocker names who holds it: "buyer" by its display name,
	// "tester" (no name known) by its slug.
	names := map[string]string{"buyer": "Buyer"}
	person := func(slug string) apitypes.PersonRef { return personNamed(names, slug) }
	goals, blocked, closedWeek := goalsAndAssignmentReadouts(set, goalsNow, person)

	want := []apitypes.Goal{
		{
			ID: 1, Title: "Ship v3", Owner: "engineering-lead", Done: 1, Total: 3,
			Blocked: []apitypes.GoalBlocker{
				{ID: 5, On: 20, OnTitle: "Fix the login redirect", OnAssignee: &apitypes.PersonRef{Slug: "buyer", Name: "Buyer"}},
				{ID: 6, On: 5, OnTitle: "Run the tests", OnAssignee: &apitypes.PersonRef{Slug: "tester", Name: "tester"}},
			},
			Workers: []string{"dev", "tester"},
		},
		{
			ID: 10, Title: "Hire a CFO", Owner: "chief-of-staff", Done: 1, Total: 3,
			Blocked: []apitypes.GoalBlocker{},
			Workers: []string{"chief-of-staff"},
		},
	}
	if !reflect.DeepEqual(goals, want) {
		gotJSON, _ := json.MarshalIndent(goals, "", "  ")
		t.Errorf("goals =\n%s", gotJSON)
	}
	apitypes.NoNilSlices(t, goals)
	if blocked != 2 { // #5 and #6; #21 is on hold
		t.Errorf("blocked = %d, want 2", blocked)
	}
	if closedWeek != 3 { // #2, #3, #11
		t.Errorf("closed_week = %d, want 3", closedWeek)
	}
}

func TestGoalsEmptySet(t *testing.T) {
	goals, blocked, closedWeek := goalsAndAssignmentReadouts(assignments.Build(nil), goalsNow, func(slug string) apitypes.PersonRef { return personNamed(nil, slug) })
	if goals == nil || len(goals) != 0 || blocked != 0 || closedWeek != 0 {
		t.Errorf("goals %#v blocked %d closed %d", goals, blocked, closedWeek)
	}
}

func TestAgentState(t *testing.T) {
	cases := []struct {
		l    lifecycle
		want apitypes.AgentState
	}{
		{lifecycle{}, apitypes.AgentStateIdle},
		{lifecycle{Working: true}, apitypes.AgentStateRunning},
		{lifecycle{WaitingTasks: 2}, apitypes.AgentStateWaiting},
		{lifecycle{Interrupted: true}, apitypes.AgentStateWaiting},
		{lifecycle{Disconnected: true}, apitypes.AgentStateDisconnected},
		{lifecycle{Disconnected: true, WaitingTasks: 1}, apitypes.AgentStateWaiting},
		{lifecycle{NeedsHelp: true, Working: true}, apitypes.AgentStateNeedsHelp},
		{lifecycle{Quarantined: true, NeedsHelp: true}, apitypes.AgentStateQuarantined},
	}
	for _, c := range cases {
		if got := agentState(c.l); got != c.want {
			t.Errorf("agentState(%+v) = %q, want %q", c.l, got, c.want)
		}
	}
}

func TestAgentTreeOrder(t *testing.T) {
	got := agentTreeOrder([]store.Agent{
		{Slug: "ceo"},
		{Slug: "zed", ReportsTo: "ceo"},
		{Slug: "cos", ReportsTo: ""},
		{Slug: "b", ReportsTo: "cos"},
		{Slug: "a", ReportsTo: "cos"},
		{Slug: "a1", ReportsTo: "a"},
		{Slug: "lost", ReportsTo: "archived-boss"},
		{Slug: "loop1", ReportsTo: "loop2"},
		{Slug: "loop2", ReportsTo: "loop1"},
	})
	var order []string
	var depths []int
	for _, n := range got {
		order = append(order, n.Agent.Slug)
		depths = append(depths, n.Depth)
	}
	wantOrder := []string{"cos", "a", "a1", "b", "zed", "loop1", "loop2", "lost"}
	wantDepths := []int{1, 2, 3, 2, 1, 1, 2, 1}
	if !reflect.DeepEqual(order, wantOrder) || !reflect.DeepEqual(depths, wantDepths) {
		t.Errorf("order = %v depths = %v, want %v %v", order, depths, wantOrder, wantDepths)
	}
}

func validAssignment(id int, title string, parent int, acceptance ...string) *assignments.Assignment {
	iss := openAssignment(id, title, "alice", parent)
	iss.Acceptance = acceptance
	iss.Created, iss.Updated = goalsNow, goalsNow
	iss.Log = []assignments.Entry{{Seq: 1, TS: goalsNow, By: "chief-of-staff", Op: assignments.OpCreated}}
	return iss
}

// Every tracker write bumps assignments_version and the snapshot's goals
// follow it; the cached set is not served stale.
func TestSnapshotAssignmentsVersionAndGoals(t *testing.T) {
	srv := newTestServer(t)
	snap := srv.orgSnapshotValue()
	if snap.AssignmentsVersion != 0 || len(snap.Goals) != 0 {
		t.Fatalf("fresh snapshot: version %d goals %+v", snap.AssignmentsVersion, snap.Goals)
	}
	if err := srv.Store.WriteAssignment(validAssignment(1, "Launch", 0, "beta", "ga")); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap = srv.orgSnapshotValue()
	if snap.AssignmentsVersion != 1 {
		t.Errorf("assignments_version = %d after one write, want 1", snap.AssignmentsVersion)
	}
	if len(snap.Goals) != 1 || snap.Goals[0].Total != 2 || snap.Goals[0].Done != 0 {
		t.Fatalf("goals = %+v", snap.Goals)
	}
	if err := srv.Store.WriteAssignment(validAssignment(1, "Launch v2", 0, "beta", "ga")); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	snap = srv.orgSnapshotValue()
	if snap.AssignmentsVersion != 2 || snap.Goals[0].Title != "Launch v2" {
		t.Errorf("after second write: version %d goals %+v", snap.AssignmentsVersion, snap.Goals)
	}
}

// Spend is priced from token counts, never from cost_usd; "today"
// starts at 00:00 UTC; the cache is invalidated by an append.
func TestSnapshotSpendReadouts(t *testing.T) {
	srv := newTestServer(t)
	fake := clock.NewFakeAt(goalsNow)
	srv.Clock = fake
	model := provider.MockModelLarge
	perCall := func(in int) float64 {
		c := srv.Provider.Price(model, provider.TokenUsage{InputTokens: in})
		if c == 0 {
			t.Fatalf("%s has no price", model)
		}
		return c
	}
	rows := []store.UsageRecord{
		{TS: goalsNow.Add(-8 * 24 * time.Hour), Model: model, InputTokens: 1_000_000, CostUSD: 999},         // outside 7d
		{TS: goalsNow.Add(-3 * 24 * time.Hour), Model: model, InputTokens: 100_000, CostUSD: 999},           // this week
		{TS: goalsNow.Add(-16 * time.Hour), Model: model, InputTokens: 10_000, CostUSD: 999},                // yesterday UTC, within 24h
		{TS: goalsNow.Add(-2 * time.Hour), Model: model, InputTokens: 1_000, CostUSD: 999},                  // today
		{TS: goalsNow.Add(-1 * time.Hour), Model: "claude-unknown-0", InputTokens: 5_000_000, CostUSD: 999}, // unpriced
	}
	for _, r := range rows {
		if err := srv.Store.AppendUsage(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	const eps = 1e-9
	near := func(a, b float64) bool { return a-b < eps && b-a < eps }

	ro := srv.orgSnapshotValue().Readouts
	if want := perCall(1_000); !near(ro.SpendToday, want) {
		t.Errorf("spend_today = %v, want %v", ro.SpendToday, want)
	}
	if want := perCall(100_000) + perCall(10_000) + perCall(1_000); !near(ro.Spend7d, want) {
		t.Errorf("spend_7d = %v, want %v", ro.Spend7d, want)
	}

	if err := srv.Store.AppendUsage(store.UsageRecord{TS: goalsNow.Add(-time.Minute), Model: model, InputTokens: 2_000}); err != nil {
		t.Fatalf("append: %v", err)
	}
	ro = srv.orgSnapshotValue().Readouts
	if want := perCall(1_000) + perCall(2_000); !near(ro.SpendToday, want) {
		t.Errorf("after append spend_today = %v, want %v", ro.SpendToday, want)
	}

	// The window moves with the clock without an append: a day later
	// "today" is empty and the three-day-old row is still in the week.
	fake.Advance(24 * time.Hour)
	ro = srv.orgSnapshotValue().Readouts
	if ro.SpendToday != 0 {
		t.Errorf("next day spend_today = %v, want 0", ro.SpendToday)
	}
	if ro.Spend7d <= perCall(100_000) {
		t.Errorf("next day spend_7d = %v, want the three-day-old row still counted", ro.Spend7d)
	}
}

func TestSnapshotContextPctAndOrder(t *testing.T) {
	srv := newTestServer(t)
	srv.AgentModel = provider.MockModelSmall
	for _, a := range []store.Agent{
		{Slug: "cos", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "alice", Role: "", ReportsTo: "cos"},
	} {
		if err := srv.Store.CreateAgent(a, "k"); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	window := srv.modelContextWindow(provider.MockModelSmall)
	// A quarter of the window, by the chars/4 estimate.
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{Role: store.RoleSent, Content: string(make([]byte, window)), TS: goalsNow}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	snap := srv.orgSnapshotValue()
	if len(snap.Agents) != 2 {
		t.Fatalf("agents = %+v", snap.Agents)
	}
	cos, alice := snap.Agents[0], snap.Agents[1]
	if cos.Slug != "cos" || cos.Depth != 1 || cos.ContextPct != 0 || cos.ReportsTo != "ceo" {
		t.Errorf("cos = %+v", cos)
	}
	if alice.Slug != "alice" || alice.Depth != 2 || alice.Name != "alice" || alice.ReportsTo != "cos" || alice.ContextPct != 25 {
		t.Errorf("alice = %+v", alice)
	}
}
