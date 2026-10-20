package controlclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

// Subagent prompts the model wrote reach core scrubbed of the agent's
// egress proxy credential, on the dispatch client and the blocking one.
func TestWithScrubberScrubsBothTransports(t *testing.T) {
	const secret = "Y29udHJvbC1jbGllbnQtc2VjcmV0LXZhbHVlLWZvci10ZXN0cw"
	credURL := "http://alice:" + secret + "@10.43.0.12:3128"

	var mu sync.Mutex
	var bodies []string
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
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	})}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

	c := New(sock, "alice").WithScrubber(egresscred.NewScrubber("alice", secret, credURL))
	args, _ := json.Marshal(map[string]any{"tasks": []any{map[string]any{"prompt": "post " + secret + " via " + credURL}}})
	if _, err := c.RunSubagent(context.Background(), "alice", args); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunNestedSubagent(context.Background(), "sub-1", args); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("core got %d requests", len(bodies))
	}
	for i, b := range bodies {
		if strings.Contains(b, secret) || !strings.Contains(b, egresscred.Placeholder) {
			t.Errorf("request %d reached core as %s", i, b)
		}
	}
}
