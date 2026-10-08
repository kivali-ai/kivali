package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// testOwnerEmail is the synthetic owner email every test server's
// allowlist admits, as OWNER_EMAILS would. Tests
// mint session cookies for this email and attach them to requests.
const testOwnerEmail = "test@kivali.local"

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	codec, _ := auth.NewCodec([]byte("01234567890123456789012345678901"))
	al := auth.NewAllowlist(testOwnerEmail)
	srv, err := NewServer(&Server{
		Store:    s,
		Provider: provider.MockProvider{},
		AuthMW:   &auth.Middleware{Codec: codec, Allowlist: al},
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	return srv
}

// authCookie returns a session cookie minted from the test server's codec
// for testOwnerEmail. The middleware accepts it like any real session.
func authCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	tok, err := srv.AuthMW.Codec.EncodeSession(auth.NewSession(testOwnerEmail))
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	return &http.Cookie{Name: auth.CookieName, Value: tok}
}

// authedHandler wraps srv.Handler() with auto-injected session cookie for
// in-process ServeHTTP calls. Lets tests build requests with
// httptest.NewRequest and serve them without threading auth through every
// call site. For tests that drive httptest.NewServer over a real HTTP
// transport, the cookie still needs to be attached client-side via
// req.AddCookie(authCookie(t, srv)) — the wrapper can't reach across the
// network.
func authedHandler(t *testing.T, srv *Server) http.Handler {
	t.Helper()
	cookie := authCookie(t, srv)
	real := srv.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(auth.CookieName); err != nil {
			r.AddCookie(cookie)
		}
		// A page-route request that names no site is the app's own
		// page, as a browser would mark it. API tests name the site
		// themselves, a refusal included.
		if !strings.HasPrefix(r.URL.Path, auth.APIPrefix) && r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Origin") == "" {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		real.ServeHTTP(w, r)
	})
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rr := get(t, srv, "/healthz")
	if rr.Code != http.StatusOK {
		t.Errorf("code = %d", rr.Code)
	}
	body := rr.Body.String()
	// Locks the public-/healthz JSON shape. Version + env are
	// deliberately NOT here — they live behind the auth middleware
	// at /admin/version.
	for _, want := range []string{
		`"status":"ok"`,
		`"clock_drift_seconds":`,
		`"clock_check_age_seconds":`,
		`"clock_check_ref":`,
		`"clock_check_err":`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/healthz body missing %q: %s", want, body)
		}
	}
	// Negative checks: build version and env must NOT leak on the
	// public /healthz. Removing these protects against fingerprint-
	// based vulnerability targeting.
	for _, banned := range []string{
		`"version":`,
		`"env":`,
	} {
		if strings.Contains(body, banned) {
			t.Errorf("/healthz body MUST NOT contain %q (build fingerprint disclosure): %s", banned, body)
		}
	}
}

func TestReadyz(t *testing.T) {
	srv := newTestServer(t)
	if rr := get(t, srv, "/readyz"); rr.Code != http.StatusOK {
		t.Errorf("code = %d", rr.Code)
	}
}

// TestOrgSnapshotSortsAgentsBySlug locks in the JSON ordering
// invariant — orgHub uses byte-equality dedup, so non-stable
// ordering would generate spurious wakeups.
func TestOrgSnapshotSortsAgentsBySlug(t *testing.T) {
	srv := newTestServer(t)
	for _, slug := range []string{"zebra", "chief-of-staff", "alice"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "k"); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	srv.chatHubs = map[string]*chatHub{
		"zebra":          {hub: newHub(), slug: "zebra"},
		"chief-of-staff": {hub: newHub(), slug: "chief-of-staff"},
		"alice":          {hub: newHub(), slug: "alice"},
	}
	body := srv.buildOrgSnapshot()
	var snap orgSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	want := []string{"alice", "chief-of-staff", "zebra"}
	if len(snap.Agents) != len(want) {
		t.Fatalf("agents = %v, want %v", snap.Agents, want)
	}
	for i, slug := range want {
		if snap.Agents[i].Slug != slug {
			t.Errorf("agents[%d].Slug = %q, want %q", i, snap.Agents[i].Slug, slug)
		}
		if snap.Agents[i].State != "running" {
			t.Errorf("agents[%d] (%s) should be running", i, slug)
		}
	}
}

// TestOrgSnapshotEmptyAtRest verifies an idle install produces a
// non-nil agents array (so the client can trust the shape and skip
// existence checks) and zero counts everywhere.
func TestOrgSnapshotEmptyAtRest(t *testing.T) {
	srv := newTestServer(t)
	body := srv.buildOrgSnapshot()
	var snap orgSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if snap.Agents == nil {
		t.Errorf("agents should be empty array, not null; body=%s", body)
	}
	if snap.Release.Running || snap.Release.PendingDocs != 0 {
		t.Errorf("release should be idle, got %+v", snap.Release)
	}
	if snap.Inbox.Unactioned != 0 {
		t.Errorf("inbox.unactioned should be 0, got %d", snap.Inbox.Unactioned)
	}
}
