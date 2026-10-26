package githubintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// token is narrowed to it, and no force switch: GitHub never
// force-pushes.
var gitIntegration = gitops.Integration{Name: Name, DisplayName: "GitHub", HostWords: gitHost, NameRepository: true}

// gitTools are clone, fetch and push as the manifest declares them,
// for the owner: push needs the write switch.
var gitTools = []integration.Tool{
	{Name: gitops.Clone, Description: "Clone a repository the agent's access names into its files, with a token for that repository alone; when the access allows writing, its git identity is the GitHub App's bot. Reads only."},
	{Name: gitops.Fetch, Description: "Fetch new commits and tags into the agent's working copy of a repository its access names, with a token for that repository alone. Reads only."},
	{Name: gitops.Push, Description: "Push branches and tags to a repository the agent's access names, with a token for that repository alone. Never forced, never deleting; GitHub's branch protection applies." + needsWrite, Access: writeKey},
}

var (
	_ builtinintegrations.ShellJobIntegration = (*GitHub)(nil)
	_ builtinintegrations.HostsOf             = (*GitHub)(nil)
)

// Hosts is github.com, which a clone, a fetch or a push reaches: what
// the connection's scoped proxy credential admits.
func (g *GitHub) Hosts(builtinintegrations.Connection) []string { return []string{gitHost} }

// gitPolicy is what caller may do through conn with the clone, fetch
// or push name, and the connection's values: on github.com, the
// access's repositories to clone and fetch, and with write to push to
// as well, any branch or tag, never forced. A push without write is
// refused as every write action is, and a connection setup has not
// finished refuses every call.
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
	return p, c
}

// callGit is a clone's, a fetch's or a push's first request: the call
// checked (gitops.Check), then a token minted for its job alone
// (jobToken), narrowed to the call's repository and to reading its
// contents, or for a push writing them; a repository under another
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
	tok, err := g.jobToken(ctx, c, repo.Path, jobPermissions(name))
	if err != nil {
		var te *tokenError
		if errors.As(err, &te) {
			return failed(te.words()), nil
		}
		return nil, err
	}
	job := call.Job(shelljob.GitAuth{Username: tokenUser, Password: tok}, conn.ID)
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
// metadata read: write the repository's contents for a push, read them
// for a clone or a fetch.
func jobPermissions(name string) permissions {
	if name == gitops.Push {
		return perms("contents", "write")
	}
	return perms("contents", "read")
}

// Complete puts the shell's result in words for the agent and the
// audit, with the call checked again under the access and the
// connection as they are at the second request (gitops.Complete). A
// writer's clone that landed without the bot's identity (GitHub did not
// give the bot user, or the shell could not write it) says commits made
// there will not read as the app's. It mints no token.
func (g *GitHub) Complete(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage, result shelljob.Result) (*builtinintegrations.Result, error) {
	p, _ := gitPolicy(caller, conn, name)
	res, err := gitops.Complete(name, args, p, conn.ID, result)
	if err != nil || res == nil || res.IsError || len(res.Content) == 0 {
		return res, err
	}
	if r := result.Git; name == gitops.Clone && readAccess(caller.Access).write && r != nil && r.Error == "" && !r.Identity {
		res.Content[0] += noIdentity
	}
	return res, nil
}

// noIdentity follows the answer of a writer's clone that landed without
// the bot's identity.
const noIdentity = " The working copy has no git identity of the GitHub App's bot, so commits you make there will not read as the app's, and github_delete_branch will not delete a branch that holds them."
