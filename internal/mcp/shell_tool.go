package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
)

// ShellToolConfig wires the run_shell tool through an MCP server.
// Exec is the local executor; From is the agent slug. Exec nil means
// run_shell isn't registered (model discovers from tools/list).
//
// With the unified /files/ namespace (see docs/developers/files-and-publishing.md),
// run_shell has no FSStore-bound bookends — bytes the command writes
// to /files/... persist directly on the PVC, share_file promotes any
// path to a content-addressed attachment when the agent wants to.
type ShellToolConfig struct {
	Exec agent.ShellExecutor
	From string
}

// ShellTool returns the MCP run_shell tool definition, or nil when
// shell execution is not available in the current process.
func ShellTool(cfg ShellToolConfig) *Tool {
	if cfg.Exec == nil || cfg.From == "" {
		return nil
	}
	// The description names the packages the dev-shell reports having
	// (or says it could not read them); see agent.ShellPackages.
	def := agent.ShellToolWithPackages(agent.ShellPackages(context.Background(), cfg.Exec))
	t := Tool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: def.InputSchema,
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			var in struct {
				Command        string `json:"command"`
				Cwd            string `json:"cwd"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return &ToolResult{IsError: true, Content: []string{"invalid input: " + err.Error()}}, nil
			}
			// Best-effort skill sync; see HandleShellCall.
			_ = cfg.Exec.SyncSkills(ctx, cfg.From)
			res, err := cfg.Exec.Exec(ctx, cfg.From, agent.ShellRequest{
				Command:        in.Command,
				Cwd:            in.Cwd,
				TimeoutSeconds: in.TimeoutSeconds,
			})
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"exec: " + err.Error()}}, nil
			}
			return &ToolResult{
				Content: []string{renderShellResult(res)},
				IsError: res.ExitCode != 0 || res.TimedOut || res.Err != "",
			}, nil
		},
	}
	return &t
}

// renderShellResult formats a ShellResult as a compact tool_result
// body. Includes exit/timing and capped stdout/stderr — captured-file
// bookends are gone (the unified /files/ mount means bytes on disk
// are already where any follow-up tool needs them).
func renderShellResult(r *agent.ShellResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code=%d duration_ms=%d", r.ExitCode, r.DurationMs)
	if r.TimedOut {
		b.WriteString(" timed_out=true")
	}
	if r.Err != "" {
		fmt.Fprintf(&b, " err=%q", r.Err)
	}
	b.WriteByte('\n')
	if r.Stdout != "" {
		b.WriteString("--- stdout ---\n")
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	if r.Stderr != "" {
		b.WriteString("--- stderr ---\n")
		b.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// subagentShellDescription is run_shell's description with the two
// paragraphs that name tools a subagent does not have (publish_*,
// share_file, assignments) swapped for ones that fit it.
func subagentShellDescription(d string) string {
	d = strings.Replace(d, agent.ShellHandOffText, "To hand a file the command produced to your caller, write it under /files/artifacts/private/. No need to base64-encode or content-address it yourself.", 1)
	return strings.Replace(d, agent.ShellClosingText, "You see the tool_result and can iterate — a typical flow is: run → inspect output → fix something → run again.", 1)
}

// SubagentShellTool is the run_shell variant exposed to a subagent. It
// reuses the parent's agent pod (the Exec passed in is expected to
// be pre-bound to the parent's slug; see SubagentShellAdapter) but
// shares the same schema as the parent's run_shell — with the
// unified /files/ namespace, both surfaces are identical.
func SubagentShellTool(exec agent.ShellExecutor) Tool {
	def := agent.ShellToolWithPackages(agent.ShellPackages(context.Background(), exec))
	return Tool{
		Name:        def.Name,
		Description: subagentShellDescription(def.Description) + "\n\n(Subagent context: cwd defaults to your scoped /files/ root. To hand a file back to your parent, write it under /files/artifacts/private/.)",
		InputSchema: def.InputSchema,
		Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
			var in struct {
				Command        string `json:"command"`
				Cwd            string `json:"cwd"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return &ToolResult{IsError: true, Content: []string{"invalid input: " + err.Error()}}, nil
			}
			// Best-effort skill sync; same idea as the regular ShellTool.
			_ = exec.SyncSkills(ctx, "")
			res, err := exec.Exec(ctx, "", agent.ShellRequest{
				Command:        in.Command,
				Cwd:            in.Cwd,
				TimeoutSeconds: in.TimeoutSeconds,
			})
			if err != nil {
				return &ToolResult{IsError: true, Content: []string{"exec: " + err.Error()}}, nil
			}
			return &ToolResult{
				Content: []string{renderShellResult(res)},
				IsError: res.ExitCode != 0 || res.TimedOut || res.Err != "",
			}, nil
		},
	}
}
