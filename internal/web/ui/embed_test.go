package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func built() fstest.MapFS {
	return fstest.MapFS{
		"index.html":               {Data: []byte("<!doctype html><div id=root></div>")},
		"assets/index-abc123.js":   {Data: []byte("console.log(1)")},
		"logos/kivali-icon.svg":    {Data: []byte("<svg/>")},
		"logos/kivali-icon-32.png": {Data: []byte("\x89PNG\r\n\x1a\n")},
		".gitkeep":                 {Data: nil},
	}
}

func serve(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestIndexForAppRoutes(t *testing.T) {
	h := Handler(built())
	for _, p := range []string{"/", "/agents/alice", "/work?goal=4", "/index.html", "/../../etc/passwd", "/.gitkeep", "/logosx/missing.svg"} {
		rr := serve(t, h, p)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: code = %d", p, rr.Code)
			continue
		}
		if !strings.Contains(rr.Body.String(), `id=root`) {
			t.Errorf("%s: body = %q, want index.html", p, rr.Body.String())
		}
		if cc := rr.Header().Get("cache-control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s: cache-control = %q, want no-store", p, cc)
		}
		if ct := rr.Header().Get("content-type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: content-type = %q", p, ct)
		}
	}
}

func TestHashedAssetsAreImmutable(t *testing.T) {
	rr := serve(t, Handler(built()), "/assets/index-abc123.js")
	if rr.Code != http.StatusOK || rr.Body.String() != "console.log(1)" {
		t.Fatalf("code = %d body = %q", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("cache-control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("cache-control = %q", cc)
	}
	if ct := rr.Header().Get("content-type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content-type = %q", ct)
	}
}

// A missing file under a public directory is a 404, never index.html: a
// script answered with HTML fails in the browser with an unreadable
// MIME error, and so does an image. That holds for the directories
// themselves and for anything path-cleaned into them.
func TestMissingAssetIs404(t *testing.T) {
	for _, p := range []string{
		"/assets/index-old999.js", "/assets", "/assets/", "/assets/index.html", "/assets/sub/../", "/logos/../assets/",
		"/logos/missing.svg", "/logos", "/logos/", "/logos/.gitkeep",
	} {
		rr := serve(t, Handler(built()), p)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d, want 404 (body %q)", p, rr.Code, rr.Body.String())
		}
	}
}

func TestPublicFilesRevalidate(t *testing.T) {
	rr := serve(t, Handler(built()), "/logos/kivali-icon.svg")
	if rr.Code != http.StatusOK || rr.Body.String() != "<svg/>" {
		t.Fatalf("code = %d body = %q", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("cache-control"); cc != "no-cache" {
		t.Errorf("cache-control = %q", cc)
	}
}

func TestNotBuiltIs503(t *testing.T) {
	h := Handler(fstest.MapFS{".gitkeep": {}})
	rr := serve(t, h, "/agents")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "make web-build") {
		t.Errorf("body = %q, want it to name make web-build", rr.Body.String())
	}
	if cc := rr.Header().Get("cache-control"); cc != "no-store" {
		t.Errorf("cache-control = %q", cc)
	}
}

func TestIconServesBuildIcon(t *testing.T) {
	rr := serve(t, Icon(built()), "/favicon.ico")
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Body.String(), "\x89PNG") {
		t.Fatalf("code = %d body = %q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("content-type"); ct != "image/png" {
		t.Errorf("content-type = %q", ct)
	}
	if rr := serve(t, Icon(fstest.MapFS{}), "/favicon.ico"); rr.Code != http.StatusNotFound {
		t.Errorf("not built: code = %d, want 404", rr.Code)
	}
}

// The public mount serves the public directories alone: index.html and
// the rest of the build stay behind the session, however the path is
// spelled.
func TestPublicServesOnlyPublicDirs(t *testing.T) {
	h := Public(built())
	for p, body := range map[string]string{
		"/assets/index-abc123.js": "console.log(1)",
		"/logos/kivali-icon.svg":  "<svg/>",
		// Cleaned first, so a path that resolves into a public
		// directory is that file.
		"/assets/%2E%2E/logos/kivali-icon.svg": "<svg/>",
	} {
		if rr := serve(t, h, p); rr.Code != http.StatusOK || rr.Body.String() != body {
			t.Errorf("%s: code = %d body = %q", p, rr.Code, rr.Body.String())
		}
	}
	for _, p := range []string{
		"/", "/index.html", "/agents/alice", "/assets/nope.js", "/.gitkeep",
		// An encoded ".." reaches this handler uncleaned by the mux.
		"/assets/../index.html", "/assets/%2E%2E/index.html", "/logos/%2e%2e/index.html",
	} {
		if rr := serve(t, h, p); rr.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d, want 404 (body %q)", p, rr.Code, rr.Body.String())
		}
	}
}

// Every top-level directory Vite copies from web/public must be public,
// or the sign-in page (served before there is a session) loses it.
func TestPublicDirsCoverWebPublic(t *testing.T) {
	entries, err := os.ReadDir("../../../web/public")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !e.IsDir() || !slices.Contains(PublicDirs, e.Name()) {
			t.Errorf("web/public/%s is not under ui.PublicDirs %v", e.Name(), PublicDirs)
		}
	}
}

// The committed tree carries only dist/.gitkeep, and the embed must
// still compile and answer.
func TestEmbeddedBuildServes(t *testing.T) {
	rr := serve(t, ServeSPA(), "/")
	if rr.Code != http.StatusOK && rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 200 (built) or 503 (not built)", rr.Code)
	}
}
