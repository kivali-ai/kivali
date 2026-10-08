package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

// startFakeCoreForPublish stands up a unix-socket HTTP server with
// per-test handlers for /publish/stage and /publish/commit. Both
// handlers are required; pass nil to use a stub that always returns
// a success body. Returns the socket path and a cleanup func.
func startFakeCoreForPublish(
	t *testing.T,
	stage http.HandlerFunc,
	commit http.HandlerFunc,
) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcp-bridge-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "core.sock")
	mux := http.NewServeMux()
	if stage == nil {
		stage = func(w http.ResponseWriter, _ *http.Request) {
			writeJSONForTest(t, w, agentpod.PublishStageResponse{StageID: "fixed-stage-id"})
		}
	}
	if commit == nil {
		commit = func(w http.ResponseWriter, _ *http.Request) {
			writeJSONForTest(t, w, agentpod.PublishCommitResponse{Body: "ok"})
		}
	}
	mux.HandleFunc("POST /v1/agent/{slug}/publish/stage", stage)
	mux.HandleFunc("POST /v1/agent/{slug}/publish/commit", commit)
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = os.RemoveAll(dir)
	}
	return path, cleanup
}

func writeJSONForTest(t *testing.T, w http.ResponseWriter, v interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Logf("encode response: %v", err)
	}
}

// shrinkCommitBackoff replaces commitRetryBackoff with negligible
// delays for the duration of t. Restores the original on cleanup so
// other tests aren't affected.
func shrinkCommitBackoff(t *testing.T) {
	t.Helper()
	prev := commitRetryBackoff
	commitRetryBackoff = []time.Duration{
		time.Millisecond,
		time.Millisecond,
		time.Millisecond,
	}
	t.Cleanup(func() { commitRetryBackoff = prev })
}

// TestAgentpodPublishBridgeRetriesCommitOnTransportError: when
// CommitPublish returns a 5xx (simulating "core died between
// commit and response"), the bridge MUST retry rather than surface
// the error. After the simulated transient failure, the second
// commit attempt succeeds, and the bridge returns the success body
// to the model. Confirms the bridge did not propagate ambiguity.
func TestAgentpodPublishBridgeRetriesCommitOnTransportError(t *testing.T) {
	shrinkCommitBackoff(t)
	var commitAttempts atomic.Int32
	sock, cleanup := startFakeCoreForPublish(t, nil,
		func(w http.ResponseWriter, _ *http.Request) {
			n := commitAttempts.Add(1)
			if n < 2 {
				http.Error(w, "simulated transport-layer failure", http.StatusServiceUnavailable)
				return
			}
			writeJSONForTest(t, w, agentpod.PublishCommitResponse{
				Body: "publish_notice published: \"hello\" → bob (saved to messages/...)",
			})
		})
	defer cleanup()

	c := agentpod.NewClient(sock, "alice")
	d := NewAgentpodPublishDispatcher(c)
	body, isErr, err := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"to":"bob","title":"hello","body":"world"}`))
	if err != nil {
		t.Fatalf("DispatchPublish: %v", err)
	}
	if isErr {
		t.Fatalf("isError=true unexpectedly; body=%q", body)
	}
	if got := commitAttempts.Load(); got != 2 {
		t.Errorf("commit attempts = %d, want 2 (first failed, second succeeded)", got)
	}
	if body == "" {
		t.Errorf("body empty, want the success body from the second attempt")
	}
}

// TestAgentpodPublishBridgeExhaustsRetries: when every commit attempt
// fails transport-layer, the bridge surfaces a wrapped error after
// commitRetryAttempts. The model sees IsError=true via publishHandler.
// The bridge did its best; the error message names the underlying
// HTTP failure so operators can trace it.
func TestAgentpodPublishBridgeExhaustsRetries(t *testing.T) {
	shrinkCommitBackoff(t)
	var commitAttempts atomic.Int32
	sock, cleanup := startFakeCoreForPublish(t, nil,
		func(w http.ResponseWriter, _ *http.Request) {
			commitAttempts.Add(1)
			http.Error(w, "persistently broken", http.StatusInternalServerError)
		})
	defer cleanup()

	c := agentpod.NewClient(sock, "alice")
	d := NewAgentpodPublishDispatcher(c)
	_, _, err := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"to":"bob","title":"hello","body":"world"}`))
	if err == nil {
		t.Fatalf("expected error after exhausting retries, got nil")
	}
	if got := commitAttempts.Load(); int(got) != commitRetryAttempts {
		t.Errorf("commit attempts = %d, want %d", got, commitRetryAttempts)
	}
}

// TestAgentpodPublishBridgeStageErrorNoCommit: a deterministic
// stage failure (IsError=true on the stage response) MUST short-
// circuit — no commit attempt fires. Parse / conflict / attachment
// errors don't get any more deterministic by retrying.
func TestAgentpodPublishBridgeStageErrorNoCommit(t *testing.T) {
	shrinkCommitBackoff(t)
	var commitAttempts atomic.Int32
	sock, cleanup := startFakeCoreForPublish(t,
		func(w http.ResponseWriter, _ *http.Request) {
			writeJSONForTest(t, w, agentpod.PublishStageResponse{
				IsError: true,
				Body:    "parse: invalid input",
			})
		},
		func(w http.ResponseWriter, _ *http.Request) {
			commitAttempts.Add(1)
			writeJSONForTest(t, w, agentpod.PublishCommitResponse{Body: "should not be called"})
		})
	defer cleanup()

	c := agentpod.NewClient(sock, "alice")
	d := NewAgentpodPublishDispatcher(c)
	body, isErr, err := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"junk":true}`))
	if err != nil {
		t.Fatalf("DispatchPublish: %v", err)
	}
	if !isErr {
		t.Errorf("expected IsError=true for stage error, got body=%q", body)
	}
	if !strings.Contains(body, "parse: invalid input") {
		t.Errorf("body = %q, want the stage error string", body)
	}
	if got := commitAttempts.Load(); got != 0 {
		t.Errorf("commit attempts = %d, want 0 (stage error must short-circuit)", got)
	}
}

// TestAgentpodPublishBridgeCommitNotFound: when /publish/commit
// returns 404 (staged record missing — janitor expired it, or a
// stale bridge against a fresh core), the bridge surfaces
// ErrCommitStageNotFound immediately rather than retrying. Retrying
// can't conjure the staged record back; the bridge fails fast so the
// caller can decide whether to re-stage.
func TestAgentpodPublishBridgeCommitNotFound(t *testing.T) {
	shrinkCommitBackoff(t)
	var commitAttempts atomic.Int32
	sock, cleanup := startFakeCoreForPublish(t, nil,
		func(w http.ResponseWriter, _ *http.Request) {
			commitAttempts.Add(1)
			http.Error(w, "stage_id not found", http.StatusNotFound)
		})
	defer cleanup()

	c := agentpod.NewClient(sock, "alice")
	d := NewAgentpodPublishDispatcher(c)
	_, _, err := d.DispatchPublish(context.Background(), "publish_notice",
		json.RawMessage(`{"to":"bob","title":"hello","body":"world"}`))
	if !errors.Is(err, agentpod.ErrCommitStageNotFound) {
		t.Fatalf("err = %v, want ErrCommitStageNotFound", err)
	}
	if got := commitAttempts.Load(); got != 1 {
		t.Errorf("commit attempts = %d, want 1 (404 short-circuits retry)", got)
	}
}
