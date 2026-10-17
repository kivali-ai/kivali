package web

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// signInRun is one web task in a site session whose first run met a
// sign-in wall and waited: the service, its deliveries, the fake clock
// the wait and the browser registry run on, the registry and its fake
// provisioner, the job id, the spec each run was given, and the status
// of every subagent_progress event the chip was sent.
type signInRun struct {
	svc      *SubagentService
	del      *deliveryRecorder
	clk      *clock.Fake
	reg      *browserRegistry
	prov     *fakeBrowserProv
	browser  *browser
	id       string
	mu       sync.Mutex
	specs    []agentpod.SubagentSpec
	entered  chan struct{}
	progress chan string
}

// startWaitingTask dispatches the task and returns once it waits. The
// first run calls web_wait_for_sign_in from inside the fake driver, as
// the subagent would, after beforeWait (when given) has run against
// the running task; a later run is answered by second.
//
// The registry is on the task's fake clock and reports to the service
// the way NewServer wires it, so a test can drive a window's life
// through the registry itself.
func startWaitingTask(t *testing.T, second func(svc *SubagentService, req fakeDriveCall) (string, error), beforeWait func(run *signInRun)) *signInRun {
	t.Helper()
	run := &signInRun{entered: make(chan struct{}), progress: make(chan string, 64)}
	driver := &fakeSubagentDriver{}
	driver.respond = func(req fakeDriveCall) (string, error) {
		run.mu.Lock()
		run.specs = append(run.specs, req.Spec)
		n := len(run.specs)
		run.mu.Unlock()
		if n > 1 {
			return second(run.svc, req)
		}
		run.mu.Lock()
		run.id = req.SubagentID
		run.mu.Unlock()
		if beforeWait != nil {
			beforeWait(run)
		}
		run.svc.recordCLISession(req.SubagentID, "5f1c7a3e-2b4d-4e6f-8a9b-0c1d2e3f4a5b")
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
	run.prov = newFakeBrowserProv()
	run.reg = newBrowserRegistry(svc.Store, run.prov, run.clk, 3, t.Logf, func(slug, id string) string { return subagentFilesRoot(svc.Store, slug, id) })
	run.reg.markBooted()
	run.reg.OnState = svc.browserStateChanged
	run.reg.OnWindowsClosed = svc.windowsClosed
	svc.Browsers = run.reg
	svc.EmitToParent = func(_, kind string, payload any) {
		if kind != "subagent_progress" {
			return
		}
		if m, ok := payload.(map[string]any); ok {
			run.progress <- m["status"].(string)
		}
	}
	if _, err := svc.StartBatch(context.Background(), "alice", webArgs(t, map[string]any{"session": "Shop.Vendor.com"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the task never waited for a sign-in")
	}
	jobs := svc.listJobs("alice")
	if len(jobs) != 1 || jobs[0].ID != run.id {
		t.Fatalf("jobs = %+v, want the one task", jobs)
	}
	return run
}

// submitQuote is a resumed run that submits a valid result.
func submitQuote(t *testing.T) func(svc *SubagentService, req fakeDriveCall) (string, error) {
	return func(svc *SubagentService, req fakeDriveCall) (string, error) {
		if _, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":12}}`)); isErr {
			t.Error("a valid result was refused")
		}
		return "", nil
	}
}

// closeWindows is the registry's report that the task's last window has
// been gone for the grace, as the registry makes it: browserWindowGrace
// after the window went.
func (run *signInRun) closeWindows() {
	run.clk.Advance(browserWindowGrace)
	run.svc.windowsClosed("alice", "vendor.com", run.id)
}

// runs is how many times the task has been driven.
func (run *signInRun) runs() int {
	run.mu.Lock()
	defer run.mu.Unlock()
	return len(run.specs)
}

// nextProgress is the status of the next subagent_progress event the
// chip was sent; the deadline is a deadlock diagnostic.
func (run *signInRun) nextProgress(t *testing.T) string {
	t.Helper()
	select {
	case st := <-run.progress:
		return st
	case <-time.After(10 * time.Second):
		t.Fatal("no subagent_progress event arrived")
		return ""
	}
}

// playBrowser answers the provisioner's start of alice's vendor.com
// browser as the pod would, up to running.
func (run *signInRun) playBrowser(t *testing.T) *fakeLink {
	t.Helper()
	var token string
	select {
	case token = <-run.prov.startedC:
	case <-time.After(10 * time.Second):
		t.Fatal("the provisioner was not asked to start a pod")
	}
	link := newFakeLink()
	b := run.reg.claim("alice", "vendor.com", token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := run.reg.attach(b); err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeStart)
	run.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	run.mu.Lock()
	run.browser = b
	run.mu.Unlock()
	return link
}

// openWindow attaches a browser window to the task through the
// registry, playing the pod when the window is what starts the browser.
func (run *signInRun) openWindow(t *testing.T, startsBrowser bool) *browserWatcher {
	t.Helper()
	type outcome struct {
		w   *browserWatcher
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		w, err := run.reg.Watch(context.Background(), "alice", "vendor.com", run.id, run.svc.windowTakesInput(run.id))
		got <- outcome{w, err}
	}()
	if startsBrowser {
		run.playBrowser(t)
	}
	select {
	case o := <-got:
		if o.err != nil {
			t.Fatalf("Watch: %v", o.err)
		}
		return o.w
	case <-time.After(10 * time.Second):
		t.Fatal("the window never attached")
		return nil
	}
}

// closeWindowThroughRegistry is a window leaving and the grace running
// out, the registry sweeping as its lifecycle would.
func (run *signInRun) closeWindowThroughRegistry(w *browserWatcher) {
	run.reg.Unwatch(w)
	run.clk.Advance(browserWindowGrace)
	run.reg.sweep(run.clk.Now())
}

// TestNewServerWiresTheRegistryToTheService: the registry's reports
// reach the service, and the service can ask the hub about the pod.
func TestNewServerWiresTheRegistryToTheService(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(&Server{
		Store: st, Provider: provider.MockProvider{}, AgentpodHub: newAgentpodHub(),
		SubagentService: &SubagentService{Store: st}, BrowserProvisioner: newFakeBrowserProv(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Browsers == nil || srv.Browsers.OnState == nil || srv.Browsers.OnWindowsClosed == nil {
		t.Fatal("the registry does not report to the service")
	}
	if srv.SubagentService.PodConnected == nil || srv.SubagentService.PodConnected("alice") {
		t.Error("the service does not ask the hub about the pod, or thinks an unsubscribed pod is connected")
	}
	sub := srv.AgentpodHub.Subscribe("alice")
	defer srv.AgentpodHub.Unsubscribe(sub)
	if !srv.SubagentService.PodConnected("alice") {
		t.Error("a subscribed pod reads as not connected")
	}
}

// TestWindowClosedThroughTheRegistryResumesTheTask: a window opened on
// the waiting task and closed, once the grace has run out, resumes it
// by way of the registry's own report.
func TestWindowClosedThroughTheRegistryResumesTheTask(t *testing.T) {
	run := startWaitingTask(t, submitQuote(t), nil)
	w := run.openWindow(t, true)
	if ev := nextEvent(t, w); !ev.Input {
		t.Errorf("the window on a waiting task takes no input: %+v", ev)
	}
	run.closeWindowThroughRegistry(w)
	body := run.del.waitFor(1)[0].Content
	if !strings.Contains(body, `"Acme"`) {
		t.Errorf("delivered = %s", body)
	}
	if run.runs() != 2 {
		t.Errorf("runs = %d, want 2", run.runs())
	}
}

// TestWindowClosedBeforeTheWaitBeganDoesNotResume: a window that went
// before the task asked for anyone is not a person who has signed in.
// The report of its close, which comes the grace after it went, is
// ignored, and only a window closed after the wait began resumes the
// task.
func TestWindowClosedBeforeTheWaitBeganDoesNotResume(t *testing.T) {
	run := startWaitingTask(t, submitQuote(t), func(run *signInRun) {
		// Someone watches the running task and leaves; three seconds
		// later the task hits the sign-in wall.
		w := run.openWindow(t, true)
		run.reg.Unwatch(w)
		run.clk.Advance(3 * time.Second)
	})
	run.clk.Advance(browserWindowGrace - 3*time.Second)
	run.reg.sweep(run.clk.Now())
	if !run.svc.jobWaiting(run.id) || run.runs() != 1 {
		t.Fatalf("a window closed before the wait resumed the task: waiting=%v runs=%d", run.svc.jobWaiting(run.id), run.runs())
	}
	w := run.openWindow(t, false)
	run.closeWindowThroughRegistry(w)
	if body := run.del.waitFor(1)[0].Content; !strings.Contains(body, `"Acme"`) {
		t.Errorf("delivered = %s", body)
	}
}

// TestResumeWaitsForTheAgentPod: a task resumed while its agent pod is
// not connected reads as running (the need is gone) and is driven once
// the pod is back, with core's line in its transcript and the chip told
// of each state.
func TestResumeWaitsForTheAgentPod(t *testing.T) {
	run := startWaitingTask(t, submitQuote(t), nil)
	for _, want := range []string{subagentJobQueued, subagentJobRunning, subagentJobWaiting} {
		if got := run.nextProgress(t); got != want {
			t.Errorf("progress = %q, want %q", got, want)
		}
	}
	var connected atomic.Bool
	checks := make(chan bool, 16)
	run.svc.PodConnected = func(string) bool {
		if run.svc.jobWaiting(run.id) {
			t.Error("the task still reads as waiting while its run is brought back")
		}
		c := connected.Load()
		checks <- c
		return c
	}
	// Armed on the clock while the task waits: the activity watcher's
	// ticker, the wait limit and the run's end after the grace.
	run.clk.BlockUntil(3)
	run.closeWindows()
	if got := run.nextProgress(t); got != subagentJobRunning {
		t.Errorf("progress on resume = %q, want running", got)
	}
	if <-checks {
		t.Fatal("the first check found a pod")
	}
	// The pod wait's limit and its ticker join the watcher and the
	// wait limit (the run's end has fired).
	run.clk.BlockUntil(4)
	run.clk.Advance(signInResumePodPoll)
	if <-checks || run.runs() != 1 {
		t.Fatalf("the task was driven without a pod: runs=%d", run.runs())
	}
	connected.Store(true)
	run.clk.Advance(signInResumePodPoll)
	if !<-checks {
		t.Fatal("the pod's return went unseen")
	}
	if body := run.del.waitFor(1)[0].Content; !strings.Contains(body, `"Acme"`) {
		t.Errorf("delivered = %s", body)
	}
	hist, err := readSubagentHistory(subagentChatPath(run.svc.Store.Root(), "alice", run.id))
	if err != nil || len(hist) < 2 || hist[len(hist)-1].Content != signInResumeLine || hist[len(hist)-1].Role != store.RoleReceived {
		t.Errorf("transcript lacks core's resume line: %+v, %v", hist, err)
	}
	if got := run.nextProgress(t); got != "completed" {
		t.Errorf("final progress = %q, want completed", got)
	}
}

// TestResumeWithoutAPodReentersTheWait: when no pod comes within the
// bound the task waits on as it was, need and all, and a later window
// closing resumes it.
func TestResumeWithoutAPodReentersTheWait(t *testing.T) {
	run := startWaitingTask(t, submitQuote(t), nil)
	var connected atomic.Bool
	run.svc.PodConnected = func(string) bool { return connected.Load() }
	run.clk.BlockUntil(3)
	run.closeWindows()
	for _, want := range []string{subagentJobQueued, subagentJobRunning, subagentJobWaiting, subagentJobRunning} {
		if got := run.nextProgress(t); got != want {
			t.Errorf("progress = %q, want %q", got, want)
		}
	}
	run.clk.BlockUntil(4)
	run.clk.Advance(signInResumePodWait)
	if got := run.nextProgress(t); got != subagentJobWaiting {
		t.Fatalf("progress after the pod wait = %q, want waiting", got)
	}
	j, _ := run.svc.lookupJob(run.id)
	if j.State != subagentJobWaiting || j.WaitReason != "mfa" || run.runs() != 1 {
		t.Fatalf("job after the pod wait: state=%s reason=%q runs=%d", j.State, j.WaitReason, run.runs())
	}
	if keys := run.svc.signInWaitKeys(); len(keys) != 1 || keys[0] != "alice/"+run.id {
		t.Errorf("signInWaitKeys = %v", keys)
	}
	connected.Store(true)
	run.closeWindows()
	if body := run.del.waitFor(1)[0].Content; !strings.Contains(body, `"Acme"`) {
		t.Errorf("delivered = %s", body)
	}
}

// TestSignInWaitsAreCappedAtThree: the fourth web_wait_for_sign_in is
// refused, saying so, and the task ends blocked on what the page kept
// asking for.
func TestSignInWaitsAreCappedAtThree(t *testing.T) {
	waited := make(chan agentpod.WebCommandResponse, 8)
	run := startWaitingTask(t, func(svc *SubagentService, req fakeDriveCall) (string, error) {
		resp := svc.WebCommand(context.Background(), "alice", req.SubagentID, agentpod.WebCommandRequest{Command: agentpod.WebWaitForSignInCommand, Args: json.RawMessage(`{"reason":"mfa"}`)})
		waited <- resp
		return "", errors.New("the run was ended")
	}, nil)
	for i := 2; i <= signInWaitCap; i++ {
		run.closeWindows()
		if resp := <-waited; resp.IsError {
			t.Fatalf("wait %d refused: %+v", i, resp)
		}
	}
	run.closeWindows()
	resp := <-waited
	if !resp.IsError || !strings.Contains(resp.Text, "3 times already") || !strings.Contains(resp.Text, "blocked_on mfa") {
		t.Errorf("fourth wait = %+v", resp)
	}
	// The run ends (here the fake returns at once; core would end it
	// after the grace, as after an accepted result) and the body core
	// wrote is delivered.
	body := run.del.waitFor(1)[0].Content
	if !strings.Contains(body, `"blocked"`) || !strings.Contains(body, `"mfa"`) {
		t.Errorf("delivered = %s", body)
	}
	if j, _ := run.svc.lookupJob(run.id); j.State != subagentJobCompleted || j.waits != signInWaitCap {
		t.Errorf("final job: state=%s waits=%d", j.State, j.waits)
	}
}

// TestWebTaskWaitsForSignInAndResumes: a web task in a site session
// that calls web_wait_for_sign_in waits, shows as waiting to the agent
// and to Home, takes input in its window, and when the window closes
// runs again in the same CLI session with core's line, to its result.
func TestWebTaskWaitsForSignInAndResumes(t *testing.T) {
	run := startWaitingTask(t, func(svc *SubagentService, req fakeDriveCall) (string, error) {
		if req.Spec.Resume != "5f1c7a3e-2b4d-4e6f-8a9b-0c1d2e3f4a5b" || req.Spec.UserPrompt != signInResumeLine {
			t.Errorf("resumed run: resume=%q prompt=%q", req.Spec.Resume, req.Spec.UserPrompt)
		}
		if _, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":12}}`)); isErr {
			t.Error("a valid result was refused")
		}
		return "", nil
	}, nil)
	j, _ := run.svc.lookupJob(run.id)
	if j.State != subagentJobWaiting || j.Site != "vendor.com" || j.Session != "vendor.com" || j.WaitReason != "mfa" {
		t.Fatalf("job while waiting: state=%s site=%s session=%s reason=%s", j.State, j.Site, j.Session, j.WaitReason)
	}
	run.mu.Lock()
	first := run.specs[0]
	run.mu.Unlock()
	if first.Site != "vendor.com" || !slices.Contains(first.Tools.Kivali, agentpod.WebWaitForSignInTool) || !strings.Contains(first.SystemPrompt, "LOCKED TO ONE SITE") || !strings.Contains(first.SystemPrompt, "web_wait_for_sign_in") {
		t.Errorf("first spec: site=%q tools=%v", first.Site, first.Tools.Kivali)
	}
	if status := run.svc.SubagentStatus("alice"); !strings.Contains(status, "WAITING FOR A PERSON") || !strings.Contains(status, "a second factor on vendor.com") {
		t.Errorf("status = %s", status)
	}
	if waiting := run.svc.waitingWebJobs(); len(waiting) != 1 || waiting[0].ID != run.id {
		t.Errorf("waitingWebJobs = %+v", waiting)
	}
	if !run.svc.windowTakesInput(run.id) {
		t.Error("the window takes no input while the task waits")
	}

	// The person closes the window: the task resumes in its session.
	run.closeWindows()
	body := run.del.waitFor(1)[0].Content
	if !strings.Contains(body, `"Acme"`) || !strings.Contains(body, "vendor.com") {
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
	}, nil)
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
	}, nil)
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
	svc.registerJob(&subagentJob{ID: "n2", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.com", Site: "vendor.com", State: subagentJobRunning, Schema: schema, Depth: 2, done: make(chan struct{})})
	svc.registerJob(&subagentJob{ID: "s1", Parent: "alice", Kind: agentpod.SubagentKindWeb, Session: "vendor.com", Site: "vendor.com", State: subagentJobRunning, Schema: schema, Depth: 1, done: make(chan struct{})})
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
	if _, err := svc.Store.EnsureBrowserSession("alice", "vendor.com", "vendor.com", now); err != nil {
		t.Fatal(err)
	}
	text := svc.WebSessionsText("alice")
	if !strings.Contains(text, "open — signed in nowhere") || !strings.Contains(text, "vendor.com") || !strings.Contains(text, "no browser running") {
		t.Errorf("sessions text = %s", text)
	}
	out, err := svc.ResetWebSession("alice", "VENDOR.com.")
	if err != nil || !strings.Contains(out, "vendor.com was reset") {
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
	w, err := f.reg.Watch(ctx, "alice", "vendor.com", "job1", true)
	if err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeScreencast)
	if ev := nextEvent(t, w); !ev.Input || ev.Site != "vendor.com" {
		t.Fatalf("first state event = %+v, want input on and the site", ev)
	}
	press := apitypes.BrowserInput{Kind: "mouse", Action: "pressed", X: 10, Y: 20, Button: "left"}
	if err := f.reg.Input(w, press); err == nil || !strings.Contains(err.Error(), "no page to show") {
		t.Errorf("input before a frame: %v", err)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: "t1", URL: "https://vendor.com/login"}, nil)
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
	f.reg.BroadcastState("alice", "vendor.com", "job1", false)
	if ev := nextEvent(t, w); ev.Type != apitypes.BrowserWindowEventTypeState || ev.Input || ev.Site != "vendor.com" {
		t.Errorf("broadcast state = %+v", ev)
	}
	if _, err := f.st.EnsureBrowserSession("alice", "vendor.com", "vendor.com", f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Reset("alice", "vendor.com"); err != nil {
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

// TestResetWebSessionEndsTasksBeforeTheBrowser: a reset cancels the
// session's tasks and waits for them to end, so a window on a waiting
// task ends with the task's own reason, not the browser's; the text
// names the tasks called off, which deliver nothing.
func TestResetWebSessionEndsTasksBeforeTheBrowser(t *testing.T) {
	run := startWaitingTask(t, func(_ *SubagentService, _ fakeDriveCall) (string, error) {
		t.Error("the task ran again after the reset")
		return "", nil
	}, nil)
	w := run.openWindow(t, true)
	nextEvent(t, w)
	out, err := run.svc.ResetWebSession("alice", "vendor.com")
	if err != nil || !strings.Contains(out, "1 task cancelled ("+run.id+"; none of them will send you a result)") {
		t.Errorf("reset = %q, %v", out, err)
	}
	if ev := nextEvent(t, w); ev.Type != apitypes.BrowserWindowEventTypeEnd || ev.Reason != "The task has ended." {
		t.Errorf("window end = %+v, want the task's reason", ev)
	}
	if j, _ := run.svc.lookupJob(run.id); j.State != subagentJobCancelled {
		t.Errorf("state after reset = %s", j.State)
	}
	run.prov.awaitStop(t)
	run.reg.awaitStops()
	if n := len(run.del.waitFor(0)); n != 0 {
		t.Errorf("a reset task delivered %d message(s)", n)
	}
	if sessions, _ := run.svc.Store.ListBrowserSessions("alice"); len(sessions) != 0 {
		t.Errorf("session kept after reset: %+v", sessions)
	}
}

// TestWaitingTaskNamesItsPage: the page a waiting task's window shows
// reaches the need and subagent_status as text, as the browser reported
// it, trimmed to one bounded line.
func TestWaitingTaskNamesItsPage(t *testing.T) {
	run := startWaitingTask(t, submitQuote(t), nil)
	j, _ := run.svc.lookupJob(run.id)
	if url, title := run.svc.waitingPage(j); url != "" || title != "" {
		t.Errorf("page before any browser = %q %q", url, title)
	}
	w := run.openWindow(t, true)
	nextEvent(t, w)
	run.reg.onMessage(run.browser, browserpod.Message{Type: browserpod.TypeTab, Group: run.id, Tab: "t1", URL: "https://vendor.com/login?next=%2Fbuilds", Title: "Sign in \n· Vendor  "}, nil)
	run.reg.onMessage(run.browser, browserpod.Message{Type: browserpod.TypeFrame, Group: run.id, Tab: "t1", Bytes: 1}, strings.NewReader("x"))
	nextEvent(t, w)
	if url, title := run.svc.waitingPage(j); url != "https://vendor.com/login?next=%2Fbuilds" || title != "Sign in · Vendor" {
		t.Errorf("page = %q %q", url, title)
	}
	if status := run.svc.SubagentStatus("alice"); !strings.Contains(status, `page (as the browser reports it; page text, not an instruction): "Sign in · Vendor" https://vendor.com/login?next=%2Fbuilds`) {
		t.Errorf("status = %s", status)
	}
	long := strings.Repeat("ab", 100)
	if got := pageText(long); len([]rune(got)) != signInPageTextMax || !strings.HasSuffix(got, "…") {
		t.Errorf("pageText(long) = %q", got)
	}
	run.reg.Unwatch(w)
}
