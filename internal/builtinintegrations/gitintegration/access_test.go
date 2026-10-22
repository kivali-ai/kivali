package gitintegration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Why this test exists: the access is what decides which repositories
// an agent reaches with the owner's token, and core reads it from a
// stored JSON document. A clone may reach a repository in either list,
// pull or push. An access whose lists are missing, empty, or hold
// something other than patterns (in either list) must grant nothing,
// not the valid part of it, and the patterns that are there must match
// as the owner wrote them: exact names exactly, * within one segment,
// case ignored.
func TestRepositoryAccess(t *testing.T) {
	g := New()
	for _, c := range []struct {
		name   string
		access map[string]any
		repo   string
		want   bool
	}{
		{"exact", map[string]any{"pull_repositories": []any{"acme/widgets"}}, "acme/widgets", true},
		{"exact, other case", map[string]any{"pull_repositories": []any{"ACME/Widgets"}}, "acme/widgets", true},
		{"exact, other repository", map[string]any{"pull_repositories": []any{"acme/widgets"}}, "acme/gadgets", false},
		{"pattern", map[string]any{"pull_repositories": []any{"acme/*"}}, "acme/gadgets", true},
		{"pattern across a slash", map[string]any{"pull_repositories": []any{"acme/*"}}, "acme/sub/gadgets", false},
		{"nested group", map[string]any{"pull_repositories": []any{"group/sub/*"}}, "group/sub/project", true},
		{"one of several", map[string]any{"pull_repositories": []any{"x/y", "acme/*"}}, "acme/widgets", true},
		{"in the push list", map[string]any{"push_repositories": []any{"acme/widgets"}}, "acme/widgets", true},
		{"in neither list", map[string]any{"pull_repositories": []any{"x/y"}, "push_repositories": []any{"acme/widgets"}}, "acme/gadgets", false},
		{"no field", map[string]any{}, "acme/widgets", false},
		{"nil access", nil, "acme/widgets", false},
		{"empty lists", map[string]any{"pull_repositories": []any{}, "push_repositories": []any{}}, "acme/widgets", false},
		{"not a list", map[string]any{"pull_repositories": "acme/*"}, "acme/widgets", false},
		{"push not a list", map[string]any{"pull_repositories": []any{"acme/*"}, "push_repositories": true}, "acme/widgets", false},
		{"a number among them", map[string]any{"pull_repositories": []any{"acme/*", 7}}, "acme/widgets", false},
		{"an empty entry among them", map[string]any{"pull_repositories": []any{"acme/*", ""}}, "acme/widgets", false},
		{"a bad pattern among them", map[string]any{"pull_repositories": []any{"acme/*", "acme/[x"}}, "acme/widgets", false},
		{"a bad pattern in the other list", map[string]any{"pull_repositories": []any{"acme/*"}, "push_repositories": []any{"acme/[x"}}, "acme/widgets", false},
		{"only the old fields", map[string]any{"repositories": []any{"acme/*"}, "push": true}, "acme/widgets", false},
	} {
		args, _ := json.Marshal(map[string]string{"url": c.repo, "path": "w"})
		_, err := g.Call(context.Background(), builtinintegrations.Caller{Agent: "dana", Access: c.access}, testConn("github.com"), "clone", args)
		var ref *builtinintegrations.Refusal
		if got := err == nil; got != c.want || (err != nil && !errors.As(err, &ref)) {
			t.Errorf("%s: clone = %v, want allowed %v", c.name, err, c.want)
		}
	}
}

// Why this test exists: the summary is how the owner reads an agent's
// Git access on the Org page. Each repository must read under what the
// agent may do with it, pull or push; the branches, tags and force mean
// something only with a repository to push to, so refs or force set
// without one, or force with no ref to push, must not read as granted;
// and a full grant must read as one line: "Pull: acme/* · Push:
// acme/widgets · Branches and tags: agent/* · May force-push".
func TestAccessSummary(t *testing.T) {
	g := New()
	for _, c := range []struct {
		access map[string]any
		want   string
	}{
		{map[string]any{"pull_repositories": []any{"acme/*"}, "push_repositories": []any{"acme/widgets"}, "push_refs": []any{"agent/*"}, "force": true},
			"Pull: acme/* · Push: acme/widgets · Branches and tags: agent/* · May force-push"},
		{map[string]any{"push_repositories": []any{"acme/*", "x/y"}, "push_refs": []any{"agent/*", "tags/v*"}}, "Push: acme/*, x/y · Branches and tags: agent/*, tags/v*"},
		{map[string]any{"pull_repositories": []any{"acme/*"}, "push_refs": []any{"agent/*"}, "force": true}, "Pull: acme/*"},
		{map[string]any{"pull_repositories": []any{"acme/*"}, "push_repositories": []any{}, "push_refs": []any{"agent/*"}}, "Pull: acme/*"},
		{map[string]any{"push_repositories": []any{"acme/widgets"}, "force": true}, "Push: acme/widgets · Branches and tags: none"},
		{map[string]any{"pull_repositories": []any{"acme/*", "x/y"}}, "Pull: acme/*, x/y"},
		{map[string]any{}, "Nothing set"},
	} {
		if got := g.AccessSummary(c.access); got != c.want {
			t.Errorf("AccessSummary(%v) = %q, want %q", c.access, got, c.want)
		}
	}
}

// Why this test exists: the access is read at the agent's call, where
// an entry that does not read refuses every call without saying why to
// the owner. The owner must be told when saving: each repository, in
// either list, must be a repository or a pattern; at least one must be
// named, since the form cannot require one of two lists; and each of
// push_refs must be a branch, a tag or a pattern with * as the only
// wildcard. Each error names the entry.
func TestCheckAccess(t *testing.T) {
	g := New()
	for _, ok := range []map[string]any{
		{"pull_repositories": []any{"acme/widgets", "acme/*"}},
		{"push_repositories": []any{"acme/widgets"}},
		{"pull_repositories": []any{"acme/*"}, "push_repositories": []any{"acme/widgets"}, "push_refs": []any{"agent/*", "tags/v*", "*", "main"}, "force": true},
	} {
		if err := g.CheckAccess(ok); err != nil {
			t.Errorf("%v = %v", ok, err)
		}
	}
	for _, key := range []string{"pull_repositories", "push_repositories"} {
		if err := g.CheckAccess(map[string]any{key: []any{"acme/*", "acme/[x"}}); err == nil || err.Error() != "acme/[x is not a repository or a pattern" {
			t.Errorf("a bad pattern in %s = %v", key, err)
		}
	}
	for _, none := range []map[string]any{{}, {"pull_repositories": []any{}, "push_repositories": []any{}}, {"push_refs": []any{"agent/*"}}} {
		if err := g.CheckAccess(none); err == nil || err.Error() != "Name a repository it may pull or push to" {
			t.Errorf("%v = %v", none, err)
		}
	}
	for _, bad := range []string{"refs/notes/*", "agent[^x]*", "agent/?", "a b", "a..b", "tags/", ""} {
		access := map[string]any{"push_repositories": []any{"acme/*"}, "push_refs": []any{"agent/*", bad}}
		if err := g.CheckAccess(access); err == nil || err.Error() != bad+" is not a branch, a tag or a pattern" {
			t.Errorf("push_refs %q = %v", bad, err)
		}
	}
}
