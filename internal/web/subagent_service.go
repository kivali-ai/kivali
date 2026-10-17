// Package web (subagent_service.go) — owns subagent runs end to end.
//
// The MCP subprocess only advertises the `subagent` tool; the actual
// work — input staging, CLI spawn, transcript watch, live progress
// events to the parent's chat hub — happens here in the web process.
// The MCP handler is a pure RPC proxy that POSTs to the control-socket
// /run-subagent endpoint and forwards what comes back to Claude Code
// as the tool_result.
//
// That tool_result is a DISPATCH RECEIPT, not an answer. Jobs run in
// the background and their results reach the parent later as messages
// (see finishJob), so the parent's turn is not pinned for the
// duration of the work. What is outstanding lives in
// subagent_jobs.go; the model-facing status/cancel tools in
// subagent_tools.go.
//
// This split exists for three reasons:
//   - Live UI updates: the parent's chat hub is in this process, so
//     emitting subagent_progress events to the chip happens with no
//     cross-process plumbing.
//   - One source of truth: the runner sees the same store and the
//     same filesystem layout the rest of Kivali sees. No
//     duplication.
//   - MCP server hygiene: the MCP subprocess gets simpler, which
//     supports the long-term direction of replacing it with an
//     in-process HTTP MCP server.

package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// subagentWatchPoll is how often each per-task watcher reads the
// subagent's chat.jsonl tail to decide what `current_activity` to
// publish. Same cadence as the existing transcript SSE handler.
const subagentWatchPoll = 250 * time.Millisecond

// SubagentDriver runs one subagent end-to-end: installs the per-
// turn-id state, publishes the ChatTurnKindSubagent event at the
// parent's agent pod, blocks until the runtime POSTs done/failed,
// and returns the final assistant text. The Server implements this;
// tests inject a fake.
type SubagentDriver interface {
	DriveSubagent(ctx context.Context, parent, turnID, subagentID string, spec agentpod.SubagentSpec) (string, error)
}

// SubagentService is the web-side owner of subagent execution. One
// instance per Server, wired in NewServer. StartBatch is the entry
// point invoked by the control-socket /run-subagent handler.
//
// Driver handles the actual subagent spawn — the in-process service
// is a pure orchestrator (input staging, output copy, transcript
// activity events, parent chat-hub fan-out) plus a thin wrapper
// around the agent-pod chat-turn publish.
type SubagentService struct {
	Store  *store.FSStore
	Driver SubagentDriver
	// Provider validates and resolves each task's model and effort.
	// NewServer binds it to Server.Provider when left nil.
	Provider provider.Provider

	// jobsRegistryFields carries the in-memory record of dispatched
	// background jobs. See subagent_jobs.go.
	jobsRegistryFields

	// DeliverToParent hands a finished job's result to the parent as
	// an inbound chat message. Wired to Server.deliverToAgent, which
	// means a result reaches a mid-turn parent by the same gentle
	// interrupt a released message uses — one mechanism, three
	// triggers (you typed, you released, a task finished).
	//
	// Injected rather than called directly so the service stays
	// testable without a Server.
	DeliverToParent func(parent string, msg store.ChatMessage) error

	// EmitToParent dispatches a typed event to the parent agent's
	// chat hub. Keys: "subagent_started", "subagent_progress",
	// "subagent_completed". Server.subagentEmit satisfies this — it's
	// a closure over Server's chatHubs map.
	EmitToParent func(parent, kind string, payload any)

	// Clock drives the activity watch poll. Wired from the Server in
	// NewServer; nil falls back to the real clock so a directly
	// constructed service still works.
	Clock clock.Clock

	// NotifyWorkingChanged tells the org snapshot that this parent's
	// outstanding-job count moved. Wired to Server.NotifyOrgState.
	//
	// Load-bearing for the waiting indicator, because /org/stream is
	// event-driven with no periodic fallback: nothing polls, so a
	// count that changes without firing this is a count the browser
	// never hears about. A finished job usually delivers a result,
	// which wakes the parent and notifies as a side effect — but a
	// CANCELLED job delivers nothing by design, so without this the
	// snapshot's waiting state and waiting_tasks count would sit there
	// after a Stop that did cancel the work.
	NotifyWorkingChanged func()

	// WakeParent starts the parent's chat loop if it is idle, so a
	// result that just landed actually gets read. Wired to
	// Server.spawnChatLoopIfIdle.
	//
	// Delivering is not the same as being heard. deliverToAgent has two
	// branches: mid-turn it stages the message and asks the turn to
	// wind up (the finalize then flushes and spawns a follow-up, so the
	// agent reads it), and IDLE it appends straight to chat.jsonl and
	// returns. Nothing in that second branch wakes anyone — every
	// delivery site pairs it with an explicit spawn, and this is the
	// spawn for subagent results.
	//
	// Idle is the normal case here: dispatching the batch is what ended
	// the parent's turn. Without the wake, results would pile up on
	// disk, correct and unread, until a human sent a message.
	//
	// Safe to call unconditionally after a successful delivery: the
	// gate is "if idle", so a parent that is mid-turn (or already
	// spawning for a sibling's result) is a no-op and the existing
	// flush/follow-up machinery covers it.
	WakeParent func(parent string)

	// Browsers is where web tasks' commands go and where their tabs
	// are released when they end. Nil means web tasks cannot run; a
	// dispatch of one is refused at validation.
	Browsers *browserRegistry

	// PodConnected reports whether the parent's agent pod is connected,
	// so a task resumed after a sign-in has somewhere to run. Wired to
	// the agentpod hub's subscriber count; nil means always.
	PodConnected func(parent string) bool

	// admissionOnce/admission bound how many subagent CLIs run at once
	// in one agent pod. Built lazily so a directly constructed service
	// (tests, fixtures) gets the production limits without wiring.
	admissionOnce sync.Once
	admission     *subagentAdmission
}

// adm returns the pod-concurrency gate, building it on first use.
func (s *SubagentService) adm() *subagentAdmission {
	s.admissionOnce.Do(func() {
		if s.admission == nil {
			s.admission = newSubagentAdmission(subagentPodConcurrency, subagentTier1Concurrency)
		}
	})
	return s.admission
}

// notifyWorkingChanged pushes a fresh org snapshot when the hook is
// wired. Nil-safe on both the service and the hook so the directly
// constructed services in tests and fixtures need no setup.
//
// Called on every transition that changes what the agent page would
// render: a job dispatched, a job starting, a job finishing. The
// snapshot is the agent page's only live signal once the parent's turn
// has ended, which is most of a batch's life.
func (s *SubagentService) notifyWorkingChanged() {
	if s == nil || s.NotifyWorkingChanged == nil {
		return
	}
	s.NotifyWorkingChanged()
}

// clk returns the service's time source, defaulting to the real one.
func (s *SubagentService) clk() clock.Clock {
	if s.Clock == nil {
		return clock.New()
	}
	return s.Clock
}

