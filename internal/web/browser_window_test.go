package web

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// nextEvent is the watcher's next text message, or a test failure
// after the deadline (a deadlock diagnostic, not a wait).
func nextEvent(t *testing.T, w *browserWatcher) apitypes.BrowserWindowEvent {
	t.Helper()
	select {
	case ev := <-w.Events():
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event arrived")
		return apitypes.BrowserWindowEvent{}
	}
}

// nextFrame is the watcher's next frame.
func nextFrame(t *testing.T, w *browserWatcher) []byte {
	t.Helper()
	select {
	case f := <-w.Frames():
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no frame arrived")
		return nil
	}
}

// screencasts is how many screencast switches the pod received, and
// the last one.
func screencasts(link *fakeLink) (int, browserpod.Message) {
	link.mu.Lock()
	defer link.mu.Unlock()
	var n int
	var last browserpod.Message
	for _, fr := range link.frames {
		if fr.msg.Type == browserpod.TypeScreencast {
			n++
			last = fr.msg
		}
	}
	return n, last
}

// TestBrowserRegistryWindowFollowsAGroup: the first window on a group
// turns the group's screencast on; the window hears the browser's
// state, the shown tab and its changes, and gets frames with the
// newest winning when it falls behind; a window that never reads
// holds nobody up; the screencast outlives the last window by the
// grace, then goes off, and a window back within the grace reuses it.
func TestBrowserRegistryWindowFollowsAGroup(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")

	w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	if on := link.next(t, browserpod.TypeScreencast); on.msg.Group != "job1" || on.msg.Tab != "" || !on.msg.OK {
		t.Fatalf("screencast switch = %+v, want the group's, on", on.msg)
	}
	if ev := nextEvent(t, w); ev.Type != apitypes.BrowserWindowEventTypeState || ev.Browser != apitypes.BrowserStateRunning {
		t.Fatalf("first event = %+v, want state running", ev)
	}

	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: "t1", URL: "about:blank"}, nil)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeNavigated, Group: "job1", Tab: "t1", URL: "https://vendor.example/pricing", Title: "Pricing"}, nil)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t1", Bytes: 6}, strings.NewReader("frame1"))
	if ev := nextEvent(t, w); ev.Type != apitypes.BrowserWindowEventTypeTab || ev.Tab == nil || ev.Tab.ID != "t1" || ev.Tab.URL != "https://vendor.example/pricing" || ev.Tab.Title != "Pricing" {
		t.Fatalf("tab event = %+v", ev)
	}
	if fr := nextFrame(t, w); string(fr) != "frame1" {
		t.Fatalf("frame = %q", fr)
	}

	// Two frames before the window reads: the newest wins.
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t1", Bytes: 6}, strings.NewReader("frame2"))
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t1", Bytes: 6}, strings.NewReader("frame3"))
	if fr := nextFrame(t, w); string(fr) != "frame3" {
		t.Fatalf("frame after a backlog = %q, want the newest", fr)
	}
	// The shown tab's navigation reaches the window; another tab's
	// does not until a frame comes from it.
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeNavigated, Group: "job1", Tab: "t1", URL: "https://vendor.example/cart", Title: "Cart"}, nil)
	if ev := nextEvent(t, w); ev.Tab == nil || ev.Tab.URL != "https://vendor.example/cart" {
		t.Fatalf("navigation event = %+v", ev)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: "t2", URL: "https://vendor.example/help", Title: "Help"}, nil)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t2", Bytes: 6}, strings.NewReader("frame4"))
	if ev := nextEvent(t, w); ev.Tab == nil || ev.Tab.ID != "t2" || ev.Tab.Title != "Help" {
		t.Fatalf("event after a frame from another tab = %+v, want that tab", ev)
	}
	nextFrame(t, w)

	// A second window that never reads: frames keep flowing to the
	// first, and the pod is asked for nothing more.
	slow, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t2", Bytes: 1}, strings.NewReader("x"))
	}
	if fr := nextFrame(t, w); string(fr) != "x" {
		t.Fatalf("frame beside a slow window = %q", fr)
	}
	if n, _ := screencasts(link); n != 1 {
		t.Errorf("screencast switches with two windows = %d, want 1", n)
	}

	// Both windows leave: the screencast stays on through the grace.
	f.reg.Unwatch(w)
	f.reg.Unwatch(slow)
	f.clk.Advance(browserWindowGrace - time.Second)
	f.reg.sweep(f.clk.Now())
	if n, _ := screencasts(link); n != 1 {
		t.Errorf("screencast switched off before the grace: %d switches", n)
	}
	// A window back within the grace reuses the running screencast.
	again, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, again); ev.Type != apitypes.BrowserWindowEventTypeState {
		t.Fatalf("returning window's first event = %+v", ev)
	}
	if ev := nextEvent(t, again); ev.Tab == nil || ev.Tab.ID != "t2" {
		t.Fatalf("returning window was not told the shown tab: %+v", ev)
	}
	f.clk.Advance(2 * time.Second)
	f.reg.sweep(f.clk.Now())
	if n, _ := screencasts(link); n != 1 {
		t.Errorf("a returning window did not keep the screencast: %d switches", n)
	}
	// It leaves for good: after the grace the screencast goes off and
	// the task's browser is still running, for the task.
	f.reg.Unwatch(again)
	f.clk.Advance(browserWindowGrace)
	f.reg.sweep(f.clk.Now())
	off := link.next(t, browserpod.TypeScreencast)
	if off.msg.Group != "job1" || off.msg.OK {
		t.Fatalf("screencast switch after the grace = %+v, want off", off.msg)
	}
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserRunning) {
		t.Errorf("state after the window = %v, want running for the task", states)
	}
	// The task ends: no window holds the browser, so it goes idle.
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserIdle) {
		t.Errorf("state after release = %v, want idle", states)
	}
}

