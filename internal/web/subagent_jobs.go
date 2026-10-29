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
// but a job in flight when core restarts is gone with its goroutine:
// the pod's completion POST lands on a turn that no longer exists. At
// boot ResumeOrphanedSubagents registers again each task the agent
// dispatched itself that was queued or running, and runs it on from its
// CLI session (resumeAfterRestart), up to subagentRestartResumes
// restarts per task. Every other such meta.json (a worker, a task
// waiting for a person or awaiting approval, one with files, one past
// its resumes) is rewritten as errored, with subagentRestartError as
// the reason, so nothing reads as live that core no longer holds, and
// each agent that dispatched one is told. A restore resumes nothing
// (FailOrphanedSubagents).

package web

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/structured"
)

// subagentRestartError is the reason a task's record carries when core
// restarted while the task was queued, running, waiting for a person or
// awaiting approval: its goroutine went with the old process, so it
// never finishes.
const subagentRestartError = "Kivali restarted before the task finished"

// subagentRestartResumes is how many restarts of core one task is
// resumed after. A task still in flight at the restart after that ends
// errored: work that keeps being cut short may be what brings core
// down, as a starved node that fails core's health check does.
const subagentRestartResumes = 2

// restartResumePodWait bounds how long a task resumed after a restart
// waits for its agent's pod, which the boot's provisioning pass brings
// up within five minutes or gives up on.
const restartResumePodWait = 6 * time.Minute

// subagentResumeNoPodError is the reason a task resumed after a restart
// ends with when its agent's pod was not back within
// restartResumePodWait.
const subagentResumeNoPodError = "Kivali restarted before the task finished, and the agent was not back in time to resume it"

// restartResumeLine is the one user message a task's run gets when it
// is resumed after a restart; restartResumeWebLine is added for a web
// task, and the last line names how a finished task answers.
const (
	restartResumeLine           = "Kivali restarted while you were working, and your run stopped partway. Continue your task from where you left off. What you wrote under /files/ is still there, so check it before you redo anything. A tool call made around the restart may have failed; make it again if you still need it."
	restartResumeWebLine        = " The browser closed with the restart and your tabs are gone: your next web command starts a new browser in the same session."
	restartResumeTextDoneLine   = " If you had already finished, give your final answer again."
	restartResumeStructuredLine = " If you had already finished, call submit_result with your answer."
)

// FailOrphanedSubagents rewrites every background task's meta.json that
// still says queued, running, waiting for a person or awaiting approval
// as errored, and tells the agents that dispatched them, resuming
// nothing. A restore calls it: a backup's live records are a past run's.
// Returns how many records it rewrote and the agents it told.
func (s *SubagentService) FailOrphanedSubagents() (int, []string) {
	n, told, _ := s.recoverOrphanedSubagents(false)
	return n, told
}

// ResumeOrphanedSubagents is what a boot does with the background tasks
// the last run left in flight: each one recoverOrphanedSubagents finds
// resumable runs again (resumeAfterRestart), and the rest are rewritten
// as errored and reported. Returns how many records it rewrote as
// errored, the agents it told, and how many tasks it resumed.
func (s *SubagentService) ResumeOrphanedSubagents() (int, []string, int) {
	return s.recoverOrphanedSubagents(true)
}

