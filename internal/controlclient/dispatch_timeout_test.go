package controlclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

// Core reads and hashes every file a dispatch's web tasks name before
// it answers. If the dispatch call's bound falls below what the largest
// batch the limits permit needs at a slow disk's pace, that batch fails
// every time, the agent's retry included, and nothing tells it why.
// This guards the bound against a raised upload limit, a lowered
// timeout, or a floor throughput made optimistic, and that it stays
// finite so a wedged core still surfaces as an error.
func TestDispatchTimeoutCoversTheLargestBatch(t *testing.T) {
	// The slowest reading and hashing this test accepts, stated here
	// rather than read from the client, so raising the client's floor
	// above it fails.
	const slowDisk = 20 << 20 // bytes a second
	largest := int64(agentpod.SubagentMaxBatch) * agentpod.WebTaskMaxFiles * agentpod.WebUploadMaxBytes
	need := time.Duration(float64(largest) / slowDisk * float64(time.Second))

	got := New("/nonexistent", "alice").dispatch.Timeout
	if got < need+shortTimeout {
		t.Errorf("dispatch timeout %s is below the %s a %d MB batch needs at %d MB/s, plus %s for the rest",
			got, need, largest>>20, slowDisk>>20, shortTimeout)
	}
	if got <= 0 {
		t.Error("the dispatch call has no bound: a wedged core would hang the agent's tool call")
	}
}

// failingTransport fails every request it is given.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the short client was used")
}

// The bound only helps if RunSubagent sends on the client that has it.
// This guards that a dispatch goes out on the dispatch client and not
// on the short one every other call shares.
func TestRunSubagentUsesTheDispatchClient(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("receipt"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := New(sock, "alice")
	c.http.Transport = failingTransport{}
	args, _ := json.Marshal(map[string]any{"tasks": []any{}})
	if got, err := c.RunSubagent(context.Background(), "alice", args); err != nil || got != "receipt" {
		t.Errorf("RunSubagent = %q, %v", got, err)
	}
	if _, err := c.SubagentStatus(context.Background(), "alice"); err == nil {
		t.Error("the short client's transport was not the one replaced")
	}
}
