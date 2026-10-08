package claudeagent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
)

// Stream spawns the Claude Code CLI as a subprocess, pipes the
// request in, and translates the CLI's stream-json output into
// provider.StreamEvents.
//
// Subprocess invocation:
//
//	claude
//	  --model <Model>
//	  --resume <session-id>        (when a session is already established)
//	  --mcp-config <written-config>
//	  --allowedTools "<all tool names, comma separated>"
//	  --output-format stream-json
//	  --input-format stream-json
//	  --system-prompt <system>
//	  --verbose
//	  -p
//
// History architecture: conversation memory lives in the CLI's own
// session log (`$HOME/.claude/projects/-/<session-id>.jsonl`). We
// feed ONE user release per call via stdin; on a repeat call we pass
// `--resume <id>` and the CLI reconstructs full context from its log
// before hitting the Anthropic API. This avoids the "orphan
// assistant tool_use on replay" 400 that the old
// "feed-full-history-via-stdin" path hit the moment structured
// tool_use blocks were included.
//
// Session bootstrap: the first call for an agent (no stored session)
// starts a fresh CLI session; we capture the assigned session_id out
// of the initial `system` event and persist it via the SessionStore
// so subsequent calls can resume.
//
// Routing: slug-bearing requests go through the persistent runner
// registry (one long-lived CLI subprocess per agent slug, see
// persistentStream); empty-slug requests (project-files
// summarization, anonymous one-offs) take the per-call path below
// since the registry is slug-keyed and has no place to put them.
func (c *Driver) Stream(ctx context.Context, req provider.CompleteRequest) (provider.Stream, error) {
	if req.Agent != "" {
		return c.persistentStream(ctx, req)
	}
	binary, err := c.resolveBinary()
	if err != nil {
		return nil, err
	}
	kivaliBin, err := c.resolveKivaliBinary()
	if err != nil {
		return nil, err
	}

	sessionDir := c.opts.SessionDir
	if sessionDir == "" {
		sessionDir = filepath.Join(os.TempDir(), "kivali-claudeagent")
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, fmt.Errorf("claudeagent: session dir: %w", err)
	}

	// Per-call mcp-config path: os.CreateTemp gives us a collision-
	// resistant filename so two concurrent per-call streams (or rapid
	// sequential calls in the same nanosecond) don't write to the
	// same path.
	tmpFile, err := os.CreateTemp(sessionDir, fmt.Sprintf("mcp-%s-*.json", safeSlug(req.Agent)))
	if err != nil {
		return nil, fmt.Errorf("claudeagent: mcp-config temp file: %w", err)
	}
	mcpConfigPath := tmpFile.Name()
	_ = tmpFile.Close()
	if err := writeMCPConfig(mcpConfigPath, kivaliBin, c.opts.DataDir, c.opts.CoreUDS, req.Agent); err != nil {
		_ = os.Remove(mcpConfigPath)
		return nil, err
	}

	systemPrompt := flattenSystem(req.System)
	allowed := strings.Join(allowedToolNames(req.Tools), ",")

	// Resolve a stored session_id (if any) for this agent, verify the
	// CLI's on-disk session log still exists, and decide whether to
	// pass --resume. A missing log means the CLI would error out, so
	// we preemptively clear the stale id and start fresh.
	resumeID := c.resolveResumeSessionID(req.Agent)

	args := []string{
		"-p",
		// Narrow Claude Code's built-in tool set to only the web tools
		// worth keeping (research / lookups). The rest — ToolSearch,
		// Bash, Edit, Read, Write, TodoWrite, Task, the Cron family,
		// worktree controls, etc. — are designed for a coding-
		// assistant CLI, not a Kivali agent. Leaving them on lets
		// the agent fall into a ToolSearch loop hunting for non-
		// existent tools, racking up iterations and tokens without
		// making progress. Our MCP tools (mcp__kivali__*) come in via
		// --mcp-config and are unaffected by this flag.
		"--tools", agentBuiltinTools,
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--mcp-config", mcpConfigPath,
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if req.Model != "" {
		args = append(args, "--model", stripName(req.Model))
	}
	// Reasoning depth. Always pass an explicit, validated --effort:
	// NormalizeEffort maps empty/typo'd values to the fleet default
	// (high), so the CLI never sees a bad flag and every turn runs at a
	// known level instead of the CLI's unstated headless default.
	args = append(args, "--effort", normalizeEffort(req.Effort))
	if systemPrompt != "" {
		// --system-prompt REPLACES Claude Code's baseline
		// coding-agent prompt with ours. Our agents (CoS, domain
		// specialists, etc.) have carefully-crafted identity
		// prompts; we don't want Claude Code's "you are a coding
		// assistant" framing layered on top biasing them toward
		// terse outputs, file-editing instincts, or its own tool
		// catalogue. If replacing the baseline breaks tool
		// discovery in practice, fall back to --append-system-prompt
		// and message the trade-off.
		args = append(args, "--system-prompt", systemPrompt)
	}
	if allowed != "" {
		args = append(args, "--allowedTools", allowed)
	}

	// Capture CLI stderr both to our parent stderr (kubectl logs)
	// AND to a file under the session dir, so we can recover
	// subprocess diagnostics after the fact. Without the file
	// capture, CLI JSON-parse errors / panics were lost in the
	// log noise and we couldn't tell why the subprocess exited
	// non-zero.
	var stderr io.Writer = os.Stderr
	var cleanup func()
	stderrPath := filepath.Join(sessionDir, "cli-stderr-latest.log")
	if stderrFile, ferr := os.OpenFile(stderrPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); ferr == nil {
		stderr = io.MultiWriter(os.Stderr, stderrFile)
		cleanup = func() { _ = stderrFile.Close() }
	}

	return c.spawn(ctx, spawnSpec{
		binary: binary,
		args:   args,
		// Bump the CLI's V8 old-space ceiling. The Node default (~1.5–1.7 GB
		// on 64-bit Linux) gets blown when a long-lived agent's session log
		// + in-memory conversation state grows large (base64 thinking
		// signatures, big tool_use inputs); V8 SIGKILLs the subprocess
		// mid-turn and the agent goes silent. 4 GiB roughly doubles the
		// headroom over the default. Kept well below the 8Gi container
		// limit (the chart's server.resources) so a single runaway CLI cannot
		// take the whole pod's memory budget by itself.
		maxOldSpaceMB:   4096,
		stderr:          stderr,
		req:             req,
		sessions:        c.opts.Sessions,
		resumeID:        resumeID,
		stdinMirrorPath: filepath.Join(sessionDir, "cli-stdin-latest.jsonl"),
		cleanup:         cleanup,
	})
}

// spawnSpec is one per-call CLI subprocess: everything the parent's
// empty-slug Stream path and a subagent run differ in. The stream-json
// reading after the spawn is shared, so both report events, usage and
// the typed error the same way.
type spawnSpec struct {
	binary string
	args   []string
	// maxOldSpaceMB is the V8 old-space ceiling handed to the CLI via
	// NODE_OPTIONS.
	maxOldSpaceMB int
	// stderr receives the subprocess's stderr; nil discards it.
	stderr io.Writer
	// req supplies the prompt feed() writes (its newest user message)
	// and the usage metadata.
	req      provider.CompleteRequest
	sessions SessionStore
	resumeID string
	// stdinMirrorPath, when set, mirrors what feed() writes.
	stdinMirrorPath string
	// cleanup, when set, runs once the subprocess has exited (or failed
	// to start).
	cleanup func()
}

// spawn starts one per-call CLI subprocess and returns the stream that
// reads it.
func (c *Driver) spawn(ctx context.Context, sp spawnSpec) (*subprocStream, error) {
	fail := func(err error) (*subprocStream, error) {
		if sp.cleanup != nil {
			sp.cleanup()
		}
		return nil, err
	}
	cmd := exec.CommandContext(ctx, sp.binary, sp.args...)
	// The CLI inherits this process's environment unchanged, HOME
	// included: it bills whatever the CLI is signed in to there.
	cmd.Env = append(os.Environ(), fmt.Sprintf("NODE_OPTIONS=--max-old-space-size=%d", sp.maxOldSpaceMB))
	cmd.Stderr = sp.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(fmt.Errorf("claudeagent: stdin pipe: %w", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(fmt.Errorf("claudeagent: stdout pipe: %w", err))
	}
	if err := cmd.Start(); err != nil {
		return fail(errors.Join(errStubNoSubprocess, err))
	}

	s := &subprocStream{
		cmd:             cmd,
		stdin:           stdin,
		stdout:          stdout,
		events:          make(chan provider.StreamEvent, 16),
		done:            make(chan struct{}),
		req:             sp.req,
		recorder:        c.opts.Recorder,
		sessions:        sp.sessions,
		resumeID:        sp.resumeID,
		gauge:           newUsageGauge(sp.resumeID == ""),
		stdinMirrorPath: sp.stdinMirrorPath,
		cleanup:         sp.cleanup,
	}
	go s.feed()
	go s.pump()
	return s, nil
}

// resolveResumeSessionID returns the stored CLI session_id for agent
// when both the id and the CLI's on-disk session log for it exist.
// If the id is stored but the log is gone (ephemeral /data wipe,
// fresh pod with a stale-sidecar agent dir, etc.) we clear the stored
// id and return "" — next call starts a fresh session.
//
// Returns "" when no SessionStore is configured (tests / API-only
// deployments). Errors reading the store are logged and treated as
// "no session" so a transient fs hiccup can't wedge an agent.
//
// Lifecycle in the persistent-runner path (slug-bearing calls): this is
// called only at runner SPAWN time — once per agent per process
// lifetime in the happy path, since the runner stays warm across
// many turns. On the happy path it's vestigial: the in-memory CLI
// session is the source of truth, and --resume just reconstitutes
// it from disk on the off-chance the runner is freshly spawned
// (process boot, pod restart, after an OOM).
//
// **Do not delete this** — it's the load-bearing piece of crash
// recovery. When a runner's subprocess dies mid-conversation
// (V8 OOM, segfault, parent-pod kill mid-turn), the supervisor
// marks the runner dead, the registry evicts it, and the next
// Acquire for that slug calls back into here. The session log on
// disk then carries the conversation forward as if nothing happened.
// Without this probe, every crash would silently amnesia the agent.
func (c *Driver) resolveResumeSessionID(agent string) string {
	if c.opts.Sessions == nil || agent == "" {
		return ""
	}
	id, err := c.opts.Sessions.ReadClaudeSessionID(agent)
	if err != nil {
		log.Printf("claudeagent: session lookup for %s failed (starting fresh): %v", agent, err)
		return ""
	}
	if id == "" {
		return ""
	}
	if !sessionLogExists(c.opts.HomeDir, id) {
		log.Printf("claudeagent: stored session %s for %s has no CLI log on disk (probably wiped); clearing and starting fresh", id, agent)
		if err := c.opts.Sessions.ClearClaudeSessionID(agent); err != nil {
			log.Printf("claudeagent: clear stale session for %s: %v", agent, err)
		}
		return ""
	}
	return id
}

// sessionLogExists probes for the CLI's on-disk session log for id.
// The CLI's project-dir name under $HOME/.claude/projects/ is derived
// from the subprocess cwd (normally "-" when cwd is "/"), but we glob
// across all project dirs so the check is robust to different
// deployments or a future cwd change. When homeDir is empty we return
// true to disable the probe (trust the stored id).
func sessionLogExists(homeDir, id string) bool {
	if id == "" {
		return false
	}
	if homeDir == "" {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(homeDir, ".claude", "projects", "*", id+".jsonl"))
	return len(matches) > 0
}

// writeMCPConfig writes a Claude Code --mcp-config file to destPath
// pointing at Kivali's own MCP server subprocess. The
// caller chooses the path; per-call writers use os.CreateTemp for
// collision-resistant names, and the persistent runner places it
// inside its own working directory.
//
// When coreUDS is non-empty (agent-pod mode), the spawned subprocess
// gets `--core-uds <path>` so every dispatcher routes through
// agentpod.Client over UDS — there is no /data mount inside an agent
// pod, so the FSStore path would fail at startup. Otherwise (Kivali
// web's in-process MCP fleet), `--data <dataDir>` is used.
func writeMCPConfig(destPath, kivaliBin, dataDir, coreUDS, agentSlug string) error {
	args := []string{"mcp", "--agent", agentSlug}
	if coreUDS != "" {
		args = append(args, "--core-uds", coreUDS)
	} else {
		args = append(args, "--data", dataDir)
	}
	return writeMCPServers(destPath, []provider.MCPServer{{
		Name:    provider.KivaliMCPServer,
		Command: kivaliBin,
		Args:    args,
	}})
}

// writeMCPServers writes servers to destPath in the CLI's --mcp-config
// format. The file is 0600: the CLI executes the command it names.
func writeMCPServers(destPath string, servers []provider.MCPServer) error {
	entries := make(map[string]mcpServerEntry, len(servers))
	for _, s := range servers {
		if s.Name == "" {
			return errors.New("claudeagent: MCP server with no name")
		}
		if _, dup := entries[s.Name]; dup {
			return fmt.Errorf("claudeagent: MCP server %q named twice", s.Name)
		}
		entries[s.Name] = mcpServerEntry{Command: s.Command, Args: s.Args}
	}
	body, err := json.MarshalIndent(mcpServersConfig{McpServers: entries}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, body, 0o600)
}

func safeSlug(s string) string {
	if s == "" {
		return "anon"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-':
			out = append(out, b)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// flattenSystem concatenates the multi-part system prompt into a
// single string. We drop cache_control hints — the CLI has no
// equivalent — but the textual content is what matters to the
// model.
func flattenSystem(parts []provider.SystemBlock) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// allowedToolNames returns the list of tool names to hand to
// `--allowedTools`. Our request-side Tool list names each publish_*
// or run_shell tool; those are the exact names registered by our
// MCP server, so Claude Code finds them via the --mcp-config.
func allowedToolNames(tools []provider.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	// Also allow tool families that live in our MCP server
	// regardless of what the request's Tools slice contains:
	//   - file_*         — /files/ virtual filesystem
	//   - agent_memory_*   — curated-summary edits, always on
	//   - get_org_chart / assignment_list — live-state lookups
	//     (on-demand tool calls rather than system-prompt text, so
	//     the model reads them as snapshots-at-lookup-time rather
	//     than as invariant background facts)
	names = append(names,
		"file_view",
		"file_create",
		"file_str_replace",
		"file_insert",
		"file_delete",
		"file_rename",
		"file_copy",
		// publishing: the only way into the read-only published area.
		"artifact_publish",
		"artifact_unpublish",
		"agent_memory_view",
		"agent_memory_append",
		"agent_memory_str_replace",
		"agent_memory_habits_view",
		"agent_memory_habits_append",
		"agent_memory_habits_str_replace",
		"get_org_chart",
		"search_past_chats",
		"list_project_files",
		"list_skills",
		"read_handbook",
		// knowledge graph reads (docs/developers/knowledge-graph.md §Reading).
		"graph_query",
		"graph_node",
		// assignment tracker (docs/developers/assignments.md §Tools).
		"assignment_create",
		"assignment_update",
		"assignment_close",
		"assignment_reopen",
		"assignment_list",
		"assignment_view",
		// subagent: dispatch focused subagents to run in the
		// background. Always-on so any agent can delegate; the MCP
		// server side gates whether the tool is actually registered
		// (FullAgentToolkit only — the SubagentToolkit deliberately
		// omits it so subagents can't nest).
		//
		// The status/cancel pair is whitelisted alongside it for the
		// same reason it is registered alongside it: dispatch returns
		// a receipt rather than an answer, so an agent that can start
		// background work must be able to see and stop it. Omitting
		// either here would leave the model holding a tool the CLI
		// refuses to call.
		"subagent",
		"subagent_status",
		"subagent_cancel",
		// CoS-only on the MCP side; harmless to whitelist on every
		// agent because the MCP server itself refuses non-CoS callers.
		"read_agent_role",
	)
	for i, n := range names {
		names[i] = cliToolName(n)
	}
	// Claude Code built-in web tools. `--tools WebFetch,WebSearch`
	// narrows Claude Code's built-in catalogue down to these two —
	// without naming them in --allowedTools as well, the CLI's hard
	// whitelist excludes them and the agent gets a "tool not allowed"
	// error when it tries to call either. Anthropic runs web_search
	// server-side, so the only egress they generate is
	// already-allowlisted api.anthropic.com.
	return append(names, cliWebFetch, cliWebSearch)
}

// The CLI's names for the built-in tools Kivali keeps, and the --tools
// value that narrows an agent's built-in catalogue to them.
const (
	cliWebFetch       = "WebFetch"
	cliWebSearch      = "WebSearch"
	agentBuiltinTools = cliWebFetch + "," + cliWebSearch
)

// cliBuiltinNames maps a neutral builtin id (provider.BuiltinWebFetch,
// ...) to the CLI's tool name.
var cliBuiltinNames = map[string]string{
	provider.BuiltinWebFetch:  cliWebFetch,
	provider.BuiltinWebSearch: cliWebSearch,
}

// cliToolPrefix is how the Claude Code CLI spells the kivali MCP
// server's tools: mcp__<server>__<tool>. It is the CLI's convention,
// so it lives here and nowhere else: every name this package emits is
// stripped of it, and every name it hands the CLI is given it.
const cliToolPrefix = "mcp__" + provider.KivaliMCPServer + "__"

// cliToolName is the CLI's name for a kivali MCP tool.
func cliToolName(bare string) string { return cliToolPrefix + bare }

// bareToolName strips the CLI's spelling off a tool name read from the
// stream. A name without it (a built-in such as WebFetch) is returned
// unchanged.
func bareToolName(name string) string { return strings.TrimPrefix(name, cliToolPrefix) }

// subprocStream implements provider.Stream against the Claude Code
// subprocess. Two goroutines run concurrently:
//   - feed() pushes the request's conversation into stdin.
//   - pump() reads JSON lines off stdout and emits StreamEvents.
type subprocStream struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	events chan provider.StreamEvent
	done   chan struct{}
	once   sync.Once

	errMu sync.Mutex
	err   error

	fmu        sync.Mutex
	id         string
	model      string
	stopReason string
	// errText is the CLI's account of the failed call that ended the
	// run (Final.Error); pendingErr is sentinel-model text held until
	// the run's outcome says what it was. See settleResult.
	errText    string
	pendingErr string
	blocks     []provider.ContentBlock
	// usage is the turn's token accounting: each API call counted once
	// by message id, superseded by the result frame's final totals
	// when it arrives, bucketed by the model that served each call so
	// a mixed turn prices per model. See turnUsage.
	usage turnUsage
	// gauge recovers this turn's per-model share from the result
	// frame's session-cumulative modelUsage. One process serves one
	// turn here, so the gauge only has a baseline when the process
	// began a fresh session; a --resume'd one reads history it cannot
	// subtract. See usageGauge.
	gauge usageGauge
	// lastReqContext is the window occupancy (input + cache_read +
	// cache_create) of the most recent assistant message that carried
	// usage — latest-wins, NOT accumulated like `usage`. Surfaces on
	// CompleteResponse.ContextTokens so a context-fill gauge can show
	// the real prompt size of the final call instead of the per-turn
	// billing sum. See CompleteResponse.ContextTokens.
	lastReqContext int
	costUSD        float64

	req      provider.CompleteRequest
	recorder provider.UsageRecorder

	// sessions persists the CLI session_id captured from the first
	// `system` event so subsequent calls can `--resume`. Nil when the
	// client was built without a SessionStore (tests, API-only).
	sessions SessionStore
	// resumeID is the id we passed to --resume on this stream, if
	// any. Tracked so we don't re-persist the same id (the CLI
	// echoes it back on every system event of a resumed session).
	resumeID     string
	sessionSaved bool

	// stdinMirrorPath, when non-empty, is a path where feed() mirrors
	// everything it writes to the CLI subprocess stdin. Used for
	// post-mortem diagnosis of stream-json rejections.
	stdinMirrorPath string

	// cleanup, when non-nil, runs once after the subprocess exits:
	// closing a stderr log, removing a run's temporary config dir.
	cleanup func()
}

func (s *subprocStream) Events() <-chan provider.StreamEvent { return s.events }
func (s *subprocStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}
func (s *subprocStream) setErr(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

func (s *subprocStream) Close() error {
	s.once.Do(func() {
		close(s.done)
		_ = s.stdin.Close()
		_ = s.stdout.Close()
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	return nil
}

func (s *subprocStream) Final() *provider.CompleteResponse {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	content := make([]provider.ContentBlock, 0, len(s.blocks))
	for _, b := range s.blocks {
		if b.Type == provider.ContentText && b.Text == "" {
			continue
		}
		content = append(content, b)
	}
	return &provider.CompleteResponse{
		ID:            s.id,
		Model:         s.model,
		StopReason:    s.stopReason,
		Error:         s.errText,
		Content:       content,
		Usage:         s.usage.Total(),
		ByModel:       s.usage.Split(),
		CostUSD:       s.costUSD,
		ContextTokens: s.lastReqContext,
	}
}

// feed streams the newest user release of the request into the
// subprocess's stdin as a single `{"type":"user","message":{…}}`
// line, then closes stdin to signal end-of-input.
//
// Why only the newest release: conversation memory lives in the CLI's
// own session log, not in what we feed via stdin. On a resumed
// session the CLI reconstructs full context from
// `$HOME/.claude/projects/-/<id>.jsonl`; on a fresh session there's
// nothing to replay by definition. Feeding prior assistant messages
// via stdin is actively harmful — the CLI writes them as orphan
// branches in its session tree, which 400s the next API call with
// "tool use concurrency issues." See the package doc.
//
// Turns earlier than the newest one are ignored here. The caller
// passes a full req.Messages slice for parity with the API
// transport; the SDK transport trims it down to the last user release
// and trusts `--resume` to carry everything else.
func (s *subprocStream) feed() {
	defer func() { _ = s.stdin.Close() }()
	// Mirror everything we feed to stdin into a per-session debug
	// file. Lets us post-mortem "what bytes did we actually send
	// the CLI on stdin?" after a subprocess exit without needing a
	// live repro. The MultiWriter is best-effort; failures to open
	// the mirror file never block the feed itself.
	var mirrorFile *os.File
	if s.stdinMirrorPath != "" {
		if f, err := os.OpenFile(s.stdinMirrorPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); err == nil {
			mirrorFile = f
			defer func() { _ = mirrorFile.Close() }()
		}
	}
	var w io.Writer = s.stdin
	if mirrorFile != nil {
		w = io.MultiWriter(s.stdin, mirrorFile)
	}
	enc := json.NewEncoder(w)

	latest := latestUserMessage(s.req.Messages)
	if latest == nil {
		// Nothing to send. The CLI will close the release with an error
		// about missing input; callers of Stream shouldn't reach this
		// path in practice (ready-agent filters always leave a trailing
		// user message).
		s.setErr(errors.New("claudeagent: feed: no user message to send"))
		return
	}
	body := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": contentBlocksForWire(latest.Content),
		},
	}
	if err := enc.Encode(body); err != nil {
		s.setErr(fmt.Errorf("claudeagent: feed: %w", err))
		return
	}
}

// latestUserMessage returns the final user-role message in msgs, or
// nil when msgs is empty or ends on assistant. The SDK transport
// feeds only this one release to the subprocess.
func latestUserMessage(msgs []provider.Message) *provider.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleUser {
			return &msgs[i]
		}
	}
	return nil
}

// contentBlocksForWire releases our neutral ContentBlock slice into
// the shape Claude Code expects on stream-json input:
//   - text          → {type:"text", text:...}
//   - tool_result   → {type:"tool_result", tool_use_id:..., content:..., is_error:...}
//   - image         → {type:"image", source:{type:"base64", media_type:..., data:...}}
//
// tool_use blocks are intentionally NOT emitted: the SDK transport
// only feeds user-role content, and a well-formed user release carries
// text, tool_result, or image — never tool_use. If a caller passes
// one it's a bug upstream; we drop it silently rather than feed a
// malformed line the CLI would log as an orphan node.
func contentBlocksForWire(blocks []provider.ContentBlock) []map[string]any {
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case provider.ContentText:
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case provider.ContentToolResult:
			out = append(out, map[string]any{
				"type":        "tool_result",
				"tool_use_id": b.ToolResultID,
				"content":     b.ToolResultContent,
				"is_error":    b.ToolResultIsError,
			})
		case provider.ContentImage:
			// Claude Code's stream-json forwards image blocks to
			// the Anthropic API unchanged (base64-encoded inline).
			// Used by project-file summarization for images —
			// without this, images were skipped entirely because
			// no canonical text could be extracted.
			out = append(out, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.ImageMediaType,
					"data":       base64.StdEncoding.EncodeToString(b.ImageData),
				},
			})
		}
	}
	return out
}

