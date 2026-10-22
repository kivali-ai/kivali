package debpkg

import (
	"strings"
	"testing"
)

// Every name this rule passes goes to apt-get as an argument, as root,
// from the agent shell and from an integration's pod alike. A name it
// wrongly passed could be an option ("-y"), a removal ("git-"), a
// pinned version or a path; one it wrongly refused would keep an
// owner's real package out. This fails on either.
func TestValidName(t *testing.T) {
	for _, ok := range []string{"git", "gh", "poppler-utils", "libc++-dev", "python3.11", "g++", "7zip", "a1", "c+", "a.", strings.Repeat("a", MaxNameLen)} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{
		"", "a", "x", "-y", "--force-yes", "git=1.0", "git/stable", "../etc", "Git", "git status", "git;rm", "git\nfoo",
		".hidden", "+x", "naïve", strings.Repeat("a", MaxNameLen+1),
		"git-", "libc++-", "a-",
	} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}
