package googleworkspace

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A document's own text, and a comment's, reaches the agent between
// these two lines, as an email's does between its own (gmail_text.go).
const (
	documentContentStart = "Content of an untrusted document follows."
	documentContentEnd   = "End of document content."
)

// delimitDocument puts a document's own text between the two lines
// (delimit).
func delimitDocument(s string) string { return delimit(s, documentContentStart, documentContentEnd) }

// docsDocument is a document as the Docs API answers documents.get:
// with includeTabsContent, every tab's content under Tabs; without, the
// first tab's under Body, DocumentStyle and NamedStyles.
type docsDocument struct {
	DocumentID    string           `json:"documentId"`
	Title         string           `json:"title"`
	Body          *docsBody        `json:"body"`
	DocumentStyle *docsDocStyle    `json:"documentStyle"`
	NamedStyles   *docsNamedStyles `json:"namedStyles"`
	Tabs          []docsTab        `json:"tabs"`
}

// docsTab is one tab of a document, the tabs inside it under ChildTabs.
type docsTab struct {
	TabProperties struct {
		Title string `json:"title"`
	} `json:"tabProperties"`
	DocumentTab *struct {
		Body          *docsBody        `json:"body"`
		DocumentStyle *docsDocStyle    `json:"documentStyle"`
		NamedStyles   *docsNamedStyles `json:"namedStyles"`
	} `json:"documentTab"`
	ChildTabs []docsTab `json:"childTabs"`
}

type docsBody struct {
	Content []docsElement `json:"content"`
}

// docsDocStyle is a tab's style, as far as Kivali reads it: the page's
// colour.
type docsDocStyle struct {
	Background *struct {
		Color *docsColor `json:"color"`
	} `json:"background"`
}

// docsNamedStyles is a tab's named styles (Normal text, Heading 1 and
// the rest), whose text styles a run inherits what it does not set.
type docsNamedStyles struct {
	Styles []struct {
		NamedStyleType string        `json:"namedStyleType"`
		TextStyle      docsTextStyle `json:"textStyle"`
	} `json:"styles"`
}

// docsElement is one structural element of a body or a table cell: a
// paragraph, a table, or what Kivali does not read as text of its own
// (a section break, a table of contents, which repeats the headings).
type docsElement struct {
	StartIndex int            `json:"startIndex"`
	EndIndex   int            `json:"endIndex"`
	Paragraph  *docsParagraph `json:"paragraph"`
	Table      *docsTable     `json:"table"`
}

type docsParagraph struct {
	Elements       []docsInline `json:"elements"`
	ParagraphStyle struct {
		NamedStyleType string `json:"namedStyleType"`
		Shading        *struct {
			BackgroundColor *docsColor `json:"backgroundColor"`
		} `json:"shading"`
	} `json:"paragraphStyle"`
	Bullet *struct {
		NestingLevel int `json:"nestingLevel"`
	} `json:"bullet"`
}

