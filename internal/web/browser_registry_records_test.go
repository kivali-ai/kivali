package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/files"
)

// windowsClosedRecorder counts OnWindowsClosed calls per group.
type windowsClosedRecorder struct {
	mu    sync.Mutex
	calls map[string]int
}

func (w *windowsClosedRecorder) record(_, _, group string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.calls == nil {
		w.calls = map[string]int{}
	}
	w.calls[group]++
}

func (w *windowsClosedRecorder) count(group string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[group]
}

// sweepNow runs one sweep and waits for what it dispatched to settle:
// the actions run on their own goroutines, so a test that reads the
// registry after a sweep lets them finish first.
func (f *browserFixture) sweepNow() {
	f.reg.sweep(f.clk.Now())
	f.reg.awaitStops()
}

// TestBrowserRegistryFinishForgetsTheWindows: a browser that stops
// under a window ends the window and forgets it, so neither the window
// leaving afterwards nor a grace already running fires OnWindowsClosed
// for a browser that is gone; a running browser's window closing still
// does, once.
func TestBrowserRegistryFinishForgetsTheWindows(t *testing.T) {
	ctx := context.Background()
	t.Run("window attached when the browser stops", func(t *testing.T) {
		f := newBrowserFixture(t)
		rec := &windowsClosedRecorder{}
		f.reg.OnWindowsClosed = rec.record
		b, link := f.running(ctx, "job1")
		w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", true)
		if err != nil {
			t.Fatal(err)
		}
		link.next(t, browserpod.TypeScreencast)
		nextEvent(t, w)
		gate := make(chan struct{})
		f.prov.mu.Lock()
		f.prov.stopGate = gate
		f.prov.mu.Unlock()
		f.reg.onLost(b)
		if ev := nextEvent(t, w); ev.Reason != "The browser stopped." {
			t.Fatalf("event = %+v", ev)
		}
		f.reg.mu.Lock()
		g := b.groups["job1"]
		left, watching, windows := len(g.watchers), g.watching, b.windows
		f.reg.mu.Unlock()
		if left != 0 || watching || windows != 0 {
			t.Errorf("after finish: %d watcher(s), watching=%v, windows=%d; want none", left, watching, windows)
		}
		// The window's handler leaves, as it does, while the record is
		// still there; the grace passes; the record goes; the grace
		// passes again.
		f.reg.Unwatch(w)
		f.clk.Advance(browserWindowGrace)
		f.reg.sweep(f.clk.Now())
		close(gate)
		f.stopped(t)
		f.clk.Advance(browserWindowGrace)
		f.sweepNow()
		if n := rec.count("job1"); n != 0 {
			t.Errorf("OnWindowsClosed fired %d time(s) for a dead browser", n)
		}
	})
	t.Run("grace running when the browser stops", func(t *testing.T) {
		f := newBrowserFixture(t)
		rec := &windowsClosedRecorder{}
		f.reg.OnWindowsClosed = rec.record
		b, link := f.running(ctx, "job1")
		w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", true)
		if err != nil {
			t.Fatal(err)
		}
		link.next(t, browserpod.TypeScreencast)
		f.reg.Unwatch(w)
		f.clk.Advance(browserWindowGrace / 2)
		f.reg.onLost(b)
		f.clk.Advance(browserWindowGrace)
		f.sweepNow()
		f.stopped(t)
		if n := rec.count("job1"); n != 0 {
			t.Errorf("OnWindowsClosed fired %d time(s) after the browser stopped mid-grace", n)
		}
	})
	t.Run("running browser", func(t *testing.T) {
		f := newBrowserFixture(t)
		rec := &windowsClosedRecorder{}
		f.reg.OnWindowsClosed = rec.record
		_, link := f.running(ctx, "job1")
		w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", true)
		if err != nil {
			t.Fatal(err)
		}
		link.next(t, browserpod.TypeScreencast)
		f.reg.Unwatch(w)
		f.clk.Advance(browserWindowGrace)
		f.reg.sweep(f.clk.Now())
		link.next(t, browserpod.TypeSnapshot)
		if n := rec.count("job1"); n != 1 {
			t.Errorf("OnWindowsClosed fired %d time(s), want once", n)
		}
		f.clk.Advance(browserWindowGrace)
		f.sweepNow()
		if n := rec.count("job1"); n != 1 {
			t.Errorf("a later sweep fired OnWindowsClosed again: %d", n)
		}
	})
}

