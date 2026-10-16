package web

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// signInRun is one web task in a site session whose first run met a
// sign-in wall and waited: the service, its deliveries, the fake clock
// the wait runs on, the job id, and the spec each run was given.
type signInRun struct {
	svc     *SubagentService
	del     *deliveryRecorder
	clk     *clock.Fake
	id      string
	mu      sync.Mutex
	specs   []agentpod.SubagentSpec
	entered chan struct{}
}

// startWaitingTask dispatches the task and returns once it waits. The
// first run calls web_wait_for_sign_in from inside the fake driver, as
// the subagent would; a later run is answered by second.
func startWaitingTask(t *testing.T, second func(svc *SubagentService, req fakeDriveCall) (string, error)) *signInRun {
	t.Helper()
	run := &signInRun{entered: make(chan struct{})}
	driver := &fakeSubagentDriver{}
	driver.respond = func(req fakeDriveCall) (string, error) {
		run.mu.Lock()
		run.specs = append(run.specs, req.Spec)
		n := len(run.specs)
		run.mu.Unlock()
		if n > 1 {
			return second(run.svc, req)
		}
		run.svc.recordCLISession(req.SubagentID, "cli-session-1")
		resp := run.svc.WebCommand(context.Background(), "alice", req.SubagentID, agentpod.WebCommandRequest{Command: agentpod.WebWaitForSignInCommand, Args: json.RawMessage(`{"reason":"mfa"}`)})
		if resp.IsError || !strings.Contains(resp.Text, "Your turn ends here") {
			t.Errorf("web_wait_for_sign_in = %+v", resp)
		}
		if again := run.svc.WebCommand(context.Background(), "alice", req.SubagentID, agentpod.WebCommandRequest{Command: "snapshot"}); !again.IsError || !strings.Contains(again.Text, "waiting for a person") {
			t.Errorf("a web call while waiting = %+v", again)
		}
		close(run.entered)
		// The run ends (core ends it after the grace; here the fake
		// just returns), with an error the wait makes irrelevant.
		return "", errors.New("the run was ended")
	}
	svc, _, del, _ := webService(t, driver)
	run.svc, run.del = svc, del
	run.clk = clock.NewFakeAt(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	svc.Clock = run.clk
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"session": "Shop.Vendor.example"})); err != nil {
		t.Fatal(err)
	}
	jobs := svc.listJobs("alice")
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	run.id = jobs[0].ID
	select {
	case <-run.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the task never waited for a sign-in")
	}
	return run
}

// TestWebTaskWaitsForSignInAndResumes: a web task in a site session
// that calls web_wait_for_sign_in waits, shows as waiting to the agent
// and to Home, takes input in its window, and when the window closes
// runs again in the same CLI session with core's line, to its result.
func TestWebTaskWaitsForSignInAndResumes(t *testing.T) {
	run := startWaitingTask(t, func(svc *SubagentService, req fakeDriveCall) (string, error) {
		if req.Spec.Resume != "cli-session-1" || req.Spec.UserPrompt != signInResumeLine {
			t.Errorf("resumed run: resume=%q prompt=%q", req.Spec.Resume, req.Spec.UserPrompt)
		}
		if _, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":12}}`)); isErr {
			t.Error("a valid result was refused")
		}
		return "", nil
	})
	j, _ := run.svc.lookupJob(run.id)
	if j.State != subagentJobWaiting || j.Site != "vendor.example" || j.Session != "vendor.example" || j.WaitReason != "mfa" {
		t.Fatalf("job while waiting: state=%s site=%s session=%s reason=%s", j.State, j.Site, j.Session, j.WaitReason)
	}
	run.mu.Lock()
	first := run.specs[0]
	run.mu.Unlock()
	if first.Site != "vendor.example" || !slices.Contains(first.Tools.Kivali, agentpod.WebWaitForSignInTool) || !strings.Contains(first.SystemPrompt, "LOCKED TO ONE SITE") || !strings.Contains(first.SystemPrompt, "web_wait_for_sign_in") {
		t.Errorf("first spec: site=%q tools=%v", first.Site, first.Tools.Kivali)
	}
	if status := run.svc.SubagentStatus("alice"); !strings.Contains(status, "WAITING FOR A PERSON") || !strings.Contains(status, "a second factor on vendor.example") {
		t.Errorf("status = %s", status)
	}
	if waiting := run.svc.waitingWebJobs(); len(waiting) != 1 || waiting[0].ID != run.id {
		t.Errorf("waitingWebJobs = %+v", waiting)
	}
	if !run.svc.windowTakesInput(run.id) {
		t.Error("the window takes no input while the task waits")
	}

	// The person closes the window: the task resumes in its session.
	run.svc.windowsClosed("alice", "vendor.example", run.id)
	body := run.del.waitFor(1)[0].Content
	if !strings.Contains(body, `"Acme"`) || !strings.Contains(body, "vendor.example") {
		t.Errorf("delivered = %s", body)
	}
	run.mu.Lock()
	runs := len(run.specs)
	run.mu.Unlock()
	if runs != 2 {
		t.Errorf("runs = %d, want 2", runs)
	}
	if j, _ := run.svc.lookupJob(run.id); j.State != subagentJobCompleted || j.WaitReason != "" {
		t.Errorf("final job: state=%s reason=%q", j.State, j.WaitReason)
	}
	if run.svc.windowTakesInput(run.id) {
		t.Error("the window still takes input after the task ended")
	}
}

