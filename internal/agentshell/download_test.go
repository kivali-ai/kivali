package agentshell

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// fakeFiles is the service a download reaches, on one host: an API that
// redirects to storage, as GitHub's does, recording what each request
// carried.
type fakeFiles struct {
	*httptest.Server
	mu      sync.Mutex
	apis    []http.Header
	storage []*http.Request
	// api answers the API's request; nil redirects to /storage?sig=s1gned.
	api func(w http.ResponseWriter, r *http.Request)
	// body answers storage's request.
	body string
}

func newFakeFiles(t *testing.T) *fakeFiles {
	t.Helper()
	f := &fakeFiles{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/file", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.apis = append(f.apis, r.Header.Clone())
		api := f.api
		f.mu.Unlock()
		if api != nil {
			api(w, r)
			return
		}
		http.Redirect(w, r, "/storage?sig=s1gned", http.StatusFound)
	})
	mux.HandleFunc("GET /storage", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.storage = append(f.storage, r)
		f.mu.Unlock()
		_, _ = w.Write([]byte(f.body))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeFiles) host() string { return strings.Split(strings.TrimPrefix(f.URL, "http://"), ":")[0] }

// downloadJob is a job for f's file, landing as name in a folder of
// root, reaching only f's host.
func downloadJob(f *fakeFiles, name string) DownloadRequest {
	return DownloadRequest{
		URL: f.URL + "/api/file", Token: "ghs_jobtoken", Accept: "application/vnd.github+json",
		Hosts: []string{f.host()}, Folder: "integrations/github/untrusted-downloads/artifact-1", Name: name, ConnectionID: "c0ffee01",
	}
}

// downloadCfg is a daemon's configuration over a new root.
func downloadCfg(t *testing.T) ServerConfig {
	t.Helper()
	return ServerConfig{Root: t.TempDir(), GitInsecureHTTP: true, Clock: clock.NewFake()}
}

