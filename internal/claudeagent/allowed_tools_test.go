package claudeagent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// stubSubagentBackend turns on the subagent tools in the full-agent
// toolkit. No handler runs: the test reads tool names only.
type stubSubagentBackend struct{}

func (stubSubagentBackend) RunSubagent(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}
func (stubSubagentBackend) SubagentStatus(context.Context, string) (string, error) { return "", nil }
func (stubSubagentBackend) SubagentCancel(context.Context, string, string) (string, error) {
	return "", nil
}
func (stubSubagentBackend) WebSessions(context.Context, string) (string, error) { return "", nil }
func (stubSubagentBackend) WebSessionReset(context.Context, string, string) (string, error) {
	return "", nil
}

// stubShell turns on run_shell in the full-agent toolkit.
type stubShell struct{}

func (stubShell) Exec(context.Context, string, agent.ShellRequest) (*agent.ShellResult, error) {
	return &agent.ShellResult{}, nil
}
func (stubShell) SyncSkills(context.Context, string) error { return nil }

// Why this test exists: the claude CLI refuses any MCP tool that is not
// in --allowedTools, answering the model with "permission not granted",
// even though the kivali MCP server registered the tool and the model
// was shown its schema. The two lists are kept by hand in different
// packages (mcp.FullAgentToolkit and allowedToolNames here), so a tool
// added to one and not the other ships as a tool the agent can see but
// never call. subagent_web_sessions and subagent_web_session_reset
// shipped that way. TestSubagentToolsCoverTheSubagentToolkit guards the
// same pair of lists for subagents; this guards them for agents.
//
// It builds the real toolkit with every optional tool switched on and
// the real request an agent sends (agent.Context.BuildRequest), and
// checks each registered tool is allowed. Both a chief of staff and an
// ordinary agent are checked, since each gets a different tool set.
func TestAllowedToolNamesCoverTheFullAgentToolkit(t *testing.T) {
	be := &files.Backend{Root: t.TempDir()}
	sock := filepath.Join(t.TempDir(), "core.sock") // never dialled: only names are read
	for _, tc := range []struct {
		slug string
		cos  bool
	}{
		{slug: "chief-of-staff", cos: true},
		{slug: "alice", cos: false},
	} {
		tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
			Client:         agentpod.NewClient(sock, tc.slug),
			IsChiefOfStaff: tc.cos,
			Files:          mcp.NewLocalFilesDispatcher(be),
			ShellExec:      stubShell{},
			Subagent:       stubSubagentBackend{},
			Provider:       provider.MockProvider{},
		})
		req := agent.Context{
			Agent:                  store.Agent{Slug: tc.slug},
			IsChiefOfStaff:         tc.cos,
			IncludeShellTool:       true,
			IncludeShareFileTool:   true,
			IncludeFilesystemTools: true,
			FilesystemAvailable:    true,
		}.BuildRequest()

		allowed := map[string]bool{}
		for _, n := range allowedToolNames(req.Tools) {
			allowed[n] = true
		}
		for _, tool := range tk.Tools {
			if !allowed[cliToolName(tool.Name)] {
				t.Errorf("%s: the MCP server registers %q but --allowedTools omits it, so the CLI refuses every call to it", tc.slug, tool.Name)
			}
		}
	}
}