// recoverOrphanedSubagents goes through every background task's
// meta.json that still says queued, running, waiting for a person or
// awaiting approval: core holds no such job after a restart, so nothing
// should read as live that is not. Called at boot, and by a restore,
// before anything dispatches.
//
// With resume, a task the agent dispatched itself that was queued or
// running is registered again and run on (resumableAfterRestart says
// which): its record reads queued, with its restarts counted. Every
// other record is rewritten as errored, with subagentRestartError, and
// without the wait reason and browser state a live task carries.
//
// Each active agent that dispatched one of the errored tasks itself is
// then told, in one subagent_result message through DeliverToParent,
// the door finishJob delivers through: an agent that ended its turn to
// wait for the results would otherwise wait for good. A resumed task
// tells nobody now: its result arrives as any task's does. A worker's
// record (one with a caller) is rewritten but not reported: its answer
// was owed to the sub-lead that dispatched it, whose run ended with it.
// An archived agent, or one whose record is gone, is told nothing.
//
// Returns how many records it rewrote as errored, the agents it told,
// and how many tasks it resumed. Nothing here wakes the agents told:
// the caller does, once their pods are up (Server.RecoverInterruptedWork).
func (s *SubagentService) recoverOrphanedSubagents(resume bool) (int, []string, int) {
	if s == nil || s.Store == nil {
		return 0, nil, 0
	}
	tasks, err := s.Store.SubagentMetaFiles()
	if err != nil {
		log.Printf("subagents: scan for unfinished tasks: %v", err)
		return 0, nil, 0
	}
	n := 0
	lost := map[string][]lostSubagent{}
	var resumes []restartResume
	active := map[string]bool{}
	for _, task := range tasks {
		parent, id := task[0], task[1]
		// Read before the record's lock is taken. An archived agent's
		// records sit under _archived/<slug>, and GetAgent would find
		// its archived record there.
		canResume := false
		if resume {
			ok, seen := active[parent]
			if !seen {
				ok = !strings.Contains(parent, "/")
				if ok {
					_, err := s.Store.GetAgent(parent)
					ok = err == nil
				}
				active[parent] = ok
			}
			canResume = ok
		}
		var ended *lostSubagent
		var again *restartResume
		err := s.Store.UpdateSubagentMeta(parent, id, func(meta map[string]any) bool {
			switch meta["status"] {
			case subagentJobQueued, subagentJobRunning, subagentJobWaiting, subagentJobAwaitingApproval:
			default:
				return false
			}
			if canResume {
				if r, ok := resumableAfterRestart(parent, id, meta); ok {
					meta["status"] = subagentJobQueued
					meta["error"] = ""
					meta["restarts"] = r.restarts
					meta["updated_at"] = time.Now().UTC().Format(time.RFC3339)
					delete(meta, "wait_reason")
					delete(meta, "browser")
					again = &r
					return true
				}
			}
			meta["status"] = "errored"
			meta["error"] = subagentRestartError
			meta["updated_at"] = time.Now().UTC().Format(time.RFC3339)
			delete(meta, "wait_reason")
			delete(meta, "browser")
			n++
			if caller, _ := meta["caller"].(string); caller == "" {
				description, _ := meta["description"].(string)
				isStructured, _ := meta["structured"].(bool)
				ended = &lostSubagent{id: id, description: description, structured: isStructured}
			}
			return true
		})
		if err != nil {
			log.Printf("subagent %s/%s: mark unfinished after restart: %v", parent, id, err)
			continue
		}
		if ended != nil {
			lost[parent] = append(lost[parent], *ended)
		}
		if again != nil {
			resumes = append(resumes, *again)
		}
	}
	for _, r := range resumes {
		s.resumeAfterRestart(r)
	}
	if len(resumes) > 0 {
		s.notifyWorkingChanged()
	}
	return n, s.tellLostSubagents(lost), len(resumes)
}

// restartResume is a task the last run left in flight, as
// resumeAfterRestart runs it again: the task as it was dispatched, the
// CLI session its last run reported (empty when it reported none, and
// the task starts over), and how many restarts it has now been resumed
// after.
type restartResume struct {
	parent   string
	id       string
	task     SubagentTaskInput
	session  string
	restarts int
}

// resumableAfterRestart reads a task's meta.json for a resume after a
// restart. A task is resumable when the agent dispatched it itself
// (a worker's answer was owed to a sub-lead whose run ended), it was
// queued or running (a wait for a person is a person's to pick up
// again), it names no files (their upload was a person's approval,
// which lived in memory), it has been resumed fewer than
// subagentRestartResumes times, and its record holds the task: the
// description and prompt, and for a task with a schema the schema
// itself. Called under the record's lock.
func resumableAfterRestart(parent, id string, meta map[string]any) (restartResume, bool) {
	switch meta["status"] {
	case subagentJobQueued, subagentJobRunning:
	default:
		return restartResume{}, false
	}
	if caller, _ := meta["caller"].(string); caller != "" {
		return restartResume{}, false
	}
	if files, _ := meta["files"].([]any); len(files) > 0 {
		return restartResume{}, false
	}
	restarts := 0
	if v, ok := meta["restarts"].(float64); ok && v > 0 {
		restarts = int(v)
	}
	if restarts >= subagentRestartResumes {
		return restartResume{}, false
	}
	var t SubagentTaskInput
	t.Description, _ = meta["description"].(string)
	t.Prompt, _ = meta["prompt"].(string)
	t.Model, _ = meta["model"].(string)
	t.Effort, _ = meta["effort"].(string)
	if strings.TrimSpace(t.Description) == "" || strings.TrimSpace(t.Prompt) == "" {
		return restartResume{}, false
	}
	if isStructured, _ := meta["structured"].(bool); isStructured {
		schema, ok := meta["schema"]
		if !ok || schema == nil {
			return restartResume{}, false
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			return restartResume{}, false
		}
		t.Schema = raw
		if parsedSchema(t) == nil {
			return restartResume{}, false
		}
	}
	if kind, _ := meta["kind"].(string); kind == agentpod.SubagentKindWeb {
		t.Kind = agentpod.SubagentKindWeb
		t.Session, _ = meta["session"].(string)
		t.StartURL, _ = meta["start_url"].(string)
	}
	t.toolUseID, _ = meta["tool_use_id"].(string)
	if v, ok := meta["index"].(float64); ok && v >= 0 {
		t.index = int(v)
	}
	session, _ := meta["cli_session"].(string)
	if !cliSessionID.MatchString(session) {
		session = ""
	}
	return restartResume{parent: parent, id: id, task: t, session: session, restarts: restarts + 1}, true
}

