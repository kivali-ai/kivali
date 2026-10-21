package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/structured"
)

// The structured path: a task with a schema answers through
// submit_result, core checks every call against the schema, and the
// caller reads JSON in place of the run's text.

const quoteSchema = `{"type":"object","properties":{"vendor":{"type":"string","maxLength":40},"price":{"type":"number"}},"required":["vendor","price"]}`

func structuredArgs(t *testing.T, schema string) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(map[string]any{"tasks": []map[string]any{{
		"description": "price the renewal", "prompt": "find the price", "schema": json.RawMessage(schema),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return args
}

// TestStructuredTaskDeliversTheAcceptedResultAsJSON follows one task
// from dispatch to delivery: the spec carries the schema and the tool,
// the run's submit_result is accepted once and final, and what the
// caller reads is the wrapper JSON, not a word of the run's own text.
func TestStructuredTaskDeliversTheAcceptedResultAsJSON(t *testing.T) {
	var svc *SubagentService
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		if string(req.Spec.Schema) != quoteSchema {
			t.Errorf("spec.Schema = %s, want the task's", req.Spec.Schema)
		}
		if !slices.Contains(req.Spec.Tools.Kivali, agentpod.SubmitResultToolName) {
			t.Errorf("spec.Tools = %v, want submit_result", req.Spec.Tools.Kivali)
		}
		if !strings.Contains(req.Spec.SystemPrompt, "`submit_result`") {
			t.Error("system prompt does not route the answer through submit_result")
		}
		text, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme","price":1200.50}}`))
		if isErr || !strings.Contains(text, `accepted with outcome "found"`) || !strings.Contains(text, "End your turn") {
			t.Errorf("accepted call = %q (error %v)", text, isErr)
		}
		// The first accepted result is final.
		text, isErr = svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"not_found"}`))
		if !isErr || !strings.Contains(text, "already accepted") {
			t.Errorf("second call = %q (error %v), want a refusal", text, isErr)
		}
		return "Submitted the quote.", nil
	}}
	var s *store.FSStore
	var rec *hubRecorder
	var del *deliveryRecorder
	svc, s, _, rec, del = newSubagentTestServiceWithDeliveries(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, quoteSchema)); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	msgs := del.waitFor(1)
	body := msgs[0].Content
	if msgs[0].Kind != store.KindSubagentResult || !strings.HasPrefix(body, "Background task ") || !strings.Contains(body, "(price the renewal) finished.") {
		t.Errorf("delivered header: %q", body)
	}
	// The wrapper's keys in its order, the result's sorted, numbers in
	// canonical form.
	if !strings.Contains(body, "```json\n{\n  \"outcome\": \"found\",\n  \"result\": {\n    \"price\": 1200.5,\n    \"vendor\": \"Acme\"\n  }\n}\n```") {
		t.Errorf("delivered body lacks the result JSON:\n%s", body)
	}
	if strings.Contains(body, "Submitted the quote.") {
		t.Errorf("the run's own text reached the caller:\n%s", body)
	}
	if !strings.Contains(body, "Nothing else of yours is running") {
		t.Errorf("delivered body lacks the job standing:\n%s", body)
	}

	jobs := svc.listJobs("alice")
	if len(jobs) != 1 || jobs[0].State != subagentJobCompleted || jobs[0].FinalText != "" || !strings.Contains(jobs[0].Result, `"vendor": "Acme"`) {
		t.Errorf("job = %+v", jobs)
	}
	// meta.json carries the result for the task rows, and marks the task.
	meta, err := readSubagentMeta(subagentMetaPath(s.Root(), "alice", jobs[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Structured || meta.Status != "completed" || !strings.Contains(string(meta.Result), `"vendor": "Acme"`) {
		t.Errorf("meta = %+v", meta)
	}
	// The chip's completed event shows the JSON too.
	var completed string
	for _, e := range rec.events {
		if e.Kind != "subagent_completed" {
			continue
		}
		tasks, _ := e.Data["tasks"].([]any)
		if len(tasks) == 1 {
			task, _ := tasks[0].(map[string]any)
			completed, _ = task["final_text"].(string)
		}
	}
	if !strings.HasPrefix(completed, "```json\n{\n  \"outcome\": \"found\"") {
		t.Errorf("subagent_completed final_text = %q", completed)
	}
}

// TestStructuredTaskEndsAfterThreeRefusedResults: each refusal says
// what to fix and which attempt it was; the third ends the task, and
// the caller reads outcome failed with core's reason and nothing from
// the refused submissions.
func TestStructuredTaskEndsAfterThreeRefusedResults(t *testing.T) {
	var svc *SubagentService
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		for i := 1; i <= subagentResultAttempts; i++ {
			text, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"found","result":{"vendor":"Acme Secret Ltd"}}`))
			if !isErr || !strings.Contains(text, fmt.Sprintf("attempt %d of 3", i)) || !strings.Contains(text, `missing required field "price"`) {
				t.Errorf("attempt %d = %q (error %v)", i, text, isErr)
			}
			if i < subagentResultAttempts && !strings.Contains(text, "call submit_result again") {
				t.Errorf("attempt %d does not invite a retry: %q", i, text)
			}
			if i == subagentResultAttempts && !strings.Contains(text, "has been ended") {
				t.Errorf("last attempt does not say the task ended: %q", text)
			}
		}
		j, _ := svc.lookupJob(req.SubagentID)
		if j.endReason == "" {
			t.Error("core did not record why it ended the task")
		}
		// A fourth call finds the task ended.
		if text, isErr := svc.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"not_found"}`)); !isErr {
			t.Errorf("a call after the cap was accepted: %q", text)
		}
		// Core cancelled the run; the pod reports it as such.
		return "", context.Canceled
	}}
	var del *deliveryRecorder
	svc, _, _, _, del = newSubagentTestServiceWithDeliveries(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, quoteSchema)); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	body := del.waitFor(1)[0].Content
	if !strings.Contains(body, "FAILED") || !strings.Contains(body, `"outcome": "failed"`) || !strings.Contains(body, `"error": "no valid result after 3 submit_result calls"`) {
		t.Errorf("delivered body:\n%s", body)
	}
	if strings.Contains(body, "Acme") || strings.Contains(body, "Secret") {
		t.Errorf("a refused submission's content reached the caller:\n%s", body)
	}
	jobs := svc.listJobs("alice")
	if len(jobs) != 1 || jobs[0].State != subagentJobFailed || jobs[0].ResultAttempts != 3 {
		t.Errorf("job = %+v", jobs)
	}
}

