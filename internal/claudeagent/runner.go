package claudeagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
)

const (
	// closeGraceTimeout is how long we wait for a runner's CLI
	// subprocess to exit cleanly after stdin is closed before
	// SIGKILLing. Tests advance the runner's clock past this to
	// fire the timeout deterministically — no real-time wait.
	closeGraceTimeout = 5 * time.Second

	// writeTimeout bounds how long stdin writes can block. A
	// healthy CLI consumes a single user release in microseconds.
	// Anything longer means the CLI is stuck (deadlocked tool,
	// runaway loop, blocked on a slow downstream) — kill the
	// runner and let the next Acquire respawn fresh.
	writeTimeout = 30 * time.Second

	// fastDeathWindow is the threshold for "the runner died too
	// fast to be a real session." When a runner spawned with
	// --resume dies inside this window, we treat it as evidence
	// the on-disk session log is unreadable to the CLI (corrupt
	// JSONL, schema drift, etc.) and clear the stored session id
	// so the next Acquire spawns without --resume — breaking the
	// otherwise-infinite respawn loop.
	fastDeathWindow = 10 * time.Second

	// gracefulInterruptDeadline is how long the runner waits for the
	// CLI to deliver an in-flight tool_result after a Stop click
	// before falling back to immediate SIGTERM. Sized to cover one
	// MCP round trip back to the CLI plus a small slack — long
	// enough to keep the session log in a clean tool-pair state on
	// the common case (cancelling a subagent), short enough that no
	// in-flight tool means the user doesn't wait perceptibly.
	gracefulInterruptDeadline = 2 * time.Second

	// forceKillGrace is how long a SIGTERM has to take effect before
	// we escalate to SIGKILL.
	forceKillGrace = 1 * time.Second

	// interruptAckDeadline is how long sendInterrupt waits for the
	// CLI to actually end the turn after we write the interrupt
	// control_request, before escalating to gracefulKill. Measured
	// round-trip on CLI 2.1.273 is ~10ms (ack) with the terminal
	// result event immediately behind it, so this is two orders of
	// magnitude of headroom — it exists to bound a wedged control
	// channel, not to pace the happy path.
	interruptAckDeadline = 1 * time.Second

	// foldAbsorbDeadline bounds how long a turn is held open waiting
	// for the continuation the CLI owes it after a fold missed (see
	// absorbMissedFolds). Measured against CLI 2.1.273 the
	// continuation's "started" frame follows the missed turn's result
	// inside the same tenth of a second, so this is two orders of
	// magnitude of headroom. It
	// bounds a CLI that silently drops a queued message, nothing
	// else — expiry falls back to reporting the miss.
	foldAbsorbDeadline = 10 * time.Second
)

// runner owns one long-lived `claude -p --input-format stream-json`
// subprocess for one agent slug. The subprocess is born with --resume
// when a session id is on disk; stdin stays open across turns; each
// turn writes one {"type":"user", ...} line, and the dispatcher routes
// resulting stream-json events back to that turn's events channel.
//
// Concurrency: only one turn can be active at a time per runner. The
// registry serializes Acquire+RunTurn per slug; RunTurn also defends
// in depth, returning an error if a turn is somehow already in flight.
//
// Lifecycle in one picture:
//
//	newRunner ─► dispatcher (stdout reader) ─► routeEvent ─► active turn
//	                supervisor (cmd.Wait)   ─► dead chan, fail active
//	  RunTurn ─► writes one user release with timeout, returns Stream
//	    Close ─► close stdin, wait dead ≤ grace, else SIGKILL; rm dir
//
// Per-runner working directory: each runner gets its own subdirectory
// under the registry session dir, holding the mcp-config file, the
// stderr capture, and the stdin-mirror log. RemoveAll'd on Close. This
// isolates runners from each other (no cross-runner filename collisions
// or stderr overwrites) and gives self-contained post-mortem
// diagnostics — the dir for a crashed runner survives until something
// removes it.
//
// Note on per-turn args:
//
//   - --model, --effort, --system-prompt are spawn-time args baked from
//     the FIRST request that hits a fresh slug. The stream-json input
//     protocol has no per-turn override, so a change means tear-down +
//     respawn — which Acquire does transparently: configMatches detects
//     drift in any of the three and respawns with --resume, preserving
//     the conversation. So edits to model/effort or to the system prompt
//     (handbook / role / agent_memory) land on the next turn.
//   - --mcp-config and --allowedTools are spawn-time too and are not
//     drift-checked: they change only with the agent's role (CoS vs
//     not) or a code change, and are silently inherited until the
//     runner respawns for another reason.
type runner struct {
	slug string
	opts Options
	clk  clock.Clock

	cmd         *exec.Cmd
	stdin       io.WriteCloser // the actual cmd.StdinPipe — closed on shutdown
	stdout      io.ReadCloser
	stderrFile  *os.File  // best-effort; may be nil if open failed
	stdinMirror *os.File  // best-effort; may be nil
	stdinW      io.Writer // = MultiWriter(stdin, stdinMirror) when mirror is open

	// stdinMu serializes whole-line writes to stdinW. Two writers now
	// share the pipe — the per-turn user message (RunTurn) and the
	// control_request (sendInterrupt) — and the CLI parses stdin one
	// JSON line at a time, so an interleaved write would corrupt both
	// messages. Never held across anything that can block on the
	// subprocess.
	stdinMu sync.Mutex

	// dir is the per-runner working directory under the registry's
	// session dir. RemoveAll'd on Close.
	dir string

	// spawnedAt is when newRunner returned (per the runner's clock).
	// The supervisor compares against fastDeathWindow to detect
	// "died too quickly" — used to decide whether to clear a stored
	// session id that the CLI couldn't replay.
	spawnedAt time.Time

	mu     sync.Mutex
	active *runnerTurn

	// dead closes when the subprocess has exited; deadErr is set
	// before close. Read with mu held or via isDead().
	dead    chan struct{}
	deadErr error

	// resumeID is the --resume id we passed at spawn (if any).
	// sessionSaved tracks whether we've written the CLI-assigned id
	// back to the SessionStore yet (one write per runner lifetime).
	resumeID     string
	sessionSaved bool

	// usageGauge recovers each turn's per-model token share from the
	// result frame's modelUsage, which the CLI reports as a gauge
	// cumulative over the session — across turns, and across processes
	// when the session was resumed. It lives on the runner rather than
	// the turn because consecutive readings on one process are the only
	// thing the delta can be taken between. Touched only by the
	// dispatcher goroutine, after construction. See usageGauge.
	usageGauge usageGauge

	// spawnModel / spawnEffort are the --model and --effort this runner
	// was born with. The CLI pins both for the subprocess's lifetime, so
	// when a later turn arrives wanting different values (the CEO changed
	// the per-chat Model/Effort selector), Acquire detects the drift and
	// respawns — see runner.configMatches and runnerRegistry.Acquire.
	spawnModel  string
	spawnEffort string

	// spawnSystemHash is the sha256 of the flattened --system-prompt
	// this runner was born with. The system prompt carries the
	// handbook, the agent's role.md, and its agent_memory.md
	// (assembled core-side, rebuilt fresh every turn). The CLI pins the
	// system prompt for the subprocess's lifetime, so when an edit to
	// any of those lands, Acquire detects the drift via configMatches
	// and respawns with --resume — the edit takes effect on the next
	// turn instead of waiting for a chat rotation. Hashed rather than
	// stored verbatim: the prompt is multi-KB and we only ever compare.
	spawnSystemHash string

	// spawnSignIn is the sign-in key (signInWatch) the registry read
	// before spawning this runner; Acquire respawns it when the key
	// moves.
	spawnSignIn string

	// closeOnce guards Close() so concurrent eviction + explicit
	// shutdown don't double-kill.
	closeOnce sync.Once

	// killOnce guards gracefulKill so a tool_result-driven kill and
	// a deadline-driven kill don't race.
	killOnce sync.Once

	// interruptArmed flips true on a per-turn ctx cancel (Stop click
	// reached the runtime). It is the "we want this turn to end"
	// intent; sendInterrupt is what acts on it. Reset by
	// resetInterruptStateLocked once the turn terminates, because the
	// subprocess survives a gentle interrupt and goes on to serve
	// later turns.
	// Guarded by mu.
	interruptArmed bool

	// interruptSent guards against writing the control_request more
	// than once for one armed turn (the boundary watcher and the
	// no-tool-in-flight path can both reach sendInterrupt).
	// Guarded by mu.
	interruptSent bool

	// toolsInFlight counts tool_use blocks the CLI has announced
	// minus the tool_results it has returned. Non-zero means a tool
	// (run_shell, a subagent batch, WebFetch) is mid-execution.
	//
	// This is what makes "don't interrupt a command in flight" real:
	// armInterrupt only writes the control_request when this is 0,
	// otherwise it defers to the tool_result boundary in routeEvent.
	// Guarded by mu.
	toolsInFlight int

	// controlSeq numbers outbound control_request ids so a
	// control_response can be correlated back. Guarded by mu.
	controlSeq int

	// capabilities is the protocol feature set this CLI build
	// advertised on its system/init frame. The CLI ships it so
	// consumers feature-detect instead of version-sniffing, and
	// sendFold refuses rather than guessing when the fold's
	// prerequisite (capabilityLifecycle) is absent.
	// Guarded by mu.
	capabilities map[string]bool

	// pendingFolds holds the uuids of messages sendFold wrote into
	// the CURRENT turn whose fate is not yet known — no
	// command_lifecycle "started" seen, and no result event to close
	// them out against its ledger.
	//
	// There is deliberately no timer here. "started" arriving before
	// the turn's result means the running turn took the message; the
	// result event arriving first means it did not, and the uuid
	// moves to `absorbing`. Both outcomes are observable, so a
	// deadline would only ever be measuring how long the current tool
	// round takes — which is unbounded and none of our business.
	// Guarded by mu.
	pendingFolds map[string]struct{}

	// absorbing holds the uuids of folds that MISSED the turn whose
	// result just arrived, and that the CLI is therefore about to run
	// as a turn of its own. While this is non-empty the runner has
	// deliberately NOT finalized the active turn: the continuation's
	// frames belong to it. See absorbMissedFolds.
	// Guarded by mu.
	absorbing map[string]struct{}

	// absorbTimer bounds the wait for the continuation's "started".
	// Cleared (and stopped) as soon as absorbing empties.
	// Guarded by mu.
	absorbTimer clock.Timer

	// userCancelled is set by gracefulKill before SIGTERM so the
	// supervisor can wrap deadErr with provider.ErrUserCancelled
	// rather than ErrSubprocessExited — distinguishing user-cancel
	// from a genuine subprocess crash on the chat-loop's failure
	// classification.
	// Guarded by mu.
	userCancelled bool
}

