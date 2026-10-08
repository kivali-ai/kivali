package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestSettingsShowsBuildVersion locks where the running build is
// readable: GET /admin/version, behind the session (it is deliberately
// not on the public /healthz; see TestHealthz).
func TestSettingsShowsBuildVersion(t *testing.T) {
	srv := newTestServer(t)
	srv.VersionName = "v9.9.9-test"

	rr := get(t, srv, "/admin/version")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /admin/version: status %d", rr.Code)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rr.Body.String())
	}
	if v.Version != "v9.9.9-test" {
		t.Errorf("version = %q, want v9.9.9-test", v.Version)
	}
}
