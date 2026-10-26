// Package gitops is the clone, fetch and push a built-in integration
// runs in the agent's shell: the checks core makes of a call, the shell
// job (shelljob.GitJob) the agent-shell daemon runs it with (POST
// /v1/git, internal/agentshell/git.go), and the words the agent and the
// owner's audit read of what the daemon gave back.
//
// The built-in decides what a call may do. It reads its own connection
// and the caller's access into a Policy, per call, and hands it in;
// gitops reads no access form and no connection. A built-in's three
// actions are then:
//
//	Tools(in)                               the clone, fetch and push an agent is listed
//	Check(name, args, p)                    the first request: a Refusal, or the Call
//	call.Job(auth, connID), call.Started()  the job core hands the agent's shell, and its "started" audit row
//	Complete(name, args, p, connID, result) the second request: the answer and the audit row that ends the call
//
// Every refusal is a *builtinintegrations.Refusal whose words name the
// integration by its display name, the cause and the owner only. The
// rules for a repository's URL and for the refs a push names are
// gitrepo's, which the daemon holds the job to again.
package gitops

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/gitrepo"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// The actions, unprefixed: an agent calls them as <prefix>clone,
// <prefix>fetch and <prefix>push (integration.ToolPrefix of the
// built-in's name).
const (
	Clone = "clone"
	Fetch = "fetch"
	Push  = "push"
)

// IsAction reports whether name is one of the actions.
func IsAction(name string) bool {
	return name == Clone || name == Fetch || name == Push
}

// The bounds a job is given: the daemon's own maxima for each
// operation (agentshell.GitCloneMaxTimeout, GitFetchMaxTimeout,
// GitPushMaxTimeout).
const (
	cloneTimeoutSeconds = 15 * 60
	fetchTimeoutSeconds = 10 * 60
	pushTimeoutSeconds  = 10 * 60
)

// Integration is a built-in as its clone, fetch and push name it to the
// agent, and what they take. It is the same for every call.
type Integration struct {
	// Name is the built-in's name, whose tool prefix names the actions in
	// words: git_fetch, github_push.
	Name string
	// DisplayName names it in the agent's words: "The Git (Basic)
	// integration's access for you ...".
	DisplayName string
	// HostWords is the host as the tools' descriptions name it: "the
	// connection's host", or "github.com".
	HostWords string
	// NameRepository is a built-in whose fetch and push must name the
	// repository (url): its credential is narrowed to the repository
	// the call names. Without it, a fetch or a push without a url reaches
	// the working copy's origin, when the daemon finds it among the
	// repositories the call may reach.
	NameRepository bool
	// ForceSwitch is a built-in whose access has a switch that lets an
	// agent force-push (Policy.Force): its push takes force. Without it
	// the built-in never force-pushes: its push takes no force, and
	// force: true or a + in front of an entry is refused as something it
	// does not do.
	ForceSwitch bool
}

// tool is the action's name as the agent calls it.
func (in Integration) tool(action string) string {
	return integration.ToolPrefix(in.Name) + action
}

// Policy is what one call may do, as the built-in read it from its
// connection and the caller's access at the moment of the call.
type Policy struct {
	Integration
	// Refused, when set, is why the connection or the access grants
	// nothing Kivali can use, a *builtinintegrations.Refusal in the
	// built-in's own words (a connection without a token, an access that
	// does not read): Check refuses every call of an action with it, and
	// Complete takes it as the check refusing. The fields below are not
	// read then.
	Refused error
	// Host is the connection's host, made exact (gitrepo.NormalizeHost).
	Host string
	// Pull are the repositories a call may clone and fetch; Push the ones
	// it may push to, which it may clone and fetch too. Each is a
	// pattern gitrepo.Match reads.
	Pull, Push []string
	// Refs are the branches and tags a push may write, patterns
	// normalised (gitrepo.NormalizeRefPatterns); none, it may write none.
	// AnyRef lets a push write any branch or tag in their place.
	Refs   []string
	AnyRef bool
	// Force lets a push force (force, or a + in front of an entry), for an
	// Integration with ForceSwitch.
	Force bool
	// InsecureHTTP makes the URLs a call builds http://: for tests, whose
	// git server is an httptest one. The binary never sets it.
	InsecureHTTP bool
}

// Call is one clone, fetch or push as Check passed it.
type Call struct {
	in     Integration
	action string
	host   string
	// patterns are the repositories the call may reach, the job's
	// AllowedRepositories: Pull and Push for a clone or a fetch, Push for
	// a push.
	patterns []string
	path     string
	// refs are a fetch's refspecs as given, or a push's normalised
	// ([+]src:dst).
	refs []string
	// A push's: the destinations, in order; the refs it may write, or
	// any; whether any refspec forces.
	dsts        []string
	allowedRefs []string
	anyRef      bool
	force       bool
	// repo is the repository the call names; zero for a fetch or a push
	// without a url, whose repository is the working copy's origin.
	repo    gitrepo.Repository
	hasRepo bool
}

