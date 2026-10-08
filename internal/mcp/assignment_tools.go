package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/tracker"
)

// Assignment tool names, shared with the native-API definitions in
// internal/agent/assignment_tools.go.
const (
	AssignmentCreateToolName = agent.AssignmentCreateToolName
	AssignmentUpdateToolName = agent.AssignmentUpdateToolName
	AssignmentCloseToolName  = agent.AssignmentCloseToolName
	AssignmentReopenToolName = agent.AssignmentReopenToolName
	AssignmentListToolName   = agent.AssignmentListToolName
	AssignmentViewToolName   = agent.AssignmentViewToolName
)

// AssignmentTools returns the six assignment tools dispatched through
// d. The four writes are not read-only: each changes a file and may
// wake an agent. Full-agent toolkit only; subagents do not see the
// tracker.
func AssignmentTools(d StateDispatcher) []Tool {
	if d == nil {
		return nil
	}
	return []Tool{
		stateTool(d, AssignmentCreateToolName, agent.AssignmentCreateDescription, agent.AssignmentCreateInputSchema),
		stateTool(d, AssignmentUpdateToolName, agent.AssignmentUpdateDescription, agent.AssignmentUpdateInputSchema),
		stateTool(d, AssignmentCloseToolName, agent.AssignmentCloseDescription, agent.AssignmentCloseInputSchema),
		stateTool(d, AssignmentReopenToolName, agent.AssignmentReopenDescription, agent.AssignmentReopenInputSchema),
		readOnlyStateTool(d, AssignmentListToolName, agent.AssignmentListDescription, agent.AssignmentListInputSchema),
		readOnlyStateTool(d, AssignmentViewToolName, agent.AssignmentViewDescription, agent.AssignmentViewInputSchema),
	}
}

// isAssignmentTool reports whether name is one of the six.
func isAssignmentTool(name string) bool {
	return agent.IsAssignmentTool(name)
}

// dispatchAssignmentTool runs one assignment tool for the caller. A
// rule refusal is a
// tool-level error carrying the rule's text; anything else is an
// internal failure, reported as such.
func dispatchAssignmentTool(deps StateDispatchDeps, tool string, raw json.RawMessage) (string, bool) {
	if deps.Tracker == nil {
		return tool + ": the assignment tracker is not configured on this deployment", true
	}
	ctx := context.Background()
	switch tool {
	case AssignmentCreateToolName:
		return renderAssignmentCreate(ctx, deps.Tracker, deps.Slug, raw)
	case AssignmentUpdateToolName:
		return renderAssignmentUpdate(ctx, deps.Tracker, deps.Slug, raw)
	case AssignmentCloseToolName:
		return renderAssignmentClose(ctx, deps.Tracker, deps.Slug, raw)
	case AssignmentReopenToolName:
		return renderAssignmentReopen(ctx, deps.Tracker, deps.Slug, raw)
	case AssignmentListToolName:
		return renderAssignmentList(deps.Tracker, deps.Slug, raw)
	case AssignmentViewToolName:
		return renderAssignmentView(deps.Tracker, raw)
	}
	return "unknown assignment tool: " + tool, true
}

func assignmentErr(tool string, err error) (string, bool) {
	if assignments.IsRefusal(err) {
		return tool + ": " + err.Error(), true
	}
	return tool + ": internal error: " + err.Error(), true
}

func decodeAssignmentInput(tool string, raw json.RawMessage, v any) (string, bool, bool) {
	if len(raw) == 0 {
		return "", false, true
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return tool + ": invalid input: " + err.Error(), true, false
	}
	return "", false, true
}

