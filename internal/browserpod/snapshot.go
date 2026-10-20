package browserpod

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// snapshotJS walks the page in the tab's isolated world and returns
// its lines and refs; see the script for the rules.
//
//go:embed snapshot.js
var snapshotJS string

// snapshotData is what snapshot.js returns.
type snapshotData struct {
	Title     string     `json:"title"`
	URL       string     `json:"url"`
	Width     float64    `json:"width"`
	Height    float64    `json:"height"`
	ScrollY   float64    `json:"scrollY"`
	DocHeight float64    `json:"docHeight"`
	Lines     []snapLine `json:"lines"`
	Refs      []*refInfo `json:"refs"`
}

// snapLine is one line of the page at a depth. Top is how far down
// the document the line's element starts, in CSS pixels.
type snapLine struct {
	Depth int
	Text  string
	Top   float64
}

func (l *snapLine) UnmarshalJSON(b []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err != nil {
		return err
	}
	if len(parts) != 2 && len(parts) != 3 {
		return fmt.Errorf("snapshot line has %d parts", len(parts))
	}
	if err := json.Unmarshal(parts[0], &l.Depth); err != nil {
		return err
	}
	if err := json.Unmarshal(parts[1], &l.Text); err != nil {
		return err
	}
	if len(parts) == 3 {
		return json.Unmarshal(parts[2], &l.Top)
	}
	return nil
}

// Page content reaches the model between these two lines, so that it
// can tell what the pod says from what the page says.
const (
	pageContentStart = "Content of an untrusted page follows."
	pageContentEnd   = "End of page content."
)

// delimitPage puts page content between the two lines.
func delimitPage(s string) string {
	return pageContentStart + "\n" + s + "\n" + pageContentEnd
}

// withPage is a result in two parts: the pod's own words, then the
// lines a page controls (a tab's URL and title, an element's role and
// name, a URL a page tried to reach) between the page content lines.
// No lines, no page content.
func withPage(text string, lines ...string) string {
	if len(lines) == 0 {
		return text
	}
	return text + "\n" + delimitPage(strings.Join(lines, "\n"))
}

// failPage is failf for a refusal that names what a page controls.
func failPage(code, text string, lines ...string) error {
	return &cmdError{code: code, msg: withPage(text, lines...)}
}

// elementLine is the page content line naming an element by its role
// and name.
func elementLine(info refInfo) string {
	return fmt.Sprintf("Element: %s %q", info.Role, info.Name)
}

// urlLine is the page content line for a URL a page reached or tried
// to reach.
func urlLine(u string) string { return "URL: " + u }

// maxDepth bounds the indentation of a line, whatever the page nests.
const maxDepth = 64

// noteRoom is what a cut snapshot keeps back for its two notes.
const noteRoom = 200

func (b *browser) cmdSnapshot(ctx context.Context, t *tab) (result, error) {
	navs := t.mark().navs
	var d snapshotData
	if err := b.evalValue(ctx, t, snapshotJS, &d); err != nil {
		return result{}, err
	}
	refs := make([]refInfo, len(d.Refs))
	for i, r := range d.Refs {
		if r != nil {
			refs[i] = *r
		}
	}
	t.update(func() {
		t.hasRefs = true
		t.refsNav = navs
		t.refs = refs
		if d.Title != "" {
			t.title = d.Title
		}
	})
	return result{text: formatSnapshot(d, MaxSnapshotChars)}, nil
}

// formatSnapshot renders the page header and the indented lines
// between the page content lines. A page that fits in limit characters
// is shown whole. One that does not shows the lines from the current
// scroll position down, then as many above as still fit, with a note
// at each end saying how many lines were left out there.
func formatSnapshot(d snapshotData, limit int) string {
	return delimitPage(snapshotBody(d, limit-len(pageContentStart)-len(pageContentEnd)-2))
}

func snapshotBody(d snapshotData, limit int) string {
	var sb strings.Builder
	title := d.Title
	if title == "" {
		title = "(untitled)"
	}
	fmt.Fprintf(&sb, "Page: %s — %s\n", title, d.URL)
	fmt.Fprintf(&sb, "Viewport: %.0fx%.0f, scrolled %.0f of %.0f", d.Width, d.Height, d.ScrollY, d.DocHeight)
	if len(d.Lines) == 0 {
		sb.WriteString("\n(the page shows nothing)")
		return sb.String()
	}
	rendered := make([]string, len(d.Lines))
	total := sb.Len()
	for i, l := range d.Lines {
		depth := min(max(l.Depth, 0), maxDepth)
		rendered[i] = "\n" + strings.Repeat("  ", depth) + l.Text
		total += len(rendered[i])
	}
	if total <= limit {
		for _, r := range rendered {
			sb.WriteString(r)
		}
		return sb.String()
	}
	// The first line at or below the top of the viewport starts the
	// shown range. Every line is above it only when the page has
	// scrolled past its last line; the range then starts at the top.
	start := 0
	for start < len(d.Lines) && d.Lines[start].Top < d.ScrollY {
		start++
	}
	if start == len(d.Lines) {
		start = 0
	}
	room := limit - sb.Len() - noteRoom
	end := start
	for end < len(rendered) && len(rendered[end]) <= room {
		room -= len(rendered[end])
		end++
	}
	for start > 0 && len(rendered[start-1]) <= room {
		room -= len(rendered[start-1])
		start--
	}
	if start > 0 {
		fmt.Fprintf(&sb, "\n… %d lines above the current scroll position not shown; scroll up and snapshot again to read them", start)
	}
	for _, r := range rendered[start:end] {
		sb.WriteString(r)
	}
	if end < len(rendered) {
		fmt.Fprintf(&sb, "\n… %d lines below not shown; scroll down, or wait for what you need, and snapshot again", len(rendered)-end)
	}
	return sb.String()
}
