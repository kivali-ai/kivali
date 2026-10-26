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
	{Name: gitops.Clone, Description: "Clone a repository the agent's access names into its files, with a token for that repository alone, its git identity the GitHub App's bot. Reads only."},
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
// refused call mints nothing. A clone's job also names the app's bot
// user as the working copy's git identity.
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
	// A clone's working copy is given the app's bot as its identity, so
	// what the agent commits there is the bot's (bot.go).
	var identity *shelljob.GitIdentity
	if name == gitops.Clone {
		b, err := g.bot(ctx, c, repo.Path)
		if err != nil {
			return gitFailure(err)
		}
		identity = &shelljob.GitIdentity{Name: b.login, Email: b.noreply()}
	}
	tok, err := g.jobToken(ctx, c, repo.Path, jobPermissions(name))
	if err != nil {
		return gitFailure(err)
	}
	job := call.Job(shelljob.GitAuth{Username: tokenUser, Password: tok}, conn.ID)
	job.Identity = identity
	return &builtinintegrations.Result{ShellJob: &shelljob.Job{Git: job}, Audit: call.Started()}, nil
}

// gitFailure is err of a clone's, a fetch's or a push's first request:
// a Refusal as it is, GitHub's failure an error result in its words.
func gitFailure(err error) (*builtinintegrations.Result, error) {
	var ref *builtinintegrations.Refusal
	if errors.As(err, &ref) {
		return nil, err
	}
	if words, ok := failureWords(err); ok {
		return failed(words), nil
	}
	return nil, err
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
// connection as they are at the second request (gitops.Complete). It
// mints no token.
func (g *GitHub) Complete(_ context.Context, caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string, args json.RawMessage, result shelljob.Result) (*builtinintegrations.Result, error) {
	p, _ := gitPolicy(caller, conn, name)
	return gitops.Complete(name, args, p, conn.ID, result)
}
