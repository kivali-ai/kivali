// Package gitintegration is the Git built-in integration: the owner
// connects a git host with a token, and agents with access clone,
// fetch and push repositories through it.
//
// Its actions run in two halves. Core checks the call (the connection's
// host, the repository, the agent's access) and answers with a shell
// job (shelljob.GitJob) carrying the token and the connection's scoped
// proxy credential; the agent's MCP server has the agent-shell daemon
// run it (POST /v1/git, internal/agentshell/git.go) and sends the call
// again with the result, which Complete puts in words. Everything local
// (status, branch, commit, merge) is plain git in the agent's shell,
// which reaches only the hosts on the team's allow list. It has no
// connection test: core never reaches the host itself.
package gitintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/gitrepo"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// Name is the built-in's name; its actions are git_clone, git_fetch
// and git_push.
const Name = "git"

// The actions, unprefixed.
const (
	actionClone = "clone"
	actionFetch = "fetch"
	actionPush  = "push"
)

// The bounds core gives a job: the daemon's own maxima for each
// operation (agentshell.GitCloneMaxTimeout, GitFetchMaxTimeout,
// GitPushMaxTimeout).
const (
	cloneTimeoutSeconds = 15 * 60
	fetchTimeoutSeconds = 10 * 60
	pushTimeoutSeconds  = 10 * 60
)

// tokenUser is the user name the token is presented with: what GitHub
// expects for a personal access token; GitLab takes any.
const tokenUser = "x-access-token"

// Git is the built-in. Its fields are test hooks; the zero value is
// what the binary runs.
type Git struct {
	// InsecureHTTP makes the URLs it builds http://: for tests, whose
	// git server is an httptest one. The binary never sets it.
	InsecureHTTP bool
}

// New is the built-in as the binary runs it.
func New() *Git { return &Git{} }

var (
	_ builtinintegrations.ShellJobIntegration = (*Git)(nil)
	_ builtinintegrations.HostsOf             = (*Git)(nil)
	_ builtinintegrations.ConnectionChecker   = (*Git)(nil)
	_ builtinintegrations.AccessChecker       = (*Git)(nil)
	_ builtinintegrations.AccessSummarizer    = (*Git)(nil)
	_ builtinintegrations.ConnectionLabeler   = (*Git)(nil)
)

// ConnectionLabel names a connection the owner gave no label by its
// host, as gitrepo.NormalizeHost reads it ("github.com"); "" while the
// host is not one.
func (g *Git) ConnectionLabel(conn builtinintegrations.Connection) string {
	host, err := gitrepo.NormalizeHost(conn.Fields["host"])
	if err != nil {
		return ""
	}
	return host
}

// ownerWords is an error whose text is a sentence for the owner, as
// the connection form shows it.
type ownerWords string

func (e ownerWords) Error() string { return string(e) }

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

const pushDescription = `Pushes refs from the working copy at path to its origin with the owner's git connection. Each entry is a branch, a tag, or a refspec such as HEAD:refs/heads/agent/x; the destination must be one the owner allowed for you. A name without refs/ is a branch (agent/x is refs/heads/agent/x); a tag is written tags/v1.2.0 or refs/tags/v1.2.0. force, or a + in front of an entry, overwrites the remote ref and needs the owner's separate permission. Deleting a remote ref is refused. url, when given, is the repository's HTTPS URL on the connection's host, or owner/name; empty, the repository is the working copy's origin. Refused for a repository, a ref or a force push outside your access.`

const cloneSchema = `{"type":"object","properties":{` +
	`"url":{"type":"string","description":"the repository's HTTPS URL on the connection's host, or owner/name"},` +
	`"path":{"type":"string","description":"a directory under /files or your home (~/...) that does not exist yet or is empty"}},` +
	`"required":["url","path"]}`

const fetchSchema = `{"type":"object","properties":{` +
	`"path":{"type":"string","description":"the working copy, under /files or your home (~/...)"},` +
	`"url":{"type":"string","description":"optional: the repository's HTTPS URL on the connection's host, or owner/name; empty, the working copy's origin"},` +
	`"refs":{"type":"array","items":{"type":"string"},"description":"optional refspecs, each writing under refs/remotes/origin/ or refs/tags/, such as +refs/heads/main:refs/remotes/origin/main"}},` +
	`"required":["path"]}`