// newRunner spawns the persistent CLI subprocess for slug. resumeID
// (when non-empty) is passed as --resume so the CLI rebuilds its
// in-memory session from $HOME/.claude/projects/.../<id>.jsonl on
// startup. The model / system prompt / tool surface are taken from
// req — the FIRST request to hit this slug — and locked for the
// runner's lifetime.
//
// Returns immediately on subprocess exec failure; otherwise the
// subprocess is alive when this returns and the dispatcher / supervisor
// goroutines are running.
func newRunner(opts Options, slug, kivaliBin, sessionDir, resumeID string, req provider.CompleteRequest) (*runner, error) {
	if opts.ClaudeBinary == "" {
		opts.ClaudeBinary = "claude"
	}
	clk := opts.clock
	if clk == nil {
		clk = clock.New()
	}
	binary, err := exec.LookPath(opts.ClaudeBinary)
	if err != nil {
		return nil, fmt.Errorf("claudeagent: %q not found on PATH (install Claude Code and sign it in by running `claude`): %w", opts.ClaudeBinary, err)
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, fmt.Errorf("claudeagent: session dir: %w", err)
	}

	// Per-runner subdir avoids cross-runner filename collisions on
	// rapid spawn/respawn and lets us drop everything via os.RemoveAll
	// on Close.
	suffix, err := randHex(4)
	if err != nil {
		return nil, fmt.Errorf("claudeagent: random suffix: %w", err)
	}
	dir := filepath.Join(sessionDir, fmt.Sprintf("%s-%s", safeSlug(slug), suffix))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("claudeagent: runner dir: %w", err)
	}

	// Cleanup-on-error: if anything below fails, remove the dir we
	// just created so partial spawns don't leak files. cleanupDir is
	// flipped to false right before we return success.
	cleanupDir := true
	defer func() {
		if cleanupDir {
			_ = os.RemoveAll(dir)
		}
	}()

	mcpConfigPath := filepath.Join(dir, "mcp-config.json")
	if err := writeMCPConfig(mcpConfigPath, kivaliBin, opts.DataDir, opts.CoreUDS, slug); err != nil {
		return nil, err
	}

	args := persistentRunnerArgs(mcpConfigPath, resumeID, req)

	// Background ctx: runner.Close() drives subprocess lifetime, not
	// any single request's ctx. A request ctx cancellation must not
	// kill a runner that other turns will reuse.
	cmd := exec.Command(binary, args...)
	// Inherit the environment unchanged so the CLI sees the same
	// credential the server runs with (see Stream).
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=--max-old-space-size=4096")

	stderrFile, _ := os.OpenFile(filepath.Join(dir, "stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if stderrFile != nil {
		cmd.Stderr = io.MultiWriter(os.Stderr, stderrFile)
	} else {
		cmd.Stderr = os.Stderr
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		if stderrFile != nil {
			_ = stderrFile.Close()
		}
		return nil, fmt.Errorf("claudeagent: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if stderrFile != nil {
			_ = stderrFile.Close()
		}
		_ = stdin.Close()
		return nil, fmt.Errorf("claudeagent: stdout pipe: %w", err)
	}

	stdinMirror, _ := os.OpenFile(filepath.Join(dir, "stdin-mirror.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	var stdinW io.Writer = stdin
	if stdinMirror != nil {
		stdinW = io.MultiWriter(stdin, stdinMirror)
	}

	if err := cmd.Start(); err != nil {
		if stderrFile != nil {
			_ = stderrFile.Close()
		}
		if stdinMirror != nil {
			_ = stdinMirror.Close()
		}
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, errors.Join(errStubNoSubprocess, err)
	}

	// We own the dir from here on; cancel the deferred cleanup.
	cleanupDir = false

	r := &runner{
		slug:        slug,
		opts:        opts,
		clk:         clk,
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderrFile:  stderrFile,
		stdinMirror: stdinMirror,
		stdinW:      stdinW,
		dir:         dir,
		spawnedAt:   clk.Now(),
		dead:        make(chan struct{}),
		resumeID:    resumeID,
		spawnModel:  req.Model,
		spawnEffort: normalizeEffort(req.Effort),

		spawnSystemHash: systemPromptHash(req),

		// A fresh session starts the CLI's gauge at zero, so the first
		// frame's reading is already a delta. A resumed one reloads
		// the session's history into it, and the baseline is unknown
		// until this process has read the gauge once.
		usageGauge: newUsageGauge(resumeID == ""),
	}

	go r.dispatcher()
	go r.supervisor()

	return r, nil
}

// persistentRunnerArgs builds the CLI args for a long-lived runner.
// The per-call path (subprocStream.Stream) builds its own arg list
// inline; we keep them separate because some flags differ subtly
// (e.g. per-call passes --resume on every spawn; persistent only at
// runner birth).
func persistentRunnerArgs(mcpConfigPath, resumeID string, req provider.CompleteRequest) []string {
	args := []string{
		"-p",
		"--tools", agentBuiltinTools,
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--mcp-config", mcpConfigPath,
		// Use ONLY the config we just wrote. Without this the CLI also
		// merges whatever MCP servers it finds in user/project scope.
		// Nothing should be there inside an agent pod, but an agent's
		// tool surface must be a function of what core decided, not of
		// what happens to be on the filesystem — and it is the roster
		// hash's job to know when that surface changed.
		"--strict-mcp-config",
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if req.Model != "" {
		args = append(args, "--model", stripName(req.Model))
	}
	// Reasoning depth — always explicit + validated (see stream.go). The
	// persistent runner is spawned once at runner birth, so this pins the
	// effort for the runner's whole lifetime from the first turn's req.
	args = append(args, "--effort", normalizeEffort(req.Effort))
	if sys := flattenSystem(req.System); sys != "" {
		args = append(args, "--system-prompt", sys)
	}
	if allowed := strings.Join(allowedToolNames(req.Tools), ","); allowed != "" {
		args = append(args, "--allowedTools", allowed)
	}
	return args
}

// RunTurn writes one user release to stdin and returns a Stream that
// emits events for this turn until the CLI emits "result". Subsequent
// calls reuse the same subprocess; Stream.Close() is a no-op on the
// subprocess in persistent mode.
//
// turnCtx drives per-turn cancellation. When it fires (Stop click
// reaches the runtime), the runner arms an interrupt: the next
// tool_result event triggers a graceful subprocess kill so the CLI's
// session log ends in a clean tool-pair state (cache-warm --resume on
// the next turn). A 2s deadline catches the no-tool-in-flight case.
//
// The stdin write is bounded by writeTimeout. If it doesn't complete
// in that window — typically because the CLI is stuck — we kill the
// runner and return an error. The next Acquire will respawn fresh.
func (r *runner) RunTurn(turnCtx context.Context, req provider.CompleteRequest) (provider.Stream, error) {
	r.mu.Lock()
	if r.isDeadLocked() {
		err := r.deadErr
		r.mu.Unlock()
		return nil, fmt.Errorf("claudeagent: runner[%s] is dead: %w", r.slug, err)
	}
	if r.active != nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("claudeagent: runner[%s] already has an in-flight turn (caller must serialize)", r.slug)
	}
	t := newRunnerTurn(req, r.opts.Recorder, r.opts.Sessions, r.slug, r.resumeID, r.clk)
	t.owner = r
	r.active = t
	r.mu.Unlock()

	// Stamp this release with a uuid so the CLI reports lifecycle for
	// it. Best-effort: if crypto/rand fails we send unstamped — the
	// turn still runs, we just get no command_lifecycle frames for it.
	turnID, idErr := randHex(16)
	if idErr != nil {
		log.Printf("claudeagent: runner[%s] could not mint a turn uuid: %v; sending unstamped", r.slug, idErr)
		turnID = ""
	}

	// Watch for per-turn cancellation. Spawned BEFORE the stdin
	// write so a cancel that lands during writeTimeout still arms
	// the interrupt. Exits cleanly on natural turn finalization
	// (including the failActive path on stdin-write timeout, which
	// also calls finalize via active.fail).
	if turnCtx != nil {
		go r.watchTurnInterrupt(turnCtx, t)
	}

	// Run the write on a goroutine so we can time it out without
	// touching the underlying syscall. If the timeout fires, Close()
	// closes stdin which unblocks the goroutine (it sees EPIPE and
	// returns), so the goroutine doesn't leak.
	doneWrite := make(chan error, 1)
	go func() {
		// Under stdinMu: sendInterrupt shares this pipe and the CLI
		// reads it one JSON line at a time.
		r.stdinMu.Lock()
		defer r.stdinMu.Unlock()
		doneWrite <- writeUserMessageLine(r.stdinW, req, turnID)
	}()
	timer := r.clk.NewTimer(writeTimeout)
	defer timer.Stop()
	select {
	case err := <-doneWrite:
		if err != nil {
			r.failActive(fmt.Errorf("claudeagent: write user release: %w", err))
			return nil, err
		}
	case <-timer.C():
		log.Printf("claudeagent: runner[%s] stdin write timed out after %s; closing runner", r.slug, writeTimeout)
		// Close async — Close blocks on cmd.Wait which can take up
		// to closeGraceTimeout. We don't want to hold the caller
		// for that long when we already know the runner is hosed.
		go func() { _ = r.Close() }()
		err := fmt.Errorf("claudeagent: stdin write timed out after %s (runner closed)", writeTimeout)
		r.failActive(err)
		return nil, err
	}
	return t, nil
}

// watchTurnInterrupt fires the per-turn interrupt sequence when
// turnCtx is cancelled mid-turn. Exits without effect on natural
// finalization (CLI emitted "result" → finalize() → t.finalized).
//
// turnCtx cancel arrives via two real paths in production:
//   - handleChatTurn's defer's cancel() — but that fires AFTER the
//     for-range over stream.Events() exits, which only happens after
//     finalize closes the events chan, so t.finalized fires first
//     and the watcher exits cleanly. (Defensive: even if the order
//     reversed, ctx.Err()==context.Canceled would arm interrupt on
//     a turn that's already terminal — harmless because the runner
//     is about to die anyway.)
//   - The agent runtime's handleCancelTurn — fires when EventCancelTurn
//     arrives over the events SSE. This is the Stop-click path.
func (r *runner) watchTurnInterrupt(turnCtx context.Context, t *runnerTurn) {
	select {
	case <-turnCtx.Done():
		// Don't arm if the turn already finalized — race-defensive
		// against a defer-cancel that lands fractionally after the
		// natural exit.
		select {
		case <-t.finalized:
			return
		default:
		}
		r.armInterrupt()
	case <-t.finalized:
		// Natural completion or supervisor-driven failure; nothing
		// to do.
	}
}

// armInterrupt records the Stop-click intent on the runner and starts
// the interrupt sequence. Idempotent.
//
// The sequence after arming, in preference order:
//
//  1. No tool in flight → write the control_request immediately.
//     There is nothing running to disturb, so waiting buys nothing.
//  2. A tool IS in flight → do nothing yet. routeEvent's tool_result
//     branch calls sendInterrupt at the boundary, once the CLI has
//     written the tool_result to its session log. This is the
//     "don't interrupt a command in flight" rule: a run_shell or a
//     subagent batch gets to finish and land its result.
//  3. Neither fired within gracefulInterruptDeadline (a stuck or
//     very long tool) → gracefulKill, the hard path.
//     Stop must stay responsive even against a wedged tool, and a
//     kill is the only thing that reaches one.
//
// sendInterrupt owns its own escalation: if the CLI doesn't end the
// turn shortly after the control_request, it falls back to
// gracefulKill too. So every path terminates.
func (r *runner) armInterrupt() {
	r.mu.Lock()
	if r.interruptArmed {
		r.mu.Unlock()
		return
	}
	r.interruptArmed = true
	idle := r.toolsInFlight == 0
	absorbing := len(r.absorbing) > 0
	active := r.active
	r.mu.Unlock()

	if absorbing {
		// The turn is open only because a folded message missed it
		// and the CLI owes a continuation it has not started. Nothing
		// is running to interrupt: the control_request's cancel_queued
		// takes the message back, and no result frame will ever end
		// this turn — so end it here, the way the absorb deadline
		// would, and report the folds missed so core re-delivers what
		// the CLI handed back. The subprocess stays warm. Left to the
		// generic path, the escalation timer would find no result and
		// kill the process for a Stop that had nothing to stop.
		log.Printf("claudeagent: runner[%s] interrupt armed while holding the turn open for a missed fold; ending the turn", r.slug)
		r.sendInterrupt("armed while awaiting a fold's continuation")
		r.mu.Lock()
		stranded := r.strandAbsorbLocked(active)
		r.mu.Unlock()
		r.reportFoldsMissed(active, stranded)
		if active != nil {
			active.fail(fmt.Errorf("%w: interrupted while awaiting a continuation", provider.ErrUserCancelled))
		}
		return
	}

	if idle {
		log.Printf("claudeagent: runner[%s] interrupt armed; no tool in flight, sending control_request now", r.slug)
		r.sendInterrupt("armed with no tool in flight")
	} else {
		log.Printf("claudeagent: runner[%s] interrupt armed; deferring to next tool_result boundary (hard-kill fallback in %s)", r.slug, gracefulInterruptDeadline)
	}

	timer := r.clk.NewTimer(gracefulInterruptDeadline)
	go func() {
		defer timer.Stop()
		select {
		case <-timer.C():
			// Only meaningful when the boundary never arrived. If
			// sendInterrupt already ran, its own escalation timer is
			// the one in charge and this is a redundant no-op —
			// gracefulKill is idempotent via killOnce.
			r.mu.Lock()
			sent := r.interruptSent
			r.mu.Unlock()
			if sent {
				return
			}
			r.gracefulKill("interrupt deadline reached without tool_result boundary")
		case <-r.dead:
			// Already dead via the kill path or natural exit.
		}
	}()
}

// sendInterrupt writes one control_request to the CLI's stdin asking
// it to end the in-flight turn, and arms the escalation that kills the
// subprocess if that doesn't take effect.
//
// Measured on CLI 2.1.273: the CLI acks in ~10ms with a
// control_response and terminates the turn with a `result` event of
// subtype "error_during_execution", leaving the subprocess alive and
// its conversation intact — so the next turn needs no --resume and no
// respawn. That is the whole point of preferring this over SIGTERM.
//
// Escalation is keyed on the turn actually finalizing rather than on
// the ack alone: an ack that is never followed by a result would
// otherwise hang Stop forever.
func (r *runner) sendInterrupt(reason string) {
	r.mu.Lock()
	if r.interruptSent {
		r.mu.Unlock()
		return
	}
	if r.isDeadLocked() {
		r.mu.Unlock()
		return
	}
	r.interruptSent = true
	r.controlSeq++
	reqID := fmt.Sprintf("int_%d", r.controlSeq)
	active := r.active
	r.mu.Unlock()

	// Ask the CLI to drop anything still queued alongside the abort,
	// when it can. Without this an interrupt that raced a fold leaves
	// the CLI holding a message we are also about to re-deliver
	// ourselves, and the agent sees it twice. With it, the receipt
	// tells us exactly which messages came back to us.
	request := map[string]any{"subtype": "interrupt"}
	if r.hasCapability(capabilityCancelQueued) {
		request["cancel_queued"] = true
	}

	log.Printf("claudeagent: runner[%s] sending interrupt control_request %s (%s)", r.slug, reqID, reason)
	if err := r.writeControlLine(map[string]any{
		"type":       "control_request",
		"request_id": reqID,
		"request":    request,
	}); err != nil {
		// Pipe is gone or wedged — the gentle path is unavailable.
		log.Printf("claudeagent: runner[%s] control_request write failed: %v; escalating", r.slug, err)
		r.gracefulKill("control_request write failed")
		return
	}

	timer := r.clk.NewTimer(interruptAckDeadline)
	go func() {
		defer timer.Stop()
		var finalized <-chan struct{}
		if active != nil {
			finalized = active.finalized
		}
		select {
		case <-finalized:
			// The CLI ended the turn. Subprocess stays warm.
		case <-r.dead:
		case <-timer.C():
			r.gracefulKill("interrupt control_request not honoured within " + interruptAckDeadline.String())
		}
	}()
}

// writeControlLine serializes one control-channel JSON line onto the
// CLI's stdin under stdinMu, so it cannot interleave with the
// per-turn user message write.
func (r *runner) writeControlLine(body map[string]any) error {
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()
	return json.NewEncoder(r.stdinW).Encode(body)
}

// resetInterruptStateLocked clears the per-turn interrupt bookkeeping
// so the next turn on this runner starts clean. Caller holds mu.
//
// The gentle path leaves the subprocess alive to serve another turn,
// and a stale interruptArmed would cancel that turn on arrival.
func (r *runner) resetInterruptStateLocked() {
	r.interruptArmed = false
	r.interruptSent = false
	r.toolsInFlight = 0
	// Folds are per-turn too: resolveFolds has already reported every
	// outstanding one by the time we get here, and a uuid left in the
	// map would otherwise be resolved a second time against the NEXT
	// turn's ledger.
	r.pendingFolds = nil
	// Same for an absorb still in progress: this turn is over, so
	// there is nothing left to hold open for.
	r.absorbing = nil
	if r.absorbTimer != nil {
		r.absorbTimer.Stop()
		r.absorbTimer = nil
	}
}

// Fold errors. All three mean "this delivery did not go onto the
// wire"; the caller treats every one of them the same way — fall back
// to ending the turn and letting the message run as its own.
var (
	errFoldUnsupported = errors.New("claudeagent: CLI does not advertise " + capabilityLifecycle)
	errFoldNoTurn      = errors.New("claudeagent: no in-flight turn to fold into")
	errFoldWinding     = errors.New("claudeagent: turn is already winding up")
)

// captureCapabilities records the protocol feature set the CLI
// advertised on its system/init frame. The CLI emits this array
// precisely so consumers feature-detect rather than version-sniff, so
// this is the ONLY thing sendFold consults — never the version string.
//
// A build that advertises nothing simply cannot fold, and callers fall
// back to the interrupt path they used before folds existed.
func (r *runner) captureCapabilities(ev *streamJSONEvent) {
	if ev.Subtype != "init" || len(ev.Capabilities) == 0 {
		return
	}
	caps := make(map[string]bool, len(ev.Capabilities))
	for _, c := range ev.Capabilities {
		caps[c] = true
	}
	r.mu.Lock()
	r.capabilities = caps
	r.mu.Unlock()
	log.Printf("claudeagent: runner[%s] CLI capabilities: %s", r.slug, strings.Join(ev.Capabilities, ","))
}

// hasCapability reports whether the CLI advertised name on init.
func (r *runner) hasCapability(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capabilities[name]
}

// sendFold splices text into the turn that is already running, by
// writing one uuid-stamped {"type":"user", ...} line to the CLI's
// stdin. The CLI queues it and folds it into the in-flight turn at the
// next tool round — in the same request that carries that round's tool
// results. No abort, no discarded output, no extra API call, and the
// prompt-cache prefix is untouched because the message is a pure
// append.
//
// The returned uuid is how the caller correlates the outcome, which
// arrives later as a StreamMessageFolded event. Landed means the turn
// consumed it — either at a tool seam, or in the continuation the CLI
// starts for it when the turn ended first, which the runner absorbs
// into the same turn (see absorbMissedFolds). Missed is the narrow
// case where no continuation came at all.
//
// Refuses (without writing) when the CLI can't report lifecycle, when
// t is not the live turn, or when the turn is already winding up under
// an interrupt — folding into a turn we are about to abort would race
// the abort and risk delivering the message twice.
//
// The capability check comes first deliberately: "this CLI cannot
// fold" is a stable fact a caller can act on once, whereas "no turn
// right now" depends on the instant it asked.
func (r *runner) sendFold(t *runnerTurn, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("claudeagent: fold text is empty")
	}
	r.mu.Lock()
	if r.isDeadLocked() {
		err := r.deadErr
		r.mu.Unlock()
		return "", fmt.Errorf("claudeagent: runner[%s] is dead: %w", r.slug, err)
	}
	if !r.capabilities[capabilityLifecycle] {
		r.mu.Unlock()
		return "", errFoldUnsupported
	}
	if t == nil || r.active != t {
		r.mu.Unlock()
		return "", errFoldNoTurn
	}
	if r.interruptArmed {
		r.mu.Unlock()
		return "", errFoldWinding
	}
	id, err := randHex(16)
	if err != nil {
		r.mu.Unlock()
		return "", fmt.Errorf("claudeagent: fold uuid: %w", err)
	}
	if r.pendingFolds == nil {
		r.pendingFolds = map[string]struct{}{}
	}
	r.pendingFolds[id] = struct{}{}
	r.mu.Unlock()

	if err := r.writeFoldLine(id, text); err != nil {
		r.mu.Lock()
		delete(r.pendingFolds, id)
		r.mu.Unlock()
		return "", err
	}
	log.Printf("claudeagent: runner[%s] folded message %s into the in-flight turn", r.slug, id)
	return id, nil
}

// writeFoldLine writes one uuid-stamped user message to stdin, bounded
// by the same writeTimeout the per-turn release write uses — this is
// the existing bound on "the CLI stopped reading stdin", not a
// fold-specific watchdog.
//
// On timeout we report the failure and leave the runner alone: a
// genuinely wedged pipe is the next turn's write to discover, and that
// path already owns the teardown. Killing a live turn over a delivery
// would trade a recoverable miss for a lost turn.
func (r *runner) writeFoldLine(id, text string) error {
	done := make(chan error, 1)
	go func() {
		r.stdinMu.Lock()
		defer r.stdinMu.Unlock()
		done <- json.NewEncoder(r.stdinW).Encode(map[string]any{
			"type": "user",
			"uuid": id,
			"message": map[string]any{
				"role":    "user",
				"content": text,
			},
			"parent_tool_use_id": nil,
		})
	}()
	timer := r.clk.NewTimer(writeTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("claudeagent: write fold: %w", err)
		}
		return nil
	case <-timer.C():
		log.Printf("claudeagent: runner[%s] fold write blocked for %s; abandoning this delivery", r.slug, writeTimeout)
		return fmt.Errorf("claudeagent: write fold: stdin blocked for %s", writeTimeout)
	}
}

