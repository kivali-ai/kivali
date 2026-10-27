package googleworkspace

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// styleProps are the CSS properties Kivali reads of an email's HTML to
// tell what a reader sees (elementHidden, textStyle.under); any other
// is left out of what styleSheet.style answers.
var styleProps = map[string]bool{
	"display": true, "mso-hide": true, "opacity": true, "overflow": true, "visibility": true,
	"height": true, "max-height": true, "width": true, "max-width": true,
	"text-indent": true, "left": true, "top": true, "margin-left": true, "clip": true, "clip-path": true,
	"color": true, "background": true, "background-color": true, "background-image": true,
	"font-size": true, "white-space": true,
}

// cssDecl is one declaration of a style: its property and value,
// lower-cased, and whether it is !important.
type cssDecl struct {
	name, value string
	important   bool
}

// importantMark is a value's "!important", with any spaces in it.
var importantMark = regexp.MustCompile(`!\s*important\s*$`)

// parseDecls is a style attribute's or a rule's declarations, those of
// styleProps alone, in order, "!important" (spaced or not) taken off
// the value and kept as important.
func parseDecls(s string) []cssDecl {
	var out []cssDecl
	for _, d := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(d, ":")
		if !ok {
			continue
		}
		name := strings.TrimSpace(strings.ToLower(k))
		if !styleProps[name] {
			continue
		}
		v = strings.TrimSpace(strings.ToLower(v))
		important := false
		if loc := importantMark.FindStringIndex(v); loc != nil {
			v, important = strings.TrimSpace(v[:loc[0]]), true
		}
		out = append(out, cssDecl{name: name, value: v, important: important})
	}
	return out
}

// An email's rules and elements are its sender's, as many as it likes,
// so Kivali applies at most maxStyleRules selectors of its <style>
// elements and checks a selector against an element at most
// maxRuleChecks times a body.
const (
	maxStyleRules = 1000
	maxRuleChecks = 1_000_000
)

// styleSheet is the rules of an HTML body's <style> elements Kivali
// applies: each selector of a comma list that is made of type, class
// and id selectors (p, .x, #y, p.x, *) and the combinators between
// them, matched by its last part alone (".a .x" and "div > .x" apply to
// every .x), in the cascade's order: by specificity, the whole
// selector's, then by the order written, with !important above.
// Elements' tags, ids and classes are compared without regard to case,
// as mail readers without a doctype do.
//
// Not applied: @media and every other at-rule and what it holds; a
// selector with an attribute ([class~=x]), a pseudo-class or a
// pseudo-element (:hover, :not(), ::before), or an escape; a style
// sheet linked from elsewhere; the selectors past the first
// maxStyleRules, and every check past maxRuleChecks. A rule whose
// context (an ancestor, a sibling) Kivali does not check applies
// wherever its last part matches.
type styleSheet struct {
	byID, byClass, byTag map[string][]*styleRule
	any                  []*styleRule
	rules, checks        int
	// memo is what the rules give an element, by its tag, id and
	// classes.
	memo map[string]sheetMatch
}

// styleRule is one selector and the declarations it gives: the last
// part's tag ("" for any), id ("" for any) and classes, sorted, the
// whole selector's specificity (ids, classes, types) and the place of
// its rule in the sheet.
type styleRule struct {
	tag, id string
	classes []string
	spec    [3]int
	order   int
	decls   []cssDecl
}

// sheetMatch is what a sheet's rules give an element: the winning value
// of each property among its normal declarations and among its
// important ones.
type sheetMatch struct{ normal, important map[string]string }

// sheetOf is the rules of every <style> element of doc, nil for none.
func sheetOf(doc *html.Node) *styleSheet {
	var b strings.Builder
	var find func(n *html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Style && n.Namespace == "" {
			for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
				if ch.Type == html.TextNode {
					b.WriteString(ch.Data)
					b.WriteByte('\n')
				}
			}
			return
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			find(ch)
		}
	}
	find(doc)
	if b.Len() == 0 {
		return nil
	}
	return parseSheet(b.String())
}