// TestWebTaskWaitForSignInEndsBlockedWhenNobodyComes: after the wait
// limit the task ends on its own, outcome blocked, user_did_not_respond,
// and never runs again.
func TestWebTaskWaitForSignInEndsBlockedWhenNobodyComes(t *testing.T) {
	run := startWaitingTask(t, func(_ *SubagentService, _ fakeDriveCall) (string, error) {
		t.Error("the task ran again after the wait limit")
		return "", nil
	})
	run.clk.Advance(signInWaitLimit)
	body := run.del.waitFor(1)[0].Content
	if !strings.Contains(body, `"blocked"`) || !strings.Contains(body, string(structured.BlockedUserDidNotRespond)) {
		t.Errorf("delivered = %s", body)
	}
	if j, _ := run.svc.lookupJob(run.id); j.State != subagentJobCompleted {
		t.Errorf("final state = %s", j.State)
	}
}

// TestWebTaskWaitingForSignInCanBeCancelled: subagent_cancel ends a
// waiting task like any other, with nothing delivered.
func TestWebTaskWaitingForSignInCanBeCancelled(t *testing.T) {
	run := startWaitingTask(t, func(_ *SubagentService, _ fakeDriveCall) (string, error) {
		t.Error("the task ran again after it was cancelled")
		return "", nil
	})
	if !run.svc.CancelSubagent("alice", run.id) {
		t.Fatal("cancel refused")
	}
	j, _ := run.svc.lookupJob(run.id)
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled task never ended")
	}
	if j, _ := run.svc.lookupJob(run.id); j.State != subagentJobCancelled {
		t.Errorf("state after cancel = %s", j.State)
	}
	if n := len(run.del.waitFor(0)); n != 0 {
		t.Errorf("a cancelled task delivered %d message(s)", n)
	}
}

// TestWaitForSignInRefusals: the open session, a task a subagent
// dispatched, and a bad reason are refused with what to do instead.
func TestWaitForSignInRefusals(t *testing.T) {
	svc, _, _, _ := webService(t, &fakeSubagentDriver{})
	schema, _ := structured.Parse([]byte(quoteSchema))
	svc.registerJob(&subagentJob{ID: "o1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: agentpod.OpenWebSession, State: subagentJobRunning, Schema: schema, Depth: 1, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "n2", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.example", Site: "vendor.example", State: subagentJobRunning, Schema: schema, Depth: 2, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "s1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.example", Site: "vendor.example", State: subagentJobRunning, Schema: schema, Depth: 1, done: make(chan struct{})})
	ctx := context.Background()
	wait := func(id, args string) agentpod.WebCommandResponse {
		return svc.WebCommand(ctx, "alice", id, agentpod.WebCommandRequest{Command: agentpod.WebWaitForSignInCommand, Args: json.RawMessage(args)})
	}
	if r := wait("o1", `{"reason":"sign_in"}`); !r.IsError || !strings.Contains(r.Text, "open session") {
		t.Errorf("open session: %+v", r)
	}
	if r := wait("n2", `{"reason":"sign_in"}`); !r.IsError || !strings.Contains(r.Text, "dispatched by a subagent") {
		t.Errorf("nested: %+v", r)
	}
	if r := wait("s1", `{"reason":"coffee"}`); !r.IsError || !strings.Contains(r.Text, "reason must be") {
		t.Errorf("bad reason: %+v", r)
	}
	if j, _ := svc.lookupJob("s1"); j.State != subagentJobRunning {
		t.Errorf("a refused wait changed the state to %s", j.State)
	}
}

