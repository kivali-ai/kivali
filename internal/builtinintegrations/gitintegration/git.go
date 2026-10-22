// Package gitintegration is the Git built-in integration: the owner
// connects a git host with a token, and agents with access clone and
// fetch repositories through it.
//
// Its actions run in two halves. Core checks the call (the connection's
// host, the repository, the agent's access) and answers with a shell
// job (shelljob.GitJob) carrying the token and the connection's scoped
// proxy credential; the agent's MCP server has the agent-shell daemon
// run it (POST /v1/git, internal/agentshell/git.go) and sends the call
// again with the result, which Complete puts in words. Everything local
// (status, branch, commit, merge) is plain git in the agent's shell,
// which reaches only the hosts on the team's allow list. Test
// connection runs in core.
package gitintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/gitrepo"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// Name is the built-in's name; its actions are git_clone and git_fetch.
const Name = "git"

// The actions, unprefixed.
const (
	actionClone = "clone"
	actionFetch = "fetch"
)

// The bounds core gives a job: the daemon's own maxima for each
// operation (agentshell.GitCloneMaxTimeout, GitFetchMaxTimeout).
const (
	cloneTimeoutSeconds = 15 * 60
	fetchTimeoutSeconds = 10 * 60
)

// tokenUser is the user name the token is presented with: what GitHub
// expects for a personal access token; GitLab takes any.
const tokenUser = "x-access-token"

// Git is the built-in. Its fields are test hooks; the zero value is
// what the binary runs.
type Git struct {
	// InsecureHTTP makes the URLs it builds http:// and lets Test
	// connection list a repository over http://: for tests, whose git
	// server is an httptest one. The binary never sets it.
	InsecureHTTP bool
	// HTTPClient asks the account endpoints Test connection uses; nil
	// is http.DefaultClient.
	HTTPClient *http.Client
	// GitHubAPI and GitLabAPI are those endpoints' bases; empty, the
	// real ones.
	GitHubAPI string
	GitLabAPI string
	// LookupIP resolves the connection's host for Test connection's
	// check that it is public; nil is the system's resolver.
	LookupIP func(ctx context.Context, host string) ([]net.IP, error)
}

// New is the built-in as the binary runs it.
func New() *Git { return &Git{} }

var (
	_ builtinintegrations.ShellJobIntegration = (*Git)(nil)
	_ builtinintegrations.HostsOf             = (*Git)(nil)
	_ builtinintegrations.ConnectionChecker   = (*Git)(nil)
	_ builtinintegrations.AccessChecker       = (*Git)(nil)
)

// errBadHost is a connection whose host is not one.
const errBadHost = ownerWords("Host is a host name such as github.com, without https:// or a path")

// CheckConnection holds the connection, as the owner saves it, to a
// host gitrepo.NormalizeHost can read.
func (g *Git) CheckConnection(conn builtinintegrations.Connection) error {
	if _, err := gitrepo.NormalizeHost(conn.Fields["host"]); err != nil {
		return errBadHost
	}
	return nil
}

const cloneDescription = `Clones a repository with the owner's git connection into path, a directory under /files or your home that does not exist yet or is empty. url is the repository's HTTPS URL on the connection's host, or owner/name. The clone's origin is the repository's URL without the token. Afterwards use plain git in run_shell for everything local: status, branch, commit, diff, log, merge, rebase. Plain git reaches only hosts on the team's allow list; this reaches the connection's host with its token. Refused for a repository outside your access.`

const fetchDescription = `Fetches from the repository's origin with the owner's git connection into the working copy at path, updating refs/remotes/origin/* and tags; then merge, rebase or check out with plain git. refs are refspecs; empty fetches every branch. url, when given, is the repository's HTTPS URL on the connection's host, or owner/name; empty, the repository is the working copy's origin. Refused for a repository outside your access.`

