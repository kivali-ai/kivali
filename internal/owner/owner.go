// Package owner names the person a Kivali team works for.
//
// Stored text never carries the person's name. The seeded handbook and
// roles, tool descriptions and rule refusals call them "the owner"; the
// name they chose (KIVALI_OWNER_NAME, or Org › Organization in the app)
// reaches agents through one section of every system prompt (Section),
// assembled per turn, so a rename lands everywhere from each agent's
// next turn. Live labels (the markup marker, the org chart, a bounce's
// title, the app) use Label. The person's slug is always "ceo".
package owner

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameRunes is the longest name a person can choose.
const MaxNameRunes = 40

// kindPersonal is store.TeamKindPersonal.
const kindPersonal = "personal"

// CleanName trims name and checks it: at most MaxNameRunes runes, no
// control, format or line-separator characters, and none of ':', '`'
// or '>', which would let a name break the markup marker ("> Jane:")
// or the backticked marker in the system prompt. Those characters are
// refused, not escaped: a name is a few words a person types once, and
// a refusal they can see beats a marker that differs from what they
// typed. Empty is allowed and means unset.
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxNameRunes {
		return "", errors.New("the name is longer than 40 characters")
	}
	if strings.IndexFunc(name, invisible) >= 0 {
		return "", errors.New("the name has a line break, a control character or an invisible formatting character")
	}
	if strings.ContainsAny(name, ":`>") {
		return "", errors.New("the name has a colon, a backtick or a >")
	}
	return name, nil
}

// invisible reports whether r has no place in a name: a control
// character (Cc), a format character such as a bidi override or a
// zero-width joiner (Cf), or a line or paragraph separator (Zl, Zp).
func invisible(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// Term is what a team calls its person.
type Term struct {
	// Kind is the team's kind: "personal", or anything else for work.
	Kind string
	// Name is the person's chosen name, or "".
	Name string
}

// For returns the term for a team of kind whose person chose name.
func For(kind, name string) Term {
	return Term{Kind: kind, Name: strings.TrimSpace(name)}
}

func (t Term) personal() bool { return t.Kind == kindPersonal }

// Label is the person as a label: their name, else "CEO" on a work
// team and "Owner" on a personal one. It is the markup marker
// ("> Label: …"), the org chart's label for the ceo node, and a
// bounce's "Bounced by Label".
func (t Term) Label() string {
	switch {
	case t.Name != "":
		return t.Name
	case t.personal():
		return "Owner"
	default:
		return "CEO"
	}
}

// noun is the person inside a sentence: their name, else "the CEO" or
// "the owner".
func (t Term) noun() string {
	switch {
	case t.Name != "":
		return t.Name
	case t.personal():
		return "the owner"
	default:
		return "the CEO"
	}
}

// Section is the system-prompt section that tells an agent what the
// person is called and which lines are their markup. Rendered per
// turn from the stored name, never stored itself.
func (t Term) Section() string {
	var b strings.Builder
	b.WriteString("## The owner\n\n")
	if t.Name != "" {
		b.WriteString("The person you work for is called " + t.Name + ". Address them and refer to them as " + t.Name + ".")
	} else {
		b.WriteString("The person you work for has not said what to call them. Address them and refer to them as " + t.noun() + ".")
	}
	b.WriteString(" Notes they add to a message appear under a line starting `> " + t.Label() + ":`; that is their direction and takes priority over the rest of the message.")
	b.WriteString(" Notes under " + t.olderMarkers() + " or a name they used before are theirs too.")
	return b.String()
}

// olderMarkers names the default markers other than the current one,
// for markup written before a rename.
func (t Term) olderMarkers() string {
	var out []string
	for _, m := range []string{"CEO", "Owner"} {
		if m != t.Label() {
			out = append(out, "`> "+m+":`")
		}
	}
	return strings.Join(out, ", ")
}

// rule is one rewrite of a work team's business wording for a personal
// team.
type rule struct {
	re   *regexp.Regexp
	repl string
}

// personalRules rewrite a work team's business wording for a personal
// team. Kind never changes, so this is a choice made once, at hire.
// Each must match the seed text it is for; the seed tests hold them to
// it.
var personalRules = []rule{
	{regexp.MustCompile(`understand the business deeply`), `understand the owner's life and priorities`},
	{regexp.MustCompile(`true about the business\)`), `true about the owner and their world)`},
	{regexp.MustCompile(`\*\*Business context\*\* — the load-bearing parts of the biz plan /(\s+)market / product for`), `**Context** — the load-bearing parts of what you know about the owner for`},
	{regexp.MustCompile(`Not every agent needs the whole biz plan\.`), `Not every agent needs everything you know.`},
	{regexp.MustCompile(`business(\s+)context`), `context`},
}

// PersonalWording returns text, written for a work team, with the
// business wording a personal team has no use for replaced.
func PersonalWording(text string) string {
	for _, r := range personalRules {
		text = r.re.ReplaceAllString(text, r.repl)
	}
	return text
}

// PersonalRulesUnmatched lists the personal rules that match nothing in
// text: a seed edit that broke one of them. Tests call it.
func PersonalRulesUnmatched(text string) []string {
	var out []string
	for _, r := range personalRules {
		if !r.re.MatchString(text) {
			out = append(out, r.re.String())
		}
	}
	return out
}
