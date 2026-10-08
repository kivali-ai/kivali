package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

func TestAPIDesktopFacts(t *testing.T) {
	srv := newTestServer(t)
	for _, slug := range []string{"chief-of-staff", "writer"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "k"); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	// One agent mid-turn.
	if srv.chatHubs == nil {
		srv.chatHubs = map[string]*chatHub{}
	}
	srv.chatHubs["writer"] = newTestHub()
	for _, name := range []string{"plan.md", "notes.txt"} {
		if _, err := srv.Store.AddProjectFile(name, strings.NewReader("hello "+name)); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	rr := apiDo(t, srv, http.MethodGet, "/api/v1/desktop/facts", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d body %q", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("cache-control"); cc != "no-store" {
		t.Errorf("cache-control = %q", cc)
	}
	got := decodeAPI[DesktopFacts](t, rr)
	want := DesktopFacts{Email: testOwnerEmail, Agents: 2, Working: 1, Files: 2}
	if got != want {
		t.Errorf("facts = %+v, want %+v", got, want)
	}
	// The desktop shell reads exactly these keys (src-tauri/src/teamapi.rs).
	for _, key := range []string{`"email":`, `"agents":`, `"working":`, `"files":`} {
		if !strings.Contains(rr.Body.String(), key) {
			t.Errorf("body lacks %s: %s", key, rr.Body.String())
		}
	}

	// "Working" is the web app's own count.
	var snap apitypes.OrgSnapshot
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Readouts.Working != got.Working {
		t.Errorf("snapshot working = %d, facts working = %d", snap.Readouts.Working, got.Working)
	}
}

func TestAPIDesktopFactsNeedsASession(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/desktop/facts", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	assertAPIError(t, rr, http.StatusUnauthorized)
}
