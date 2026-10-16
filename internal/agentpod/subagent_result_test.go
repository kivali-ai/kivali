package agentpod

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
	"testing"
	"time"
)

// TestSubmitSubagentResultPostsToCore: the client posts the submission
// as the body to the task's result route with the slug header, and
// hands back core's verdict; a non-2xx is a transport error.
func TestSubmitSubagentResultPostsToCore(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "s")

	var gotSlug, gotID, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/subagents/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		gotSlug, gotID = r.Header.Get(SlugHeader), r.PathValue("id")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if gotID == "boom" {
			http.Error(w, "subagent service not configured", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SubagentResultResponse{Body: "Result not accepted (attempt 1 of 3)", IsError: true})
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
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	c := NewClient(path, "alice")
	submission := json.RawMessage(`{"outcome":"found","result":{"count":3}}`)
	resp, err := c.SubmitSubagentResult(context.Background(), "ab12cd34", submission)
	if err != nil {
		t.Fatalf("SubmitSubagentResult: %v", err)
	}
	if gotSlug != "alice" || gotID != "ab12cd34" || gotBody != string(submission) {
		t.Errorf("core saw slug %q id %q body %q", gotSlug, gotID, gotBody)
	}
	if !resp.IsError || !strings.Contains(resp.Body, "attempt 1 of 3") {
		t.Errorf("response = %+v, want core's verdict", resp)
	}

	if _, err := c.SubmitSubagentResult(context.Background(), "boom", submission); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("non-2xx err = %v, want the status named", err)
	}
	if _, err := c.SubmitSubagentResult(context.Background(), "ab12cd34", json.RawMessage(`{`)); err == nil {
		t.Error("malformed submission was sent")
	}
	if _, err := c.SubmitSubagentResult(context.Background(), "", submission); err == nil {
		t.Error("empty id was sent")
	}
}