func renderAssignmentCreate(ctx context.Context, t *tracker.Service, caller string, raw json.RawMessage) (string, bool) {
	var in struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Assignee    string   `json:"assignee"`
		Parent      int      `json:"parent"`
		BlockedBy   []int    `json:"blocked_by"`
		Acceptance  []string `json:"acceptance"`
		Satisfies   []string `json:"satisfies"`
	}
	if body, isErr, ok := decodeAssignmentInput(AssignmentCreateToolName, raw, &in); !ok {
		return body, isErr
	}
	ch, err := t.Create(ctx, caller, assignments.CreateInput{
		Title: in.Title, Body: in.Description, Assignee: strings.ToLower(strings.TrimSpace(in.Assignee)),
		Parent: in.Parent, BlockedBy: in.BlockedBy, Acceptance: in.Acceptance, Satisfies: in.Satisfies,
	})
	if err != nil {
		return assignmentErr(AssignmentCreateToolName, err)
	}
	iss := ch.Assignment
	var b strings.Builder
	fmt.Fprintf(&b, "Opened %s %q — assignee %s, %s.", iss.Ref(), iss.Title, iss.Assignee, ch.Set.State(iss.ID))
	if iss.Parent != 0 {
		fmt.Fprintf(&b, " Part of %s", assignments.Ref(iss.Parent))
		if len(iss.Satisfies) > 0 {
			fmt.Fprintf(&b, "; counts toward %s", quoteItems(iss.Satisfies))
		}
		b.WriteString(".")
	}
	if len(iss.Acceptance) > 0 {
		fmt.Fprintf(&b, " Done when: %s.", quoteItems(iss.Acceptance))
	}
	b.WriteString(wakeSummary(caller, ch.Wakes))
	return b.String(), false
}

// quoteItems renders item names the way the tools print them.
func quoteItems(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = fmt.Sprintf("%q", it)
	}
	return strings.Join(parts, ", ")
}