// resumeAfterRestart registers a task the last run left in flight under
// its own id, and runs it as launch runs a dispatched task: its chip,
// its record and its result are the task's as before. runJob waits for
// the agent's pod (restartResumePodWait), and the run continues the
// task's CLI session with restartResumeLineFor, or starts the task over
// when its last run reported no session.
func (s *SubagentService) resumeAfterRestart(r restartResume) {
	jobCtx, cancel := context.WithCancel(context.Background())
	job := s.newJob(r.parent, r.task.toolUseID, r.task.index, r.id, r.task, 1, "", time.Now().UTC(), cancel)
	job.CLISession = r.session
	job.afterRestart = true
	s.registerJob(job)
	log.Printf("subagent %s/%s: resuming after a restart (%d of %d)", r.parent, r.id, r.restarts, subagentRestartResumes)
	go func() {
		defer cancel()
		s.runJob(jobCtx, r.parent, r.task.toolUseID, job, r.task.index, r.task)
	}()
}

// restartResumeLineFor is the message a task's run resumed after a
// restart gets: what happened, for a web task what became of its
// browser, and how to answer if it had finished.
func restartResumeLineFor(job *subagentJob) string {
	line := restartResumeLine
	if job.Kind == agentpod.SubagentKindWeb {
		line += restartResumeWebLine
	}
	if job.Schema != nil {
		return line + restartResumeStructuredLine
	}
	return line + restartResumeTextDoneLine
}

// lostSubagent is a task recoverOrphanedSubagents rewrote as errored,
// as its agent is told about it.
type lostSubagent struct {
	id          string
	description string
	structured  bool
}

// tellLostSubagents delivers to each agent in lost one subagent_result
// message naming the tasks the restart ended, and returns the agents it
// reached, sorted.
func (s *SubagentService) tellLostSubagents(lost map[string][]lostSubagent) []string {
	parents := make([]string, 0, len(lost))
	for parent := range lost {
		parents = append(parents, parent)
	}
	sort.Strings(parents)
	var told []string
	for _, parent := range parents {
		// An archived agent's records sit under _archived/<slug>, and
		// GetAgent would find its archived record there.
		if strings.Contains(parent, "/") {
			continue
		}
		if _, err := s.Store.GetAgent(parent); err != nil {
			continue
		}
		if s.DeliverToParent == nil {
			log.Printf("subagents %s: no delivery hook; restart notice stranded", parent)
			continue
		}
		msg := store.ChatMessage{
			Role:    store.RoleReceived,
			Kind:    store.KindSubagentResult,
			Content: renderSubagentRestartMessage(lost[parent], s.OutstandingSubagents(parent), loadBackgroundPlan(s.Store, parent)),
			TS:      time.Now().UTC(),
		}
		if err := s.DeliverToParent(parent, msg); err != nil {
			log.Printf("subagents %s: deliver restart notice: %v", parent, err)
			continue
		}
		told = append(told, parent)
	}
	return told
}

// RecoverInterruptedWork is what a boot, and a restore, do with the work
// the last run left unfinished: at boot (resumeTasks) background tasks
// that can be are resumed and the rest marked errored, their agents
// told (ResumeOrphanedSubagents); a restore marks them all errored
// (FailOrphanedSubagents). Then interrupted turns get their disruption
// rows (RecoverInterruptedTurns). Returns every agent to wake once its
// pod is up, each once, for SpawnRecovered, which applies the spawn
// gate.
func (s *Server) RecoverInterruptedWork(resumeTasks bool) []string {
	var n, resumed int
	var told []string
	if resumeTasks {
		n, told, resumed = s.SubagentService.ResumeOrphanedSubagents()
	} else {
		n, told = s.SubagentService.FailOrphanedSubagents()
	}
	if resumed > 0 {
		log.Printf("subagents: %d task(s) left unfinished by the last run resumed", resumed)
	}
	if n > 0 {
		log.Printf("subagents: %d task(s) left unfinished by the last run marked errored; %d agent(s) told", n, len(told))
	}
	wake := s.RecoverInterruptedTurns()
	for _, slug := range told {
		if !slices.Contains(wake, slug) {
			wake = append(wake, slug)
		}
	}
	return wake
}

