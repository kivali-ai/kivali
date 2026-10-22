package agentshell

// POST /v1/git: a clone, a fetch or a push through an integration's
// git connection, run in this process with go-git.
//
// Core builds the job (agentpod.GitJob) from the agent's call, after it
// has checked the agent's access; the agent's MCP server hands it here
// as a GitRequest. The daemon runs it with the daemon's filesystem uid,
// so what it writes is owned as every file the shell writes is. The
// token and the proxy credential live in the request for the one
// operation: nothing here logs a request, writes either to disk or
// keeps either after the answer, and every string of the answer is
// scrubbed of both.
//
// The daemon reaches only the URL it derived and checked:
//
//   - A clone's URL comes from the job, and must be an https URL on the
//     job's host, of a repository AllowedRepositories includes. The
//     clone's origin is that URL, with no user info, so plain git keeps
//     working for a repository the team's allow list reaches.
//   - A fetch's or a push's URL comes from the job, by the same rules,
//     or, when the job has none, from the working copy's origin as
//     written in .git/config (raw: never rewritten by a
//     url.<base>.insteadOf), parsed by the same rules, and refused unless
//     it is on the job's host and one of AllowedRepositories. .git/config
//     is the agent's to write, so nothing else in it is read.
//   - A fetch and a push go through an anonymous remote with that URL in
//     code. go-git then applies no insteadOf or pushInsteadOf rule, from
//     the repository's config or a global one, ignores the
//     [remote "origin"] the config holds (its pushurl among it), and
//     runs no hook. A named remote (RemoteName "origin" without
//     RemoteURL) would follow the config's URL: never use one. A push
//     through an anonymous remote needs PushOptions.RemoteName
//     "anonymous".
//   - go-git follows no redirect: the http and https transports it uses
//     are installed with NoFollowRedirects (init below). A host that
//     answers with a redirect (GitHub does for a renamed or transferred
//     repository) fails the operation, so the repository served is the
//     one checked, at the URL checked.
//   - A push writes only the branches and tags of AllowedRefs, forces
//     only a refspec with + and only when the job's Force allows it, and
//     never deletes a remote ref: every refspec is held to that again
//     here (gitrepo.ParsePushRefSpec), as the URL is to
//     AllowedRepositories. Without +, an existing tag never moves and a
//     branch moves only forward, by the daemon's own walk through
//     objects checked against their names (checkPushMoves). The push is
//     atomic, so a host that refuses one ref takes none.
//   - The proxy is always given in code (ProxyOptions), the daemon's own
//     bare proxy URL with the job's credential, so go-git never reads a
//     proxy from the environment. With no proxy in the daemon's
//     environment (tests, a laptop), the operation connects directly.
//
// Jobs on one working copy run one after the other (gitPathLocks): two
// fetches into one .git at once could leave its refs half written. The
// answer is bounded, so it always fits the agent's next request to
// core: at most GitUpdatedMax refs (UpdatedTotal counts them all) and
// GitErrorMaxBytes of error.

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// init installs go-git's http and https transports with redirects
// refused. go-git's transports are process-wide; in core's process
// gitintegration installs the same.
func init() {
	noRedirects := githttp.NewClientWithOptions(nil, &githttp.ClientOptions{RedirectPolicy: githttp.NoFollowRedirects})
	gitclient.InstallProtocol("https", noRedirects)
	gitclient.InstallProtocol("http", noRedirects)
}

// gitMovedError is what the agent reads when the host answered with a
// redirect, which go-git is not let follow.
const gitMovedError = "the repository moved; ask the owner to update the access"

// isGitRedirect reports whether err is go-git refusing a redirect.
func isGitRedirect(err error) bool {
	return err != nil && strings.Contains(err.Error(), "http redirect:")
}

// The bounds of an answer.
const (
	// GitUpdatedMax is the most refs a GitResponse lists.
	GitUpdatedMax = 200
	// GitErrorMaxBytes is the most bytes of a GitResponse's Error.
	GitErrorMaxBytes = 4 << 10
)

// The operations POST /v1/git runs.
const (
	GitOpClone = "clone"
	GitOpFetch = "fetch"
	GitOpPush  = "push"
)

