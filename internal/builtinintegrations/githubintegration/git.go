package githubintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitops"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/shelljob"
)

// gitHost is where every repository a clone, a fetch or a push
// reaches is, and the one host its job's scoped proxy credential
// admits.
const gitHost = "github.com"

// tokenUser is the user name a job's token is presented with over
// HTTPS, as GitHub takes an installation token.
const tokenUser = "x-access-token"

// gitIntegration is GitHub as its clone, fetch and push name it: on
// github.com, a fetch and a push naming the repository, since a job's
// token is narrowed to it, no force switch (GitHub never force-pushes),
// and the workflows switch: GitHub refuses a push that changes a
// workflow file unless its token may write workflows, which a push asks
// for only when the agent says its commits do and its access allows it
// (gitPolicy). An agent is listed workflows only when its access allows
// it (Tools); any other that gives it is refused for its access.
var gitIntegration = gitops.Integration{Name: Name, DisplayName: "GitHub", HostWords: gitHost, NameRepository: true, WorkflowsSwitch: true}

// gitTools are clone, fetch and push as the manifest declares them,
// for the owner: push needs the write switch.
var gitTools = []integration.Tool{
	{Name: gitops.Clone, Description: "Clone a repository the agent's access names into its files, with a token for that repository alone; when the access allows writing, its git identity is the GitHub App's bot. Reads only."},
	{Name: gitops.Fetch, Description: "Fetch new commits and tags into the agent's working copy of a repository its access names, with a token for that repository alone. Reads only."},
	{Name: gitops.Push, Description: "Push branches and tags to a repository the agent's access names, with a token for that repository alone. Never forced, never deleting; GitHub's branch protection applies. Commits that change workflow files (.github/workflows) need the separate workflows switch as well, and the agent saying so." + needsWrite, Access: writeKey},
}

var (
	_ builtinintegrations.ShellJobIntegration = (*GitHub)(nil)
	_ builtinintegrations.HostsOf             = (*GitHub)(nil)
)

// Hosts is github.com, which a clone, a fetch or a push reaches, and
// the hosts GitHub keeps LFS files on (lfsHosts): what the connection's
// scoped proxy credential admits.
func (g *GitHub) Hosts(builtinintegrations.Connection) []string {
	return append([]string{gitHost}, lfsHosts...)
}

// lfsHosts are where GitHub's LFS API sends a clone, a fetch or a push
// for a file's content, at an address it signs for that file: the
// daemon goes nowhere else for one (agentshell.GitLFS), and sends it no
// token of Kivali's.
var lfsHosts = []string{"github-cloud.githubusercontent.com", "github-cloud.s3.amazonaws.com"}

// gitPolicy is what caller may do through conn with the clone, fetch
// or push name, and the connection's values: on github.com, the
// access's repositories to clone and fetch, and with write to push to
// as well, any branch or tag, never forced, changing workflow files
// only with the workflows switch too. A push without write is refused
// as every write action is, and a connection setup has not finished
// refuses every call.
func gitPolicy(caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string) (gitops.Policy, connection) {
	p := gitops.Policy{Integration: gitIntegration, Host: gitHost}
	acc := readAccess(caller.Access)
	if name == gitops.Push && !acc.write {
		p.Refused = refuse(writeRefusal, writeReason)
		return p, connection{}
	}
	c, err := readConnection(conn)
	if err != nil {
		p.Refused = refuse(notSetUp, err.Error())
		return p, c
	}
	// Each repository is in one of the lists, so a job's allowed
	// repositories are the access's.
	if acc.write {
		p.Push, p.AnyRef = acc.repositories, true
	} else {
		p.Pull = acc.repositories
	}
	p.Workflows = acc.pushesWorkflows()
	return p, c
}

