package web

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// fakeSubagentDriver is a test stand-in for SubagentDriver. Every
// DriveSubagent call records the request, optionally writes outputs
// into the subagent's /files/outputs/, and returns a configurable
// result. Concurrent calls track the peak inflight count so we can
// assert on parallel fan-out.
type fakeSubagentDriver struct {
	mu       sync.Mutex
	requests []fakeDriveCall
	inflight int32
	maxSeen  int32
	respond  func(req fakeDriveCall) (string, error)
}

type fakeDriveCall struct {
	Parent     string
	TurnID     string
	SubagentID string
	Spec       agentpod.SubagentSpec
}

func (f *fakeSubagentDriver) DriveSubagent(_ context.Context, parent, turnID, subagentID string, spec agentpod.SubagentSpec) (string, error) {
	cur := atomic.AddInt32(&f.inflight, 1)
	defer atomic.AddInt32(&f.inflight, -1)
	for {
		old := atomic.LoadInt32(&f.maxSeen)
		if cur <= old || atomic.CompareAndSwapInt32(&f.maxSeen, old, cur) {
			break
		}
	}
	call := fakeDriveCall{Parent: parent, TurnID: turnID, SubagentID: subagentID, Spec: spec}
	f.mu.Lock()
	f.requests = append(f.requests, call)
	f.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	if f.respond == nil {
		return "ok: " + spec.Description, nil
	}
	return f.respond(call)
}

// hubRecorder records every event emitted to a hub via the
// SubagentService. Lets tests assert on the sequence of subagent_*
// events without spinning up a real chat hub.
type hubRecorder struct {
	mu     sync.Mutex
	events []hubRecorderEvent
}

type hubRecorderEvent struct {
	Parent string
	Kind   string
	Data   map[string]any
}

func (r *hubRecorder) emit(parent, kind string, payload any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, _ := json.Marshal(payload)
	var data map[string]any
	_ = json.Unmarshal(body, &data)
	r.events = append(r.events, hubRecorderEvent{Parent: parent, Kind: kind, Data: data})
}

func (r *hubRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

func newSubagentTestService(t *testing.T, driver SubagentDriver) (*SubagentService, *store.FSStore, string, *hubRecorder) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	if err := files.Sync(files.BootstrapOptions{AgentRoot: filepath.Join(s.Root(), "agents", "alice")}); err != nil {
		t.Fatal(err)
	}
	parentRoot := files.StorageRoot(filepath.Join(s.Root(), "agents", "alice"))
	rec := &hubRecorder{}
	svc := &SubagentService{
		Provider:     provider.MockProvider{},
		Store:        s,
		Driver:       driver,
		EmitToParent: rec.emit,
		// Sink by default. Tests that care about what came back use
		// newSubagentTestServiceWithDeliveries instead; the rest just
		// need finishJob's delivery step not to log about a missing
		// hook on every job.
		DeliverToParent: func(string, store.ChatMessage) error { return nil },
	}
	return svc, s, parentRoot, rec
}

// deliveryRecorder captures the result messages a finished job hands
// to its parent, and lets a test block until they have landed.
//
// Dispatch is fire-and-forget, so "the work is done" is not something
// a test can observe by the call returning. Waiting on the
// delivery channel is the synchronization point: a real signal from
// the code under test, not a sleep long enough to probably be enough.
type deliveryRecorder struct {
	t    *testing.T
	mu   sync.Mutex
	msgs []store.ChatMessage
	ch   chan struct{}
}

func newDeliveryRecorder(t *testing.T) *deliveryRecorder {
	return &deliveryRecorder{t: t, ch: make(chan struct{}, 64)}
}

func (d *deliveryRecorder) deliver(_ string, msg store.ChatMessage) error {
	d.mu.Lock()
	d.msgs = append(d.msgs, msg)
	d.mu.Unlock()
	d.ch <- struct{}{}
	return nil
}