// TestStructuredTaskThatNeverSubmitsFails: a run that ends with prose
// instead of a submit_result call delivers outcome failed, and the
// prose stays out of the caller's context.
func TestStructuredTaskThatNeverSubmitsFails(t *testing.T) {
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		return "The price is 1200 from Acme.", nil
	}}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, quoteSchema)); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	body := del.waitFor(1)[0].Content
	if !strings.Contains(body, "FAILED") || !strings.Contains(body, `"error": "the task finished without calling submit_result"`) {
		t.Errorf("delivered body:\n%s", body)
	}
	if strings.Contains(body, "Acme") {
		t.Errorf("the run's prose reached the caller:\n%s", body)
	}
	if jobs := svc.listJobs("alice"); len(jobs) != 1 || jobs[0].State != subagentJobFailed {
		t.Errorf("job = %+v", jobs)
	}
}

// TestStructuredTaskFailureBeforeSubmitting: a run that dies before it
// submits is a failure whose body names core's reason; a run that dies
// after an accepted result still delivers that result.
func TestStructuredTaskFailureBeforeSubmitting(t *testing.T) {
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		return "", fmt.Errorf("the model call failed: usage limit")
	}}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	if _, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, quoteSchema)); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	body := del.waitFor(1)[0].Content
	// The body is core's fixed text; the driver's detail stays on the
	// job record.
	if !strings.Contains(body, `"outcome": "failed"`) || !strings.Contains(body, `"error": "the task ended before it submitted a result"`) || strings.Contains(body, "usage limit") {
		t.Errorf("delivered body:\n%s", body)
	}
	if jobs := svc.listJobs("alice"); len(jobs) != 1 || !strings.Contains(jobs[0].Err, "usage limit") {
		t.Errorf("job record lacks the driver's error: %+v", jobs)
	}

	// The body names the task's id, a random 8-hex-digit string, so the
	// check for the run's error is on its whole text: the exit code's
	// digits alone can be part of an id. The id here holds them.
	const runErr, id = "process exited 137", "a1370bcd"
	var svc2 *SubagentService
	driver2 := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		if _, isErr := svc2.SubmitResult("alice", req.SubagentID, json.RawMessage(`{"outcome":"not_found"}`)); isErr {
			t.Error("valid not_found refused")
		}
		return "", errors.New(runErr)
	}}
	var del2 *deliveryRecorder
	svc2, _, _, _, del2 = newSubagentTestServiceWithDeliveries(t, driver2)
	task := SubagentTaskInput{Description: "price the renewal", Prompt: "find the price", Schema: json.RawMessage(quoteSchema)}
	svc2.launch("alice", "", []SubagentTaskInput{task}, []string{id}, 1, "")
	body = del2.waitFor(1)[0].Content
	if !strings.Contains(body, id+" (price the renewal) finished.") || !strings.Contains(body, `"outcome": "not_found"`) || strings.Contains(body, runErr) {
		t.Errorf("accepted-then-died body:\n%s", body)
	}
}