// SubagentBatchRequest is the wire shape the control-socket handler
// receives + forwards to StartBatch. JSON tag names match the MCP-side
// tool schema (tasks/description/prompt/model/inputs) so the body
// can be passed through with no rewriting.
type SubagentBatchRequest struct {
	Tasks []SubagentTaskInput `json:"tasks"`
}

// SubagentTaskInput is one task in a batched call. Mirror of the MCP
// schema's task object. It carries no input files: the subagent
// inherits a narrowed view of the parent's /files/ via the symlink
// overlay, so any path the parent could file_view is already reachable
// from the prompt.
type SubagentTaskInput struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Model       string `json:"model"`
	// Effort is the per-task reasoning level (claude effort). Empty
	// defaults to the fleet default (high) at spawn. Mirrors the
	// `effort` property in the subagent tool's input schema.
	Effort string `json:"effort"`
	// Schema is the caller's JSON Schema for an answer as data, in the
	// subset internal/structured accepts, or empty for a free-text
	// answer. Mirrors the `schema` property in the tool's input schema.
	Schema json.RawMessage `json:"schema,omitempty"`
	// Kind is general (the default) or web. A web task runs in a
	// browser: Session names the web session (open when empty),
	// StartURL where it starts, and Files the paths under the caller's
	// /files/ it may upload. Mirrors the tool's input schema.
	Kind     string   `json:"kind,omitempty"`
	Session  string   `json:"session,omitempty"`
	StartURL string   `json:"start_url,omitempty"`
	Files    []string `json:"files,omitempty"`
}

// hasSchema reports whether the task asks for a structured answer. A
// JSON null counts as absent, the way an omitted field does.
func (t SubagentTaskInput) hasSchema() bool {
	trimmed := strings.TrimSpace(string(t.Schema))
	return trimmed != "" && trimmed != "null"
}

// web reports whether the task is a web task.
func (t SubagentTaskInput) web() bool { return t.Kind == agentpod.SubagentKindWeb }

// session is the web session the task runs in: the one named, or open.
func (t SubagentTaskInput) session() string {
	if s := strings.TrimSpace(t.Session); s != "" {
		return s
	}
	return agentpod.OpenWebSession
}

// StartBatch is the entry point for one /run-subagent call. It
// validates, registers a job per task, starts them, and returns the
// dispatch receipt the parent's model sees as its tool_result —
// WITHOUT waiting for any of them.
//
// The ctx argument is the inbound HTTP request's, and is used only for
// validation-time work. Jobs deliberately do NOT inherit it: it is
// cancelled the moment this handler returns, which under the old
// blocking contract was the right lifetime and under this one would
// kill every job at dispatch. Each job gets its own cancellable
// context instead, held in the registry so subagent_cancel can reach
// it.
//
// Side effects:
//   - per-task subagent dirs created under data/agents/<parent>/subagents/<id>/
//   - jobs registered, so the parent renders as working until they finish
//   - subagent_started / _progress / _activity / _completed events on
//     the parent's chat hub, as before
//   - on completion, a subagent_result message delivered to the parent
func (s *SubagentService) StartBatch(ctx context.Context, parent string, raw json.RawMessage) (string, error) {
	if parent == "" {
		return "", fmt.Errorf("subagent: parent is required")
	}
	if s.Store == nil || s.Driver == nil {
		return "", fmt.Errorf("subagent: service not configured")
	}

	var req SubagentBatchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	if len(req.Tasks) == 0 {
		return "", fmt.Errorf("`tasks` is required and must be non-empty")
	}
	if len(req.Tasks) > mcp.SubagentMaxBatch {
		return "", fmt.Errorf("at most %d tasks per call (got %d)", mcp.SubagentMaxBatch, len(req.Tasks))
	}
	if err := validateTasks(s.Provider, req.Tasks); err != nil {
		return "", err
	}
	if err := s.validateWebTasks(parent, req.Tasks); err != nil {
		return "", err
	}

	// Look up the parent's tool_use_id for this batch. The runtime
	// appends the tool_use entry to chat.jsonl on stream-tool-use-end,
	// which races with the MCP subprocess dispatching the call to us.
	// In practice the entry is there by the time we get here, but we
	// retry briefly to be safe. Empty tool_use_id is acceptable — the
	// chip JS falls back to heuristic matching ("the most recent
	// non-terminal is-subagent chip").
	toolUseID := s.findPendingSubagentToolUseID(parent, 500*time.Millisecond)

	// Allocate per-task IDs upfront so the subagent_started event can
	// carry the full task list — the chip JS uses the IDs to subscribe
	// to per-task transcript SSE for the "view session →" link.
	taskIDs := make([]string, len(req.Tasks))
	for i := range req.Tasks {
		id, err := newSubagentID()
		if err != nil {
			return "", fmt.Errorf("alloc id: %w", err)
		}
		taskIDs[i] = id
	}

	// Seed the chip with the structured task list. The chip JS clears
	// any "running…" placeholders and renders the real rows once this
	// event arrives, regardless of timing wrt the parent's tool_use
	// chat.jsonl write.
	//
	// model and effort are the RESOLVED values — the same ones launch
	// records on the job — rendered the way the chip shows them: model
	// in friendly form (the delta event does the same for the parent's
	// own bubble), effort raw. A task the agent dispatched without
	// naming either still says what it is running on.
	startedTasks := make([]map[string]any, len(req.Tasks))
	for i, t := range req.Tasks {
		startedTasks[i] = map[string]any{
			"index":       i,
			"id":          taskIDs[i],
			"description": t.Description,
			"model":       provider.Label(s.Provider, resolveSubagentModel(s.Provider, t.Model)),
			"effort":      resolveSubagentEffort(s.Provider, resolveSubagentModel(s.Provider, t.Model), t.Effort),
		}
		if t.web() {
			// The chip shows a web task as one from its first row.
			startedTasks[i]["kind"] = agentpod.SubagentKindWeb
			startedTasks[i]["session"] = taskSession(t)
		}
	}
	s.emit(parent, "subagent_started", map[string]any{
		"parent":      parent,
		"tool_use_id": toolUseID,
		"tasks":       startedTasks,
	})

	s.launch(parent, toolUseID, req.Tasks, taskIDs, 1, "")

	return renderSubagentDispatch(parent, req.Tasks, taskIDs), nil
}