// GitMaxBodyBytes caps a POST /v1/git body.
const GitMaxBodyBytes = 64 << 10

// The bounds of one git operation: a request's TimeoutSeconds, at most
// GitCloneMaxTimeout for a clone, GitFetchMaxTimeout for a fetch and
// GitPushMaxTimeout for a push; GitDefaultTimeout when it gives none.
const (
	GitDefaultTimeout  = 5 * time.Minute
	GitCloneMaxTimeout = 15 * time.Minute
	GitFetchMaxTimeout = 10 * time.Minute
	GitPushMaxTimeout  = 10 * time.Minute
)

// GitTimeoutFor is the bound of op given seconds.
func GitTimeoutFor(op string, seconds int) time.Duration {
	if seconds <= 0 {
		return GitDefaultTimeout
	}
	max := GitFetchMaxTimeout
	switch op {
	case GitOpClone:
		max = GitCloneMaxTimeout
	case GitOpPush:
		max = GitPushMaxTimeout
	}
	return min(time.Duration(seconds)*time.Second, max)
}

// gitDefaultRefSpec is what a fetch with no refspecs fetches: every
// branch, into origin's remote-tracking refs.
const gitDefaultRefSpec = "+refs/heads/*:refs/remotes/origin/*"

// gitAnonymousRemote is the name go-git requires of an anonymous
// remote, and the RemoteName its operations must then be given.
const gitAnonymousRemote = "anonymous"

func handleGit(cfg ServerConfig) http.HandlerFunc {
	locks := &gitPathLocks{held: map[string]*gitPathLock{}}
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, GitMaxBodyBytes)
		var req GitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// A decode error names a field or a character, never a value.
			status := http.StatusBadRequest
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "decode GitRequest: "+err.Error(), status)
			return
		}
		scrub := GitScrubber(req, cfg.GitProxyURL)
		res := runGit(r.Context(), cfg, locks, req)
		res.scrub(scrub)
		res.Error = clipUTF8(res.Error, GitErrorMaxBytes)
		writeJSON(w, http.StatusOK, res)
	}
}

// GitTokenPlaceholder is what a GitScrubber writes in place of the
// token; the proxy credential becomes egresscred.Placeholder.
const GitTokenPlaceholder = "[token]"

// GitScrubber is what scrubs req's token and proxy credential from a
// text: the token, user:token in base64 and their URL-escaped forms
// become GitTokenPlaceholder; the proxy credential, in every form
// egresscred's scrubbers know (the secret, user:secret in base64, the
// credentialed proxy URL on bareProxy, which may be empty) and
// URL-escaped, becomes egresscred.Placeholder. The agent's MCP server
// uses it too, on an error of its own.
func GitScrubber(req GitRequest, bareProxy string) func(string) string {
	token := tokenReplacer(req.Auth.Username, req.Auth.Password)
	proxyURL := ""
	if bareProxy != "" && req.Proxy.Password != "" {
		proxyURL, _ = egresscred.ProxyURL(bareProxy, req.Proxy.Username, req.Proxy.Password)
	}
	proxy := egresscred.Combine(
		egresscred.NewScrubber(req.Proxy.Username, req.Proxy.Password, proxyURL),
		escapedScrubber(req.Proxy.Password),
	)
	return func(s string) string {
		if token != nil {
			s = token.Replace(s)
		}
		return proxy.String(s)
	}
}

// tokenReplacer replaces token, user:token in base64 and their
// URL-escaped forms with GitTokenPlaceholder, the longest first. Nil
// for no token.
func tokenReplacer(user, token string) *strings.Replacer {
	if token == "" {
		return nil
	}
	set := map[string]bool{token: true, base64.StdEncoding.EncodeToString([]byte(user + ":" + token)): true}
	set[url.QueryEscape(token)] = true
	set[url.PathEscape(token)] = true
	needles := make([]string, 0, len(set))
	for n := range set {
		needles = append(needles, n)
	}
	sort.Slice(needles, func(i, j int) bool {
		if len(needles[i]) != len(needles[j]) {
			return len(needles[i]) > len(needles[j])
		}
		return needles[i] < needles[j]
	})
	pairs := make([]string, 0, 2*len(needles))
	for _, n := range needles {
		pairs = append(pairs, n, GitTokenPlaceholder)
	}
	return strings.NewReplacer(pairs...)
}

