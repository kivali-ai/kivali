package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// putAgentRole saves an agent's role through the docs API.
func putAgentRole(t *testing.T, srv *Server, slug, content string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	return apiDo(t, srv, http.MethodPut, "/api/v1/agents/"+slug+"/docs/role", string(body), nil)
}

// TestAgentSetRolePersists drives the happy path: PUT a new body, the
// store writes it verbatim, the next read reflects the new role.
func TestAgentSetRolePersists(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "# old role\n"); err != nil {
		t.Fatal(err)
	}
	const newRole = "# New role\n\nDo the updated thing."
	if rr := putAgentRole(t, srv, "alice", newRole); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	got, err := srv.Store.ReadRole("alice")
	if err != nil {
		t.Fatalf("ReadRole: %v", err)
	}
	if got != newRole {
		t.Errorf("role = %q, want %q", got, newRole)
	}
}

// TestAgentSetRoleRefusesCEO asserts the CEO slug is off-limits.
// CEO isn't a real agent; there's no role.md at /data/agents/ceo
// to edit.
func TestAgentSetRoleRefusesCEO(t *testing.T) {
	srv := newTestServer(t)
	if rr := putAgentRole(t, srv, "ceo", "not valid"); rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rr.Code)
	}
}

// TestAgentSetRoleRefusesEmpty — blank or whitespace-only body gets
// 400 rather than silently wiping the agent's identity doc.
func TestAgentSetRoleRefusesEmpty(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst"}, "# keep me\n"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "   ", "\n\n"} {
		if rr := putAgentRole(t, srv, "alice", body); rr.Code != http.StatusBadRequest {
			t.Errorf("body=%q code = %d, want 400", body, rr.Code)
		}
	}
	if got, _ := srv.Store.ReadRole("alice"); got != "# keep me\n" {
		t.Errorf("role clobbered by blank PUT: %q", got)
	}
}
