package web

import (
	"cmp"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The agent's Background and About tabs: what it has running in the
// background against its plan, and its three documents (role, habits,
// memory). Background reads two sources — the plan file and the
// subagent job registry.

func (s *Server) wireAPIAgentWorkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/{slug}/background", s.handleAPIAgentBackground)
	mux.HandleFunc("GET /api/v1/agents/{slug}/docs/{kind}", s.handleAPIAgentDocGet)
	mux.HandleFunc("PUT /api/v1/agents/{slug}/docs/{kind}", s.handleAPIAgentDocPut)
}

// agentWorkSlug reads {slug} and refuses what these endpoints cannot
// serve: a path-like slug is 404 (see plainSlug), and the CEO, who has
// no background work and no documents, is 400. Writes the error itself.
func agentWorkSlug(w http.ResponseWriter, r *http.Request) (string, bool) {
	slug := r.PathValue("slug")
	if !plainSlug(slug) {
		writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, "whoever sent you the link")
		return "", false
	}
	if slug == agent.CEOSlug {
		writeAPIError(w, http.StatusBadRequest, "the owner has no background work or documents", whoDevelopers)
		return "", false
	}
	return slug, true
}

// ---- background ----

// handleAPIAgentBackground serves GET /api/v1/agents/{slug}/background.
// An archived agent has nothing running and its plan left with it, so it
// is served as empty rather than refused: its tabs still open.
func (s *Server) handleAPIAgentBackground(w http.ResponseWriter, r *http.Request) {
	slug, ok := agentWorkSlug(w, r)
	if !ok {
		return
	}
	_, err := s.Store.GetAgent(slug)
	if errors.Is(err, store.ErrNotFound) {
		if _, aerr := s.Store.GetArchivedAgent(slug); aerr == nil {
			writeJSON(w, http.StatusOK, apitypes.Background{Sessions: []apitypes.BackgroundSession{}})
			return
		}
		writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, "whoever sent you the link")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoDevelopers)
		return
	}
	writeJSON(w, http.StatusOK, s.backgroundView(slug, s.clk().Now().UTC()))
}

// backgroundView assembles the Background tab for one live agent.
func (s *Server) backgroundView(slug string, now time.Time) apitypes.Background {
	var jobs []subagentJob
	if s.SubagentService != nil {
		jobs = s.SubagentService.listJobs(slug)
	}
	promptOf := func(id string) string {
		meta, err := readSubagentMeta(subagentMetaPath(s.Store.Root(), slug, id))
		if err != nil {
			return ""
		}
		return meta.Prompt
	}
	bg := buildBackground(slug, loadBackgroundPlan(s.Store, slug), jobs, promptOf, now)
	for i := range bg.Sessions {
		bg.Sessions[i].Model = s.modelLabel(bg.Sessions[i].Model)
	}
	return bg
}