const pushSchema = `{"type":"object","properties":{` +
	`"path":{"type":"string","description":"the working copy, under /files or your home (~/...)"},` +
	`"refs":{"type":"array","items":{"type":"string"},"description":"what to push: a branch (agent/x), a tag (tags/v1.2.0) or a refspec such as HEAD:refs/heads/agent/x"},` +
	`"force":{"type":"boolean","description":"optional: overwrite the remote refs even where the push is not a fast-forward"},` +
	`"url":{"type":"string","description":"optional: the repository's HTTPS URL on the connection's host, or owner/name; empty, the working copy's origin"}},` +
	`"required":["path","refs"]}`

// accessSchema is the access form: the repositories, then the push
// switch, the refs it may push and the force switch. Only repositories
// is required: a row with only repositories is valid; push, push_refs
// and force are optional.
const accessSchema = `{"type":"object","properties":{` +
	`"repositories":{"type":"array","items":{"type":"string"},"title":"Repositories","description":"owner/name, or a pattern such as acme/*"},` +
	`"push":{"type":"boolean","title":"May push","description":"Push the branches and tags below to repositories in the list."},` +
	`"push_refs":{"type":"array","items":{"type":"string"},"title":"Branches and tags it may push","description":"A branch, a tag, or a pattern: agent/*, feature/*, tags/v*. Nothing means none."},` +
	`"force":{"type":"boolean","title":"May force-push","description":"Overwrites history on the remote. Leave off unless the work needs it.","x-kivali-danger":true}},` +
	`"required":["repositories"]}`

// Manifest is git's manifest.
func (g *Git) Manifest() *integration.Manifest {
	return &integration.Manifest{
		ManifestVersion: "0.3",
		Name:            Name,
		Version:         "1.2.0",
		DisplayName:     "Git",
		Description:     "Clones, fetches and pushes repositories over HTTPS with the owner's token; agents use plain git for everything local.",
		Author:          integration.Author{Name: "Kivali"},
		UserConfig: integration.UserConfig{
			{Key: "host", Type: "string", Title: "Host", Required: true,
				Description: "github.com, gitlab.com or any HTTPS git host, without https://"},
			{Key: "token", Type: "string", Title: "Access token", Sensitive: true, Required: true,
				Description: "A fine-grained personal access token on GitHub, or the host's equivalent, that can read the repositories agents will use"},
		},
		Tools: []integration.Tool{
			{Name: actionClone, Description: "Clones a repository the agent's access includes into the agent's files, with the connection's token."},
			{Name: actionFetch, Description: "Fetches new commits and tags into a working copy of a repository the agent's access includes."},
			{Name: actionPush, Description: "Pushes the branches and tags the agent's access allows to a repository it includes."},
		},
		Kivali: integration.Kivali{
			Connection: integration.Connection{Method: "token", Multiple: true},
			Access:     &integration.Access{Schema: json.RawMessage(accessSchema)},
		},
	}
}

