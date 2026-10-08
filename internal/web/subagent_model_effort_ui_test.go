package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
)

// The mock provider's subagent default and the effort it resolves to
// (none: the default model has no reasoning control).
var (
	mockSubagentDefault       = provider.MockProvider{}.Defaults().Subagent
	mockSubagentDefaultEffort = ""
)

// Which model and effort a delegated task runs on is the main cost
// lever an agent has. These pin the three surfaces that say it: the
// task rows in the transcript chip (from the chat API and live), the
// task rows in the background panel, and the subagent transcript
// page's header.
//
// The values are RESOLVED everywhere. A task dispatched with neither
// field set runs on the fleet defaults, and a blank chip would have
// answered "which tier is this burning" with nothing.

// TestUnsetModelAndEffortResolveOnceAndReachEveryReader is the
// contract behind every chip: resolution happens once, at
// registration, and the job record, the spec the pod runs and the
// meta.json the page reads back all carry the same value. The
// previous contract passed omission through and let the pod decide —
// correct for the spawn, but it left every UI reader with a blank.
func TestUnsetModelAndEffortResolveOnceAndReachEveryReader(t *testing.T) {
	driver := &fakeSubagentDriver{}
	svc, st, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)

	if _, err := svc.StartBatch(context.Background(), "alice", taskArgs("", "")); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	// The delivery is the last thing finishJob does, after the final
	// meta.json snapshot, so once it lands everything below is settled.
	del.waitFor(1)

	if len(driver.requests) != 1 {
		t.Fatalf("spawned %d, want 1", len(driver.requests))
	}
	spec := driver.requests[0].Spec
	if spec.Model != mockSubagentDefault || spec.Effort != mockSubagentDefaultEffort {
		t.Errorf("Spec.Model = %q Spec.Effort = %q, want the fleet defaults resolved in core",
			spec.Model, spec.Effort)
	}

	jobs := svc.listJobs("alice")
	if len(jobs) != 1 {
		t.Fatalf("registry has %d jobs, want 1", len(jobs))
	}
	if jobs[0].Model != mockSubagentDefault || jobs[0].Effort != mockSubagentDefaultEffort {
		t.Errorf("job Model = %q Effort = %q, want the resolved defaults (the panel reads these)",
			jobs[0].Model, jobs[0].Effort)
	}

	meta, err := readSubagentMeta(filepath.Join(st.Root(), "agents", "alice", "subagents", jobs[0].ID, "meta.json"))
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	if meta.Model != mockSubagentDefault || meta.Effort != mockSubagentDefaultEffort {
		t.Errorf("meta.json model = %q effort = %q, want the resolved defaults (the transcript page reads these)",
			meta.Model, meta.Effort)
	}
}

// TestStartedEventNamesModelAndEffortPerTask covers the live path. A
// page watching the batch start builds its rows from subagent_started,
// so the event has to carry what the chat API's row would show:
// model already in friendly form, effort raw — the same split the
// delta event uses for the parent's own bubble.
func TestStartedEventNamesModelAndEffortPerTask(t *testing.T) {
	svc, _, _, rec, del := newSubagentTestServiceWithDeliveries(t, &fakeSubagentDriver{})

	args, _ := json.Marshal(map[string]any{"tasks": []map[string]any{
		{"description": "deep read", "prompt": "p", "model": provider.MockModelLarge, "effort": "low"},
		{"description": "quick grep", "prompt": "p"},
	}})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	del.waitFor(2)

	rec.mu.Lock()
	events := append([]hubRecorderEvent(nil), rec.events...)
	rec.mu.Unlock()
	var tasks []any
	for _, e := range events {
		if e.Kind == "subagent_started" {
			tasks, _ = e.Data["tasks"].([]any)
			break
		}
	}
	if len(tasks) != 2 {
		t.Fatalf("subagent_started carried %d tasks, want 2", len(tasks))
	}
	first, _ := tasks[0].(map[string]any)
	second, _ := tasks[1].(map[string]any)
	if first["model"] != provider.Label(provider.MockProvider{}, provider.MockModelLarge) || first["effort"] != "low" {
		t.Errorf("explicit task: model=%v effort=%v, want %q and low",
			first["model"], first["effort"], provider.Label(provider.MockProvider{}, provider.MockModelLarge))
	}
	if second["model"] != provider.Label(provider.MockProvider{}, mockSubagentDefault) || second["effort"] != mockSubagentDefaultEffort {
		t.Errorf("unset task: model=%v effort=%v, want the resolved defaults %q and %q",
			second["model"], second["effort"], provider.Label(provider.MockProvider{}, mockSubagentDefault), mockSubagentDefaultEffort)
	}
}

// TestSubagentTaskViewsCarryResolvedModelAndEffort is the replay path
// for a chip still in flight: no tool_result yet, nothing on disk, so
// the row is built from the tool input alone — and still says what
// each task runs on.
func TestSubagentTaskViewsCarryResolvedModelAndEffort(t *testing.T) {
	srv := newTestServer(t)
	input := `{"tasks":[
		{"description":"deep read","prompt":"p","model":"mock-large-0","effort":"low"},
		{"description":"quick grep","prompt":"p"}
	]}`
	views := srv.subagentTaskViews("alice", input, "")
	if len(views) != 2 {
		t.Fatalf("views = %d, want 2", len(views))
	}
	if views[0].Model != provider.MockModelRetired || views[0].Effort != "low" {
		t.Errorf("explicit task: Model=%q Effort=%q, want what the agent set", views[0].Model, views[0].Effort)
	}
	if views[1].Model != mockSubagentDefault || views[1].Effort != mockSubagentDefaultEffort {
		t.Errorf("unset task: Model=%q Effort=%q, want the fleet defaults it runs on", views[1].Model, views[1].Effort)
	}
}

// TestSubagentTaskViewsPreferMetaButFallBackToInput: once a task has
// an id, meta.json says what ran and wins. A meta.json without an
// effort still gets the input's resolved effort rather than an empty
// pill.
func TestSubagentTaskViewsPreferMetaButFallBackToInput(t *testing.T) {
	srv := newTestServer(t)
	dir := filepath.Join(srv.Store.Root(), "agents", "alice", "subagents", "deadbeef")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"),
		[]byte(`{"id":"deadbeef","status":"completed","model":"mock-small"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	input := `{"tasks":[{"description":"quick grep","prompt":"p","model":"mock-large","effort":"low"}]}`
	result := "\n=== task 1: quick grep ===\nid: deadbeef · transcript: /agents/alice/subagents/deadbeef\n"

	views := srv.subagentTaskViews("alice", input, result)
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	if views[0].Model != provider.MockModelSmall {
		t.Errorf("Model = %q, want the model meta.json says ran", views[0].Model)
	}
	if views[0].Effort != "low" {
		t.Errorf("Effort = %q, want the input's effort when meta.json has none", views[0].Effort)
	}
}