// pump reads stream-json lines off stdout and translates them into
// StreamEvents. Runs until the subprocess exits or Close is called.
func (s *subprocStream) pump() {
	defer func() {
		// Wait for the subprocess; report non-zero exit on Err.
		waitErr := s.cmd.Wait()
		if waitErr != nil {
			// Wrap with provider.ErrSubprocessExited so the chat-loop
			// can distinguish runtime crashes from soft errors and
			// write a KindRuntimeDisruption row.
			s.setErr(fmt.Errorf("claudeagent: %w (cause: %v)", provider.ErrSubprocessExited, waitErr))
		}
		// Sentinel text no result frame resolved: the exit decides.
		s.fmu.Lock()
		out := settleExit(s.stopReason, s.errText, s.pendingErr, waitErr, "per-call stream")
		s.stopReason, s.errText, s.pendingErr = out.stopReason, out.detail, ""
		s.fmu.Unlock()
		if out.emit != "" {
			s.emit(provider.StreamEvent{Kind: provider.StreamError, Text: out.emit})
		}
		if s.cleanup != nil {
			s.cleanup()
		}
		// Record usage for whatever we captured: one row per answering
		// model on a mixed turn, one row total otherwise.
		s.fmu.Lock()
		rows := usageEvents(provider.UsageEvent{
			TS:      time.Now().UTC(),
			Model:   s.model,
			Purpose: s.req.Purpose,
			Agent:   s.req.Agent,
		}, s.usage.Total(), s.usage.Split(), s.costUSD)
		s.fmu.Unlock()
		if s.recorder != nil {
			for _, row := range rows {
				_ = s.recorder(row)
			}
		}
		s.events <- provider.StreamEvent{Kind: provider.StreamEnd}
		close(s.events)
	}()

	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 0, 128*1024), 16*1024*1024)
	for scanner.Scan() {
		select {
		case <-s.done:
			return
		default:
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev streamJSONEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// A protocol violation fails the run (Err), and the
			// detail quotes the line so it can be diagnosed.
			s.setErr(fmt.Errorf("claudeagent: parse stream-json line %s: %w", quoteLine(line), err))
			continue
		}
		s.handleEvent(&ev)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.setErr(err)
	}
}