// TestBrowserRegistryRemembersAReleaseWithoutABrowser: a task's end is
// remembered for its session whether or not a browser runs, so a call
// of the task's that arrives after starts no browser and opens no
// group; a reset of the session forgets its releases; the set is
// bounded.
func TestBrowserRegistryRemembersAReleaseWithoutABrowser(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	if _, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot}); !errors.Is(err, errTaskEnded) {
		t.Errorf("command after a release with no browser: %v, want the task ended", err)
	}
	if _, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false); !errors.Is(err, errTaskEnded) {
		t.Errorf("watch after a release with no browser: %v", err)
	}
	if err := f.reg.SendFile(ctx, "alice", "vendor.example", "job1", "x", strings.NewReader(""), 0); !errors.Is(err, errTaskEnded) {
		t.Errorf("file after a release with no browser: %v", err)
	}
	if len(f.prov.started) != 0 {
		t.Fatal("a call for an ended task started a browser")
	}
	// Another task starts the browser; the ended one still cannot join.
	_, link := f.running(ctx, "job2")
	if _, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot}); !errors.Is(err, errTaskEnded) {
		t.Errorf("command on the next browser: %v", err)
	}
	// The browser goes and comes back: still remembered.
	f.reg.ReleaseGroup("alice", "vendor.example", "job2")
	link.next(t, browserpod.TypeCloseGroup)
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	link.next(t, browserpod.TypeSave)
	f.reg.mu.Lock()
	saving := f.reg.browsers["alice/vendor.example"]
	f.reg.mu.Unlock()
	f.reg.onMessage(saving, browserpod.Message{Type: browserpod.TypeSaved}, nil)
	f.stopped(t)
	if _, err := f.reg.Command(ctx, "alice", "vendor.example", "job2", browserpod.Message{Name: browserpod.CommandSnapshot}); !errors.Is(err, errTaskEnded) {
		t.Errorf("command for a task released on a browser since gone: %v", err)
	}

	// Bounded: the oldest releases go first.
	for i := 0; i < browserReleasedPerSession; i++ {
		f.reg.ReleaseGroup("alice", "vendor.example", fmt.Sprintf("g%d", i))
	}
	f.reg.mu.Lock()
	rel := f.reg.released["alice/vendor.example"]
	n, hasFirst, hasLast := len(rel.set), rel.set["job1"], rel.set[fmt.Sprintf("g%d", browserReleasedPerSession-1)]
	f.reg.mu.Unlock()
	if n != browserReleasedPerSession || hasFirst || !hasLast {
		t.Errorf("released set: %d entries, oldest kept=%v, newest kept=%v", n, hasFirst, hasLast)
	}
	// A reset forgets the session's releases.
	if err := f.reg.Reset("alice", "vendor.example"); err != nil {
		t.Fatal(err)
	}
	f.reg.mu.Lock()
	_, kept := f.reg.released["alice/vendor.example"]
	f.reg.mu.Unlock()
	if kept {
		t.Error("releases kept after the session was reset")
	}
}

