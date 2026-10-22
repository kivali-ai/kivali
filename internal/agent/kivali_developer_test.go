package agent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// Why this test exists: the Kivali Developer stays with the team and
// always reports to the Chief of Staff. The parser refuses an offboard
// of it and a reorg that moves it (internal/message), but the apply
// step is the chokepoint any other path to these changes passes
// through; without its own check, an approval request written some
// other way would archive the one agent that builds integrations, or
// put it under another manager.
func TestApplyRefusesToOffboardOrMoveTheKivaliDeveloper(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.Agent{
		{Slug: ChiefOfStaffSlug, Role: "Chief of Staff", ReportsTo: CEOSlug},
		{Slug: KivaliDeveloperSlug, Role: "Kivali Developer", ReportsTo: ChiefOfStaffSlug},
		{Slug: "alice", Role: "Analyst", ReportsTo: ChiefOfStaffSlug},
	} {
		if err := st.CreateAgent(a, "role"); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRuntime(st, RuntimeDefaults{AgentModel: "mock-large-0"})

	if err := r.ApplyOffboard(store.Offboard{Slug: KivaliDeveloperSlug}); err == nil || !strings.Contains(err.Error(), "cannot offboard") {
		t.Errorf("offboard = %v", err)
	}
	if _, err := st.GetAgent(KivaliDeveloperSlug); err != nil {
		t.Errorf("the Kivali Developer is gone after a refused offboard: %v", err)
	}

	out, err := r.ApplyReorg(store.Reorg{Moves: []store.ReorgMove{{Slug: KivaliDeveloperSlug, NewManager: "alice"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Applied) != 0 || len(out.Failed) != 1 || !strings.Contains(out.Failed[0].Reason, "always reports to the Chief of Staff") {
		t.Errorf("reorg = %+v", out)
	}
	if a, _ := st.GetAgent(KivaliDeveloperSlug); a.ReportsTo != ChiefOfStaffSlug {
		t.Errorf("the Kivali Developer reports to %q", a.ReportsTo)
	}
}
