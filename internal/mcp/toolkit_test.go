package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
)

// fakeBackend is a stand-in for the controlclient-based backend used
// in production. Tests inject it to verify the MCP handler is a pure
// proxy and that toolkit registration honors a non-nil Subagent dep.
type fakeBackend struct{}

func (fakeBackend) RunSubagent(_ context.Context, _ string, _ json.RawMessage) (string, error) {
	return "ok", nil
}

func (fakeBackend) SubagentStatus(_ context.Context, _ string) (string, error) {
	return "no background tasks", nil
}

func (fakeBackend) SubagentCancel(_ context.Context, _, _ string) (string, error) {
	return "cancelled", nil
}

// newTestClient returns an agentpod.Client wired to a non-existent
// socket path. The toolkit composer only reads the client's slug
// during registration; no UDS calls are made until a tool is actually
// dispatched, so this is enough for tool-list assertions.
func newTestClient(slug string) *agentpod.Client {
	return agentpod.NewClient("/tmp/kivali-toolkit-test.sock", slug)
}

// TestSubagentToolkitOmitsParentTools is the load-bearing assertion of
// the whole subagent-toolkit design: a subagent MCP server MUST NOT
// expose publish_*, agent_memory_*, share_file, search_past_chats,
// list_project_files, or the org-state tools beyond list_skills and
// the two read-only graph lookups.
//
// If this test starts failing, a tool slipped into the wrong toolkit
// — the subagent boundary is what stops the subagent from reaching
// into the parent's identity and state.
func TestSubagentToolkitOmitsParentTools(t *testing.T) {
	be := &files.Backend{Root: t.TempDir()}

	tk := SubagentToolkit(SubagentDeps{
		Client:     newTestClient("alice"),
		SubagentID: "st-1",
		Files:      NewLocalFilesDispatcher(be),
		// ShellExec nil → run_shell stays out (verified separately).
	})

	got := map[string]bool{}
	for _, t := range tk.Tools {
		got[t.Name] = true
	}

	mustHave := []string{
		"file_view", "file_create", "file_str_replace",
		"file_insert", "file_delete", "file_rename", "file_copy",
		"list_skills",
		// Read-only graph lookups: public by definition, and a subagent
		// handed an object needs to find what binds it.
		"graph_query", "graph_node",
		// Publishing into the parent's area: the published tree is
		// read-only to file_*, so this is how a subagent's deliverable
		// reaches the org.
		"artifact_publish", "artifact_unpublish",
	}
	for _, n := range mustHave {
		if !got[n] {
			t.Errorf("subagent toolkit missing %q", n)
		}
	}

	mustNotHave := []string{
		"publish_task_request",
		"publish_status_update",
		"publish_ceo_approval_request",
		"publish_ceo_notification",
		"propose_role_update",
		"agent_memory_view",
		"agent_memory_append",
		"agent_memory_str_replace",
		"share_file",
		"search_past_chats",
		"list_project_files",
		"get_org_chart",
		"get_pending_work",
		"redact_pending_request",
		"read_agent_role",
		"subagent",
		// The tracker is the parent's: a subagent is one bounded job
		// inside an assignment, never a party to it.
		"assignment_create", "assignment_update", "assignment_close",
		"assignment_reopen", "assignment_list", "assignment_view",
	}
	for _, n := range mustNotHave {
		if got[n] {
			t.Errorf("subagent toolkit must NOT expose %q", n)
		}
	}
}

func TestFullAgentToolkitIncludesSubagentWhenRunnerProvided(t *testing.T) {
	be := &files.Backend{Root: t.TempDir()}

	withRunner := FullAgentToolkit(FullAgentDeps{
		Client:   newTestClient("alice"),
		Files:    NewLocalFilesDispatcher(be),
		Subagent: fakeBackend{},
		Provider: provider.MockProvider{},
	})
	if !hasTool(withRunner.Tools, "subagent") {
		t.Error("FullAgentToolkit with Subagent should expose `subagent`")
	}
	for _, name := range []string{"artifact_publish", "artifact_unpublish"} {
		if !hasTool(withRunner.Tools, name) {
			t.Errorf("FullAgentToolkit should expose %s", name)
		}
	}

	withoutRunner := FullAgentToolkit(FullAgentDeps{
		Client: newTestClient("alice"),
		Files:  NewLocalFilesDispatcher(be),
	})
	if hasTool(withoutRunner.Tools, "subagent") {
		t.Error("FullAgentToolkit without Subagent should NOT expose `subagent`")
	}
}

func hasTool(tools []Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