// clipUTF8 is s cut to at most n bytes, on a rune boundary.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}

// escapedScrubber scrubs secret's URL-escaped forms when they differ
// from it.
func escapedScrubber(secret string) *egresscred.Scrubber {
	var out []*egresscred.Scrubber
	for _, e := range []string{url.QueryEscape(secret), url.PathEscape(secret)} {
		if e != secret {
			out = append(out, egresscred.NewScrubber("", e, ""))
		}
	}
	return egresscred.Combine(out...)
}

func (r *GitResponse) scrub(s func(string) string) {
	r.Repository = s(r.Repository)
	r.Error = s(r.Error)
	for i := range r.Updated {
		r.Updated[i].Ref = s(r.Updated[i].Ref)
	}
}

// gitPathLocks serialises the jobs on one working copy, by its
// resolved path.
type gitPathLocks struct {
	mu   sync.Mutex
	held map[string]*gitPathLock
}

// gitPathLock is one path's turn: a job holds it while ch holds a
// token; waiters counts the jobs that hold it or wait for it.
type gitPathLock struct {
	ch      chan struct{}
	waiters int
}

// acquire waits for path's turn, or for ctx to end. onWait, when set,
// is told the path is busy before the wait (a test hook). release
// gives the turn to the next job.
func (l *gitPathLocks) acquire(ctx context.Context, path string, onWait func(string)) (release func(), err error) {
	l.mu.Lock()
	pl := l.held[path]
	if pl == nil {
		pl = &gitPathLock{ch: make(chan struct{}, 1)}
		l.held[path] = pl
	}
	pl.waiters++
	busy := pl.waiters > 1
	l.mu.Unlock()
	done := func() {
		l.mu.Lock()
		pl.waiters--
		if pl.waiters == 0 {
			delete(l.held, path)
		}
		l.mu.Unlock()
	}
	if busy && onWait != nil {
		onWait(path)
	}
	select {
	case pl.ch <- struct{}{}:
		return func() { <-pl.ch; done() }, nil
	case <-ctx.Done():
		done()
		return nil, ctx.Err()
	}
}

// runGit runs req under its timeout on cfg.Clock, after any other job
// on the same working copy.
func runGit(ctx context.Context, cfg ServerConfig, locks *gitPathLocks, req GitRequest) GitResponse {
	start := cfg.Clock.Now()
	res := runGitOp(ctx, cfg, locks, req)
	res.ConnectionID = req.ConnectionID
	res.DurationMs = cfg.Clock.Now().Sub(start).Milliseconds()
	if n := len(res.Updated); n > 0 {
		res.UpdatedTotal = n
		if n > GitUpdatedMax {
			res.Updated = res.Updated[:GitUpdatedMax]
		}
	}
	return res
}

