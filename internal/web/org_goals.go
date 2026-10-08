package web

import (
	"sort"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// closedWeekWindow is how far back "closed this week" looks: a rolling
// seven days, not a calendar week, so the number never drops to zero
// on a Monday morning.
const closedWeekWindow = 7 * 24 * time.Hour

// assignmentSetCache holds the last assignment set read for the snapshot, tagged
// with the assignments version it was read under. ReadAssignmentSet opens every
// assignment file, and the snapshot rebuilds on every agent state change,
// so the set is reread only after a tracker write bumps the version.
type assignmentSetCache struct {
	mu      sync.Mutex
	version int64
	set     *assignments.Set
}

// bumpAssignmentsVersion records a tracker write. Called from onAssignmentWrite
// (every WriteAssignment) and after a restore replaces the assignment files
// without going through WriteAssignment.
func (s *Server) bumpAssignmentsVersion() { s.assignmentsVersion.Add(1) }

// assignmentSetCached returns the assignment set as of version, reading it from
// disk when the cached copy is older. The caller reads the version
// before calling, so a write racing the read leaves the cache tagged
// with the older version and the next call rereads. A read failure
// yields an empty set and caches nothing.
func (s *Server) assignmentSetCached(version int64) *assignments.Set {
	set, err := s.readAssignmentSetCached(version)
	if err != nil {
		return assignments.Build(nil)
	}
	return set
}

// currentAssignmentSet is the assignment set as of now, from the cache when no
// tracker write has landed since it was read. The set is shared with
// the snapshot: callers read it and never change it.
func (s *Server) currentAssignmentSet() (*assignments.Set, error) {
	return s.readAssignmentSetCached(s.assignmentsVersion.Load())
}

// readAssignmentSetCached is assignmentSetCached with the read failure returned.
func (s *Server) readAssignmentSetCached(version int64) (*assignments.Set, error) {
	c := &s.assignmentCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && c.version == version {
		return c.set, nil
	}
	set, err := s.Store.ReadAssignmentSet()
	if err != nil {
		return nil, err
	}
	c.set, c.version = set, version
	return set, nil
}

// goalsAndAssignmentReadouts derives the snapshot's goals and the two
// tracker readouts from one assignment set.
//
// A goal is an open top-level assignment (no parent) with Done-when
// conditions or parts. Its done/total count the conditions when it
// declares any (assignments.Set.Progress), else its parts with dropped
// parts left out of both — dropped work is neither progress nor
// remaining. Its blocked list is every dependency wait inside it: an
// open assignment in the goal's subtree naming an open blocked_by
// target. Waiting on one's own parts (which Set.Blockers also counts)
// is progress, not blockage, and is left out. Workers are the distinct
// assignees of its open parts at any depth.
//
// The blocked readout counts open assignments, not on hold, with such
// a wait; closedWeek counts assignments closed within closedWeekWindow
// of now. person names each blocker's assignee.
func goalsAndAssignmentReadouts(set *assignments.Set, now time.Time, person personFunc) (goals []apitypes.Goal, blocked, closedWeek int) {
	goals = []apitypes.Goal{}
	cut := now.Add(-closedWeekWindow)
	for _, id := range set.IDs() {
		iss, _ := set.Get(id)
		if !iss.Open() {
			if iss.Closed != nil && !iss.Closed.Before(cut) {
				closedWeek++
			}
			continue
		}
		if !set.Held(id) && len(dependencyWaits(set, iss)) > 0 {
			blocked++
		}
		if iss.Parent != 0 {
			continue
		}
		children := set.Parts(id)
		if len(iss.Acceptance) == 0 && len(children) == 0 {
			continue
		}
		goals = append(goals, goalFor(set, iss, children, person))
	}
	return goals, blocked, closedWeek
}

// goalFor builds one goal row. children is set.Children(iss.ID).
func goalFor(set *assignments.Set, iss *assignments.Assignment, children []int, person personFunc) apitypes.Goal {
	g := apitypes.Goal{
		ID:      iss.ID,
		Title:   iss.Title,
		Owner:   iss.Assignee,
		Blocked: []apitypes.GoalBlocker{},
		Workers: []string{},
	}
	if p, ok := set.Progress(iss.ID); ok {
		g.Done, g.Total = p.Satisfied, p.Total()
	} else {
		for _, c := range children {
			child, _ := set.Get(c)
			if child.Resolution == assignments.ResolutionDropped {
				continue
			}
			g.Total++
			if !child.Open() {
				g.Done++
			}
		}
	}
	descendants := set.OpenDescendants(iss.ID)
	for _, id := range append([]int{iss.ID}, descendants...) {
		waiter, _ := set.Get(id)
		for _, on := range dependencyWaits(set, waiter) {
			target, _ := set.Get(on)
			b := apitypes.GoalBlocker{ID: id, On: on, OnTitle: target.Title}
			if target.Assignee != "" {
				who := person(target.Assignee)
				b.OnAssignee = &who
			}
			g.Blocked = append(g.Blocked, b)
		}
	}
	seen := map[string]bool{}
	for _, id := range descendants {
		d, _ := set.Get(id)
		if d.Assignee != "" && !seen[d.Assignee] {
			seen[d.Assignee] = true
			g.Workers = append(g.Workers, d.Assignee)
		}
	}
	sort.Strings(g.Workers)
	return g
}

// dependencyWaits lists the open assignments iss names in blocked_by,
// ascending. An unknown target counts as satisfied, as in
// Set.Blockers.
func dependencyWaits(set *assignments.Set, iss *assignments.Assignment) []int {
	var out []int
	for _, b := range iss.BlockedBy {
		if t, ok := set.Get(b); ok && t.Open() {
			out = append(out, b)
		}
	}
	sort.Ints(out)
	return out
}

// spendWindow is how much usage history the spend cache holds: the
// seven-day readout plus a day of slack, so the cache stays a superset
// of what any later snapshot needs until the next append replaces it.
const spendWindow = 8 * 24 * time.Hour

// pricedUsage is one usage row reduced to what the readouts sum.
type pricedUsage struct {
	TS   time.Time
	Cost float64
}

// spendCache holds the priced usage window, tagged with the usage
// version it was read under. usage.jsonl is read (backwards, window
// only) once per append rather than once per snapshot; the sums are
// recomputed from the cached rows each time, since "today" and "the
// last seven days" move with the clock even when nothing is appended.
type spendCache struct {
	mu      sync.Mutex
	version int64
	loaded  bool
	rows    []pricedUsage
}

// bumpUsageVersion records a usage append (the store's post-append
// hook) or a restore that replaced usage.jsonl.
func (s *Server) bumpUsageVersion() { s.usageVersion.Add(1) }

// onUsageAppend is the store's post-append hook for usage.jsonl.
// Non-blocking: it bumps the version and pings the snapshot notifier
// so the spend readouts move.
func (s *Server) onUsageAppend() {
	s.bumpUsageVersion()
	s.NotifyOrgState()
}

// spendReadouts returns US dollars spent since 00:00 UTC on now's date
// and over the seven days before now.
//
// Priced from each row's token counts at the provider's rates (Price),
// NEVER from the stored CostUSD: that column mixes a session-cumulative
// gauge with per-run totals and is not summable (see the comment on
// store.UsageRecord.CostUSD). A row whose model has no price is left
// out, as the settings dashboard does, rather than counted as free.
func (s *Server) spendReadouts(now time.Time) (today, week float64) {
	rows := s.pricedUsageWindow(now)
	dayStart := now.UTC().Truncate(24 * time.Hour)
	weekStart := now.Add(-7 * 24 * time.Hour)
	for _, r := range rows {
		if !r.TS.Before(weekStart) && !r.TS.After(now) {
			week += r.Cost
		}
		if !r.TS.Before(dayStart) && !r.TS.After(now) {
			today += r.Cost
		}
	}
	return today, week
}

// pricedUsageWindow returns the cached priced rows, rereading
// usage.jsonl when an append has landed since they were read. The
// version is read before the file for the same reason as the assignment
// cache. A read failure yields no rows and caches nothing.
func (s *Server) pricedUsageWindow(now time.Time) []pricedUsage {
	version := s.usageVersion.Load()
	c := &s.spendCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded && c.version == version {
		return c.rows
	}
	records, err := s.Store.ReadUsageSince(now.Add(-spendWindow))
	if err != nil {
		return nil
	}
	rows := make([]pricedUsage, 0, len(records))
	for _, u := range records {
		cost, priced := priceUsage(s.Provider, u)
		if !priced {
			continue
		}
		rows = append(rows, pricedUsage{TS: u.TS, Cost: cost})
	}
	c.rows, c.version, c.loaded = rows, version, true
	return rows
}
