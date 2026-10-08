// Package web (subagent_jobs.go) — the registry of background
// subagent jobs.
//
// A subagent dispatch returns immediately with job ids and the work
// continues underneath; results come back to the parent as messages.
// The parent's turn (and its CLI process) is not pinned for the batch,
// and a long job is distinguishable from a hung one.
//
// This file owns what is outstanding. Execution lives in
// subagent_service.go, and the model-facing tools in subagent_tools.go.
//
// DURABILITY: the registry is in memory. Each job also writes
// meta.json to the PVC as it goes, so a finished job's record survives,
// but jobs in flight when core restarts are orphaned — their driving
// goroutine is gone and the pod's completion POST lands on a turn that
// no longer exists. The visible symptom is a meta.json stuck at
// "running".
// Recovery is deliberately not built here: it needs a decision about
// whether to re-attach or fail such jobs, and that decision wants real
// restart data behind it.

package web

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Subagent job states. A job is terminal in every state except
// subagentJobQueued and subagentJobRunning.
//
// Queued is what admission control made visible: a dispatched job that
// has not been given a slot in the pod yet. It is emphatically NOT
// terminal — it is owed a result, it counts as outstanding work, and
// it must stay cancellable, because "call off the batch I just sent"
// is most useful precisely while the batch is still waiting to start.
const (
	subagentJobQueued    = "queued"
	subagentJobRunning   = "running"
	subagentJobCompleted = "completed"
	subagentJobFailed    = "failed"
	subagentJobCancelled = "cancelled"
)

// subagentJob is one dispatched background task.
//
// Mutable fields are guarded by the owning registry's jobsMu, not by
// the job itself: callers hold the registry lock across read-modify
// sequences (list-then-render, check-then-cancel), so a per-job lock
// would add a second thing to get wrong without removing the first.
type subagentJob struct {
	ID          string
	Parent      string
	Description string
	Model       string
	Effort      string

	State     string
	StartedAt time.Time
	EndedAt   time.Time

	// Depth is how far below the durable agent this job sits. 1 is a
	// subagent the agent dispatched itself; 2 is a worker dispatched
	// by one of those sub-leads. Recorded at registration and never
	// read from a request body — a nested dispatch's depth is derived
	// from the CALLER's recorded job, so a subagent cannot talk its
	// way deeper than the cap by claiming a smaller number.
	Depth int

	// CallerID is the job id of the subagent that dispatched this one,
	// empty for a tier-1 job dispatched by the durable agent. It is
	// what lets status output show a sub-lead's workers underneath it
	// instead of as a flat list of unexplained tasks.
	CallerID string

	// LastActivityAt is when this job's transcript last grew, fed by
	// the activity watcher. It is the raw material for "is this
	// stuck": a job whose transcript has been silent far longer than
	// any tool plausibly runs is the signal.
	//
	// Nothing acts on it yet, by design — thresholds picked before
	// seeing real jobs are guesses, and a too-eager one cries wolf on
	// a legitimately slow build. It is surfaced through
	// subagent_status so the shape of real silence can be observed
	// first.
	LastActivityAt time.Time

	// Activity is the human-readable current step ("running
	// run_shell", "thinking"), derived by the same watcher.
	Activity string

	// QueuedAt is when the job was registered, as distinct from
	// StartedAt, which is when it was admitted and actually began.
	// The gap between them is queue time, and it is worth showing:
	// "silent for 8 minutes" means something very different for a job
	// that has not started than for one that has.
	QueuedAt time.Time

	FinalText string
	Err       string

	// cancel stops this job's execution. Held so subagent_cancel can
	// reach a job that no longer has anything blocking on it.
	cancel context.CancelFunc

	// done is closed by finishJob when the job reaches a terminal
	// state. The async tier-1 path ignores it; a nested batch waits on
	// it, because a sub-lead's dispatch is synchronous — it is a
	// one-shot process with nowhere to receive a later delivery.
	//
	// Closed exactly once, by finishJob, which is already the single
	// exit every job path goes through.
	done chan struct{}
}