// TestStructuredSchemaIsRefusedAtDispatch: a schema outside the subset
// costs the agent a tool error naming the path and the fix, and no
// subagent spawns.
func TestStructuredSchemaIsRefusedAtDispatch(t *testing.T) {
	driver := &fakeSubagentDriver{}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	_, err := svc.StartBatch(context.Background(), "alice", structuredArgs(t, `{"type":"object","properties":{"name":{"type":"string"}}}`))
	if err == nil || !strings.Contains(err.Error(), `tasks[0].schema at name: a string needs "maxLength"`) {
		t.Fatalf("StartBatch err = %v", err)
	}
	if len(svc.listJobs("alice")) != 0 {
		t.Error("a job was registered for a refused schema")
	}
	// A null schema is no schema.
	args, _ := json.Marshal(map[string]any{"tasks": []map[string]any{{"description": "d", "prompt": "p", "schema": nil}}})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Errorf("null schema refused: %v", err)
	}
	del.waitFor(1)
	driver.mu.Lock()
	req := driver.requests
	driver.mu.Unlock()
	if len(req) == 0 || req[0].Spec.Structured() || slices.Contains(req[0].Spec.Tools.Kivali, agentpod.SubmitResultToolName) {
		t.Errorf("null schema dispatched as structured: %+v", req)
	}
}

// TestSubmitResultRefusesTasksThatAreNotTheCallers: an unknown id,
// another agent's task and a free-text task are all refused, so a
// subagent can answer only the structured task it is running.
func TestSubmitResultRefusesTasksThatAreNotTheCallers(t *testing.T) {
	release := make(chan struct{})
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		<-release
		return "ok", nil
	}}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	args, _ := json.Marshal(map[string]any{"tasks": []map[string]any{{"description": "free text", "prompt": "p"}}})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Fatal(err)
	}
	id := svc.listJobs("alice")[0].ID
	cases := map[string]struct {
		parent, id, want string
	}{
		"unknown id":    {"alice", "nope", "no running task"},
		"another agent": {"bob", id, "no running task"},
		"free text":     {"alice", id, "has no schema"},
	}
	for name, c := range cases {
		text, isErr := svc.SubmitResult(c.parent, c.id, json.RawMessage(`{"outcome":"not_found"}`))
		if !isErr || !strings.Contains(text, c.want) {
			t.Errorf("%s: %q (error %v), want %q", name, text, isErr, c.want)
		}
	}
	close(release)
	del.waitFor(1)
	if text, isErr := svc.SubmitResult("alice", id, json.RawMessage(`{"outcome":"not_found"}`)); !isErr || !strings.Contains(text, "has no schema") {
		t.Errorf("finished free-text task: %q", text)
	}
}

// TestNestedResultsRenderStructuredWorkersAsJSON: a sub-lead reads a
// structured worker's JSON, however the worker ended, and none of its
// text.
func TestNestedResultsRenderStructuredWorkersAsJSON(t *testing.T) {
	out := renderNestedSubagentResults([]subagentJob{
		{ID: "a1", Description: "price it", State: subagentJobCompleted, FinalText: "Done.", Result: "{\n  \"outcome\": \"found\"\n}"},
		{ID: "b2", Description: "count them", State: subagentJobFailed, Err: "no valid result", Result: "{\n  \"outcome\": \"failed\",\n  \"error\": \"no valid result\"\n}"},
	})
	if !strings.Contains(out, "```json\n{\n  \"outcome\": \"found\"\n}\n```") {
		t.Errorf("structured worker's JSON missing:\n%s", out)
	}
	if strings.Contains(out, "Done.") {
		t.Errorf("structured worker's text leaked:\n%s", out)
	}
	if !strings.Contains(out, `"outcome": "failed"`) || strings.Count(out, "no valid result") != 1 {
		t.Errorf("failed structured worker not rendered as its JSON once:\n%s", out)
	}
}