// handleLifecycle acts on a command_lifecycle frame for one of OUR
// folds. Frames for anything else (the per-turn release, another
// client's prompt) are ignored.
//
// "started" means the command drained into a turn. We only register a
// fold while a turn is live, and resolveFolds closes out everything
// still outstanding when that turn's result arrives — so a "started"
// that reaches a still-pending uuid necessarily belongs to the running
// turn. That is the fold landing.
func (r *runner) handleLifecycle(ev *streamJSONEvent) {
	if ev.CommandUUID == "" {
		return
	}
	r.mu.Lock()
	_, pending := r.pendingFolds[ev.CommandUUID]
	_, absorbing := r.absorbing[ev.CommandUUID]
	active := r.active
	started := ev.State == lifecycleStarted
	if pending && started {
		delete(r.pendingFolds, ev.CommandUUID)
	}
	if absorbing && started {
		delete(r.absorbing, ev.CommandUUID)
		if len(r.absorbing) == 0 {
			r.absorbing = nil
			if r.absorbTimer != nil {
				r.absorbTimer.Stop()
				r.absorbTimer = nil
			}
		}
	}
	r.mu.Unlock()
	if absorbing && started {
		// The continuation we held the turn open for has begun, so
		// the message IS being answered — by what core sees as the
		// same turn. That is a landing.
		log.Printf("claudeagent: runner[%s] fold %s started as the turn's continuation; absorbing it", r.slug, ev.CommandUUID)
		if active != nil {
			active.emitFold(ev.CommandUUID, true)
		}
		return
	}
	if !pending {
		return
	}
	switch ev.State {
	case lifecycleQueued:
		// Accepted into the queue. Says nothing about WHICH turn will
		// consume it, so there is nothing to report yet.
		log.Printf("claudeagent: runner[%s] fold %s queued by the CLI", r.slug, ev.CommandUUID)
	case lifecycleStarted:
		log.Printf("claudeagent: runner[%s] fold %s landed in the running turn", r.slug, ev.CommandUUID)
		if active != nil {
			active.emitFold(ev.CommandUUID, true)
		}
	}
}

