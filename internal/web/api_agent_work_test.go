package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// agentWorkDo serves one request through the agent-work routes behind
// the API's own header, origin and fallback layers. The session layer
// is auth's and is covered by the API's middleware tests.
func agentWorkDo(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.wireAPIAgentWorkRoutes(mux)
	mux.Handle("/api/", apiFallback(mux))
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	rr := httptest.NewRecorder()
	apiHeaders(requireSameOrigin(mux)).ServeHTTP(rr, req)
	return rr
}

func seedAlice(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func getBackground(t *testing.T, srv *Server) apitypes.Background {
	t.Helper()
	rr := agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/alice/background", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d (body %s)", rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.Background](t, rr)
}

// With no plan the tab still lists every session.
func TestAPIBackgroundNoPlanTwoSessions(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)
	started := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	seedJob(t, srv, &subagentJob{
		ID: "done1", Parent: "alice", Description: "survey the corpus", Model: provider.MockModelLarge, Effort: "high",
		State: subagentJobCompleted, QueuedAt: started, StartedAt: started, EndedAt: started.Add(4 * time.Minute), Depth: 1,
		Activity: "running file_view", // stale on a finished job; must not surface
	})
	seedJob(t, srv, &subagentJob{
		ID: "live1", Parent: "alice", Description: "price the parts", Model: provider.MockModelRetired, Effort: "low",
		State: subagentJobRunning, QueuedAt: started.Add(time.Minute), StartedAt: started.Add(time.Minute),
		Activity: "thinking", Depth: 1,
	})

	bg := getBackground(t, srv)
	if bg.Plan != nil {
		t.Errorf("plan = %+v, want absent with no plan file", bg.Plan)
	}
	if len(bg.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(bg.Sessions))
	}
	done, live := bg.Sessions[0], bg.Sessions[1]
	if done.ID != "done1" || done.State != apitypes.SessionStateDone || done.ElapsedS != 240 || done.Ended == nil || done.Activity != nil {
		t.Errorf("finished session = %+v", done)
	}
	if done.Kind != apitypes.SessionKindSubagent || done.Model != "Mock Large" || done.Effort != "high" {
		t.Errorf("finished session kind/model/effort = %q %q %q", done.Kind, done.Model, done.Effort)
	}
	if done.URL != "/agents/alice/subagents/done1" || done.Step != nil || done.Error != nil {
		t.Errorf("finished session url/step/error = %q %v %v", done.URL, done.Step, done.Error)
	}
	if live.State != apitypes.SessionStateRunning || live.Ended != nil || live.Activity == nil || *live.Activity != "thinking" {
		t.Errorf("live session = %+v", live)
	}
}

// TestAPIBackgroundEmpty: nothing dispatched and no plan is an empty
// list, never null.
func TestAPIBackgroundEmpty(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)
	rr := agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/alice/background", "")
	if !strings.Contains(rr.Body.String(), `"sessions":[]`) {
		t.Errorf("body = %s, want an empty sessions list", rr.Body.String())
	}
}