func runGitOp(ctx context.Context, cfg ServerConfig, locks *gitPathLocks, req GitRequest) GitResponse {
	if cfg.GitUnavailable != "" {
		return GitResponse{Error: cfg.GitUnavailable}
	}
	if req.Op != GitOpClone && req.Op != GitOpFetch && req.Op != GitOpPush {
		return GitResponse{Error: fmt.Sprintf("%q is not an operation: clone, fetch or push", req.Op)}
	}
	host, err := gitrepo.NormalizeHost(req.Host)
	if err != nil {
		return GitResponse{Error: "the connection's host is not a host name"}
	}
	dir, err := resolveGitPath(cfg, req.Path)
	if err != nil {
		return GitResponse{Error: err.Error()}
	}

	timeout := GitTimeoutFor(req.Op, req.TimeoutSeconds)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	// Armed before the operation starts, so it counts from the request.
	t := cfg.Clock.NewTimer(timeout)
	defer t.Stop()
	go func() {
		select {
		case <-t.C():
			timedOut.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()

	// The wait for another job on the same working copy counts against
	// the bound too.
	release, err := locks.acquire(ctx, dir, cfg.gitWaitHook)
	if err != nil {
		if timedOut.Load() {
			return GitResponse{TimedOut: true, Error: fmt.Sprintf("did not finish within %s", timeout)}
		}
		return GitResponse{Error: "the call ended before the operation started"}
	}
	defer release()

	var res GitResponse
	switch req.Op {
	case GitOpClone:
		res = gitClone(ctx, cfg, req, host, dir)
	case GitOpFetch:
		res = gitFetch(ctx, cfg, req, host, dir)
	default:
		res = gitPush(ctx, cfg, req, host, dir)
	}
	if timedOut.Load() {
		res.TimedOut = true
		res.Error = fmt.Sprintf("did not finish within %s", timeout)
	}
	return res
}

// gitTransport is the auth and the proxy an operation runs with: the
// request's token as basic auth, and the daemon's bare proxy URL with
// the request's proxy credential.
func gitTransport(cfg ServerConfig, req GitRequest) (transport.AuthMethod, transport.ProxyOptions) {
	var auth transport.AuthMethod
	if req.Auth.Password != "" {
		auth = &githttp.BasicAuth{Username: cmp.Or(req.Auth.Username, "x-access-token"), Password: req.Auth.Password}
	}
	var proxy transport.ProxyOptions
	if cfg.GitProxyURL != "" {
		proxy = transport.ProxyOptions{URL: cfg.GitProxyURL, Username: req.Proxy.Username, Password: req.Proxy.Password}
	}
	return auth, proxy
}

// gitRepository is the repository of the job's URL, held to the job's
// host and its allowed repositories.
func gitRepository(cfg ServerConfig, req GitRequest, host string) (gitrepo.Repository, error) {
	repo, err := gitrepo.ParseURL(req.URL, host, cfg.GitInsecureHTTP)
	if err != nil {
		return repo, fmt.Errorf("the repository's URL: %w", err)
	}
	if !gitrepo.Match(req.AllowedRepositories, repo.Key()) {
		return repo, fmt.Errorf("%s is not a repository the access includes", repo.Path)
	}
	return repo, nil
}

func gitClone(ctx context.Context, cfg ServerConfig, req GitRequest, host, dir string) GitResponse {
	if req.URL == "" {
		return GitResponse{Error: "a clone needs the repository's URL"}
	}
	repo, err := gitRepository(cfg, req, host)
	if err != nil {
		return GitResponse{Error: err.Error()}
	}
	res := GitResponse{Repository: repo.Path}
	switch entries, err := os.ReadDir(dir); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		res.Error = fmt.Sprintf("%s is not a directory the clone can go in", req.Path)
		return res
	case len(entries) > 0:
		res.Error = fmt.Sprintf("%s is not empty; clone into a new or empty directory", req.Path)
		return res
	}
	auth, proxy := gitTransport(cfg, req)
	r, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:          repo.URL(),
		RemoteName:   "origin",
		Auth:         auth,
		ProxyOptions: proxy,
		Tags:         git.AllTags,
	})
	switch {
	case isGitRedirect(err):
		res.Error = gitMovedError
		return res
	case err != nil:
		res.Error = "clone: " + err.Error()
		return res
	}
	res.Updated = refUpdates(nil, refSnapshot(r))
	return res
}

// openWorkingCopy is the working copy at dir, whose .git must be a
// directory, and the repository a fetch or a push reaches from it: the
// job's URL, or the working copy's origin (originRepository).
func openWorkingCopy(cfg ServerConfig, req GitRequest, host, dir string) (*git.Repository, gitrepo.Repository, error) {
	if fi, err := os.Lstat(filepath.Join(dir, git.GitDirName)); err != nil || !fi.IsDir() {
		return nil, gitrepo.Repository{}, fmt.Errorf("%s is not a working copy (it has no .git directory)", req.Path)
	}
	r, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{})
	if err != nil {
		return nil, gitrepo.Repository{}, errors.New("open the working copy: " + err.Error())
	}
	var repo gitrepo.Repository
	if req.URL != "" {
		repo, err = gitRepository(cfg, req, host)
	} else {
		repo, err = originRepository(cfg, r, req, host)
	}
	if err != nil {
		return nil, gitrepo.Repository{}, err
	}
	return r, repo, nil
}

