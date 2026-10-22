package gitrepo

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Why this test exists: a push's destination is what the owner's
// push_refs are checked against, so a name must mean one ref and only
// one. A bare name read as a tag, a tag without its prefix read as one,
// or a destination outside refs/heads/ and refs/tags/ (refs/notes,
// refs/remotes, a name git refuses) let through, would let an agent
// write a ref the owner never named.
func TestNormalizeRef(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"agent/x", "refs/heads/agent/x"},
		{" main ", "refs/heads/main"},
		{"v1.2.0", "refs/heads/v1.2.0"},
		{"tags/v1.2.0", "refs/tags/v1.2.0"},
		{"refs/tags/v1.2.0", "refs/tags/v1.2.0"},
		{"refs/heads/agent/x", "refs/heads/agent/x"},
		{"heads/x", "refs/heads/heads/x"},
		{"Feature/X", "refs/heads/Feature/X"},
	} {
		if got, err := NormalizeRef(c.in); err != nil || got != c.want {
			t.Errorf("NormalizeRef(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"", " ", "HEAD", "refs/heads/HEAD", "-x", "tags/", "refs/heads/", "refs/notes/x", "refs/remotes/origin/main",
		"refs/pull/1/head", "a..b", "a b", "a~1", "a^", "a:b", `a\b`, "a//b", "a/", "a.", ".a", "a/.b", "a.lock",
		"a@{1}", "agent/*", "a?", "a[b]", "a\x01",
	} {
		if got, err := NormalizeRef(bad); err == nil {
			t.Errorf("NormalizeRef(%q) = %q, want an error", bad, got)
		}
	}
}

// Why this test exists: push_refs are the owner's words for what an
// agent may push. * must be every branch and every tag, a bare pattern
// branches alone, tags/ tags alone; a * must never cross a / (agent/*
// is not agent/a/b), case must count as git's does, and a pattern
// that does not read must grant nothing.
func TestMatchRef(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		ref      string
		want     bool
	}{
		{[]string{"agent/*"}, "refs/heads/agent/x", true},
		{[]string{"agent/*"}, "refs/heads/agent/a/b", false},
		{[]string{"agent/*/*"}, "refs/heads/agent/a/b", true},
		{[]string{"agent/*"}, "refs/tags/agent/x", false},
		{[]string{"agent/*"}, "refs/heads/Agent/x", false},
		{[]string{"main"}, "refs/heads/main", true},
		{[]string{"main"}, "refs/heads/main2", false},
		{[]string{"v*"}, "refs/tags/v1", false},
		{[]string{"v*"}, "refs/heads/v1", true},
		{[]string{"tags/v*"}, "refs/tags/v1.2.0", true},
		{[]string{"tags/v*"}, "refs/heads/v1", false},
		{[]string{"refs/tags/*"}, "refs/tags/v1", true},
		{[]string{"*"}, "refs/heads/main", true},
		{[]string{"*"}, "refs/tags/v1", true},
		{[]string{"*"}, "refs/heads/a/b", false},
		{[]string{"*"}, "refs/notes/x", false},
		{[]string{"refs/heads/*", "refs/tags/*"}, "refs/tags/v1", true},
		{[]string{"x/[", "agent/*"}, "refs/heads/agent/x", true},
		// * is the only wildcard: a class, which path.Match lets match
		// a /, and ? are refused and match nothing.
		{[]string{"agent[^x]*"}, "refs/heads/agent/y", false},
		{[]string{"agent/?"}, "refs/heads/agent/y", false},
		{[]string{"refs/notes/*"}, "refs/notes/x", false},
		{nil, "refs/heads/main", false},
	} {
		if got := MatchRef(c.patterns, c.ref); got != c.want {
			t.Errorf("MatchRef(%q, %q) = %v, want %v", c.patterns, c.ref, got, c.want)
		}
	}
	if got, err := NormalizeRefPatterns([]string{"*", "agent/*", "tags/v*"}); err != nil ||
		!slices.Equal(got, []string{"refs/heads/*", "refs/tags/*", "refs/heads/agent/*", "refs/tags/v*"}) {
		t.Errorf("NormalizeRefPatterns = %q, %v", got, err)
	}
	for _, bad := range []string{"", "refs/notes/*", "a[", "a b", "a..b", "tags/", "agent[^x]*", "agent/?", "agent/[ab]"} {
		if _, err := NormalizeRefPatterns([]string{"agent/*", bad}); err == nil {
			t.Errorf("NormalizeRefPatterns with %q accepted", bad)
		}
	}
}

// Why this test exists: an agent's refs become the refspecs the daemon
// pushes with the owner's token. A bare ref must push itself, a
// refspec's source may be HEAD, a hash or any ref but its destination
// only a branch or a tag, + must be read as force (so the force
// permission is checked), and an empty source, git's way to delete the
// remote ref, must always be ErrDeleteRef.
func TestParsePushRefSpec(t *testing.T) {
	hash := strings.Repeat("ab", 20)
	for _, c := range []struct {
		in   string
		want PushRefSpec
	}{
		{"agent/x", PushRefSpec{Src: "refs/heads/agent/x", Dst: "refs/heads/agent/x"}},
		{"tags/v1.2.0", PushRefSpec{Src: "refs/tags/v1.2.0", Dst: "refs/tags/v1.2.0"}},
		{"+agent/x", PushRefSpec{Src: "refs/heads/agent/x", Dst: "refs/heads/agent/x", Force: true}},
		{"HEAD:refs/heads/agent/x", PushRefSpec{Src: "HEAD", Dst: "refs/heads/agent/x"}},
		{"HEAD:agent/x", PushRefSpec{Src: "HEAD", Dst: "refs/heads/agent/x"}},
		{"refs/heads/a:refs/heads/b", PushRefSpec{Src: "refs/heads/a", Dst: "refs/heads/b"}},
		{"a:b", PushRefSpec{Src: "refs/heads/a", Dst: "refs/heads/b"}},
		{"refs/remotes/origin/main:agent/x", PushRefSpec{Src: "refs/remotes/origin/main", Dst: "refs/heads/agent/x"}},
		{"+" + hash + ":tags/v2", PushRefSpec{Src: hash, Dst: "refs/tags/v2", Force: true}},
	} {
		got, err := ParsePushRefSpec(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParsePushRefSpec(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	if s := (PushRefSpec{Src: "HEAD", Dst: "refs/heads/x", Force: true}).String(); s != "+HEAD:refs/heads/x" {
		t.Errorf("String = %q", s)
	}
	for _, del := range []string{":refs/heads/main", "+:main", " :x"} {
		if _, err := ParsePushRefSpec(del); !errors.Is(err, ErrDeleteRef) {
			t.Errorf("ParsePushRefSpec(%q) = %v, want ErrDeleteRef", del, err)
		}
	}
	for _, bad := range []string{
		"", "+", "HEAD", hash, "a:", "a:refs/notes/x", "a:refs/remotes/origin/a", "refs/heads/*:refs/heads/*",
		"a:b*", "^a", "a..b:c", "HEAD:HEAD", "a:-b", strings.Repeat("a", 600),
	} {
		if got, err := ParsePushRefSpec(bad); err == nil || errors.Is(err, ErrDeleteRef) {
			t.Errorf("ParsePushRefSpec(%q) = %+v, %v; want an error", bad, got, err)
		}
	}
}
