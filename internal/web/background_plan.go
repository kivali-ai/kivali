// Package web (background_plan.go) — the CEO's view of work in progress.
//
// The chip in the chat transcript is a dispatch log: rows appear when
// tasks are dispatched, show an activity string, and scroll away into
// history. That answers "what is happening right now" and not "how far
// along is this", which is the question someone watching delegated work
// actually has.
//
// The plan answers it, and the plan is a file the agent wrote —
// /files/background/plan.md. Nothing here decides what runs; this reads what
// the agent said it would do and shows it next to what is running and
// what has already run. A malformed plan costs legibility and never execution, which
// is the property that lets the format stay a convention rather than
// becoming a schema with a migration behind it.

package web

import (
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// maxPlanBytes caps what is read off disk for the panel. A plan is a
// page of checkboxes; anything larger is a document that wandered into
// the wrong filename, and rendering all of it would push the chat off
// the screen.
const maxPlanBytes = 64 << 10

// PlanItem is one checkbox line.
type PlanItem struct {
	Text string
	Done bool
}

// PlanView is the rendered plan, or Present=false when the agent has
// not written one. Absence is the normal state for an agent doing
// ordinary single-turn work — the panel simply does not appear.
type PlanView struct {
	Present   bool
	Title     string
	Body      string
	Items     []PlanItem
	Done      int
	Total     int
	UpdatedAt time.Time
}

// loadBackgroundPlan reads and parses the agent's plan file.
func loadBackgroundPlan(st *store.FSStore, slug string) PlanView {
	if st == nil || slug == "" {
		return PlanView{}
	}
	// background/ is the agent's to write, links included, and this
	// read runs on core: open without following any link, so a plan.md
	// pointing elsewhere on the volume is simply no plan.
	root := files.StorageRoot(filepath.Join(st.Root(), "agents", slug))
	f, err := files.OpenNoFollow(root, files.SharedWorkspaceDir+"/"+files.PlanFileName)
	if err != nil {
		return PlanView{}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return PlanView{}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPlanBytes))
	if err != nil {
		return PlanView{}
	}
	body := string(b)

	view := PlanView{
		Present:   true,
		Body:      body,
		UpdatedAt: info.ModTime(),
		Title:     planTitle(body),
	}
	view.Items = parsePlanItems(body)
	for _, it := range view.Items {
		if it.Done {
			view.Done++
		}
	}
	view.Total = len(view.Items)
	return view
}

// planTitle pulls the first markdown heading, falling back to the first
// non-empty line. Purely cosmetic — a plan without a heading still
// renders, it just gets a generic label.
func planTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			return strings.TrimSpace(strings.TrimLeft(t, "#"))
		}
		return t
	}
	return ""
}

// parsePlanItems finds markdown task-list lines anywhere in the plan.
//
// Loose on purpose. It accepts any list bullet, any indentation, and
// any non-space character as "checked", because the agent writing
// these is a language model and the cost of a strict parser is a plan
// that renders as prose the one time it writes `* [X]`. Lines that do
// not look like checkboxes are simply not items; the full body is
// rendered alongside regardless, so nothing is ever lost by failing to
// match.
func parsePlanItems(body string) []PlanItem {
	var out []PlanItem
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "- "), strings.HasPrefix(t, "* "), strings.HasPrefix(t, "+ "):
			t = t[2:]
		default:
			continue
		}
		t = strings.TrimSpace(t)
		if len(t) < 3 || t[0] != '[' || t[2] != ']' {
			continue
		}
		mark := t[1]
		text := strings.TrimSpace(t[3:])
		if text == "" {
			continue
		}
		out = append(out, PlanItem{Text: text, Done: mark != ' '})
	}
	return out
}
