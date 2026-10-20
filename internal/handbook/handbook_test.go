package handbook

import (
	"strings"
	"testing"
)

const valid = "# Handbook\n\nAn opening line.\n\n## About\n\nUs.\n\n### Where we are\n\nHere.\n\n## Values\n\nCare.\n\n## Communicating\n\nPlainly.\n\n## Working together\n\nKindly.\n"

// A handbook with the four sections in order, a title, an opening
// line and a subsection is accepted: the shape rule is about the `## `
// sections only, or an owner could not title their handbook or break
// a section down.
func TestValidateAcceptsTheShape(t *testing.T) {
	if err := Validate(valid); err != nil {
		t.Fatalf("Validate(valid) = %v", err)
	}
	// Case does not matter: read_handbook matches sections without
	// regard to case, and a handbook that passed there but failed here
	// would confuse the Chief of Staff.
	if err := Validate(strings.ReplaceAll(valid, "## Working together", "## WORKING TOGETHER")); err != nil {
		t.Errorf("a differently cased heading was refused: %v", err)
	}
	// Closing hashes are markdown for the same heading.
	if err := Validate(strings.ReplaceAll(valid, "## Values", "## Values ##")); err != nil {
		t.Errorf("a heading with closing hashes was refused: %v", err)
	}
}

// Each way a draft can miss the shape is refused with a message that
// names the section at fault, so a Chief of Staff whose proposal is
// refused knows what to fix without reading the rule book.
func TestValidateRefusesEachBreak(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"blank", "  \n", "empty"},
		{"missing", strings.Replace(valid, "## Values\n\nCare.\n\n", "", 1), "no section `## Values`"},
		{"extra", valid + "\n## Money\n\nSpend less.\n", "`## Money`"},
		{"twice", valid + "\n## About\n\nAgain.\n", "`## About` twice"},
		{"order", strings.Replace(strings.Replace(valid, "## Values", "## Communicating", 1), "## Communicating\n\nPlainly.", "## Values\n\nPlainly.", 1), "out of order"},
	} {
		err := Validate(tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error mentioning %q", tc.name, err, tc.want)
		}
	}
}

// Headings skips `### ` subsections, code fences and lines that only
// look like headings, matching how read_handbook and the Org page
// split a document; a fenced "## x" counted as a section would refuse
// a handbook whose example block shows one.
func TestHeadingsFindsSectionsOnly(t *testing.T) {
	md := "# T\n## About\n### Sub\n```\n## not one\n```\n##no space\n##\tTabbed\n## Values ##\n  ## indented\n"
	got := Headings(md)
	want := []string{"About", "Tabbed", "Values"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Headings = %v, want %v", got, want)
	}
}

func TestNames(t *testing.T) {
	if Names() != "About, Values, Communicating and Working together" {
		t.Errorf("Names = %q", Names())
	}
}
