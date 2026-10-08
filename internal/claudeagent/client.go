// Package claudeagent is the one implementation of provider.Client.
// Model calls are routed through the Claude Code CLI (`claude -p
// --output-format stream-json`), which authenticates itself with
// whatever the CLI's sign-in set up under $HOME: a Claude subscription, an
// Anthropic Console account, Amazon Bedrock or Google Vertex AI.
//
// The CLI is responsible for the Anthropic wire; this package
// shells out, streams messages in, and translates streamed events
// back into the neutral provider.StreamEvent shape the rest of
// Kivali consumes.
//
// Tool dispatch is delegated to the MCP server in internal/mcp: we
// write a small --mcp-config file pointing at `kivali mcp --agent
// <slug>` and Claude Code spawns it as a subprocess. All tool
// routing happens there, not here.
//
// Trade-offs baked in:
//   - No cache_control hint support. The CLI manages caching
//     itself; our CacheControl flags on CompleteRequest are dropped.
//   - Coarse usage: CacheReadTokens / CacheCreateTokens come from
//     the CLI's reported usage if present, otherwise zero. CostUSD is
//     the total_cost_usd the CLI reports on its final "result" event
//     when available; on a signed-in account it is notional.
//   - StreamToolInputDelta is not emitted by Claude Code's
//     stream-json (tool inputs arrive whole at tool_use_end). UI
//     chips update at completion, not mid-stream.
package claudeagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/provider"
)

// SessionStore persists the Claude Code CLI session_id per agent so
// we can resume the same CLI-owned conversation on subsequent streams
// (`claude -p --resume <id>`). Without a stored id the client starts
// a fresh session; on the first assistant event the real session_id
// comes back on the wire and we Write it here.
//
// The Kivali *store.FSStore satisfies this shape — main.go wires it.
// A tiny in-memory implementation is used in tests.
type SessionStore interface {
	ReadClaudeSessionID(slug string) (string, error)
	WriteClaudeSessionID(slug, id string) error
	ClearClaudeSessionID(slug string) error
}

// Options configures a subscription-backed client.
type Options struct {
	// Recorder receives per-call usage events. On the SDK
	// transport, token fields are populated from the CLI's usage
	// reporting when available and zero otherwise.
	Recorder provider.UsageRecorder

	// ClaudeBinary is the path to the `claude` CLI executable.
	// Defaults to "claude" (expected on PATH).
	ClaudeBinary string

	// KivaliBinary is the path to this binary — needed so the CLI
	// can spawn `<KivaliBinary> mcp --agent <slug>` as the MCP
	// tool server. Defaults to the calling process's executable
	// path resolved via os.Executable().
	KivaliBinary string

	// DataDir is the Kivali data directory (/data in-cluster).
	// Passed to the MCP subprocess so tools operate against the
	// same filesystem the main Kivali server does. Ignored when
	// CoreUDS is set — agent pods don't mount /data.
	DataDir string

	// CoreUDS is the path to core's Unix-domain socket. When set
	// (agent-pod mode), the spawned `kivali mcp` subprocess gets
	// `--core-uds <path>` and routes every dispatcher through
	// agentpod.Client over UDS instead of opening a local FSStore.
	// When empty (Kivali web's in-process MCP fleet), `--data
	// <DataDir>` is used instead. Mutually exclusive with DataDir
	// in practice; if both are set, CoreUDS wins.
	CoreUDS string

	// SessionDir is a writable directory the client uses to stash
	// per-call MCP config files and (future) session state.
	// Defaults to os.TempDir()/kivali-claudeagent.
	SessionDir string

	// Sessions, when non-nil, enables per-agent `--resume` replay:
	// on each Stream call, the client reads the persisted session_id
	// (if any), passes `--resume <id>` to the subprocess, feeds ONLY
	// the newest user release via stdin, and lets the CLI's on-disk
	// session log carry prior context. Capturing the fresh session_id
	// on first call is automatic — it arrives in the CLI's initial
	// `system` event.
	//
	// When Sessions is nil the client feeds the whole history via
	// stream-json stdin, for tests that can't supply a session store.
	Sessions SessionStore

	// HomeDir is the $HOME directory the claude CLI reads session
	// logs from (`$HOME/.claude/projects/…`). Used to probe whether
	// a stored session_id still has a matching on-disk log before
	// we pass `--resume` — if the log is gone (/data wipe, fresh
	// pod, etc.) the CLI would error out; detecting the missing log
	// lets us clear the stale id and start fresh.
	//
	// Defaults to os.Getenv("HOME"). Empty disables the probe — we
	// trust the stored id.
	HomeDir string

	// ShutdownCtx, when non-nil, drains the registry when canceled.
	// Plumb the process-wide shutdown ctx here so SIGTERM cleanly
	// closes warm runners. When nil, runners stay alive until
	// process exit.
	ShutdownCtx context.Context

	// clock is an unexported test seam — production callers cannot
	// set it (different package), so it is always nil and the runner
	// falls back to the real clock. Tests in this package set it to a
	// clock.Fake.
	clock clock.Clock
}

// New returns a provider.Client that routes model calls through the
// Claude Code CLI. Nothing is probed here: whether the CLI is
// installed, signed in or keyed only surfaces on the first Stream /
// Complete.
func New(opts Options) *Driver {
	if opts.ClaudeBinary == "" {
		opts.ClaudeBinary = "claude"
	}
	// Resolve HomeDir once so the session-log probe finds the CLI's
	// log under `$HOME/.claude/projects/`. Callers can pin this in
	// Options to force a specific dir (tests do this against a temp
	// dir); everything else inherits the process's $HOME — which is
	// `/data/claude-home` in the k8s deployment (Dockerfile sets
	// HOME there so the CLI's sign-in persists to the PVC).
	if opts.HomeDir == "" {
		opts.HomeDir = os.Getenv("HOME")
	}
	return &Driver{opts: opts}
}

