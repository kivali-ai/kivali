package web

import (
	"context"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
)

// TestBrowserRegistryOpenSessionKeepsNothing: the open session is
// ephemeral. Its pod is told so and gets no profile, whatever was
// stored for it before is deleted, and at its end core deletes the
// pod without asking for a profile; no session record is left.
func TestBrowserRegistryOpenSessionKeepsNothing(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	open := agentpod.OpenWebSession
	if _, err := f.st.EnsureBrowserSession("alice", open, "", f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.WriteBrowserProfile("alice", open, strings.NewReader("stale profile"), f.clk.Now()); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", open, "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	token := <-f.prov.startedC
	link := newFakeLink()
	b := f.reg.claim("alice", open, token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := f.reg.attach(ctx, b); err != nil {
		t.Fatal(err)
	}
	hello := link.next(t, browserpod.TypeHello)
	if !hello.msg.Ephemeral || hello.msg.Bytes != 0 || len(hello.body) != 0 {
		t.Errorf("hello = %+v with %d bytes, want ephemeral and no profile", hello.msg, len(hello.body))
	}
	if r, n, err := f.st.OpenBrowserProfile("alice", open); err != nil || n != 0 || r != nil {
		t.Errorf("the stale open profile was kept: %d bytes, %v", n, err)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "ok"}, nil)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}

	f.reg.ReleaseGroup("alice", open, "job1")
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	if stopped := f.stopped(t); stopped != "alice/"+open {
		t.Errorf("stop = %q", stopped)
	}
	link.mu.Lock()
	for _, fr := range link.frames {
		if fr.msg.Type == browserpod.TypeSave {
			t.Error("an open browser was asked for its profile")
		}
	}
	link.mu.Unlock()
	if sessions, err := f.st.ListBrowserSessions("alice"); err != nil || len(sessions) != 0 {
		t.Errorf("open left a session record: %+v, %v", sessions, err)
	}
}