// Tools lists clone, fetch and push for every caller: the access is
// checked on each call, against the repository and the refs it names.
func (g *Git) Tools(context.Context, builtinintegrations.Caller) ([]builtinintegrations.Tool, error) {
	return []builtinintegrations.Tool{
		{Name: actionClone, Description: cloneDescription, InputSchema: json.RawMessage(cloneSchema)},
		{Name: actionFetch, Description: fetchDescription, InputSchema: json.RawMessage(fetchSchema)},
		{Name: actionPush, Description: pushDescription, InputSchema: json.RawMessage(pushSchema)},
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

// Verify: git's manifest names no kivali.verify, so core offers no
// Test connection and never calls this.
func (g *Git) Verify(context.Context, builtinintegrations.Connection) (string, error) {
	return "", errors.New("Git has no connection test")
}

// call is one clone, fetch or push as core checked it.
type call struct {
	action   string
	host     string
	token    string
	patterns []string
	path     string
	// refs are a fetch's refspecs as given, or a push's normalised
	// ([+]src:dst).
	refs []string
	// A push's: the destinations, in order; the access's push_refs,
	// normalised; whether any refspec forces.
	dsts        []string
	allowedRefs []string
	force       bool
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

type pushArgs struct {
	Path  string   `json:"path"`
	Refs  []string `json:"refs"`
	Force bool     `json:"force"`
	URL   string   `json:"url"`
}

func refuse(text, reason string) error {
	return &builtinintegrations.Refusal{Text: text, Reason: reason}
}

// check reads a call of name and holds it to the connection and the
// caller's access: every way it can be refused is a Refusal, before
// anything runs.
func (g *Git) check(caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage) (call, error) {
	c := call{action: name}
	if name != actionClone && name != actionFetch && name != actionPush {
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
	switch name {
	case actionClone:
		var a cloneArgs
		if err := dec.Decode(&a); err != nil {
			return c, refuse("git_clone takes url and path, both strings.", "the arguments do not read")
		}
		url, c.path = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path)
		if url == "" || c.path == "" {
			return c, refuse("git_clone needs url and path.", "the arguments lack url or path")
		}
	case actionFetch:
		var a fetchArgs
		if err := dec.Decode(&a); err != nil {
			return c, refuse("git_fetch takes path, and optionally url and refs, a list of strings.", "the arguments do not read")
		}
		url, c.path, c.refs = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path), a.Refs
		if c.path == "" {
			return c, refuse("git_fetch needs path.", "the arguments lack path")
		}
	default:
		var a pushArgs
		if err := dec.Decode(&a); err != nil {
			return c, refuse("git_push takes path and refs, a list of strings, and optionally force (true or false) and url.", "the arguments do not read")
		}
		url, c.path = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path)
		if c.path == "" || len(a.Refs) == 0 {
			return c, refuse("git_push needs path and refs.", "the arguments lack path or refs")
		}
		if err := checkPush(&c, caller.Access, a); err != nil {
			return c, err
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

// checkPush holds a push's refs to the caller's access: push must be
// on, every entry must read as a ref or a refspec that deletes nothing,
// its destination must be one of push_refs, and a forced one (force, or
// a + in front) needs force on. It fills c's refs, destinations and
// force.
func checkPush(c *call, access map[string]any, a pushArgs) error {
	if !accessAllows(access, pushKey) {
		return refuse("The Git integration's access for you does not allow pushing. Ask the owner if your work needs it.", "the access does not allow pushing")
	}
	allowed, err := pushRefPatterns(access)
	if err != nil {
		return refuse("Your access to the Git integration names branches or tags Kivali cannot read. Ask the owner to set it again.", err.Error())
	}
	c.allowedRefs = allowed
	seen := map[string]bool{}
	for _, raw := range a.Refs {
		spec, err := gitrepo.ParsePushRefSpec(raw)
		switch {
		case errors.Is(err, gitrepo.ErrDeleteRef):
			return refuse("Deleting a remote ref is not something the Git integration does.", "the call deletes a remote ref")
		case err != nil:
			return refuse(fmt.Sprintf("git_push cannot read %q: %v. Each entry is a branch (agent/x), a tag (tags/v1.2.0) or a refspec such as HEAD:refs/heads/agent/x.", clip(raw, 200), err),
				"a ref does not read")
		case seen[spec.Dst]:
			return refuse(fmt.Sprintf("git_push names %s twice; push each destination once.", spec.Dst), "a destination is named twice")
		}
		seen[spec.Dst] = true
		spec.Force = spec.Force || a.Force
		if !gitrepo.MatchRef(allowed, spec.Dst) {
			return refuse(fmt.Sprintf("The Git integration's access for you does not include %s. Ask the owner if your work needs it.", spec.Dst),
				"the access does not include "+spec.Dst)
		}
		if spec.Force && !accessAllows(access, forceKey) {
			return refuse("The Git integration's access for you does not allow force-pushing. Ask the owner if your work needs it.", "the access does not allow force-pushing")
		}
		c.refs = append(c.refs, spec.String())
		c.dsts = append(c.dsts, spec.Dst)
		c.force = c.force || spec.Force
	}
	return nil
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
		ConnectionID:        conn.ID,
	}
	if c.hasRepo {
		job.URL = c.repo.URL()
	}
	switch c.action {
	case actionClone:
		job.TimeoutSeconds = cloneTimeoutSeconds
	case actionFetch:
		job.RefSpecs = c.refs
	case actionPush:
		job.RefSpecs, job.AllowedRefs, job.Force = c.refs, c.allowedRefs, c.force
		job.TimeoutSeconds = pushTimeoutSeconds
	}
	// The audit row core writes as it hands the job out.
	started := "started: the working copy's origin at " + clip(c.path, 200)
	if c.hasRepo {
		started = "started: " + c.repo.Path
	}
	if c.action == actionPush {
		// From core's own parse of the refs, never the shell's report.
		verb := "push of "
		if c.force {
			verb = "force push of "
		}
		started += ", " + verb + auditNames(c.dsts, len(c.dsts))
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
//
// A push the check now refuses, because the owner changed the agent's
// access while the job ran, may have written the remote all the same:
// when the result says refs landed, the answer and the audit say what
// landed and that the access changed (pushedUnderOldAccess).
//
// A job that ran through another connection than the one the agent's
// access names now (the owner moved the access to another connection
// while it ran: result.Git.ConnectionID is not conn.ID) is never
// refused: it is told as it ran, and the audit says the access changed
// (ranThroughAnotherConnection). conn's host and token are not the
// job's, so the call is not checked against them.
func (g *Git) Complete(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage, result shelljob.Result) (*builtinintegrations.Result, error) {
	if r := result.Git; r != nil && r.ConnectionID != "" && r.ConnectionID != conn.ID {
		return ranThroughAnotherConnection(name, r), nil
	}
	c, err := g.check(caller, conn, name, args)
	if err != nil {
		var ref *builtinintegrations.Refusal
		if name == actionPush && errors.As(err, &ref) {
			if res := pushedUnderOldAccess(args, result); res != nil {
				return res, nil
			}
		}
		return nil, err
	}
	verb, done := "fetch", "fetched"
	switch c.action {
	case actionClone:
		verb, done = "clone", "cloned"
	case actionPush:
		verb, done = "push", "pushed"
		if c.force {
			verb = "force push"
		}
	}
	repo := "the working copy's origin"
	if c.hasRepo {
		repo = c.repo.Path
	} else if result.Git != nil && result.Git.Repository != "" {
		repo = clip(result.Git.Repository, 200)
	}
	failed := func(text, audit string) (*builtinintegrations.Result, error) {
		if c.action == actionPush {
			audit = verb + " of " + auditNames(c.dsts, len(c.dsts)) + ": " + audit
		}
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
	if c.action == actionPush {
		return pushed(repo, c, result.Git), nil
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
		writeRefLines(&b, updated, total)
	}
	return &builtinintegrations.Result{Content: []string{b.String()}, Audit: repo + ": " + auditRefs(updated, total)}, nil
}

// writeRefLines writes a line for each of the first answerRefsMax of
// updated, then how many more of total there are.
func writeRefLines(b *strings.Builder, updated []shelljob.GitRefUpdate, total int) {
	for i, u := range updated {
		if i == answerRefsMax {
			break
		}
		if u.Old == "" {
			fmt.Fprintf(b, "\n%s new at %s", clip(u.Ref, 200), short(u.New))
		} else {
			fmt.Fprintf(b, "\n%s %s..%s", clip(u.Ref, 200), short(u.Old), short(u.New))
		}
	}
	if shown := min(len(updated), answerRefsMax); total > shown {
		fmt.Fprintf(b, "\n... and %d more", total-shown)
	}
}

// pushedRefs is the refs of updated whose names are among dsts, the
// destinations core asked for: the result is the agent's pod's, so a
// ref it reports that the push did not name is left out. total is how
// many the daemon said moved, at least the ones kept.
func pushedRefs(dsts []string, r *shelljob.GitResult) (kept []shelljob.GitRefUpdate, total int) {
	asked := make(map[string]bool, len(dsts))
	for _, d := range dsts {
		asked[d] = true
	}
	for _, u := range r.Updated {
		if asked[u.Ref] {
			asked[u.Ref] = false
			kept = append(kept, u)
		}
	}
	total = len(kept)
	// The daemon cut its list short: the refs past it are the push's
	// too, but cannot be named.
	if r.UpdatedTotal > len(r.Updated) {
		total = max(len(kept), min(r.UpdatedTotal, len(dsts)))
	}
	return kept, total
}

// pushed is a push's answer and audit: the refs it wrote, each marked
// new when the remote did not have it, or that the remote had them all.
func pushed(repo string, c call, r *shelljob.GitResult) *builtinintegrations.Result {
	verb := "pushed"
	if c.force {
		verb = "force-pushed"
	}
	updated, total := pushedRefs(c.dsts, r)
	if total == 0 {
		return &builtinintegrations.Result{
			Content: []string{fmt.Sprintf("Nothing to push: %s already has %s as the working copy does.", repo, joinNames(c.dsts, len(c.dsts), answerRefsMax))},
			Audit:   repo + ": up to date, " + auditNames(c.dsts, len(c.dsts)),
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s to %s.\n", strings.ToUpper(verb[:1])+verb[1:], joinNames(newMarked(updated), total, answerRefsMax), repo)
	writeRefLines(&b, updated, total)
	return &builtinintegrations.Result{Content: []string{b.String()}, Audit: repo + ": " + verb + " " + auditNames(refNames(updated), total)}
}

// ranThroughAnotherConnection is the answer to a call whose job ran
// through a connection the agent's access no longer names: what ran,
// or why it did not, as the result says, and that the access changed
// during it. Its audit row is allowed, since the job ran as the access
// allowed when it was handed out.
func ranThroughAnotherConnection(name string, r *shelljob.GitResult) *builtinintegrations.Result {
	verb, done := "fetch", "Fetched"
	switch name {
	case actionClone:
		verb, done = "clone", "Cloned"
	case actionPush:
		verb, done = "push", "Pushed to"
	}
	repo := "the repository"
	if r.Repository != "" {
		repo = clip(r.Repository, 200)
	}
	note := fmt.Sprintf("Your access to the Git integration changed to another connection during the %s; what it allows now is what your next call gets.", verb)
	changed := "; the access changed during the " + verb
	switch {
	case r.TimedOut:
		return &builtinintegrations.Result{Content: []string{fmt.Sprintf("The %s of %s did not finish in time and was stopped. %s", verb, repo, note)}, IsError: true, Audit: repo + ": did not finish in time" + changed}
	case r.Error != "":
		words := clip(r.Error, errorMax)
		return &builtinintegrations.Result{Content: []string{fmt.Sprintf("The %s of %s did not go through: %s\n\n%s", verb, repo, words, note)}, IsError: true, Audit: repo + ": " + words + changed}
	}
	total := max(r.UpdatedTotal, len(r.Updated))
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s. %s", done, repo, note)
	if total > 0 {
		fmt.Fprintf(&b, "\n\nUpdated (%d):", total)
		writeRefLines(&b, r.Updated, total)
	}
	return &builtinintegrations.Result{Content: []string{b.String()}, Audit: repo + ": " + auditRefs(r.Updated, total) + changed}
}

// pushedUnderOldAccess is the answer to a push whose second request
// the agent's access, changed while the job ran, now refuses, when the
// result says refs landed: they are on the remote, so the audit row is
// allowed and names them, and the agent reads what landed and that its
// access changed. nil when nothing landed, and the refusal stands.
func pushedUnderOldAccess(args json.RawMessage, result shelljob.Result) *builtinintegrations.Result {
	r := result.Git
	if result.Skipped != "" || r == nil || r.Error != "" || r.TimedOut || len(r.Updated) == 0 {
		return nil
	}
	var a pushArgs
	if json.Unmarshal(args, &a) != nil {
		return nil
	}
	var dsts []string
	for _, raw := range a.Refs {
		if spec, err := gitrepo.ParsePushRefSpec(raw); err == nil {
			dsts = append(dsts, spec.Dst)
		}
	}
	updated, total := pushedRefs(dsts, r)
	if total == 0 {
		return nil
	}
	repo := "the repository"
	if r.Repository != "" {
		repo = clip(r.Repository, 200)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Pushed %s to %s. Your access to the Git integration changed during the push; what it allows now is what your next call gets.\n", joinNames(newMarked(updated), total, answerRefsMax), repo)
	writeRefLines(&b, updated, total)
	return &builtinintegrations.Result{
		Content: []string{b.String()},
		Audit:   "pushed " + auditNames(refNames(updated), total) + " to " + repo + "; the access changed during the push",
	}
}

// newMarked is each ref's name, with " (new)" for one the remote did
// not have.
func newMarked(updated []shelljob.GitRefUpdate) []string {
	names := make([]string, 0, len(updated))
	for _, u := range updated {
		n := clip(u.Ref, 200)
		if u.Old == "" {
			n += " (new)"
		}
		names = append(names, n)
	}
	return names
}

func refNames(updated []shelljob.GitRefUpdate) []string {
	names := make([]string, 0, len(updated))
	for _, u := range updated {
		names = append(names, u.Ref)
	}
	return names
}

// joinNames is names in words, "a, b and c", at most max of them, and
// how many more of total there are.
func joinNames(names []string, total, max int) string {
	if len(names) > max {
		names = names[:max]
	}
	extra := total - len(names)
	switch {
	case extra > 0:
		return strings.Join(names, ", ") + fmt.Sprintf(" and %d more", extra)
	case len(names) == 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// auditNames is refs as the audit names them: at most auditRefsMax,
// then how many more of total.
func auditNames(refs []string, total int) string {
	names := make([]string, 0, min(len(refs), auditRefsMax))
	for i, r := range refs {
		if i == auditRefsMax {
			break
		}
		names = append(names, clip(r, 200))
	}
	if len(names) == 0 {
		return fmt.Sprintf("%d refs", total)
	}
	s := strings.Join(names, ", ")
	if total > len(names) {
		s += fmt.Sprintf(" and %d more", total-len(names))
	}
	return s
}

// auditRefs is the refs an operation moved, as the audit names them:
// the first few of updated, and how many more of total.
func auditRefs(updated []shelljob.GitRefUpdate, total int) string {
	if total == 0 {
		return "up to date"
	}
	return "updated " + auditNames(refNames(updated), total)
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