// folderFiles is every entry of the job's folder under root, temporary
// ones included.
func folderFiles(t *testing.T, cfg ServerConfig, req DownloadRequest) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cfg.Root, req.Folder))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Why this test exists: this is the shell's half of an artifact's
// download. The token is the API's alone: the redirect's storage host
// must get no credential of the job's, or a leaked or logged request
// there would hand out access to the repository. The file must land
// whole under its folder, carrying .zip only because it is one, and the
// answer must echo the connection for core's second request.
func TestDownloadFollowsTheRedirectWithNoCredential(t *testing.T) {
	f := newFakeFiles(t)
	f.body = "PK\x03\x04 pretend zip"
	cfg := downloadCfg(t)
	req := downloadJob(f, "coverage")
	req.NameZip = true

	res := runDownload(context.Background(), cfg, req)
	if res.Error != "" {
		t.Fatalf("download = %+v", res)
	}
	if want := req.Folder + "/coverage.zip"; res.Path != want || res.Bytes != int64(len(f.body)) || !res.Zip || res.ConnectionID != "c0ffee01" {
		t.Errorf("response = %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(cfg.Root, res.Path))
	if err != nil || string(got) != f.body {
		t.Errorf("file = %q, %v", got, err)
	}
	if len(f.apis) != 1 || f.apis[0].Get("Authorization") != "Bearer ghs_jobtoken" || f.apis[0].Get("Accept") != req.Accept {
		t.Errorf("API requests = %+v", f.apis)
	}
	if len(f.storage) != 1 {
		t.Fatalf("storage requests = %d", len(f.storage))
	}
	s := f.storage[0]
	if s.Header.Get("Authorization") != "" || s.URL.Query().Get("sig") != "s1gned" {
		t.Errorf("storage request = %+v", s)
	}
}

// Why this test exists: GitHub keeps a file uploaded with archive: false
// as it is, under its own name, so .zip on it would misname it. A name
// that ends .zip must not get it twice, and a second download of the
// same artifact replaces the file rather than piling up copies.
func TestDownloadNamesWhatArrived(t *testing.T) {
	f := newFakeFiles(t)
	cfg := downloadCfg(t)

	f.body = "<html>"
	req := downloadJob(f, "report.html")
	req.NameZip = true
	if res := runDownload(context.Background(), cfg, req); res.Zip || res.Path != req.Folder+"/report.html" {
		t.Errorf("plain file = %+v", res)
	}
	f.body = "PK\x05\x06"
	req = downloadJob(f, "build.ZIP")
	req.NameZip = true
	for range 2 {
		if res := runDownload(context.Background(), cfg, req); !res.Zip || res.Path != req.Folder+"/build.ZIP" {
			t.Errorf("zip = %+v", res)
		}
	}
	if got := folderFiles(t, cfg, req); len(got) != 2 {
		t.Errorf("folder holds %v", got)
	}
}

// Why this test exists: a redirect's address comes from the far side of
// the network, and the daemon is the last check before bytes land in an
// agent's files. It must go only where the job says, over https, and
// the signed address must never reach the agent: only a host is named.
// A redirect to the same answer, a second hop, must not be followed.
func TestDownloadHoldsTheRedirectToTheJobsHosts(t *testing.T) {
	f := newFakeFiles(t)
	cfg := downloadCfg(t)
	strict := cfg
	strict.GitInsecureHTTP = false
	base := first(t, f.URL+"/api/file")
	for loc, ok := range map[string]bool{
		"https://productionresultssa18.blob.core.windows.net/x?sig=1":   true,
		"https://actions.githubusercontent.com/x":                       true,
		"http://productionresultssa18.blob.core.windows.net/x":          false,
		"https://evil.blob.core.windows.net/x":                          false,
		"https://productionresultssa1.blob.core.windows.net.evil.com/x": false,
		"https://user@productionresultssa1.blob.core.windows.net/x":     false,
		"": false,
	} {
		req := DownloadRequest{Hosts: []string{"productionresultssa*.blob.core.windows.net", "actions.githubusercontent.com"}}
		if _, err := downloadRedirect(strict, req, base, loc); (err == nil) != ok {
			t.Errorf("%q: %v", loc, err)
		}
	}

	f.api = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://files.example.com/x?sig=s3cret", http.StatusFound)
	}
	req := downloadJob(f, "x")
	res := runDownload(context.Background(), cfg, req)
	if !strings.Contains(res.Error, "files.example.com, which is not a host") || strings.Contains(res.Error, "s3cret") {
		t.Errorf("elsewhere = %+v", res)
	}
	if len(f.storage) != 0 || folderFiles(t, cfg, req) != nil {
		t.Errorf("went on: %d requests, files %v", len(f.storage), folderFiles(t, cfg, req))
	}
}

func first(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := downloadURL(ServerConfig{GitInsecureHTTP: true}, raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Why this test exists: a file that only partly arrived must not look
// like the artifact. Whatever stops it, nothing may be left under its
// name or as a leftover in the folder: a body cut short, one over the
// job's bound, and a call whose time runs out mid-body.
func TestDownloadThatFailsLeavesNoFile(t *testing.T) {
	f := newFakeFiles(t)
	cfg := downloadCfg(t)

	// One over the bound.
	f.body = "12345"
	req := downloadJob(f, "x")
	req.MaxBytes = 4
	if res := runDownload(context.Background(), cfg, req); !res.TooLarge || res.Error == "" {
		t.Errorf("over the bound = %+v", res)
	}
	if got := folderFiles(t, cfg, req); len(got) != 0 {
		t.Errorf("left %v", got)
	}

	// Storage promises more than it sends.
	f.api = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(100))
		_, _ = w.Write([]byte("PK\x03\x04"))
	}
	req = downloadJob(f, "x")
	if res := runDownload(context.Background(), cfg, req); res.Error != "it stopped arriving part way" {
		t.Errorf("cut short = %+v", res)
	}
	if got := folderFiles(t, cfg, req); len(got) != 0 {
		t.Errorf("left %v", got)
	}

	// The time runs out with part of the body written.
	ctx, end := context.WithCancel(context.Background())
	defer end()
	release := make(chan struct{})
	defer close(release)
	f.api = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		end()
		<-release
	}
	req = downloadJob(f, "x")
	if res := runDownload(ctx, cfg, req); res.Error == "" {
		t.Errorf("cancelled = %+v", res)
	}
	if got := folderFiles(t, cfg, req); len(got) != 0 {
		t.Errorf("left %v", got)
	}
}

