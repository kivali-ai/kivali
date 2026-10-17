package web

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/browserpod"
)

// stuckLink is a fakeLink whose pod stops reading at the first message
// of type stuckOn: that send, and every send queued behind it, blocks
// until release is closed, as a write to a hung pod does until its
// limit. stuck is closed once a send has blocked.
type stuckLink struct {
	*fakeLink
	stuckOn string
	release chan struct{}
	stuck   chan struct{}
	once    sync.Once
}

func newStuckLink(stuckOn string) *stuckLink {
	return &stuckLink{fakeLink: newFakeLink(), stuckOn: stuckOn, release: make(chan struct{}), stuck: make(chan struct{})}
}

func (l *stuckLink) send(_ time.Duration, msg browserpod.Message, body io.Reader) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if msg.Type == l.stuckOn {
		l.once.Do(func() { close(l.stuck) })
		<-l.release
		return errors.New("the pod stopped reading")
	}
	return l.recordLocked(msg, body)
}

// awaitStuck waits until a send has blocked on the pod.
func (l *stuckLink) awaitStuck(t *testing.T) {
	t.Helper()
	select {
	case <-l.stuck:
	case <-time.After(10 * time.Second):
		t.Fatal("no send blocked on the pod")
	}
}

// runningWith is browserFixture.running over the given link.
func (f *browserFixture) runningWith(ctx context.Context, group string, link *stuckLink) *browser {
	f.t.Helper()
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", group, browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	b := f.connectWith("vendor.example", link)
	link.next(f.t, browserpod.TypeStart)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(f.t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "ok"}, nil)
	if err := <-errs; err != nil {
		f.t.Fatalf("first command: %v", err)
	}
	return b
}

// TestBrowserRegistryFinishDoesNotWaitOnAStuckPod: a pod that has
// stopped reading holds an upload's writer; losing the connection ends
// the browser at once all the same, failing the commands in flight,
// and the pod is deleted while the writer is still held.
func TestBrowserRegistryFinishDoesNotWaitOnAStuckPod(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	link := newStuckLink(browserpod.TypeFile)
	b := f.runningWith(ctx, "job1", link)

	sendErr := make(chan error, 1)
	go func() {
		sendErr <- f.reg.SendFile(ctx, "alice", "vendor.example", "job1", "x.pdf", nil, 0)
	}()
	link.awaitStuck(t)
	cmdErr := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		cmdErr <- err
	}()

	// The connection is lost while the upload's writer is held: the
	// browser is finished and its pod deleted without waiting on it.
	f.reg.onLost(b)
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserStopped) {
		t.Errorf("state after the loss = %v, want stopped", states)
	}
	if stopped := f.stopped(t); stopped != "alice/vendor.example" {
		t.Errorf("stop = %q", stopped)
	}
	if !link.closed.Load() {
		t.Error("the link was not closed while its writer was held")
	}
	// The held writers end when the pod lets go, as failures.
	close(link.release)
	if err := <-sendErr; err == nil {
		t.Error("the stuck upload reported no error")
	}
	if err := <-cmdErr; err == nil {
		t.Error("the command queued behind the stuck upload reported no error")
	}
}

// TestBrowserRegistrySweepActsOffItsGoroutine: the sweep returns while
// a pod is stuck. An idle browser whose pod has stopped reading begins
// its save and, when that does not finish in time, is finished, each
// at one sweep that comes straight back; a second browser's start
// timeout is handled at the same sweep.
func TestBrowserRegistrySweepActsOffItsGoroutine(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	link := newStuckLink(browserpod.TypeSave)
	f.runningWith(ctx, "job1", link)
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	link.next(t, browserpod.TypeCloseGroup)

	// A second browser that never connects: its start timeout and the
	// first's idle limit are both due at one sweep.
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.com", "job2", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	<-f.prov.startedC

	f.clk.Advance(browserIdleAfter)
	returned := make(chan time.Time, 1)
	go func() { returned <- f.reg.sweep(f.clk.Now()) }()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not return")
	}
	link.awaitStuck(t)
	if err := <-errs; err == nil || err.Error() != "the browser did not start in time" {
		t.Errorf("the second browser's start: %v", err)
	}
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserSaving) {
		t.Errorf("state with the save asked for = %v, want saving", states)
	}

	// The save never finishes: at its timeout the browser is finished
	// and its pod deleted, with the writer still held.
	f.clk.Advance(browserSaveTimeout)
	go func() { returned <- f.reg.sweep(f.clk.Now()) }()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the second sweep did not return")
	}
	stops := map[string]bool{f.prov.awaitStop(t): true, f.prov.awaitStop(t): true}
	if !stops["alice/vendor.example"] || !stops["alice/vendor.com"] {
		t.Errorf("stops = %v, want both browsers", stops)
	}
	f.reg.awaitStops()
	close(link.release)
	if states := f.reg.states(); len(states) != 0 {
		t.Errorf("browsers left after the sweeps: %v", states)
	}
}