// buildBackground is the Background tab from the plan and the job
// registry (oldest dispatch first, as listJobs returns it). promptOf
// reads a job's dispatch prompt; it is consulted only when the plan is
// present and the description does not name a step.
//
// A session's step. Nothing links a dispatched task to a plan line —
// the plan is a convention the agent writes, not a schema — so the step
// is read from what the agent wrote, in this order, and left absent
// when none says:
//
//  1. the description starts with the step's number ("2. List every
//     incident", "2) …", "2: …") or names it ("task 2", "step 2");
//  2. the dispatch prompt says which task the subagent owns ("You own
//     task 2"), the wording plan-and-fan-out teaches;
//  3. the description is the step's text.
//
// A worker a sub-lead dispatched inherits the sub-lead's step.
func buildBackground(slug string, plan PlanView, jobs []subagentJob, promptOf func(id string) string, now time.Time) apitypes.Background {
	out := apitypes.Background{Sessions: make([]apitypes.BackgroundSession, 0, len(jobs))}

	var titles []string
	if plan.Present {
		titles = make([]string, len(plan.Items))
		for i, it := range plan.Items {
			titles[i] = planStepTitle(it.Text)
		}
	}

	stepOf := make(map[string]int, len(jobs)) // job id → step n, absent when none
	byID := make(map[string]subagentJob, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	var resolve func(j subagentJob, seen int) int
	resolve = func(j subagentJob, seen int) int {
		if n, ok := stepOf[j.ID]; ok {
			return n
		}
		n := 0
		if j.CallerID != "" {
			// Bounded by the number of jobs, so a malformed caller loop
			// cannot recurse forever.
			if caller, ok := byID[j.CallerID]; ok && seen < len(jobs) {
				n = resolve(caller, seen+1)
			}
		} else if len(titles) > 0 {
			n = matchPlanStep(j, titles, promptOf)
		}
		stepOf[j.ID] = n
		return n
	}

	running, finished := 0, 0
	for _, j := range jobs {
		sess := backgroundSession(slug, j, now)
		if n := resolve(j, 0); n > 0 {
			sess.Step = &n
		}
		if j.terminal() {
			finished++
		} else {
			running++
		}
		out.Sessions = append(out.Sessions, sess)
	}

	if !plan.Present {
		return out
	}
	bp := &apitypes.BackgroundPlan{
		Title:    plan.Title,
		Steps:    make([]apitypes.PlanStep, 0, len(plan.Items)),
		Done:     plan.Done,
		Total:    plan.Total,
		Running:  running,
		Finished: finished,
	}
	for i, it := range plan.Items {
		n := i + 1
		st := planStepState(it.Done, n, jobs, stepOf)
		if st == apitypes.PlanStepStateNeedsHelp {
			bp.NeedsHelp++
		}
		bp.Steps = append(bp.Steps, apitypes.PlanStep{N: n, Title: titles[i], State: st})
	}
	out.Plan = bp
	return out
}

// planStepState is one step's state: ticked is done; otherwise running
// while any session under it is live, needs_help when the latest
// session the agent itself dispatched for it failed, else idle. A
// cancelled session does not ask for help: someone called it off.
func planStepState(ticked bool, n int, jobs []subagentJob, stepOf map[string]int) apitypes.PlanStepState {
	if ticked {
		return apitypes.PlanStepStateDone
	}
	live, latestFailed := false, false
	for _, j := range jobs {
		if stepOf[j.ID] != n {
			continue
		}
		if !j.terminal() {
			live = true
		}
		if j.CallerID == "" {
			latestFailed = j.State == subagentJobFailed
		}
	}
	switch {
	case live:
		return apitypes.PlanStepStateRunning
	case latestFailed:
		return apitypes.PlanStepStateNeedsHelp
	default:
		return apitypes.PlanStepStateIdle
	}
}

var (
	// planLeadingNumber is a plan line's or description's own number:
	// "2. …", "2) …", "2: …".
	planLeadingNumber = regexp.MustCompile(`^\s*(\d+)\s*[.):]\s+`)
	// planStepRef names a step anywhere in a description.
	planStepRef = regexp.MustCompile(`(?i)\b(?:task|step)\s*#?(\d+)\b`)
	// planOwnsStep is how plan-and-fan-out tells a subagent its part.
	planOwnsStep = regexp.MustCompile(`(?i)\bowns?\s+(?:task|step)\s*#?(\d+)\b`)
)

// planStepTitle is a plan line without its own leading number, which
// the step's n already carries.
func planStepTitle(text string) string {
	if loc := planLeadingNumber.FindStringIndex(text); loc != nil {
		if t := strings.TrimSpace(text[loc[1]:]); t != "" {
			return t
		}
	}
	return text
}

// matchPlanStep is the step a tier-1 job works on, or 0. See
// buildBackground for the rules.
func matchPlanStep(j subagentJob, titles []string, promptOf func(string) string) int {
	valid := func(m []string) int {
		if len(m) < 2 {
			return 0
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 || n > len(titles) {
			return 0
		}
		return n
	}
	if loc := planLeadingNumber.FindStringSubmatchIndex(j.Description); loc != nil && !startsWithMonth(j.Description[loc[1]:]) {
		if n := valid([]string{"", j.Description[loc[2]:loc[3]]}); n > 0 {
			return n
		}
	}
	if n := valid(planStepRef.FindStringSubmatch(j.Description)); n > 0 {
		return n
	}
	if promptOf != nil {
		if n := valid(planOwnsStep.FindStringSubmatch(promptOf(j.ID))); n > 0 {
			return n
		}
	}
	desc := normalizeStepText(j.Description)
	if desc == "" {
		return 0
	}
	for i, t := range titles {
		if normalizeStepText(t) == desc {
			return i + 1
		}
	}
	return 0
}

// startsWithMonth reports whether s opens with a month name, which makes
// a leading "2. " a date ("2. October review") rather than a step number.
func startsWithMonth(s string) bool {
	word := strings.ToLower(strings.TrimRight(strings.SplitN(strings.TrimSpace(s), " ", 2)[0], ".,"))
	if word == "sept" {
		return true
	}
	for _, m := range []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"} {
		if word == m || word == m[:3] {
			return true
		}
	}
	return false
}

func normalizeStepText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// sessionState maps a job's registry state to the app's.
//
//	queued    → queued
//	running   → running
//	completed → done
//	failed    → errored (with the failure as Error)
//	cancelled → cancelled
func sessionState(state string) apitypes.SessionState {
	switch state {
	case subagentJobQueued:
		return apitypes.SessionStateQueued
	case subagentJobRunning:
		return apitypes.SessionStateRunning
	case subagentJobCompleted:
		return apitypes.SessionStateDone
	case subagentJobCancelled:
		return apitypes.SessionStateCancelled
	default:
		return apitypes.SessionStateErrored
	}
}

// backgroundSession is one job as the app shows it. Its clock runs
// from admission, or dispatch if it never started,
// to its end, or now while it is live.
func backgroundSession(slug string, j subagentJob, now time.Time) apitypes.BackgroundSession {
	start := j.QueuedAt
	if !j.StartedAt.IsZero() {
		start = j.StartedAt
	}
	end := now
	var ended *time.Time
	if j.terminal() && !j.EndedAt.IsZero() {
		e := j.EndedAt
		ended, end = &e, e
	}
	elapsed := int64(0)
	if !start.IsZero() && end.After(start) {
		elapsed = int64(end.Sub(start) / time.Second)
	}
	sess := apitypes.BackgroundSession{
		ID:       j.ID,
		Kind:     apitypes.SessionKindSubagent,
		State:    sessionState(j.State),
		Title:    j.Description,
		Model:    j.Model, // the id; backgroundView labels it
		Effort:   j.Effort,
		ElapsedS: elapsed,
		Started:  start,
		Ended:    ended,
		URL:      "/agents/" + url.PathEscape(slug) + "/subagents/" + url.PathEscape(j.ID),
	}
	if sess.State == apitypes.SessionStateErrored {
		e := cmp.Or(strings.TrimSpace(j.Err), "The task failed without saying why")
		sess.Error = &e
	}
	if !j.terminal() && j.Activity != "" {
		a := j.Activity
		sess.Activity = &a
	}
	if j.CallerID != "" {
		c := j.CallerID
		sess.CallerID = &c
	}
	return sess
}

// ---- documents ----

// apiAgentDocKinds maps the API's document names to the stored ones.
// The API says habits for what is stored as operating principles.
var apiAgentDocKinds = map[apitypes.AgentDocKind]agentDocKind{
	apitypes.AgentDocKindRole:   agentRoleDoc,
	apitypes.AgentDocKindHabits: agentHabitsDoc,
	apitypes.AgentDocKindMemory: agentMemoryDoc,
}

// agentDocTarget resolves {slug} and {kind} to a live agent's document.
// Live agents only: an archived agent's documents moved with its
// directory, so a read at the live path would report a real identity
// as empty. Writes the error itself.
func (s *Server) agentDocTarget(w http.ResponseWriter, r *http.Request) (string, apitypes.AgentDocKind, agentDocKind, bool) {
	slug, ok := agentWorkSlug(w, r)
	if !ok {
		return "", "", agentDocKind{}, false
	}
	name := apitypes.AgentDocKind(r.PathValue("kind"))
	kind, ok := apiAgentDocKinds[name]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "no document is called "+string(name), "whoever sent you the link")
		return "", "", agentDocKind{}, false
	}
	if _, err := s.Store.GetAgent(slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "no agent on the team is called "+slug, "whoever sent you the link")
			return "", "", agentDocKind{}, false
		}
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoDevelopers)
		return "", "", agentDocKind{}, false
	}
	return slug, name, kind, true
}

