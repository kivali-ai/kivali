package web

import (
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// Test CLI session ids: the UUIDs a task's run reports.
const (
	restartSession1 = "0d6f3c1e-9a7b-4c2d-8e5f-1a2b3c4d5e6f"
	restartSession2 = "1e7a4d2f-0b8c-4d3e-9f6a-2b3c4d5e6f70"
	restartSession3 = "2f8b5e3a-1c9d-4e4f-8a7b-3c4d5e6f7081"
)

// setMetaFields writes fields into a task's meta.json as a run of core
// would have left them.
func setMetaFields(t *testing.T, st *store.FSStore, parent, id string, fields map[string]any) {
	t.Helper()
	if err := st.UpdateSubagentMeta(parent, id, func(meta map[string]any) bool {
		for k, v := range fields {
			meta[k] = v
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
}

// driveCallFor is the driver's call for one task, failing when there
// is not exactly one.
func driveCallFor(t *testing.T, d *fakeSubagentDriver, id string) fakeDriveCall {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	var calls []fakeDriveCall
	for _, c := range d.requests {
		if c.SubagentID == id {
			calls = append(calls, c)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("task %s was driven %d times, want once", id, len(calls))
	}
	return calls[0]
}

// driveCount is how many runs the driver was asked for.
func driveCount(d *fakeSubagentDriver) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

// A restart of core must not throw away the work of the tasks it was
// running: those the agent dispatched itself run on from the CLI
// session their last run reported, with one line saying what happened,
// and their results arrive as any task's do. This guards which records
// are resumed (queued or running, the agent's own, no files, fewer than
// subagentRestartResumes restarts) and which end errored and are
// reported (a wait for a person, a task with files, one out of
// resumes; a worker is ended but reported to nobody), that each resumed
// run gets the session and line for its kind (a queued task that never
// ran starts over from its prompt), that the task keeps its place in
// its chip, and that its record counts the restart.
func TestBootResumesTheAgentsOwnTasksAndReportsTheRest(t *testing.T) {
	driver := &fakeSubagentDriver{}
	svc, st, _, rec, del := newSubagentTestServiceWithDeliveries(t, driver)
	plain := SubagentTaskInput{Description: "list the incidents", Prompt: "find them", toolUseID: "toolu_1", index: 1}
	fresh := SubagentTaskInput{Description: "sort the incidents", Prompt: "sort them"}
	quoted := SubagentTaskInput{Description: "price the parts", Prompt: "price them", Schema: json.RawMessage(quoteSchema)}
	page := SubagentTaskInput{Description: "check the price", Prompt: "look it up", Kind: agentpod.SubagentKindWeb, StartURL: "https://example.com/"}
	upload := SubagentTaskInput{Description: "send the form", Prompt: "upload it", Kind: agentpod.SubagentKindWeb, StartURL: "https://example.com/", Files: []string{"/files/project/form.pdf"}}
	worker := SubagentTaskInput{Description: "read the export", Prompt: "read it", caller: "lead0001"}

	svc.writeSubagentMeta("alice", plain, "runs0001", subagentJobRunning, "", "")
	setMetaFields(t, st, "alice", "runs0001", map[string]any{"cli_session": restartSession1})
	svc.writeSubagentMeta("alice", fresh, "queu0001", subagentJobQueued, "", "")
	svc.writeSubagentMeta("alice", quoted, "stru0001", subagentJobRunning, "", "")
	setMetaFields(t, st, "alice", "stru0001", map[string]any{"cli_session": restartSession2})
	svc.writeSubagentMeta("alice", page, "webs0001", subagentJobRunning, "", "")
	setMetaFields(t, st, "alice", "webs0001", map[string]any{"cli_session": restartSession3})
	svc.writeSubagentMeta("alice", plain, "wait0001", subagentJobWaiting, "", "")
	svc.writeSubagentMeta("alice", upload, "file0001", subagentJobRunning, "", "")
	svc.writeSubagentMeta("alice", worker, "work0001", subagentJobRunning, "", "")
	svc.writeSubagentMeta("alice", plain, "last0001", subagentJobRunning, "", "")
	setMetaFields(t, st, "alice", "last0001", map[string]any{"cli_session": restartSession1, "restarts": subagentRestartResumes})

	failed, told, resumed := svc.ResumeOrphanedSubagents()
	if failed != 4 || resumed != 4 || !slices.Equal(told, []string{"alice"}) {
		t.Fatalf("failed %d, resumed %d, told %v; want 4, 4, [alice]", failed, resumed, told)
	}
	// The restart notice, then the four resumed tasks' results.
	msgs := del.waitFor(5)
	var notice string
	finished := map[string]bool{}
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "Kivali restarted") {
			notice = m.Content
			continue
		}
		for _, id := range []string{"runs0001", "queu0001", "stru0001", "webs0001"} {
			if strings.Contains(m.Content, "Background task "+id+" ") {
				finished[id] = true
			}
		}
	}
	if len(finished) != 4 {
		t.Errorf("results arrived for %v, want the four resumed tasks", finished)
	}
	for _, want := range []string{"ended 3 of your background tasks", "wait0001", "file0001", "last0001"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice lacks %q:\n%s", want, notice)
		}
	}
	for _, not := range []string{"runs0001", "queu0001", "stru0001", "webs0001", "work0001"} {
		if strings.Contains(notice, not) {
			t.Errorf("notice names %s:\n%s", not, notice)
		}
	}

	if c := driveCallFor(t, driver, "runs0001"); c.Spec.Resume != restartSession1 || c.Spec.UserPrompt != restartResumeLine+restartResumeTextDoneLine {
		t.Errorf("runs0001 resumed %q with %q", c.Spec.Resume, c.Spec.UserPrompt)
	}
	if c := driveCallFor(t, driver, "queu0001"); c.Spec.Resume != "" || c.Spec.UserPrompt != "sort them" {
		t.Errorf("queu0001 resumed %q with %q, want a fresh start from its prompt", c.Spec.Resume, c.Spec.UserPrompt)
	}
	if c := driveCallFor(t, driver, "stru0001"); c.Spec.Resume != restartSession2 || len(c.Spec.Schema) == 0 || !strings.HasSuffix(c.Spec.UserPrompt, restartResumeStructuredLine) {
		t.Errorf("stru0001 resumed %q with %q, schema %s", c.Spec.Resume, c.Spec.UserPrompt, c.Spec.Schema)
	}
	if c := driveCallFor(t, driver, "webs0001"); c.Spec.Resume != restartSession3 || c.Spec.Kind != agentpod.SubagentKindWeb || !strings.Contains(c.Spec.UserPrompt, restartResumeWebLine) {
		t.Errorf("webs0001 resumed %q as %q with %q", c.Spec.Resume, c.Spec.Kind, c.Spec.UserPrompt)
	}

	if meta := readMetaMap(t, st, "alice", "runs0001"); meta["status"] != subagentJobCompleted || meta["restarts"] != float64(1) {
		t.Errorf("runs0001 record = %v, want completed after 1 restart", meta)
	}
	for _, id := range []string{"wait0001", "file0001", "work0001", "last0001"} {
		if meta := readMetaMap(t, st, "alice", id); meta["status"] != "errored" || meta["error"] != subagentRestartError {
			t.Errorf("%s record = %v, want errored", id, meta)
		}
	}
	hist, err := readSubagentHistory(subagentChatPath(st.Root(), "alice", "runs0001"))
	if err != nil || !slices.ContainsFunc(hist, func(m store.ChatMessage) bool {
		return m.Role == store.RoleReceived && m.Content == restartResumeLine+restartResumeTextDoneLine
	}) {
		t.Errorf("runs0001 transcript lacks the resume line: %+v, %v", hist, err)
	}
	placed := false
	rec.mu.Lock()
	for _, e := range rec.events {
		if e.Kind == "subagent_progress" && e.Data["id"] == "runs0001" && e.Data["tool_use_id"] == "toolu_1" && e.Data["index"] == float64(1) {
			placed = true
		}
	}
	rec.mu.Unlock()
	if !placed {
		t.Error("runs0001's progress does not name its place in the chip it was dispatched from")
	}
}

// A restore must not run a backup's tasks again: they are a past run's.
// This guards that RecoverInterruptedWork without resumeTasks ends a
// running task errored and reports it.
func TestRestoreResumesNoTask(t *testing.T) {
	driver := &fakeSubagentDriver{}
	svc, st, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	srv, err := NewServer(&Server{Store: st, Provider: provider.MockProvider{}, SubagentService: svc})
	if err != nil {
		t.Fatal(err)
	}
	svc.writeSubagentMeta("alice", SubagentTaskInput{Description: "list the incidents", Prompt: "p"}, "runs0001", subagentJobRunning, "", "")
	setMetaFields(t, st, "alice", "runs0001", map[string]any{"cli_session": restartSession1})

	if wake := srv.RecoverInterruptedWork(false); !slices.Equal(wake, []string{"alice"}) {
		t.Errorf("wake = %v, want alice", wake)
	}
	if body := del.waitFor(1)[0].Content; !strings.Contains(body, "Background task runs0001 (list the incidents) FAILED.") {
		t.Errorf("delivered = %s", body)
	}
	if n := driveCount(driver); n != 0 {
		t.Errorf("driven %d times after a restore", n)
	}
}

// A task resumed at boot runs in its agent's pod, which the boot may
// still be bringing up: driven before then, DriveSubagent fails at
// once and the task is lost after all. This guards that the run waits
// for the pod, and starts once it is there.
func TestRestartResumeWaitsForTheAgentsPod(t *testing.T) {
	driver := &fakeSubagentDriver{}
	svc, st, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	clk := clock.NewFakeAt(time.Date(2026, 10, 24, 20, 30, 0, 0, time.UTC))
	svc.Clock = clk
	var connected atomic.Bool
	checks := make(chan bool, 16)
	svc.PodConnected = func(string) bool {
		c := connected.Load()
		checks <- c
		return c
	}
	svc.writeSubagentMeta("alice", SubagentTaskInput{Description: "list the incidents", Prompt: "p"}, "runs0001", subagentJobRunning, "", "")
	setMetaFields(t, st, "alice", "runs0001", map[string]any{"cli_session": restartSession1})

	if _, _, resumed := svc.ResumeOrphanedSubagents(); resumed != 1 {
		t.Fatalf("resumed %d, want 1", resumed)
	}
	if <-checks {
		t.Fatal("the first check found a pod")
	}
	// The pod wait's limit and its ticker.
	clk.BlockUntil(2)
	clk.Advance(signInResumePodPoll)
	if <-checks || driveCount(driver) != 0 {
		t.Fatalf("the task was driven before its pod was back")
	}
	connected.Store(true)
	clk.Advance(signInResumePodPoll)
	if !<-checks {
		t.Fatal("the pod's return went unseen")
	}
	if body := del.waitFor(1)[0].Content; !strings.Contains(body, "Background task runs0001 (list the incidents) finished.") {
		t.Errorf("delivered = %s", body)
	}
}

// A task whose agent's pod does not come back must still end, or its
// agent waits on it for good. This guards that past
// restartResumePodWait it ends failed with a reason that says so, a
// structured task as the failed result JSON, and that nothing was
// driven.
func TestRestartResumeFailsWhenThePodIsNotBack(t *testing.T) {
	driver := &fakeSubagentDriver{}
	svc, st, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	clk := clock.NewFakeAt(time.Date(2026, 10, 24, 20, 30, 0, 0, time.UTC))
	svc.Clock = clk
	svc.PodConnected = func(string) bool { return false }
	svc.writeSubagentMeta("alice", SubagentTaskInput{Description: "price the parts", Prompt: "p", Schema: json.RawMessage(quoteSchema)}, "stru0001", subagentJobRunning, "", "")

	if _, _, resumed := svc.ResumeOrphanedSubagents(); resumed != 1 {
		t.Fatalf("resumed %d, want 1", resumed)
	}
	clk.BlockUntil(2)
	clk.Advance(restartResumePodWait)
	body := del.waitFor(1)[0].Content
	for _, want := range []string{"Background task stru0001 (price the parts) FAILED.", subagentResumeNoPodError, `"outcome": "failed"`} {
		if !strings.Contains(body, want) {
			t.Errorf("delivered lacks %q:\n%s", want, body)
		}
	}
	if n := driveCount(driver); n != 0 {
		t.Errorf("driven %d times without a pod", n)
	}
	if meta := readMetaMap(t, st, "alice", "stru0001"); meta["status"] != "errored" {
		t.Errorf("record = %v, want errored", meta)
	}
}

// A resume after a restart rebuilds the task from its record alone, so
// the record must hold all of it. This guards that meta.json keeps the
// schema, a web task's start URL and files, the task's place in its
// chip, and the CLI session its run reported (a UUID only).
func TestTaskRecordKeepsWhatAResumeNeeds(t *testing.T) {
	svc, st, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	task := SubagentTaskInput{
		Description: "price the parts", Prompt: "p", Schema: json.RawMessage(quoteSchema),
		Kind: agentpod.SubagentKindWeb, StartURL: " https://example.com/parts ", Files: []string{"/files/project/list.csv"},
		toolUseID: "toolu_7", index: 2,
	}
	svc.writeSubagentMeta("alice", task, "keep0001", subagentJobRunning, "", "")
	meta := readMetaMap(t, st, "alice", "keep0001")
	if schema, _ := json.Marshal(meta["schema"]); !strings.Contains(string(schema), `"vendor"`) {
		t.Errorf("schema = %s", schema)
	}
	if meta["start_url"] != "https://example.com/parts" || meta["tool_use_id"] != "toolu_7" || meta["index"] != float64(2) {
		t.Errorf("record = %v", meta)
	}
	if files, _ := meta["files"].([]any); len(files) != 1 || files[0] != "/files/project/list.csv" {
		t.Errorf("files = %v", meta["files"])
	}

	registerTestJob(svc, "keep0001", "alice", 1, subagentJobRunning)
	svc.recordCLISession("keep0001", "--not-a-session")
	if _, ok := readMetaMap(t, st, "alice", "keep0001")["cli_session"]; ok {
		t.Error("a session id that is not a UUID was kept")
	}
	svc.recordCLISession("keep0001", restartSession1)
	if got := readMetaMap(t, st, "alice", "keep0001")["cli_session"]; got != restartSession1 {
		t.Errorf("cli_session = %v", got)
	}
}

// A pod still running from before a restart dials in at once, and the
// startup pass may recreate it for a new spec: a task resumed onto it
// would die with it. This guards that the service reads such a pod as
// not there until the pass has released it.
func TestPodConnectedWaitsForTheStartupPass(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(&Server{Store: st, Provider: provider.MockProvider{}, AgentpodHub: newAgentpodHub(), SubagentService: &SubagentService{Store: st}})
	if err != nil {
		t.Fatal(err)
	}
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)
	srv.streamMu.Lock()
	srv.startupHolds = map[string]bool{"alice": true}
	srv.holdTurnsLocked("alice")
	srv.streamMu.Unlock()
	if srv.SubagentService.PodConnected("alice") {
		t.Error("a pod the startup pass still holds reads as connected")
	}
	srv.releaseStartupHold("alice")
	if !srv.SubagentService.PodConnected("alice") {
		t.Error("a released pod reads as not connected")
	}
}