// TestSubagentTaskViewShowsTheStructuredResult: the task rows show a
// structured task's JSON, fenced, in place of the run's final text.
func TestSubagentTaskViewShowsTheStructuredResult(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents", "alice", "subagents", "ab12cd34")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"ab12cd34","status":"completed","structured":true,"result":{"outcome":"found","result":{"price":12}}}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chat.jsonl"), []byte(`{"role":"sent","kind":"direct_chat","content":"Submitted.","ts":"2026-10-16T09:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := deriveSubagentTaskView(root, "alice", "ab12cd34", "price it")
	if v.Result == "" || v.FinalText != "Submitted." {
		t.Fatalf("view = %+v", v)
	}
	task := subagentTask(provider.MockProvider{}, "alice", 0, v)
	if task.OutputMD == nil || !strings.HasPrefix(*task.OutputMD, "```json\n") || !strings.Contains(*task.OutputMD, `"price":12`) || strings.Contains(*task.OutputMD, "Submitted.") {
		t.Errorf("OutputMD = %v", task.OutputMD)
	}
}

// TestSubagentResultRouteAnswersTheTool: the UDS route takes the
// submission as its body under the slug binding and answers with the
// tool result, a refusal included, as JSON; a request without the
// header is refused.
func TestSubagentResultRouteAnswersTheTool(t *testing.T) {
	srv := newTestServer(t)
	srv.SubagentService = &SubagentService{Store: srv.Store, Provider: provider.MockProvider{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/subagents/{id}/result", srv.handleAgentpodSubagentResult)

	req := httptest.NewRequest(http.MethodPost, "/v1/agent/alice/subagents/nope/result", strings.NewReader(`{"outcome":"not_found"}`))
	req.Header.Set(agentpod.SlugHeader, "alice")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body)
	}
	var resp agentpod.SubagentResultResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Body, "no running task nope") {
		t.Errorf("response = %+v", resp)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/agent/alice/subagents/nope/result", strings.NewReader(`{}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("without the slug header: status = %d, want 403", rr.Code)
	}
}

// TestSubagentResultRouteRefusesAnOversizeBodyAsTheValidatorDoes: a
// submission past what core reads of a body is still answered as a
// refusal the model can act on, with the validator's size message and
// the attempt counted, never a bare 413.
func TestSubagentResultRouteRefusesAnOversizeBodyAsTheValidatorDoes(t *testing.T) {
	srv := newTestServer(t)
	svc := &SubagentService{Store: srv.Store, Provider: provider.MockProvider{}}
	srv.SubagentService = svc
	schema, err := structured.Parse(json.RawMessage(quoteSchema))
	if err != nil {
		t.Fatal(err)
	}
	svc.registerJob(&subagentJob{ID: "job-big", Parent: "alice", State: subagentJobRunning, Schema: schema, done: make(chan struct{})})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/{slug}/subagents/{id}/result", srv.handleAgentpodSubagentResult)

	huge := `{"outcome":"found","result":{"vendor":"` + strings.Repeat("x", subagentResultMaxBody+1024) + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/alice/subagents/job-big/result", strings.NewReader(huge))
	req.Header.Set(agentpod.SlugHeader, "alice")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a refusal; body %s", rr.Code, rr.Body)
	}
	var resp agentpod.SubagentResultResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Body, "attempt 1 of 3") || !strings.Contains(resp.Body, "Put bulk in a file") {
		t.Errorf("response = %+v, want the validator's size refusal as attempt 1", resp)
	}
	if j, ok := svc.lookupJob("job-big"); !ok || j.ResultAttempts != 1 {
		t.Errorf("ResultAttempts = %d, want the oversize call counted", j.ResultAttempts)
	}
}