const cloneSchema = `{"type":"object","properties":{` +
	`"url":{"type":"string","description":"the repository's HTTPS URL on the connection's host, or owner/name"},` +
	`"path":{"type":"string","description":"a directory under /files or your home (~/...) that does not exist yet or is empty"}},` +
	`"required":["url","path"]}`

const fetchSchema = `{"type":"object","properties":{` +
	`"path":{"type":"string","description":"the working copy, under /files or your home (~/...)"},` +
	`"url":{"type":"string","description":"optional: the repository's HTTPS URL on the connection's host, or owner/name; empty, the working copy's origin"},` +
	`"refs":{"type":"array","items":{"type":"string"},"description":"optional refspecs, each writing under refs/remotes/origin/ or refs/tags/, such as +refs/heads/main:refs/remotes/origin/main"}},` +
	`"required":["path"]}`

// Manifest is git's manifest.
func (g *Git) Manifest() *integration.Manifest {
	return &integration.Manifest{
		ManifestVersion: "0.3",
		Name:            Name,
		Version:         "1.0.0",
		DisplayName:     "Git",
		Description:     "Clones and fetches repositories over HTTPS with the owner's token; agents use plain git for everything local.",
		Author:          integration.Author{Name: "Kivali"},
		UserConfig: integration.UserConfig{
			{Key: "host", Type: "string", Title: "Host", Required: true,
				Description: "github.com, gitlab.com or any HTTPS git host, without https://"},
			{Key: "token", Type: "string", Title: "Access token", Sensitive: true, Required: true,
				Description: "A fine-grained personal access token on GitHub, or the host's equivalent, that can read the repositories agents will use"},
			{Key: "test_repository", Type: "string", Title: "Repository for Test connection",
				Description: "owner/name. Test connection lists its branches. Empty, it checks the token with GitHub's or GitLab's account endpoint."},
		},
		Tools: []integration.Tool{
			{Name: actionClone, Description: "Clones a repository the agent's access includes into the agent's files, with the connection's token."},
			{Name: actionFetch, Description: "Fetches new commits and tags into a working copy of a repository the agent's access includes."},
		},
		Kivali: integration.Kivali{
			Connection: integration.Connection{Method: "token"},
			Access: &integration.Access{
				Schema: json.RawMessage(`{"type":"object","properties":{` +
					`"repositories":{"type":"array","items":{"type":"string"},"title":"Repositories","description":"owner/name, or a pattern such as acme/*"}},` +
					`"required":["repositories"]}`),
			},
			Verify: "ls_remote",
		},
	}
}

// Tools lists clone and fetch for every caller: the access is checked
// on each call, against the repository it names.
func (g *Git) Tools(context.Context, builtinintegrations.Caller) ([]builtinintegrations.Tool, error) {
	return []builtinintegrations.Tool{
		{Name: actionClone, Description: cloneDescription, InputSchema: json.RawMessage(cloneSchema)},
		{Name: actionFetch, Description: fetchDescription, InputSchema: json.RawMessage(fetchSchema)},
	}, nil
}

// Hosts is the host the connection names, without its port: what its
// scoped proxy credential admits. None while the host is not one.
func (g *Git) Hosts(conn builtinintegrations.Connection) []string {
	host, err := gitrepo.NormalizeHost(conn.Fields["host"])
	if err != nil {
		return nil
	}
	return []string{gitrepo.HostName(host)}
}

// Choices: git's access form lists no options.
func (g *Git) Choices(context.Context, builtinintegrations.Connection, string) ([]string, error) {
	return nil, errors.New("Git lists no options")
}

// call is one clone or fetch as core checked it.
type call struct {
	action   string
	host     string
	token    string
	patterns []string
	path     string
	refs     []string
	// repo is the repository the call names; zero for a fetch without
	// a url, whose repository is the working copy's origin.
	repo    gitrepo.Repository
	hasRepo bool
}

type cloneArgs struct {
	URL  string `json:"url"`
	Path string `json:"path"`
}