// Why this test exists: core and the agent must be able to tell "GitHub
// has no such file" from a failure worth retrying, so the API's status
// comes back; and GitHub's words, scrubbed of the job's token, say why.
// A job past its bound is told apart too: retrying one cannot help.
func TestDownloadReportsWhyNothingArrived(t *testing.T) {
	f := newFakeFiles(t)
	cfg := downloadCfg(t)
	f.api = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"message":"Artifact has expired, ghs_jobtoken"}`))
	}
	req := downloadJob(f, "x")
	res := runDownload(context.Background(), cfg, req)
	if res.Status != http.StatusGone || !strings.Contains(res.Error, "answered 410") {
		t.Errorf("gone = %+v", res)
	}

	past, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	if res := runDownload(past, cfg, downloadJob(f, "x")); !res.TimedOut {
		t.Errorf("past its bound = %+v", res)
	}
}

// Why this test exists: what a job's URL answers can quote what it was
// sent, and the daemon's error goes to the MCP server, core's answer and
// the audit, none of which knows the job's token. It must leave the
// daemon's socket without the token or the proxy credential, whatever
// the service wrote.
func TestDownloadErrorLeavesTheSocketWithoutCredentials(t *testing.T) {
	f := newFakeFiles(t)
	f.api = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"refused ghs_jobtoken and pr0xysecret"}`))
	}
	d := newGitDaemon(t, "")
	req := downloadJob(f, "x")
	req.Proxy = GitProxy{Username: "scoped", Password: "pr0xysecret"}
	res, err := d.shell.Download(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != http.StatusForbidden || !strings.Contains(res.Error, "answered 403: refused") ||
		strings.Contains(res.Error, "ghs_jobtoken") || strings.Contains(res.Error, "pr0xysecret") {
		t.Errorf("error = %+v", res)
	}
}

// Why this test exists: an agent can write under /files/, so a link it
// left must not carry the download out of the tree, and a file name must
// never be a path. Both are refused with the network untouched, and so
// is a shell whose daemon was started without a proxy.
func TestDownloadRefusesAPathOutOfItsFiles(t *testing.T) {
	f := newFakeFiles(t)
	cfg := downloadCfg(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cfg.Root, "out")); err != nil {
		t.Fatal(err)
	}
	req := downloadJob(f, "x")
	req.Folder = "out/folder"
	if res := runDownload(context.Background(), cfg, req); !strings.Contains(res.Error, "leads out of") {
		t.Errorf("link = %+v", res)
	}
	for _, name := range []string{"", "../x", "a/b", ".", "..", downloadTempPrefix + "x"} {
		if res := runDownload(context.Background(), cfg, downloadJob(f, name)); res.Error != "the job's file name is not a plain name" {
			t.Errorf("name %q = %+v", name, res)
		}
	}
	off := cfg
	off.GitUnavailable = "not here"
	if res := runDownload(context.Background(), off, downloadJob(f, "x")); res.Error != "not here" {
		t.Errorf("unavailable = %+v", res)
	}
	if len(f.apis) != 0 {
		t.Errorf("reached the network %d times", len(f.apis))
	}
}

// Why this test exists: the job's proxy credential is what the egress
// proxy admits it by. Every request of the job, both hops, must go
// through the daemon's proxy with that credential, and the request and
// the answer must cross the daemon's socket whole.
func TestDownloadGoesThroughTheProxyOverTheSocket(t *testing.T) {
	f := newFakeFiles(t)
	f.body = "bytes"
	proxy := newFakeProxy(t)
	d := newGitDaemon(t, proxy.URL)
	req := downloadJob(f, "x")
	req.Proxy = GitProxy{Username: "scoped", Password: "pr0xysecret"}

	res, err := d.shell.Download(context.Background(), req)
	if err != nil || res.Error != "" || res.Bytes != 5 {
		t.Fatalf("download = %+v, %v", res, err)
	}
	if got, err := os.ReadFile(filepath.Join(d.root, res.Path)); err != nil || string(got) != "bytes" {
		t.Errorf("file = %q, %v", got, err)
	}
	if len(proxy.auths) != 2 || proxy.auths[0] == "" || proxy.auths[1] == "" {
		t.Errorf("proxy saw %v", proxy.auths)
	}
}