// terminal reports whether the job has stopped, for any reason.
func (j *subagentJob) terminal() bool {
	return j.State != subagentJobRunning && j.State != subagentJobQueued
}

// idleFor reports how long the job's transcript has been silent, as of
// now. Zero for a terminal job and for one that has not yet produced
// anything (there is no silence to measure before the first entry).
func (j *subagentJob) idleFor(now time.Time) time.Duration {
	if j.terminal() || j.LastActivityAt.IsZero() {
		return 0
	}
	return now.Sub(j.LastActivityAt)
}

// registerJob adds a job to the registry.
func (s *SubagentService) registerJob(j *subagentJob) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.jobs == nil {
		s.jobs = map[string]*subagentJob{}
	}
	s.jobs[j.ID] = j
}

// updateJob applies fn to the job under the registry lock. No-op when
// the id is unknown. Returns whether the job was found.
func (s *SubagentService) updateJob(id string, fn func(*subagentJob)) bool {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false
	}
	fn(j)
	return true
}

// listJobs returns copies of parent's jobs, newest dispatch first.
// Terminal jobs are included: an agent waking to a result wants to see
// what else finished while it was away, not just what is still moving.
func (s *SubagentService) listJobs(parent string) []subagentJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	var out []subagentJob
	for _, j := range s.jobs {
		if j.Parent == parent {
			out = append(out, *j)
		}
	}
	// Ordered by dispatch, not by start: a queued job has no start
	// time yet, and sorting on one would file every waiting job at the
	// bottom under the zero time regardless of when it was asked for.
	//
	// OLDEST FIRST. The list is read as a history — first launched at
	// the top, most recent at the bottom — which is the order both the
	// panel and subagent_status want, and the order a batch was
	// actually asked for. (ID breaks ties because a batch of five is
	// stamped from one `now`, so QueuedAt alone is not a total order.)
	sort.Slice(out, func(a, b int) bool {
		if out[a].QueuedAt.Equal(out[b].QueuedAt) {
			return out[a].ID < out[b].ID
		}
		return out[a].QueuedAt.Before(out[b].QueuedAt)
	})
	return out
}