// Action is the call's action: Clone, Fetch or Push.
func (c Call) Action() string { return c.action }

// Repository is the repository the call names, and whether it names
// one: a fetch or a push without a url names none.
func (c Call) Repository() (gitrepo.Repository, bool) { return c.repo, c.hasRepo }

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

// pushArgsNoForce is a push's arguments where the built-in never
// forces: force is not one.
type pushArgsNoForce struct {
	Path string   `json:"path"`
	Refs []string `json:"refs"`
	URL  string   `json:"url"`
}

func refuse(text, reason string) error {
	return &builtinintegrations.Refusal{Text: text, Reason: reason}
}

// Check reads a call of name and holds it to p: every way it can be
// refused is a Refusal, before anything runs. It checks, in order, the
// action, p.Refused, the arguments (for a push, then its refs:
// checkPush) and the repository the url names, on p.Host and among
// what the call may reach.
func Check(name string, args json.RawMessage, p Policy) (Call, error) {
	in := p.Integration
	c := Call{in: in, action: name}
	if !IsAction(name) {
		return c, refuse(fmt.Sprintf("The %s integration has no action %s.", in.DisplayName, name), "no such action")
	}
	if p.Refused != nil {
		return c, p.Refused
	}
	c.host = p.Host
	c.patterns = slices.Concat(p.Pull, p.Push)
	tool := in.tool(name)
	var url string
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	switch name {
	case Clone:
		var a cloneArgs
		if err := dec.Decode(&a); err != nil {
			return c, refuse(tool+" takes url and path, both strings.", "the arguments do not read")
		}
		url, c.path = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path)
		if url == "" || c.path == "" {
			return c, refuse(tool+" needs url and path.", "the arguments lack url or path")
		}
	case Fetch:
		var a fetchArgs
		if err := dec.Decode(&a); err != nil {
			return c, refuse(fetchTakes(in), "the arguments do not read")
		}
		url, c.path, c.refs = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path), a.Refs
		if c.path == "" {
			return c, refuse(tool+" needs path.", "the arguments lack path")
		}
		if url == "" && in.NameRepository {
			return c, refuseNoURL(in, name, p.Host)
		}
	default:
		var a pushArgs
		if in.ForceSwitch {
			if err := dec.Decode(&a); err != nil {
				return c, refuse(pushTakes(in), "the arguments do not read")
			}
		} else {
			var b pushArgsNoForce
			if err := dec.Decode(&b); err != nil {
				// force: true asks for what the built-in never does.
				if json.Unmarshal(args, &a) == nil && a.Force {
					return c, refuseForcing(in)
				}
				return c, refuse(pushTakes(in), "the arguments do not read")
			}
			a = pushArgs{Path: b.Path, Refs: b.Refs, URL: b.URL}
		}
		url, c.path = strings.TrimSpace(a.URL), strings.TrimSpace(a.Path)
		if c.path == "" || len(a.Refs) == 0 {
			return c, refuse(tool+" needs path and refs.", "the arguments lack path or refs")
		}
		if url == "" && in.NameRepository {
			return c, refuseNoURL(in, name, p.Host)
		}
		if err := checkPush(&c, p, a); err != nil {
			return c, err
		}
	}
	if url == "" {
		return c, nil
	}
	repo, err := gitrepo.Parse(url, p.Host, p.InsecureHTTP)
	var other *gitrepo.OtherHostError
	switch {
	case errors.As(err, &other):
		return c, refuse(fmt.Sprintf("The %s integration's connection is for %s; give a repository's URL on it, or owner/name.", in.DisplayName, p.Host),
			"the URL is on another host than the connection's")
	case err != nil:
		return c, refuse(fmt.Sprintf("Kivali cannot read the repository: %v. Give its HTTPS URL on %s, or owner/name.", err, p.Host),
			"the repository does not read")
	}
	switch {
	case gitrepo.Match(c.patterns, repo.Key()):
	case name == Push && gitrepo.Match(p.Pull, repo.Key()):
		return c, refuse(fmt.Sprintf("The %s integration's access for you lets you pull %s but not push to it. Ask the owner if your work needs it.", in.DisplayName, repo.Path),
			"the access does not allow pushing to "+repo.Path)
	default:
		return c, refuse(fmt.Sprintf("The %s integration's access for you does not include %s. Ask the owner if your work needs it.", in.DisplayName, repo.Path),
			"the access does not include "+repo.Path)
	}
	c.repo, c.hasRepo = repo, true
	return c, nil
}

