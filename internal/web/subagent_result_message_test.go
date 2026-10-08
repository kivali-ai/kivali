package web

import (
	"strings"
	"testing"
)

// The delivered result is the only moment the agent is reliably awake
// and thinking about a delegated job: results arrive minutes apart with
// unrelated conversation in between. What this message says at the
// bottom is therefore the whole mechanism for keeping a multi-wave job
// coherent across interleaved turns. These pin it.

// TestResultMessageCarriesJobStanding covers the common case — a result
// lands with siblings still running and a plan on disk.
func TestResultMessageCarriesJobStanding(t *testing.T) {
	plan := PlanView{Present: true, Done: 1, Total: 4}
	out := renderSubagentResultMessage("a1b2", "check the invoices", subagentJobCompleted,
		"Seven overdue.", nil, 2, plan)

	if !strings.Contains(out, "Seven overdue.") {
		t.Error("the answer itself is missing")
	}
	if !strings.Contains(out, "2 of your other background tasks are still running") {
		t.Error("outstanding count missing; the agent cannot tell whether the job is finished")
	}
	if !strings.Contains(out, "1/4 done") {
		t.Error("plan progress missing")
	}
	if !strings.Contains(out, "/files/background/plan.md") {
		t.Error("no path to the plan it is being told to update")
	}
	// The two moves an agent reliably forgets, and the two that get
	// much more expensive one wave later.
	if !strings.Contains(out, "cancel work this result just made pointless") {
		t.Error("does not prompt cancelling invalidated work")
	}
	if !strings.Contains(out, "ask the owner") {
		t.Error("does not prompt asking the owner — a wave boundary is the only moment it can")
	}
}

// TestResultMessageSaysWhenTheJobIsDone is the close-out case. An agent
// left to infer "that was the last one" from a count of zero will
// sometimes report the final fragment instead of the deliverable.
func TestResultMessageSaysWhenTheJobIsDone(t *testing.T) {
	plan := PlanView{Present: true, Done: 4, Total: 4}
	out := renderSubagentResultMessage("z9", "last task", subagentJobCompleted, "done", nil, 0, plan)

	if !strings.Contains(out, "this was the last one") {
		t.Error("does not say nothing else is running")
	}
	if !strings.Contains(out, "assemble and deliver") {
		t.Error("does not tell a finished job to assemble")
	}
}

// TestResultMessageWithoutAPlanStillCloses keeps the footer useful for
// an agent that dispatched without writing a plan — the right call for
// a one-off lookup, where a plan would be pure overhead.
func TestResultMessageWithoutAPlanStillCloses(t *testing.T) {
	out := renderSubagentResultMessage("q1", "quick lookup", subagentJobCompleted, "42", nil, 0, PlanView{})

	if strings.Contains(out, "plan.md") {
		t.Error("points at a plan that does not exist")
	}
	if !strings.Contains(out, "this was the last one") {
		t.Error("does not close out the job")
	}
	if !strings.Contains(out, "assemble the result now") {
		t.Error("no nudge to assemble rather than report fragments one at a time")
	}
}

// TestResultMessageDoesNotAssemblePrematurely is the negative half of
// the one above. Telling an agent to assemble while three tasks are
// still running would produce a deliverable built on part of the
// evidence — the exact failure the footer exists to prevent.
func TestResultMessageDoesNotAssemblePrematurely(t *testing.T) {
	out := renderSubagentResultMessage("q2", "another", subagentJobCompleted, "x", nil, 3, PlanView{})
	if strings.Contains(out, "assemble") {
		t.Error("told the agent to assemble while three tasks are still running")
	}

	withPlan := renderSubagentResultMessage("q3", "another", subagentJobCompleted, "x", nil, 3,
		PlanView{Present: true, Done: 1, Total: 4})
	if strings.Contains(withPlan, "assemble and deliver") {
		t.Error("issued the close-out instruction with work outstanding")
	}
}
