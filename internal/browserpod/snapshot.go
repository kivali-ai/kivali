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

// snapLine is one line of the page at a depth.
type snapLine struct {
	Depth int
	Text  string
}

func (l *snapLine) UnmarshalJSON(b []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil {
		return err
	}
	if len(pair) != 2 {
		return fmt.Errorf("snapshot line has %d parts", len(pair))
	}
	if err := json.Unmarshal(pair[0], &l.Depth); err != nil {
		return err
	}
	return json.Unmarshal(pair[1], &l.Text)
}

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

// formatSnapshot renders the page header and the indented lines, cut
// to max characters with a line saying how many were left out.
func formatSnapshot(d snapshotData, max int) string {
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
	for i, l := range d.Lines {
		depth := l.Depth
		if depth < 0 {
			depth = 0
		}
		line := "\n" + strings.Repeat("  ", depth) + l.Text
		if sb.Len()+len(line) > max {
			fmt.Fprintf(&sb, "\n… %d more lines not shown; scroll, or wait for what you need, and snapshot again", len(d.Lines)-i)
			break
		}
		sb.WriteString(line)
	}
	return sb.String()
}
