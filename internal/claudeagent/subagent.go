package claudeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/provider"
)

// RunSubagent spawns one `claude -p` for a subagent run and returns the
// stream reading it. It is the per-call Stream path with the subagent's
// own flags: a fresh session (no --resume, no session id kept), the
// request's ToolSet spelled the CLI's way, and the request's MCP
// servers written as the run's --mcp-config.
//
// Subprocess invocation:
//
//	claude -p
//	  --tools <Builtins as CLI names>
//	  --output-format stream-json --input-format stream-json --verbose
//	  --mcp-config <RunDir>/mcp-config.json
//	  --model <Model> --effort <Effort>
//	  --system-prompt <System>
//	  --allowedTools <mcp__kivali__<Kivali>..., Builtins as CLI names>
//
// The prompt is fed as one stream-json user line and stdin is closed,
// so the CLI exits when the run ends. Cancelling ctx kills it.
func (c *Driver) RunSubagent(ctx context.Context, req provider.SubagentRequest) (provider.Stream, error) {
	binary, err := c.resolveBinary()
	if err != nil {
		return nil, err
	}
	builtins, err := cliBuiltins(req.Tools.Builtins)
	if err != nil {
		return nil, err
	}

	// The run's own files: the --mcp-config and the stdin mirror. A
	// caller-chosen RunDir is the caller's to remove; one made here is
	// removed when the subprocess exits.
	runDir := req.RunDir
	var cleanup func()
	if runDir == "" {
		sessionDir := c.opts.SessionDir
		if sessionDir == "" {
			sessionDir = filepath.Join(os.TempDir(), "kivali-claudeagent")
		}
		if err := os.MkdirAll(sessionDir, 0o700); err != nil {
			return nil, fmt.Errorf("claudeagent: session dir: %w", err)
		}
		dir, err := os.MkdirTemp(sessionDir, "subagent-*")
		if err != nil {
			return nil, fmt.Errorf("claudeagent: subagent run dir: %w", err)
		}
		runDir = dir
		cleanup = func() { _ = os.RemoveAll(dir) }
	}
	mcpConfigPath := filepath.Join(runDir, "mcp-config.json")
	if err := writeMCPServers(mcpConfigPath, req.MCPServers); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, fmt.Errorf("claudeagent: subagent mcp-config: %w", err)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = DefaultSubagentModel
	}
	allowed := make([]string, 0, len(req.Tools.Kivali)+len(builtins))
	for _, n := range req.Tools.Kivali {
		allowed = append(allowed, cliToolName(n))
	}
	allowed = append(allowed, builtins...)

	args := []string{
		"-p",
		// The CLI's built-in catalogue narrowed to exactly the builtins
		// the request names; an empty value turns every one off.
		"--tools", strings.Join(builtins, ","),
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--mcp-config", mcpConfigPath,
		"--model", stripName(model),
		// Reasoning depth — validated, defaults to high (the fleet
		// default) when the request leaves it empty.
		"--effort", normalizeEffort(req.Effort),
		"--system-prompt", req.System,
		"--allowedTools", strings.Join(allowed, ","),
	}

	return c.spawn(ctx, spawnSpec{
		binary:        binary,
		args:          args,
		maxOldSpaceMB: subagentMaxOldSpaceMB,
		stderr:        req.Stderr,
		req: provider.CompleteRequest{
			Model:  model,
			Effort: req.Effort,
			Messages: []provider.Message{{
				Role:    provider.RoleUser,
				Content: []provider.ContentBlock{{Type: provider.ContentText, Text: req.Prompt}},
			}},
			Purpose: req.Purpose,
			Agent:   req.Agent,
		},
		// No session store: a subagent run is one-shot and is never
		// resumed, so its session id is not kept.
		stdinMirrorPath: filepath.Join(runDir, "cli-stdin.jsonl"),
		cleanup:         cleanup,
	})
}

// cliBuiltins maps neutral builtin ids to the CLI's tool names,
// refusing an id this driver does not know rather than dropping it.
func cliBuiltins(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		name, ok := cliBuiltinNames[id]
		if !ok {
			return nil, fmt.Errorf("claudeagent: unknown builtin tool %q", id)
		}
		out = append(out, name)
	}
	return out, nil
}