// TestBrowserRegistryWindowEndsWithTheTaskOrTheBrowser: a window is
// told, and done, when its task ends or its browser stops; a task that
// ends under a window lets the browser go idle.
func TestBrowserRegistryWindowEndsWithTheTaskOrTheBrowser(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeScreencast)
	nextEvent(t, w) // running
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	if ev := nextEvent(t, w); ev.Type != apitypes.BrowserWindowEventTypeEnd || ev.Reason != "The task has ended." {
		t.Fatalf("event after release = %+v", ev)
	}
	select {
	case <-w.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("done not closed after release")
	}
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserIdle) {
		t.Errorf("state after release under a window = %v, want idle", states)
	}
	if _, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Errorf("watch after release: %v", err)
	}

	// A second task's window, then the browser is lost.
	w2, err := f.reg.Watch(ctx, "alice", "vendor.example", "job2", false)
	if err != nil {
		t.Fatal(err)
	}
	nextEvent(t, w2)
	f.reg.onLost(b)
	if ev := nextEvent(t, w2); ev.Type != apitypes.BrowserWindowEventTypeEnd || ev.Reason != "The browser stopped." {
		t.Fatalf("event after the browser was lost = %+v", ev)
	}
	f.stopped(t)
}

// TestBrowserRegistryReportsBrowserState: OnState hears starting,
// running and stopped, once each, for a session with a profile and
// for the open session.
func TestBrowserRegistryReportsBrowserState(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	var mu sync.Mutex
	var seen []string
	f.reg.OnState = func(slug, session string, state apitypes.BrowserState) {
		mu.Lock()
		seen = append(seen, slug+"/"+session+":"+string(state))
		mu.Unlock()
	}
	_, link := f.running(ctx, "job1")
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	link.next(t, browserpod.TypeCloseGroup)
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	link.next(t, browserpod.TypeSave)
	mu.Lock()
	got := strings.Join(seen, " ")
	mu.Unlock()
	if got != "alice/vendor.example:starting alice/vendor.example:running alice/vendor.example:stopped" {
		t.Errorf("states = %s", got)
	}
	if st := f.reg.StateFor("alice", "vendor.example"); st != apitypes.BrowserStateStopped {
		t.Errorf("StateFor while saving = %s, want stopped", st)
	}
	if st := f.reg.StateFor("alice", agentpod.OpenWebSession); st != apitypes.BrowserStateStopped {
		t.Errorf("StateFor with no browser = %s, want stopped", st)
	}
}

// TestBrowserRegistryRefusesAnOversizedFrame: a frame over the limit
// is not read and reaches no window.
func TestBrowserRegistryRefusesAnOversizedFrame(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeScreencast)
	nextEvent(t, w)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t1", Bytes: browserMaxFrameBytes + 1}, strings.NewReader("x"))
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t1", Bytes: 2}, strings.NewReader("ok"))
	nextEvent(t, w) // the tab event for t1
	if fr := nextFrame(t, w); string(fr) != "ok" {
		t.Errorf("frame = %q, want the one within the limit", fr)
	}
}
