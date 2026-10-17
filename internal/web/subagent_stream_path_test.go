package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// TestSubagentStreamRefusesPathLikeIDs: the stream route joins the
// slug and the task id into a path under agents/, so either one that
// is not a plain segment is 404, as the transcript route has it; a
// plain id of a task on disk streams.
func TestSubagentStreamRefusesPathLikeIDs(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	// A task of alice's, and a directory a path-like id could reach.
	for _, dir := range []string{
		filepath.Join(srv.Store.Root(), "agents", "alice", "subagents", "abc123ef"),
		filepath.Join(srv.Store.Root(), "agents", "_archived", "bob", "subagents", "gone1234"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"status":"completed"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := authedHandler(t, srv)
	for _, path := range []string{
		"/agents/alice/subagents/..%2F..%2F_archived%2Fbob%2Fsubagents%2Fgone1234/stream",
		"/agents/alice/subagents/.hidden/stream",
		"/agents/_archived%2Fbob/subagents/gone1234/stream",
		"/agents/..%2Falice/subagents/abc123ef/stream",
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/agents/alice/subagents/abc123ef/stream", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("a plain id: status %d, want 200", rr.Code)
	}
}
