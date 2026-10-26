package agentshell

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
)

const (
	gitTestToken       = "ghp_t0ken-for-the-git-tests-0123"
	gitTestProxyUser   = "integration-git.1a2b3c4d"
	gitTestProxySecret = "cHJveHktc2VjcmV0LWZvci10aGUtZ2l0LXRlc3Rz"
)

// gitCLI is the git binary the test servers and fixtures run; a test
// that needs it is skipped where there is none (CI's ubuntu and a
// developer's Mac have it).
func gitCLI(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	return p
}

// gitCommand is git with args in dir, with no system or global
// configuration.
func gitCommand(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(gitCLI(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	return cmd
}

// runGitCLI runs git in dir with no system or global configuration.
func runGitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCommand(t, dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// runGitCLIInput is runGitCLI with input on git's standard input.
func runGitCLIInput(t *testing.T, dir, input string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitCLI(t), args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// testGitServer is a smart-HTTP git server: git http-backend under
// net/http/cgi, behind a check of the basic auth the Git (Basic)
// integration sends (x-access-token and the token). Its repositories are bare
// ones under root; requests counts what reached it, and served the
// requests per repository path prefix the backend served.
type testGitServer struct {
	*httptest.Server
	root     string
	requests atomic.Int32
	// block, when set, holds every request until its context ends,
	// after closing entered once and sending on arrivals.
	block    atomic.Bool
	entered  chan struct{}
	arrivals chan struct{}
	once     sync.Once
	// redirects maps a repository's path ("/acme/old.git") to the one
	// its requests are sent to with a 301; set before any request.
	redirects map[string]string
	// pad lengthens the words of a refusal by that many bytes.
	pad atomic.Int32
	// receivePacks counts the requests of a push's receive-pack (its
	// advertisement and the push itself).
	receivePacks atomic.Int32

	mu     sync.Mutex
	served map[string]int
}

func newTestGitServer(t *testing.T) *testGitServer {
	t.Helper()
	s := &testGitServer{root: t.TempDir(), entered: make(chan struct{}), arrivals: make(chan struct{}, 16), served: map[string]int{}}
	backend := cgi.Handler{
		Path: gitCLI(t),
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + s.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"},
		// The host's hooks write to stderr; the test reads the push's
		// answer, not the log.
		Stderr: io.Discard,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack" {
			s.receivePacks.Add(1)
		}
		if s.block.Load() {
			s.once.Do(func() { close(s.entered) })
			s.arrivals <- struct{}{}
			<-r.Context().Done()
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass != gitTestToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			// The words quote what was sent, as a careless host's might.
			http.Error(w, "bad credentials "+pass+" in "+r.Header.Get("Authorization")+strings.Repeat("x", int(s.pad.Load())), http.StatusUnauthorized)
			return
		}
		for from, to := range s.redirects {
			if rest, ok := strings.CutPrefix(r.URL.Path, from); ok {
				http.Redirect(w, r, to+rest+"?"+r.URL.RawQuery, http.StatusMovedPermanently)
				return
			}
		}
		if i := strings.Index(r.URL.Path, ".git/"); i >= 0 {
			s.mu.Lock()
			s.served[r.URL.Path[:i+4]]++
			s.mu.Unlock()
		}
		h := backend
		h.Env = append(append([]string(nil), backend.Env...), "REMOTE_USER="+user)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// servedOf is how many requests the backend served for repo's path
// ("/acme/widgets.git").
func (s *testGitServer) servedOf(repo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served[repo]
}

// host is the server's host:port, as a connection's host.
func (s *testGitServer) host() string {
	u, _ := url.Parse(s.URL)
	return u.Host
}

// seed makes repo (owner/name) on the server with one commit on main
// and a tag v1, and returns the working tree it was made in.
func (s *testGitServer) seed(t *testing.T, repo, content string) string {
	t.Helper()
	work := t.TempDir()
	runGitCLI(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, work, "add", "README")
	runGitCLI(t, work, "commit", "-q", "-m", "one")
	runGitCLI(t, work, "tag", "v1")
	bare := filepath.Join(s.root, repo+".git")
	runGitCLI(t, "", "init", "-q", "--bare", "-b", "main", bare)
	runGitCLI(t, work, "push", "-q", bare, "main", "--tags")
	return work
}

// advance commits in work and pushes it to repo, returning the commit.
func (s *testGitServer) advance(t *testing.T, work, repo string) string {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(work, "README"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\nmore")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	runGitCLI(t, work, "commit", "-q", "-am", "more")
	runGitCLI(t, work, "push", "-q", filepath.Join(s.root, repo+".git"), "main")
	return runGitCLI(t, work, "rev-parse", "HEAD")
}

// fakeProxy is a forward proxy for plain-HTTP requests with absolute
// URLs. It records each request's Proxy-Authorization and host, and
// passes the request on directly; with refuse set it answers 401 with
// words that quote the credential it was given.
type fakeProxy struct {
	*httptest.Server
	mu     sync.Mutex
	auths  []string
	hosts  []string
	refuse atomic.Bool
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	p := &fakeProxy{}
	direct := &http.Transport{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		auth := r.Header.Get("Proxy-Authorization")
		p.mu.Lock()
		p.auths = append(p.auths, auth)
		p.hosts = append(p.hosts, r.URL.Host)
		p.mu.Unlock()
		if p.refuse.Load() {
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
			_, secret, _ := strings.Cut(string(raw), ":")
			http.Error(w, "proxy refused "+auth+" with secret "+secret, http.StatusUnauthorized)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		resp, err := direct.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.Close)
	t.Cleanup(direct.CloseIdleConnections)
	return p
}

func (p *fakeProxy) seen() (auths, hosts []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.auths...), append([]string(nil), p.hosts...)
}

// gitDaemon is the daemon as an agent pod runs it, on a socket, with
// a root and a home of its own, http:// allowed, on clk.
type gitDaemon struct {
	root, home string
	clk        *clock.Fake
	shell      *SidecarShell
}

func newGitDaemon(t *testing.T, proxyURL string, edits ...func(*ServerConfig)) *gitDaemon {
	t.Helper()
	gitCLI(t)
	// go-git reads a global configuration from HOME: the test's own,
	// never the developer's.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	d := &gitDaemon{root: t.TempDir(), home: t.TempDir(), clk: clock.NewFake()}
	sock := shortSock(t)
	cfg := ServerConfig{
		Root:            d.root,
		Home:            d.home,
		GitProxyURL:     proxyURL,
		GitInsecureHTTP: true,
		Clock:           d.clk,
	}
	for _, e := range edits {
		e(&cfg)
	}
	stop := startTestDaemon(t, sock, cfg)
	t.Cleanup(stop)
	d.shell = &SidecarShell{SocketPath: sock, Slug: "dana"}
	return d
}

func (d *gitDaemon) run(t *testing.T, req GitRequest) *GitResponse {
	t.Helper()
	res, err := d.shell.Git(context.Background(), req)
	if err != nil {
		t.Fatalf("Git: %v", err)
	}
	return res
}

// cloneJob is a clone of acme/widgets from srv into path.
func cloneJob(srv *testGitServer, path string) GitRequest {
	return GitRequest{
		Op:                  GitOpClone,
		Path:                path,
		URL:                 srv.URL + "/acme/widgets.git",
		Host:                srv.host(),
		AllowedRepositories: []string{"acme/*"},
		Auth:                GitAuth{Username: "x-access-token", Password: gitTestToken},
		Proxy:               GitProxy{Username: gitTestProxyUser, Password: gitTestProxySecret},
		TimeoutSeconds:      60,
	}
}

func hasRef(updated []GitRefUpdate, ref string) bool {
	for _, u := range updated {
		if u.Ref == ref {
			return true
		}
	}
	return false
}

// Why this test exists: a clone is the one place the owner's token
// leaves Kivali for the agent's shell. It must go through the proxy
// the daemon's environment names with the connection's credential on
// it (else the scoped proxy refuses the host, or the token rides an
// agent's credential), land as an ordinary working copy whose origin
// is the URL without user info (so plain git keeps working), and leave
// the token nowhere on disk.
func TestGitCloneThroughTheProxy(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	proxy := newFakeProxy(t)
	d := newGitDaemon(t, proxy.URL)

	res := d.run(t, cloneJob(srv, "work/widgets"))
	if res.Error != "" || res.Repository != "acme/widgets" || res.TimedOut {
		t.Fatalf("clone = %+v", res)
	}
	for _, ref := range []string{"refs/heads/main", "refs/remotes/origin/main", "refs/tags/v1"} {
		if !hasRef(res.Updated, ref) {
			t.Errorf("updated %+v lacks %s", res.Updated, ref)
		}
	}
	wc := filepath.Join(d.root, "work", "widgets")
	if b, err := os.ReadFile(filepath.Join(wc, "README")); err != nil || string(b) != "one" {
		t.Fatalf("README = %q, %v", b, err)
	}
	cfg, err := os.ReadFile(filepath.Join(wc, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), gitTestToken) || strings.Contains(string(cfg), gitTestProxySecret) || strings.Contains(string(cfg), "x-access-token") {
		t.Fatalf(".git/config holds a credential:\n%s", cfg)
	}
	if got := runGitCLI(t, wc, "config", "--get", "remote.origin.url"); got != srv.URL+"/acme/widgets.git" {
		t.Errorf("origin = %q", got)
	}
	auths, hosts := proxy.seen()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(gitTestProxyUser+":"+gitTestProxySecret))
	if len(auths) == 0 {
		t.Fatal("nothing went through the proxy")
	}
	for i := range auths {
		if auths[i] != want || hosts[i] != srv.host() {
			t.Errorf("proxy request %d: auth %q host %q", i, auths[i], hosts[i])
		}
	}

	// Into the home, by ~/.
	if res := d.run(t, cloneJob(srv, "~/repos/widgets")); res.Error != "" {
		t.Fatalf("clone into the home = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(d.home, "repos", "widgets", ".git")); err != nil {
		t.Fatalf("no working copy in the home: %v", err)
	}
}

// Why this test exists: everything a clone is given comes from a
// model's call. A path outside /files and the home (directly, with
// .., or through a symbolic link), a directory already holding files,
// a URL off the connection's host or with user info, or a repository
// the access does not include must each be refused before any byte
// goes to a git host.
func TestGitCloneRefusals(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(d.root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.root, "full"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.root, "full", "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*GitRequest){
		"absolute elsewhere": func(r *GitRequest) { r.Path = filepath.Join(outside, "w") },
		"climbs out":         func(r *GitRequest) { r.Path = "../w" },
		"home climbs out":    func(r *GitRequest) { r.Path = "~/../w" },
		"the root itself":    func(r *GitRequest) { r.Path = d.root },
		"no path":            func(r *GitRequest) { r.Path = " " },
		"through a link":     func(r *GitRequest) { r.Path = "link/w" },
		"not empty":          func(r *GitRequest) { r.Path = "full" },
		"another host":       func(r *GitRequest) { r.URL = "http://example.com/acme/widgets.git" },
		"user info": func(r *GitRequest) {
			r.URL = strings.Replace(srv.URL, "http://", "http://x:y@", 1) + "/acme/widgets.git"
		},
		"outside the access": func(r *GitRequest) { r.AllowedRepositories = []string{"other/*"} },
		"no access":          func(r *GitRequest) { r.AllowedRepositories = nil },
		"no URL":             func(r *GitRequest) { r.URL = "" },
		"not an operation":   func(r *GitRequest) { r.Op = "pull" },
	} {
		req := cloneJob(srv, "w")
		edit(&req)
		res := d.run(t, req)
		if res.Error == "" {
			t.Errorf("%s: accepted: %+v", name, res)
		}
		if strings.Contains(res.Error, gitTestToken) {
			t.Errorf("%s: the error quotes the token: %q", name, res.Error)
		}
	}
	if n := srv.requests.Load(); n != 0 {
		t.Errorf("%d requests reached the git host", n)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was written outside: %v", entries)
	}
}

// Why this test exists: a clone through GitHub carries the app's bot
// as its identity, so the commits an agent makes there are authored and
// committed as the bot, which is what lets the agent delete such a
// branch later. The daemon must write it into the new working copy's
// .git/config as user.name and user.email, as git reads them, and say
// it did (core tells the agent when its commits will not read as the
// bot's); a clone without one (Git (Basic)'s) must get no user section
// at all and say none was written, and an
// identity git would not take (a line break could add config of its
// own) must be refused before any byte goes to the git host.
func TestGitCloneWritesTheIdentity(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	id := &GitIdentity{Name: "kivali-acme[bot]", Email: "99+kivali-acme[bot]@users.noreply.github.com"}
	req := cloneJob(srv, "with")
	req.Identity = id
	if res := d.run(t, req); res.Error != "" || !res.Identity {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "with")
	if got := runGitCLI(t, wc, "config", "--get", "user.name"); got != id.Name {
		t.Errorf("user.name = %q", got)
	}
	if got := runGitCLI(t, wc, "config", "--get", "user.email"); got != id.Email {
		t.Errorf("user.email = %q", got)
	}
	if got := runGitCLI(t, wc, "config", "--get", "remote.origin.url"); got != srv.URL+"/acme/widgets.git" {
		t.Errorf("origin = %q", got)
	}

	if res := d.run(t, cloneJob(srv, "without")); res.Error != "" || res.Identity {
		t.Fatalf("clone = %+v", res)
	}
	cfg, err := os.ReadFile(filepath.Join(d.root, "without", ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "[user]") {
		t.Errorf("a clone without an identity has one:\n%s", cfg)
	}

	before := srv.requests.Load()
	for _, bad := range []GitIdentity{
		{Name: "bot\n[core]\n\thooksPath = /tmp", Email: id.Email},
		{Name: id.Name, Email: "x@example.com>"},
		{Name: "", Email: id.Email},
		{Name: id.Name, Email: strings.Repeat("e", GitIdentityMaxBytes+1)},
	} {
		req := cloneJob(srv, "bad")
		req.Identity = &bad
		if res := d.run(t, req); res.Error != "the job's git identity is not one git takes" {
			t.Errorf("%q: %+v", bad, res)
		}
	}
	if n := srv.requests.Load() - before; n != 0 {
		t.Errorf("%d requests reached the git host for a refused identity", n)
	}
}

// Why this test exists: a working copy is the agent's to write, so
// its .git/config can send git anywhere: an insteadOf rule to another
// server, an origin that names it, a global ~/.gitconfig doing the
// same, hooks that run on checkout or push. A fetch with the job's URL
// must reach that URL alone and run nothing; a fetch without one must
// read origin as written (never through insteadOf) and refuse it when
// it is off the connection's host or outside the access. If any of
// this failed, the owner's token would go to the agent's server.
func TestGitFetchReachesOnlyTheCheckedURL(t *testing.T) {
	good := newTestGitServer(t)
	work := good.seed(t, "acme/widgets", "one")
	evil := newTestGitServer(t)
	evil.seed(t, "acme/widgets", "evil")
	d := newGitDaemon(t, "")

	if res := d.run(t, cloneJob(good, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	wc := filepath.Join(d.root, "wc")
	first := runGitCLI(t, wc, "rev-parse", "refs/remotes/origin/main")
	second := good.advance(t, work, "acme/widgets")

	// The poison: a rule and an origin to the evil server, the same
	// rule in the global configuration, and hooks that leave a mark.
	runGitCLI(t, wc, "config", "url."+evil.URL+"/.insteadOf", good.URL+"/")
	runGitCLI(t, wc, "config", "remote.origin.url", evil.URL+"/acme/widgets.git")
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".gitconfig"),
		[]byte("[url \""+evil.URL+"/\"]\n\tinsteadOf = "+good.URL+"/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(t.TempDir(), "hook-ran")
	if err := os.MkdirAll(filepath.Join(wc, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"pre-push", "post-checkout", "post-merge", "reference-transaction", "pre-auto-gc"} {
		if err := os.WriteFile(filepath.Join(wc, ".git", "hooks", hook), []byte("#!/bin/sh\ntouch "+mark+"\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fetch := GitRequest{
		Op: GitOpFetch, Path: "wc", URL: good.URL + "/acme/widgets.git", Host: good.host(),
		AllowedRepositories: []string{"acme/widgets"},
		Auth:                GitAuth{Username: "x-access-token", Password: gitTestToken},
		TimeoutSeconds:      60,
	}
	evilBefore := evil.requests.Load()

	res := d.run(t, fetch)
	if res.Error != "" || res.Repository != "acme/widgets" {
		t.Fatalf("fetch with the job's URL = %+v", res)
	}
	if got := runGitCLI(t, wc, "rev-parse", "refs/remotes/origin/main"); got != second {
		t.Errorf("origin/main = %s, want %s", got, second)
	}
	if len(res.Updated) != 1 || res.Updated[0] != (GitRefUpdate{Ref: "refs/remotes/origin/main", Old: first, New: second}) {
		t.Errorf("updated = %+v", res.Updated)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a hook ran")
	}
	if n := evil.requests.Load(); n != evilBefore {
		t.Fatalf("%d requests reached the evil server", n-evilBefore)
	}
	// Up to date: success with nothing moved.
	if res := d.run(t, fetch); res.Error != "" || len(res.Updated) != 0 {
		t.Errorf("fetch again = %+v", res)
	}
	// A clone, with the global rule in place, goes to the job's URL too.
	if res := d.run(t, cloneJob(good, "wc2")); res.Error != "" {
		t.Errorf("clone under the global rule = %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(d.root, "wc2", "README")); strings.Contains(string(b), "evil") {
		t.Error("the clone came from the evil server")
	}
	if n := evil.requests.Load(); n != evilBefore {
		t.Fatalf("%d requests of the clone reached the evil server", n-evilBefore)
	}

	// Without a URL: origin names the evil server, off the host.
	fetch.URL = ""
	goodBefore := good.requests.Load()
	if res := d.run(t, fetch); !strings.Contains(res.Error, "origin") {
		t.Errorf("an origin off the host = %+v", res)
	}
	// On the host, but a repository the access does not include.
	runGitCLI(t, wc, "config", "remote.origin.url", good.URL+"/other/thing.git")
	if res := d.run(t, fetch); !strings.Contains(res.Error, "other/thing") {
		t.Errorf("an origin outside the access = %+v", res)
	}
	if evil.requests.Load() != evilBefore || good.requests.Load() != goodBefore {
		t.Fatal("a refused fetch reached a server")
	}
	// Origin as written is the good server; the insteadOf rules in the
	// repository's and the global configuration would rewrite it to the
	// evil one, and must not.
	runGitCLI(t, wc, "config", "remote.origin.url", good.URL+"/acme/widgets.git")
	third := good.advance(t, work, "acme/widgets")
	res = d.run(t, fetch)
	if res.Error != "" || res.Repository != "acme/widgets" {
		t.Fatalf("fetch from origin = %+v", res)
	}
	if got := runGitCLI(t, wc, "rev-parse", "refs/remotes/origin/main"); got != third {
		t.Errorf("origin/main = %s, want %s", got, third)
	}
	if evil.requests.Load() != evilBefore || good.requests.Load() == goodBefore {
		t.Errorf("evil %d, good %d requests", evil.requests.Load()-evilBefore, good.requests.Load()-goodBefore)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a hook ran")
	}

	// The controls, which pin the go-git behaviour the daemon relies on.
	// Origin is the good server and only the repository's insteadOf rule
	// points at the evil one: a fetch through the named remote follows
	// the rule there, while the anonymous remote's fetch just above did
	// not.
	r, err := git.PlainOpen(wc)
	if err != nil {
		t.Fatal(err)
	}
	named := &git.FetchOptions{RemoteName: "origin", Auth: &githttp.BasicAuth{Username: "x-access-token", Password: gitTestToken}}
	_ = r.Fetch(named)
	if evil.requests.Load() == evilBefore {
		t.Error("the named remote did not follow the insteadOf rule to the evil server: the rule proves nothing")
	}
	// Origin itself names the evil server: the named remote follows it.
	evilBefore = evil.requests.Load()
	runGitCLI(t, wc, "config", "--unset", "url."+evil.URL+"/.insteadOf")
	runGitCLI(t, wc, "config", "remote.origin.url", evil.URL+"/acme/widgets.git")
	if r, err = git.PlainOpen(wc); err != nil {
		t.Fatal(err)
	}
	_ = r.Fetch(named)
	if evil.requests.Load() == evilBefore {
		t.Error("the named remote did not follow origin to the evil server: the poisoned origin proves nothing")
	}

	// A working copy whose .git is not a directory is refused.
	if err := os.MkdirAll(filepath.Join(d.root, "pointer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.root, "pointer", ".git"), []byte("gitdir: "+filepath.Join(wc, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetch.Path = "pointer"
	if res := d.run(t, fetch); !strings.Contains(res.Error, "not a working copy") {
		t.Errorf("a .git file = %+v", res)
	}
}

// Why this test exists: a refspec names what a fetch may write. One
// whose destination is a local branch (refs/heads/...), outside
// origin's remote-tracking refs and the tags, or that climbs with ..,
// would let a fetch move the agent's own branches to whatever the host
// serves; each must be refused before the fetch, and the ones under
// refs/remotes/origin/ and refs/tags/ must work.
func TestGitFetchRefSpecs(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	d := newGitDaemon(t, "")
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	fetch := func(specs ...string) *GitResponse {
		return d.run(t, GitRequest{
			Op: GitOpFetch, Path: "wc", URL: srv.URL + "/acme/widgets.git", Host: srv.host(),
			AllowedRepositories: []string{"acme/widgets"}, RefSpecs: specs,
			Auth: GitAuth{Username: "x-access-token", Password: gitTestToken}, TimeoutSeconds: 60,
		})
	}
	before := srv.requests.Load()
	for _, bad := range []string{
		"+refs/heads/*:refs/heads/*",
		"refs/heads/main:refs/heads/main",
		"refs/heads/main",
		":refs/remotes/origin/main",
		"^refs/heads/main",
		"refs/heads/main:refs/remotes/origin/../../heads/main",
		"refs/heads/main:refs/remotes/origin/",
		"refs/heads/main:refs/notes/x",
		"refs/heads/*:refs/remotes/origin/main",
	} {
		if res := fetch(bad); !strings.Contains(res.Error, bad) {
			t.Errorf("%q = %+v", bad, res)
		}
	}
	if srv.requests.Load() != before {
		t.Fatal("a refused refspec reached the host")
	}
	for _, good := range []string{"refs/heads/main:refs/remotes/origin/main", "+refs/heads/*:refs/remotes/origin/*", "refs/tags/v1:refs/tags/v1"} {
		if res := fetch(good); res.Error != "" {
			t.Errorf("%q = %+v", good, res)
		}
	}
}

// Why this test exists: a host that never answers would hold the
// agent's call, and its MCP server's, forever. The operation's bound
// must end it on the daemon's clock, answer with TimedOut, and leave no
// half-made working copy behind.
func TestGitTimeout(t *testing.T) {
	srv := newTestGitServer(t)
	srv.block.Store(true)
	d := newGitDaemon(t, "")
	done := make(chan *GitResponse, 1)
	go func() {
		res, err := d.shell.Git(context.Background(), cloneJob(srv, "slow"))
		if err != nil {
			t.Errorf("Git: %v", err)
		}
		done <- res
	}()
	<-srv.entered
	d.clk.Advance(GitTimeoutFor(GitOpClone, 60))
	res := <-done
	if res == nil || !res.TimedOut || res.Error != "did not finish within 1m0s" || res.DurationMs != 60000 {
		t.Fatalf("timed out = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(d.root, "slow")); !os.IsNotExist(err) {
		t.Errorf("the clone's directory is left: %v", err)
	}
	if GitTimeoutFor(GitOpClone, 0) != GitDefaultTimeout || GitTimeoutFor(GitOpClone, 99999) != GitCloneMaxTimeout ||
		GitTimeoutFor(GitOpFetch, 99999) != GitFetchMaxTimeout {
		t.Error("GitTimeoutFor's bounds")
	}
}

// Why this test exists: a daemon that did not drop to its own user, or
// whose socket a command could stand in for, would hand a git job's
// token to the agent's commands. Such a daemon must run nothing for
// /v1/git and say why, before any byte goes to the host.
func TestGitUnavailable(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	cfg := ServerConfig{Root: t.TempDir(), GitInsecureHTTP: true, GitUnavailable: "not here", Clock: clock.NewFake()}
	if res := runGit(context.Background(), cfg, &gitPathLocks{held: map[string]*gitPathLock{}}, cloneJob(srv, "w")); res.Error != "not here" {
		t.Errorf("clone = %+v", res)
	}
	if n := srv.requests.Load(); n != 0 {
		t.Errorf("%d requests reached the host", n)
	}
}

// Why this test exists: the call's second request tells core which
// connection the job ran through, from the daemon's answer. A
// connection id the daemon dropped, on the wire either way, would have
// core scrub and audit a job moved to another connection as if it ran
// through the one the access names now.
func TestGitEchoesTheConnection(t *testing.T) {
	srv := newTestGitServer(t)
	cfg := ServerConfig{Root: t.TempDir(), GitInsecureHTTP: true, GitUnavailable: "not here", Clock: clock.NewFake()}
	job := cloneJob(srv, "w")
	job.ConnectionID = "1a2b3c4d"
	body, _ := json.Marshal(job)
	rr := httptest.NewRecorder()
	handleGit(cfg)(rr, httptest.NewRequest(http.MethodPost, "/v1/git", bytes.NewReader(body)))
	var res GitResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil || rr.Code != http.StatusOK || res.ConnectionID != "1a2b3c4d" || res.Error != "not here" {
		t.Errorf("answer = %d %s (%v)", rr.Code, rr.Body.String(), err)
	}
}

// Why this test exists: a git host's or a proxy's error words come
// back to the agent, and may quote what they were sent. The token and
// the proxy secret, raw, in basic auth's base64 or in the proxy URL,
// must never be in the daemon's answer; the token reads [token] and
// the proxy credential [proxy credential], so the agent can tell which
// was refused. Words longer than the answer's bound are cut, after the
// scrubbing, so a cut never leaves part of a secret.
func TestGitScrubsCredentials(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	proxy := newFakeProxy(t)
	d := newGitDaemon(t, proxy.URL)
	leaks := []string{
		gitTestToken, gitTestProxySecret,
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + gitTestToken + "-wrong")),
		base64.StdEncoding.EncodeToString([]byte(gitTestProxyUser + ":" + gitTestProxySecret)),
	}

	// The host refuses the token and quotes it.
	req := cloneJob(srv, "a")
	req.Auth.Password = gitTestToken + "-wrong"
	res := d.run(t, req)
	if !strings.Contains(res.Error, "bad credentials") || !strings.Contains(res.Error, GitTokenPlaceholder) {
		t.Fatalf("host refusal = %q", res.Error)
	}
	// Long words are cut to the bound.
	srv.pad.Store(10 << 10)
	long := d.run(t, req)
	if len(long.Error) != GitErrorMaxBytes || !strings.Contains(long.Error, GitTokenPlaceholder) {
		t.Errorf("a long refusal is %d bytes: %.80q", len(long.Error), long.Error)
	}
	srv.pad.Store(0)
	// The proxy refuses its credential and quotes it.
	proxy.refuse.Store(true)
	res2 := d.run(t, cloneJob(srv, "b"))
	if !strings.Contains(res2.Error, "proxy refused") || !strings.Contains(res2.Error, egresscred.Placeholder) {
		t.Fatalf("proxy refusal = %q", res2.Error)
	}
	for _, text := range []string{res.Error, res2.Error, long.Error} {
		for _, leak := range leaks {
			if strings.Contains(text, leak) {
				t.Errorf("%q holds %q", text, leak)
			}
		}
	}
}

// Why this test exists: GitHub answers a renamed or transferred
// repository with a 301 to its new path. Followed with the token, the
// repository served would not be the one core checked against the
// agent's access. go-git must not follow it: the clone and the fetch
// fail with words the agent can act on, and nothing reaches the
// repository the redirect points at.
func TestGitRefusesRedirects(t *testing.T) {
	srv := newTestGitServer(t)
	srv.seed(t, "acme/widgets", "one")
	srv.redirects = map[string]string{"/acme/old.git": "/acme/widgets.git"}
	d := newGitDaemon(t, "")

	moved := cloneJob(srv, "w")
	moved.URL = srv.URL + "/acme/old.git"
	if res := d.run(t, moved); res.Error != gitMovedError {
		t.Errorf("clone = %+v", res)
	}
	if res := d.run(t, cloneJob(srv, "wc")); res.Error != "" {
		t.Fatalf("clone = %+v", res)
	}
	before := srv.servedOf("/acme/widgets.git")
	fetch := GitRequest{
		Op: GitOpFetch, Path: "wc", URL: srv.URL + "/acme/old.git", Host: srv.host(),
		AllowedRepositories: []string{"acme/*"}, Auth: GitAuth{Username: "x-access-token", Password: gitTestToken}, TimeoutSeconds: 60,
	}
	if res := d.run(t, fetch); res.Error != gitMovedError {
		t.Errorf("fetch = %+v", res)
	}
	if n := srv.servedOf("/acme/widgets.git") - before; n != 0 {
		t.Errorf("%d requests reached the repository the redirect named", n)
	}
	if _, err := os.Stat(filepath.Join(d.root, "w")); !os.IsNotExist(err) {
		t.Errorf("the refused clone left its directory: %v", err)
	}
}

// Why this test exists: a clone of a repository with very many refs
// would answer with all of them, and the agent's MCP server sends the
// answer on to core, whose request body is bounded: past it the call
// "did not reach Kivali" and the clone that used the token has no
// closing audit row. The answer must list at most GitUpdatedMax refs,
// in order, and count them all.
func TestGitBoundsTheRefs(t *testing.T) {
	srv := newTestGitServer(t)
	work := srv.seed(t, "acme/widgets", "one")
	var refs strings.Builder
	for i := range 250 {
		fmt.Fprintf(&refs, "create refs/tags/t%03d HEAD\n", i)
	}
	runGitCLIInput(t, work, refs.String(), "update-ref", "--stdin")
	runGitCLI(t, work, "push", "-q", filepath.Join(srv.root, "acme/widgets.git"), "--tags")
	d := newGitDaemon(t, "")
	res := d.run(t, cloneJob(srv, "wc"))
	// 250 tags, v1, main and origin/main.
	if res.Error != "" || len(res.Updated) != GitUpdatedMax || res.UpdatedTotal != 253 {
		t.Fatalf("clone = %d refs of %d, %q", len(res.Updated), res.UpdatedTotal, res.Error)
	}
	if !sort.SliceIsSorted(res.Updated, func(i, j int) bool { return res.Updated[i].Ref < res.Updated[j].Ref }) {
		t.Error("the refs are not in order")
	}
}

// Why this test exists: two jobs on one working copy at once (two
// fetches into one .git) can leave its refs half written. The second
// must wait until the first has ended, and only then reach the host.
func TestGitSerialisesAWorkingCopy(t *testing.T) {
	srv := newTestGitServer(t)
	srv.block.Store(true)
	waited := make(chan string, 4)
	d := newGitDaemon(t, "", func(c *ServerConfig) { c.gitWaitHook = func(p string) { waited <- p } })
	first, second := make(chan *GitResponse, 1), make(chan *GitResponse, 1)
	go func() {
		res, _ := d.shell.Git(context.Background(), cloneJob(srv, "same"))
		first <- res
	}()
	<-srv.arrivals // the first holds the path, at the host
	long := cloneJob(srv, "same")
	long.TimeoutSeconds = 120
	go func() {
		res, _ := d.shell.Git(context.Background(), long)
		second <- res
	}()
	select {
	case p := <-waited:
		if filepath.Base(p) != "same" {
			t.Errorf("waited on %s", p)
		}
	case <-srv.arrivals:
		t.Fatal("the second job reached the host while the first ran")
	}
	// The first times out; the second goes on to the host and times out
	// in turn.
	d.clk.Advance(GitTimeoutFor(GitOpClone, 60))
	if res := <-first; res == nil || !res.TimedOut {
		t.Fatalf("first = %+v", res)
	}
	<-srv.arrivals
	d.clk.Advance(GitTimeoutFor(GitOpClone, 60))
	if res := <-second; res == nil || !res.TimedOut {
		t.Fatalf("second = %+v", res)
	}
	if n := srv.requests.Load(); n != 2 {
		t.Errorf("%d requests", n)
	}
}
