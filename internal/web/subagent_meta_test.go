package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// readMetaMap is a task's meta.json as written, every field.
func readMetaMap(t *testing.T, st *store.FSStore, parent, id string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(subagentMetaPath(st.Root(), parent, id))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatalf("meta.json: %v\n%s", err, body)
	}
	return meta
}

// TestSubagentMetaWritersKeepEachOthersFields: the full write and a
// patch, from different goroutines, leave one file with both the
// terminal status and the result, and the browser state the patch set;
// a site session's name is its site; the wait reason goes with the
// wait.
func TestSubagentMetaWritersKeepEachOthersFields(t *testing.T) {
	svc, st, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	task := SubagentTaskInput{Description: "price it", Prompt: "find the price", Model: "m", Effort: "high", Kind: agentpod.SubagentKindWeb, Session: "Shop.Vendor.example", Schema: json.RawMessage(quoteSchema)}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("job%02d", i)
		svc.writeSubagentMeta("alice", task, id, subagentJobRunning, "", "")
		svc.patchSubagentMeta("alice", id, map[string]any{"status": subagentJobWaiting, "wait_reason": "mfa", "site": "vendor.example"})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			svc.writeSubagentMeta("alice", task, id, "completed", "", `{"outcome":"found"}`)
		}()
		go func() {
			defer wg.Done()
			svc.setSubagentMetaBrowser("alice", id, apitypes.BrowserStateRunning)
		}()
		wg.Wait()
		meta := readMetaMap(t, st, "alice", id)
		if meta["status"] != "completed" || meta["result"] == nil || meta["browser"] != "running" || meta["site"] != "vendor.example" || meta["session"] != "vendor.example" {
			t.Fatalf("meta after interleaved writes = %v", meta)
		}
		if _, has := meta["wait_reason"]; has {
			t.Fatalf("a finished task keeps its wait reason: %v", meta)
		}
	}
	// A patch on a task with no meta.json yet writes none.
	svc.patchSubagentMeta("alice", "ghost", map[string]any{"browser": "running"})
	if _, err := os.Stat(subagentMetaPath(st.Root(), "alice", "ghost")); err == nil {
		t.Error("a patch created a meta.json for a task with none")
	}
}

// TestBrowserStateReachesWaitingTasks: a browser's state lands in the
// meta.json of every live web task on its session, one waiting for a
// person included, and not in a finished task's.
func TestBrowserStateReachesWaitingTasks(t *testing.T) {
	svc, st, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	task := SubagentTaskInput{Description: "price it", Prompt: "p", Kind: agentpod.SubagentKindWeb, Session: "vendor.example"}
	for id, state := range map[string]string{"waiting": subagentJobWaiting, "running": subagentJobRunning, "done": subagentJobCompleted} {
		svc.registerJob(&subagentJob{ID: id, Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.example", Site: "vendor.example", State: state, done: make(chan struct{})})
		svc.writeSubagentMeta("alice", task, id, state, "", "")
	}
	svc.browserStateChanged("alice", "vendor.example", apitypes.BrowserStateRunning)
	for id, want := range map[string]any{"waiting": "running", "running": "running", "done": nil} {
		if got := readMetaMap(t, st, "alice", id)["browser"]; got != want {
			t.Errorf("%s task's browser = %v, want %v", id, got, want)
		}
	}
}

// TestWebTaskSessionIsItsSiteOnTheWire: the chip's started row and the
// task's meta.json carry the session as the job does, the site a name
// resolves to, not the spelling the agent used.
func TestWebTaskSessionIsItsSiteOnTheWire(t *testing.T) {
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		return "", context.Canceled
	}}
	svc, st, _, rec, _ := newSubagentTestServiceWithDeliveries(t, driver)
	svc.Browsers = newBrowserRegistry(st, newFakeBrowserProv(), svc.clk(), 3, t.Logf, func(slug, id string) string { return subagentFilesRoot(st, slug, id) })
	svc.Browsers.markBooted()
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"session": "Shop.Vendor.example"})); err != nil {
		t.Fatal(err)
	}
	id := svc.listJobs("alice")[0].ID
	j, _ := svc.lookupJob(id)
	<-j.done
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var started map[string]any
	for _, e := range rec.events {
		if e.Kind == "subagent_started" {
			started = e.Data
		}
	}
	if started == nil {
		t.Fatal("no subagent_started event")
	}
	tasks := started["tasks"].([]any)
	if first := tasks[0].(map[string]any); first["session"] != "vendor.example" || first["kind"] != agentpod.SubagentKindWeb {
		t.Errorf("started row = %v", first)
	}
	if meta := readMetaMap(t, st, "alice", id); meta["session"] != "vendor.example" {
		t.Errorf("meta session = %v", meta["session"])
	}
}

