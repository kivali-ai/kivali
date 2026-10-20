package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// parentDeliveries records what was delivered to which agent.
type parentDeliveries struct {
	mu   sync.Mutex
	msgs map[string][]store.ChatMessage
}

func (d *parentDeliveries) deliver(parent string, msg store.ChatMessage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.msgs == nil {
		d.msgs = map[string][]store.ChatMessage{}
	}
	d.msgs[parent] = append(d.msgs[parent], msg)
	return nil
}

func (d *parentDeliveries) snapshot() map[string][]store.ChatMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string][]store.ChatMessage{}
	for k, v := range d.msgs {
		out[k] = append([]store.ChatMessage(nil), v...)
	}
	return out
}

// A restart ends every background task in flight, and nothing else
// would ever tell the agent that dispatched them: one that ended its
// turn to wait for the results would wait for good. This guards that
// each agent gets one subagent_result naming its own lost tasks (a
// structured one as the failed result JSON its schema caller expects),
// that a worker is not reported (its answer was owed to its sub-lead,
// which ended with it), that an archived or vanished agent is told
// nothing, and that a second boot tells nobody again.
func TestRestartTellsEachAgentWhichOfItsTasksEnded(t *testing.T) {
	svc, st, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	del := &parentDeliveries{}
	svc.DeliverToParent = del.deliver
	if err := st.CreateAgent(store.Agent{Slug: "bob", Role: "Buyer", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	plain := SubagentTaskInput{Description: "list the incidents", Prompt: "p"}
	quoted := SubagentTaskInput{Description: "price the parts", Prompt: "p", Schema: json.RawMessage(quoteSchema)}
	worker := SubagentTaskInput{Description: "read the export", Prompt: "p", caller: "lead0001"}
	svc.writeSubagentMeta("alice", quoted, "lead0001", subagentJobRunning, "", "")
	svc.writeSubagentMeta("alice", plain, "queu0001", subagentJobQueued, "", "")
	svc.writeSubagentMeta("alice", worker, "work0001", subagentJobRunning, "", "")
	svc.writeSubagentMeta("alice", plain, "done0001", subagentJobCompleted, "", "")
	svc.writeSubagentMeta("bob", plain, "bobs0001", subagentJobRunning, "", "")
	if err := st.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	// Records of an agent whose agent.yaml is gone.
	svc.writeSubagentMeta("ghost", plain, "ghst0001", subagentJobRunning, "", "")

	n, told := svc.FailOrphanedSubagents()
	if n != 5 {
		t.Errorf("marked %d records, want 5", n)
	}
	if !slices.Equal(told, []string{"alice"}) {
		t.Errorf("told %v, want alice alone", told)
	}
	got := del.snapshot()
	if len(got) != 1 || len(got["alice"]) != 1 {
		t.Fatalf("deliveries = %v, want one to alice", got)
	}
	msg := got["alice"][0]
	if msg.Role != store.RoleReceived || msg.Kind != store.KindSubagentResult {
		t.Errorf("delivered %s/%s, want a received subagent_result", msg.Role, msg.Kind)
	}
	for _, want := range []string{
		"Kivali restarted, which ended 2 of your background tasks",
		"Background task lead0001 (price the parts) FAILED.",
		`"outcome": "failed"`,
		"Background task queu0001 (list the incidents) FAILED.",
		"error: " + subagentRestartError,
		"/files/subagents/queu0001/artifacts/private/",
		"Nothing else of yours is running",
	} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("message lacks %q:\n%s", want, msg.Content)
		}
	}
	for _, not := range []string{"work0001", "done0001"} {
		if strings.Contains(msg.Content, not) {
			t.Errorf("message names %s:\n%s", not, msg.Content)
		}
	}
	if meta := readMetaMap(t, st, "alice", "work0001"); meta["status"] != "errored" || meta["caller"] != "lead0001" {
		t.Errorf("worker record = %v", meta)
	}

	if n, told := svc.FailOrphanedSubagents(); n != 0 || len(told) != 0 {
		t.Errorf("second boot: marked %d, told %v", n, told)
	}
	if got := del.snapshot(); len(got["alice"]) != 1 {
		t.Errorf("second boot delivered again: %v", got)
	}
}