// TestBrowserRegistryBoundsWhatThePodSaysAboutTabs: a tab is recorded
// only under an id the pod would mint, only up to the pod's tab limit,
// with its URL and title cut to size; a title change on the shown tab
// reaches the windows.
func TestBrowserRegistryBoundsWhatThePodSaysAboutTabs(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	for _, id := range []string{"", "T1", "t", "tab1", "t1/../t2", "t12345678901", "../t1"} {
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: id, URL: "https://vendor.example/"}, nil)
	}
	long := strings.Repeat("é", browserTabTextMax)
	for i := 1; i <= browserpod.MaxTabsPerBrowser+3; i++ {
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: fmt.Sprintf("t%d", i), URL: "https://vendor.example/" + long, Title: long}, nil)
	}
	f.reg.mu.Lock()
	g := b.groups["job1"]
	tabs, info := len(g.tabs), g.tabInfo["t1"]
	f.reg.mu.Unlock()
	if tabs != browserpod.MaxTabsPerBrowser {
		t.Errorf("%d tabs recorded, want the pod's limit of %d", tabs, browserpod.MaxTabsPerBrowser)
	}
	if len(info.title) > browserTabTextMax || len(info.url) > browserTabTextMax || !strings.HasPrefix(info.url, "https://vendor.example/") || !strings.HasSuffix(info.title, "é") {
		t.Errorf("tab info = %d-byte URL, %d-byte title; want both cut to %d at a character", len(info.url), len(info.title), browserTabTextMax)
	}

	// The shown tab's title changes without a navigation: the window
	// hears it; another tab's does not.
	w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeScreencast)
	nextEvent(t, w) // state
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "t1", Bytes: 1}, strings.NewReader("x"))
	nextEvent(t, w) // the shown tab
	nextFrame(t, w)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: "t2", URL: "https://vendor.example/other", Title: "Other"}, nil)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: "t1", URL: "https://vendor.example/app", Title: "Inbox (3)"}, nil)
	if ev := nextEvent(t, w); ev.Tab == nil || ev.Tab.ID != "t1" || ev.Tab.Title != "Inbox (3)" {
		t.Errorf("event after the shown tab's title changed = %+v", ev)
	}
	// A frame from a tab the pod could not have minted shows nothing.
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeFrame, Group: "job1", Tab: "../t1", Bytes: 1}, strings.NewReader("x"))
	f.reg.mu.Lock()
	shown := g.shownTab
	f.reg.mu.Unlock()
	if shown != "t1" {
		t.Errorf("shown tab after a bad frame = %q", shown)
	}
}

// TestBrowserRegistryRecordsASessionsUse: a task's first command on a
// session records the use on the session's stored record, and later
// commands of the same task do not touch it.
func TestBrowserRegistryRecordsASessionsUse(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	first, _, err := f.st.ReadBrowserSession("alice", "vendor.example")
	if err != nil {
		t.Fatal(err)
	}
	if !first.LastUsed.Equal(f.clk.Now()) {
		t.Errorf("last used after the first task = %v, want %v", first.LastUsed, f.clk.Now())
	}
	f.clk.Advance(time.Minute)
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	cmd := link.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true}, nil)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if again, _, _ := f.st.ReadBrowserSession("alice", "vendor.example"); !again.LastUsed.Equal(first.LastUsed) {
		t.Errorf("a later command of the same task moved last used to %v", again.LastUsed)
	}
	f.clk.Advance(time.Minute)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job2", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	cmd = link.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true}, nil)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if next, _, _ := f.st.ReadBrowserSession("alice", "vendor.example"); !next.LastUsed.Equal(f.clk.Now()) {
		t.Errorf("last used after another task = %v, want %v", next.LastUsed, f.clk.Now())
	}
}

// TestBrowserRegistryRefundsAFailedDownload: the bytes reserved for a
// download that could not be written are given back, so the group's
// limit counts only what landed.
func TestBrowserRegistryRefundsAFailedDownload(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, _ := f.running(ctx, "job1")
	// Nothing can be written under a file where the directory should be.
	dir := filepath.Join(f.root, files.UntrustedDownloadsDir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeDownload, Group: "job1", Tab: "t1", FileName: "a.pdf", Bytes: 5}, strings.NewReader("abcde"))
	f.reg.mu.Lock()
	reserved, downloads := b.groups["job1"].downloadBytes, len(b.groups["job1"].downloads)
	f.reg.mu.Unlock()
	if reserved != 0 || downloads != 0 {
		t.Errorf("after a failed download: %d bytes reserved, %d listed; want none", reserved, downloads)
	}
	// With the directory back, the same bytes fit.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeDownload, Group: "job1", Tab: "t1", FileName: "a.pdf", Bytes: 5}, strings.NewReader("abcde"))
	f.reg.mu.Lock()
	reserved, downloads = b.groups["job1"].downloadBytes, len(b.groups["job1"].downloads)
	f.reg.mu.Unlock()
	if reserved != 5 || downloads != 1 {
		t.Errorf("after a download that landed: %d bytes reserved, %d listed", reserved, downloads)
	}
}
