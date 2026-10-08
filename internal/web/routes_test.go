package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// The web app is the protected mux's catch-all, "GET /{path...}". Go's
// mux gives every request to the most specific pattern that matches,
// so each server-owned route must stay registered as its own, more
// specific pattern or the app's index.html would answer it. These pin
// which pattern each server-owned path resolves to, on the public mux
// and behind the session middleware.
func TestServerOwnedRoutesWinOverTheApp(t *testing.T) {
	srv := newTestServer(t)
	const app = "GET /{path...}"

	public := []struct{ method, path, want string }{
		{"GET", "/healthz", "GET /healthz"},
		{"GET", "/readyz", "GET /readyz"},
		{"GET", "/favicon.ico", "GET /favicon.ico"},
		{"GET", "/branding/icon-180.png", "GET /branding/{name}"},
		{"GET", "/assets/index-abc123.js", "GET /assets/"},
		{"GET", "/logos/kivali-icon.svg", "GET /logos/"},
		{"GET", "/login", "GET /login"},
		{"GET", "/api/v1/login", "GET /api/v1/login"},
		{"GET", "/auth/logout", "GET /auth/logout"},
		// Everything else goes behind the session middleware.
		{"GET", "/", "/"},
		{"GET", "/agents/engineering-lead", "/"},
		{"GET", "/api/v1/me", "/"},
		{"GET", "/debug/pprof/heap", "/"},
	}
	for _, c := range public {
		if _, got := srv.mux.Handler(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("public %s %s -> %q, want %q", c.method, c.path, got, c.want)
		}
	}

	protected := []struct{ method, path, want string }{
		{"GET", "/org/stream", "GET /org/stream"},
		{"GET", "/agents/engineering-lead/stream", "GET /agents/{slug}/stream"},
		{"GET", "/agents/engineering-lead/subagents/a1b2/stream", "GET /agents/{slug}/subagents/{id}/stream"},
		{"POST", "/agents/engineering-lead/messages", "POST /agents/{slug}/messages"},
		{"POST", "/agents/engineering-lead/stop", "POST /agents/{slug}/stop"},
		{"GET", "/messages/2026-09-28/0001-ceo.md", "GET /messages/{path...}"},
		{"GET", "/attachments/3f2a", "GET /attachments/{sha}"},
		{"GET", "/admin/version", "GET /admin/version"},
		{"GET", "/api/v1/me", "GET /api/"},
		{"GET", "/api/v1/nope", "GET /api/"},
		{"POST", "/api/v1/needs/approve", "POST /api/"},
		{"DELETE", "/api/v1/org/logo", "DELETE /api/"},
		// Client-router paths all load the app.
		{"GET", "/", app},
		{"GET", "/team", app},
		{"GET", "/agents/engineering-lead", app},
		{"GET", "/agents/engineering-lead/background", app},
		{"GET", "/agents/engineering-lead/subagents/a1b2", app},
		{"GET", "/assignments/12", app},
		{"GET", "/proposals/messages/2026-09-28/0003-chief-of-staff.md", app},
		{"GET", "/setup/files", app},
		{"GET", "/no/such/page", app},
	}
	for _, c := range protected {
		if _, got := srv.protectedMux.Handler(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("protected %s %s -> %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// An agent's page loads the app; its /stream is the SSE handler, which
// answers an idle agent with 204 rather than a page.
func TestAgentPageIsTheAppAndItsStreamIsSSE(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "engineering-lead", Role: "Engineering lead", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatal(err)
	}
	page := get(t, srv, "/agents/engineering-lead")
	if page.Code != http.StatusOK && page.Code != http.StatusServiceUnavailable {
		t.Fatalf("/agents/engineering-lead: code = %d, want the index (200) or the not-built page (503)", page.Code)
	}
	if ct := page.Header().Get("content-type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/agents/engineering-lead: content-type = %q, want the app's HTML", ct)
	}
	if stream := get(t, srv, "/agents/engineering-lead/stream"); stream.Code != http.StatusNoContent {
		t.Errorf("/agents/engineering-lead/stream: code = %d, want 204 from the SSE handler for an idle agent", stream.Code)
	}
}

// Everything but the sign-in screen, the build files it loads and health
// sits behind the session. Without one, a GET
// for an app path or a server-owned document goes to the sign-in
// screen with the path in next, a non-GET is a plain 401, and the API
// answers its JSON 401 for every method and for unknown paths.
func TestAppRoutesNeedASession(t *testing.T) {
	srv := newTestServer(t)
	serve := func(method, path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		return rr
	}
	for _, path := range []string{
		"/", "/agents/engineering-lead", "/work?goal=4", "/team", "/org", "/setup", "/assignments/12",
		"/org/stream", "/agents/engineering-lead/stream", "/agents/engineering-lead/subagents/a1b2/stream",
		"/messages/2026-09-28/0001-ceo.md", "/messages/2026-09-28/0001-ceo.md?dl=1",
		"/attachments/3f2a", "/admin/version",
		// No debug surface is served: this is just another app path.
		"/debug/pprof/", "/debug/pprof/heap",
	} {
		rr := serve(http.MethodGet, path)
		if want := "/login?next=" + url.QueryEscape(path); rr.Code != http.StatusFound || rr.Header().Get("location") != want {
			t.Errorf("unauthenticated GET %s: code = %d location = %q, want 302 to %q", path, rr.Code, rr.Header().Get("location"), want)
		}
	}
	for _, path := range []string{
		"/agents/engineering-lead/messages", "/agents/engineering-lead/stop",
	} {
		rr := serve(http.MethodPost, path)
		if rr.Code != http.StatusUnauthorized || rr.Header().Get("location") != "" {
			t.Errorf("unauthenticated POST %s: code = %d location = %q, want a plain 401", path, rr.Code, rr.Header().Get("location"))
		}
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/me"},
		{http.MethodGet, "/api/v1/home"},
		{http.MethodGet, "/api/v1/nope"},
		{http.MethodPost, "/api/v1/needs/approve"},
		{http.MethodPost, "/api/v1/queue/release-all"},
		{http.MethodDelete, "/api/v1/org/logo"},
	} {
		rr := serve(c.method, c.path)
		if ct := rr.Header().Get("content-type"); rr.Code != http.StatusUnauthorized || !strings.HasPrefix(ct, "application/json") {
			t.Errorf("unauthenticated %s %s: code = %d content-type = %q, want a JSON 401", c.method, c.path, rr.Code, ct)
		}
	}

	// The public surface: the sign-in screen, what it reads and loads,
	// and health.
	if rr := serve(http.MethodGet, "/login"); rr.Code != http.StatusOK && rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/login: code = %d, want the index (200) or the not-built page (503)", rr.Code)
	}
	if rr := serve(http.MethodGet, "/api/v1/login"); rr.Code != http.StatusOK {
		t.Errorf("/api/v1/login: code = %d, want 200", rr.Code)
	}
	if rr := serve(http.MethodGet, "/healthz"); rr.Code != http.StatusOK {
		t.Errorf("/healthz: code = %d, want 200", rr.Code)
	}
	// A missing build file is a 404 even without a session: never a
	// redirect to the sign-in page, which a <script> or <img> cannot
	// follow, and never index.html. Nor does an encoded ".." under a
	// public directory reach the rest of the build.
	for _, path := range []string{
		"/assets/index-nope.js", "/logos/nope.svg", "/assets/", "/logos/",
		"/assets/%2E%2E/index.html", "/logos/%2e%2e/index.html",
	} {
		if rr := serve(http.MethodGet, path); rr.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d, want 404", path, rr.Code)
		}
	}
}
