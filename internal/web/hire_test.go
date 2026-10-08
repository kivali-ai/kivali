package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

func TestFireRouteIsGone(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/fire", nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code == http.StatusSeeOther {
		t.Fatal("POST /agents/{slug}/fire still archives — the unapproved path must be gone")
	}
	if _, err := srv.Store.GetAgent("alice"); err != nil {
		t.Errorf("alice must still be active after hitting the retired route: %v", err)
	}
}
