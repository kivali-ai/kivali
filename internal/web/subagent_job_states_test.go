package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// subagentJobStates is every job state subagent_jobs.go defines (the
// string values of its subagentJob* constants), read from the source so
// a state added there is picked up here without anyone remembering to
// list it. A test that maps or classifies job states walks this list,
// which is what keeps a new state from falling through a switch's
// default unnoticed.
func subagentJobStates(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "subagent_jobs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse subagent_jobs.go: %v", err)
	}
	var states []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "subagentJob") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", name.Name, err)
				}
				states = append(states, v)
			}
		}
	}
	sort.Strings(states)
	if len(states) < 7 {
		t.Fatalf("read %d job states from subagent_jobs.go (%v); the reader is out of step with the file", len(states), states)
	}
	return states
}

// checkEveryJobStateHasAnExpectation fails for a job state the table
// does not name, and for a table entry that is no job state. Without it
// a state added to subagent_jobs.go would take a mapping's default
// branch and nobody would decide what it should show.
func checkEveryJobStateHasAnExpectation[V any](t *testing.T, what string, want map[string]V) {
	t.Helper()
	known := map[string]bool{}
	for _, st := range subagentJobStates(t) {
		known[st] = true
		if _, ok := want[st]; !ok {
			t.Errorf("job state %q has no expected %s: decide what it maps to, add it to the table and to the code under test", st, what)
		}
	}
	for st := range want {
		if !known[st] {
			t.Errorf("expected %s names %q, which is no job state", what, st)
		}
	}
}

// TestSubagentStateCoversEveryJobState: the task page and the
// transcript's task pills read a task's recorded status through
// subagentState. If it is deleted, a state added to subagent_jobs.go
// reads as running there by default (a task that ended would show as
// still going; one that waits for a person would show no ask at all).
func TestSubagentStateCoversEveryJobState(t *testing.T) {
	want := map[string]apitypes.SubagentTaskState{
		subagentJobQueued:           apitypes.SubagentTaskStateRunning,
		subagentJobRunning:          apitypes.SubagentTaskStateRunning,
		subagentJobWaiting:          apitypes.SubagentTaskStateWaitingForSignIn,
		subagentJobAwaitingApproval: apitypes.SubagentTaskStateAwaitingApproval,
		subagentJobCompleted:        apitypes.SubagentTaskStateDone,
		subagentJobFailed:           apitypes.SubagentTaskStateErrored,
		subagentJobCancelled:        apitypes.SubagentTaskStateErrored,
	}
	checkEveryJobStateHasAnExpectation(t, "task state", want)
	for status, state := range want {
		if got, _ := subagentState(status, ""); got != state {
			t.Errorf("subagentState(%q) = %q, want %q", status, got, state)
		}
	}
}
