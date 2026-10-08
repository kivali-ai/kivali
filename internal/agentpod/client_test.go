package agentpod

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServerOpts configures startFakeCoreSocket — controls how the
// per-event-stream handler behaves.
type fakeServerOpts struct {
	// onConnect is called for every events stream that opens. Useful
	// to count reconnects.
	onConnect func()
	// behavior runs inside the SSE handler with a Flusher. Returning
	// closes the stream.
	behavior func(w http.ResponseWriter, flush func(), r *http.Request)
}

func startFakeCoreSocket(t *testing.T, opts fakeServerOpts) (path string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir tmp: %v", err)
	}
	path = filepath.Join(dir, "s")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if opts.onConnect != nil {
			opts.onConnect()
		}
		if opts.behavior != nil {
			flush := func() {
				if flusher != nil {
					flusher.Flush()
				}
			}
			opts.behavior(w, flush, r)
		}
	})
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen unix %s: %v", path, err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.RemoveAll(dir)
	}
	return path, cleanup
}

// TestRunEventsReconnectsOnDrop verifies the reconnect-with-backoff
// loop in RunEvents. The fake server emits a single event then closes
// the stream; RunEvents should re-dial.
func TestRunEventsReconnectsOnDrop(t *testing.T) {
	var connects atomic.Int32
	path, cleanup := startFakeCoreSocket(t, fakeServerOpts{
		onConnect: func() { connects.Add(1) },
		behavior: func(w http.ResponseWriter, flush func(), _ *http.Request) {
			_, _ = fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			flush()
			// Server returns → stream closes.
		},
	})
	defer cleanup()

	c := NewClient(path, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var pings atomic.Int32
	go func() {
		_ = c.RunEvents(ctx, func(ev Event) error {
			if ev.Type == EventPing {
				pings.Add(1)
				if pings.Load() >= 3 {
					cancel()
				}
			}
			return nil
		})
	}()

	// Wait for at least 3 pings (== 3 reconnects through the
	// drop/redial loop). Generous timeout for slow CI; backoff is
	// 200ms minimum so 3 cycles is at least 400ms.
	deadline := time.Now().Add(5 * time.Second)
	for pings.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d pings after timeout (connects=%d)", pings.Load(), connects.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if connects.Load() < 3 {
		t.Errorf("connects = %d, want at least 3 (reconnect loop)", connects.Load())
	}
}

// TestRunEventsHandlerErrorTerminates locks in the rule that a
// non-nil return from the handler stops the loop. The agent
// runtime uses this to bail on truly fatal events.
func TestRunEventsHandlerErrorTerminates(t *testing.T) {
	path, cleanup := startFakeCoreSocket(t, fakeServerOpts{
		behavior: func(w http.ResponseWriter, flush func(), _ *http.Request) {
			_, _ = fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			flush()
			time.Sleep(50 * time.Millisecond) // hold the stream open
		},
	})
	defer cleanup()

	c := NewClient(path, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sentinel := errors.New("handler bailed")
	err := c.RunEvents(ctx, func(_ Event) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
}

// TestRunEventsCancelsOnContextDone verifies clean ctx cancel.
func TestRunEventsCancelsOnContextDone(t *testing.T) {
	path, cleanup := startFakeCoreSocket(t, fakeServerOpts{
		behavior: func(w http.ResponseWriter, flush func(), r *http.Request) {
			<-r.Context().Done()
		},
	})
	defer cleanup()

	c := NewClient(path, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := c.RunEvents(ctx, func(_ Event) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Done", err)
	}
}

// TestOpenEventsRejectsNon200 ensures we surface a clean error when
// the server replies with anything but 200 + text/event-stream.
// Builds a bespoke server here (the standard fake helper writes
// 200 + content-type for SSE before delegating, which we want to
// short-circuit for this test).
func TestOpenEventsRejectsNon200(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "s")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/events", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	})
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = srv.Serve(ln)
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	c := NewClient(sockPath, "alice")
	_, err = c.OpenEvents(context.Background())
	if err == nil {
		t.Fatal("expected error on 503; got nil")
	}
	if got := err.Error(); !contains(got, "503") {
		t.Errorf("err = %q, expected to mention status code", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