// resolveFolds reports every fold the ending turn CONSUMED, using the
// result event's user_message_uuids as the authority: that array lists
// every user message the turn actually took, so presence in it is the
// definition of "landed". The uuids that are absent are returned
// rather than reported, because what happens to them is the caller's
// decision — see absorbMissedFolds.
//
// Deliberately ledger-driven rather than trusting our own "started"
// bookkeeping — on a turn that failed or was interrupted the ledger is
// empty and every outstanding fold correctly comes back as missed.
//
// Runs on the dispatcher goroutine before the turn finalizes, because
// finalize() closes the events channel this emits on.
func (r *runner) resolveFolds(active *runnerTurn, consumed []string) []string {
	r.mu.Lock()
	pending := r.pendingFolds
	r.pendingFolds = nil
	r.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	landedSet := make(map[string]bool, len(consumed))
	for _, id := range consumed {
		landedSet[id] = true
	}
	var missed []string
	for id := range pending {
		if !landedSet[id] {
			missed = append(missed, id)
			continue
		}
		log.Printf("claudeagent: runner[%s] fold %s landed (named in the turn ledger)", r.slug, id)
		if active != nil {
			active.emitFold(id, true)
		}
	}
	// Map iteration is unordered; sort so logs and tests read the
	// same way on every run.
	sort.Strings(missed)
	return missed
}