// docsInline is one element of a paragraph Kivali reads: a run of
// text, a person's chip, a link's chip, or a horizontal rule. The
// others (images, page breaks, footnote references, equations) are not
// text.
type docsInline struct {
	TextRun *struct {
		Content   string        `json:"content"`
		TextStyle docsTextStyle `json:"textStyle"`
	} `json:"textRun"`
	Person *struct {
		PersonProperties struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"personProperties"`
		TextStyle docsTextStyle `json:"textStyle"`
	} `json:"person"`
	RichLink *struct {
		RichLinkProperties struct {
			Title string `json:"title"`
			URI   string `json:"uri"`
		} `json:"richLinkProperties"`
		TextStyle docsTextStyle `json:"textStyle"`
	} `json:"richLink"`
	HorizontalRule *struct{} `json:"horizontalRule"`
}

type docsTable struct {
	TableRows []struct {
		TableCells []struct {
			Content        []docsElement `json:"content"`
			TableCellStyle struct {
				BackgroundColor *docsColor `json:"backgroundColor"`
			} `json:"tableCellStyle"`
		} `json:"tableCells"`
	} `json:"tableRows"`
}

// docsTextStyle is a run's style, as far as Kivali reads it. A field
// left out is inherited: from the paragraph's named style, then from
// Normal text.
type docsTextStyle struct {
	ForegroundColor *docsColor `json:"foregroundColor"`
	BackgroundColor *docsColor `json:"backgroundColor"`
	FontSize        *struct {
		Magnitude float64 `json:"magnitude"`
	} `json:"fontSize"`
	Link *struct {
		URL string `json:"url"`
	} `json:"link"`
}

// over is s with what it leaves out taken from base. A link is the
// run's own and is not inherited.
func (s docsTextStyle) over(base docsTextStyle) docsTextStyle {
	s.ForegroundColor = cmp.Or(s.ForegroundColor, base.ForegroundColor)
	s.BackgroundColor = cmp.Or(s.BackgroundColor, base.BackgroundColor)
	s.FontSize = cmp.Or(s.FontSize, base.FontSize)
	return s
}

// docsColor is the Docs API's OptionalColor: a colour, or, with no
// Color in it, transparent.
type docsColor struct {
	Color *struct {
		RGBColor *struct {
			Red   float64 `json:"red"`
			Green float64 `json:"green"`
			Blue  float64 `json:"blue"`
		} `json:"rgbColor"`
	} `json:"color"`
}

// rgb is a colour, each part from 0 to 1.
type rgb struct{ r, g, b float64 }

var (
	white = rgb{1, 1, 1}
	black = rgb{0, 0, 0}
)

// read is the colour c gives: set is false for no colour given at all
// (nil), transparent for one given as none. A colour whose parts are
// left out is black, as the API means it.
func (c *docsColor) read() (col rgb, transparent, set bool) {
	switch {
	case c == nil:
		return rgb{}, false, false
	case c.Color == nil:
		return rgb{}, true, true
	case c.Color.RGBColor == nil:
		return black, false, true
	}
	x := c.Color.RGBColor
	return rgb{x.Red, x.Green, x.Blue}, false, true
}

// The bounds of what hides a run: a font of tinyFontPt points or less,
// and a colour whose every part is within colorNear of the colour
// behind it (about 20 of 255, so #ebebeb on white is hidden and
// #e0e0e0 is not).
const (
	tinyFontPt = 3
	colorNear  = 0.08
)

// near reports whether two colours are too close to tell apart.
func near(a, b rgb) bool {
	d := func(x, y float64) float64 { return max(x-y, y-x) }
	return d(a.r, b.r) <= colorNear && d(a.g, b.g) <= colorNear && d(a.b, b.b) <= colorNear
}

// behind is the colour behind a run: the first of layers that gives
// one (the run's highlight, its paragraph's shading, its table cell's
// background), else the page's.
func behind(page rgb, layers ...*docsColor) rgb {
	for _, l := range layers {
		if col, transparent, set := l.read(); set && !transparent {
			return col
		}
	}
	return page
}

// hides reports whether a run in style st, its inherited style
// resolved, on back is out of a reader's sight: in a font of
// tinyFontPt or less, in a transparent colour, or in a colour near
// back's. A run that sets no colour is black.
func (st docsTextStyle) hides(back rgb) bool {
	if st.FontSize != nil && st.FontSize.Magnitude <= tinyFontPt {
		return true
	}
	fg, transparent, set := st.ForegroundColor.read()
	if !set {
		fg = black
	}
	return transparent || near(fg, back)
}

// documentText is a document as get_document reads it: its title and
// its body as text, every tab's in order, their URLs replaced by
// markers in links, and how many characters of hidden text were left
// out. It is built afresh for every call.
type documentText struct {
	title  string
	body   []rune
	links  *linkList
	hidden int
}

// docRenderer walks one tab's structure into lines.
type docRenderer struct {
	links  *linkList
	hidden int
	// named is the tab's named styles' text styles, by type; page the
	// colour of its page, white unless it sets one.
	named map[string]docsTextStyle
	page  rgb
}

// renderDocument is d as text. A heading starts with as many # as its
// level ("# " for the title style too), a list item with "- ", indented
// two spaces a level, and a table is a line a row, its cells' text
// split by " | ". With more than one tab, each tab starts with a line
// naming it. Runs that hide (docsTextStyle.hides) are left out and
// counted, and so are Unicode tag characters; links become markers.
func renderDocument(d docsDocument) *documentText {
	r := &docRenderer{links: newLinkList()}
	out := &documentText{links: r.links}
	title, tags := cleanText(d.Title)
	r.hidden += tags
	out.title = r.links.replace(oneLine(title, nameClip))
	type part struct {
		title  string
		body   *docsBody
		style  *docsDocStyle
		styles *docsNamedStyles
	}
	var parts []part
	var walk func(ts []docsTab)
	walk = func(ts []docsTab) {
		for _, t := range ts {
			if t.DocumentTab != nil {
				parts = append(parts, part{t.TabProperties.Title, t.DocumentTab.Body, t.DocumentTab.DocumentStyle, t.DocumentTab.NamedStyles})
			}
			walk(t.ChildTabs)
		}
	}
	walk(d.Tabs)
	if len(d.Tabs) == 0 {
		parts = append(parts, part{"", d.Body, d.DocumentStyle, d.NamedStyles})
	}
	var lines []string
	for _, p := range parts {
		if len(parts) > 1 {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, "Tab: "+r.links.replace(cmp.Or(oneLine(p.title, nameClip), "(no name)")))
		}
		r.named = map[string]docsTextStyle{}
		if p.styles != nil {
			for _, s := range p.styles.Styles {
				r.named[s.NamedStyleType] = s.TextStyle
			}
		}
		r.page = white
		if p.style != nil && p.style.Background != nil {
			r.page = behind(white, p.style.Background.Color)
		}
		if p.body != nil {
			lines = append(lines, r.content(p.body.Content, nil)...)
		}
	}
	text := blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	out.body = []rune(trimBody(text))
	out.hidden = r.hidden
	return out
}

// blankRuns is more than one blank line.
var blankRuns = regexp.MustCompile(`\n{3,}`)

// headingMarks are the heading styles' marks.
var headingMarks = map[string]string{
	"TITLE": "# ", "HEADING_1": "# ", "HEADING_2": "## ", "HEADING_3": "### ",
	"HEADING_4": "#### ", "HEADING_5": "##### ", "HEADING_6": "###### ",
}

// content is els as lines, cell the background of the table cell they
// are in (nil outside a table). A heading and a table have a blank line
// before them.
func (r *docRenderer) content(els []docsElement, cell *docsColor) []string {
	var lines []string
	blank := func() {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
	}
	for _, el := range els {
		switch {
		case el.Paragraph != nil:
			line, keep := r.paragraph(el.Paragraph, cell)
			if !keep {
				continue
			}
			if strings.HasPrefix(line, "#") && headingMarks[el.Paragraph.ParagraphStyle.NamedStyleType] != "" {
				blank()
			}
			lines = append(lines, line)
		case el.Table != nil:
			blank()
			for _, row := range el.Table.TableRows {
				var cells []string
				for _, tc := range row.TableCells {
					back := cell
					if _, transparent, set := tc.TableCellStyle.BackgroundColor.read(); set && !transparent {
						back = tc.TableCellStyle.BackgroundColor
					}
					cells = append(cells, strings.Join(strings.Fields(strings.Join(r.content(tc.Content, back), " ")), " "))
				}
				lines = append(lines, strings.Join(cells, " | "))
			}
			lines = append(lines, "")
		}
	}
	return lines
}

// paragraph is p as text: its runs a reader sees, each link's marker
// after the text it is on, its heading or list item's mark before it.
// keep is false for a paragraph whose text is all hidden, which leaves
// no line; an empty paragraph is an empty line.
func (r *docRenderer) paragraph(p *docsParagraph, cell *docsColor) (line string, keep bool) {
	base := r.named["NORMAL_TEXT"]
	if t := p.ParagraphStyle.NamedStyleType; t != "" && t != "NORMAL_TEXT" {
		base = r.named[t].over(base)
	}
	var shading *docsColor
	if p.ParagraphStyle.Shading != nil {
		shading = p.ParagraphStyle.Shading.BackgroundColor
	}
	var b, group strings.Builder
	groupURL, hid := "", false
	// flush writes the runs since the last one with another link: their
	// text with its URLs replaced, then the link's marker.
	flush := func() {
		text := r.links.replace(group.String())
		if groupURL != "" && strings.TrimSpace(text) != "" {
			m := r.links.marker(groupURL)
			if kept := strings.TrimRight(text, " \t\n"); !strings.HasSuffix(kept, m) {
				text = kept + " " + m + text[len(kept):]
			}
		}
		b.WriteString(text)
		group.Reset()
	}
	for _, el := range p.Elements {
		var content, link string
		var st docsTextStyle
		switch {
		case el.TextRun != nil:
			content, st = el.TextRun.Content, el.TextRun.TextStyle
		case el.Person != nil:
			pp := el.Person.PersonProperties
			content, st = cmp.Or(pp.Name, pp.Email), el.Person.TextStyle
		case el.RichLink != nil:
			rl := el.RichLink.RichLinkProperties
			content, st, link = cmp.Or(rl.Title, rl.URI), el.RichLink.TextStyle, rl.URI
		case el.HorizontalRule != nil:
			content = "---"
		default:
			continue
		}
		if st.Link != nil {
			link = st.Link.URL
		}
		link = httpURL(link)
		// A vertical tab is the API's line break within a paragraph.
		content, tags := cleanText(strings.ReplaceAll(content, "\v", "\n"))
		r.hidden += tags
		resolved := st.over(base)
		if resolved.hides(behind(r.page, resolved.BackgroundColor, shading, cell)) {
			r.hidden += charCount(content)
			hid = hid || strings.TrimSpace(content) != ""
			continue
		}
		if link != groupURL {
			flush()
			groupURL = link
		}
		group.WriteString(content)
	}
	flush()
	text := strings.TrimRight(b.String(), " \t\n")
	if strings.TrimSpace(text) == "" {
		return "", !hid
	}
	switch {
	case p.Bullet != nil:
		text = strings.Repeat("  ", min(max(p.Bullet.NestingLevel, 0), 8)) + "- " + strings.TrimLeft(text, " \t")
	case headingMarks[p.ParagraphStyle.NamedStyleType] != "":
		text = headingMarks[p.ParagraphStyle.NamedStyleType] + strings.TrimLeft(text, " \t")
	}
	return text, true
}

// httpURL is u, cleaned, when it is an http or https URL; "" for any
// other (a link to a heading or a bookmark in the document has none).
func httpURL(u string) string {
	u, _ = cleanText(strings.TrimSpace(u))
	if l := strings.ToLower(u); !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
		return ""
	}
	if strings.ContainsAny(u, " \t\n") {
		return ""
	}
	return u
}

// pageTries bounds the cuts page tries, and leastPage is the fewest
// characters of the body a page holds when there are more left.
const (
	pageTries = 4
	leastPage = 1000
)

// page is get_document's answer for the body's characters from offset,
// and where they stop, the next page's offset: Kivali's heading and
// provenance lines; between the framing lines the title, the body's
// characters and the links they name; then Kivali's notes. It holds as
// many characters as fit in answerClip with every framing closed, and
// never stops inside a link's marker.
func (d *documentText) page(id, provenance string, offset int) (string, int) {
	total := len(d.body)
	most := min(answerClip-answerRoom, total-offset)
	var text string
	end := offset
	for range pageTries {
		end = markerSafe(d.body, offset, offset+most)
		text = d.render(id, provenance, offset, end)
		over := utf8.RuneCountInString(text) - answerClip
		if over <= 0 || most <= leastPage {
			break
		}
		most = max(most-over-100, leastPage)
	}
	return text, end
}

// markerSafe is end, moved back to the start of a marker it would cut.
// Every "[link" in the body is a marker's start: the document's own
// text that reads like one is spaced apart (linkList.replace).
func markerSafe(body []rune, start, end int) int {
	if end >= len(body) {
		return len(body)
	}
	for i := end - 1; i >= max(start, end-len("[link 999]")); i-- {
		if body[i] == ']' {
			return end
		}
		if body[i] == '[' {
			if strings.HasPrefix(string(body[i:min(i+5, len(body))]), "[link") && i > start {
				return i
			}
			return end
		}
	}
	return end
}

// render is the page of the body's characters from start to end.
func (d *documentText) render(id, provenance string, start, end int) string {
	total := len(d.body)
	part := string(d.body[start:end])
	inner := []string{"Title: " + d.title}
	if strings.TrimSpace(part) != "" {
		inner = append(inner, "", part)
	}
	if l := d.links.listingOf(markersIn(strings.Join(inner, "\n"))); l != "" {
		inner = append(inner, "", l)
	}
	notes := []string{fmt.Sprintf("Document %s: characters %d to %d of %d.", id, start, end, total), provenance, delimitDocument(strings.Join(inner, "\n"))}
	if total == 0 {
		notes = append(notes, "The document has no text.")
	}
	if d.hidden > 0 {
		notes = append(notes, fmt.Sprintf("Kivali left out %d characters of hidden text from the document.", d.hidden))
	}
	if n := d.links.moreNoteOf("document"); n != "" {
		notes = append(notes, n)
	}
	if end < total {
		notes = append(notes, fmt.Sprintf("next_offset: %d (pass it as offset for the next page).", end))
	} else {
		notes = append(notes, "This is the end of the document.")
	}
	return strings.Join(notes, "\n")
}

// markersIn is the numbers of the markers s names, each once, in
// order.
func markersIn(s string) []int {
	var out []int
	for _, m := range markerNumber.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
