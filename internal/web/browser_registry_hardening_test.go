package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/browserpod"
)

// frameTypes is the order of the message types a fake pod received.
func frameTypes(l *fakeLink) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var types []string
	for _, f := range l.frames {
		types = append(types, f.msg.Type)
	}
	return types
}

// TestBrowserRegistryKeepsCookieSnapshots: a cookie snapshot from the
// pod is kept beside the profile, within bounds; a window closing asks
// the pod for a fresh one; the next browser gets the snapshot after
// its profile and before start.
func TestBrowserRegistryKeepsCookieSnapshots(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")

	jar := []byte(`[{"name":"sid","value":"1","domain":"vendor.example"}]`)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeCookies, Bytes: int64(len(jar))}, bytes.NewReader(jar))
	if r, n, err := f.st.OpenBrowserCookies("alice", "vendor.example"); err != nil || n != int64(len(jar)) {
		t.Fatalf("stored cookies = %d, %v", n, err)
	} else if got, _ := io.ReadAll(r); !bytes.Equal(got, jar) {
		t.Errorf("stored cookies = %q", got)
	} else {
		_ = r.Close()
	}
	// Over the bound nothing is kept.
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeCookies, Bytes: browserpod.MaxCookiesBytes + 1}, strings.NewReader("[]"))
	if r, n, _ := f.st.OpenBrowserCookies("alice", "vendor.example"); n != int64(len(jar)) {
		t.Errorf("an oversized snapshot replaced the stored one: %d bytes", n)
	} else {
		_ = r.Close()
	}

	// The window's grace runs out: the cast ends and a snapshot is asked for.
	w, err := f.reg.Watch(ctx, "alice", "vendor.example", "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	link.next(t, browserpod.TypeScreencast)
	f.reg.Unwatch(w)
	f.clk.Advance(browserWindowGrace)
	f.reg.sweep(f.clk.Now())
	if off := link.next(t, browserpod.TypeScreencast); off.msg.OK {
		t.Errorf("screencast after the window closed = %+v, want off", off.msg)
	}
	link.next(t, browserpod.TypeSnapshot)

	// The browser goes without a save: the snapshot is what the next
	// one starts from.
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	link.next(t, browserpod.TypeCloseGroup)
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	link.next(t, browserpod.TypeSave)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeSaved}, nil)
	f.stopped(t)

	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job2", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	b2, link2 := f.connect(ctx)
	cookies := link2.next(t, browserpod.TypeCookies)
	if cookies.msg.Bytes != int64(len(jar)) || !bytes.Equal(cookies.body, jar) {
		t.Errorf("second start's cookies = %+v %q, want the snapshot", cookies.msg, cookies.body)
	}
	link2.next(t, browserpod.TypeStart)
	if got := frameTypes(link2); strings.Join(got, " ") != "hello cookies start" {
		t.Errorf("second start's frames = %v, want hello, cookies, start", got)
	}
	f.reg.onMessage(b2, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link2.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b2, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true}, nil)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

// TestBrowserRegistryLifetime: a browser past its lifetime takes no
// new task while the one on it carries on, and is saved as soon as
// that task ends rather than after the idle wait.
func TestBrowserRegistryLifetime(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	f.clk.Advance(browserLifetime)

	if _, err := f.reg.Command(ctx, "alice", "vendor.example", "job2", browserpod.Message{Name: browserpod.CommandSnapshot}); !errors.Is(err, errBrowserLifetime) {
		t.Errorf("new task on an expired browser: %v, want the lifetime refusal", err)
	}
	if _, err := f.reg.Watch(ctx, "alice", "vendor.example", "job3", false); !errors.Is(err, errBrowserLifetime) {
		t.Errorf("window for a new task on an expired browser: %v, want the lifetime refusal", err)
	}
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	cmd := link.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true}, nil)
	if err := <-errs; err != nil {
		t.Fatalf("the task already on the browser: %v", err)
	}

	// The clock does not move: the save begins at the very sweep
	// after the release, not browserIdleAfter later.
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	link.next(t, browserpod.TypeCloseGroup)
	f.reg.sweep(f.clk.Now())
	link.next(t, browserpod.TypeSave)
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserSaving) {
		t.Fatalf("state after the last task ended = %v, want saving at once", states)
	}
}