// launch registers one job per task and starts each on its own
// goroutine. Shared by the async tier-1 path (StartBatch, which does
// not wait) and the synchronous nested path (RunNestedBatch, which
// waits on the returned jobs' done channels).
//
// depth and callerID are supplied by the CALLER, never by the request
// body: tier-1 is always depth 1 with no caller, and a nested batch
// derives both from the dispatching job's own registry entry.
//
// Jobs start life queued rather than running. Admission happens inside
// runJob, so a batch that exceeds the pod's concurrency budget is
// registered in full — visible, cancellable, owed a result — and
// trickles into execution as slots free up.
func (s *SubagentService) launch(parent, toolUseID string, tasks []SubagentTaskInput, taskIDs []string, depth int, callerID string) []*subagentJob {
	now := time.Now().UTC()
	jobs := make([]*subagentJob, len(tasks))
	for i, t := range tasks {
		i, t := i, t
		// context.WithCancel off Background, NOT off ctx — see the
		// lifetime note on StartBatch.
		jobCtx, cancel := context.WithCancel(context.Background())
		// Model and effort are resolved HERE, once, and never again:
		// the job is what the panel, the chip and meta.json read, and
		// the spec the pod runs is built from the same job below. An
		// unset field would otherwise be blank on every screen while
		// the pod quietly ran the default.
		job := &subagentJob{
			ID:          taskIDs[i],
			Parent:      parent,
			Description: t.Description,
			Model:       resolveSubagentModel(s.Provider, t.Model),
			Effort:      resolveSubagentEffort(s.Provider, resolveSubagentModel(s.Provider, t.Model), t.Effort),
			State:       subagentJobQueued,
			QueuedAt:    now,
			Depth:       depth,
			CallerID:    callerID,
			toolUseID:   toolUseID,
			index:       i,
			// validateTasks accepted the schema before launch; a task
			// without one, or whose schema failed to parse, is free text.
			Schema:   parsedSchema(t),
			Kind:     taskKind(t),
			Session:  taskSession(t),
			Site:     taskSite(t),
			StartURL: strings.TrimSpace(t.StartURL),
			Files:    append([]string(nil), t.Files...),
			cancel:   cancel,
			done:     make(chan struct{}),
		}
		jobs[i] = job
		s.registerJob(job)
		go func() {
			defer cancel()
			s.runJob(jobCtx, parent, toolUseID, job, i, t)
		}()
	}
	// Once per batch, not once per job: the whole batch registers in
	// this loop, and a client refetches once either way. Without
	// it a batch dispatched while someone is watching does not appear
	// until the next thing happens to move.
	s.notifyWorkingChanged()
	return jobs
}

// runJob handles one dispatched task end-to-end: build the symlink
// overlay so the subagent inherits a narrowed view of the parent's
// /files/, publish the subagent chat-turn at the agent pod, watch the
// transcript for live activity, write meta.json snapshots at start +
// end. Files the subagent writes under artifacts/private/ persist on
// the PVC directly — no separate output-copy step (see
// docs/developers/files-and-publishing.md §"Subagent overlay").
//
// Runs on its own goroutine with nothing waiting on it, so every exit
// path must go through finishJob: that is what moves the job out of
// "running" (releasing the parent's working indicator) and delivers
// the outcome. An early return that skips it strands the parent
// displaying work that will never arrive.
func (s *SubagentService) runJob(ctx context.Context, parent, toolUseID string, job *subagentJob, index int, t SubagentTaskInput) {
	id := job.ID
	// The job carries the resolved model and effort (see launch). Fold
	// them back into the task so the spec and every meta.json snapshot
	// below record what actually runs, not the blank the agent sent.
	// Both fields are set at registration and never mutated, so this
	// read needs no lock.
	t.Model, t.Effort = job.Model, job.Effort
	overlayRoot := subagentFilesRoot(s.Store, parent, id)
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: overlayRoot}); err != nil {
		s.emitProgress(parent, toolUseID, index, id, "errored", "", "overlay: "+err.Error())
		s.finishJob(job, toolUseID, index, "", "", fmt.Errorf("overlay: %w", err))
		return
	}

	// Wait for a slot in the pod before forking anything. Everything
	// above this line is cheap filesystem work; everything below it
	// spawns a `claude` process that shares the parent's memory limit.
	//
	// The wait honours ctx, so a job cancelled while queued never
	// starts and never consumes a slot — which is what makes
	// "dispatch five, change your mind, cancel" cost nothing.
	s.writeSubagentMeta(parent, t, id, subagentJobQueued, "", "")
	s.emitProgress(parent, toolUseID, index, id, subagentJobQueued, "", "")
	if err := s.adm().acquire(ctx, parent, job.Depth); err != nil {
		s.writeSubagentMeta(parent, t, id, "cancelled", err.Error(), "")
		s.finishJob(job, toolUseID, index, "", "", err)
		return
	}
	// The slot is held while a run is on and given back while the task
	// waits for a person (a web task's sign-in), so a wait of hours
	// costs the parent nothing.
	held := true
	release := func() {
		if held {
			held = false
			s.adm().release(parent, job.Depth)
		}
	}
	defer release()

	s.updateJob(id, func(j *subagentJob) {
		j.State = subagentJobRunning
		j.StartedAt = time.Now().UTC()
	})
	s.writeSubagentMeta(parent, t, id, "running", "", "")
	s.emitProgress(parent, toolUseID, index, id, "running", "", "")
	// queued → running does not change the OUTSTANDING count, so this
	// transition would otherwise be invisible to the agent page: its
	// Background view refreshes off /org/stream, and a job that
	// started would keep reading "queued" until something else moved.
	// emitProgress above only reaches the parent's chat hub, which by
	// this point usually no longer exists — dispatching the batch is
	// what ended the parent's turn.
	s.notifyWorkingChanged()

	// Seed the subagent transcript with the inbound prompt — gives the
	// task's transcript the same "kicked off by ..." anchor the parent's
	// chat has.
	if err := s.Store.AppendSubagentMessage(parent, id, store.ChatMessage{
		Role:    store.RoleReceived,
		Kind:    "direct_chat",
		Content: t.Prompt,
		TS:      time.Now().UTC(),
	}); err != nil {
		log.Printf("subagent %s/%s: seed transcript: %v", parent, id, err)
	}

	// Spawn a watcher goroutine that derives current_activity from
	// the subagent's chat.jsonl tail. Stops when the run exits.
	stopWatch := make(chan struct{})
	go s.watchActivity(parent, toolUseID, index, id, stopWatch)

	// Depth comes from the job core registered, which was derived from
	// the dispatching job — never from the request that asked for this
	// work. Everything tier-dependent (which tools exist, which
	// delegation frame the prompt carries) is computed here, on the
	// authoritative side, and shipped to the pod as a finished spec.
	depth := job.Depth
	if depth < 1 {
		depth = 1
	}
	// Schema, Kind, Session, StartURL and Files are read without the
	// lock like Model and Effort: set at registration, never mutated.
	structuredTask := job.Schema != nil
	spec := agentpod.SubagentSpec{
		SubagentID:   id,
		Description:  t.Description,
		Model:        t.Model,
		Effort:       t.Effort,
		SystemPrompt: agentpod.SubagentSystemPrompt(parent, t.Description, depth, structuredTask),
		UserPrompt:   t.Prompt,
		Tools:        agentpod.SubagentTools(depth, structuredTask),
		Depth:        depth,
	}
	if structuredTask {
		spec.Schema = job.Schema.Raw
	}
	if job.Kind == agentpod.SubagentKindWeb {
		// A web task runs the browser toolkit and nothing else; its
		// prompt says pages are untrusted and the answer is data. Only
		// a task in a site session, dispatched by the agent itself, may
		// wait for a person to sign in.
		names := webTaskFileNames(job.Files)
		canWait := job.Site != "" && depth == 1
		spec.Kind = agentpod.SubagentKindWeb
		spec.Session = job.Session
		spec.Site = job.Site
		spec.StartURL = job.StartURL
		spec.Files = names
		spec.Tools = agentpod.WebSubagentTools(job.Session, canWait)
		spec.SystemPrompt = agentpod.WebSubagentSystemPrompt(parent, t.Description, job.Session, job.Site, job.StartURL, names, canWait)
	}

	// One run, or several: a web task that waits for a person ends its
	// run and runs again, in the same session, when the person is done.
	var finalText string
	var runErr error
