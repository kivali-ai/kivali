// Package handbook says what a team's handbook is: four sections,
// each a `## ` heading, in one order. The store holds the text and
// internal/seed holds the defaults; this package holds the shape, so
// the Chief of Staff's proposals, the owner's edits and setup are all
// held to the same rule.
package handbook

import (
	"errors"
	"fmt"
	"strings"
)

// Sections are the team handbook's sections, in order. Each is a
// `## ` heading. What goes under one, `### ` subsections included, is
// the team's to write.
var Sections = []string{"About", "Values", "Communicating", "Working together"}

// Names is the sections as a sentence lists them: "About, Values,
// Communicating and Working together".
func Names() string {
	n := len(Sections)
	return strings.Join(Sections[:n-1], ", ") + " and " + Sections[n-1]
}

// Validate reports why body is not a team handbook: it is blank, a
// section of Sections is missing or repeated, the sections are out of
// order, or a `## ` heading is not one of them. Heading text is
// matched without regard to case, and a line inside a code fence is
// not a heading. Anything before the first section, such as a title,
// is allowed.
func Validate(body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("the handbook is empty")
	}
	seen := map[string]bool{}
	var order []string
	for _, h := range Headings(body) {
		var known string
		for _, s := range Sections {
			if strings.EqualFold(h, s) {
				known = s
				break
			}
		}
		if known == "" {
			return fmt.Errorf("the handbook has a section `## %s`, which is not one of %s; put it under one of those as a `### ` subsection", h, Names())
		}
		if seen[known] {
			return fmt.Errorf("the handbook has `## %s` twice", known)
		}
		seen[known] = true
		order = append(order, known)
	}
	for _, s := range Sections {
		if !seen[s] {
			return fmt.Errorf("the handbook has no section `## %s`; it needs %s, in that order", s, Names())
		}
	}
	for i, s := range Sections {
		if order[i] != s {
			return fmt.Errorf("the handbook's sections are out of order; it needs %s, in that order", Names())
		}
	}
	return nil
}

// Headings returns the text of every `## ` heading in md, in order:
// exactly two hashes then a space, outside code fences, with any
// closing hashes dropped. `### ` subsections are not among them.
func Headings(md string) []string {
	var out []string
	fenced := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		rest, ok := strings.CutPrefix(line, "##")
		if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		text := strings.TrimSpace(rest)
		text = strings.TrimSpace(strings.TrimRight(text, "#"))
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}