// subagentMaxOldSpaceMB is the V8 old-space ceiling handed to a
// subagent's `claude -p`, deliberately far below the parent's 4096.
//
// The parent CLI and every concurrent subagent share one cgroup, and
// V8 knows nothing about that cgroup: given a 4096 ceiling it will
// keep growing until the kernel intervenes, and the kernel picks its
// victim by RSS — which is the long-lived parent, holding the durable
// agent's whole session. A lower ceiling converts that into the
// failure we want: the greedy subagent hits its own heap limit, dies
// on its own, and surfaces as one failed job while everything else
// keeps running.
//
// 1024 MiB is generous for the focused, mostly-IO work a subagent
// does. Raise it only alongside the pod MemoryLimit and the
// concurrency cap — the three numbers are one budget.
const subagentMaxOldSpaceMB = 1024

// The CLI closes a run whose model call failed with an assistant
// message under a placeholder model id ("<synthetic>", see
// isSentinelModel) carrying the error text, then a result frame
// with is_error (or a non-zero exit). The text alone does not say the
// run failed — the CLI has also been seen to emit it on a run that then
// completed — so a reader HOLDS it (joinText into its pending text) and
// decides what it was when the run's outcome arrives: settleResult at
// the result frame, settleExit when the process ends without one.

// joinText appends one text block to accumulated text, newline-joined.
func joinText(prev, text string) string {
	if prev == "" {
		return text
	}
	return prev + "\n" + text
}

// cliOutcome is a reader's error state after a result frame or exit:
// the stop reason, the Final.Error detail, and the text to emit as a
// StreamError ("" when there is none to emit).
type cliOutcome struct {
	stopReason, detail, emit string
}

// settleResult resolves held sentinel text against a result frame.
// is_error makes it the typed error; any other frame means the run
// completed, and the text is logged and dropped, never filed as a
// reply. A StopError with no sentinel text (an is_error frame such as
// error_during_execution) gets its detail from the frame, so the cause
// is never reported as "no detail".
func settleResult(stopReason, detail, pending string, ev *streamJSONEvent, who string) cliOutcome {
	out := cliOutcome{stopReason: resultStopReason(stopReason, ev), detail: detail}
	if pending != "" {
		if ev.IsError {
			out.stopReason = provider.StopError
			out.detail = joinText(out.detail, pending)
			out.emit = pending
		} else {
			log.Printf("claudeagent: %s: sentinel-model text on a completed turn, dropped: %q", who, pending)
		}
	}
	if out.stopReason == provider.StopError && out.detail == "" {
		out.detail = resultErrorDetail(ev)
	}
	return out
}

// settleExit resolves held sentinel text when the process ended (or
// the run was failed) without a result frame resolving it. A failed
// exit makes the text the typed error; a clean one drops it, logged.
// A StopError still without detail takes the exit's error text.
func settleExit(stopReason, detail, pending string, exitErr error, who string) cliOutcome {
	out := cliOutcome{stopReason: stopReason, detail: detail}
	if pending != "" {
		if exitErr != nil {
			out.stopReason = provider.StopError
			out.detail = joinText(out.detail, pending)
			out.emit = pending
		} else {
			log.Printf("claudeagent: %s: sentinel-model text with no error outcome, dropped: %q", who, pending)
		}
	}
	if out.stopReason == provider.StopError && out.detail == "" && exitErr != nil {
		out.detail = exitErr.Error()
	}
	return out
}

// quoteLine names a stream-json line that failed to parse, for an
// error a person will read: its type when that much decodes, else its
// first 80 characters.
func quoteLine(line []byte) string {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &head) == nil && head.Type != "" {
		return fmt.Sprintf("of type %q", head.Type)
	}
	r := []rune(string(line))
	if len(r) > 80 {
		return fmt.Sprintf("%q…", string(r[:80]))
	}
	return fmt.Sprintf("%q", string(r))
}

// resultErrorDetail is what an is_error result frame says about itself
// when no sentinel message carried the provider's text.
func resultErrorDetail(ev *streamJSONEvent) string {
	if ev.Result != "" {
		return ev.Result
	}
	if ev.Subtype != "" {
		return fmt.Sprintf("the claude CLI ended the run with an error result (subtype %q)", ev.Subtype)
	}
	return "the claude CLI ended the run with an error result"
}

// resultStopReason is the stop reason after a result frame: the one
// already set, else end_turn
// for a frame with a final result, else error for an is_error frame.
func resultStopReason(cur string, ev *streamJSONEvent) string {
	if cur != "" {
		return cur
	}
	if ev.Result != "" {
		return provider.StopEndTurn
	}
	if ev.IsError {
		return provider.StopError
	}
	return ""
}