// OutstandingSubagents reports how many of parent's jobs are still
// running. This is what makes an agent waiting on background work
// render as working rather than idle.
//
// DISPLAY ONLY. It must not reach turnInFlight or deliverToAgent: an
// agent waiting on background tasks is emphatically still able to
// receive a message, and gating delivery on this would silently strand
// messages behind a long batch.
func (s *SubagentService) OutstandingSubagents(parent string) int {
	if s == nil {
		return 0
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	n := 0
	for _, j := range s.jobs {
		if j.Parent == parent && !j.terminal() {
			n++
		}
	}
	return n
}

// CancelSubagent stops one of parent's running jobs. Reports whether
// anything was cancelled: an unknown id and an already-finished job
// both return false, and the caller turns that into a message the
// model can act on.
//
// Ownership is checked here rather than by the caller — a parent slug
// must not be able to cancel another agent's work by guessing an id.
func (s *SubagentService) CancelSubagent(parent, id string) bool {
	s.jobsMu.Lock()
	j, ok := s.jobs[id]
	if !ok || j.Parent != parent || j.terminal() {
		s.jobsMu.Unlock()
		return false
	}
	// Cancel the task AND everything it dispatched.
	//
	// Each job gets its own context off Background, deliberately — a
	// job must outlive the turn that asked for it. The cost is that
	// cancelling a sub-lead does NOT reach its workers the way a
	// derived context would, so they kept running and queued after
	// their caller was called off: work nobody was waiting for, still
	// holding admission slots, still delivering into a batch that had
	// been abandoned.
	//
	// Collected transitively rather than one level deep. Depth is
	// capped at 2 today, so this is a single hop in practice, but a
	// cancel that only reached the first generation would be a silent
	// hole the moment the cap moved.
	cancels := s.collectCancelsLocked(parent, id)
	s.jobsMu.Unlock()

	// Outside the lock: cancel unwinds DriveSubagent, which publishes
	// to the agent pod. The job's own goroutine writes the terminal
	// state as it exits, so this does not set it here — doing both
	// would race over which reason lands.
	for _, cancel := range cancels {
		cancel()
	}
	return true
}

// collectCancelsLocked returns the cancel funcs for id and every
// non-terminal descendant of it, walking CallerID links. jobsMu must
// be held.
//
// Ownership is checked on every node, not just the root: a job's
// Parent is the durable agent that owns the whole tree, so this
// refuses to follow a link out of that agent's work even if the
// registry somehow held one.
func (s *SubagentService) collectCancelsLocked(parent, rootID string) []func() {
	var out []func()
	frontier := []string{rootID}
	seen := map[string]bool{rootID: true}

	for len(frontier) > 0 {
		id := frontier[0]
		frontier = frontier[1:]

		if j, ok := s.jobs[id]; ok && j.Parent == parent && !j.terminal() && j.cancel != nil {
			out = append(out, j.cancel)
		}
		for childID, j := range s.jobs {
			if seen[childID] || j.CallerID != id || j.Parent != parent {
				continue
			}
			seen[childID] = true
			frontier = append(frontier, childID)
		}
	}
	return out
}

// CancelAllSubagents stops every one of parent's jobs that has not
// finished, and reports how many it cancelled.
//
// This is what Stop needs. Background jobs outlive the turn that
// dispatched them, so by the time someone reaches for Stop the parent
// usually has no live turn at all — there is no per-job id in play,
// just "whatever this agent still has running". Queued jobs count:
// they are owed a result and must not start after the CEO has said
// stop.
//
// Cancelled jobs deliver nothing, the same as a single
// CancelSubagent: a "your task was cancelled" message is RoleReceived,
// which the spawn gate reads as work owed a reply, and would wake the
// agent the CEO just stopped.
func (s *SubagentService) CancelAllSubagents(parent string) int {
	if s == nil {
		return 0
	}
	// Collect under the lock, cancel outside it: cancel unwinds
	// DriveSubagent, which publishes to the agent pod, and the job's
	// own goroutine takes jobsMu on its way out.
	s.jobsMu.Lock()
	var cancels []func()
	for _, j := range s.jobs {
		if j.Parent != parent || j.terminal() || j.cancel == nil {
			continue
		}
		cancels = append(cancels, j.cancel)
	}
	s.jobsMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// ForgetFinished drops parent's finished jobs from the registry and
// reports how many it dropped. Called when the agent's chat rotates.
//
// The registry is the panel's memory of what ran, and the chat is the
// context those jobs were dispatched from: their chips are in it,
// their results landed in it, and rotation archives it. So the
// finished set follows the chat into the archive and the fresh chat
// starts with an empty history — which is what subagent_status has
// always claimed with "none finished this session".
//
// Live jobs stay, queued or running: their results are owed to the
// NEW chat and they must remain cancellable. A finished job with a
// live descendant stays too, so the worker keeps the caller it is
// nested under instead of becoming an orphan that names a task no
// longer listed.
//
// Nothing on disk goes with them: each job's meta.json and transcript
// stay under subagents/<id>/, and the archived chat's chips still
// link there.
func (s *SubagentService) ForgetFinished(parent string) int {
	if s == nil {
		return 0
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()

	// Every caller above a live job, walking CallerID links up: those
	// are the finished jobs kept for a worker's sake.
	shelters := map[string]bool{}
	for _, j := range s.jobs {
		if j.Parent != parent || j.terminal() {
			continue
		}
		for id := j.CallerID; id != "" && !shelters[id]; {
			shelters[id] = true
			caller, ok := s.jobs[id]
			if !ok {
				break
			}
			id = caller.CallerID
		}
	}

	n := 0
	for id, j := range s.jobs {
		if j.Parent != parent || !j.terminal() || shelters[id] {
			continue
		}
		delete(s.jobs, id)
		n++
	}
	return n
}

// jobsRegistryFields is embedded into SubagentService. Kept here so
// the registry's state and its methods live together.
type jobsRegistryFields struct {
	jobsMu sync.Mutex
	jobs   map[string]*subagentJob
}
