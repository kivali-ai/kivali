package gitintegration

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitops"
	"github.com/kivali-ai/kivali/internal/gitrepo"
	"github.com/kivali-ai/kivali/internal/integration"
)

// policy is what caller may do through conn with the action name: the
// connection's host, the access's repositories to pull and to push to,
// and for a push the branches and tags it may write and whether it may
// force. A connection without a usable host or token, or an access
// whose repositories (or, for a push to a repository it may push to,
// whose push_refs) do not read, grants nothing: the policy's Refused
// says why, in the order a call meets them.
func (g *Git) policy(caller builtinintegrations.Caller, conn builtinintegrations.Connection, name string) gitops.Policy {
	p := gitops.Policy{Integration: g.integration(), InsecureHTTP: g.InsecureHTTP}
	host, err := gitrepo.NormalizeHost(conn.Fields["host"])
	if err != nil {
		p.Refused = &builtinintegrations.Refusal{Text: "The " + displayName + " integration's connection has no host Kivali can use. Ask the owner to fix the connection.",
			Reason: "the connection's host is not a host name"}
		return p
	}
	p.Host = host
	if conn.Fields["token"] == "" {
		p.Refused = &builtinintegrations.Refusal{Text: "The " + displayName + " integration's connection has no token. Ask the owner to connect it again.",
			Reason: "the connection has no token"}
		return p
	}
	pull, push, err := repositoryPatterns(caller.Access)
	if err != nil {
		p.Refused = &builtinintegrations.Refusal{Text: "Your access to the " + displayName + " integration names no repositories Kivali can read. Ask the owner to set it again.",
			Reason: err.Error()}
		return p
	}
	p.Pull, p.Push = pull, push
	p.Force = accessAllows(caller.Access, forceKey)
	// push_refs mean something only to a push with a repository to push
	// to: a clone or a fetch is not refused for them, and a push with
	// none is refused for that first.
	if name == gitops.Push && len(push) > 0 {
		refs, err := pushRefPatterns(caller.Access)
		if err != nil {
			p.Refused = &builtinintegrations.Refusal{Text: "Your access to the " + displayName + " integration names branches or tags Kivali cannot read. Ask the owner to set it again.",
				Reason: err.Error()}
			return p
		}
		p.Refs = refs
	}
	return p
}

// The access form's fields: the repositories an agent may pull (clone
// and fetch); the repositories it may push to, which it may pull too;
// the branches and tags it may push there; whether it may force-push.
const (
	pullKey     = "pull_repositories"
	pushKey     = "push_repositories"
	pushRefsKey = "push_refs"
	forceKey    = "force"
)

// errNoRepositories is an access that names no repository Kivali can
// read: both lists missing or empty, or either holding anything but
// patterns.
var errNoRepositories = errors.New("the access names no repositories")

// errNameARepository is CheckAccess's refusal of an access that names
// no repository in either list.
const errNameARepository = ownerWords("Name a repository it may pull or push to")

// errBadPushRefs is an access whose push_refs hold anything but
// patterns of branches and tags.
var errBadPushRefs = errors.New("the access's branches and tags do not read")

// CheckAccess holds an agent's access, as the owner saves it, to
// patterns Kivali can read: at least one repository to pull or push
// to, each a repository or a pattern (gitrepo.Match), and each of
// push_refs a branch, a tag or a pattern
// (gitrepo.NormalizeRefPattern). The form has made sure the entries
// are strings.
func (g *Git) CheckAccess(access map[string]any) error {
	named := false
	for _, key := range []string{pullKey, pushKey} {
		raw, _ := access[key].([]any)
		for _, v := range raw {
			if s, _ := v.(string); !gitrepo.ValidPattern(s) {
				return fmt.Errorf("%v is not a repository or a pattern", v)
			}
			named = true
		}
	}
	if !named {
		return errNameARepository
	}
	refs, _ := access[pushRefsKey].([]any)
	for _, v := range refs {
		s, _ := v.(string)
		if _, err := gitrepo.NormalizeRefPattern(s); err != nil {
			return fmt.Errorf("%v is not a branch, a tag or a pattern", v)
		}
	}
	return nil
}

// repositoryPatterns is the access's repositories: pull, the ones it
// may pull only, and push, the ones it may push to (and pull). Each
// list is missing, or a list of strings that gitrepo.Match reads
// (owner/name, or a pattern such as acme/*), and together they name at
// least one. The API refuses an access that is not this when it is
// saved (CheckAccess, and the form's own check); a stored row that is
// anything else all the same refuses every repository, rather than
// granting what the rest names.
func repositoryPatterns(access map[string]any) (pull, push []string, err error) {
	read := func(key string) ([]string, bool) {
		v, ok := access[key]
		if !ok || v == nil {
			return nil, true
		}
		raw, ok := v.([]any)
		if !ok {
			return nil, false
		}
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			s, ok := v.(string)
			if !ok || !gitrepo.ValidPattern(s) {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	pull, okPull := read(pullKey)
	push, okPush := read(pushKey)
	if !okPull || !okPush || len(pull)+len(push) == 0 {
		return nil, nil, errNoRepositories
	}
	return pull, push, nil
}

// accessAllows reports whether the access's switch key is on: true,
// and nothing else.
func accessAllows(access map[string]any, key string) bool {
	on, _ := access[key].(bool)
	return on
}

// pushRefPatterns is the access's push_refs, normalised
// (gitrepo.NormalizeRefPatterns): none when it is missing or empty, so
// no ref may be pushed. Anything but strings that read as patterns
// refuses every ref.
func pushRefPatterns(access map[string]any) ([]string, error) {
	v, ok := access[pushRefsKey]
	if !ok || v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, errBadPushRefs
	}
	in := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, errBadPushRefs
		}
		in = append(in, s)
	}
	out, err := gitrepo.NormalizeRefPatterns(in)
	if err != nil {
		return nil, errBadPushRefs
	}
	return out, nil
}

// AccessSummary is the access in words for the Org page: the
// repositories it may pull, the ones it may push to, then the branches
// and tags it may push there and whether it may force, which mean
// something only together: "Pull: acme/* · Push: acme/widgets ·
// Branches and tags: agent/* · May force-push". push_refs and force
// without a repository to push to, and force without a ref to push,
// are left out, since they grant nothing.
func (g *Git) AccessSummary(access map[string]any) string {
	var parts []string
	if pull := accessStrings(access, pullKey); len(pull) > 0 {
		parts = append(parts, "Pull: "+strings.Join(pull, ", "))
	}
	if push := accessStrings(access, pushKey); len(push) > 0 {
		parts = append(parts, "Push: "+strings.Join(push, ", "))
		refs := accessStrings(access, pushRefsKey)
		if len(refs) == 0 {
			parts = append(parts, "Branches and tags: none")
		} else {
			parts = append(parts, "Branches and tags: "+strings.Join(refs, ", "))
			if accessAllows(access, forceKey) {
				parts = append(parts, "May force-push")
			}
		}
	}
	if len(parts) == 0 {
		return integration.SummaryNothingSet
	}
	return strings.Join(parts, " · ")
}

// accessStrings is the strings of the access's list key, as written.
func accessStrings(access map[string]any, key string) []string {
	raw, _ := access[key].([]any)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