// TestFailOrphanedSubagentsMarksUnfinishedTasks: at boot, every task
// record left queued, running or waiting for a person by the last run
// reads errored with the restart as the reason, without its wait
// reason or browser state; finished records and archived agents'
// finished records are untouched, and an archived agent's unfinished
// task is marked too.
func TestFailOrphanedSubagentsMarksUnfinishedTasks(t *testing.T) {
	svc, st, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	if err := st.CreateAgent(store.Agent{Slug: "bob", Role: "Buyer", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	task := SubagentTaskInput{Description: "d", Prompt: "p", Kind: agentpod.SubagentKindWeb, Session: "vendor.example"}
	svc.writeSubagentMeta("alice", task, "queued1", subagentJobQueued, "", "")
	svc.writeSubagentMeta("alice", task, "running1", subagentJobRunning, "", "")
	svc.writeSubagentMeta("alice", task, "waiting1", subagentJobRunning, "", "")
	svc.patchSubagentMeta("alice", "waiting1", map[string]any{"status": subagentJobWaiting, "wait_reason": "mfa", "site": "vendor.example", "browser": "running"})
	svc.writeSubagentMeta("alice", task, "done1", "completed", "", `{"outcome":"found"}`)
	svc.writeSubagentMeta("alice", task, "failed1", "errored", "the pod went away", "")
	svc.writeSubagentMeta("bob", task, "bobrun", subagentJobRunning, "", "")
	svc.writeSubagentMeta("bob", task, "bobdone", "completed", "", "")
	if err := st.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	before := readMetaMap(t, st, "alice", "done1")["updated_at"]

	if n := svc.FailOrphanedSubagents(); n != 4 {
		t.Errorf("marked %d records, want 4", n)
	}
	for _, id := range []string{"queued1", "running1", "waiting1"} {
		meta := readMetaMap(t, st, "alice", id)
		if meta["status"] != "errored" || meta["error"] != subagentRestartError {
			t.Errorf("%s after restart = %v", id, meta)
		}
		if _, has := meta["wait_reason"]; has {
			t.Errorf("%s keeps its wait reason", id)
		}
		if _, has := meta["browser"]; has {
			t.Errorf("%s keeps its browser state", id)
		}
	}
	if meta := readMetaMap(t, st, "alice", "waiting1"); meta["site"] != "vendor.example" {
		t.Errorf("the site went with the restart: %v", meta)
	}
	if meta := readMetaMap(t, st, "alice", "done1"); meta["status"] != "completed" || meta["updated_at"] != before {
		t.Errorf("a finished record was rewritten: %v", meta)
	}
	if meta := readMetaMap(t, st, "alice", "failed1"); meta["error"] != "the pod went away" {
		t.Errorf("a failed record lost its reason: %v", meta)
	}
	if meta := readMetaMap(t, st, "_archived/bob", "bobrun"); meta["status"] != "errored" || meta["error"] != subagentRestartError {
		t.Errorf("archived agent's unfinished task = %v", meta)
	}
	if meta := readMetaMap(t, st, "_archived/bob", "bobdone"); meta["status"] != "completed" {
		t.Errorf("archived agent's finished task = %v", meta)
	}
	// A second boot finds nothing to do.
	if n := svc.FailOrphanedSubagents(); n != 0 {
		t.Errorf("second pass marked %d records", n)
	}
	// The task's view reads the record as a failure with the reason.
	if state, errText := subagentState("errored", subagentRestartError); state != apitypes.SubagentTaskStateErrored || errText == nil || *errText != subagentRestartError {
		t.Errorf("view of a restarted task = %s, %v", state, errText)
	}
}
