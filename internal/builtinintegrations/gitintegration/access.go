package gitintegration

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/gitrepo"
	"github.com/kivali-ai/kivali/internal/integration"
)

// The access form's fields: the repositories an agent may clone, fetch
// and push to; whether it may push; the branches and tags it may push;
// whether it may force-push.
const (
	accessKey   = "repositories"
	pushKey     = "push"
	pushRefsKey = "push_refs"
	forceKey    = "force"
)

// errNoRepositories is an access that names no repository Kivali can
// read: missing, empty, or holding anything but patterns.
var errNoRepositories = errors.New("the access names no repositories")

// errBadPushRefs is an access whose push_refs hold anything but
// patterns of branches and tags.
var errBadPushRefs = errors.New("the access's branches and tags do not read")

// CheckAccess holds an agent's access, as the owner saves it, to
// patterns Kivali can read: each repository a repository or a pattern
// (gitrepo.Match), each of push_refs a branch, a tag or a pattern
// (gitrepo.NormalizeRefPattern). The form has made sure the entries are
// strings.
func (g *Git) CheckAccess(access map[string]any) error {
	raw, _ := access[accessKey].([]any)
	for _, v := range raw {
		if s, _ := v.(string); !gitrepo.ValidPattern(s) {
			return fmt.Errorf("%v is not a repository or a pattern", v)
		}
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

// repositoryPatterns is the access's repositories: a list of one or
// more strings, each a pattern gitrepo.Match reads (owner/name, or a
// pattern such as acme/*). The API refuses an access that is not this
// when it is saved (CheckAccess, and the form's own check); a stored
// row that is anything else all the same refuses every repository,
// rather than granting what the rest names.
func repositoryPatterns(access map[string]any) ([]string, error) {
	raw, ok := access[accessKey].([]any)
	if !ok || len(raw) == 0 {
		return nil, errNoRepositories
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok || !gitrepo.ValidPattern(s) {
			return nil, errNoRepositories
		}
		out = append(out, s)
	}
	return out, nil
}

// accessIncludes reports whether the access lets its agent reach repo.
func accessIncludes(access map[string]any, repo gitrepo.Repository) bool {
	patterns, err := repositoryPatterns(access)
	return err == nil && gitrepo.Match(patterns, repo.Key())
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
// repositories, then what it may push and whether it may force, which
// mean something only together: "Repositories: acme/* · May push:
// agent/* · May force-push". push_refs without push, and force without
// a ref to push, are left out, since they grant nothing.
func (g *Git) AccessSummary(access map[string]any) string {
	var parts []string
	if repos := strings.Join(accessStrings(access, accessKey), ", "); repos != "" {
		parts = append(parts, "Repositories: "+repos)
	}
	if accessAllows(access, pushKey) {
		refs := accessStrings(access, pushRefsKey)
		if len(refs) == 0 {
			parts = append(parts, "May push: no branches or tags")
		} else {
			parts = append(parts, "May push: "+strings.Join(refs, ", "))
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