// parseSheet reads a style sheet's rules: comments and at-rules (with
// their blocks) skipped, each other rule's selectors read
// (parseSelector), the rules giving none of styleProps left out.
func parseSheet(css string) *styleSheet {
	sh := &styleSheet{byID: map[string][]*styleRule{}, byClass: map[string][]*styleRule{}, byTag: map[string][]*styleRule{},
		memo: map[string]sheetMatch{}}
	css = stripCSSComments(css)
	for i, order := 0, 0; i < len(css) && sh.rules < maxStyleRules; {
		switch css[i] {
		case '}', ';', ' ', '\t', '\n', '\r', '\f':
			i++
			continue
		case '@':
			j := cssScan(css, i, "{;")
			switch {
			case j >= len(css):
				return sh
			case css[j] == ';':
				i = j + 1
			default:
				i = cssBlockEnd(css, j) + 1
			}
			continue
		}
		open := cssScan(css, i, "{")
		if open >= len(css) {
			break
		}
		end := cssBlockEnd(css, open)
		selectors, decls := css[i:open], parseDecls(css[open+1:min(end, len(css))])
		i = end + 1
		if len(decls) == 0 {
			continue
		}
		order++
		for _, sel := range strings.Split(selectors, ",") {
			r, ok := parseSelector(sel)
			if !ok || sh.rules >= maxStyleRules {
				continue
			}
			r.order, r.decls = order, decls
			sh.rules++
			switch {
			case r.id != "":
				sh.byID[r.id] = append(sh.byID[r.id], &r)
			case len(r.classes) > 0:
				sh.byClass[r.classes[0]] = append(sh.byClass[r.classes[0]], &r)
			case r.tag != "":
				sh.byTag[r.tag] = append(sh.byTag[r.tag], &r)
			default:
				sh.any = append(sh.any, &r)
			}
		}
	}
	return sh
}

