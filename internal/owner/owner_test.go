package owner

import (
	"strings"
	"testing"
)

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{"": "", "  Mom ": "Mom", "Dr. Ada Lovelace": "Dr. Ada Lovelace", "Human Overlord": "Human Overlord", strings.Repeat("é", 40): strings.Repeat("é", 40)} {
		got, err := CleanName(in)
		if err != nil || got != want {
			t.Errorf("CleanName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		strings.Repeat("x", 41), "Mom\nDad", "a\tb",
		"Mom\u2028Dad", // line separator (Zl)
		"Mom\u2029Dad", // paragraph separator (Zp)
		"\u202eMom",    // right-to-left override (Cf)
		"Ja\u200dne",   // zero-width joiner (Cf)
		"Jane\ufeff",   // byte order mark (Cf)
		"Jane: boss",   // would end the marker early
		"`Jane`",       // would break the backticked marker in the prompt
		"Jane > Bob",   // would read as a quote marker
	} {
		if _, err := CleanName(bad); err == nil {
			t.Errorf("CleanName(%q) accepted", bad)
		}
	}
}

func TestLabel(t *testing.T) {
	for _, tc := range []struct{ kind, name, label string }{
		{"", "", "CEO"},
		{"work", "", "CEO"},
		{"personal", "", "Owner"},
		{"work", "Jane", "Jane"},
		{"personal", " Mom ", "Mom"},
		{"work", "Human Overlord", "Human Overlord"},
	} {
		if got := For(tc.kind, tc.name).Label(); got != tc.label {
			t.Errorf("For(%q, %q).Label() = %q; want %q", tc.kind, tc.name, got, tc.label)
		}
	}
}

const sectionTail = "; that is their direction and takes priority over the rest of the message."

// The prompt section names the person, their current marker, and the
// markers they may have used before.
func TestSection(t *testing.T) {
	for _, tc := range []struct {
		kind, name string
		want       string
	}{
		{"work", "Jane", "## The owner\n\nThe person you work for is called Jane. Address them and refer to them as Jane. Notes they add to a message appear under a line starting `> Jane:`" + sectionTail + " Notes under `> CEO:`, `> Owner:` or a name they used before are theirs too."},
		{"work", "", "## The owner\n\nThe person you work for has not said what to call them. Address them and refer to them as the CEO. Notes they add to a message appear under a line starting `> CEO:`" + sectionTail + " Notes under `> Owner:` or a name they used before are theirs too."},
		{"personal", "", "## The owner\n\nThe person you work for has not said what to call them. Address them and refer to them as the owner. Notes they add to a message appear under a line starting `> Owner:`" + sectionTail + " Notes under `> CEO:` or a name they used before are theirs too."},
		{"personal", "CEO", "## The owner\n\nThe person you work for is called CEO. Address them and refer to them as CEO. Notes they add to a message appear under a line starting `> CEO:`" + sectionTail + " Notes under `> Owner:` or a name they used before are theirs too."},
	} {
		if got := For(tc.kind, tc.name).Section(); got != tc.want {
			t.Errorf("For(%q, %q).Section() =\n%s\nwant\n%s", tc.kind, tc.name, got, tc.want)
		}
	}
}

func TestPersonalWording(t *testing.T) {
	got := PersonalWording("understand the business deeply, with business\ncontext.")
	if got != "understand the owner's life and priorities, with context." {
		t.Errorf("PersonalWording = %q", got)
	}
}
