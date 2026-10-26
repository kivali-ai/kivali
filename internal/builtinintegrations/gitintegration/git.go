// Package gitintegration is the Git (Basic) built-in integration: the
// owner connects a git host other than github.com (which the GitHub
// integration handles) with a token, and agents with access clone,
// fetch and push repositories through it.
//
// Its actions are gitops's (internal/builtinintegrations/gitops), run
// in two halves. Core checks the call (the connection's host, the
// repository, the agent's access, read here into a gitops.Policy) and
// answers with a shell job (shelljob.GitJob) carrying the token and the
// connection's scoped proxy credential; the agent's MCP server has the
// agent-shell daemon run it (POST /v1/git, internal/agentshell/git.go)
// and sends the call again with the result, which Complete puts in
// words. Everything local (status, branch, commit, merge) is plain git
// in the agent's shell, which reaches only the hosts on the team's
// allow list. It has no connection test: core never reaches the host
// itself.
package gitintegration

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitops"
	"github.com/kivali-ai/kivali/internal/gitrepo"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// Name is the built-in's name; its actions are git_clone, git_fetch
// and git_push.
const Name = "git"

// displayName is the name the owner and the agents read.
const displayName = "Git (Basic)"

// tokenUser is the user name the token is presented with; GitLab takes
// any.
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
)

// ownerWords is an error whose text is a sentence for the owner, as
// the connection form shows it.
type ownerWords string

func (e ownerWords) Error() string { return string(e) }

// errBadHost is a connection whose host is not one.
const errBadHost = ownerWords("Host is a host name such as gitlab.com, without https:// or a path")

// errGitHubHost is a connection whose host is github.com.
const errGitHubHost = ownerWords("github.com is handled by the GitHub integration; connect it there")

// CheckConnection holds the connection, as the owner saves it, to a
// host gitrepo.NormalizeHost can read that is not github.com.
func (g *Git) CheckConnection(conn builtinintegrations.Connection) error {
	host, err := gitrepo.NormalizeHost(conn.Fields["host"])
	switch {
	case err != nil:
		return errBadHost
	case isGitHub(host):
		return errGitHubHost
	}
	return nil
}

// isGitHub reports whether host (as gitrepo.NormalizeHost returns it)
// is github.com or www.github.com, on any port: the GitHub
// integration's, which Git (Basic) does not handle.
func isGitHub(host string) bool {
	name := gitrepo.HostName(host)
	return name == "github.com" || name == "www.github.com"
}

// throughGitHub refuses a call through conn when its host is github.com:
// a connection an earlier version saved, which this one refuses at
// save. Nil for any other connection, one without a host among them.
func throughGitHub(conn builtinintegrations.Connection) error {
	if host, err := gitrepo.NormalizeHost(conn.Fields["host"]); err != nil || !isGitHub(host) {
		return nil
	}
	return &builtinintegrations.Refusal{
		Text:   "The " + displayName + " integration does not handle github.com; ask the owner.",
		Reason: "the connection is for github.com, which " + displayName + " does not handle",
	}
}

// accessSchema is the access form: the repositories it may pull, the
// ones it may push to, then the refs it may push there and the force
// switch. No field is required: the form cannot ask for one of two
// lists, so CheckAccess asks for a repository in either.
const accessSchema = `{"type":"object","properties":{` +
	`"pull_repositories":{"type":"array","items":{"type":"string"},"title":"Repositories it may pull","description":"Clone and fetch. owner/name, or a pattern such as acme/*."},` +
	`"push_repositories":{"type":"array","items":{"type":"string"},"title":"Repositories it may push to","description":"Clone, fetch and push the branches and tags below. owner/name, or a pattern such as acme/*."},` +
	`"push_refs":{"type":"array","items":{"type":"string"},"title":"Branches and tags it may push","description":"In the repositories it may push to. A branch, a tag, or a pattern: agent/*, feature/*, tags/v*. Nothing means none."},` +
	`"force":{"type":"boolean","title":"May force-push","description":"Overwrites history on the remote. Leave off unless the work needs it.","x-kivali-danger":true}}}`

