// Package web (subagent_tools.go) — the model-facing view of
// background subagent jobs.
//
// Two tools, deliberately. Dispatch is fire-and-forget and results
// arrive as messages, so what is left is "what is still running" and
// "stop that one". Everything else an agent might want is already
// reachable: a job's output is on the filesystem it can file_view, and
// its transcript is a page it can link to.
//
// Nothing here fetches a result. Results are delivered, not polled —
// adding a getter would invite an agent to sit in a loop asking, which
// is the blocking behaviour this whole change removes.

package web

import (
	"fmt"
	"strings"
	"time"
)

// SubagentStatusToolName / SubagentCancelToolName are the MCP-side
// names, which are also the bare names core sees on events.
const (
	SubagentStatusToolName = "subagent_status"
	SubagentCancelToolName = "subagent_cancel"
)

// SubagentStatus renders parent's background jobs for the model.
//
// Terminal jobs are included rather than filtered out: an agent woken
// by one result wants to see what else landed while it was away, and
// distinguishing "the other two failed" from "the other two are still
// going" changes what it does next.
func (s *SubagentService) SubagentStatus(parent string) string {
	if s == nil {
		return "Subagents are not available in this deployment."
	}
	jobs := s.listJobs(parent)
	if len(jobs) == 0 {
		return "You have no background tasks — none running, none finished this session."
	}

	now := time.Now().UTC()
	var queued, running, done []subagentJob
	for _, j := range jobs {
		switch {
		case j.terminal():
			done = append(done, j)
		case j.State == subagentJobQueued:
			queued = append(queued, j)
		default:
			running = append(running, j)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d background task%s running, %d queued, %d finished.\n", len(running), pluralS(len(running)), len(queued), len(done))

	// Queued is reported separately and first, because the reflex it
	// has to suppress is the wrong one: a task that has not started is
	// not a task that is stuck, and an agent told only "no activity
	// for 6 minutes" would reasonably cancel and re-dispatch it — which
	// puts the replacement at the back of the same queue.
	if len(queued) > 0 {
		b.WriteString("\nQUEUED (waiting for a free slot in your pod — not stuck, not started):\n")
		for _, j := range queued {
			fmt.Fprintf(&b, "  %s — %s\n", j.ID, j.Description)
			fmt.Fprintf(&b, "      waiting %s%s\n", roundDuration(now.Sub(j.QueuedAt)), depthSuffix(j))
		}
	}

	if len(running) > 0 {
		b.WriteString("\nRUNNING:\n")
		for _, j := range running {
			fmt.Fprintf(&b, "  %s — %s%s\n", j.ID, j.Description, depthSuffix(j))
			activity := j.Activity
			if activity == "" {
				activity = "starting up"
			}
			fmt.Fprintf(&b, "      %s · running for %s", activity, roundDuration(now.Sub(j.StartedAt)))
			// Silence is reported as an observation, not a verdict.
			// How long is too long depends on what the task is doing —
			// a build that prints nothing for ten minutes is fine, a
			// file_view that does is not — so the judgement is left to
			// whoever is reading.
			if idle := j.idleFor(now); idle > 0 {
				fmt.Fprintf(&b, " · last activity %s ago", roundDuration(idle))
			}
			b.WriteByte('\n')
		}
	}

	if len(done) > 0 {
		b.WriteString("\nFINISHED:\n")
		for _, j := range done {
			fmt.Fprintf(&b, "  %s — %s%s · %s", j.ID, j.Description, depthSuffix(j), j.State)
			if j.Err != "" {
				fmt.Fprintf(&b, " (%s)", j.Err)
			}
			if !j.EndedAt.IsZero() {
				fmt.Fprintf(&b, " · %s ago", roundDuration(now.Sub(j.EndedAt)))
			}
			b.WriteByte('\n')
		}
		b.WriteString("\nFinished tasks have already sent you their results as messages; ")
		b.WriteString("their files are at /files/subagents/<id>/artifacts/private/.\n")
	}
	return b.String()
}

// SubagentCancel stops one of parent's running jobs.
//
// A cancelled job sends no result message — you asked for it to stop,
// so there is nothing to report back, and a delivered "it was
// cancelled" would wake the agent for no reason.
func (s *SubagentService) SubagentCancel(parent, id string) string {
	if s == nil {
		return "Subagents are not available in this deployment."
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "subagent_cancel: `id` is required — get it from subagent_status."
	}
	if s.CancelSubagent(parent, id) {
		return fmt.Sprintf("Cancelled background task %s, along with any sub-tasks it had "+
			"dispatched — calling off a task calls off the work underneath it. None of them "+
			"will send you a result. Anything already written under "+
			"/files/subagents/%s/artifacts/private/ is still there.", id, id)
	}
	// One message for both misses. Distinguishing "no such id" from
	// "already finished" would leak whether an id exists under another
	// agent, and the useful next step is the same either way.
	return fmt.Sprintf("No running background task %s of yours to cancel — it may have already finished, "+
		"or the id may be wrong. Call subagent_status to see what you have.", id)
}

// depthSuffix labels a job that one of the agent's own subagents
// dispatched, so the agent can tell its own tasks from its sub-leads'
// workers. Empty for tier-1 jobs, which are the common case and need
// no annotation.
func depthSuffix(j subagentJob) string {
	if j.Depth <= 1 {
		return ""
	}
	if j.CallerID != "" {
		return fmt.Sprintf(" [dispatched by %s]", j.CallerID)
	}
	return " [nested]"
}

// roundDuration renders a duration at a granularity a human (or a
// model) will actually read: seconds under a minute, minutes under an
// hour, hours beyond. Sub-second precision on a task that runs for
// minutes is noise.
func roundDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