// With a plan, sessions land under the step they name, workers under
// their sub-lead's step, and each step's state follows its sessions.
func TestAPIBackgroundPlanStepsAndStates(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)
	writePlan(t, srv.Store, "alice", strings.Join([]string{
		"# Renewal recommendation: Acme",
		"",
		"## Tasks",
		"- [x] 1. Pull actual spend vs. contracted minimum",
		"- [ ] 2. List every support incident",
		"- [ ] 3. Find two comparable vendors",
		"- [ ] 4. Write the recommendation",
		"- [ ] 5. Check the contract dates",
	}, "\n"))
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	jobs := []*subagentJob{
		// Step 1 by its leading number.
		{ID: "j1", Description: "1. Pull spend", State: subagentJobCompleted, EndedAt: at.Add(time.Minute)},
		// Step 2 by "task 2", live; its worker inherits step 2.
		{ID: "j2", Description: "incidents for task 2", State: subagentJobRunning},
		{ID: "j2w", Description: "read the ticket export", State: subagentJobQueued, CallerID: "j2", Depth: 2},
		// Step 3 by the prompt's "You own task 3", failed: needs help.
		{ID: "j3", Description: "vendors", State: subagentJobFailed, Err: "the pod ran out of memory", EndedAt: at.Add(2 * time.Minute)},
		// Step 5 by its text; cancelled, which does not ask for help.
		{ID: "j5", Description: "check the  contract dates", State: subagentJobCancelled, EndedAt: at.Add(3 * time.Minute)},
		// Names no step.
		{ID: "jx", Description: "tidy the workspace", State: subagentJobCompleted, EndedAt: at.Add(4 * time.Minute)},
	}
	for i, j := range jobs {
		j.Parent = "alice"
		j.QueuedAt = at.Add(time.Duration(i) * time.Second)
		if j.Depth == 0 {
			j.Depth = 1
		}
		seedJob(t, srv, j)
	}
	writeTestSubagentPrompt(t, srv, "j3", "Read /files/background/plan.md for context. You own task 3. After task 1 lands, compare.")

	bg := getBackground(t, srv)
	if bg.Plan == nil {
		t.Fatal("plan absent")
	}
	p := bg.Plan
	if p.Title != "Renewal recommendation: Acme" || p.Done != 1 || p.Total != 5 {
		t.Errorf("plan header = %q %d/%d", p.Title, p.Done, p.Total)
	}
	if p.Running != 2 || p.Finished != 4 || p.NeedsHelp != 1 {
		t.Errorf("counts running=%d finished=%d needs_help=%d, want 2, 4, 1", p.Running, p.Finished, p.NeedsHelp)
	}
	wantSteps := []apitypes.PlanStep{
		{N: 1, Title: "Pull actual spend vs. contracted minimum", State: apitypes.PlanStepStateDone},
		{N: 2, Title: "List every support incident", State: apitypes.PlanStepStateRunning},
		{N: 3, Title: "Find two comparable vendors", State: apitypes.PlanStepStateNeedsHelp},
		{N: 4, Title: "Write the recommendation", State: apitypes.PlanStepStateIdle},
		{N: 5, Title: "Check the contract dates", State: apitypes.PlanStepStateIdle},
	}
	if len(p.Steps) != len(wantSteps) {
		t.Fatalf("steps = %+v", p.Steps)
	}
	for i, want := range wantSteps {
		if p.Steps[i] != want {
			t.Errorf("step %d = %+v, want %+v", i+1, p.Steps[i], want)
		}
	}

	wantStep := map[string]int{"j1": 1, "j2": 2, "j2w": 2, "j3": 3, "j5": 5, "jx": 0}
	wantState := map[string]apitypes.SessionState{
		"j1": apitypes.SessionStateDone, "j2": apitypes.SessionStateRunning, "j2w": apitypes.SessionStateQueued,
		"j3": apitypes.SessionStateErrored, "j5": apitypes.SessionStateCancelled, "jx": apitypes.SessionStateDone,
	}
	for _, sess := range bg.Sessions {
		got := 0
		if sess.Step != nil {
			got = *sess.Step
		}
		if got != wantStep[sess.ID] {
			t.Errorf("session %s step = %d, want %d", sess.ID, got, wantStep[sess.ID])
		}
		if sess.State != wantState[sess.ID] {
			t.Errorf("session %s state = %q, want %q", sess.ID, sess.State, wantState[sess.ID])
		}
		switch sess.ID {
		case "j3":
			if sess.Error == nil || *sess.Error != "the pod ran out of memory" {
				t.Errorf("failed session error = %v", sess.Error)
			}
		case "j2w":
			if sess.CallerID == nil || *sess.CallerID != "j2" {
				t.Errorf("worker caller = %v", sess.CallerID)
			}
		default:
			if sess.Error != nil {
				t.Errorf("session %s carries an error %q", sess.ID, *sess.Error)
			}
		}
	}
}