// handleAPIAgentDocGet serves GET /api/v1/agents/{slug}/docs/{kind}. A
// document never written is empty, with a null updated_at.
func (s *Server) handleAPIAgentDocGet(w http.ResponseWriter, r *http.Request) {
	slug, name, kind, ok := s.agentDocTarget(w, r)
	if !ok {
		return
	}
	s.writeAgentDoc(w, slug, name, kind)
}

// handleAPIAgentDocPut serves PUT /api/v1/agents/{slug}/docs/{kind}:
// replace the document, then answer as GET does. Validation is
// saveAgentDoc's.
//
// The body is held to decodeJSON's apiMaxJSONBody (1 MB), deliberately
// not raised here: every one of these documents is inlined into each
// Claude call's system prompt, so one near 1 MB (~250k tokens) is
// already unusable, and the largest shipped document, the seed
// handbook, is about 42 KB. Line endings are stored as sent; a
// JSON client sending a textarea's value sends LF, since only a form
// submission converts it to CRLF.
func (s *Server) handleAPIAgentDocPut(w http.ResponseWriter, r *http.Request) {
	slug, name, kind, ok := s.agentDocTarget(w, r)
	if !ok {
		return
	}
	var req apitypes.AgentDocPut
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	switch err := s.saveAgentDoc(kind, slug, req.Content); {
	case errors.Is(err, errAgentDocRequired):
		writeAPIError(w, http.StatusBadRequest, "the "+string(name)+" can't be empty", "you")
		return
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "no agent on the team is called "+slug, "whoever sent you the link")
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the "+string(name)+" could not be saved", whoDevelopers)
		return
	}
	s.writeAgentDoc(w, slug, name, kind)
}