// fetchTakes is the refusal of a fetch's arguments that do not read.
func fetchTakes(in Integration) string {
	if in.NameRepository {
		return in.tool(Fetch) + " takes path and url, and optionally refs, a list of strings."
	}
	return in.tool(Fetch) + " takes path, and optionally url and refs, a list of strings."
}

// pushTakes is the refusal of a push's arguments that do not read.
func pushTakes(in Integration) string {
	tool := in.tool(Push)
	switch {
	case in.ForceSwitch && in.NameRepository:
		return tool + " takes path, url and refs, a list of strings, and optionally force (true or false)."
	case in.ForceSwitch:
		return tool + " takes path and refs, a list of strings, and optionally force (true or false) and url."
	case in.NameRepository:
		return tool + " takes path, url and refs, a list of strings."
	default:
		return tool + " takes path and refs, a list of strings, and optionally url."
	}
}

// refuseNoURL refuses a fetch or a push without a url, for a built-in
// whose calls must name the repository.
func refuseNoURL(in Integration, action, host string) error {
	return refuse(fmt.Sprintf("%s needs url: name the repository, as its HTTPS URL on %s or owner/name.", in.tool(action), host),
		"the arguments lack url")
}

// refuseForcing refuses a forced push, for a built-in that never
// forces (no ForceSwitch).
func refuseForcing(in Integration) error {
	return refuse(fmt.Sprintf("Force-pushing is not something the %s integration does.", in.DisplayName), "the call forces a push")
}

// checkPush holds a push's refs to p: it must have a repository to push
// to (p.Push, which becomes c's patterns), every entry must read as a
// ref or a refspec that deletes nothing, its destination must be one of
// p.Refs (any branch or tag with p.AnyRef), and a forced one (force, or
// a + in front) needs p.Force, where the built-in forces at all. It
// fills c's refs, destinations and force.
func checkPush(c *Call, p Policy, a pushArgs) error {
	in := p.Integration
	tool := in.tool(Push)
	if len(p.Push) == 0 {
		return refuse(fmt.Sprintf("The %s integration's access for you does not allow pushing. Ask the owner if your work needs it.", in.DisplayName), "the access does not allow pushing")
	}
	c.patterns = p.Push
	c.anyRef = p.AnyRef
	if !p.AnyRef {
		c.allowedRefs = p.Refs
	}
	seen := map[string]bool{}
	for _, raw := range a.Refs {
		spec, err := gitrepo.ParsePushRefSpec(raw)
		switch {
		case errors.Is(err, gitrepo.ErrDeleteRef):
			return refuse(fmt.Sprintf("Deleting a remote ref is not something the %s integration does.", in.DisplayName), "the call deletes a remote ref")
		case err != nil:
			return refuse(fmt.Sprintf("%s cannot read %q: %v. Each entry is a branch (agent/x), a tag (tags/v1.2.0) or a refspec such as HEAD:refs/heads/agent/x.", tool, clip(raw, 200), err),
				"a ref does not read")
		case seen[spec.Dst]:
			return refuse(fmt.Sprintf("%s names %s twice; push each destination once.", tool, spec.Dst), "a destination is named twice")
		}
		seen[spec.Dst] = true
		spec.Force = spec.Force || a.Force
		if !p.AnyRef && !gitrepo.MatchRef(p.Refs, spec.Dst) {
			return refuse(fmt.Sprintf("The %s integration's access for you does not include %s. Ask the owner if your work needs it.", in.DisplayName, spec.Dst),
				"the access does not include "+spec.Dst)
		}
		switch {
		case spec.Force && !in.ForceSwitch:
			return refuseForcing(in)
		case spec.Force && !p.Force:
			return refuse(fmt.Sprintf("The %s integration's access for you does not allow force-pushing. Ask the owner if your work needs it.", in.DisplayName), "the access does not allow force-pushing")
		}
		c.refs = append(c.refs, spec.String())
		c.dsts = append(c.dsts, spec.Dst)
		c.force = c.force || spec.Force
	}
	return nil
}

// Job is the shell job that runs c through the connection connectionID,
// presenting auth to the host. Core adds the connection's scoped proxy
// credential. It carries the credential: nothing on its path logs it.
func (c Call) Job(auth shelljob.GitAuth, connectionID string) *shelljob.GitJob {
	job := &shelljob.GitJob{
		Op:                  c.action,
		Path:                c.path,
		Host:                c.host,
		AllowedRepositories: c.patterns,
		Auth:                auth,
		TimeoutSeconds:      fetchTimeoutSeconds,
		ConnectionID:        connectionID,
	}
	if c.hasRepo {
		job.URL = c.repo.URL()
	}
	switch c.action {
	case Clone:
		job.TimeoutSeconds = cloneTimeoutSeconds
	case Fetch:
		job.RefSpecs = c.refs
	case Push:
		job.RefSpecs, job.AllowedRefs, job.AnyBranchOrTag, job.Force = c.refs, c.allowedRefs, c.anyRef, c.force
		job.TimeoutSeconds = pushTimeoutSeconds
	}
	return job
}