// waitFor blocks until at least n results have been delivered IN
// TOTAL, and returns everything delivered so far.
//
// Total rather than "n more", so successive calls compose: a test that
// waits for the first result and later for both asks for 1 then 2, not
// 1 then 1. Each iteration blocks on a real delivery signal — the loop
// re-checks only after the code under test has done something, so this
// is a channel wait and not a poll. The deadline is a deadlock
// diagnostic, so a regression reports "waited for 2, only 1 arrived"
// instead of hanging until the package timeout.
func (d *deliveryRecorder) waitFor(n int) []store.ChatMessage {
	d.t.Helper()
	for {
		d.mu.Lock()
		got := len(d.msgs)
		d.mu.Unlock()
		if got >= n {
			break
		}
		select {
		case <-d.ch:
		case <-time.After(15 * time.Second):
			d.t.Fatalf("waited for %d deliveries, only %d arrived", n, got)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]store.ChatMessage(nil), d.msgs...)
}

// newSubagentTestServiceWithDeliveries is newSubagentTestService plus a
// handle on what got delivered — the shape most tests want now that
// results arrive after dispatch returns.
func newSubagentTestServiceWithDeliveries(t *testing.T, driver SubagentDriver) (*SubagentService, *store.FSStore, string, *hubRecorder, *deliveryRecorder) {
	t.Helper()
	svc, s, parentRoot, rec := newSubagentTestService(t, driver)
	del := newDeliveryRecorder(t)
	svc.DeliverToParent = del.deliver
	return svc, s, parentRoot, rec, del
}

// TestSubagentServiceDispatchesWithoutBlocking is the core of the
// async contract: StartBatch returns a receipt while the work is still
// running, and each answer arrives later as its own delivered message.
func TestSubagentServiceDispatchesWithoutBlocking(t *testing.T) {
	// release gates the fake driver so every task is provably still
	// in flight when StartBatch returns. Without it a fast fake could
	// finish first and the test would pass even if dispatch blocked.
	release := make(chan struct{})
	driver := &fakeSubagentDriver{
		respond: func(req fakeDriveCall) (string, error) {
			<-release
			return "ok: " + req.Spec.Description, nil
		},
	}
	svc, _, _, rec, del := newSubagentTestServiceWithDeliveries(t, driver)

	args, _ := json.Marshal(map[string]any{
		"tasks": []map[string]any{
			{"description": "task one", "prompt": "do A"},
			{"description": "task two", "prompt": "do B"},
			{"description": "task three", "prompt": "do C"},
		},
	})
	rendered, err := svc.StartBatch(context.Background(), "alice", args)
	if err != nil {
		t.Fatalf("StartBatch: %v", err)
	}

	// Returned while everything is still blocked on `release`.
	if n := svc.OutstandingSubagents("alice"); n != 3 {
		t.Errorf("outstanding after dispatch = %d, want 3 — dispatch should not have waited", n)
	}
	if strings.Contains(rendered, "ok: task one") {
		t.Errorf("receipt contains a result; dispatch blocked:\n%s", rendered)
	}
	for _, want := range []string{"task one", "task two", "task three", "Dispatched 3 background task"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("receipt missing %q\n%s", want, rendered)
		}
	}

	close(release)
	msgs := del.waitFor(3)

	if len(driver.requests) != 3 {
		t.Errorf("ran %d, want 3", len(driver.requests))
	}
	if max := atomic.LoadInt32(&driver.maxSeen); max < 2 {
		t.Errorf("expected concurrent execution; peak inflight = %d", max)
	}
	joined := ""
	for _, m := range msgs {
		if m.Kind != store.KindSubagentResult {
			t.Errorf("delivered kind = %q, want %q", m.Kind, store.KindSubagentResult)
		}
		if m.Role != store.RoleReceived {
			t.Errorf("delivered role = %q, want received (the spawn gate keys off it)", m.Role)
		}
		joined += m.Content + "\n"
	}
	for _, want := range []string{"ok: task one", "ok: task two", "ok: task three"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no delivery carried %q\n%s", want, joined)
		}
	}
	if n := svc.OutstandingSubagents("alice"); n != 0 {
		t.Errorf("outstanding after completion = %d, want 0 — the parent would show working forever", n)
	}

	kinds := rec.kinds()
	if len(kinds) < 2 || kinds[0] != "subagent_started" || kinds[len(kinds)-1] != "subagent_completed" {
		t.Errorf("event sequence wrong: %v", kinds)
	}
}