// reportFoldsMissed tells the caller its message did not make the
// turn, so it can deliver it the way it did before folds existed.
func (r *runner) reportFoldsMissed(active *runnerTurn, missed []string) {
	for _, id := range missed {
		log.Printf("claudeagent: runner[%s] fold %s missed this turn and no continuation claimed it", r.slug, id)
		if active != nil {
			active.emitFold(id, false)
		}
	}
}

// absorbMissedFolds answers the question: what happens to a message
// that missed the turn?
//
// Measured against the real CLI, with no tool round in the turn: the
// message is NOT handed back. The CLI keeps it queued and, within the
// same tenth of a second as the missed turn's result, starts a fresh
// turn for it — new init frame, thinking, text, a second result naming
// that uuid in its ledger. The agent answers the CEO either way.
//
// Ending the turn on the first result would drop every frame of the
// continuation (no turn to route to) while core, told the fold had
// missed, re-delivered the same text as a brand-new turn: two calls
// billed, the first answer lost, and a continuation result that could
// finalize the WRONG turn.
//
// So: don't end the turn. The continuation IS this turn as far as core
// is concerned — same turn id, same stream, same chat bubble sequence
// — and the fold is reported as landed the moment its "started" frame
// proves the CLI has begun. Caller holds mu.
//
// beginAbsorbLocked is the state half; watchAbsorb bounds the wait.
func (r *runner) beginAbsorbLocked(active *runnerTurn, missed []string) {
	if len(missed) > 0 && r.absorbing == nil {
		r.absorbing = map[string]struct{}{}
	}
	for _, id := range missed {
		log.Printf("claudeagent: runner[%s] fold %s missed the turn; holding the turn open for the CLI's continuation", r.slug, id)
		r.absorbing[id] = struct{}{}
	}
	// One timer per absorbed result: a continuation that itself ends
	// with another fold still queued restarts the wait.
	if r.absorbTimer != nil {
		r.absorbTimer.Stop()
	}
	timer := r.clk.NewTimer(foldAbsorbDeadline)
	r.absorbTimer = timer
	go r.watchAbsorb(active, timer)
}

