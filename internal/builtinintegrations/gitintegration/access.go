package gitintegration

import (
	"errors"
	"fmt"

	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// accessKey is the access form's one field: the repositories an agent
// may clone and fetch.
const accessKey = "repositories"

// errNoRepositories is an access that names no repository Kivali can
// read: missing, empty, or holding anything but patterns.
var errNoRepositories = errors.New("the access names no repositories")

// CheckAccess holds an agent's access, as the owner saves it, to
// repositories gitrepo.Match can read: each entry a repository or a
// pattern. The form has made sure the entries are strings.
func (g *Git) CheckAccess(access map[string]any) error {
	raw, _ := access[accessKey].([]any)
	for _, v := range raw {
		if s, _ := v.(string); !gitrepo.ValidPattern(s) {
			return fmt.Errorf("%v is not a repository or a pattern", v)
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