func renderAssignmentUpdate(ctx context.Context, t *tracker.Service, caller string, raw json.RawMessage) (string, bool) {
	var in struct {
		ID               int      `json:"id"`
		Title            *string  `json:"title"`
		Description      *string  `json:"description"`
		Assignee         *string  `json:"assignee"`
		Parent           *int     `json:"parent"`
		AddBlockedBy     []int    `json:"add_blocked_by"`
		RemoveBlockedBy  []int    `json:"remove_blocked_by"`
		Hold             *bool    `json:"hold"`
		AddAcceptance    []string `json:"add_acceptance"`
		RemoveAcceptance []string `json:"remove_acceptance"`
		AddSatisfies     []string `json:"add_satisfies"`
		RemoveSatisfies  []string `json:"remove_satisfies"`
		Note             string   `json:"note"`
	}
	if body, isErr, ok := decodeAssignmentInput(AssignmentUpdateToolName, raw, &in); !ok {
		return body, isErr
	}
	if in.Assignee != nil {
		a := strings.ToLower(strings.TrimSpace(*in.Assignee))
		in.Assignee = &a
	}
	ch, err := t.Update(ctx, caller, in.ID, assignments.UpdateInput{
		Title: in.Title, Body: in.Description, Assignee: in.Assignee, Parent: in.Parent,
		AddBlockedBy: in.AddBlockedBy, RemoveBlockedBy: in.RemoveBlockedBy, Hold: in.Hold, Note: in.Note,
		AddAcceptance: in.AddAcceptance, RemoveAcceptance: in.RemoveAcceptance,
		AddSatisfies: in.AddSatisfies, RemoveSatisfies: in.RemoveSatisfies,
	})
	if err != nil {
		return assignmentErr(AssignmentUpdateToolName, err)
	}
	iss := ch.Assignment
	ops := make([]string, 0, len(ch.Entries))
	for _, e := range ch.Entries {
		ops = append(ops, describeEntry(e))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Updated %s %q: %s. Now %s, assignee %s.", iss.Ref(), iss.Title, strings.Join(ops, "; "), ch.Set.State(iss.ID), iss.Assignee)
	b.WriteString(wakeSummary(caller, ch.Wakes))
	return b.String(), false
}

func renderAssignmentClose(ctx context.Context, t *tracker.Service, caller string, raw json.RawMessage) (string, bool) {
	var in struct {
		ID         int    `json:"id"`
		Resolution string `json:"resolution"`
		Outcome    string `json:"outcome"`
	}
	if body, isErr, ok := decodeAssignmentInput(AssignmentCloseToolName, raw, &in); !ok {
		return body, isErr
	}
	ch, err := t.Close(ctx, caller, in.ID, assignments.Resolution(strings.ToLower(strings.TrimSpace(in.Resolution))), in.Outcome)
	if err != nil {
		return assignmentErr(AssignmentCloseToolName, err)
	}
	iss := ch.Assignment
	var b strings.Builder
	fmt.Fprintf(&b, "Closed %s %q as %s.", iss.Ref(), iss.Title, iss.Resolution)
	if w := assignments.UnmetWarning(ch.Unmet); w != "" {
		b.WriteString(" " + w)
	}
	if freed := readyBecause(ch); len(freed) > 0 {
		fmt.Fprintf(&b, " Now ready: %s.", strings.Join(freed, ", "))
	}
	b.WriteString(wakeSummary(caller, ch.Wakes))
	return b.String(), false
}

func renderAssignmentReopen(ctx context.Context, t *tracker.Service, caller string, raw json.RawMessage) (string, bool) {
	var in struct {
		ID   int    `json:"id"`
		Note string `json:"note"`
	}
	if body, isErr, ok := decodeAssignmentInput(AssignmentReopenToolName, raw, &in); !ok {
		return body, isErr
	}
	ch, err := t.Reopen(ctx, caller, in.ID, in.Note)
	if err != nil {
		return assignmentErr(AssignmentReopenToolName, err)
	}
	iss := ch.Assignment
	var b strings.Builder
	fmt.Fprintf(&b, "Reopened %s %q — assignee %s, %s.", iss.Ref(), iss.Title, iss.Assignee, ch.Set.State(iss.ID))
	b.WriteString(wakeSummary(caller, ch.Wakes))
	return b.String(), false
}

// readyBecause names the assignments a change made ready, other than
// the changed assignment itself.
func readyBecause(ch assignments.Change) []string {
	var out []string
	for _, w := range ch.Wakes {
		for _, line := range strings.Split(w.Body, "\n") {
			if i := strings.Index(line, " is now ready"); i > 0 && strings.HasPrefix(line, "#") {
				out = append(out, line[:i])
			}
		}
	}
	return out
}

// wakeSummary says who the change woke and by which path, so the
// caller knows whether to expect anything and when. A hold is told,
// not woken: it reaches an agent mid-turn at once and an idle one
// with their next wake.
func wakeSummary(caller string, wakes []assignments.Wake) string {
	if len(wakes) == 0 {
		return " Nobody is woken."
	}
	hold := wakes[0].Op == assignments.OpHeld
	parts := make([]string, 0, len(wakes))
	for _, w := range wakes {
		switch {
		case w.To == assignments.CEO:
			parts = append(parts, "the owner (in their inbox now)")
		case caller == assignments.CEO && hold:
			parts = append(parts, w.To+" (now if mid-turn, else with their next wake)")
		case caller == assignments.CEO:
			parts = append(parts, w.To+" (now)")
		default:
			parts = append(parts, w.To+" (when the owner releases it)")
		}
	}
	if hold {
		return " Told to stop: " + strings.Join(parts, ", ") + "."
	}
	return " Woken: " + strings.Join(parts, ", ") + "."
}

func describeEntry(e assignments.Entry) string {
	switch e.Op {
	case assignments.OpAmended:
		return "amended " + strings.ReplaceAll(e.Fields, ",", " and ")
	case assignments.OpAssigned:
		return fmt.Sprintf("reassigned from %s to %s", e.From, e.To)
	case assignments.OpParent:
		if e.Ref == 0 {
			return "made a goal"
		}
		return "made part of " + assignments.Ref(e.Ref)
	case assignments.OpBlocked:
		return "now waits on " + assignments.Ref(e.Ref)
	case assignments.OpUnblocked:
		return "no longer waits on " + assignments.Ref(e.Ref)
	case assignments.OpHeld:
		return "put on hold with everything under it"
	case assignments.OpResumed:
		return "resumed with everything under it"
	case assignments.OpAcceptance:
		if len(e.Items) == 0 {
			return "Done-when conditions cleared"
		}
		return "Done-when conditions now " + quoteItems(e.Items)
	case assignments.OpSatisfies:
		if len(e.Items) == 0 {
			return "counts toward nothing now"
		}
		return "counts toward " + quoteItems(e.Items)
	}
	return string(e.Op)
}

func renderAssignmentList(t *tracker.Service, caller string, raw json.RawMessage) (string, bool) {
	var in struct {
		Assignee string `json:"assignee"`
		Creator  string `json:"creator"`
		Parent   int    `json:"parent"`
		Status   string `json:"status"`
		Ready    *bool  `json:"ready"`
	}
	if body, isErr, ok := decodeAssignmentInput(AssignmentListToolName, raw, &in); !ok {
		return body, isErr
	}
	set, err := t.Set()
	if err != nil {
		return assignmentErr(AssignmentListToolName, err)
	}
	f := assignments.Filter{
		Assignee: strings.ToLower(strings.TrimSpace(in.Assignee)),
		Creator:  strings.ToLower(strings.TrimSpace(in.Creator)),
		Parent:   in.Parent,
		Status:   strings.ToLower(strings.TrimSpace(in.Status)),
		Ready:    in.Ready,
	}
	switch f.Status {
	case "", "open", "closed", "all":
	default:
		return AssignmentListToolName + ": status must be open, closed or all", true
	}
	if f.Assignee == "" && f.Creator == "" && f.Parent == 0 {
		f.Assignee = caller
	}
	if f.Assignee == "any" {
		f.Assignee = ""
	}
	var parts []string
	if f.Assignee != "" {
		parts = append(parts, "assignee="+f.Assignee)
	}
	if f.Creator != "" {
		parts = append(parts, "creator="+f.Creator)
	}
	if f.Parent != 0 {
		parts = append(parts, "parent="+assignments.Ref(f.Parent))
	}
	status := f.Status
	if status == "" {
		status = "open"
	}
	parts = append(parts, "status="+status)
	if f.Ready != nil {
		parts = append(parts, fmt.Sprintf("ready=%v", *f.Ready))
	}
	list := set.Query(f)
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	var b strings.Builder
	fmt.Fprintf(&b, "Assignments snapshot as of %s (%s): %d match", now, strings.Join(parts, ", "), len(list))
	if len(list) == 0 {
		b.WriteString(".\n")
		return b.String(), false
	}
	b.WriteString(".\n\n")
	for _, iss := range list {
		b.WriteString("- ")
		b.WriteString(set.Line(iss))
		b.WriteByte('\n')
	}
	b.WriteString("\nOpen one with assignment_view.\n")
	return b.String(), false
}

func renderAssignmentView(t *tracker.Service, raw json.RawMessage) (string, bool) {
	var in struct {
		ID int `json:"id"`
	}
	if body, isErr, ok := decodeAssignmentInput(AssignmentViewToolName, raw, &in); !ok {
		return body, isErr
	}
	set, err := t.Set()
	if err != nil {
		return assignmentErr(AssignmentViewToolName, err)
	}
	iss, ok := set.Get(in.ID)
	if !ok {
		return fmt.Sprintf("%s: no assignment %s; assignment_list shows what exists", AssignmentViewToolName, assignments.Ref(in.ID)), true
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	var b strings.Builder
	fmt.Fprintf(&b, "Assignment %s %q — snapshot as of %s\n", iss.Ref(), iss.Title, now)
	fmt.Fprintf(&b, "  state: %s\n", set.State(iss.ID))
	fmt.Fprintf(&b, "  assignee: %s\n", iss.Assignee)
	fmt.Fprintf(&b, "  creator: %s\n", iss.Creator)
	fmt.Fprintf(&b, "  created: %s · updated: %s\n", iss.Created.UTC().Format("2006-01-02 15:04"), iss.Updated.UTC().Format("2006-01-02 15:04"))
	if iss.Parent != 0 {
		if p, ok := set.Get(iss.Parent); ok {
			fmt.Fprintf(&b, "  part of: %s %q (assignee %s, %s)\n", p.Ref(), p.Title, p.Assignee, set.State(p.ID))
		} else {
			fmt.Fprintf(&b, "  part of: %s (missing)\n", assignments.Ref(iss.Parent))
		}
		if len(iss.Satisfies) > 0 {
			fmt.Fprintf(&b, "  counts toward: %s of %s\n", quoteItems(iss.Satisfies), assignments.Ref(iss.Parent))
		}
	}
	if items := set.Items(iss.ID); len(items) > 0 {
		p, _ := set.Progress(iss.ID)
		fmt.Fprintf(&b, "  done when (%s):\n", p)
		for _, it := range items {
			fmt.Fprintf(&b, "    [%s] %q", it.State(), it.Name)
			switch it.State() {
			case assignments.ConditionMet:
				fmt.Fprintf(&b, " — by %s", refsWithTitles(set, it.SatisfiedBy))
			case assignments.ConditionClaimed:
				fmt.Fprintf(&b, " — claimed by %s", refsWithTitles(set, it.ClaimedBy))
			default:
				b.WriteString(" — nothing opened names it")
			}
			b.WriteByte('\n')
		}
	}
	if parts := set.Parts(iss.ID); len(parts) > 0 {
		fmt.Fprintf(&b, "  parts (%d, %d open):\n", len(parts), len(set.OpenParts(iss.ID)))
		for _, c := range parts {
			ci, _ := set.Get(c)
			fmt.Fprintf(&b, "    %s %q — assignee %s, %s", ci.Ref(), ci.Title, ci.Assignee, set.State(c))
			if len(ci.Satisfies) > 0 {
				fmt.Fprintf(&b, ", counts toward %s", quoteItems(ci.Satisfies))
			}
			b.WriteByte('\n')
		}
	}
	if len(iss.BlockedBy) > 0 {
		parts := make([]string, 0, len(iss.BlockedBy))
		for _, bid := range iss.BlockedBy {
			if bi, ok := set.Get(bid); ok {
				parts = append(parts, fmt.Sprintf("%s %q (assignee %s, %s)", bi.Ref(), bi.Title, bi.Assignee, set.State(bid)))
			} else {
				parts = append(parts, assignments.Ref(bid)+" (missing)")
			}
		}
		fmt.Fprintf(&b, "  waits on: %s\n", strings.Join(parts, "; "))
	}
	if blocks := set.Blocks(iss.ID); len(blocks) > 0 {
		parts := make([]string, 0, len(blocks))
		for _, bid := range blocks {
			bi, _ := set.Get(bid)
			parts = append(parts, fmt.Sprintf("%s %q (assignee %s)", bi.Ref(), bi.Title, bi.Assignee))
		}
		fmt.Fprintf(&b, "  holds up: %s\n", strings.Join(parts, "; "))
	}
	if !iss.Open() {
		fmt.Fprintf(&b, "  closed: %s as %s\n", iss.Closed.UTC().Format("2006-01-02 15:04"), iss.Resolution)
		fmt.Fprintf(&b, "  outcome: %s\n", iss.Outcome)
	}
	b.WriteString("\nDescription:\n")
	if strings.TrimSpace(iss.Body) == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(iss.Body)
		b.WriteString("\n")
	}
	b.WriteString("\nLog:\n")
	for _, e := range iss.Log {
		b.WriteString("  ")
		b.WriteString(logLine(e))
		b.WriteByte('\n')
	}
	return b.String(), false
}

// refsWithTitles renders "#12 (alice)" style references for the
// parts a Done-when table names.
func refsWithTitles(set *assignments.Set, ids []int) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if ci, ok := set.Get(id); ok {
			parts = append(parts, fmt.Sprintf("%s (%s)", ci.Ref(), ci.Assignee))
			continue
		}
		parts = append(parts, assignments.Ref(id))
	}
	return strings.Join(parts, ", ")
}