// Started is the reason of the audit row core writes as it hands c's
// job out: "started: <owner/name>", or "started: the working copy's
// origin at <path>" without a url; a push's adds what core read it to
// write, ", push of <refs>" or ", force push of <refs>", from core's own
// reading of the refs, never the shell's report.
func (c Call) Started() string {
	started := "started: the working copy's origin at " + clip(c.path, 200)
	if c.hasRepo {
		started = "started: " + c.repo.Path
	}
	if c.action == Push {
		verb := "push of "
		if c.force {
			verb = "force push of "
		}
		started += ", " + verb + auditNames(c.dsts, len(c.dsts))
	}
	return started
}

// answerRefsMax and auditRefsMax bound the refs an answer and an audit
// row name.
const (
	answerRefsMax = 50
	auditRefsMax  = 10
	// errorMax bounds the host's words an answer quotes.
	errorMax = 1000
)

// Complete puts the shell's result of the call name with args in words
// for the agent and the audit, under p as it stands at the second
// request; connectionID is the connection that request found. The call
// is checked again first, as on the first request. A fetch without a
// url names the repository the daemon reported, which is text from the
// agent's pod and only ever shown.
//
// A push the check now refuses, because the access changed while the
// job ran, may have written the remote all the same: when the result
// says refs landed, the answer and the audit say what landed and that
// the access changed (pushedUnderOldAccess).
//
// A job that ran through another connection than connectionID (the
// integration was disconnected and connected again while it ran:
// result.Git.ConnectionID is not connectionID) is never refused: it is
// told as it ran, and the audit says the connection changed
// (ranThroughReplacedConnection). p's host and the credential are not
// the job's then, so the call is not checked against them.
func Complete(name string, args json.RawMessage, p Policy, connectionID string, result shelljob.Result) (*builtinintegrations.Result, error) {
	in := p.Integration
	if r := result.Git; r != nil && r.ConnectionID != "" && r.ConnectionID != connectionID {
		return ranThroughReplacedConnection(in, name, r), nil
	}
	c, err := Check(name, args, p)
	if err != nil {
		var ref *builtinintegrations.Refusal
		if name == Push && errors.As(err, &ref) {
			if res := pushedUnderOldAccess(in, args, result); res != nil {
				return res, nil
			}
		}
		return nil, err
	}
	verb, done := "fetch", "fetched"
	switch c.action {
	case Clone:
		verb, done = "clone", "cloned"
	case Push:
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
		if c.action == Push {
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
	if c.action == Push {
		return pushed(repo, c, result.Git), nil
	}
	var b strings.Builder
	if c.action == Clone {
		fmt.Fprintf(&b, "Cloned %s into %s. Its origin is %s; use plain git there for everything local, and %s to fetch with the connection.", repo, c.path, c.repo.URL(), in.tool(Fetch))
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
func pushed(repo string, c Call, r *shelljob.GitResult) *builtinintegrations.Result {
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

// ranThroughReplacedConnection is the answer to a call whose job ran
// through a connection the integration no longer has: what ran, or why
// it did not, as the result says, and that the connection changed
// during it. Its audit row is allowed, since the job ran as the access
// allowed when it was handed out.
func ranThroughReplacedConnection(in Integration, name string, r *shelljob.GitResult) *builtinintegrations.Result {
	verb, done := "fetch", "Fetched"
	switch name {
	case Clone:
		verb, done = "clone", "Cloned"
	case Push:
		verb, done = "push", "Pushed to"
	}
	repo := "the repository"
	if r.Repository != "" {
		repo = clip(r.Repository, 200)
	}
	note := fmt.Sprintf("The %s connection changed during the %s; what your access allows now is what your next call gets.", in.DisplayName, verb)
	changed := "; the connection changed during the " + verb
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
// the access, changed while the job ran, now refuses, when the result
// says refs landed: they are on the remote, so the audit row is
// allowed and names them, and the agent reads what landed and that its
// access changed. nil when nothing landed, and the refusal stands.
func pushedUnderOldAccess(in Integration, args json.RawMessage, result shelljob.Result) *builtinintegrations.Result {
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
	fmt.Fprintf(&b, "Pushed %s to %s. Your access to the %s integration changed during the push; what it allows now is what your next call gets.\n", joinNames(newMarked(updated), total, answerRefsMax), repo, in.DisplayName)
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
