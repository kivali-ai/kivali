// Package web (subagent_nested.go) — dispatch from a SUB-LEAD.
//
// A durable agent's dispatch is asynchronous: it returns a receipt and
// each answer is delivered later as a message that wakes the agent.
// One tier down that contract is unavailable, and not for want of
// plumbing. A subagent is a one-shot `claude -p` whose stdin closes
// after its prompt (see agentpod.handleSubagentTurn), so nothing can
// reach it after it starts. Hand it a receipt and it would finish its
// turn with workers still running and no way to ever hear from them.
//
// So a nested dispatch blocks and returns the answers. The machinery
// is the same machinery — same registry, same admission gate, same
// runJob — with one difference at each end: the caller waits on the
// jobs' done channels, and finishJob skips message delivery for a job
// deeper than tier 1, because its answer is already going back as the
// sub-lead's tool result.
//
// Depth is derived here, from the CALLER's registry entry, and never
// read from the request. A subagent that could state its own depth
// could state 0 and delegate forever.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

// RunNestedBatch runs a sub-lead's dispatch to completion and returns
// its workers' answers as the tool result.
//
// callerID is the dispatching subagent's own job id. It must name a
// live job belonging to parent: an unknown or finished id means the
// caller is not who it says it is, or is dispatching after its own
// cancellation, and neither should start new work.
func (s *SubagentService) RunNestedBatch(ctx context.Context, parent, callerID string, raw json.RawMessage) (string, error) {
	if parent == "" {
		return "", fmt.Errorf("subagent: parent is required")
	}
	if s.Store == nil || s.Driver == nil {
		return "", fmt.Errorf("subagent: service not configured")
	}

	caller, ok := s.lookupJob(callerID)
	if !ok || caller.Parent != parent {
		return "", fmt.Errorf("subagent: unknown caller %q", callerID)
	}
	if caller.terminal() {
		// The caller was cancelled (or somehow finished) while its
		// dispatch was in flight. Starting the workers now would run
		// work nobody is waiting for, in a pod whose slots are wanted
		// by jobs that are.
		return "", fmt.Errorf("subagent: your own task is no longer running — not dispatching")
	}

	depth := caller.Depth + 1
	if depth > agentpod.MaxSubagentDepth {
		// Belt and braces: a subagent at the last tier is not given
		// the tool at all, so reaching this means the toolkit wiring
		// and the depth rule disagree. Refuse rather than let the
		// disagreement decide.
		return "", fmt.Errorf("subagent: you are at the last tier that can delegate (max depth %d) — do this work yourself",
			agentpod.MaxSubagentDepth)
	}

	var req SubagentBatchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if len(req.Tasks) == 0 {
		return "", fmt.Errorf("`tasks` is required and must be non-empty")
	}
	if len(req.Tasks) > agentpod.SubagentMaxNestedFanout {
		return "", fmt.Errorf("at most %d tasks per call at your tier (got %d)", agentpod.SubagentMaxNestedFanout, len(req.Tasks))
	}
	if err := validateTasks(s.Provider, req.Tasks); err != nil {
		return "", err
	}

	taskIDs := make([]string, len(req.Tasks))
	for i := range req.Tasks {
		id, err := newSubagentID()
		if err != nil {
			return "", fmt.Errorf("alloc id: %w", err)
		}
		taskIDs[i] = id
	}

	// No tool_use_id: there is no chip for a nested batch. The parent's
	// chip shows the sub-lead, and what the sub-lead is doing shows up
	// in its activity line. Workers are visible in subagent_status,
	// where they are labelled with the caller that dispatched them.
	jobs := s.launch(parent, "", req.Tasks, taskIDs, depth, callerID)

	// Wait for every worker. ctx is the sub-lead's turn: if the CEO
	// stops the agent, that cancellation has already cascaded to these
	// jobs through the pod's cancel path, and this wait unblocks too
	// rather than holding a control-socket request open forever.
	for _, j := range jobs {
		select {
		case <-j.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	results := make([]subagentJob, 0, len(jobs))
	for _, j := range jobs {
		if snap, ok := s.lookupJob(j.ID); ok {
			results = append(results, snap)
		}
	}
	return renderNestedSubagentResults(results), nil
}

// lookupJob returns a copy of one job. A copy rather than the pointer
// so callers cannot read mutable fields outside the registry lock.
func (s *SubagentService) lookupJob(id string) (subagentJob, bool) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return subagentJob{}, false
	}
	return *j, true
}

// renderNestedSubagentResults is what a sub-lead reads when its
// dispatch returns. Answers in full, in dispatch order, each one
// labelled with the task it answers.
//
// Failures are reported alongside the successes rather than collapsing
// the batch into an error. A sub-lead with two good answers and one
// failure can usually still produce something worth having, and it is
// the only one in a position to judge that — but only if it is told
// plainly which piece is missing rather than left to infer it from a
// gap.
func renderNestedSubagentResults(jobs []subagentJob) string {
	var b strings.Builder
	var failed int
	for _, j := range jobs {
		if j.State != subagentJobCompleted {
			failed++
		}
	}
	fmt.Fprintf(&b, "Your %d worker%s finished (%d succeeded, %d did not).\n",
		len(jobs), pluralS(len(jobs)), len(jobs)-failed, failed)
	b.WriteString("These are raw inputs to YOUR answer, not your answer. Reconcile them before you reply.\n")

	for i, j := range jobs {
		fmt.Fprintf(&b, "\n--- %d. %s (%s) ---\n", i+1, j.Description, j.State)
		switch {
		case j.State == subagentJobCompleted && strings.TrimSpace(j.FinalText) != "":
			b.WriteString(strings.TrimSpace(j.FinalText))
			b.WriteByte('\n')
		case j.State == subagentJobCompleted:
			b.WriteString("(finished without producing any answer text)\n")
		case j.Err != "":
			fmt.Fprintf(&b, "FAILED: %s\n", j.Err)
		default:
			b.WriteString("FAILED (no error reported)\n")
		}
		fmt.Fprintf(&b, "files: /files/subagents/%s/artifacts/private/\n", j.ID)
	}

	if failed > 0 {
		b.WriteString("\nSomething did not finish. Decide whether you can still answer well without it — ")
		b.WriteString("if you can, say what is missing; if you cannot, do that piece yourself. ")
		b.WriteString("Do not silently drop it.\n")
	}
	b.WriteString("\nAnything a worker wrote to /files/background/ is visible to you at the same path.\n")
	return b.String()
}