// Manifest is git's manifest.
func (g *Git) Manifest() *integration.Manifest {
	return &integration.Manifest{
		ManifestVersion: "0.3",
		Name:            Name,
		Version:         "2.1.0",
		DisplayName:     displayName,
		Description:     "Clones, fetches and pushes repositories over HTTPS with the owner's token, on a git host other than github.com, which the GitHub integration handles; agents use plain git for everything local.",
		Author:          integration.Author{Name: "Kivali"},
		UserConfig: integration.UserConfig{
			{Key: "host", Type: "string", Title: "Host", Required: true,
				Description: "gitlab.com or any HTTPS git host other than github.com, without https://"},
			{Key: "token", Type: "string", Title: "Access token", Sensitive: true, Required: true,
				Description: "A token for the host, such as a GitLab access token, that can read the repositories agents will use"},
		},
		Tools: []integration.Tool{
			{Name: gitops.Clone, Description: "Clones a repository the agent's access includes into the agent's files, with the connection's token."},
			{Name: gitops.Fetch, Description: "Fetches new commits and tags into a working copy of a repository the agent's access includes."},
			{Name: gitops.Push, Description: "Pushes the branches and tags the agent's access allows to a repository it may push to.", Access: pushKey},
		},
		Kivali: integration.Kivali{
			Connection: integration.Connection{Method: "token"},
			Access:     &integration.Access{Schema: json.RawMessage(accessSchema)},
		},
	}
}

// integration is Git as its clone, fetch and push name it: a fetch or
// a push may reach the working copy's origin, and the access has a
// force switch.
func (g *Git) integration() gitops.Integration {
	return gitops.Integration{Name: Name, DisplayName: displayName, HostWords: "the connection's host", ForceSwitch: true}
}

// Tools lists clone and fetch for every caller, and push for one whose
// access names a repository it may push to, as the manifest's push
// says. The access is checked on each call, against the repository and
// the refs it names.
func (g *Git) Tools(_ context.Context, caller builtinintegrations.Caller) ([]builtinintegrations.Tool, error) {
	all := gitops.Tools(g.integration())
	if _, push, _ := repositoryPatterns(caller.Access); len(push) > 0 {
		return all, nil
	}
	out := make([]builtinintegrations.Tool, 0, len(all))
	for _, t := range all {
		if t.Name != gitops.Push {
			out = append(out, t)
		}
	}
	return out, nil
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
	return nil, errors.New(displayName + " lists no options")
}

// Verify: git's manifest names no kivali.verify, so core offers no
// Test connection and never calls this.
func (g *Git) Verify(context.Context, builtinintegrations.Connection) (string, error) {
	return "", errors.New(displayName + " has no connection test")
}

// Call checks the call and answers with the shell job that runs it,
// the token presented under tokenUser. A call through a github.com
// connection is refused first.
func (g *Git) Call(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage) (*builtinintegrations.Result, error) {
	if err := throughGitHub(conn); err != nil {
		return nil, err
	}
	c, err := gitops.Check(name, args, g.policy(caller, conn, name))
	if err != nil {
		return nil, err
	}
	job := c.Job(shelljob.GitAuth{Username: tokenUser, Password: conn.Fields["token"]}, conn.ID)
	return &builtinintegrations.Result{ShellJob: &shelljob.Job{Git: job}, Audit: c.Started()}, nil
}

// Complete puts the shell's result in words for the agent and the
// audit, with the call checked again under the access and the
// connection as they are at the second request (gitops.Complete). A
// call through a github.com connection is refused first, whatever the
// result says.
func (g *Git) Complete(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage, result shelljob.Result) (*builtinintegrations.Result, error) {
	if err := throughGitHub(conn); err != nil {
		return nil, err
	}
	return gitops.Complete(name, args, g.policy(caller, conn, name), conn.ID, result)
}
