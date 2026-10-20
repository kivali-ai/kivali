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
	// The description names the packages the agent-shell reports having
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
				Content: []string{renderShellResult(res, agent.ShellProxyRefusalNotice)},
				IsError: res.ExitCode != 0 || res.TimedOut || res.Err != "",
			}, nil
		},
	}
	return &t
}

// renderShellResult formats a ShellResult as a compact tool_result
// body: exit/timing, the shell's notice when it has one (with refused,
// the caller's line on a proxy refusal, when the output shows one), and
// capped stdout/stderr. Files the command wrote are on disk under
// /files/, where any follow-up tool reads them.
func renderShellResult(r *agent.ShellResult, refused string) string {
	notice := r.Notice
	if agent.ShellProxyRefused(r.Stdout, r.Stderr) {
		if notice != "" {
			notice += "\n"
		}
		notice += refused
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code=%d duration_ms=%d", r.ExitCode, r.DurationMs)
	if r.TimedOut {
		b.WriteString(" timed_out=true")
	}
	if r.Err != "" {
		fmt.Fprintf(&b, " err=%q", r.Err)
	}
	b.WriteByte('\n')
	if notice != "" {
		b.WriteString("--- notice ---\n")
		b.WriteString(notice)
		b.WriteByte('\n')
	}
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

// subagentShellSoftwareText is the software paragraph for a subagent,
// which cannot propose an install itself.
const subagentShellSoftwareText = `SOFTWARE. System software (Debian packages) is installed by the owner; apt-get does not work in this shell. If you need a package that is not installed, say so to your caller, naming the Debian package and what it is for. For Python libraries use "pip install --user <pkg>" or a venv instead.`

// subagentShellDescription is run_shell's description with the
// paragraphs that name tools or people a subagent cannot reach
// (publish_*, share_file, assignments, propose_software_install, the
// owner) swapped for ones that fit it.
func subagentShellDescription(d string) string {
	d = strings.Replace(d, agent.ShellSoftwareText, subagentShellSoftwareText, 1)
	d = strings.Replace(d, agent.ShellNetworkText, agent.SubagentShellNetworkText, 1)
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
				Content: []string{renderShellResult(res, agent.SubagentShellProxyRefusalNotice)},
				IsError: res.ExitCode != 0 || res.TimedOut || res.Err != "",
			}, nil
		},
	}
}