drive:
	for {
		turnID, terr := newAgentpodTurnIDForSubagent()
		if terr != nil {
			close(stopWatch)
			s.emitProgress(parent, toolUseID, index, id, "errored", "", "alloc turn-id: "+terr.Error())
			s.writeSubagentMeta(parent, t, id, "errored", terr.Error(), "")
			s.finishJob(job, toolUseID, index, "", "", terr)
			return
		}
		runCtx, cancelRun := context.WithCancel(ctx)
		s.updateJob(id, func(j *subagentJob) { j.cancelRun = cancelRun })
		finalText, runErr = s.Driver.DriveSubagent(runCtx, parent, turnID, id, spec)
		cancelRun()
		if !s.jobWaiting(id) {
			break
		}
		release()
	wait:
		for {
			switch s.awaitSignIn(ctx, job) {
			case signInResumed:
				// The person is done. The task reads as running from
				// here, so Home drops its need and the window stops
				// taking input, while the run is brought back: first
				// the pod it runs in, then a slot in it.
				var sid string
				var snapshot subagentJob
				s.updateJob(id, func(j *subagentJob) {
					sid = j.CLISession
					j.State = subagentJobRunning
					snapshot = *j
				})
				s.leftWait(snapshot)
				if !s.awaitPod(ctx, parent) {
					if ctx.Err() != nil {
						finalText, runErr = "", context.Canceled
						break drive
					}
					// Nobody to run it on within the bound: the wait
					// goes on as it was, for the next window to close.
					log.Printf("subagent %s/%s: agent pod not connected; the task waits on", parent, id)
					s.reenterWait(id)
					continue wait
				}
				if err := s.adm().acquire(ctx, parent, job.Depth); err != nil {
					finalText, runErr = "", err
					break drive
				}
				held = true
				s.endWait(id)
				s.updateJob(id, func(j *subagentJob) {
					j.WaitReason = ""
					j.WaitSince = time.Time{}
				})
				if sid == "" {
					const why = "the task could not be resumed: its run reported no session"
					s.updateJob(id, func(j *subagentJob) { j.endReason = why })
					finalText, runErr = "", errors.New(why)
					break drive
				}
				spec.Resume = sid
				spec.UserPrompt = signInResumeLine
				// The resumed run's one message is in the transcript
				// like the first run's prompt.
				if err := s.Store.AppendSubagentMessage(parent, id, store.ChatMessage{
					Role:    store.RoleReceived,
					Kind:    "direct_chat",
					Content: signInResumeLine,
					TS:      time.Now().UTC(),
				}); err != nil {
					log.Printf("subagent %s/%s: resume transcript: %v", parent, id, err)
				}
				break wait
			case signInTimedOut:
				// Nobody came: core writes the answer.
				s.updateJob(id, func(j *subagentJob) {
					j.State = subagentJobRunning
					j.WaitReason = ""
					j.Result = structured.Blocked(structured.BlockedUserDidNotRespond).Body()
				})
				finalText, runErr = "", nil
				break drive
			case signInCancelled:
				s.endWait(id)
				finalText, runErr = "", context.Canceled
				break drive
			}
		}
	}

	close(stopWatch)

	// A structured task's answer is what core accepted through
	// submit_result, or the body core writes when it ended without one.
	// The run's text never counts: it is the subagent's, and the
	// caller asked for data in a shape it wrote.
	var result string
	if structuredTask {
		result, runErr = s.structuredEnding(job, runErr)
		finalText = ""
	}
	if job.Kind == agentpod.SubagentKindWeb {
		// The browser's tabs for this task are released whatever the
		// ending, and what core saw of the run joins a delivered body.
		result = s.finishWebJob(job, result)
	}

	if runErr != nil {
		s.writeSubagentMeta(parent, t, id, "errored", runErr.Error(), result)
		s.emitProgress(parent, toolUseID, index, id, "errored", finalText, runErr.Error())
		s.finishJob(job, toolUseID, index, finalText, result, runErr)
		return
	}
	s.writeSubagentMeta(parent, t, id, "completed", "", result)
	s.emitProgress(parent, toolUseID, index, id, "completed", resultOrText(result, finalText), "")
	s.finishJob(job, toolUseID, index, finalText, result, nil)
}

// subagentResultAttempts is how many submit_result calls a task may
// have refused before core ends it: enough to fix a shape mistake from
// the errors, few enough that a subagent that cannot fit the schema
// stops spending.
const subagentResultAttempts = 3

// taskKind is the kind a task runs as: web when it says so, general
// otherwise (validateTasks refused anything else).
func taskKind(t SubagentTaskInput) string {
	if t.web() {
		return agentpod.SubagentKindWeb
	}
	return agentpod.SubagentKindGeneral
}

// taskSession is the web session a task runs in, empty for a general
// task: open, or a site session named by its site (validation accepted
// the name, so a site is what webSessionSite makes of it).
func taskSession(t SubagentTaskInput) string {
	if !t.web() {
		return ""
	}
	if site := taskSite(t); site != "" {
		return site
	}
	return t.session()
}

// taskSite is the site a web task's session is locked to, empty for
// the open session and for a general task.
func taskSite(t SubagentTaskInput) string {
	if !t.web() {
		return ""
	}
	site, err := webSessionSite(t.session())
	if err != nil {
		return ""
	}
	return site
}

// webTaskFileNames are the names a web task's files are uploaded by:
// each path's last element.
func webTaskFileNames(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, path.Base(p))
	}
	return names
}

// finishWebJob releases the task's tabs in its browser and returns the
// delivered body with core's facts about the run: the session, the
// last page's origin, the duration, the pages loaded, and every
// download under the task's untrusted-downloads/. A cancelled task,
// which delivers nothing, keeps its empty body.
func (s *SubagentService) finishWebJob(job *subagentJob, result string) string {
	if s.Browsers == nil {
		return result
	}
	facts, downloads := s.Browsers.ReleaseGroup(job.Parent, job.Session, job.ID)
	if result == "" {
		return result
	}
	facts.Session = job.Session
	s.jobsMu.Lock()
	started := job.StartedAt
	s.jobsMu.Unlock()
	if !started.IsZero() {
		facts.DurationSeconds = int(s.clk().Now().Sub(started).Seconds())
	}
	callerPaths := make([]string, 0, len(downloads))
	for _, name := range downloads {
		callerPaths = append(callerPaths, subagentFilesPrefix(job.ID)+files.UntrustedDownloadsDir+"/"+name)
	}
	return structured.WithWebFacts(result, facts, callerPaths)
}