// handleEvent processes one parsed stream-json line. Text and
// tool_use blocks inside assistant messages become StreamEvents;
// the final "result" line carries totals we fold into usage.
func (s *subprocStream) handleEvent(ev *streamJSONEvent) {
	switch ev.Type {
	case "system":
		// Initial session info. We pull the session-level model name
		// if it's present, and — for fresh sessions — capture the
		// session_id the CLI assigned so we can `--resume` on future
		// calls. The id travels on the top-level event (parsed into
		// ev.SessionID in protocol.go).
		var sys struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(ev.Message, &sys)
		if sys.Model != "" {
			s.fmu.Lock()
			s.model = sys.Model
			s.fmu.Unlock()
		}
		s.captureSessionID(ev.SessionID)
	case "assistant":
		s.handleAssistantMessage(ev.Message)
	case "user":
		// In single-subprocess mode, user-role events carry the
		// CLI-dispatched tool_result blocks from our MCP server.
		// Surface them so the web layer can persist + render them
		// without doing its own dispatch.
		s.handleUserMessage(ev.Message)
	case "result":
		s.fmu.Lock()
		// The frame's usage is the turn's real total — final output
		// count included — and supersedes the per-call snapshots
		// accumulated above. The gauge reading is taken whether or not
		// the total is usable, so the gauge stays in step.
		delta := s.gauge.Delta(ev.gaugeReading())
		if total, ok := ev.resultTotal(); ok {
			s.usage.Result(total, delta)
		}
		if ev.TotalCostUSD > 0 {
			s.costUSD = ev.TotalCostUSD
		}
		out := settleResult(s.stopReason, s.errText, s.pendingErr, ev, "per-call stream")
		s.stopReason, s.errText, s.pendingErr = out.stopReason, out.detail, ""
		s.fmu.Unlock()
		if out.emit != "" {
			s.emit(provider.StreamEvent{Kind: provider.StreamError, Text: out.emit})
		}
	}
}