func gitFetch(ctx context.Context, cfg ServerConfig, req GitRequest, host, dir string) GitResponse {
	r, repo, err := openWorkingCopy(cfg, req, host, dir)
	if err != nil {
		return GitResponse{Error: err.Error()}
	}
	res := GitResponse{Repository: repo.Path}
	specs, err := fetchRefSpecs(req.RefSpecs)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	before := refSnapshot(r)
	remote, err := r.CreateRemoteAnonymous(&config.RemoteConfig{Name: gitAnonymousRemote, URLs: []string{repo.URL()}})
	if err != nil {
		res.Error = "fetch: " + err.Error()
		return res
	}
	auth, proxy := gitTransport(cfg, req)
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RemoteName:   gitAnonymousRemote,
		RefSpecs:     specs,
		Auth:         auth,
		ProxyOptions: proxy,
		Tags:         git.AllTags,
		Force:        req.Force,
	})
	switch {
	case isGitRedirect(err):
		res.Error = gitMovedError
	case err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate):
		res.Error = "fetch: " + err.Error()
	}
	res.Updated = refUpdates(before, refSnapshot(r))
	return res
}

// pushSpec is one refspec of a push as the daemon checked it: what
// go-git is given, the remote ref it writes, the object it writes there
// and whether it may move that ref other than fast-forward.
type pushSpec struct {
	spec  config.RefSpec
	dst   string
	new   plumbing.Hash
	force bool
}

func gitPush(ctx context.Context, cfg ServerConfig, req GitRequest, host, dir string) GitResponse {
	r, repo, err := openWorkingCopy(cfg, req, host, dir)
	if err != nil {
		return GitResponse{Error: err.Error()}
	}
	res := GitResponse{Repository: repo.Path}
	specs, err := pushRefSpecs(r, req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	auth, proxy := gitTransport(cfg, req)
	// go-git reports no ref of a push and does not say whether the host
	// takes an atomic push: the host's receive-pack advertisement, read
	// first, gives both, and what each ref moves from.
	adv, err := pushAdvertisement(ctx, repo.URL(), auth, proxy)
	switch {
	case isGitRedirect(err):
		res.Error = gitMovedError
		return res
	case err != nil:
		res.Error = "push: " + err.Error()
		return res
	}
	if len(specs) > 1 && !adv.Capabilities.Supports(capability.Atomic) {
		res.Error = "the host does not take atomic pushes, so a push of several refs could land in part; push one ref per call"
		return res
	}
	remoteRefs, err := adv.AllReferences()
	if err != nil {
		res.Error = "push: " + err.Error()
		return res
	}
	before := map[string]plumbing.Hash{}
	if iter, err := remoteRefs.IterReferences(); err == nil {
		_ = iter.ForEach(func(ref *plumbing.Reference) error {
			if ref.Type() == plumbing.HashReference {
				before[ref.Name().String()] = ref.Hash()
			}
			return nil
		})
	}
	if err := checkPushMoves(ctx, r, specs, before); err != nil {
		res.Error = err.Error()
		return res
	}
	remote, err := r.CreateRemoteAnonymous(&config.RemoteConfig{Name: gitAnonymousRemote, URLs: []string{repo.URL()}})
	if err != nil {
		res.Error = "push: " + err.Error()
		return res
	}
	refspecs := make([]config.RefSpec, 0, len(specs))
	for _, s := range specs {
		refspecs = append(refspecs, s.spec)
	}
	// Force stays off: a refspec forces by its own +, which
	// pushRefSpecs allowed only when the job's Force does. Atomic: a host
	// that refuses one ref refuses them all, so a refused push wrote
	// nothing.
	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName:   gitAnonymousRemote,
		RefSpecs:     refspecs,
		Auth:         auth,
		ProxyOptions: proxy,
		Atomic:       true,
	})
	switch {
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		return res
	case isGitRedirect(err):
		res.Error = gitMovedError
		return res
	case err != nil:
		res.Error = "push: " + err.Error()
		return res
	}
	for _, s := range specs {
		old, had := before[s.dst]
		if had && old == s.new {
			continue
		}
		u := GitRefUpdate{Ref: s.dst, New: s.new.String()}
		if had {
			u.Old = old.String()
		}
		res.Updated = append(res.Updated, u)
	}
	sort.Slice(res.Updated, func(i, j int) bool { return res.Updated[i].Ref < res.Updated[j].Ref })
	return res
}

