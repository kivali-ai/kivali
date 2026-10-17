package web

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
)

// shutdownIn runs Shutdown on its own goroutine and returns a channel
// closed when it has returned.
func shutdownIn(f *browserFixture, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.reg.Shutdown(ctx)
	}()
	return done
}

// awaitReturn fails the test if Shutdown has not returned; the deadline
// is a deadlock diagnostic.
func awaitReturn(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}
}

// TestBrowserRegistryShutdownSavesIdleBrowsers: at shutdown an idle
// site browser begins its save, and Shutdown returns once the profile
// is stored and the browser finished.
func TestBrowserRegistryShutdownSavesIdleBrowsers(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, link := f.running(ctx, "job1")
	f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	link.next(t, browserpod.TypeCloseGroup)

	done := shutdownIn(f, ctx)
	link.next(t, browserpod.TypeSave)
	select {
	case <-done:
		t.Fatal("Shutdown returned before the save finished")
	default:
	}
	profile := bytes.Repeat([]byte("p"), 300)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeProfile, Bytes: int64(len(profile))}, bytes.NewReader(profile))
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeSaved}, nil)
	awaitReturn(t, done)
	if r, n, err := f.st.OpenBrowserProfile("alice", "vendor.example"); err != nil || n != int64(len(profile)) {
		t.Errorf("stored profile = %d bytes, %v", n, err)
	} else {
		_ = r.Close()
	}
	f.stopped(t)
}

// TestBrowserRegistryShutdownAsksRunningBrowsersForCookies: a running
// site browser is asked for a cookie snapshot and Shutdown returns when
// it lands; one with nothing new to report is given the grace, on the
// clock, and no more.
func TestBrowserRegistryShutdownAsksRunningBrowsersForCookies(t *testing.T) {
	ctx := context.Background()
	t.Run("cookies land", func(t *testing.T) {
		f := newBrowserFixture(t)
		b, link := f.running(ctx, "job1")
		done := shutdownIn(f, ctx)
		link.next(t, browserpod.TypeSnapshot)
		select {
		case <-done:
			t.Fatal("Shutdown returned before the cookies landed")
		default:
		}
		jar := []byte(`[{"name":"sid","value":"1"}]`)
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeCookies, Bytes: int64(len(jar))}, bytes.NewReader(jar))
		awaitReturn(t, done)
		if r, n, err := f.st.OpenBrowserCookies("alice", "vendor.example"); err != nil || n != int64(len(jar)) {
			t.Errorf("stored cookies = %d bytes, %v", n, err)
		} else {
			got, _ := io.ReadAll(r)
			_ = r.Close()
			if !bytes.Equal(got, jar) {
				t.Errorf("stored cookies = %q", got)
			}
		}
		if states := f.reg.states(); states["alice/vendor.example"] != string(browserRunning) {
			t.Errorf("a running browser was ended by shutdown: %v", states)
		}
	})
	t.Run("nothing to report", func(t *testing.T) {
		f := newBrowserFixture(t)
		_, link := f.running(ctx, "job1")
		done := shutdownIn(f, ctx)
		link.next(t, browserpod.TypeSnapshot)
		f.clk.BlockUntil(1) // the grace is armed
		f.clk.Advance(browserShutdownSnapshotGrace - time.Second)
		select {
		case <-done:
			t.Fatal("Shutdown returned before the grace")
		default:
		}
		f.clk.Advance(time.Second)
		awaitReturn(t, done)
	})
}

// TestBrowserRegistryShutdownIsBoundedAndSkipsWhatHasNothingToKeep: a
// pod that never answers its save holds Shutdown only until its
// context ends; the open session's browser and a starting one are left
// to the boot cleanup, so Shutdown returns at once for them.
func TestBrowserRegistryShutdownIsBoundedAndSkipsWhatHasNothingToKeep(t *testing.T) {
	ctx := context.Background()
	t.Run("a save that never finishes", func(t *testing.T) {
		f := newBrowserFixture(t)
		_, link := f.running(ctx, "job1")
		f.reg.ReleaseGroup("alice", "vendor.example", "job1")
		link.next(t, browserpod.TypeCloseGroup)
		sctx, cancel := context.WithCancel(ctx)
		done := shutdownIn(f, sctx)
		link.next(t, browserpod.TypeSave)
		cancel()
		awaitReturn(t, done)
	})
	t.Run("open and starting browsers", func(t *testing.T) {
		f := newBrowserFixture(t)
		errs := make(chan error, 2)
		go func() {
			_, err := f.reg.Command(ctx, "alice", agentpod.OpenWebSession, "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
			errs <- err
		}()
		link := newFakeLink()
		b := f.connectWith(agentpod.OpenWebSession, link)
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
		cmd := link.next(t, browserpod.TypeCommand)
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true}, nil)
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		// A site browser still starting.
		go func() {
			_, err := f.reg.Command(ctx, "alice", "vendor.example", "job2", browserpod.Message{Name: browserpod.CommandSnapshot})
			errs <- err
		}()
		<-f.prov.startedC
		awaitReturn(t, shutdownIn(f, ctx))
		link.mu.Lock()
		defer link.mu.Unlock()
		for _, fr := range link.frames {
			if fr.msg.Type == browserpod.TypeSave || fr.msg.Type == browserpod.TypeSnapshot {
				t.Errorf("the open browser was asked for %s at shutdown", fr.msg.Type)
			}
		}
	})
}