type Driver struct {
	opts Options

	// registryOnce + registry: lazy-constructed on the first
	// slug-bearing Stream() arrival. Lazy because newRunnerRegistry
	// can fail (mkdir of session dir) and we don't want to surface
	// that at process boot — the empty-slug per-call path stays
	// usable even if the registry never spins up successfully.
	registryOnce sync.Once
	registry     *runnerRegistry
	registryErr  error
}

func (c *Driver) Complete(ctx context.Context, req provider.CompleteRequest) (*provider.CompleteResponse, error) {
	// Complete is defined as "run to completion, return the full
	// response". We implement it as a thin wrapper around Stream:
	// drive the stream to completion, accumulate, and return
	// Final. This mirrors what the HTTP client effectively does.
	stream, err := c.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	for range stream.Events() {
		// drain
	}
	if err := stream.Err(); err != nil {
		return stream.Final(), err
	}
	return stream.Final(), nil
}

// FormatUsage renders the CLI's view of usage: the token counts it
// reported, when it reported any, plus a narrative saying how the
// calls are paid for: whatever the CLI is signed in to.
func (c *Driver) FormatUsage(u provider.TokenUsage) provider.UsageDisplay {
	fields := []provider.UsageField{}
	if u.InputTokens > 0 {
		fields = append(fields, provider.UsageField{Label: "in", Value: formatTokens(u.InputTokens)})
	}
	if u.OutputTokens > 0 {
		fields = append(fields, provider.UsageField{Label: "out", Value: formatTokens(u.OutputTokens)})
	}
	return provider.UsageDisplay{
		Fields:    fields,
		Narrative: "billed to the Claude Code sign-in",
	}
}

// HandlesToolLoop returns true: one `claude -p` subprocess drives
// the full conversation. Stream emits events for every assistant
// release (including CLI/MCP-dispatched tool_results) until the loop
// reaches a final non-tool_use release — callers must NOT iterate or
// re-dispatch.
func (c *Driver) HandlesToolLoop() bool { return true }

// formatTokens mirrors the helper in the http client — exported
// shape kept internal to the package for now; consolidate when a
// second caller lands.
func formatTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// errStubNoSubprocess is returned if subprocess plumbing fails in a
// way that's not worth wrapping. Callers surface the message to the
// operator through the usual error path.
var errStubNoSubprocess = errors.New("claudeagent: subprocess could not be started — is the claude CLI installed and on PATH?")

// ensureRegistry lazily constructs the runner registry on the first
// slug-bearing Stream() call. Returns (nil, err) when construction
// failed permanently (subsequent calls return the same error — we
// don't retry; the failure modes are "session dir is unwritable" or
// similar boot-time fatal things).
func (c *Driver) ensureRegistry() (*runnerRegistry, error) {
	c.registryOnce.Do(func() {
		kivaliBin, err := c.resolveKivaliBinary()
		if err != nil {
			c.registryErr = err
			return
		}
		sessionDir := registrySessionDirDefault(c.opts)
		c.registry, c.registryErr = newRunnerRegistry(c.opts, kivaliBin, sessionDir, c.opts.ShutdownCtx)
	})
	return c.registry, c.registryErr
}

// ResetSession tears down the warm runner for slug so its long-lived
// `claude -p` subprocess is killed and the next turn spawns fresh.
//
// Called on chat rotation: core clears the stored CLI session id and
// then signals this (via the agent-pod EventSessionReset path) so the
// model's in-memory context is actually dropped. Clearing the id alone
// is inert against a warm runner — persistentStream's Acquire returns
// the live runner without re-reading the id. After eviction the next
// Acquire reads the now-empty session id and spawns without --resume:
// a genuinely fresh conversation, which is the whole point of rotation.
//
// No-op when the registry was never constructed (no slug-bearing turn
// has run in this process yet — there is no warm runner to evict).
func (c *Driver) ResetSession(slug string) error {
	if slug == "" {
		return errors.New("claudeagent: ResetSession requires a non-empty agent slug")
	}
	reg, err := c.ensureRegistry()
	if err != nil {
		return fmt.Errorf("claudeagent: ResetSession: registry init: %w", err)
	}
	if reg == nil {
		return nil
	}
	reg.Evict(slug)
	return nil
}

// persistentStream routes Stream() through the runner registry:
// acquires (or spawns) the runner for req.Agent, then submits one
// turn.
//
// Caller-side preconditions enforced upstream:
//   - req.Agent is non-empty (slug-keyed registry)
//   - turns for the same agent are serialized (web-layer streamMu)
func (c *Driver) persistentStream(ctx context.Context, req provider.CompleteRequest) (provider.Stream, error) {
	reg, err := c.ensureRegistry()
	if err != nil {
		return nil, fmt.Errorf("claudeagent: registry init: %w", err)
	}
	if reg == nil {
		// ensureRegistry returned (nil, nil) only on a path that no
		// longer exists; defensive belt-and-suspenders.
		return nil, errors.New("claudeagent: persistentStream called without registry")
	}
	resumeID := c.resolveResumeSessionID(req.Agent)
	r, err := reg.Acquire(ctx, req.Agent, resumeID, req)
	if err != nil {
		return nil, err
	}
	return r.RunTurn(ctx, req)
}
