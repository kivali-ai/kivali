// Package ui serves the Kivali web app: the Vite build of web/,
// embedded into the binary and mounted at the site root.
//
// `make web-build` (and the Dockerfile's web-build stage) writes the
// build into dist/. Only dist/.gitkeep is committed, so `go build` works
// on a fresh checkout; a binary built that way answers every app route
// with a 503 that says the frontend has not been built.
package ui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Dist is the embedded build, rooted at dist/.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// fs.Sub fails only on an invalid path; "dist" is a literal.
		panic(err)
	}
	return sub
}

// PublicDirs are the build's top-level directories, served without a
// session because the sign-in page loads from them: Vite's hashed
// output and the logos copied from web/public. A test keeps this list
// in step with web/public.
var PublicDirs = []string{assetsDir, "logos"}

// assetsDir holds Vite's content-hashed output: a file's name changes
// whenever its bytes do, so it can be cached forever.
const assetsDir = "assets"

// publicDir reports which of PublicDirs the build-relative path rel is
// or lies under, if any.
func publicDir(rel string) (string, bool) {
	for _, dir := range PublicDirs {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return dir, true
		}
	}
	return "", false
}

// IconPath is the build file /favicon.ico answers with.
const IconPath = "logos/kivali-icon-32.png"

// notBuiltPage is what an app route answers when the binary carries no
// build.
const notBuiltPage = "<!doctype html><meta charset=utf-8><title>Kivali</title>" +
	"<p>The web app has not been built into this binary; <code>make web-build</code> builds it.</p>\n"

// ServeSPA serves the embedded build. See Handler.
func ServeSPA() http.Handler { return Handler(Dist()) }

// ServeIcon serves the embedded build's Kivali icon. See Icon.
func ServeIcon() http.Handler { return Icon(Dist()) }

// Handler serves a Vite build from fsys at the site root. The caller
// mounts it only on paths the server does not own itself (the API,
// streams, raw messages, attachments and so on are registered as more
// specific routes on the same mux):
//
//   - /assets/<file>: the hashed file, cached for a year as immutable,
//     or 404. Never index.html: a missing script answered with HTML
//     fails in the browser with a MIME error nobody can read.
//   - a file under any other PublicDirs entry (the logos copied from
//     web/public): that file, revalidated on every use since its name
//     is not hashed, or 404 for the same reason.
//   - any other path naming a file in the build: that file, revalidated.
//   - everything else: index.html with no-store, so the client router
//     owns the path and a deploy is picked up on the next navigation.
//     503 when the build has no index.html.
//
// Dotfiles (dist/.gitkeep) are never served.
func Handler(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := buildPath(r.URL.Path)
		if dir, ok := publicDir(rel); ok {
			servePublicFile(w, r, fsys, dir, rel)
			return
		}
		if rel != "" && rel != "index.html" && serveFile(w, r, fsys, rel, "no-cache") {
			return
		}
		if !serveFile(w, r, fsys, "index.html", "no-store, must-revalidate") {
			w.Header().Set("content-type", "text/html; charset=utf-8")
			w.Header().Set("cache-control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, notBuiltPage)
		}
	})
}

// Public serves the files under PublicDirs exactly as Handler does and
// answers every other path 404, never index.html. It is what the
// server mounts without a session, so a request there reaches nothing
// but the sign-in page's own files however its path is spelled: an
// encoded ".." in a segment is cleaned here, not by the mux.
func Public(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := buildPath(r.URL.Path)
		dir, ok := publicDir(rel)
		if !ok {
			http.NotFound(w, r)
			return
		}
		servePublicFile(w, r, fsys, dir, rel)
	})
}

// ServePublic serves the embedded build's PublicDirs alone. See Public.
func ServePublic() http.Handler { return Public(Dist()) }

// buildPath is the build-relative file a request path names: cleaned
// of "." and ".." segments, without the leading slash.
func buildPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// servePublicFile writes the file rel under the public directory dir,
// or 404, the directory itself included: nothing under a public
// directory is ever index.html, because a missing script or image
// answered with HTML fails in the browser with an error nobody can
// read. Hashed assets are immutable; the rest revalidate.
func servePublicFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, dir, rel string) {
	cache := "no-cache"
	if dir == assetsDir {
		cache = "public, max-age=31536000, immutable"
	}
	if !serveFile(w, r, fsys, rel, cache) {
		http.NotFound(w, r)
	}
}

// Icon answers with the build's IconPath, revalidated on every use, or
// 404 when the binary carries no build.
func Icon(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveFile(w, r, fsys, IconPath, "no-cache") {
			http.NotFound(w, r)
		}
	})
}

// serveFile writes fsys's regular file name with the given
// cache-control and reports whether there was one to write.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name, cacheControl string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	w.Header().Set("cache-control", cacheControl)
	if rs, ok := f.(io.ReadSeeker); ok {
		// embed.FS has a zero modtime, so ServeContent sets no
		// Last-Modified; the cache-control above is the whole policy.
		http.ServeContent(w, r, name, time.Time{}, rs)
		return true
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(b)))
	return true
}