// stripCSSComments is css with each comment, and each "<!--" and "-->"
// a sheet inside HTML may hold, made a space; quoted strings are kept
// whole, so a "/*" inside one starts no comment.
func stripCSSComments(css string) string {
	var b strings.Builder
	for i := 0; i < len(css); {
		switch {
		case css[i] == '"' || css[i] == '\'':
			j := cssStringEnd(css, i)
			b.WriteString(css[i:j])
			i = j
		case strings.HasPrefix(css[i:], "/*"):
			end := strings.Index(css[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += 2 + end + 2
		case strings.HasPrefix(css[i:], "<!--"):
			b.WriteByte(' ')
			i += 4
		case strings.HasPrefix(css[i:], "-->"):
			b.WriteByte(' ')
			i += 3
		default:
			b.WriteByte(css[i])
			i++
		}
	}
	return b.String()
}

// cssStringEnd is the index just past the quoted string starting at i,
// its backslash escapes skipped; len(css) when it does not close.
func cssStringEnd(css string, i int) int {
	q := css[i]
	for j := i + 1; j < len(css); j++ {
		switch css[j] {
		case '\\':
			j++
		case q:
			return j + 1
		}
	}
	return len(css)
}

// cssScan is the index of the first byte of stops at or after i outside
// a quoted string; len(css) for none.
func cssScan(css string, i int, stops string) int {
	for i < len(css) {
		switch c := css[i]; {
		case c == '"' || c == '\'':
			i = cssStringEnd(css, i)
		case strings.IndexByte(stops, c) >= 0:
			return i
		default:
			i++
		}
	}
	return len(css)
}

// cssBlockEnd is the index of the "}" that closes the "{" at open,
// blocks inside it and quoted strings skipped; len(css) when it does
// not close.
func cssBlockEnd(css string, open int) int {
	depth := 0
	for i := open; i < len(css); {
		switch css[i] {
		case '"', '\'':
			i = cssStringEnd(css, i)
			continue
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return i
			}
		}
		i++
	}
	return len(css)
}

// selectorChars are the characters of a selector Kivali reads: names,
// ".", "#", "*", combinators and white space.
var selectorChars = regexp.MustCompile(`^[A-Za-z0-9_.#*>+~\s-]+$`)

// cssIdent is a CSS name: a tag's, a class's or an id's.
var cssIdent = regexp.MustCompile(`^(--|-?[A-Za-z_])[A-Za-z0-9_-]*$`)

// parseSelector reads one selector of a comma list as a styleRule (its
// order and declarations left to the caller); ok is false for one
// Kivali does not apply (styleSheet).
func parseSelector(sel string) (styleRule, bool) {
	sel = strings.TrimSpace(sel)
	if !selectorChars.MatchString(sel) {
		return styleRule{}, false
	}
	parts := strings.FieldsFunc(sel, func(r rune) bool {
		return r == '>' || r == '+' || r == '~' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f'
	})
	if len(parts) == 0 {
		return styleRule{}, false
	}
	var r styleRule
	for i, p := range parts {
		tag, ids, classes, ok := parseCompound(p)
		if !ok {
			return styleRule{}, false
		}
		r.spec[0] += len(ids)
		r.spec[1] += len(classes)
		if tag != "" {
			r.spec[2]++
		}
		if i == len(parts)-1 {
			r.tag, r.classes = tag, classes
			if len(ids) > 0 {
				r.id = ids[0]
			}
		}
	}
	slices.Sort(r.classes)
	return r, true
}

// parseCompound reads one part of a selector between combinators: a
// tag or "*" ("" for either), then ids and classes. ok is false for a
// part that is not one, or one naming two ids, which no element has.
func parseCompound(p string) (tag string, ids, classes []string, ok bool) {
	i := strings.IndexAny(p, ".#")
	if i < 0 {
		i = len(p)
	}
	tag = strings.ToLower(p[:i])
	switch {
	case tag == "*":
		tag = ""
	case tag != "" && !cssIdent.MatchString(tag):
		return "", nil, nil, false
	}
	for rest := p[i:]; rest != ""; {
		kind := rest[0]
		rest = rest[1:]
		j := strings.IndexAny(rest, ".#")
		if j < 0 {
			j = len(rest)
		}
		name := strings.ToLower(rest[:j])
		rest = rest[j:]
		if !cssIdent.MatchString(name) {
			return "", nil, nil, false
		}
		if kind == '#' {
			ids = append(ids, name)
		} else {
			classes = append(classes, name)
		}
	}
	if len(ids) > 1 && slices.ContainsFunc(ids, func(id string) bool { return id != ids[0] }) {
		return "", nil, nil, false
	}
	return tag, ids, classes, true
}

// matches reports whether the rule's last part names an element of tag,
// id and classes (sorted).
func (r *styleRule) matches(tag, id string, classes []string) bool {
	if (r.tag != "" && r.tag != tag) || (r.id != "" && r.id != id) {
		return false
	}
	for _, c := range r.classes {
		if _, found := slices.BinarySearch(classes, c); !found {
			return false
		}
	}
	return true
}

// match is what the sheet's rules give n.
func (sh *styleSheet) match(n *html.Node) sheetMatch {
	id, _ := attr(n, "id")
	id = strings.ToLower(strings.TrimSpace(id))
	cv, _ := attr(n, "class")
	classes := slices.Compact(slices.Sorted(slices.Values(strings.Fields(strings.ToLower(cv)))))
	// The id is quoted, as it may hold spaces, so no two elements share
	// a key.
	key := n.Data + " " + strconv.Quote(id) + " " + strings.Join(classes, " ")
	if m, ok := sh.memo[key]; ok {
		return m
	}
	var found []*styleRule
	consider := func(rs []*styleRule) {
		for _, r := range rs {
			if sh.checks >= maxRuleChecks {
				return
			}
			sh.checks++
			if r.matches(n.Data, id, classes) {
				found = append(found, r)
			}
		}
	}
	if id != "" {
		consider(sh.byID[id])
	}
	for _, c := range classes {
		consider(sh.byClass[c])
	}
	consider(sh.byTag[n.Data])
	consider(sh.any)
	slices.SortStableFunc(found, func(a, b *styleRule) int {
		return cmp.Or(slices.Compare(a.spec[:], b.spec[:]), cmp.Compare(a.order, b.order))
	})
	m := sheetMatch{normal: map[string]string{}, important: map[string]string{}}
	for _, r := range found {
		for _, d := range r.decls {
			if d.important {
				m.important[d.name] = d.value
			} else {
				m.normal[d.name] = d.value
			}
		}
	}
	sh.memo[key] = m
	return m
}

// style is the properties of styleProps that apply to n, names and
// values lower-cased, in the cascade's order: the sheet's rules (nil
// for a body with none), then n's style attribute, then the rules'
// !important declarations, then the attribute's; nil for none.
func (sh *styleSheet) style(n *html.Node) map[string]string {
	v, _ := attr(n, "style")
	inline := parseDecls(v)
	var m sheetMatch
	if sh != nil {
		m = sh.match(n)
	}
	if len(inline) == 0 && len(m.normal) == 0 && len(m.important) == 0 {
		return nil
	}
	out := maps.Clone(m.normal)
	if out == nil {
		out = map[string]string{}
	}
	for _, important := range []bool{false, true} {
		if important {
			maps.Copy(out, m.important)
		}
		for _, d := range inline {
			if d.important == important {
				out[d.name] = d.value
			}
		}
	}
	return out
}
