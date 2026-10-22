package gitintegration

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// Why this test exists: the access is what decides which repositories
// an agent reaches with the owner's token, and core reads it from a
// stored JSON document. An access whose repositories are missing,
// empty, or hold something other than patterns must grant nothing
// (not the valid part of it), and the patterns that are there must
// match as the owner wrote them: exact names exactly, * within one
// segment, case ignored.
func TestRepositoryAccess(t *testing.T) {
	repo := func(p string) gitrepo.Repository {
		return gitrepo.Repository{Scheme: "https", Host: "github.com", Path: p}
	}
	for _, c := range []struct {
		name   string
		access map[string]any
		repo   string
		want   bool
	}{
		{"exact", map[string]any{"repositories": []any{"acme/widgets"}}, "acme/widgets", true},
		{"exact, other case", map[string]any{"repositories": []any{"ACME/Widgets"}}, "acme/widgets", true},
		{"exact, other repository", map[string]any{"repositories": []any{"acme/widgets"}}, "acme/gadgets", false},
		{"pattern", map[string]any{"repositories": []any{"acme/*"}}, "acme/gadgets", true},
		{"pattern across a slash", map[string]any{"repositories": []any{"acme/*"}}, "acme/sub/gadgets", false},
		{"nested group", map[string]any{"repositories": []any{"group/sub/*"}}, "group/sub/project", true},
		{"one of several", map[string]any{"repositories": []any{"x/y", "acme/*"}}, "acme/widgets", true},
		{"no field", map[string]any{}, "acme/widgets", false},
		{"nil access", nil, "acme/widgets", false},
		{"empty list", map[string]any{"repositories": []any{}}, "acme/widgets", false},
		{"not a list", map[string]any{"repositories": "acme/*"}, "acme/widgets", false},
		{"a number among them", map[string]any{"repositories": []any{"acme/*", 7}}, "acme/widgets", false},
		{"an empty entry among them", map[string]any{"repositories": []any{"acme/*", ""}}, "acme/widgets", false},
		{"a bad pattern among them", map[string]any{"repositories": []any{"acme/*", "acme/[x"}}, "acme/widgets", false},
	} {
		if got := accessIncludes(c.access, repo(c.repo)); got != c.want {
			t.Errorf("%s: accessIncludes = %v, want %v", c.name, got, c.want)
		}
	}
}