// pushAdvertisement is the host's receive-pack advertisement of the
// repository at url: its refs and its capabilities. It goes through
// the same transports, auth and proxy as the push.
func pushAdvertisement(ctx context.Context, url string, auth transport.AuthMethod, proxy transport.ProxyOptions) (*packp.AdvRefs, error) {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return nil, err
	}
	ep.Proxy = proxy
	c, err := gitclient.NewClient(ep)
	if err != nil {
		return nil, err
	}
	s, err := c.NewReceivePackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	return s.AdvertisedReferencesContext(ctx)
}

// errObjectMismatch is an object of the working copy whose content does
// not hash to the name it is stored under.
var errObjectMismatch = errors.New("the working copy's objects do not match their names")

// checkPushMoves holds each refspec without + to what git's own client
// allows, before anything is sent: an existing tag never moves, and an
// existing branch moves only to a descendant of the commit the remote
// has. The ancestry is the daemon's own walk, through objects whose
// content is checked against their names (verifiedObjects): go-git's
// check reads the working copy's objects, which the agent writes, as
// they are named.
func checkPushMoves(ctx context.Context, r *git.Repository, specs []pushSpec, before map[string]plumbing.Hash) error {
	objects := verifiedObjects{r.Storer}
	for _, s := range specs {
		old, had := before[s.dst]
		if s.force || !had || old == s.new {
			continue
		}
		if strings.HasPrefix(s.dst, gitrepo.TagPrefix) {
			return fmt.Errorf("%s exists; moving a tag needs force", s.dst)
		}
		ok, err := descends(ctx, objects, s.new, old)
		switch {
		case errors.Is(err, errObjectMismatch):
			return err
		case err != nil:
			return fmt.Errorf("%s: %v", s.dst, err)
		case !ok:
			return fmt.Errorf("%s: non-fast-forward update; the remote has commits the pushed one does not descend from, and pushing over them needs force", s.dst)
		}
	}
	return nil
}

// descends reports whether the commit from has ancestor among its
// ancestors, walking parents through objects.
func descends(ctx context.Context, objects storer.EncodedObjectStorer, from, ancestor plumbing.Hash) (bool, error) {
	seen := map[plumbing.Hash]bool{from: true}
	queue := []plumbing.Hash{from}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		h := queue[0]
		queue = queue[1:]
		c, err := object.GetCommit(objects, h)
		switch {
		case errors.Is(err, errObjectMismatch):
			return false, err
		case errors.Is(err, plumbing.ErrObjectNotFound):
			// History the working copy does not hold (a shallow clone):
			// the ancestor is not shown to be there.
			continue
		case err != nil:
			return false, fmt.Errorf("commit %s does not read: %v", h, err)
		}
		for _, p := range c.ParentHashes {
			if p == ancestor {
				return true, nil
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false, nil
}

// verifiedObjects is a storer whose objects are checked against their
// names: each one read is hashed (plumbing.ComputeHash) and refused
// with errObjectMismatch when the hash is not the one asked for.
type verifiedObjects struct {
	storer.EncodedObjectStorer
}

func (v verifiedObjects) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := v.EncodedObjectStorer.EncodedObject(t, h)
	if err != nil {
		return nil, err
	}
	rd, err := obj.Reader()
	if err != nil {
		return nil, err
	}
	content, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil {
		return nil, err
	}
	if plumbing.ComputeHash(obj.Type(), content) != h {
		return nil, errObjectMismatch
	}
	return obj, nil
}

// pushRefSpecs is the job's push refspecs, each held again to the
// rules core held it to: it parses (gitrepo.ParsePushRefSpec), deletes
// nothing, writes a branch or a tag AllowedRefs includes, forces only
// when the job's Force allows, and names a destination no other one
// does. Each source is resolved in the working copy, so a source that
// is not there is an error rather than a push of nothing.
func pushRefSpecs(r *git.Repository, req GitRequest) ([]pushSpec, error) {
	if len(req.RefSpecs) == 0 {
		return nil, errors.New("a push needs at least one ref")
	}
	out := make([]pushSpec, 0, len(req.RefSpecs))
	seen := map[string]bool{}
	for _, raw := range req.RefSpecs {
		p, err := gitrepo.ParsePushRefSpec(raw)
		switch {
		case errors.Is(err, gitrepo.ErrDeleteRef):
			return nil, fmt.Errorf("%q deletes a remote ref, which a push here never does", raw)
		case err != nil:
			return nil, fmt.Errorf("%q is not a refspec of a push: %v", raw, err)
		case !gitrepo.MatchRef(req.AllowedRefs, p.Dst):
			return nil, fmt.Errorf("%s is not a branch or tag the access lets you push", p.Dst)
		case p.Force && !req.Force:
			return nil, fmt.Errorf("%q forces, and the access does not allow force-pushing", raw)
		case seen[p.Dst]:
			return nil, fmt.Errorf("%s is named twice", p.Dst)
		}
		seen[p.Dst] = true
		src, hash, err := pushSource(r, p.Src)
		if err != nil {
			return nil, err
		}
		spec := config.RefSpec(gitrepo.PushRefSpec{Src: src, Dst: p.Dst, Force: p.Force}.String())
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("%q is not a refspec of a push: %v", raw, err)
		}
		out = append(out, pushSpec{spec: spec, dst: p.Dst, new: hash, force: p.Force})
	}
	return out, nil
}