func TestSubagentServiceEnforcesBatchCap(t *testing.T) {
	svc, _, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	tasks := make([]map[string]any, 0, 20)
	for i := 0; i < 10; i++ {
		tasks = append(tasks, map[string]any{"description": "x", "prompt": "y"})
	}
	args, _ := json.Marshal(map[string]any{"tasks": tasks})
	_, err := svc.StartBatch(context.Background(), "alice", args)
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("expected batch-cap error, got %v", err)
	}
}

func TestSubagentServiceValidatesInput(t *testing.T) {
	svc, _, _, _ := newSubagentTestService(t, &fakeSubagentDriver{})
	cases := []struct {
		name string
		body map[string]any
	}{
		{"no tasks", map[string]any{"tasks": []map[string]any{}}},
		{"missing description", map[string]any{"tasks": []map[string]any{{"prompt": "p"}}}},
		{"missing prompt", map[string]any{"tasks": []map[string]any{{"description": "d"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(tc.body)
			if _, err := svc.StartBatch(context.Background(), "alice", args); err == nil {
				t.Error("expected error")
			}
		})
	}
}

// TestSubagentServiceBuildsOverlayAndPersistsArtifacts proves the
// post-unified-files contract: SubagentService builds the symlink
// overlay before driving the subagent, and any file the subagent
// writes under artifacts/private/ stays where it landed (no copy
// step) — the parent file_views the same path.
func TestSubagentServiceBuildsOverlayAndPersistsArtifacts(t *testing.T) {
	driver := &fakeSubagentDriver{}
	svc, _, parentRoot, _, del := newSubagentTestServiceWithDeliveries(t, driver)

	driver.respond = func(req fakeDriveCall) (string, error) {
		// At drive time, the overlay should already exist with the
		// real artifacts/private/ dir + relative symlinks.
		overlayRoot := filepath.Join(parentRoot, "subagents", req.SubagentID)
		if info, err := os.Stat(filepath.Join(overlayRoot, "artifacts", "private")); err != nil || !info.IsDir() {
			t.Errorf("overlay artifacts/private missing: err=%v info=%+v", err, info)
		}
		if _, err := os.Readlink(filepath.Join(overlayRoot, "project")); err != nil {
			t.Errorf("overlay project symlink missing: %v", err)
		}
		// Mimic the subagent writing a deliverable.
		_ = os.WriteFile(filepath.Join(overlayRoot, "artifacts", "private", "result.txt"), []byte("done"), 0o644)
		return "all set", nil
	}

	args, _ := json.Marshal(map[string]any{
		"tasks": []map[string]any{
			{"description": "summarize", "prompt": "do it"},
		},
	})
	rendered, err := svc.StartBatch(context.Background(), "alice", args)
	if err != nil {
		t.Fatal(err)
	}
	// The overlay assertions live inside the driver callback, which
	// now runs on the job's goroutine — wait for the delivery before
	// reading anything it produced.
	del.waitFor(1)
	if len(driver.requests) != 1 {
		t.Fatalf("requests = %d", len(driver.requests))
	}
	got := driver.requests[0]
	if got.Parent != "alice" || got.SubagentID == "" || got.TurnID == "" {
		t.Errorf("request: %+v", got)
	}
	// The subagent's deliverable persists at the overlay path — no
	// copy step needed; the parent file_views it at the same path.
	parentVisible := filepath.Join(parentRoot, "subagents", got.SubagentID, "artifacts", "private", "result.txt")
	if _, err := os.Stat(parentVisible); err != nil {
		t.Errorf("subagent's artifact missing in parent's view: %v", err)
	}
	// Render carries the per-subagent artifacts pointer so the parent
	// model knows where to look.
	if !strings.Contains(rendered, "/files/subagents/"+got.SubagentID+"/artifacts/private/") {
		t.Errorf("rendered missing artifacts pointer: %s", rendered)
	}
}

// TestDeriveCurrentActivityFromTranscript locks in the activity-string
// derivation so the next refactor of the chat.jsonl shape catches if
// the watcher silently regresses to "running…" forever (which is what
// happened the first time we shipped — the watcher polled the wrong
// path and there was no test to catch it).
func TestDeriveCurrentActivityFromTranscript(t *testing.T) {
	dir := t.TempDir()
	chatPath := filepath.Join(dir, "chat.jsonl")
	write := func(msgs []store.ChatMessage) {
		t.Helper()
		f, err := os.Create(chatPath)
		if err != nil {
			t.Fatal(err)
		}
		enc := json.NewEncoder(f)
		for _, m := range msgs {
			if err := enc.Encode(m); err != nil {
				t.Fatal(err)
			}
		}
		_ = f.Close()
	}

	// Empty / missing file → "".
	if got := deriveCurrentActivity(filepath.Join(dir, "missing.jsonl")); got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
	write(nil)
	if got := deriveCurrentActivity(chatPath); got != "" {
		t.Errorf("empty: got %q", got)
	}

	// Last entry is an in-flight tool_use → "running <name>".
	write([]store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "go", TS: time.Now()},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u1", ToolName: "run_shell", TS: time.Now()},
	})
	if got := deriveCurrentActivity(chatPath); got != "running run_shell" {
		t.Errorf("in-flight tool_use: got %q, want %q", got, "running run_shell")
	}

	// tool_use followed by tool_result → "thinking".
	write([]store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "go", TS: time.Now()},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u1", ToolName: "run_shell", TS: time.Now()},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "u1", Content: "ok", TS: time.Now()},
	})
	if got := deriveCurrentActivity(chatPath); got != "thinking" {
		t.Errorf("post-tool_result: got %q, want thinking", got)
	}

	// tool_result alone (last entry) → "thinking".
	write([]store.ChatMessage{
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "u1", Content: "ok", TS: time.Now()},
	})
	if got := deriveCurrentActivity(chatPath); got != "thinking" {
		t.Errorf("tool_result tail: got %q, want thinking", got)
	}

	// Final assistant text → "responding".
	write([]store.ChatMessage{
		{Role: store.RoleSent, Kind: "direct_chat", Content: "answer", TS: time.Now()},
	})
	if got := deriveCurrentActivity(chatPath); got != "responding" {
		t.Errorf("final text: got %q, want responding", got)
	}
}

func TestSubagentServiceSurfacesDriverError(t *testing.T) {
	boom := errors.New("subagent crashed")
	driver := &fakeSubagentDriver{
		respond: func(_ fakeDriveCall) (string, error) {
			return "", boom
		},
	}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	args, _ := json.Marshal(map[string]any{
		"tasks": []map[string]any{{"description": "x", "prompt": "y"}},
	})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Fatalf("StartBatch returned error (a failing TASK is not a failing DISPATCH): %v", err)
	}
	// The failure now reaches the parent the same way a success does —
	// as a delivered message. It must still arrive: a job that dies
	// silently leaves the parent showing work that never lands.
	msgs := del.waitFor(1)
	if !strings.Contains(msgs[0].Content, "subagent crashed") {
		t.Errorf("delivered message missing the error: %s", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "FAILED") {
		t.Errorf("delivered message should say the task failed: %s", msgs[0].Content)
	}
	if n := svc.OutstandingSubagents("alice"); n != 0 {
		t.Errorf("failed job left %d outstanding; parent would show working forever", n)
	}
}
