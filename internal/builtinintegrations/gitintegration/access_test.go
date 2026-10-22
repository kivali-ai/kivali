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

// Why this test exists: the summary is how the owner reads an agent's
// Git access on the Org page. Push, its refs and force mean something
// only together, so refs set while push is off, or force with no ref
// to push, must not read as granted, and a full grant must read as one
// line: "Repositories: acme/* · May push: agent/* · May force-push".
func TestAccessSummary(t *testing.T) {
	g := New()
	for _, c := range []struct {
		access map[string]any
		want   string
	}{
		{map[string]any{"repositories": []any{"acme/*"}, "push": true, "push_refs": []any{"agent/*"}, "force": true}, "Repositories: acme/* · May push: agent/* · May force-push"},
		{map[string]any{"repositories": []any{"acme/*", "x/y"}, "push": true, "push_refs": []any{"agent/*", "tags/v*"}}, "Repositories: acme/*, x/y · May push: agent/*, tags/v*"},
		{map[string]any{"repositories": []any{"acme/*"}, "push": false, "push_refs": []any{"agent/*"}, "force": true}, "Repositories: acme/*"},
		{map[string]any{"repositories": []any{"acme/*"}, "push": true, "force": true}, "Repositories: acme/* · May push: no branches or tags"},
		{map[string]any{"repositories": []any{"acme/*"}}, "Repositories: acme/*"},
		{map[string]any{}, "Nothing set"},
	} {
		if got := g.AccessSummary(c.access); got != c.want {
			t.Errorf("AccessSummary(%v) = %q, want %q", c.access, got, c.want)
		}
	}
}

// Why this test exists: push_refs are read at the agent's call, where
// an entry that does not read refuses every push without saying why to
// the owner. The owner must be told when saving: each entry must be a
// branch, a tag or a pattern with * as the only wildcard, and the
// error names the entry.
func TestCheckAccessPushRefs(t *testing.T) {
	g := New()
	ok := map[string]any{"repositories": []any{"acme/*"}, "push": true, "push_refs": []any{"agent/*", "tags/v*", "*", "main"}}
	if err := g.CheckAccess(ok); err != nil {
		t.Errorf("good push_refs = %v", err)
	}
	for _, bad := range []string{"refs/notes/*", "agent[^x]*", "agent/?", "a b", "a..b", "tags/", ""} {
		access := map[string]any{"repositories": []any{"acme/*"}, "push_refs": []any{"agent/*", bad}}
		if err := g.CheckAccess(access); err == nil || err.Error() != bad+" is not a branch, a tag or a pattern" {
			t.Errorf("push_refs %q = %v", bad, err)
		}
	}
}