// watchAbsorb ends the turn if the continuation never comes.
//
// Nothing about the CLI's measured behaviour needs this — the
// continuation starts immediately — but "the turn stays open until a
// frame that may never arrive" is not a state to leave unbounded. On
// expiry the outstanding folds are reported as missed and core's
// fallback (wind up, flush, re-deliver) takes over.
func (r *runner) watchAbsorb(active *runnerTurn, timer clock.Timer) {
	defer timer.Stop()
	select {
	case <-timer.C():
	case <-active.finalized:
		return
	case <-r.dead:
		return
	}
	r.mu.Lock()
	if r.absorbTimer != timer {
		// Superseded by a later absorb, which owns the wait now.
		r.mu.Unlock()
		return
	}
	stranded := r.strandAbsorbLocked(active)
	r.mu.Unlock()
	log.Printf("claudeagent: runner[%s] no continuation within %s for %d missed fold(s); ending the turn", r.slug, foldAbsorbDeadline, len(stranded))
	r.reportFoldsMissed(active, stranded)
	active.finalize()
}

// strandAbsorbLocked gives up on the continuation: it clears the
// absorb state and the per-turn interrupt bookkeeping, detaches the
// turn, and returns the fold uuids that never got their continuation,
// sorted. The caller holds mu and finishes the turn (finalize on the
// deadline, fail on an interrupt).
func (r *runner) strandAbsorbLocked(active *runnerTurn) []string {
	stranded := make([]string, 0, len(r.absorbing))
	for id := range r.absorbing {
		stranded = append(stranded, id)
	}
	sort.Strings(stranded)
	r.resetInterruptStateLocked()
	if r.active == active {
		r.active = nil
	}
	return stranded
}

// gracefulKill SIGTERMs the runner subprocess, escalates to SIGKILL
// after forceKillGrace, and stamps userCancelled so the supervisor
// wraps deadErr with provider.ErrUserCancelled (not ErrSubprocessExited).
// Idempotent.
func (r *runner) gracefulKill(reason string) {
	r.killOnce.Do(func() {
		r.mu.Lock()
		r.userCancelled = true
		proc := r.cmd.Process
		r.mu.Unlock()
		log.Printf("claudeagent: runner[%s] graceful-killing: %s", r.slug, reason)
		if proc == nil {
			return
		}
		_ = proc.Signal(syscall.SIGTERM)
		timer := r.clk.NewTimer(forceKillGrace)
		go func() {
			defer timer.Stop()
			select {
			case <-r.dead:
			case <-timer.C():
				log.Printf("claudeagent: runner[%s] SIGTERM grace expired; SIGKILL", r.slug)
				_ = proc.Kill()
			}
		}()
	})
}

// dispatcher reads stream-json off stdout and routes events to the
// active turn. Runs until stdout EOF (subprocess exit).
func (r *runner) dispatcher() {
	scanner := bufio.NewScanner(r.stdout)
	scanner.Buffer(make([]byte, 0, 128*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev streamJSONEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			log.Printf("claudeagent: runner[%s] parse stream-json line %s: %v", r.slug, quoteLine(line), err)
			continue
		}
		r.routeEvent(&ev)
	}
}

