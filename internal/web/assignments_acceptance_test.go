package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The CEO's side: the detail counts a parent by its Done-when
// conditions, and an edit changes the list.
func TestCEOSeesAcceptanceProgress(t *testing.T) {
	srv := assignmentsServer(t)
	ctx := context.Background()
	tr := srv.tracker()
	items := []string{"login page reviewed", "export page reviewed", "settings page reviewed", "version tagged", "release notes"}
	if _, err := tr.Create(ctx, agent.CEOSlug, assignments.CreateInput{Title: "v3 release", Assignee: "alice", Acceptance: items}); err != nil {
		t.Fatal(err)
	}
	for i, item := range []string{"login page reviewed", "export page reviewed", "version tagged"} {
		if _, err := tr.Create(ctx, "alice", assignments.CreateInput{Title: "Work " + item, Assignee: "bob", Parent: 1, Satisfies: []string{item}}); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Close(ctx, "bob", 2+i, assignments.ResolutionDone, "done"); err != nil {
			t.Fatal(err)
		}
	}
	want := apitypes.ConditionProgress{Satisfied: 3, Claimed: 0, Unclaimed: 2}
	a := getAssignment(t, srv, 1)
	if a.Progress != want {
		t.Errorf("detail progress = %+v, want %+v", a.Progress, want)
	}
	states := map[string]apitypes.ConditionState{}
	for _, c := range a.Conditions {
		states[c.Name] = c.State
	}
	if states["login page reviewed"] != apitypes.ConditionStateSatisfied || states["settings page reviewed"] != apitypes.ConditionStateUnclaimed {
		t.Errorf("condition states = %v", states)
	}

	// An edit that sends the list unchanged records nothing.
	rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{Conditions: &items, Seq: &a.Seq})
	if rr.Code != http.StatusOK {
		t.Fatalf("no-op save: %d %s", rr.Code, rr.Body.String())
	}
	rel, _ := srv.Store.ReadAssignment(1)
	if rel.LastSeq() != a.Seq {
		t.Fatalf("an untouched list was logged: %+v", rel.Log)
	}
	more := append(append([]string{}, items...), "launch review held")
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{Conditions: &more, Note: "review added"})
	if rr.Code != http.StatusOK {
		t.Fatalf("add item: %d %s", rr.Code, rr.Body.String())
	}
	rel, _ = srv.Store.ReadAssignment(1)
	if last := rel.Log[len(rel.Log)-1]; last.Op != assignments.OpAcceptance || len(last.Items) != 6 || last.Note != "review added" {
		t.Fatalf("log = %+v", rel.Log)
	}
	// Removing a condition an open part claims is the rules' refusal.
	if rr := workDo(t, srv, http.MethodPost, "/api/v1/assignments/2/reopen", apitypes.AssignmentReopenRequest{Note: "redo"}); rr.Code != http.StatusOK {
		t.Fatalf("reopen: %d %s", rr.Code, rr.Body.String())
	}
	only := []string{"export page reviewed"}
	rr = workDo(t, srv, http.MethodPost, "/api/v1/assignments/1/update", apitypes.AssignmentUpdateRequest{Conditions: &only})
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "is claimed by open #2") {
		t.Fatalf("remove claimed: %d %s", rr.Code, rr.Body.String())
	}
}

// The wake note's assignments section shows the counts on a parent and
// carries the nudge once, through the real release path: the
// assignments are released, alice wakes, the note behind them lists
// what she holds, and her next wake does not repeat the nudge. The
// deliveries themselves carry none of it.
func TestWakeNoteCarriesCountsAndTheNudgeOnce(t *testing.T) {
	srv := newChatServer(t)
	for _, a := range []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"},
		{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "bob", Role: "Engineer", ReportsTo: "chief-of-staff"},
	} {
		if err := srv.Store.CreateAgent(a, "# role"); err != nil {
			t.Fatal(err)
		}
	}
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponse(deltaResponse("ok"))
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)
	ctx := context.Background()
	tr := srv.tracker()
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Release", Assignee: "alice", Acceptance: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Create(ctx, "chief-of-staff", assignments.CreateInput{Title: "Epic", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Create(ctx, "alice", assignments.CreateInput{Title: "Part", Assignee: "bob", Parent: 2}); err != nil {
		t.Fatal(err)
	}
	notes := func() []string {
		t.Helper()
		var out []string
		for _, m := range readChat(t, srv, "alice") {
			switch {
			case m.Kind == store.KindWakeUpdate:
				out = append(out, m.Content)
			case m.Role == store.RoleReceived && strings.Contains(m.Content, "open assignments"):
				t.Errorf("a delivery repeats what alice holds: %+v", m)
			}
		}
		return out
	}
	// The release batch carries the two assignments; the wake they
	// cause appends the note behind them.
	if _, err := srv.Messenger.ReleaseAll(ctx, nil, srv.deliveryHooks()); err != nil {
		t.Fatal(err)
	}
	fake.AwaitTurn(t, 5*time.Second)
	fake.AwaitFinished(t, 5*time.Second)
	waitForChatIdle(t, srv, "alice", 5*time.Second)
	want := "Your open assignments:\n- #1 \"Release\" (ready; done when 0 met / 0 claimed-open / 2 unclaimed)\n- #2 \"Epic\" (blocked by #3)\n\n#2 now has parts and no Done-when conditions."
	if first := notes(); len(first) != 1 || !strings.Contains(first[0], want) {
		t.Fatalf("first wake's notes = %q\nwant one containing\n%q", first, want)
	}
	sendMessage(t, srv, fake, "again")
	second := notes()
	if len(second) != 2 || strings.Contains(second[1], "no acceptance items") || !strings.Contains(second[1], `#2 "Epic" (blocked by #3)`) {
		t.Fatalf("second wake's notes = %q", second)
	}
}