// parsedSchema is the task's schema as the job carries it, nil for a
// free-text task. validateTasks has already refused one outside the
// subset, so a parse failure here is treated as no schema rather than
// a second rejection.
func parsedSchema(t SubagentTaskInput) *structured.Schema {
	if !t.hasSchema() {
		return nil
	}
	s, err := structured.Parse(t.Schema)
	if err != nil {
		return nil
	}
	return s
}

// subagentFilesPrefix is the task's own /files/ tree as the CALLER
// addresses it. The subagent sees the same tree at /files/.
func subagentFilesPrefix(id string) string {
	return "/files/subagents/" + id + "/"
}

// subagentPrivateArtifacts is the task's own scratch under its /files/,
// the real directory files.BuildSubagentOverlay makes.
const subagentPrivateArtifacts = "artifacts/private"

// subagentFileRules are the directories a task's result may name files
// in, as the subagent sees them, mapped to the caller's view: the
// task's own artifacts, and the shared workspace, which both see at
// the same path. FileExists refuses a path the task did not make: a
// regular file at that place in the task's view, never a link.
func (s *SubagentService) subagentFileRules(parent, id, kind string) structured.Rules {
	root := subagentFilesRoot(s.Store, parent, id)
	callerPrefix := subagentFilesPrefix(id)
	dirs := map[string]string{
		"/files/" + subagentPrivateArtifacts + "/": callerPrefix + subagentPrivateArtifacts + "/",
		"/files/" + files.SharedWorkspaceDir + "/": "/files/" + files.SharedWorkspaceDir + "/",
	}
	if kind == agentpod.SubagentKindWeb {
		// A web task writes nothing; what it may name is what its
		// browser downloaded, which core wrote for it.
		dirs = map[string]string{
			"/files/" + files.UntrustedDownloadsDir + "/": callerPrefix + files.UntrustedDownloadsDir + "/",
		}
	}
	return structured.Rules{
		Files: dirs,
		FileExists: func(callerPath string) bool {
			// The allowed directory the path is under, as the task's
			// view has it, and the file's path within it.
			var dir, rel string
			for taskDir, callerDir := range dirs {
				if strings.HasPrefix(callerPath, callerDir) {
					dir, rel = strings.TrimPrefix(taskDir, "/files/"), strings.TrimPrefix(callerPath, callerDir)
					break
				}
			}
			if dir == "" || rel == "" {
				return false
			}
			return regularFileUnder(filepath.Join(root, filepath.FromSlash(dir)), filepath.FromSlash(rel))
		},
	}
}

// regularFileUnder reports whether rel names a regular file under base
// through no link of the task's making: not at its last element
// (Lstat), and not on the way there, since the directory holding the
// file must resolve to a place under base's own resolution. base may
// itself be a link core made (background/ is the parent's workspace);
// a link planted inside it to a directory elsewhere, or a path that
// climbs out, names no file of the task's.
func regularFileUnder(base, rel string) bool {
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	full := filepath.Join(base, clean)
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return false
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return false
	}
	return realDir == realBase || strings.HasPrefix(realDir, realBase+string(filepath.Separator))
}

// subagentAcceptGrace is how long an accepted run keeps going before
// core ends it: time for the tool result to reach the CLI and the
// transcript, so the run's last row is the acceptance and not a kill.
// Core ends it at all because an accepted result is the task's end: a
// run that went on would spend tokens, and could change the files it
// named, on an answer that is already on its way.
const subagentAcceptGrace = 3 * time.Second

// endRunAfterAccept stops id's run once subagentAcceptGrace has passed,
// unless it has already ended, and with it the workers a sub-lead
// dispatched, whose answers nobody is left to read. The result is
// accepted, so finishJob delivers it as completed however the process
// exits.
func (s *SubagentService) endRunAfterAccept(id string) {
	t := s.clk().NewTimer(subagentAcceptGrace)
	defer t.Stop()
	<-t.C()
	var ends []func()
	s.updateJob(id, func(j *subagentJob) {
		if j.State == subagentJobRunning && j.Result != "" {
			ends = s.collectEndCancelsLocked(j.Parent, id)
		}
	})
	for _, end := range ends {
		end()
	}
}

// resultOrText is what a chip shows as a task's output: the JSON of a
// structured answer, fenced, or the free-text answer.
func resultOrText(result, finalText string) string {
	if result != "" {
		return "```json\n" + result + "\n```"
	}
	return finalText
}

// structuredEnding decides how a task with a schema ended once its run
// is over: the body core delivers and the error to record, if any.
// Every body is core's own text; the run's error detail stays on the
// job record and in meta.json, never in what the caller reads.
func (s *SubagentService) structuredEnding(job *subagentJob, runErr error) (string, error) {
	s.jobsMu.Lock()
	// From here the answer is decided: a submit_result landing later
	// is refused (SubmitResult reads closing) rather than recorded on
	// a job whose body is already on its way.
	job.closing = true
	accepted, ended, cancelled := job.Result, job.endReason, job.cancelled
	s.jobsMu.Unlock()
	switch {
	case cancelled:
		// Stop or subagent_cancel: nothing is delivered, accepted or
		// not, as for a free-text task. Canceled is what finishJob reads
		// as cancelled, whatever error the dying run reported.
		return "", context.Canceled
	case accepted != "":
		// Accepted before the run ended: the answer stands however the
		// process then exited, including the end core gave it.
		return accepted, nil
	case ended != "":
		// Core ended the task itself (the attempt cap). The cancel that
		// stopped the run was the means; the reason is what the caller
		// should read.
		return structured.Failed(ended).Body(), errors.New(ended)
	case runErr != nil && errors.Is(runErr, context.Canceled):
		return "", runErr
	case runErr != nil:
		return structured.Failed("the task ended before it submitted a result").Body(), runErr
	default:
		err := errors.New("the task finished without calling submit_result")
		return structured.Failed(err.Error()).Body(), err
	}
}

