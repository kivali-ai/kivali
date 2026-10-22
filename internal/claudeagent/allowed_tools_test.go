package claudeagent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
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
// checks each registered tool is allowed. A chief of staff, an
// ordinary agent and the Kivali Developer are checked, since each gets
// a different tool set.
func TestAllowedToolNamesCoverTheFullAgentToolkit(t *testing.T) {
	be := &files.Backend{Root: t.TempDir()}
	sock := filepath.Join(t.TempDir(), "core.sock") // never dialled: only names are read
	for _, tc := range []struct {
		slug      string
		cos       bool
		developer bool
	}{
		{slug: "chief-of-staff", cos: true},
		{slug: "alice", cos: false},
		{slug: "kivali-developer", developer: true},
	} {
		tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
			Client:            agentpod.NewClient(sock, tc.slug),
			IsChiefOfStaff:    tc.cos,
			Files:             mcp.NewLocalFilesDispatcher(be),
			ShellExec:         stubShell{},
			Subagent:          stubSubagentBackend{},
			Provider:          provider.MockProvider{},
			IsKivaliDeveloper: tc.developer,
		})
		if tc.developer && !slices.ContainsFunc(tk.Tools, func(t mcp.Tool) bool { return t.Name == mcp.IntegrationPublishToolName }) {
			t.Fatal("the Kivali Developer's toolkit has no integration_publish: the fixture is not the developer's")
		}
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

// Why this test exists: Kivali OS (internal/agent/kivali_os.md) is
// where an agent learns what its tools are for, and it ships in the
// binary so that it tracks the code. A tool the toolkit registers but
// the document never names is one agents never reach for; a name the
// document gives that no tool answers to sends them after nothing.
// The full toolkit is built here, with every optional tool on, so the
// check runs against the real set rather than a list kept by hand.
func TestKivaliOSNamesEveryRegisteredTool(t *testing.T) {
	be := &files.Backend{Root: t.TempDir()}
	sock := filepath.Join(t.TempDir(), "core.sock")
	tk := mcp.FullAgentToolkit(mcp.FullAgentDeps{
		Client:            agentpod.NewClient(sock, "chief-of-staff"),
		IsChiefOfStaff:    true,
		Files:             mcp.NewLocalFilesDispatcher(be),
		ShellExec:         stubShell{},
		Subagent:          stubSubagentBackend{},
		Provider:          provider.MockProvider{},
		IsKivaliDeveloper: true,
	})
	have := map[string]bool{}
	for _, tool := range tk.Tools {
		have[tool.Name] = true
	}
	if len(have) < 30 {
		t.Fatalf("the toolkit has %d tools; the fixture is not the full set", len(have))
	}
	// A family the document names with a trailing star covers every
	// tool with that prefix.
	families := []string{"agent_memory_habits_", "agent_memory_", "file_", "publish_", "propose_", "assignment_"}
	named := map[string]bool{}
	for _, m := range regexp.MustCompile("`([A-Za-z_*]+)`").FindAllStringSubmatch(agent.KivaliOS, -1) {
		named[m[1]] = true
	}
	covered := func(name string) bool {
		if named[name] {
			return true
		}
		for _, f := range families {
			if named[f+"*"] && strings.HasPrefix(name, f) {
				return true
			}
		}
		return false
	}
	// Described elsewhere, on purpose: read_agent_role is the Chief of
	// Staff's own workflow, in cos_role.md, and the integration_* tools
	// the Kivali Developer's, in its role
	// (internal/seed/kivali_developer_role.md).
	elsewhere := map[string]bool{"read_agent_role": true}
	for _, name := range mcp.IntegrationEngineerToolNames {
		elsewhere[name] = true
	}
	for name := range have {
		if !covered(name) && !elsewhere[name] {
			t.Errorf("Kivali OS never names the tool %s", name)
		}
	}
	// A name shaped like a tool's that no tool answers to. submit_result
	// is a subagent's own tool, which the document explains to the
	// agent that reads its result.
	prefixes := []string{"publish_", "propose_", "assignment_", "file_", "subagent", "graph_", "agent_memory", "artifact_", "read_", "list_", "search_", "get_", "share_", "run_shell"}
	notTools := map[string]bool{"submit_result": true}
	for name := range named {
		shaped := false
		for _, p := range prefixes {
			shaped = shaped || strings.HasPrefix(name, p)
		}
		if !shaped || have[name] || notTools[name] {
			continue
		}
		if strings.HasSuffix(name, "*") && slices.Contains(families, strings.TrimSuffix(name, "*")) {
			continue
		}
		t.Errorf("Kivali OS names `%s`, which is not a tool an agent has", name)
	}
}