func (r *runner) routeEvent(ev *streamJSONEvent) {
	// System events update runner-level state regardless of turn
	// presence: the very first system event of a fresh runner carries
	// the CLI-assigned session_id which we persist for crash recovery.
	if ev.Type == "system" {
		r.captureSessionID(ev.SessionID)
		r.captureCapabilities(ev)
	}

	// command_lifecycle reports the fate of a uuid-stamped message we
	// wrote to stdin. It is runner-scoped like control_response, and
	// can legitimately arrive with no active turn (a fold that missed
	// its turn reports "started" just after that turn's result), so
	// handle it before the no-active-turn drop below.
	if ev.Type == "command_lifecycle" {
		r.handleLifecycle(ev)
		return
	}

	// control_response is the CLI's reply to a control_request we
	// wrote (sendInterrupt). It is runner-scoped, not turn-scoped —
	// handle it before the no-active-turn drop below and never pass
	// it to the turn, which has no notion of the control channel.
	if ev.Type == "control_response" {
		var cr controlResponse
		if err := json.Unmarshal(ev.Response, &cr); err != nil {
			log.Printf("claudeagent: runner[%s] parse control_response: %v", r.slug, err)
			return
		}
		if cr.Subtype != "success" {
			// The CLI declined or couldn't parse it. Don't wait out
			// the ack deadline — the gentle path is not going to work
			// for this request.
			log.Printf("claudeagent: runner[%s] control_response %s not successful (subtype=%q err=%q); escalating", r.slug, cr.RequestID, cr.Subtype, cr.Error)
			r.gracefulKill("control_request rejected by CLI")
			return
		}
		log.Printf("claudeagent: runner[%s] control_response %s acked (cancelled=%v still_queued=%v)",
			r.slug, cr.RequestID, cr.Response.Cancelled, cr.Response.StillQueued)
		return
	}

	// Track tool_use/tool_result pairing so armInterrupt can tell
	// whether a command is mid-flight. Done before the active==nil
	// drop so the count can't skew on a stray event.
	r.trackToolsInFlight(ev)

	// Read the usage gauge off every result frame, also before the
	// active==nil drop: a frame from an unmodelled turn still moved
	// the CLI's cumulative reading, and the next turn's share has to
	// be measured from after it, or that turn is billed for both.
	var frameByModel []provider.ModelUsage
	if ev.Type == "result" {
		frameByModel = r.usageGauge.Delta(ev.gaugeReading())
	}

	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	if active == nil {
		// No turn to route to, so the frame is dropped. A missed fold
		// is absorbed into its turn (beginAbsorbLocked), so reaching
		// this point means something we do not model is running, and
		// a silent drop would hide it. Log the frames that identify
		// the turn; the rest stay quiet so a stray stream cannot
		// flood the log.
		switch ev.Type {
		case "result":
			log.Printf("claudeagent: runner[%s] DROPPED a result with no active turn (consumed %v); an unmodelled turn ran on this runner", r.slug, ev.UserMessageUUIDs)
		case "assistant":
			log.Printf("claudeagent: runner[%s] dropped an assistant frame with no active turn", r.slug)
		}
		return
	}
	// A result that lands while the interrupt is armed is the gentle
	// path completing: the CLI ended the turn because we asked it to,
	// not because the model finished. Surface that as a user cancel
	// so the chat loop classifies it exactly as it did when the only
	// way to stop a turn was to kill the process — otherwise a
	// stopped turn would read as a normal (if errored) completion.
	//
	// active.fail still runs finalize, so partial content already
	// accumulated in the turn is preserved in Final().
	if ev.Type == "result" {
		// Resolve any outstanding fold against this turn's ledger
		// BEFORE finalizing: user_message_uuids names every user
		// message the turn actually consumed, so a pending uuid
		// missing from it did not make this turn. Emitting first
		// matters because finalize() closes the events channel.
		missed := r.resolveFolds(active, ev.UserMessageUUIDs)

		r.mu.Lock()
		armed := r.interruptArmed
		// A missed fold is not a message we still hold — the CLI has
		// it queued and starts answering it immediately. Keep the
		// turn open and take that continuation as part of it. An
		// armed interrupt is the exception: its control_request
		// carries cancel_queued, so the message is coming back to us.
		absorb := !armed && (len(missed) > 0 || len(r.absorbing) > 0)
		if absorb {
			r.beginAbsorbLocked(active, missed)
		} else {
			// A result that ends the turn while folds were being
			// absorbed: the interrupt's cancel_queued handed those
			// messages back, so they are misses to report, not state
			// to drop with the turn.
			for id := range r.absorbing {
				missed = append(missed, id)
			}
			sort.Strings(missed)
			r.resetInterruptStateLocked()
			if r.active == active {
				r.active = nil
			}
		}
		r.mu.Unlock()
		if absorb {
			// Tokens, cost and stop-reason from this result still
			// belong to the turn; only the ending does not.
			active.recordResult(ev, frameByModel)
			return
		}
		r.reportFoldsMissed(active, missed)
		if armed {
			// The calls the CLI made before honouring the interrupt
			// were billed; this frame has their final counts. Bank
			// them, but not the frame's stop reason — the turn is
			// failing as a cancel.
			active.recordUsage(ev, frameByModel)
			log.Printf("claudeagent: runner[%s] turn ended by interrupt (subtype=%q); subprocess stays warm", r.slug, ev.Subtype)
			active.fail(fmt.Errorf("%w: interrupted at step boundary", provider.ErrUserCancelled))
			return
		}
		active.recordResult(ev, frameByModel)
		active.finalize()
		return
	}

	active.handleEvent(ev)
	// Stop-click cascade: when the interrupt is armed (per-turn ctx
	// was cancelled), the next tool_result event is our cue to kill
	// gracefully. The CLI has already written the tool_result to its
	// session log by the time it streams the event to us; SIGTERMing
	// now leaves the log in a clean tool-pair state for --resume on
	// the next turn. Buffered events ahead of close (StreamToolResult
	// already in the events chan) still post via handleChatTurn's
	// for-range before exit.
	//
	// We discriminate on Type=="user": in stream-json from `claude -p`
	// the user-message channel is exclusively for tool_result blocks
	// (handleUserMessage skips anything else).
	if ev.Type == "user" {
		r.mu.Lock()
		armed := r.interruptArmed
		idle := r.toolsInFlight == 0
		r.mu.Unlock()
		// Only once every announced tool has returned. With parallel
		// tool calls the CLI streams one tool_result per block, and
		// interrupting after the first would cut the others off
		// mid-flight — precisely what we're avoiding.
		if armed && idle {
			r.sendInterrupt("tool_result boundary reached post-interrupt")
		}
	}
}

// trackToolsInFlight maintains the outstanding tool_use count from the
// event stream: assistant messages announce tool_use blocks, user
// messages carry their tool_results back. The delta is what tells
// armInterrupt whether a command is currently running.
//
// Counting (rather than a bool) is what makes parallel tool calls
// safe: one assistant message may announce several tool_use blocks,
// and their results arrive across one or more user messages.
func (r *runner) trackToolsInFlight(ev *streamJSONEvent) {
	var delta int
	switch ev.Type {
	case "assistant":
		delta = countContentBlocks(ev.Message, "tool_use")
	case "user":
		delta = -countContentBlocks(ev.Message, "tool_result")
	default:
		return
	}
	if delta == 0 {
		return
	}
	r.mu.Lock()
	r.toolsInFlight += delta
	if r.toolsInFlight < 0 {
		// Defensive: a tool_result with no matching announcement
		// (replayed history on --resume, say) must not drive the
		// count negative and make a later tool look already-finished.
		r.toolsInFlight = 0
	}
	r.mu.Unlock()
}

