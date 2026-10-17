package web

import (
	"context"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/browserpod"
)

// siteLockFrames are the site-lock messages the pod received, in order.
func siteLockFrames(link *fakeLink) []browserpod.Message {
	link.mu.Lock()
	defer link.mu.Unlock()
	var out []browserpod.Message
	for _, fr := range link.frames {
		if fr.msg.Type == browserpod.TypeSiteLock {
			out = append(out, fr.msg)
		}
	}
	return out
}

// expectSiteLock reads the next site-lock message and checks it.
func expectSiteLock(t *testing.T, link *fakeLink, group string, unlocked bool) {
	t.Helper()
	m := link.next(t, browserpod.TypeSiteLock).msg
	if m.Group != group || m.Unlocked != unlocked {
		t.Fatalf("site lock = %+v, want group %q unlocked %v", m, group, unlocked)
	}
}

// expectSiteLocks checks how many site-lock messages the pod has had,
// once every sync in flight is done.
func expectSiteLocks(t *testing.T, f *browserFixture, link *fakeLink, n int) {
	t.Helper()
	f.reg.awaitSiteLocks()
	if got := siteLockFrames(link); len(got) != n {
		t.Fatalf("site-lock messages = %+v, want %d", got, n)
	}
}

// TestBrowserRegistryUnlocksTheSiteForAPerson: a group is unlocked
// while its task waits for a person and a window is on it, and
// relocked when the task resumes or the last window has been gone for
// the grace; a state the pod already has is not sent again, and a
// released group is not told.
func TestBrowserRegistryUnlocksTheSiteForAPerson(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	_, link := f.running(ctx, "job1")

	// A window on a running task: locked, nothing sent.
	w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	expectSiteLocks(t, f, link, 0)

	// The task waits for a person: unlocked, once.
	f.reg.BroadcastState("alice", "vendor.example", "job1", true)
	expectSiteLock(t, link, "job1", true)
	w2, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", true)
	if err != nil {
		t.Fatal(err)
	}
	f.reg.BroadcastState("alice", "vendor.example", "job1", true)
	expectSiteLocks(t, f, link, 1)

	// The task resumes: relocked.
	f.reg.BroadcastState("alice", "vendor.example", "job1", false)
	expectSiteLock(t, link, "job1", false)

	// It waits again; both windows leave. Within the grace a reload
	// keeps the group unlocked; after it the group is relocked.
	f.reg.BroadcastState("alice", "vendor.example", "job1", true)
	expectSiteLock(t, link, "job1", true)
	f.reg.Unwatch(w)
	f.reg.Unwatch(w2)
	f.clk.Advance(browserWindowGrace - time.Second)
	f.reg.sweep(f.clk.Now())
	expectSiteLocks(t, f, link, 3)
	f.clk.Advance(time.Second)
	f.reg.sweep(f.clk.Now())
	expectSiteLock(t, link, "job1", false)

	// A window back on the waiting task unlocks it; the task ends and
	// its group closes with no relock: the pod forgets the lock with
	// the group.
	if _, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", true); err != nil {
		t.Fatal(err)
	}
	expectSiteLock(t, link, "job1", true)
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	link.next(t, browserpod.TypeCloseGroup)
	expectSiteLocks(t, f, link, 5)
}

// TestBrowserRegistryResendsTheSiteUnlock: a pod starts with every
// group locked, so a pod that attaches is told each group core holds
// unlocked, after start.
func TestBrowserRegistryResendsTheSiteUnlock(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	if _, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", true); err != nil {
		t.Fatal(err)
	}
	expectSiteLock(t, link, "job1", true)
	if err := f.reg.attach(b); err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeStart)
	expectSiteLock(t, link, "job1", true)
	expectSiteLocks(t, f, link, 2)
}

// TestBrowserRegistryNeverUnlocksTheOpenSession: the open session has
// no site to lock.
func TestBrowserRegistryNeverUnlocksTheOpenSession(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	link := newFakeLink()
	b := f.connectWith("open", link)
	link.next(t, browserpod.TypeStart)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true}, nil)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Watch(ctx, "alice", "open", "job1", true); err != nil {
		t.Fatal(err)
	}
	f.reg.BroadcastState("alice", "open", "job1", true)
	expectSiteLocks(t, f, link, 0)
}