// Subagent job states. A job is terminal in every state except
// subagentJobQueued, subagentJobRunning, subagentJobWaiting and
// subagentJobAwaitingApproval.
//
// Queued is what admission control made visible: a dispatched job that
// has not been given a slot in the pod yet. It is emphatically NOT
// terminal — it is owed a result, it counts as outstanding work, and
// it must stay cancellable, because "call off the batch I just sent"
// is most useful precisely while the batch is still waiting to start.
const (
	subagentJobQueued  = "queued"
	subagentJobRunning = "running"
	// subagentJobWaiting: a web task in a site session paused by
	// web_wait_for_sign_in until a person has signed in through its
	// browser window; its run has ended and resumes afterwards. Not
	// terminal.
	subagentJobWaiting = "waiting_for_sign_in"
	// subagentJobAwaitingApproval: a web task that names files, held
	// before it starts until a person approves its uploads. Not
	// terminal; nothing has run and no browser is started.
	subagentJobAwaitingApproval = "awaiting_approval"
	subagentJobCompleted        = "completed"
	subagentJobFailed           = "failed"
	subagentJobCancelled        = "cancelled"
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

	// Site is a web task's site session's registrable domain, empty in
	// the open session. WaitReason describes a task in
	// subagentJobWaiting, and WaitSince when its current wait for a
	// person began, in subagentJobWaiting or
	// subagentJobAwaitingApproval. CLISession is the session id the
	// task's run reported, what a resumed run continues.
	Site       string
	WaitReason string
	WaitSince  time.Time
	CLISession string
	// afterRestart: the job is a task the last run of core left in
	// flight, registered again at boot (resumeAfterRestart). Its run
	// waits for the agent's pod and continues CLISession. Set at
	// registration, never mutated.
	afterRestart bool
	// cancelRun ends the task's current run and nothing else; a task
	// that waits for a person ends its run this way and runs again
	// later. resumed is closed when the wait is over; resumeDone says
	// it has been (both under jobsMu, so the close happens once).
	cancelRun  context.CancelFunc
	resumed    chan struct{}
	resumeDone bool
	// waitTimer ends the wait at signInWaitLimit; started as the wait
	// begins, so the limit counts from then. waits counts the task's
	// calls to web_wait_for_sign_in that were granted; at
	// signInWaitCap the next is refused and the task ends blocked.
	waitTimer clock.Timer
	waits     int
	// lost is closed when the task's browser ends under it while it
	// waits for a person (browserLost); lostDone says it has been (both
	// under jobsMu). reopens counts the browsers core opened again for
	// the current wait, and tabs says what became of the task's tabs,
	// for the line its resumed run gets.
	lost     chan struct{}
	lostDone bool
	reopens  int
	tabs     waitTabs

	// LockSite is the site a web task is held to for its whole run:
	// its session's site, or for a task that names files its start
	// URL's site. Empty for an open-session task without files. Set at
	// registration, never mutated.
	LockSite string
	// Prompt is the task's instruction, kept for the approval a person
	// reads. Set at registration, never mutated.
	Prompt string
	// ApprovedFiles are the task's Files as they were at dispatch, the
	// bytes a person approves and an upload sends. Set at registration,
	// never mutated.
	ApprovedFiles []webApprovedFile
	// Approved: a person approved the task's uploads. It covers the
	// uploads only; the web budget counts the task like any other.
	Approved bool
	// approval carries the person's answer to a task awaiting
	// approval; approvalDone says one was taken (both under jobsMu, so
	// one answer lands).
	approval     chan webUploadAnswer
	approvalDone bool

	// Depth is how far below the durable agent this job sits. 1 is a
	// subagent the agent dispatched itself; 2 is a worker dispatched
	// by one of those sub-leads. Recorded at registration and never
	// read from a request body — a nested dispatch's depth is derived
	// from the CALLER's recorded job, so a subagent cannot talk its
	// way deeper than the cap by claiming a smaller number.
	Depth int

	// toolUseID and index place the job in the chip that dispatched it:
	// the parent's subagent tool_use and the task's position in that
	// batch, which every subagent_progress event names. Set at
	// registration, never mutated.
	toolUseID string
	index     int

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

	// Schema is the caller's parsed schema for a task that answers as
	// data, nil for a free-text task. It decides which tools the run
	// gets and what SubmitResult checks a submission against.
	Schema *structured.Schema

	// Kind is general or web. A web task has a Session (its browser
	// session), a StartURL and Files it may upload, by path under the
	// caller's /files/. Set at registration, never mutated.
	Kind     string
	Session  string
	StartURL string
	Files    []string

	// Result is a structured task's answer as core delivers it: the
	// wrapper JSON, indented. Set by SubmitResult when a submission is
	// accepted, and by finishJob for the body core writes itself when
	// the task ended without one. Empty for a free-text task.
	Result string

	// ResultAttempts counts the submit_result calls that were refused.
	// At subagentResultAttempts core ends the task.
	ResultAttempts int

	// endReason is set, under jobsMu, before core cancels a run it is
	// ending itself, so the job's goroutine reports that reason as a
	// failure rather than reading the cancel as a Stop.
	endReason string

	// closing is set, under jobsMu, as structuredEnding reads the
	// job's answer, so a submit_result that lands after that read is
	// refused rather than accepted into a record nothing delivers.
	closing bool

	// cancelled is set, under jobsMu, when Stop or subagent_cancel
	// called the job off. A structured task then delivers nothing,
	// whatever it had submitted: the agent was stopped, and a result
	// landing after that would wake it.
	cancelled bool

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
	switch j.State {
	case subagentJobRunning, subagentJobQueued, subagentJobWaiting, subagentJobAwaitingApproval:
		return false
	}
	return true
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

// cliSessionID is the shape of a CLI session id: a UUID. The id is
// handed back to the CLI after --resume, so anything else, a value
// starting with a dash above all, is not kept.
var cliSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// recordCLISession keeps the session id a task's run reported, so a
// later run of the task can continue it: on the job, for a resume after
// a wait for a person, and in the task's meta.json, for a resume after
// a restart of core (resumeAfterRestart). An id that is not a UUID is
// logged and dropped.
func (s *SubagentService) recordCLISession(id, sessionID string) {
	if sessionID == "" {
		return
	}
	if !cliSessionID.MatchString(sessionID) {
		log.Printf("subagent %s: ignoring a CLI session id that is not a UUID (%d bytes)", id, len(sessionID))
		return
	}
	var parent string
	s.updateJob(id, func(j *subagentJob) {
		if j.CLISession != sessionID {
			j.CLISession = sessionID
			parent = j.Parent
		}
	})
	if parent == "" || s.Store == nil {
		return
	}
	err := s.Store.UpdateSubagentMeta(parent, id, func(meta map[string]any) bool {
		if len(meta) == 0 || meta["cli_session"] == sessionID {
			return false
		}
		meta["cli_session"] = sessionID
		return true
	})
	if err != nil {
		log.Printf("subagent %s/%s: record CLI session: %v", parent, id, err)
	}
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
// render as working rather than idle, and what keeps a pod restart
// waiting (holdIfIdle): recreating the pod would kill the jobs.
//
// It must not reach turnInFlight or deliverToAgent: an agent waiting
// on background tasks is emphatically still able to receive a message,
// and gating delivery on this would silently strand messages behind a
// long batch.
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
			j.cancelled = true
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

// collectEndCancelsLocked returns the cancel funcs for ending id's run
// as core ends it (an accepted result, the attempt cap, a browser that
// could not start): id's own cancel, with id NOT marked cancelled,
// because its outcome is already decided and is delivered, and every
// non-terminal descendant's, marked cancelled, because nobody is left
// to read their answers. jobsMu must be held.
func (s *SubagentService) collectEndCancelsLocked(parent, id string) []func() {
	root, ok := s.jobs[id]
	if !ok || root.Parent != parent {
		return nil
	}
	var out []func()
	if root.cancel != nil {
		out = append(out, root.cancel)
	}
	return append(out, s.collectWorkerCancelsLocked(parent, id)...)
}

// collectWorkerCancelsLocked returns the cancel funcs for every
// non-terminal descendant of id, each marked cancelled: once id's run
// is over, nobody is left to read their answers. jobsMu must be held.
func (s *SubagentService) collectWorkerCancelsLocked(parent, id string) []func() {
	var out []func()
	for childID, j := range s.jobs {
		if j.CallerID == id && j.Parent == parent {
			out = append(out, s.collectCancelsLocked(parent, childID)...)
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
		j.cancelled = true
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
