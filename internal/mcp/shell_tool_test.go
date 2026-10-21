package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
)

// fakeShellExec is a scripted ShellExecutor — returns a canned
// ShellResult without spawning a process.
type fakeShellExec struct {
	res    *agent.ShellResult
	lastIn agent.ShellRequest
}

func (f *fakeShellExec) Exec(_ context.Context, _ string, req agent.ShellRequest) (*agent.ShellResult, error) {
	f.lastIn = req
	return f.res, nil
}
func (f *fakeShellExec) SyncSkills(_ context.Context, _ string) error { return nil }

// TestShellToolForwardsCommandAndRenders covers the post-unified-files
// surface: the handler decodes (command, cwd, timeout_seconds), forwards
// the request to the executor, and renders the ShellResult as text.
// Captured-files persistence is gone — bytes the command writes to
// /files/... persist directly on the PVC.
func TestShellToolForwardsCommandAndRenders(t *testing.T) {
	exec := &fakeShellExec{res: &agent.ShellResult{
		ExitCode:   0,
		Stdout:     "ok\n",
		DurationMs: 12,
	}}
	tool := ShellTool(ShellToolConfig{Exec: exec, From: "alice"})
	if tool == nil {
		t.Fatal("ShellTool returned nil")
	}
	res, err := tool.Handler(context.Background(), json.RawMessage(`{"command":"echo ok","cwd":"artifacts/private","timeout_seconds":10}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %+v", res.Content)
	}
	if exec.lastIn.Command != "echo ok" {
		t.Errorf("Command = %q, want echo ok", exec.lastIn.Command)
	}
	if exec.lastIn.Cwd != "artifacts/private" {
		t.Errorf("Cwd = %q, want artifacts/private", exec.lastIn.Cwd)
	}
	if exec.lastIn.TimeoutSeconds != 10 {
		t.Errorf("TimeoutSeconds = %d, want 10", exec.lastIn.TimeoutSeconds)
	}
	body := res.Content[0]
	if !strings.Contains(body, "exit_code=0") {
		t.Errorf("rendered body missing exit_code=0:\n%s", body)
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("rendered body missing stdout:\n%s", body)
	}
}

// listingShellExec is a fakeShellExec that also reports its packages,
// as the agent-shell sidecar client does.
type listingShellExec struct {
	fakeShellExec
	pkgs []string
}

func (l *listingShellExec) Packages(context.Context) (agent.ShellSoftware, error) {
	return agent.ShellSoftware{Packages: l.pkgs}, nil
}

// TestShellToolDescriptionIsDerivedFromTheExecutor: the toolkit the
// description names is what the executor reports, for the full agent
// and the subagent alike; an executor that cannot report gets the
// fallback sentence.
func TestShellToolDescriptionIsDerivedFromTheExecutor(t *testing.T) {
	exec := &listingShellExec{pkgs: []string{"bash", "jq"}}
	full := ShellTool(ShellToolConfig{Exec: exec, From: "alice"})
	sub := SubagentShellTool(exec)
	for name, d := range map[string]string{"full": full.Description, "subagent": sub.Description} {
		if !strings.Contains(d, "Installed packages: bash, jq.") {
			t.Errorf("%s description does not list the executor's packages:\n%s", name, d)
		}
	}
	// Both tie pip to the allow list: a fresh team's list has no package
	// index, so a plain "use pip" would send the agent into a refusal.
	for name, d := range map[string]string{"full": full.Description, "subagent": sub.Description} {
		if !strings.Contains(d, "python3-<name>") || !strings.Contains(d, "on the team's allow list") {
			t.Errorf("%s description does not say where Python libraries come from:\n%s", name, d)
		}
	}
	// The full agent asks for software itself; a subagent, which has no
	// propose_* tools, asks its caller.
	if !strings.Contains(full.Description, "propose_software_install") {
		t.Errorf("full description does not name propose_software_install:\n%s", full.Description)
	}
	if strings.Contains(sub.Description, "propose_software_install") {
		t.Errorf("subagent description names propose_software_install:\n%s", sub.Description)
	}
	if !strings.Contains(sub.Description, "say so to your caller") {
		t.Errorf("subagent description lacks its software paragraph:\n%s", sub.Description)
	}
	plain := ShellTool(ShellToolConfig{Exec: &fakeShellExec{}, From: "alice"})
	if !strings.Contains(plain.Description, "package list could not be read") {
		t.Errorf("description without a list lacks the fallback:\n%s", plain.Description)
	}
}

// TestShellToolMarksErrorOnNonZeroExit covers the error-flag path so
// the model sees a tool_use is_error=true for failing commands.
func TestShellToolMarksErrorOnNonZeroExit(t *testing.T) {
	exec := &fakeShellExec{res: &agent.ShellResult{
		ExitCode: 7,
		Stderr:   "boom\n",
	}}
	tool := ShellTool(ShellToolConfig{Exec: exec, From: "alice"})
	res, err := tool.Handler(context.Background(), json.RawMessage(`{"command":"exit 7"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError=true for exit 7")
	}
}

// The shell's notice (software it could not install) reaches the model
// with the command's output, and does not by itself mark the call an
// error.
func TestShellToolRendersTheNotice(t *testing.T) {
	exec := &fakeShellExec{res: &agent.ShellResult{
		Stdout: "ok\n",
		Notice: "This software could not be installed when your shell started, so commands that need it will fail: nosuch (Unable to locate package nosuch).",
	}}
	tool := ShellTool(ShellToolConfig{Exec: exec, From: "alice"})
	res, err := tool.Handler(context.Background(), json.RawMessage(`{"command":"true"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if res.IsError {
		t.Errorf("a notice alone marked the call an error: %+v", res.Content)
	}
	body := res.Content[0]
	if !strings.Contains(body, "--- notice ---\n"+exec.res.Notice) {
		t.Errorf("rendered body lacks the notice:\n%s", body)
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("rendered body lacks stdout:\n%s", body)
	}
	plain := renderShellResult(&agent.ShellResult{Stdout: "ok\n"}, agent.ShellProxyRefusalNotice)
	if strings.Contains(plain, "notice") {
		t.Errorf("result without a notice renders one:\n%s", plain)
	}
}

// A proxy refusal in the output gets the allow-list notice, after the
// shell's own notice: the agent's run_shell sends it to the owner and a
// subagent's to its answer, as their descriptions do.
func TestShellToolNamesAProxyRefusal(t *testing.T) {
	exec := &fakeShellExec{res: &agent.ShellResult{
		ExitCode: 56,
		Stderr:   "curl: (56) CONNECT tunnel failed, response 403\n",
		Notice:   "shell notice",
	}}
	sub := SubagentShellTool(exec)
	for _, c := range []struct {
		name, notice, network string
		tool                  *Tool
	}{
		{"agent", agent.ShellProxyRefusalNotice, agent.ShellNetworkText, ShellTool(ShellToolConfig{Exec: exec, From: "alice"})},
		{"subagent", agent.SubagentShellProxyRefusalNotice, agent.SubagentShellNetworkText, &sub},
	} {
		res, err := c.tool.Handler(context.Background(), json.RawMessage(`{"command":"curl https://python.org"}`))
		if err != nil {
			t.Fatalf("%s: Handler: %v", c.name, err)
		}
		if want := "--- notice ---\nshell notice\n" + c.notice + "\n"; !strings.Contains(res.Content[0], want) {
			t.Errorf("%s: rendered body lacks %q:\n%s", c.name, want, res.Content[0])
		}
		if !strings.Contains(c.tool.Description, c.network) {
			t.Errorf("%s: description lacks its network paragraph:\n%s", c.name, c.tool.Description)
		}
	}
	if strings.Contains(sub.Description, "tell the owner") {
		t.Errorf("subagent description sends it to the owner:\n%s", sub.Description)
	}
}

// TestSubagentShellToolForwards mirrors TestShellToolForwardsCommandAndRenders
// for the subagent variant, which takes the same schema.
func TestSubagentShellToolForwards(t *testing.T) {
	exec := &fakeShellExec{res: &agent.ShellResult{
		ExitCode: 0,
		Stdout:   "subagent ok\n",
	}}
	tool := SubagentShellTool(exec)
	res, err := tool.Handler(context.Background(), json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	if res.IsError {
		t.Errorf("expected success, got %+v", res.Content)
	}
	body := res.Content[0]
	if !strings.Contains(body, "subagent ok") {
		t.Errorf("rendered body missing stdout:\n%s", body)
	}
}