// handleAssistantMessage peels apart an assistant message into text
// deltas and tool_use announcements. Claude Code's stream-json
// emits complete assistant messages (not token-by-token deltas);
// we translate each content block into a single StreamDelta /
// StreamToolUseStart+End pair.
func (s *subprocStream) handleAssistantMessage(raw json.RawMessage) {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		s.setErr(fmt.Errorf("claudeagent: assistant message: %w", err))
		return
	}
	// A sentinel ID must not win this latest-wins assignment — see
	// isSentinelModel. s.model is the turn's answering model on
	// CompleteResponse and the fallback every usage row prices against;
	// letting "<synthetic>" land here books the turn at $0.
	if m.Model != "" && !isSentinelModel(m.Model) {
		s.fmu.Lock()
		s.model = m.Model
		s.fmu.Unlock()
	}
	// Each assistant message in stream-json corresponds to one
	// Anthropic /v1/messages call, and the CLI streams that message
	// once per content block, each event repeating the call's
	// start-of-message usage snapshot. The reducer keys on m.ID so a
	// three-block message counts as one call, and lets the result
	// frame's final totals replace these snapshots later — the
	// snapshot's output_tokens is only what had been written when the
	// message began. Summing every event here was a 2.4x over-count on
	// prompt tokens; taking the snapshots as final was a 25x
	// under-count on output.
	if snapshot := m.Usage.tokens(); !snapshot.IsZero() {
		s.fmu.Lock()
		// Bucket this call's tokens under the model that served IT.
		// m.Model is the per-call id; s.model is only the most recent
		// one. attributeModel keeps a synthetic close-out message's
		// real, billed tokens on the model that actually answered.
		s.usage.Call(m.ID, attributeModel(m.Model, s.model, s.req.Model), snapshot)
		// Window occupancy is per-call, not cumulative: each request
		// re-sends the whole prompt, so this single message's
		// input+cache footprint IS the window fill at that point. Keep
		// the latest (the final call sees the most context), distinct
		// from the billing total. The snapshot's prompt-side counts
		// are final, so this needs no correction from the result frame.
		s.lastReqContext = snapshot.InputTokens + snapshot.CacheReadTokens + snapshot.CacheCreateTokens
		s.fmu.Unlock()
	}
	// Walk the content blocks. thinkingEmitted ensures we emit at most
	// one thinking signal per assistant message even if the model
	// produced multiple thinking blocks — each assistant message is one
	// model round-trip = one "thinking step" for the progress counter.
	// A sentinel-model message is the CLI's close-out text, not the
	// model speaking: keep it out of the content and hold it until the
	// result frame (or the exit) says whether the run failed. See
	// settleResult.
	if isSentinelModel(m.Model) {
		s.fmu.Lock()
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				s.pendingErr = joinText(s.pendingErr, c.Text)
			}
		}
		s.fmu.Unlock()
		return
	}
	thinkingEmitted := false
	for _, c := range m.Content {
		switch c.Type {
		case "thinking":
			// Extended-reasoning marker. Live-only progress signal: not
			// appended to s.blocks (we don't persist/replay thinking) —
			// forwarded so the UI's thinking bubble can count reasoning
			// steps and show progress. Emitted even when Thinking is
			// empty: Opus encrypts its reasoning (signature only, no
			// text), so the empty marker is the ONLY progress signal we
			// get from it. Text rides along when present (Sonnet/Haiku)
			// for a future summary; the step counter works regardless.
			if thinkingEmitted {
				continue
			}
			thinkingEmitted = true
			s.emit(provider.StreamEvent{Kind: provider.StreamThinking, Text: c.Thinking})
		case "text":
			if c.Text == "" {
				continue
			}
			s.fmu.Lock()
			s.blocks = append(s.blocks, provider.ContentBlock{Type: provider.ContentText, Text: c.Text})
			s.fmu.Unlock()
			// Tag the delta with the model that produced THIS message.
			// Consumers label the bubble from it, and a turn can switch
			// models between calls, so the per-call id is the only one
			// that labels the text correctly.
			s.emit(provider.StreamEvent{Kind: provider.StreamDelta, Text: c.Text, Model: m.Model})
		case "tool_use":
			// The CLI delivers the full tool_use in one shot; emit
			// start + end back-to-back so downstream chip lifecycle
			// sees the same two events a streamed tool_use would give.
			id := c.ID
			// MCP-registered tools arrive with the CLI's per-server
			// prefix. Strip it so callers see the canonical name
			// (e.g. "file_create") that matches their dispatch
			// logic.
			name := bareToolName(c.Name)
			s.fmu.Lock()
			s.blocks = append(s.blocks, provider.ContentBlock{
				Type:      provider.ContentToolUse,
				ToolUseID: id,
				ToolName:  name,
				ToolInput: c.Input,
			})
			s.fmu.Unlock()
			s.emit(provider.StreamEvent{Kind: provider.StreamToolUseStart, ToolUseID: id, ToolName: name})
			s.emit(provider.StreamEvent{Kind: provider.StreamToolUseEnd, ToolUseID: id, ToolName: name, ToolInput: c.Input})
		}
	}
}

