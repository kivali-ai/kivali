package agent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
)

// Why this test exists: the engineer flag is what gives an agent the
// integration_* tools, and approving a hire from the Integrations
// engineer's template is the only thing that sets it. A hire with no
// template must not get it, whatever its slug or role says, and a
// template Kivali does not ship is refused rather than hired as an
// ordinary agent the owner did not approve.
func TestApplyHireFromTheIntegrationsEngineerTemplate(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	r := NewRuntime(st, RuntimeDefaults{AgentModel: "mock-large-0"})

	if _, err := r.ApplyHire(store.Hire{
		Slug: "builder", Role: "Integrations engineer", ReportsTo: "chief-of-staff",
		Body: seed.IntegrationsEngineerRole, Template: seed.IntegrationsEngineerTemplate,
	}); err != nil {
		t.Fatal(err)
	}
	if a, err := st.GetAgent("builder"); err != nil || !a.IntegrationsEngineer {
		t.Errorf("builder = %+v, %v; want the engineer flag", a, err)
	}
	if role, _ := st.ReadRole("builder"); role != seed.IntegrationsEngineerRole {
		t.Errorf("role.md is not the template's")
	}

	if _, err := r.ApplyHire(store.Hire{Slug: "integrations-engineer", Role: "Integrations engineer", ReportsTo: "chief-of-staff", Body: "# a role of its own\n"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetAgent("integrations-engineer"); a.IntegrationsEngineer {
		t.Error("a hire with no template got the engineer flag from its slug")
	}

	_, err = r.ApplyHire(store.Hire{Slug: "other", Role: "Other", ReportsTo: "chief-of-staff", Body: "x", Template: "web-designer"})
	if err == nil || !strings.Contains(err.Error(), `template "web-designer" is not a role Kivali ships`) {
		t.Errorf("unknown template = %v", err)
	}
	if _, err := st.GetAgent("other"); err == nil {
		t.Error("a hire with an unknown template created the agent")
	}
}
