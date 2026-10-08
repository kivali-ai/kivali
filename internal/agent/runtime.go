package agent

import (
	"context"
	"sync"

	"github.com/kivali-ai/kivali/internal/store"
)

// Provisioner is the narrow interface the runtime needs for per-agent
// pod lifecycle. Kept here so internal/agent stays free of k8s
// concerns in its dependency graph. main.go plugs in the real
// *agentpod.Provisioner at startup.
type Provisioner interface {
	Provision(ctx context.Context, slug string) error
}

// ShellExecutor runs commands in an agent's pod. Real implementation
// is agentpod.LocalShell, wired inside the agent pod's MCP
// subprocess; tests inject a fake. Used by the in-pod MCP run_shell
// tool and by FixedSlugExec for subagent routing.
//
// SyncSkills is the hook for making the current org-wide skill set
// available to run_shell before Exec. Both implementations are no-ops:
// skills reach the shell through the per-agent /files/skills/ symlink
// farm. Returning a non-nil error does not block Exec — a stale skill
// set is not catastrophic.
type ShellExecutor interface {
	Exec(ctx context.Context, slug string, req ShellRequest) (*ShellResult, error)
	SyncSkills(ctx context.Context, slug string) error
}

// ShellResult is the runtime-facing shape of a run_shell execution.
// Used by the agent pod's LocalShell + the in-pod MCP run_shell tool.
type ShellResult struct {
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMs int64
	TimedOut   bool
	Err        string
}

// ShellRequest is the runtime-facing payload for a single shell call.
// Cwd is relative to the executor's root (the agent pod's /files/
// mount in production). There are no input or capture file lists:
// with the unified /files/ namespace bash reads/writes the same
// physical paths file_view sees. See
// docs/developers/files-and-publishing.md.
type ShellRequest struct {
	Command        string
	Cwd            string
	TimeoutSeconds int
}

// Runtime is the per-process holder of everything Kivali web needs to
// reason about per-agent execution: the persistence layer, the
// agent-pod provisioner, plus the running-set tracker that the
// sidebar uses to render "active agents". The actual chat/tool loop
// runs inside agent pods; this struct is a thin coordinator.
//
// One Runtime per server process. Web wires it up in NewServer.
//
// Constructed via NewRuntime so callers can't forget to initialize
// the running map or its mutex.
type Runtime struct {
	Store *store.FSStore
	// AgentPod is the per-agent Pod + PVC provisioner. ApplyHire
	// fires Provision so a fresh hire gets its agent pod in the
	// same flow that creates its on-disk record.
	AgentPod Provisioner
	Defaults RuntimeDefaults

	// runningMu guards the running map + serves as the lock the
	// Running/RunningAgents probes acquire. Separate from any
	// outer per-release mutex (which serializes ReleaseAll).
	runningMu sync.Mutex
	running   map[string]bool

	// onRunningChange is fired (outside the lock) every time an
	// agent enters or leaves the running set. The web layer wires
	// this to Server.NotifyOrgState so the sidebar lights up the
	// instant Release-all kicks off and goes dark the instant the
	// last agent finishes — without waiting on a polling tick.
	onRunningChange func()
}

// RuntimeDefaults are the per-call tunables an agent run picks up
// from the surrounding configuration. Sized to the model's window;
// callers should set sensible values at construction.
type RuntimeDefaults struct {
	// AgentModel is the default model identifier (e.g. "claude-opus-5")
	// used when an Agent's per-agent Model field is empty.
	AgentModel string

	// SummaryModel is the cheap-model identifier used when an
	// agent-to-CEO message needs a short summary line.
	SummaryModel string

	// MaxTokens caps a single Claude response. Sized near the model
	// ceiling so agents can one-shot large artifacts (long status
	// updates, full role drafts) without chunking.
	MaxTokens int

	// MaxTurnTokens caps cumulative input+output tokens across one
	// agent's tool-loop in a single release run. Zero disables the cap.
	MaxTurnTokens int

	// Temperature is the per-call sampling temperature passed to
	// Claude. Zero is fine — the API treats it as the default.
	Temperature float64
}

// NewRuntime returns a ready-to-use Runtime with the running map
// initialized.
func NewRuntime(s *store.FSStore, defaults RuntimeDefaults) *Runtime {
	return &Runtime{
		Store:    s,
		Defaults: defaults,
		running:  map[string]bool{},
	}
}

// SetOnRunningChange installs the callback fired every time the
// per-agent running set transitions (enter or leave). Safe to call
// at startup before any concurrent runs; not safe to mutate live.
// The callback runs OUTSIDE runningMu so a slow notify can't stall
// the next markRunning caller.
func (r *Runtime) SetOnRunningChange(fn func()) {
	r.onRunningChange = fn
}

// Running reports whether ANY agent is currently inside a tool loop.
// Cheap probe — returns immediately whether or not callers are
// running concurrently.
func (r *Runtime) Running() bool {
	r.runningMu.Lock()
	defer r.runningMu.Unlock()
	return len(r.running) > 0
}

// RunningAgents returns the slugs currently inside a tool loop
// (either via release path or chat path). Snapshot under runningMu,
// so callers see a consistent set even when transitions are firing.
// Empty when no agent is running.
func (r *Runtime) RunningAgents() []string {
	r.runningMu.Lock()
	defer r.runningMu.Unlock()
	out := make([]string, 0, len(r.running))
	for s := range r.running {
		out = append(out, s)
	}
	return out
}

// IsRunning reports whether the named slug is currently inside a
// tool loop. Used by cross-path coordinators (runtime vs.
// chat path) to avoid double-spawning Claude on the same chat
// history.
func (r *Runtime) IsRunning(slug string) bool {
	r.runningMu.Lock()
	defer r.runningMu.Unlock()
	return r.running[slug]
}

// MarkRunning sets/clears the running flag for slug and fires the
// onRunningChange callback (outside the lock). Called by the per-
// agent execution paths at the start and end of each run.
func (r *Runtime) MarkRunning(slug string, running bool) {
	r.runningMu.Lock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	if running {
		r.running[slug] = true
	} else {
		delete(r.running, slug)
	}
	cb := r.onRunningChange
	r.runningMu.Unlock()
	if cb != nil {
		cb()
	}
}