// pushSource is src (a full ref name, HEAD or an object id) as go-git
// is given it, and the object it pushes. A ref that names an object
// directly is kept by name, so an annotated tag is pushed as the tag;
// HEAD, a symbolic ref and an object id are given as the hash of the
// commit they name, which go-git pushes only when it is a commit. That
// hash is the decoded commit's, which go-git computes from its
// content: one stored under another name is errObjectMismatch.
func pushSource(r *git.Repository, src string) (name string, hash plumbing.Hash, err error) {
	commit := func(h plumbing.Hash) (string, plumbing.Hash, error) {
		c, err := r.CommitObject(h)
		if err != nil {
			return "", plumbing.ZeroHash, fmt.Errorf("%s is not a commit in the working copy", src)
		}
		if c.Hash != h {
			return "", plumbing.ZeroHash, errObjectMismatch
		}
		return c.Hash.String(), c.Hash, nil
	}
	switch {
	case src == "HEAD":
		head, err := r.Head()
		if err != nil {
			return "", plumbing.ZeroHash, errors.New("HEAD names no commit in the working copy")
		}
		return commit(head.Hash())
	case gitrepo.IsObjectID(src):
		return commit(plumbing.NewHash(src))
	}
	ref, err := r.Reference(plumbing.ReferenceName(src), false)
	if err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("%s is not in the working copy", src)
	}
	if ref.Type() == plumbing.HashReference {
		return src, ref.Hash(), nil
	}
	if ref, err = r.Reference(plumbing.ReferenceName(src), true); err != nil {
		return "", plumbing.ZeroHash, fmt.Errorf("%s names no commit in the working copy", src)
	}
	return commit(ref.Hash())
}

// originRepository is the repository of the working copy's origin, read
// from .git/config as written, never rewritten by an insteadOf rule,
// and used only when it is on host and one of the job's allowed
// repositories.
func originRepository(cfg ServerConfig, r *git.Repository, req GitRequest, host string) (gitrepo.Repository, error) {
	c, err := r.Config()
	if err != nil {
		return gitrepo.Repository{}, errors.New("the working copy's configuration does not read")
	}
	var urls []string
	for _, sub := range c.Raw.Section("remote").Subsections {
		if sub.Name == "origin" {
			urls = append(urls, sub.Options.GetAll("url")...)
		}
	}
	if len(urls) != 1 {
		return gitrepo.Repository{}, errors.New("the working copy's origin does not have one URL; give the repository's url")
	}
	repo, err := gitrepo.ParseURL(urls[0], host, cfg.GitInsecureHTTP)
	if err != nil {
		return gitrepo.Repository{}, fmt.Errorf("the working copy's origin: %w", err)
	}
	if !gitrepo.Match(req.AllowedRepositories, repo.Key()) {
		return repo, fmt.Errorf("the working copy's origin, %s, is not a repository the access includes", repo.Path)
	}
	return repo, nil
}

