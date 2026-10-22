package agentshell

// POST /v1/git: a clone or a fetch through an integration's git
// connection, run in this process with go-git.
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
//   - A fetch's URL comes from the job, by the same rules, or, when the
//     job has none, from the working copy's origin as written in
//     .git/config (raw: never rewritten by a url.<base>.insteadOf),
//     parsed by the same rules, and refused unless it is on the job's
//     host and one of AllowedRepositories. .git/config is the agent's to
//     write, so nothing else in it is read.
//   - A fetch goes through an anonymous remote with that URL in code.
//     go-git then applies no insteadOf rule, from the repository's
//     config or a global one, ignores the [remote "origin"] the config
//     holds, and runs no hook. A named remote (RemoteName "origin"
//     without RemoteURL) would follow the config's URL: never use one.
//     A push through an anonymous remote needs PushOptions.RemoteName
//     "anonymous".
//   - The proxy is always given in code (ProxyOptions), the daemon's own
//     bare proxy URL with the job's credential, so go-git never reads a
//     proxy from the environment. With no proxy in the daemon's
//     environment (tests, a laptop), the operation connects directly.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// The operations POST /v1/git runs.
const (
	GitOpClone = "clone"
	GitOpFetch = "fetch"
)

// GitMaxBodyBytes caps a POST /v1/git body.
const GitMaxBodyBytes = 64 << 10

// The bounds of one git operation: a request's TimeoutSeconds, at most
// GitCloneMaxTimeout for a clone and GitFetchMaxTimeout for a fetch;
// GitDefaultTimeout when it gives none.
const (
	GitDefaultTimeout  = 5 * time.Minute
	GitCloneMaxTimeout = 15 * time.Minute
	GitFetchMaxTimeout = 10 * time.Minute
)

// GitTimeoutFor is the bound of op given seconds.
func GitTimeoutFor(op string, seconds int) time.Duration {
	if seconds <= 0 {
		return GitDefaultTimeout
	}
	max := GitFetchMaxTimeout
	if op == GitOpClone {
		max = GitCloneMaxTimeout
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
		scrub := gitScrubber(req, cfg.GitProxyURL)
		res := runGit(r.Context(), cfg, req)
		res.scrub(scrub)
		writeJSON(w, http.StatusOK, res)
	}
}

// gitScrubber is the scrubber of req's token and proxy credential, in
// every form egresscred's scrubbers know (the value, user:value in
// base64, the credentialed proxy URL), and URL-escaped.
func gitScrubber(req GitRequest, bareProxy string) *egresscred.Scrubber {
	proxyURL := ""
	if bareProxy != "" && req.Proxy.Password != "" {
		proxyURL, _ = egresscred.ProxyURL(bareProxy, req.Proxy.Username, req.Proxy.Password)
	}
	return egresscred.Combine(
		egresscred.NewScrubber(req.Auth.Username, req.Auth.Password, ""),
		escapedScrubber(req.Auth.Password),
		egresscred.NewScrubber(req.Proxy.Username, req.Proxy.Password, proxyURL),
		escapedScrubber(req.Proxy.Password),
	)
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

func (r *GitResponse) scrub(s *egresscred.Scrubber) {
	if s == nil {
		return
	}
	r.Repository = s.String(r.Repository)
	r.Error = s.String(r.Error)
	for i := range r.Updated {
		r.Updated[i].Ref = s.String(r.Updated[i].Ref)
	}
}

// runGit runs req under its timeout on cfg.Clock.
func runGit(ctx context.Context, cfg ServerConfig, req GitRequest) GitResponse {
	start := cfg.Clock.Now()
	res := runGitOp(ctx, cfg, req)
	res.DurationMs = cfg.Clock.Now().Sub(start).Milliseconds()
	return res
}

func runGitOp(ctx context.Context, cfg ServerConfig, req GitRequest) GitResponse {
	if cfg.GitUnavailable != "" {
		return GitResponse{Error: cfg.GitUnavailable}
	}
	if req.Op != GitOpClone && req.Op != GitOpFetch {
		return GitResponse{Error: fmt.Sprintf("%q is not an operation: clone or fetch", req.Op)}
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

	var res GitResponse
	if req.Op == GitOpClone {
		res = gitClone(ctx, cfg, req, host, dir)
	} else {
		res = gitFetch(ctx, cfg, req, host, dir)
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
	if err != nil {
		res.Error = "clone: " + err.Error()
		return res
	}
	res.Updated = refUpdates(nil, refSnapshot(r))
	return res
}

func gitFetch(ctx context.Context, cfg ServerConfig, req GitRequest, host, dir string) GitResponse {
	if fi, err := os.Lstat(filepath.Join(dir, git.GitDirName)); err != nil || !fi.IsDir() {
		return GitResponse{Error: fmt.Sprintf("%s is not a working copy (it has no .git directory)", req.Path)}
	}
	r, err := git.PlainOpenWithOptions(dir, &git.PlainOpenOptions{})
	if err != nil {
		return GitResponse{Error: "open the working copy: " + err.Error()}
	}
	var repo gitrepo.Repository
	if req.URL != "" {
		if repo, err = gitRepository(cfg, req, host); err != nil {
			return GitResponse{Error: err.Error()}
		}
	} else if repo, err = originRepository(cfg, r, req, host); err != nil {
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
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		res.Error = "fetch: " + err.Error()
	}
	res.Updated = refUpdates(before, refSnapshot(r))
	return res
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