// TestAPIBackgroundNeedsHelpClearsOnRedispatch: a step whose failed task
// was dispatched again and is running is running, not needs help; once
// the retry lands it is idle until the agent ticks it.
func TestAPIBackgroundNeedsHelpClearsOnRedispatch(t *testing.T) {
	plan := PlanView{Present: true, Title: "p", Items: []PlanItem{{Text: "one"}, {Text: "two"}}, Total: 2}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	failed := subagentJob{ID: "a", Description: "task 1", State: subagentJobFailed, QueuedAt: at}
	retry := subagentJob{ID: "b", Description: "task 1 again", State: subagentJobRunning, QueuedAt: at.Add(time.Second)}
	other := subagentJob{ID: "c", Description: "task 2", State: subagentJobFailed, QueuedAt: at.Add(2 * time.Second)}

	bg := buildBackground("alice", plan, []subagentJob{failed, retry, other}, nil, at.Add(time.Minute))
	if got := bg.Plan.Steps[0].State; got != apitypes.PlanStepStateRunning {
		t.Errorf("step 1 with a live retry = %q, want running", got)
	}
	if got := bg.Plan.Steps[1].State; got != apitypes.PlanStepStateNeedsHelp || bg.Plan.NeedsHelp != 1 {
		t.Errorf("step 2 = %q (needs_help %d), want needs_help, 1", got, bg.Plan.NeedsHelp)
	}

	retry.State, retry.EndedAt = subagentJobCompleted, at.Add(30*time.Second)
	bg = buildBackground("alice", plan, []subagentJob{failed, retry, other}, nil, at.Add(time.Minute))
	if got := bg.Plan.Steps[0].State; got != apitypes.PlanStepStateIdle {
		t.Errorf("step 1 after a successful retry = %q, want idle", got)
	}
}

// TestBuildBackgroundElapsed: a live session's clock runs to now from
// its admission, or from its dispatch while queued; a finished one's
// stops at its end.
func TestBuildBackgroundElapsed(t *testing.T) {
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	now := at.Add(10 * time.Minute)
	jobs := []subagentJob{
		{ID: "q", State: subagentJobQueued, QueuedAt: at},
		{ID: "r", State: subagentJobRunning, QueuedAt: at, StartedAt: at.Add(time.Minute)},
		{ID: "d", State: subagentJobCompleted, QueuedAt: at, StartedAt: at.Add(time.Minute), EndedAt: at.Add(3 * time.Minute)},
		{ID: "f", State: subagentJobFailed, QueuedAt: at},
	}
	bg := buildBackground("alice", PlanView{}, jobs, nil, now)
	want := map[string]int64{"q": 600, "r": 540, "d": 120, "f": 600}
	for _, s := range bg.Sessions {
		if s.ElapsedS != want[s.ID] {
			t.Errorf("%s elapsed = %d, want %d", s.ID, s.ElapsedS, want[s.ID])
		}
	}
	if e := bg.Sessions[3].Error; e == nil || *e != "The task failed without saying why" {
		t.Errorf("failure with no text = %v", e)
	}
}

// TestMatchPlanStepDateIsNotAStep: a description opening with a date
// ("2. October review") is not a claim on step 2; a numbered one is.
func TestMatchPlanStepDateIsNotAStep(t *testing.T) {
	titles := []string{"Collect numbers", "Draft the review", "Send it"}
	cases := map[string]int{
		"2. October review":    0,
		"3. Sept. board pack":  0,
		"2. Draft it":          2,
		"3) Send it":           3,
		"Send it":              3,
		"2. May board, task 1": 1,
	}
	for desc, want := range cases {
		if got := matchPlanStep(subagentJob{ID: "x", Description: desc}, titles, nil); got != want {
			t.Errorf("%q → step %d, want %d", desc, got, want)
		}
	}
}