// logLine renders one log entry for the model: when, who, what, why.
func logLine(e assignments.Entry) string {
	when := e.TS.UTC().Format("2006-01-02 15:04")
	var what string
	switch e.Op {
	case assignments.OpCreated:
		what = "created, assigned to " + e.To
	case assignments.OpAssigned:
		what = fmt.Sprintf("reassigned from %s to %s", e.From, e.To)
	case assignments.OpAmended:
		what = "amended " + strings.ReplaceAll(e.Fields, ",", " and ")
	case assignments.OpParent:
		if e.Ref == 0 {
			what = "made a goal"
		} else {
			what = "made part of " + assignments.Ref(e.Ref)
		}
	case assignments.OpBlocked:
		what = "added blocker " + assignments.Ref(e.Ref)
	case assignments.OpUnblocked:
		what = "removed blocker " + assignments.Ref(e.Ref)
	case assignments.OpClosed:
		what = "closed as " + string(e.Resolution)
		if len(e.Unmet) > 0 {
			what += fmt.Sprintf(" with %d Done-when condition%s unmet (%s)", len(e.Unmet), plural(len(e.Unmet)), quoteItems(e.Unmet))
		}
	case assignments.OpReopened:
		what = "reopened"
	case assignments.OpHeld:
		what = "put on hold"
	case assignments.OpResumed:
		what = "resumed"
	case assignments.OpAcceptance:
		what = "set the Done-when conditions to " + quoteItems(e.Items)
		if len(e.Items) == 0 {
			what = "cleared the Done-when conditions"
		}
	case assignments.OpSatisfies:
		what = "set what it counts toward to " + quoteItems(e.Items)
		if len(e.Items) == 0 {
			what = "cleared what it counts toward"
		}
	default:
		what = string(e.Op)
	}
	line := fmt.Sprintf("%s %s %s", when, e.By, what)
	if e.Note != "" {
		line += ": " + e.Note
	}
	return line
}