// TestWebSessionSite: a session name is the open session or a site's
// registrable domain; addresses, suffixes and URLs are refused.
func TestWebSessionSite(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"open", ""}, {"vendor.com", "vendor.com"}, {"Shop.Vendor.COM.", "vendor.com"},
		{"docs.vendor.co.uk", "vendor.co.uk"}, {"sub.example.github.io", "example.github.io"},
	} {
		got, err := webSessionSite(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("webSessionSite(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "co.uk", "10.0.0.1", "::1", "https://vendor.com", "vendor.com/shop", "vendor.com:8443", "localhost", "vendor com"} {
		if got, err := webSessionSite(in); err == nil {
			t.Errorf("webSessionSite(%q) = %q, want a refusal", in, got)
		}
	}
}

// TestWebSessionsTextAndReset: the agent's sessions are listed by name
// and state, and a reset forgets one, by any spelling of its site.
func TestWebSessionsTextAndReset(t *testing.T) {
	svc, _, _, _ := webService(t, &fakeSubagentDriver{})
	now := svc.clk().Now()
	if _, err := svc.Store.EnsureBrowserSession("alice", "vendor.example", "vendor.example", now); err != nil {
		t.Fatal(err)
	}
	text := svc.WebSessionsText("alice")
	if !strings.Contains(text, "open — signed in nowhere") || !strings.Contains(text, "vendor.example") || !strings.Contains(text, "no browser running") {
		t.Errorf("sessions text = %s", text)
	}
	out, err := svc.ResetWebSession("alice", "VENDOR.example.")
	if err != nil || !strings.Contains(out, "vendor.example was reset") {
		t.Errorf("reset = %q, %v", out, err)
	}
	if sessions, _ := svc.Store.ListBrowserSessions("alice"); len(sessions) != 0 {
		t.Errorf("session kept after reset: %+v", sessions)
	}
	if _, err := svc.ResetWebSession("alice", "co.uk"); err == nil {
		t.Error("a suffix was accepted as a session")
	}
	if !strings.Contains(svc.WebSessionsText("alice"), "No site sessions yet") {
		t.Errorf("sessions text after reset = %s", svc.WebSessionsText("alice"))
	}
}

// TestBrowserRegistryWindowInputAndReset: input goes to the shown tab
// only, a state can be re-sent with the input flag and the site, and a
// reset ends the browser and deletes the stored session.
func TestBrowserRegistryWindowInputAndReset(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", true)
	if err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeScreencast)
	if ev := nextEvent(t, w); !ev.Input || ev.Site != "vendor.example" {
		t.Fatalf("first state event = %+v, want input on and the site", ev)
	}
	press := apitypes.BrowserInput{Kind: "mouse", Action: "pressed", X: 10, Y: 20, Button: "left"}
	if err := f.reg.Input(w, press); err == nil || !strings.Contains(err.Error(), "no tab is shown") {
		t.Errorf("input before a frame: %v", err)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: "t1", URL: "https://vendor.example/login"}, nil)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t1", Bytes: 1}, strings.NewReader("x"))
	nextEvent(t, w)
	nextFrame(t, w)
	if err := f.reg.Input(w, press); err != nil {
		t.Fatal(err)
	}
	in := link.next(t, browserpod.TypeInput)
	if in.msg.Group != "job1" || in.msg.Tab != "t1" || !strings.Contains(string(in.msg.Args), `"kind":"mouse"`) || !strings.Contains(string(in.msg.Args), `"button":"left"`) {
		t.Errorf("input frame = %+v %s", in.msg, in.msg.Args)
	}
	f.reg.BroadcastState("alice", "vendor.example", "job1", false)
	if ev := nextEvent(t, w); ev.Type != apitypes.BrowserWindowEventTypeState || ev.Input || ev.Site != "vendor.example" {
		t.Errorf("broadcast state = %+v", ev)
	}
	if _, err := f.st.EnsureBrowserSession("alice", "vendor.example", "vendor.example", f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Reset("alice", "vendor.example"); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, w); ev.Type != apitypes.BrowserWindowEventTypeEnd {
		t.Errorf("event after reset = %+v", ev)
	}
	f.stopped(t)
	if sessions, _ := f.st.ListBrowserSessions("alice"); len(sessions) != 0 {
		t.Errorf("session kept after reset: %+v", sessions)
	}
}
