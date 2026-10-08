package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/store"
)

// An archived agent cannot be woken, so approving an offboard moves
// every open assignment it held to whoever it reported to, each with a
// note saying why, and leaves its closed ones where they are.
func TestCEOApproveOffboardMovesOpenAssignmentsToManager(t *testing.T) {
	srv, _ := newTurnServer(t)
	_ = srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "# role\n")
	_ = srv.Store.CreateAgent(store.Agent{Slug: "market-analyst", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# Analyst\n")
	ctx := context.Background()
	tr := srv.tracker()
	for _, title := range []string{"Sizing", "Pricing"} {
		if _, err := tr.Create(ctx, agent.CEOSlug, assignments.CreateInput{Title: title, Assignee: "market-analyst"}); err != nil {
			t.Fatal(err)
		}
	}
	done, err := tr.Create(ctx, agent.CEOSlug, assignments.CreateInput{Title: "Old", Assignee: "market-analyst"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Close(ctx, "market-analyst", done.Assignment.ID, assignments.ResolutionDone, "shipped"); err != nil {
		t.Fatal(err)
	}
	rel := seedOffboardProposal(t, srv, "market-analyst")
	if rr := approve(t, srv, rel); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	set, err := tr.Set()
	if err != nil {
		t.Fatal(err)
	}
	moved := set.Query(assignments.Filter{Assignee: "chief-of-staff"})
	if len(moved) != 2 {
		t.Fatalf("cos holds %d open assignments, want the 2 that were open", len(moved))
	}
	for _, iss := range moved {
		last := iss.Log[len(iss.Log)-1]
		if last.Op != assignments.OpAssigned || last.By != agent.CEOSlug || !strings.Contains(last.Note, "market-analyst was offboarded") {
			t.Errorf("%s log tail = %+v", iss.Ref(), last)
		}
	}
	if old, _ := set.Get(done.Assignment.ID); old.Assignee != "market-analyst" {
		t.Errorf("closed assignment was moved: %+v", old)
	}
	// The manager was told about each at once (the CEO made the change).
	cosChat, _ := srv.Store.ReadChatHistory("chief-of-staff")
	var told int
	for _, m := range cosChat {
		if strings.Contains(m.Content, "market-analyst was offboarded") {
			told++
		}
	}
	if told != 2 {
		t.Errorf("cos told %d times, want 2", told)
	}
}