// callGit is a clone's, a fetch's or a push's first request: the call
// checked (gitops.Check), then a token minted for its job alone
// (jobToken), narrowed to the call's repository and to reading its
// contents, or for a push writing them, and its workflow files too when
// the push says it changes them (jobPermissions); a repository under another
// account than the installation's is refused first (narrowedName). A
// refused call mints nothing. A clone for an agent whose access may
// write also names the app's bot user as the working copy's git
// identity, when GitHub gives it; the clone goes ahead without one when
// it does not (Complete says so).
// The token is presented as x-access-token's password and is in
// nothing but the job: not in the answer, the audit or an error.
func (g *GitHub) callGit(ctx context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage) (*builtinintegrations.Result, error) {
	p, c := gitPolicy(caller, conn, name)
	call, err := gitops.Check(name, args, p)
	if err != nil {
		return nil, err
	}
	repo, ok := call.Repository()
	switch {
	case !ok:
		// Not reached: Check passes no call without a repository
		// (NameRepository). It stays as a defence, since a job never
		// takes a token for the whole installation.
		return nil, refuse(integration.ToolPrefix(Name)+name+" needs url: name the repository, as its HTTPS URL on github.com or owner/name.", "the arguments lack url")
	case strings.Count(repo.Path, "/") != 1:
		echoed, _ := clip(repo.Path, 200)
		return nil, refuse(fmt.Sprintf("A GitHub repository is owner/name, and %s is not one.", echoed), "the repository is not owner/name")
	}
	tok, err := g.jobToken(ctx, c, repo.Path, jobPermissions(name, call.Workflows()))
	if err != nil {
		var te *tokenError
		switch {
		case errors.As(err, &te) && call.Workflows() && te.api.Status == http.StatusUnprocessableEntity:
			// GitHub mints no token at all for a permission the
			// installation lacks.
			return failed("GitHub refused the GitHub App a token that may change workflow files in " + repo.Path + " (HTTP 422): " + sentence(te.api.Message) +
				". The app's installation may lack the workflows permission; ask the owner to approve it on GitHub."), nil
		case errors.As(err, &te):
			return failed(te.words()), nil
		}
		return nil, err
	}
	job := call.Job(shelljob.GitAuth{Username: tokenUser, Password: tok}, conn.ID)
	// The job moves the LFS files its commits name with the same token,
	// through the repository's own LFS API.
	job.LFS = &shelljob.GitLFS{Hosts: lfsHosts}
	// A writer's clone is given the app's bot as its identity, so what
	// the agent commits there is the bot's (bot.go), which is what lets
	// it delete such a branch. The token comes first: a call the token's
	// checks refuse (a repository under another account) asks GitHub for
	// nothing.
	if name == gitops.Clone && readAccess(caller.Access).write {
		if b, err := g.bot(ctx, c, repo.Path); err == nil {
			job.Identity = &shelljob.GitIdentity{Name: b.login, Email: b.noreply()}
		}
	}
	return &builtinintegrations.Result{ShellJob: &shelljob.Job{Git: job}, Audit: call.Started()}, nil
}

// jobPermissions is what the job of the action name may do besides
// metadata read: read the repository's contents for a clone or a
// fetch; for a push, write them, and write its workflow files as well
// when workflows (the push said its commits change one, and the access
// allowed it: gitops.Call.Workflows). A push without it cannot change a
// workflow file: GitHub refuses it whole.
func jobPermissions(name string, workflows bool) permissions {
	switch {
	case name == gitops.Push && workflows:
		return perms("contents", "write", workflowsPermission, "write")
	case name == gitops.Push:
		return perms("contents", "write")
	}
	return perms("contents", "read")
}

// workflowsPermission is GitHub's permission to change a repository's
// workflow files, which a push needs for a commit that touches one.
// GitHub offers it as write alone.
const workflowsPermission = "workflows"

// workflowRefusal matches GitHub's refusal of a push that changes a
// workflow file with a token that may not: "refusing to allow a GitHub
// App to create or update workflow `.github/workflows/x.yml` without
// `workflows` permission".
var workflowRefusal = regexp.MustCompile("(?i)without `?workflows`? permission")

// Complete puts the shell's result in words for the agent and the
// audit, with the call checked again under the access and the
// connection as they are at the second request (gitops.Complete). A
// writer's clone that landed without the bot's identity (GitHub did not
// give the bot user, or the shell could not write it) says commits made
// there will not read as the app's, and a push GitHub refused for
// changing a workflow file says what to do (workflowWords). It mints no
// token.
func (g *GitHub) Complete(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage, result shelljob.Result) (*builtinintegrations.Result, error) {
	p, _ := gitPolicy(caller, conn, name)
	res, err := gitops.Complete(name, args, p, conn.ID, result)
	if err == nil && res != nil && res.IsError && len(res.Content) > 0 && name == gitops.Push &&
		result.Git != nil && workflowRefusal.MatchString(result.Git.Error) {
		res.Content[0] = strings.TrimRight(res.Content[0], " ") + " " + workflowWords(readAccess(caller.Access), args)
	}
	if err != nil || res == nil || res.IsError || len(res.Content) == 0 {
		return res, err
	}
	if r := result.Git; name == gitops.Clone && readAccess(caller.Access).write && r != nil && r.Error == "" && !r.Identity {
		res.Content[0] += noIdentity
	}
	return res, nil
}

// workflowWords follow the answer of a push GitHub refused for changing
// a workflow file: push again saying so, when the access allows it and
// the push did not say so; ask the owner for the switch, when the
// access does not allow it; or, when the push said so and still was
// refused, ask the owner to approve the app's workflows permission on
// GitHub.
func workflowWords(acc access, args json.RawMessage) string {
	var a struct {
		Workflows bool `json:"workflows"`
	}
	_ = json.Unmarshal(args, &a)
	switch {
	case !acc.pushesWorkflows():
		return "The commits change a workflow file (.github/workflows), and your access to GitHub does not include changing workflows; ask the owner if your work needs it."
	case !a.Workflows:
		return "The commits change a workflow file (.github/workflows): push again with workflows set to true."
	}
	return "GitHub refused the GitHub App's token the workflows permission; ask the owner to approve it for the app's installation on GitHub."
}

// noIdentity follows the answer of a writer's clone that landed without
// the bot's identity.
const noIdentity = " The working copy has no git identity of the GitHub App's bot, so commits you make there will not read as the app's, and github_delete_branch will not delete a branch that holds them."