// SubmitResult is one submit_result call from a running task of
// parent's. It validates the submission against the task's schema and
// either records it as the job's answer or returns the errors. The
// text is the tool result the subagent reads; isError marks a
// rejection.
//
// The first accepted result is final: a second call is refused, so a
// subagent cannot replace an answer that may already be on its way.
// After subagentResultAttempts refusals core ends the task, delivering
// outcome failed; the subagent's submissions never reach the caller.
func (s *SubagentService) SubmitResult(parent, id string, submission json.RawMessage) (string, bool) {
	if s == nil {
		return "submit_result: subagents are not available in this deployment.", true
	}
	s.jobsMu.Lock()
	j, ok := s.jobs[id]
	switch {
	case !ok || j.Parent != parent:
		s.jobsMu.Unlock()
		return fmt.Sprintf("submit_result: no running task %s of yours.", id), true
	case j.Schema == nil:
		s.jobsMu.Unlock()
		return "submit_result: this task has no schema; your final message is your answer.", true
	case j.State != subagentJobRunning || j.endReason != "" || j.cancelled || j.closing:
		s.jobsMu.Unlock()
		return "submit_result: this task is no longer running.", true
	case j.Result != "":
		s.jobsMu.Unlock()
		return "submit_result: a result was already accepted for this task and is final. End your turn.", true
	}
	schema, kind := j.Schema, j.Kind
	s.jobsMu.Unlock()

	res, errs := structured.Validate(schema, submission, s.subagentFileRules(parent, id, kind))
	if len(errs) > 0 {
		var attempt int
		var ends []func()
		s.updateJob(id, func(j *subagentJob) {
			j.ResultAttempts++
			attempt = j.ResultAttempts
			// Never past an accepted result: a model may send a valid
			// and an invalid call in one message, and the valid one
			// stands.
			if attempt >= subagentResultAttempts && j.endReason == "" && j.Result == "" && j.State == subagentJobRunning && !j.cancelled && !j.closing {
				// The reason names no detail from the submissions:
				// what the subagent wrote stays with the subagent.
				j.endReason = fmt.Sprintf("no valid result after %d submit_result calls", subagentResultAttempts)
				// The task's run ends, and with it the workers a
				// sub-lead dispatched: nobody is left to read them.
				ends = s.collectEndCancelsLocked(parent, id)
			}
		})
		var b strings.Builder
		fmt.Fprintf(&b, "Result not accepted (attempt %d of %d):\n", attempt, subagentResultAttempts)
		for _, e := range errs {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
		if len(ends) > 0 {
			log.Printf("subagent %s/%s: ending after %d refused results; last errors: %s", parent, id, attempt, strings.Join(errs, "; "))
			b.WriteString("\nThat was the last attempt. The task has been ended without an answer.")
			for _, end := range ends {
				end()
			}
		} else {
			b.WriteString("\nFix these and call submit_result again.")
		}
		return b.String(), true
	}

	body := res.Body()
	accepted := false
	s.updateJob(id, func(j *subagentJob) {
		if j.Result == "" && j.State == subagentJobRunning && j.endReason == "" && !j.cancelled && !j.closing {
			j.Result = body
			accepted = true
		}
	})
	if !accepted {
		return "submit_result: this task already has its result or is no longer running. End your turn.", true
	}
	go s.endRunAfterAccept(id)
	return fmt.Sprintf("Result accepted with outcome %q. It is final and is on its way to your caller. End your turn now: nothing else you say reaches them.", res.Outcome), false
}

// finishJob is the single exit for a background job: it moves the job
// to a terminal state, closes out the chip, and hands the outcome to
// the parent as a message.
//
// Delivery rides DeliverToParent (Server.deliverToAgent), which is
// the same door a released inbox message and a direct chat post come
// through. So a result landing on a mid-turn parent takes the gentle
// interrupt at the next clean step boundary, and one landing on an
// idle parent writes straight in and wakes it. Nothing here needs to
// know which case it is.
//
// result is a structured task's answer as the wrapper JSON (accepted,
// or written by core), empty for a free-text task; finalText is the
// run's own text, which a structured task never delivers.
func (s *SubagentService) finishJob(job *subagentJob, toolUseID string, index int, finalText, result string, runErr error) {
	state := subagentJobCompleted
	switch {
	case runErr != nil && errors.Is(runErr, context.Canceled) && result == "":
		state = subagentJobCancelled
	case runErr != nil:
		state = subagentJobFailed
	}
	s.updateJob(job.ID, func(j *subagentJob) {
		j.State = state
		j.EndedAt = time.Now().UTC()
		j.FinalText = finalText
		j.Result = result
		j.Err = errString(runErr)
		if j.waitTimer != nil {
			// A wait that ended with the job, however it ended.
			j.waitTimer.Stop()
		}
	})
	// Wake anything waiting on this job before any of the reporting
	// below. A nested batch's caller is blocked here, and it should
	// not be held up by chip emission or message delivery.
	if job.done != nil {
		close(job.done)
	}

	// The job just left the outstanding set, so the parent's waiting
	// count moved. Push it: /org/stream never polls, and a cancelled
	// job delivers nothing that would notify as a side effect.
	s.notifyWorkingChanged()

	// subagent_completed is the chip's signal to disconnect its
	// per-task watcher and lock the row in its final state. Emitted
	// per job now rather than once per batch — jobs finish
	// independently, so there is no batch-wide moment to report.
	s.emit(job.Parent, "subagent_completed", map[string]any{
		"parent":      job.Parent,
		"tool_use_id": toolUseID,
		"tasks": []map[string]any{{
			"index":      index,
			"id":         job.ID,
			"final_text": resultOrText(result, finalText),
			"error":      errString(runErr),
		}},
	})

	// A cancelled job reports nothing back. Cancellation is always
	// something that was asked for — either the CEO pressed Stop
	// (which cancels the parent's turn AND every subagent under it)
	// or the agent called subagent_cancel itself. Delivering a
	// RoleReceived "your task was cancelled" in the Stop case would
	// be actively wrong: the spawn gate treats an unanswered received
	// entry as work to do, so the agent the CEO just stopped would
	// wake straight back up to read about it.
	if state == subagentJobCancelled {
		return
	}
	// A nested job's answer is already going where it belongs: back to
	// the sub-lead that dispatched it, as the return value of its
	// blocking tool call. Delivering it to the durable agent as well
	// would hand the CEO's agent a pile of intermediate worker output
	// it never asked for, and — worse — wake it, since a RoleReceived
	// entry reads to the spawn gate as work owed a reply.
	if job.Depth > 1 {
		return
	}
	if s.DeliverToParent == nil {
		log.Printf("subagent %s/%s: no delivery hook; result stranded", job.Parent, job.ID)
		return
	}
	msg := store.ChatMessage{
		Role: store.RoleReceived,
		Kind: store.KindSubagentResult,
		// Outstanding is read AFTER this job was moved to a terminal
		// state above, so it counts siblings only. The plan is read off
		// disk at delivery time rather than captured at dispatch: the
		// agent may have rewritten it since, and a stale count in the
		// nudge would be worse than no count.
		Content: renderSubagentResultMessage(job.ID, job.Description, state, finalText, result, runErr,
			s.OutstandingSubagents(job.Parent), loadBackgroundPlan(s.Store, job.Parent)),
		TS: time.Now().UTC(),
	}
	if err := s.DeliverToParent(job.Parent, msg); err != nil {
		log.Printf("subagent %s/%s: deliver result: %v", job.Parent, job.ID, err)
		return
	}
	// Delivered — now make sure someone reads it. Only on success:
	// there is nothing new for a woken agent to find if the write
	// failed.
	if s.WakeParent != nil {
		s.WakeParent(job.Parent)
	}
}

// newAgentpodTurnIDForSubagent returns a fresh 16-hex-char turn id
// for a subagent run. Sibling of newAgentpodTurnID (which is the
// chat-turn variant) — same source of uniqueness, different log-grep
// affordance.
func newAgentpodTurnIDForSubagent() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// watchActivity polls the subagent's chat.jsonl until stop fires, and
// emits subagent_progress events whose `current_activity` reflects
// the latest meaningful entry. Heuristics:
//   - last entry is a tool_use with no matching tool_result yet →
//     "running <toolname>"
//   - last entry is a tool_result → "thinking" (model is between
//     tool calls, awaiting next assistant turn)
//   - last entry is a sent direct_chat → "responding"
//
// We don't emit on every poll — only when the activity string would
// actually change. Reduces hub traffic.
func (s *SubagentService) watchActivity(parent, toolUseID string, index int, id string, stop <-chan struct{}) {
	// The transcript AppendSubagentMessage writes, beside the job's
	// meta.json in core's own agents/<parent>/subagents/<id>/ — the
	// one the task transcript page reads. Not anything under the
	// subagent's /files/ root (…/<storage>/subagents/<id>/), which the
	// agent writes: a chat.jsonl there is the agent's, and a link or
	// FIFO planted under that name would be followed, or block, here.
	chatPath := subagentChatPath(s.Store.Root(), parent, id)
	tk := s.clk().NewTicker(subagentWatchPoll)
	defer tk.Stop()
	var lastActivity string
	for {
		select {
		case <-stop:
			return
		case <-tk.C():
			activity := deriveCurrentActivity(chatPath)
			if activity == lastActivity {
				continue
			}
			lastActivity = activity
			// A CHANGE in derived activity means the transcript grew,
			// which is the liveness signal: it is the last moment this
			// job demonstrably did something. Recorded on the job so
			// subagent_status can report how long a task has been
			// silent — the raw material for judging "stuck" before any
			// threshold is chosen.
			s.updateJob(id, func(j *subagentJob) {
				j.Activity = activity
				j.LastActivityAt = time.Now().UTC()
			})
			s.emit(parent, "subagent_activity", map[string]any{
				"parent":           parent,
				"tool_use_id":      toolUseID,
				"index":            index,
				"id":               id,
				"current_activity": activity,
			})
		}
	}
}

// deriveCurrentActivity walks the subagent's chat.jsonl backward to
// the most recent meaningful entry and returns a human-readable
// activity string ("running file_view", "thinking", "responding",
// or empty when there's nothing yet).
func deriveCurrentActivity(chatPath string) string {
	hist, err := readSubagentHistory(chatPath)
	if err != nil || len(hist) == 0 {
		return ""
	}
	// Track tool_use ids we've seen tool_results for, scanning forward.
	completed := map[string]bool{}
	for _, m := range hist {
		if m.Kind == "tool_result" && m.ToolUseID != "" {
			completed[m.ToolUseID] = true
		}
	}
	// Walk backward to the most recent entry.
	last := hist[len(hist)-1]
	switch last.Kind {
	case "tool_use":
		if completed[last.ToolUseID] {
			return "thinking"
		}
		if last.ToolName != "" {
			return "running " + last.ToolName
		}
		return "running tool"
	case "tool_result":
		return "thinking"
	case "direct_chat":
		if last.Role == store.RoleSent {
			return "responding"
		}
		return ""
	default:
		return ""
	}
}

// emit is a nil-safe wrapper around EmitToParent.
func (s *SubagentService) emit(parent, kind string, payload any) {
	if s.EmitToParent != nil {
		s.EmitToParent(parent, kind, payload)
	}
}

// emitProgress emits subagent_progress with the standard payload
// shape. Status is "running" | "completed" | "errored".
func (s *SubagentService) emitProgress(parent, toolUseID string, index int, id, status, finalText, errMsg string) {
	s.emit(parent, "subagent_progress", map[string]any{
		"parent":      parent,
		"tool_use_id": toolUseID,
		"index":       index,
		"id":          id,
		"status":      status,
		"final_text":  finalText,
		"error":       errMsg,
	})
}

// findPendingSubagentToolUseID scans the parent's chat.jsonl for the
// most recent `tool_use` entry whose ToolName is the subagent tool
// AND has no matching tool_result yet. Polls briefly so we can win
// the race against the runtime's StreamToolUseEnd → AppendChatMessage
// write. Returns "" if no match within the timeout — callers fall
// back to heuristic chip matching client-side.
func (s *SubagentService) findPendingSubagentToolUseID(parent string, timeout time.Duration) string {
	chatPath := filepath.Join(s.Store.Root(), "agents", parent, "chat.jsonl")
	deadline := time.Now().Add(timeout)
	for {
		if id := scanPendingSubagentToolUseID(chatPath); id != "" {
			return id
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// scanPendingSubagentToolUseID is the inner scan: read chat.jsonl,
// walk forward, build a set of tool_use_ids that have matching
// tool_results, and return the most recent subagent tool_use id NOT
// in that set.
func scanPendingSubagentToolUseID(chatPath string) string {
	hist, err := readSubagentHistory(chatPath)
	if err != nil {
		return ""
	}
	completed := map[string]bool{}
	for _, m := range hist {
		if m.Kind == "tool_result" && m.ToolUseID != "" {
			completed[m.ToolUseID] = true
		}
	}
	for i := len(hist) - 1; i >= 0; i-- {
		m := hist[i]
		if m.Kind != "tool_use" {
			continue
		}
		if !isSubagentToolName(m.ToolName) {
			continue
		}
		if !completed[m.ToolUseID] {
			return m.ToolUseID
		}
	}
	return ""
}

// renderSubagentDispatch builds the tool_result body the parent's
// model sees the instant it dispatches — a receipt, not an answer.
//
// The wording works hard on one point: the model must not read this as
// "the tasks produced nothing" and re-dispatch them. It is told the
// work is running, that its turn is free, and how the answers will
// arrive.
func renderSubagentDispatch(parent string, tasks []SubagentTaskInput, ids []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dispatched %d background task%s. They are RUNNING NOW — this is a receipt, not a result, and your turn is not blocked.\n\n",
		len(tasks), pluralS(len(tasks)))
	for i, t := range tasks {
		fmt.Fprintf(&b, "  %s — %s\n", ids[i], t.Description)
		fmt.Fprintf(&b, "      transcript: /agents/%s/subagents/%s · artifacts: /files/subagents/%s/artifacts/private/\n",
			parent, ids[i], ids[i])
	}
	b.WriteString("\nEach task's answer arrives as a separate message when it finishes, so do NOT re-dispatch these ")
	b.WriteString("and do not wait for them in this turn. Finish what you can without them and end your turn; ")
	b.WriteString("you will be woken when results land.\n")
	b.WriteString("Use subagent_status to see what is still running, subagent_cancel to stop one.\n")
	return b.String()
}

// renderSubagentResultMessage is the body of the message delivered to
// the parent when a job finishes. It reads as an inbound report
// because that is what it now is: there is no pending tool call for it
// to satisfy, the parent's dispatch call returned long ago.
//
// It closes with the JOB's standing, not just this task's, because
// this message is the only moment the agent is reliably awake and
// thinking about this work. Results arrive minutes apart with
// unrelated conversation in between, so an agent told only "task a1b2
// finished" has to reconstruct which larger job that belonged to and
// what it implies — and the reliable failure is that it reads the
// answer, replies to the CEO about it, and never touches the plan
// again. Stating the outstanding count and the plan's own progress
// turns each arrival into the bookkeeping prompt it should be.
//
// outstanding counts the agent's OTHER unfinished jobs; plan is
// whatever is on disk at delivery time.
//
// A structured task's body is its result JSON, fenced, in place of the
// run's text: the only words of the subagent's in it are the values
// the caller's schema made room for. A failure's reason is inside that
// JSON (outcome failed, error), so no separate error line repeats it.
func renderSubagentResultMessage(id, description, state, finalText, result string, runErr error, outstanding int, plan PlanView) string {
	var b strings.Builder
	switch state {
	case subagentJobCompleted:
		fmt.Fprintf(&b, "Background task %s (%s) finished.\n\n", id, description)
	case subagentJobCancelled:
		fmt.Fprintf(&b, "Background task %s (%s) was cancelled before it finished.\n\n", id, description)
	default:
		fmt.Fprintf(&b, "Background task %s (%s) FAILED.\n\n", id, description)
	}
	switch {
	case result != "":
		b.WriteString(resultOrText(result, ""))
		b.WriteString("\n\n")
	default:
		if runErr != nil {
			fmt.Fprintf(&b, "error: %v\n\n", runErr)
		}
		if strings.TrimSpace(finalText) != "" {
			b.WriteString(strings.TrimRight(finalText, "\n"))
			b.WriteString("\n\n")
		}
	}
	fmt.Fprintf(&b, "Anything it wrote is at /files/subagents/%s/artifacts/private/ · full transcript: /agents/<you>/subagents/%s\n", id, id)

	b.WriteString(renderJobStanding(outstanding, plan))
	return b.String()
}

// renderJobStanding is the "where does this leave the job" footer.
//
// Split out because it is the part worth getting right: it has to name
// the next decision without pretending to make it. The four moves
// listed are the real ones, and the last two are the ones an agent
// reliably forgets — cancelling work a result just invalidated, and
// asking the CEO something the result raised. Both are far cheaper now
// than after the next wave has run.
func renderJobStanding(outstanding int, plan PlanView) string {
	var b strings.Builder
	b.WriteString("\n")

	switch {
	case outstanding == 1:
		b.WriteString("1 of your other background tasks is still running.\n")
	case outstanding > 1:
		fmt.Fprintf(&b, "%d of your other background tasks are still running.\n", outstanding)
	default:
		b.WriteString("Nothing else of yours is running — this was the last one.\n")
	}

	if !plan.Present {
		if outstanding == 0 {
			b.WriteString("If this was part of a larger job, assemble the result now rather than reporting the pieces one at a time.\n")
		}
		return b.String()
	}

	fmt.Fprintf(&b, "Your plan (%s/%s/%s) stands at %d/%d done.\n",
		files.ModelRootPath, files.SharedWorkspaceDir, files.PlanFileName, plan.Done, plan.Total)
	b.WriteString("Update it now, before anything else: tick what this result completes, and note anything it changed.\n")
	b.WriteString("Then pick one — dispatch the tasks that are now ready, cancel work this result just made pointless, ")
	b.WriteString("ask the owner a question it raised, or, if nothing is left, assemble the deliverable.\n")
	if outstanding == 0 && plan.Total > 0 && plan.Done >= plan.Total {
		b.WriteString("Every task is ticked and nothing is running: assemble and deliver.\n")
	}
	return b.String()
}

// subagentFilesRoot is the on-disk root for one subagent's /files/
// overlay. Lives under the parent's /files/ tree
// (agents/<parent>/<storage>/subagents/<id>/) so the agent pod's
// /files/ mount exposes it at /files/subagents/<id>/ — same view both
// the parent's file_view and the subagent's file_view see.
func subagentFilesRoot(s *store.FSStore, parent, id string) string {
	return filepath.Join(files.StorageRoot(filepath.Join(s.Root(), "agents", parent)), "subagents", id)
}

// writeSubagentMeta persists the subagent's meta.json snapshot, beside
// its chat.jsonl: the task's facts and its status. Best-effort.
//
// The path is subagentMetaPath, shared with both readers (the
// transcript's task rows and the background task transcript), so every
// job writes its own subagents/<id>/meta.json where they look. The
// test that follows model and effort through to the file pins it.
//
// Every write to the file goes through store.UpdateSubagentMeta, so
// this one and the patches from the HTTP and registry goroutines
// (patchSubagentMeta) cannot lose each other's fields: what others
// keep there (browser, site, a result) stays; wait_reason goes once
// the status is no longer waiting.
//
// result is a structured task's answer once it has one (the wrapper
// JSON); the task rows show it in place of the run's text.
func (s *SubagentService) writeSubagentMeta(parent string, t SubagentTaskInput, id, status, errMsg, result string) {
	err := s.Store.UpdateSubagentMeta(parent, id, func(meta map[string]any) bool {
		meta["id"] = id
		meta["description"] = t.Description
		meta["prompt"] = t.Prompt
		meta["model"] = t.Model
		meta["effort"] = t.Effort
		meta["status"] = status
		meta["error"] = errMsg
		meta["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		if t.hasSchema() {
			meta["structured"] = true
		}
		if t.web() {
			meta["kind"] = agentpod.SubagentKindWeb
			meta["session"] = taskSession(t)
		}
		if result != "" && json.Valid([]byte(result)) {
			meta["result"] = json.RawMessage(result)
		}
		if status != subagentJobWaiting {
			delete(meta, "wait_reason")
		}
		return true
	})
	if err != nil {
		log.Printf("subagent %s/%s: write meta.json: %v", parent, id, err)
	}
}

// browserStateChanged is the registry's report of a browser's state
// for (slug, session). Every live web task on that session, one waiting
// for a person included, gets it in its meta.json, which the task's
// stream watches, so the task's view learns it at once and can show
// Open browser accordingly.
func (s *SubagentService) browserStateChanged(slug, session string, state apitypes.BrowserState) {
	for _, j := range s.listJobs(slug) {
		if j.Kind != agentpod.SubagentKindWeb || j.Session != session || j.terminal() {
			continue
		}
		s.setSubagentMetaBrowser(slug, j.ID, state)
	}
}

// setSubagentMetaBrowser writes a task's browser state into its
// meta.json, leaving the rest of the file as it is. A task with no
// meta.json yet, or one already saying so, is left alone.
func (s *SubagentService) setSubagentMetaBrowser(parent, id string, state apitypes.BrowserState) {
	_ = s.Store.UpdateSubagentMeta(parent, id, func(meta map[string]any) bool {
		if len(meta) == 0 || meta["browser"] == string(state) {
			return false
		}
		meta["browser"] = string(state)
		return true
	})
}

// newSubagentID returns a short hex id for a new subagent.
func newSubagentID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// emitToHub is the Server-side implementation of EmitToParent. Looks
// up the parent's existing chatHub and forwards the event. No-op if
// no hub exists for the slug (parent isn't actively running a chat
// loop, which shouldn't happen during subagent execution but is
// graceful if it does).
func (srv *Server) emitToHub(parent, kind string, payload any) {
	srv.streamMu.Lock()
	hub, ok := srv.chatHubs[parent]
	srv.streamMu.Unlock()
	if !ok || hub == nil {
		return
	}
	hub.Emit(kind, payload)
}