// countContentBlocks reports how many content blocks of the given type
// a wrapped stream-json message carries. Malformed payloads count 0 —
// this feeds a scheduling hint, never correctness of the transcript.
func countContentBlocks(raw json.RawMessage, blockType string) int {
	if len(raw) == 0 {
		return 0
	}
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	n := 0
	for _, c := range m.Content {
		if c.Type == blockType {
			n++
		}
	}
	return n
}

// supervisor blocks on cmd.Wait() and, when the subprocess exits,
// detects fast-death after --resume (clearing the stored session id
// to break a corrupt-log respawn loop), marks the runner dead, fails
// any in-flight turn, and closes capture files.
func (r *runner) supervisor() {
	err := r.cmd.Wait()
	elapsed := r.clk.Now().Sub(r.spawnedAt)

	r.mu.Lock()
	userCancelled := r.userCancelled
	r.mu.Unlock()

	// If we passed --resume and the subprocess died too quickly to be
	// a real session (CLI couldn't replay the on-disk log), clear the
	// stored session id. The next Acquire's resolveResumeSessionID
	// will return "" and the runner will be born fresh.
	//
	// userCancelled deaths are excluded: a Stop click within
	// fastDeathWindow looks identical from cmd.Wait's POV (non-zero
	// exit, fast elapsed time, --resume was passed), but the on-disk
	// session log is healthy — the user just pressed Stop. Clearing
	// the id here would orphan the entire conversation log, and the
	// next turn would spawn fresh with no context. That manifests to
	// the model (and the user) as "the chat history was truncated."
	if err != nil && !userCancelled && elapsed < fastDeathWindow && r.resumeID != "" && r.opts.Sessions != nil && r.slug != "" {
		log.Printf("claudeagent: runner[%s] died after %s with --resume %s (likely corrupt session log); clearing stored session id",
			r.slug, elapsed, r.resumeID)
		if clearErr := r.opts.Sessions.ClearClaudeSessionID(r.slug); clearErr != nil {
			log.Printf("claudeagent: clear stale session for %s: %v", r.slug, clearErr)
		}
	}

	r.mu.Lock()
	if err != nil {
		r.deadErr = err
	}
	close(r.dead)
	if active := r.active; active != nil {
		// Pick the sentinel that matches the cause so the chat-loop
		// surfaces user-cancel as FailedReasonCancelled and crashes
		// as FailedReasonSubprocessDied.
		sentinel := provider.ErrSubprocessExited
		if userCancelled {
			sentinel = provider.ErrUserCancelled
		}
		active.fail(fmt.Errorf("claudeagent: runner[%s]: %w (cause: %v)", r.slug, sentinel, err))
		r.active = nil
	}
	r.mu.Unlock()

	if r.stderrFile != nil {
		_ = r.stderrFile.Close()
	}
	if r.stdinMirror != nil {
		_ = r.stdinMirror.Close()
	}
}

func (r *runner) isDead() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isDeadLocked()
}

// configMatches reports whether this live runner was spawned with the
// same model + effort + system prompt the incoming request wants. The
// CLI bakes all three into the subprocess at launch, so a mismatch means
// the runner can't honor a change the CEO just made — Acquire evicts and
// respawns (preserving the conversation via --resume on the stored
// session id). spawnModel is compared verbatim (the [1m] suffix is part
// of the identity); effort is normalized on both sides so an empty/typo'd
// request can't spuriously look like drift against a runner already on
// the default; the system prompt is compared by hash so an edit to the
// handbook / role / agent_memory respawns, while the same content
// rebuilt fresh each turn does not.
func (r *runner) configMatches(req provider.CompleteRequest) bool {
	return r.spawnModel == req.Model &&
		r.spawnEffort == normalizeEffort(req.Effort) &&
		r.spawnSystemHash == systemPromptHash(req)
}

// systemPromptHash is the identity of a request's --system-prompt: the
// sha256 of the flattened system blocks (handbook + role +
// agent_memory). Used by configMatches to detect when an edit to any of
// those should respawn the runner. Both the spawn-time capture and the
// per-turn comparison go through this single function so they can never
// disagree on how the prompt is flattened.
func systemPromptHash(req provider.CompleteRequest) string {
	sum := sha256.Sum256([]byte(flattenSystem(req.System)))
	return hex.EncodeToString(sum[:])
}

func (r *runner) isDeadLocked() bool {
	select {
	case <-r.dead:
		return true
	default:
		return false
	}
}

func (r *runner) failActive(err error) {
	r.mu.Lock()
	if r.active != nil {
		r.active.fail(err)
		r.active = nil
	}
	r.mu.Unlock()
}

func (r *runner) captureSessionID(id string) {
	if id == "" || r.opts.Sessions == nil || r.slug == "" {
		return
	}
	r.mu.Lock()
	if r.sessionSaved {
		r.mu.Unlock()
		return
	}
	r.sessionSaved = true
	r.mu.Unlock()
	if id == r.resumeID {
		return
	}
	if err := r.opts.Sessions.WriteClaudeSessionID(r.slug, id); err != nil {
		log.Printf("claudeagent: persist session %s for %s: %v", id, r.slug, err)
	}
}

// Close gracefully shuts down the runner: closes stdin (signals CLI to
// flush + exit), waits up to closeGraceTimeout, then SIGKILLs if it
// hangs. Removes the per-runner working directory. Idempotent.
func (r *runner) Close() error {
	var outErr error
	r.closeOnce.Do(func() {
		_ = r.stdin.Close()
		timer := r.clk.NewTimer(closeGraceTimeout)
		defer timer.Stop()
		select {
		case <-r.dead:
			outErr = r.deadErr
		case <-timer.C():
			if r.cmd.Process != nil {
				_ = r.cmd.Process.Kill()
			}
			<-r.dead
			outErr = fmt.Errorf("claudeagent: runner[%s] did not exit within %s; killed", r.slug, closeGraceTimeout)
		}
		if r.dir != "" {
			_ = os.RemoveAll(r.dir)
		}
	})
	return outErr
}

// writeUserMessageLine encodes the latest user release of req as one
// stream-json line and writes it to w.
func writeUserMessageLine(w io.Writer, req provider.CompleteRequest, id string) error {
	latest := latestUserMessage(req.Messages)
	if latest == nil {
		return errors.New("claudeagent: no user release to send")
	}
	body := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": contentBlocksForWire(latest.Content),
		},
	}
	// The CLI emits NO command_lifecycle frames for a message sent
	// without a uuid, which is what left mid-turn delivery
	// unobservable before: the only signal left was whether the model
	// happened to act on the message, which is a statement about the
	// model, not about delivery. Stamping one costs nothing and makes
	// the transport answerable.
	if id != "" {
		body["uuid"] = id
	}
	return json.NewEncoder(w).Encode(body)
}

// randHex returns 2*n hex characters of cryptographic randomness.
// Used for runner-dir suffix uniqueness.
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