type fetchArgs struct {
	Path string   `json:"path"`
	URL  string   `json:"url"`
	Refs []string `json:"refs"`
}

func refuse(text, reason string) error {
	return &builtinintegrations.Refusal{Text: text, Reason: reason}
}

// check reads a call of name and holds it to the connection and the
// caller's access: every way it can be refused is a Refusal, before
// anything runs.
func (g *Git) check(caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage) (call, error) {
	c := call{action: name}
	if name != actionClone && name != actionFetch {
		return c, refuse("The Git integration has no action "+name+".", "no such action")
	}
	host, err := gitrepo.NormalizeHost(conn.Fields["host"])
	if err != nil {
		return c, refuse("The Git integration's connection has no host Kivali can use. Ask the owner to fix the connection.", "the connection's host is not a host name")
	}
	c.host, c.token = host, conn.Fields["token"]
	if c.token == "" {
		return c, refuse("The Git integration's connection has no token. Ask the owner to connect it again.", "the connection has no token")
	}
	if c.patterns, err = repositoryPatterns(caller.Access); err != nil {
		return c, refuse("Your access to the Git integration names no repositories Kivali can read. Ask the owner to set it again.", err.Error())
	}
	var url string
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if name == actionClone {
		var a cloneArgs
		if err := dec.Decode(&a); err != nil {
			return c, refuse("git_clone takes url and path, both strings.", "the arguments do not read")
		}
		url, c.path = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path)
		if url == "" || c.path == "" {
			return c, refuse("git_clone needs url and path.", "the arguments lack url or path")
		}
	} else {
		var a fetchArgs
		if err := dec.Decode(&a); err != nil {
			return c, refuse("git_fetch takes path, and optionally url and refs, a list of strings.", "the arguments do not read")
		}
		url, c.path, c.refs = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path), a.Refs
		if c.path == "" {
			return c, refuse("git_fetch needs path.", "the arguments lack path")
		}
	}
	if url == "" {
		return c, nil
	}
	repo, err := gitrepo.Parse(url, host, g.InsecureHTTP)
	var other *gitrepo.OtherHostError
	switch {
	case errors.As(err, &other):
		return c, refuse(fmt.Sprintf("The Git integration's connection is for %s; give a repository's URL on it, or owner/name.", host),
			"the URL is on another host than the connection's")
	case err != nil:
		return c, refuse(fmt.Sprintf("Kivali cannot read the repository: %v. Give its HTTPS URL on %s, or owner/name.", err, host),
			"the repository does not read")
	}
	if !accessIncludes(caller.Access, repo) {
		return c, refuse(fmt.Sprintf("The Git integration's access for you does not include %s. Ask the owner if your work needs it.", repo.Path),
			"the access does not include "+repo.Path)
	}
	c.repo, c.hasRepo = repo, true
	return c, nil
}

// Call checks the call and answers with the shell job that runs it.
func (g *Git) Call(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage) (*builtinintegrations.Result, error) {
	c, err := g.check(caller, conn, name, args)
	if err != nil {
		return nil, err
	}
	job := &shelljob.GitJob{
		Op:                  c.action,
		Path:                c.path,
		Host:                c.host,
		AllowedRepositories: c.patterns,
		Auth:                shelljob.GitAuth{Username: tokenUser, Password: c.token},
		TimeoutSeconds:      fetchTimeoutSeconds,
	}
	if c.hasRepo {
		job.URL = c.repo.URL()
	}
	if c.action == actionClone {
		job.TimeoutSeconds = cloneTimeoutSeconds
	} else {
		job.RefSpecs = c.refs
	}
	// The audit row core writes as it hands the job out.
	started := "started: the working copy's origin at " + clip(c.path, 200)
	if c.hasRepo {
		started = "started: " + c.repo.Path
	}
	return &builtinintegrations.Result{ShellJob: &shelljob.Job{Git: job}, Audit: started}, nil
}