// fetchRefSpecs is a fetch's refspecs: the default when there are
// none; otherwise each must parse, have a source, and write under
// refs/remotes/origin/ or refs/tags/, so a fetch never moves a local
// branch.
func fetchRefSpecs(in []string) ([]config.RefSpec, error) {
	if len(in) == 0 {
		return []config.RefSpec{gitDefaultRefSpec}, nil
	}
	out := make([]config.RefSpec, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		rs := config.RefSpec(s)
		if s == "" || strings.HasPrefix(s, "^") || rs.Validate() != nil || rs.IsDelete() || rs.Src() == "" {
			return nil, fmt.Errorf("%q is not a refspec such as +refs/heads/main:refs/remotes/origin/main", raw)
		}
		dst := s[strings.Index(s, ":")+1:]
		if !refUnder(dst, "refs/remotes/origin/") && !refUnder(dst, "refs/tags/") {
			return nil, fmt.Errorf("%q writes outside refs/remotes/origin/ and refs/tags/", raw)
		}
		out = append(out, rs)
	}
	return out, nil
}

// refUnder reports whether ref is a clean name below prefix.
func refUnder(ref, prefix string) bool {
	rest, ok := strings.CutPrefix(ref, prefix)
	return ok && rest != "" && path.Clean(ref) == ref && !strings.Contains(ref, "..")
}

// refSnapshot is every ref of r that names a commit, by name.
func refSnapshot(r *git.Repository) map[string]string {
	out := map[string]string{}
	iter, err := r.References()
	if err != nil {
		return out
	}
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference {
			out[ref.Name().String()] = ref.Hash().String()
		}
		return nil
	})
	return out
}

// refUpdates is each ref of after that is new or moved since before,
// by name.
func refUpdates(before, after map[string]string) []GitRefUpdate {
	var out []GitRefUpdate
	for name, h := range after {
		if old := before[name]; old != h {
			out = append(out, GitRefUpdate{Ref: name, Old: old, New: h})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// resolveGitPath is p as a directory under cfg.Root (/files) or
// cfg.Home: absolute, ~/ for the home, or relative to the root, as a
// command's cwd is. The existing part of the path is resolved through
// its symbolic links, and the result must still be under the same
// base, so no link leads the operation elsewhere. The base itself is
// refused: an operation goes in a directory of its own. The daemon
// writes with the same filesystem uid as the agent's commands, so this
// keeps an operation where it was asked to go, not out of anything the
// agent could write itself.
func resolveGitPath(cfg ServerConfig, p string) (string, error) {
	raw := p
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "", errors.New("path is required")
	case p == "~" || strings.HasPrefix(p, "~/"):
		if cfg.Home == "" {
			return "", errors.New("the shell has no home directory")
		}
		p = filepath.Join(cfg.Home, p[1:])
	case !filepath.IsAbs(p):
		p = filepath.Join(cfg.Root, p)
	}
	p = filepath.Clean(p)
	var base string
	for _, b := range []string{cfg.Root, cfg.Home} {
		if b != "" && pathBelow(p, filepath.Clean(b)) {
			base = filepath.Clean(b)
		}
	}
	if base == "" {
		return "", fmt.Errorf("%s is not a directory under %s or your home", raw, cfg.Root)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("%s: %v", base, err)
	}
	real, err := evalExisting(p)
	if err != nil {
		return "", fmt.Errorf("%s does not resolve", raw)
	}
	if !pathBelow(real, realBase) {
		return "", fmt.Errorf("%s leads out of %s through a symbolic link", raw, base)
	}
	return real, nil
}

// pathBelow reports whether p is strictly below base.
func pathBelow(p, base string) bool {
	return p != base && strings.HasPrefix(p, base+string(filepath.Separator))
}

// evalExisting is p with its longest existing prefix resolved through
// its symbolic links and the rest, which does not exist, appended.
func evalExisting(p string) (string, error) {
	var rest []string
	cur := p
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", os.ErrNotExist
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}
