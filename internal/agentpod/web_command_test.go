package agentpod

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blockingCore is a fake core on a Unix socket whose web command and
// result routes hold their response until release is closed, as core
// does while a browser starts or a page settles. Its turn event route
// holds the same way, so a test can see which client gives up on a
// slow answer.
func blockingCore(t *testing.T, release <-chan struct{}) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	hold := func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-release:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/subagents/{id}/web", func(w http.ResponseWriter, r *http.Request) {
		if !hold(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(WebCommandResponse{Text: "Loaded Pricing"})
	})
	mux.HandleFunc("POST /v1/agent/{slug}/subagents/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		if !hold(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SubagentResultResponse{Body: "Result accepted"})
	})
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turnID}/event", func(w http.ResponseWriter, r *http.Request) {
		if !hold(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return path
}

// TestWebCommandOutlivesTheShortClientsHeaderTimeout: a web command and
// a result submission wait for core as long as their context allows,
// while a short call on the same client gives up at the header timeout.
// The timeout is made tiny so the short call's failure is observed, not
// waited for; the long calls are released only after it.
func TestWebCommandOutlivesTheShortClientsHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	path := blockingCore(t, release)
	c := newClientWithHeaderTimeout(path, "alice", 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type webOut struct {
		resp *WebCommandResponse
		err  error
	}
	type resOut struct {
		resp *SubagentResultResponse
		err  error
	}
	web := make(chan webOut, 1)
	res := make(chan resOut, 1)
	go func() {
		r, err := c.WebCommand(ctx, "w1", WebCommandRequest{Command: "navigate", Args: json.RawMessage(`{"url":"https://vendor.example/"}`)})
		web <- webOut{r, err}
	}()
	go func() {
		r, err := c.SubmitSubagentResult(ctx, "w1", json.RawMessage(`{"outcome":"found"}`))
		res <- resOut{r, err}
	}()

	// The short client, on the same slow core, fails at its header
	// timeout; the long calls are still waiting.
	if err := c.PostTurnEvent(ctx, "t1", TurnEvent{}); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("short call on a slow core: %v, want a header timeout", err)
	}
	select {
	case out := <-web:
		t.Fatalf("WebCommand returned before core answered: %+v %v", out.resp, out.err)
	case out := <-res:
		t.Fatalf("SubmitSubagentResult returned before core answered: %+v %v", out.resp, out.err)
	default:
	}

	close(release)
	if out := <-web; out.err != nil || out.resp == nil || out.resp.Text != "Loaded Pricing" {
		t.Errorf("WebCommand after core answered = %+v, %v", out.resp, out.err)
	}
	if out := <-res; out.err != nil || out.resp == nil || out.resp.Body != "Result accepted" {
		t.Errorf("SubmitSubagentResult after core answered = %+v, %v", out.resp, out.err)
	}
}

// TestWebCommandStopsWithItsContext: with core silent, the call ends
// when its context does, with that error.
func TestWebCommandStopsWithItsContext(t *testing.T) {
	path := blockingCore(t, make(chan struct{}))
	c := NewClient(path, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := c.WebCommand(ctx, "w1", WebCommandRequest{Command: "snapshot"})
		errs <- err
	}()
	cancel()
	if err := <-errs; err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("WebCommand with a cancelled context: %v, want context.Canceled", err)
	}
}