// answerRefsMax and auditRefsMax bound the refs an answer and an audit
// row name.
const (
	answerRefsMax = 50
	auditRefsMax  = 10
	// errorMax bounds the host's words an answer quotes.
	errorMax = 1000
)

// Complete puts the shell's result in words for the agent and the
// audit. The call is checked again first, as on the first request. A
// fetch without a url names the repository the daemon reported, which
// is text from the agent's pod and only ever shown.
func (g *Git) Complete(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage, result shelljob.Result) (*builtinintegrations.Result, error) {
	c, err := g.check(caller, conn, name, args)
	if err != nil {
		return nil, err
	}
	verb, done := "fetch", "fetched"
	if c.action == actionClone {
		verb, done = "clone", "cloned"
	}
	repo := "the working copy's origin"
	if c.hasRepo {
		repo = c.repo.Path
	} else if result.Git != nil && result.Git.Repository != "" {
		repo = clip(result.Git.Repository, 200)
	}
	failed := func(text, audit string) (*builtinintegrations.Result, error) {
		return &builtinintegrations.Result{Content: []string{text}, IsError: true, Audit: repo + ": " + audit}, nil
	}
	switch r := result.Git; {
	case result.Skipped != "":
		return failed(fmt.Sprintf("You have no shell to run git in, so nothing was %s.", done), "nothing ran: "+clip(result.Skipped, 200))
	case r == nil:
		return failed(fmt.Sprintf("Your shell gave back no result for the %s; try again.", verb), "the shell gave back no result")
	case r.TimedOut:
		return failed(fmt.Sprintf("The %s of %s did not finish in time and was stopped.", verb, repo), "did not finish in time")
	case r.Error != "":
		words := clip(r.Error, errorMax)
		return failed(fmt.Sprintf("The %s of %s did not go through: %s", verb, repo, words), words)
	}
	var b strings.Builder
	if c.action == actionClone {
		fmt.Fprintf(&b, "Cloned %s into %s. Its origin is %s; use plain git there for everything local, and git_fetch to fetch with the connection.", repo, c.path, c.repo.URL())
	} else {
		fmt.Fprintf(&b, "Fetched %s into %s.", repo, c.path)
	}
	updated := result.Git.Updated
	// The daemon lists at most its first refs; the total counts all.
	total := max(result.Git.UpdatedTotal, len(updated))
	if total == 0 {
		b.WriteString(" Nothing changed: the working copy was up to date.")
	} else {
		fmt.Fprintf(&b, "\n\nUpdated (%d):", total)
		for i, u := range updated {
			if i == answerRefsMax {
				break
			}
			if u.Old == "" {
				fmt.Fprintf(&b, "\n%s new at %s", clip(u.Ref, 200), short(u.New))
			} else {
				fmt.Fprintf(&b, "\n%s %s..%s", clip(u.Ref, 200), short(u.Old), short(u.New))
			}
		}
		if shown := min(len(updated), answerRefsMax); total > shown {
			fmt.Fprintf(&b, "\n... and %d more", total-shown)
		}
	}
	return &builtinintegrations.Result{Content: []string{b.String()}, Audit: repo + ": " + auditRefs(updated, total)}, nil
}

// auditRefs is the refs an operation moved, as the audit names them:
// the first few of updated, and how many more of total.
func auditRefs(updated []shelljob.GitRefUpdate, total int) string {
	if total == 0 {
		return "up to date"
	}
	names := make([]string, 0, min(len(updated), auditRefsMax))
	for i, u := range updated {
		if i == auditRefsMax {
			break
		}
		names = append(names, clip(u.Ref, 200))
	}
	if len(names) == 0 {
		return fmt.Sprintf("updated %d refs", total)
	}
	s := "updated " + strings.Join(names, ", ")
	if total > len(names) {
		s += fmt.Sprintf(" and %d more", total-len(names))
	}
	return s
}

func short(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}

// clip is s cut to at most n bytes, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}