// writeAgentDoc reads one document and writes it as the response.
func (s *Server) writeAgentDoc(w http.ResponseWriter, slug string, name apitypes.AgentDocKind, kind agentDocKind) {
	content, err := kind.read(s.Store, slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "the "+string(name)+" could not be read", whoDevelopers)
		return
	}
	doc := apitypes.AgentDoc{Kind: name, Content: content, Stats: agentDocStats(name, content)}
	if at, err := kind.updatedAt(s.Store, slug); err == nil {
		doc.UpdatedAt = &at
	}
	writeJSON(w, http.StatusOK, doc)
}

// agentDocStats counts a document's lines (a trailing newline does not
// start another) and characters, and for memory its notes.
func agentDocStats(name apitypes.AgentDocKind, content string) apitypes.AgentDocStats {
	st := apitypes.AgentDocStats{Chars: utf8.RuneCountInString(content)}
	if content != "" {
		st.Lines = strings.Count(strings.TrimSuffix(content, "\n"), "\n") + 1
	}
	if name == apitypes.AgentDocKindMemory {
		n := memoryNoteCount(content)
		st.Notes = &n
	}
	return st
}

// memoryNoteCount is how many entries the agent memory holds. An entry
// of the durable-memory format cites where it came from — [[ep:<ts>]]
// or [[stated]] — so a line carrying a citation is a note. A memory
// written before citations (or by hand without them) counts its list
// items instead.
func memoryNoteCount(content string) int {
	cited, items := 0, 0
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "[[ep:") || strings.Contains(line, "[[stated]]") {
			cited++
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ") || planLeadingNumber.MatchString(t) {
			items++
		}
	}
	if cited > 0 {
		return cited
	}
	return items
}
