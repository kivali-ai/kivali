package main

import (
	"os"
	"regexp"
	"testing"
)

// argClaudeCodeVersion pulls the default value of the Dockerfile's
// CLAUDE_CODE_VERSION build arg. Anchored to the start of a line so a
// mention inside a comment or a --build-arg example can't match.
var argClaudeCodeVersion = regexp.MustCompile(`(?m)^ARG CLAUDE_CODE_VERSION=(\S*)\s*$`)

// concreteVersion is npm's semver, optionally with a prerelease tag.
// Deliberately does not accept dist-tags.
var concreteVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// TestClaudeCodeVersionIsPinned guards the one Dockerfile arg that can
// rot without anything failing.
//
// The npm install line in the claude-build stage is the entire cache key
// for that layer. A dist-tag like `latest` is a fixed string, so the key
// never changes, the layer is never rebuilt, and every image forever
// after carries whichever release was current the first time someone
// built it — the exact opposite of what "latest" reads as. The CLI
// validates --model against a list compiled into its binary, so agents
// pinned to any model newer than that build would fail with "please
// upgrade" while the picker offers them.
//
// A concrete version is what makes the bump visible in a diff and what
// busts the layer when it changes.
func TestClaudeCodeVersionIsPinned(t *testing.T) {
	b, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	m := argClaudeCodeVersion.FindSubmatch(b)
	if m == nil {
		t.Fatal("no `ARG CLAUDE_CODE_VERSION=...` line in Dockerfile; if the arg was renamed, update this test rather than deleting it")
	}

	got := string(m[1])
	if !concreteVersion.MatchString(got) {
		t.Errorf("CLAUDE_CODE_VERSION = %q, want a concrete version like 2.1.258.\n"+
			"Dist-tags (latest, next, beta) freeze the image on the first build instead of tracking the tag —\n"+
			"the RUN line never changes, so Docker never rebuilds the layer.\n"+
			"Bump this deliberately, and keep it at or above the release that introduced the newest model ID\n"+
			"in internal/claudeagent/catalog.go.", got)
	}
}