// The notice must reach the agent's chat at boot and wake it once its
// pod is up, through the spawn gate like a recovered turn: written but
// never woken, it would sit unread until a person wrote. This guards
// that RecoverInterruptedWork writes the row through deliverToAgent,
// that the gate reads it as owed a reply, and that the agent is in the
// list SpawnRecovered wakes, once.
func TestRecoverInterruptedWorkQueuesTheNoticeBehindTheSpawnGate(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	svc := &SubagentService{Store: st}
	srv, err := NewServer(&Server{Store: st, Provider: provider.MockProvider{}, SubagentService: svc})
	if err != nil {
		t.Fatal(err)
	}
	svc.writeSubagentMeta("alice", SubagentTaskInput{Description: "list the incidents", Prompt: "p"}, "lost0001", subagentJobRunning, "", "")
	// alice was mid-turn too: she is woken once, not twice.
	if err := st.MarkActiveTurn("alice", store.ActiveTurnMarker{StartedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Source: "chat"}); err != nil {
		t.Fatal(err)
	}

	wake := srv.RecoverInterruptedWork()
	if !slices.Equal(wake, []string{"alice"}) {
		t.Errorf("wake = %v, want alice once", wake)
	}
	hist, err := st.ReadChatHistory("alice")
	if err != nil {
		t.Fatal(err)
	}
	var notice *store.ChatMessage
	for i := range hist {
		if hist[i].Kind == store.KindSubagentResult {
			notice = &hist[i]
		}
	}
	if notice == nil || !strings.Contains(notice.Content, "lost0001") {
		t.Fatalf("alice's chat = %+v, want the restart notice", hist)
	}
	if v := store.SpawnDecision(hist); v != store.SpawnNow {
		t.Errorf("spawn gate verdict = %v, want SpawnNow", v)
	}
}

// A task record written before records said whose a worker was has no
// caller, and reads as the agent's own: it is reported rather than
// silently dropped. Guards that FailOrphanedSubagents keys on the
// caller field alone and copes with a record that lacks it.
func TestRestartReportsARecordWithoutCallerAsTheAgents(t *testing.T) {
	svc, st, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	del := &parentDeliveries{}
	svc.DeliverToParent = del.deliver
	path := subagentMetaPath(st.Root(), "alice", "old00001")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"id": "old00001", "status": subagentJobRunning, "description": "an older task"})
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, told := svc.FailOrphanedSubagents(); !slices.Equal(told, []string{"alice"}) {
		t.Errorf("told %v, want alice", told)
	}
	if got := del.snapshot()["alice"]; len(got) != 1 || !strings.Contains(got[0].Content, "Background task old00001 (an older task) FAILED.") {
		t.Errorf("alice got %v", got)
	}
}

// A worker's record must say whose it is, or a restart that ends it
// reports a sub-lead's intermediate work to the agent as its own task.
// This guards that a nested dispatch writes the caller into each
// worker's meta.json.
func TestWorkerRecordNamesItsCaller(t *testing.T) {
	svc, st, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	registerTestJob(svc, "lead0001", "alice", 1, subagentJobRunning)
	if _, err := svc.RunNestedBatch(context.Background(), "alice", "lead0001", nestedArgs(1)); err != nil {
		t.Fatal(err)
	}
	var worker string
	for _, j := range svc.listJobs("alice") {
		if j.CallerID == "lead0001" {
			worker = j.ID
		}
	}
	if worker == "" {
		t.Fatal("no worker registered")
	}
	if meta := readMetaMap(t, st, "alice", worker); meta["caller"] != "lead0001" {
		t.Errorf("worker record = %v, want caller lead0001", meta)
	}
}