// handleUserMessage decodes a user-role stream-json event and emits
// a StreamToolResult for each tool_result content block. The CLI
// injects these after its MCP server returns from a tool dispatch,
// so they're the tool results the caller would otherwise have to
// compute itself.
func (s *subprocStream) handleUserMessage(raw json.RawMessage) {
	var m struct {
		Content []messageContent `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		s.setErr(fmt.Errorf("claudeagent: user message: %w", err))
		return
	}
	for _, c := range m.Content {
		if c.Type != "tool_result" {
			continue
		}
		// The CLI shapes tool_result content as either a string or an
		// array of text blocks. Our MCP server always returns text
		// blocks. Flatten to a single string either way.
		text := flattenToolResultContent(c.ContentRaw)
		s.emit(provider.StreamEvent{
			Kind:              provider.StreamToolResult,
			ToolUseID:         c.ToolUseID,
			ToolResultText:    text,
			ToolResultIsError: c.IsError,
		})
	}
}

// flattenToolResultContent accepts either a JSON string or a JSON
// array of {type:"text", text:"..."} blocks (Claude's standard tool
// result content shape) and returns the concatenated text.
func flattenToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try as string first.
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return asStr
	}
	// Fall back to array of blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// captureSessionID persists the CLI-assigned session_id for this
// agent so the NEXT stream call can pass `--resume <id>`. Called on
// every `system` event; we only write once per stream (first
// non-empty id wins) and skip when the id matches the one we
// resumed against (nothing changed).
func (s *subprocStream) captureSessionID(id string) {
	if id == "" || s.sessions == nil || s.req.Agent == "" {
		return
	}
	s.fmu.Lock()
	if s.sessionSaved {
		s.fmu.Unlock()
		return
	}
	s.sessionSaved = true
	s.fmu.Unlock()
	// No-op when the id is unchanged — the CLI echoes the resumed id
	// back on every system event of a continued session.
	if id == s.resumeID {
		return
	}
	if err := s.sessions.WriteClaudeSessionID(s.req.Agent, id); err != nil {
		log.Printf("claudeagent: persist session %s for %s: %v", id, s.req.Agent, err)
	}
}

func (s *subprocStream) emit(ev provider.StreamEvent) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

func (c *Driver) resolveBinary() (string, error) {
	p, err := exec.LookPath(c.opts.ClaudeBinary)
	if err != nil {
		return "", fmt.Errorf("claudeagent: %q not found on PATH (install Claude Code and sign it in by running `claude`): %w", c.opts.ClaudeBinary, err)
	}
	return p, nil
}

func (c *Driver) resolveKivaliBinary() (string, error) {
	if c.opts.KivaliBinary != "" {
		return c.opts.KivaliBinary, nil
	}
	bin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("claudeagent: resolving own binary path: %w", err)
	}
	return bin, nil
}
