package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/structured"
)

// recordingSubmit stands in for core: it keeps what the tool relayed
// and answers with a scripted verdict.
type recordingSubmit struct {
	got   []json.RawMessage
	body  string
	isErr bool
}

func (r *recordingSubmit) SubmitResult(_ context.Context, submission json.RawMessage) (string, bool, error) {
	r.got = append(r.got, submission)
	return r.body, r.isErr, nil
}

// TestSubmitResultOnlyForATaskWithASchema: a free-text task's toolkit
// has no submit_result at all, so no flag mistake can hand one out; a
// task with a schema gets the tool with that schema as the result's
// type, and the tool relays the submission untouched and core's
// verdict as the tool result.
func TestSubmitResultOnlyForATaskWithASchema(t *testing.T) {
	filesDisp := NewLocalFilesDispatcher(&files.Backend{Root: t.TempDir()})
	free := SubagentToolkit(SubagentDeps{SubagentID: "s1", Files: filesDisp})
	for _, tool := range free.Tools {
		if tool.Name == SubmitResultToolName {
			t.Fatal("free-text toolkit registers submit_result")
		}
	}

	schema, err := structured.Parse([]byte(`{"type":"object","properties":{"count":{"type":"integer","description":"how many"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingSubmit{body: "Result accepted."}
	tk := SubagentToolkit(SubagentDeps{SubagentID: "s1", Files: filesDisp, ResultSchema: schema, SubmitResult: rec})
	var tool *Tool
	for i := range tk.Tools {
		if tk.Tools[i].Name == SubmitResultToolName {
			tool = &tk.Tools[i]
		}
	}
	if tool == nil {
		t.Fatal("structured toolkit lacks submit_result")
	}
	// The caller's schema, description included, types the result.
	if !strings.Contains(string(tool.InputSchema), `"result":{"type":"object","properties":{"count":{"type":"integer","description":"how many"}}}`) {
		t.Errorf("input schema does not embed the caller's schema:\n%s", tool.InputSchema)
	}
	if !strings.Contains(tool.Description, "only way your answer reaches them") {
		t.Errorf("description does not say the answer goes through this tool:\n%s", tool.Description)
	}

	submission := json.RawMessage(`{"outcome":"found","result":{"count":3}}`)
	res, err := tool.Handler(context.Background(), submission)
	if err != nil || res.IsError || res.Content[0] != "Result accepted." {
		t.Fatalf("accepted call = %+v, %v", res, err)
	}
	if len(rec.got) != 1 || string(rec.got[0]) != string(submission) {
		t.Errorf("relayed %s, want the submission verbatim", rec.got)
	}

	// Core's refusal is a tool error with core's text: the model reads
	// what to fix.
	rec.body, rec.isErr = "Result not accepted (attempt 1 of 3)", true
	res, err = tool.Handler(context.Background(), submission)
	if err != nil || !res.IsError || !strings.Contains(res.Content[0], "attempt 1 of 3") {
		t.Errorf("refused call = %+v, %v", res, err)
	}

	// Malformed JSON never reaches core.
	res, err = tool.Handler(context.Background(), json.RawMessage(`{"outcome":`))
	if err != nil || !res.IsError || len(rec.got) != 2 {
		t.Errorf("malformed call = %+v, %v; relayed %d", res, err, len(rec.got))
	}
}
