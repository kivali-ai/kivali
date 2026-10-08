package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

func writePlan(t *testing.T, st *store.FSStore, slug, body string) {
	t.Helper()
	root := files.StorageRoot(filepath.Join(st.Root(), "agents", slug))
	dir := filepath.Join(root, files.SharedWorkspaceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, files.PlanFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func planTestStore(t *testing.T) *store.FSStore {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

// TestLoadBackgroundPlanCountsProgress covers the panel's headline number.
func TestLoadBackgroundPlanCountsProgress(t *testing.T) {
	st := planTestStore(t)
	writePlan(t, st, "alice", `# Renewal recommendation: Acme

**Deliverable:** a recommend/decline call.

## Tasks
- [x] 1. Pull spend vs. minimum → /files/background/spend.md
- [X] 2. List support incidents → /files/background/incidents.md
- [ ] 3. Find comparable vendors
- [ ] 4. Write the recommendation  (me)
`)

	view := loadBackgroundPlan(st, "alice")
	if !view.Present {
		t.Fatal("Present = false for a plan that exists")
	}
	if view.Title != "Renewal recommendation: Acme" {
		t.Errorf("Title = %q", view.Title)
	}
	if view.Total != 4 || view.Done != 2 {
		t.Errorf("progress = %d/%d, want 2/4", view.Done, view.Total)
	}
	if view.Items[0].Text != "1. Pull spend vs. minimum → /files/background/spend.md" {
		t.Errorf("first item text = %q", view.Items[0].Text)
	}
	if !view.Items[1].Done {
		t.Error("an uppercase [X] should count as done")
	}
}

// TestLoadBackgroundPlanToleratesLooseFormatting is the point of a
// convention rather than a schema. The author is a language model, and
// a parser that only accepted one bullet style would render a perfectly
// good plan as unstructured prose the first time it wrote `* [ ]`.
func TestLoadBackgroundPlanToleratesLooseFormatting(t *testing.T) {
	st := planTestStore(t)
	writePlan(t, st, "alice", `Plan

* [ ] star bullet
+ [x] plus bullet
  - [ ] indented dash
- not a checkbox at all
- [ ]
`)

	view := loadBackgroundPlan(st, "alice")
	if view.Total != 3 {
		t.Fatalf("Total = %d, want 3 (an empty checkbox and a plain bullet are not items)", view.Total)
	}
	if view.Done != 1 {
		t.Errorf("Done = %d, want 1", view.Done)
	}
	if view.Title != "Plan" {
		t.Errorf("Title = %q; a plan with no heading should fall back to its first line", view.Title)
	}
}

// TestLoadBackgroundPlanAbsentIsNotAnError keeps the panel optional. Most
// agents, most of the time, are answering a question rather than
// running a project, and they must not get an error or an empty box.
func TestLoadBackgroundPlanAbsentIsNotAnError(t *testing.T) {
	st := planTestStore(t)
	if view := loadBackgroundPlan(st, "alice"); view.Present {
		t.Error("Present = true with no plan file on disk")
	}
	if view := loadBackgroundPlan(nil, "alice"); view.Present {
		t.Error("Present = true with no store")
	}
}

// TestLoadBackgroundPlanProseStillRenders proves a plan whose items do not
// parse is shown rather than swallowed. Failing to match a checkbox
// costs the progress count, never the content.
func TestLoadBackgroundPlanProseStillRenders(t *testing.T) {
	st := planTestStore(t)
	writePlan(t, st, "alice", "# Approach\n\nI am going to read the contracts first, then decide.\n")

	view := loadBackgroundPlan(st, "alice")
	if !view.Present {
		t.Fatal("a prose plan should still render")
	}
	if view.Total != 0 {
		t.Errorf("Total = %d, want 0", view.Total)
	}
	if view.Body == "" {
		t.Error("Body is empty; the panel would have nothing to show")
	}
}

// background/ is the agent's to write, and the plan is read on core,
// which mounts the whole volume. A plan.md that is a link — here to a
// file outside the agent's tree — is no plan: core does not follow it
// into the CEO's panel.
func TestLoadBackgroundPlanIgnoresASymlink(t *testing.T) {
	st := planTestStore(t)
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("# not the plan\n- [ ] secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(files.StorageRoot(filepath.Join(st.Root(), "agents", "alice")), files.SharedWorkspaceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, files.PlanFileName)); err != nil {
		t.Fatal(err)
	}
	if view := loadBackgroundPlan(st, "alice"); view.Present {
		t.Errorf("a symlinked plan.md was read: %+v", view)
	}
}