func TestAPIBackgroundRefusals(t *testing.T) {
	srv := newTestServer(t)
	assertAPIError(t, agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/nobody/background", ""), http.StatusNotFound)
	assertAPIError(t, agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/ceo/background", ""), http.StatusBadRequest)
	assertAPIError(t, agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/_archived/background", ""), http.StatusNotFound)
}

// TestAPIBackgroundArchivedAgentIsEmpty: a departed agent's tabs still
// open; nothing runs for it.
func TestAPIBackgroundArchivedAgentIsEmpty(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)
	if err := srv.Store.ArchiveAgent("alice"); err != nil {
		t.Fatal(err)
	}
	bg := getBackground(t, srv)
	if bg.Plan != nil || len(bg.Sessions) != 0 {
		t.Errorf("archived agent background = %+v", bg)
	}
}

func writeTestSubagentPrompt(t *testing.T, srv *Server, id, prompt string) {
	t.Helper()
	path := subagentMetaPath(srv.Store.Root(), "alice", id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(subagentMeta{ID: id, Prompt: prompt})
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- documents ----

func getDoc(t *testing.T, srv *Server, kind string) apitypes.AgentDoc {
	t.Helper()
	rr := agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/alice/docs/"+kind, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: code = %d (body %s)", kind, rr.Code, rr.Body.String())
	}
	return decodeAPI[apitypes.AgentDoc](t, rr)
}

func TestAPIAgentDocsGet(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)
	if err := srv.Store.WriteRole("alice", "# Analyst\n\nDo the thing.\n"); err != nil {
		t.Fatal(err)
	}
	memory := "# Facts\n- prod runs v0.14 [[ep:2026-09-17T01]]\n- budget is $40k [[stated]]\n\nloose prose line\n"
	if err := srv.Store.WriteAgentMemory("alice", memory); err != nil {
		t.Fatal(err)
	}

	role := getDoc(t, srv, "role")
	if role.Kind != apitypes.AgentDocKindRole || role.Content != "# Analyst\n\nDo the thing.\n" || role.UpdatedAt == nil {
		t.Errorf("role = %+v", role)
	}
	if role.Stats.Lines != 3 || role.Stats.Chars != 25 || role.Stats.Notes != nil {
		t.Errorf("role stats = %+v, want 3 lines, 25 chars, no notes", role.Stats)
	}

	mem := getDoc(t, srv, "memory")
	if mem.Content != memory || mem.UpdatedAt == nil {
		t.Errorf("memory = %+v", mem)
	}
	if mem.Stats.Notes == nil || *mem.Stats.Notes != 2 || mem.Stats.Lines != 5 {
		t.Errorf("memory stats = %+v, want 2 notes over 5 lines", mem.Stats)
	}

	// Habits is the stored principles document, never written: empty,
	// with a null updated_at on the wire.
	rr := agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/alice/docs/habits", "")
	if !strings.Contains(rr.Body.String(), `"updated_at":null`) {
		t.Errorf("empty habits body = %s, want updated_at null", rr.Body.String())
	}
	habits := decodeAPI[apitypes.AgentDoc](t, rr)
	if habits.Kind != apitypes.AgentDocKindHabits || habits.Content != "" || habits.UpdatedAt != nil || habits.Stats.Lines != 0 || habits.Stats.Chars != 0 {
		t.Errorf("empty habits = %+v", habits)
	}
}

func TestMemoryNoteCountFallsBackToListItems(t *testing.T) {
	if got := memoryNoteCount("# Old style\n- one\n- two\n* three\nprose\n"); got != 3 {
		t.Errorf("uncited list items = %d, want 3", got)
	}
	if got := memoryNoteCount(""); got != 0 {
		t.Errorf("empty memory = %d, want 0", got)
	}
}

// TestAPIAgentDocPutRoundTrip: a PUT lands in the stored document the
// form page reads, and the response is the new document.
func TestAPIAgentDocPutRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)

	rr := agentWorkDo(t, srv, http.MethodPut, "/api/v1/agents/alice/docs/habits", `{"content":"- Ask before spending.\n"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: code = %d (body %s)", rr.Code, rr.Body.String())
	}
	put := decodeAPI[apitypes.AgentDoc](t, rr)
	if put.Kind != apitypes.AgentDocKindHabits || put.Content != "- Ask before spending.\n" || put.UpdatedAt == nil || put.Stats.Lines != 1 {
		t.Errorf("PUT response = %+v", put)
	}
	if got, _ := srv.Store.ReadAgentHabits("alice"); got != "- Ask before spending.\n" {
		t.Errorf("stored principles = %q", got)
	}
	if got := getDoc(t, srv, "habits"); got.Content != put.Content {
		t.Errorf("GET after PUT = %q", got.Content)
	}

	// Memory may be cleared.
	if err := srv.Store.WriteAgentMemory("alice", "old"); err != nil {
		t.Fatal(err)
	}
	rr = agentWorkDo(t, srv, http.MethodPut, "/api/v1/agents/alice/docs/memory", `{"content":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("clearing memory: code = %d (body %s)", rr.Code, rr.Body.String())
	}
	if got := decodeAPI[apitypes.AgentDoc](t, rr); got.Content != "" || got.Stats.Notes == nil || *got.Stats.Notes != 0 {
		t.Errorf("cleared memory = %+v", got)
	}

	rr = agentWorkDo(t, srv, http.MethodPut, "/api/v1/agents/alice/docs/role", `{"content":"# New role\n"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("role PUT: code = %d", rr.Code)
	}
	if got, _ := srv.Store.ReadRole("alice"); got != "# New role\n" {
		t.Errorf("stored role = %q", got)
	}
}

// TestAPIAgentDocPutValidation: the role may not be emptied (the form's
// rule, through the same saveAgentDoc), and the body must be JSON with
// no unknown fields.
func TestAPIAgentDocPutValidation(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)
	if err := srv.Store.WriteRole("alice", "# Analyst\n"); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, agentWorkDo(t, srv, http.MethodPut, "/api/v1/agents/alice/docs/role", `{"content":"  \n"}`), http.StatusBadRequest)
	if got, _ := srv.Store.ReadRole("alice"); got != "# Analyst\n" {
		t.Errorf("refused PUT changed the role to %q", got)
	}
	assertAPIError(t, agentWorkDo(t, srv, http.MethodPut, "/api/v1/agents/alice/docs/role", `{"body":"x"}`), http.StatusBadRequest)
	assertAPIError(t, agentWorkDo(t, srv, http.MethodPut, "/api/v1/agents/alice/docs/principles", `{"content":"x"}`), http.StatusNotFound)
}

func TestAPIAgentDocRefusals(t *testing.T) {
	srv := newTestServer(t)
	seedAlice(t, srv)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := ""
		if method == http.MethodPut {
			body = `{"content":"x"}`
		}
		assertAPIError(t, agentWorkDo(t, srv, method, "/api/v1/agents/nobody/docs/role", body), http.StatusNotFound)
		assertAPIError(t, agentWorkDo(t, srv, method, "/api/v1/agents/ceo/docs/memory", body), http.StatusBadRequest)
		assertAPIError(t, agentWorkDo(t, srv, method, "/api/v1/agents/alice/docs/diary", body), http.StatusNotFound)
	}
	if _, err := srv.Store.ReadRole("nobody"); err == nil {
		t.Error("a PUT for an unknown agent wrote a role")
	}
	// ArchiveAgent moves the whole agent directory under
	// agents/_archived/, so a read at the live path would report a
	// real identity as empty: an archived agent's documents are gone.
	if err := srv.Store.ArchiveAgent("alice"); err != nil {
		t.Fatalf("archive agent: %v", err)
	}
	for _, kind := range []string{"role", "memory", "habits"} {
		assertAPIError(t, agentWorkDo(t, srv, http.MethodGet, "/api/v1/agents/alice/docs/"+kind, ""), http.StatusNotFound)
	}
}
