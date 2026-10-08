package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// fakeStateDispatcher wraps DispatchStateToolInProcess so tests can
// drive the renderer logic without spinning up an agentpod UDS server.
// Production has only the agentpod.Client-backed dispatcher; this is
// a test-only shim.
type fakeStateDispatcher struct{ deps StateDispatchDeps }

func (f fakeStateDispatcher) DispatchStateTool(_ context.Context, tool string, raw json.RawMessage) (string, bool, error) {
	body, isErr := DispatchStateToolInProcess(f.deps, tool, raw)
	return body, isErr, nil
}

// stateHandler returns a per-tool MCP handler closure for tests. slug
// binds the dispatcher to the caller; isCoS flips the read_agent_role
// gate.
func stateHandler(s *store.FSStore, slug string, isCoS bool, tool string) func(context.Context, json.RawMessage) (*ToolResult, error) {
	d := fakeStateDispatcher{deps: StateDispatchDeps{Store: s, Slug: slug, IsChiefOfStaff: isCoS}}
	return func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
		body, isErr, err := d.DispatchStateTool(ctx, tool, raw)
		if err != nil {
			return nil, err
		}
		return &ToolResult{Content: []string{body}, IsError: isErr}, nil
	}
}

func TestStateToolsExposeAllLookups(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	tools := StateTools(fakeStateDispatcher{deps: StateDispatchDeps{Store: s, Slug: "alice"}}, false)
	if len(tools) != 11 {
		t.Fatalf("want 11 state tools (non-CoS), got %d", len(tools))
	}
	names := map[string]bool{}
	for _, t := range tools {
		names[t.Name] = true
	}
	for _, want := range []string{GetOrgChartToolName, ListSkillsToolName, ReadHandbookToolName, GraphQueryToolName, GraphNodeToolName,
		AssignmentCreateToolName, AssignmentUpdateToolName, AssignmentCloseToolName, AssignmentReopenToolName, AssignmentListToolName, AssignmentViewToolName} {
		if !names[want] {
			t.Errorf("missing tool %q: have %v", want, names)
		}
	}
	if names[ReadAgentRoleToolName] {
		t.Errorf("read_agent_role should be CoS-only, but it's exposed to non-CoS")
	}

	cosTools := StateTools(fakeStateDispatcher{deps: StateDispatchDeps{Store: s, Slug: "chief-of-staff", IsChiefOfStaff: true}}, true)
	if len(cosTools) != 12 {
		t.Fatalf("want 12 state tools (CoS), got %d", len(cosTools))
	}
	cosNames := map[string]bool{}
	for _, t := range cosTools {
		cosNames[t.Name] = true
	}
	for _, want := range []string{ReadAgentRoleToolName, ReadHandbookToolName} {
		if !cosNames[want] {
			t.Errorf("%s missing for CoS: have %v", want, cosNames)
		}
	}
}

const testHandbook = "# Kivali Handbook\n\nOpening.\n\n## How the org works\n\nCharts.\n\n### Hiring chain\n\nAsk your manager.\n\n```sh\n# not a heading\n```\n\n## Behavior rules\n\nBe kind.\n"

// read_handbook returns the saved handbook under a timestamp, to the
// Chief of Staff and to any other agent alike.
func TestReadHandbookReturnsTheSavedText(t *testing.T) {
	s, _ := store.New(t.TempDir())
	if err := s.WriteHandbook(testHandbook); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []struct {
		slug  string
		isCoS bool
	}{{"chief-of-staff", true}, {"market-analyst", false}} {
		h := stateHandler(s, caller.slug, caller.isCoS, ReadHandbookToolName)
		res, err := h(context.Background(), json.RawMessage(`{}`))
		if err != nil || res.IsError {
			t.Fatalf("%s: read_handbook = %v, %v", caller.slug, res, err)
		}
		if !strings.HasPrefix(res.Content[0], "Handbook as of ") || !strings.HasSuffix(res.Content[0], testHandbook) {
			t.Errorf("%s: read_handbook = %q", caller.slug, res.Content[0])
		}
	}
}

// After an approved change the tool returns the new text.
func TestReadHandbookReadsTheCurrentFile(t *testing.T) {
	s, _ := store.New(t.TempDir())
	if err := s.WriteHandbook("# first\n"); err != nil {
		t.Fatal(err)
	}
	h := stateHandler(s, "chief-of-staff", true, ReadHandbookToolName)
	res, _ := h(context.Background(), nil)
	if res.IsError || !strings.HasSuffix(res.Content[0], "# first\n") {
		t.Errorf("first: %v", res.Content)
	}
	if err := s.WriteHandbook("# approved change\n"); err != nil {
		t.Fatal(err)
	}
	res, _ = h(context.Background(), nil)
	if res.IsError || !strings.HasSuffix(res.Content[0], "# approved change\n") {
		t.Errorf("after a change: %v", res.Content)
	}
}

func TestReadHandbookSection(t *testing.T) {
	s, _ := store.New(t.TempDir())
	if err := s.WriteHandbook(testHandbook); err != nil {
		t.Fatal(err)
	}
	h := stateHandler(s, "chief-of-staff", true, ReadHandbookToolName)
	res, _ := h(context.Background(), json.RawMessage(`{"section":"## how the org works"}`))
	if res.IsError {
		t.Fatalf("section: %v", res.Content)
	}
	want := "## How the org works\n\nCharts.\n\n### Hiring chain\n\nAsk your manager.\n\n```sh\n# not a heading\n```"
	if !strings.HasSuffix(res.Content[0], "\n\n"+want) || !strings.HasPrefix(res.Content[0], `Handbook section "how the org works" as of `) {
		t.Errorf("section = %q", res.Content[0])
	}
	res, _ = h(context.Background(), json.RawMessage(`{"section":"Hiring chain"}`))
	if res.IsError || !strings.HasSuffix(res.Content[0], "### Hiring chain\n\nAsk your manager.\n\n```sh\n# not a heading\n```") {
		t.Errorf("subsection = %q", res.Content[0])
	}
	res, _ = h(context.Background(), json.RawMessage(`{"section":"Money"}`))
	if !res.IsError || !strings.Contains(res.Content[0], "## Behavior rules") || strings.Contains(res.Content[0], "not a heading") {
		t.Errorf("missing section = %v", res.Content)
	}
}

func TestReadHandbookWithNoneSaved(t *testing.T) {
	s, _ := store.New(t.TempDir())
	h := stateHandler(s, "chief-of-staff", true, ReadHandbookToolName)
	res, _ := h(context.Background(), nil)
	if !res.IsError || !strings.Contains(res.Content[0], "no handbook") {
		t.Errorf("no handbook = %v", res.Content)
	}
}

func TestGetOrgChartReturnsTimestampedSnapshot(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS"}, "role"); err != nil {
		t.Fatalf("seed CoS: %v", err)
	}
	if err := s.CreateAgent(store.Agent{Slug: "market-analyst", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatalf("seed analyst: %v", err)
	}

	h := stateHandler(s, "", false, GetOrgChartToolName)
	res, err := h(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("should not error: %v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(res.Content))
	}
	got := res.Content[0]
	// Timestamp header is load-bearing: when this tool_result sits
	// in chat history several releases later, the agent needs to know
	// it's a snapshot, not current state.
	if !strings.Contains(got, "as of ") {
		t.Errorf("result missing timestamp header: %q", got)
	}
	if !strings.Contains(got, "market-analyst") {
		t.Errorf("result missing the seeded agent: %q", got)
	}
	if !strings.Contains(got, "chief-of-staff") {
		t.Errorf("result missing CoS: %q", got)
	}
}

func TestReadAgentRoleReturnsRole(t *testing.T) {
	s, _ := store.New(t.TempDir())
	if err := s.CreateAgent(store.Agent{Slug: "market-analyst", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# Analyst\n\nOwns market sizing.\n"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	h := stateHandler(s, "chief-of-staff", true, ReadAgentRoleToolName)
	res, err := h(context.Background(), json.RawMessage(`{"slug":"market-analyst"}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("should not error: %v", res.Content)
	}
	got := res.Content[0]
	if !strings.Contains(got, "Owns market sizing.") {
		t.Errorf("role body not returned: %q", got)
	}
	if !strings.Contains(got, "as of ") {
		t.Errorf("missing snapshot timestamp: %q", got)
	}
}

// read_agent_role: defense-in-depth runtime check. The tool list
// already excludes this tool from non-CoS agents, but the handler
// rejects non-CoS callers too, in case the lists drift.
func TestReadAgentRoleRejectsNonCoS(t *testing.T) {
	s, _ := store.New(t.TempDir())
	_ = s.CreateAgent(store.Agent{Slug: "market-analyst", Role: "Analyst"}, "# Analyst\n")
	h := stateHandler(s, "market-analyst", false, ReadAgentRoleToolName)
	res, _ := h(context.Background(), json.RawMessage(`{"slug":"market-analyst"}`))
	if !res.IsError {
		t.Fatalf("should reject non-CoS caller; got success: %v", res.Content)
	}
	if !strings.Contains(res.Content[0], "Chief of Staff") {
		t.Errorf("error should explain the gating: %q", res.Content[0])
	}
}

// read_agent_role: falls back to archived agents so CoS can review a
// fired predecessor's role when drafting a replacement.
func TestReadAgentRoleFallsBackToArchive(t *testing.T) {
	s, _ := store.New(t.TempDir())
	_ = s.CreateAgent(store.Agent{Slug: "old-analyst", Role: "Analyst"}, "# Old Analyst\n")
	if err := s.ArchiveAgent("old-analyst"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	h := stateHandler(s, "chief-of-staff", true, ReadAgentRoleToolName)
	res, err := h(context.Background(), json.RawMessage(`{"slug":"old-analyst"}`))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.IsError {
		t.Fatalf("should fall back to archive: %v", res.Content)
	}
	if !strings.Contains(res.Content[0], "ARCHIVED") {
		t.Errorf("should mark result as archived: %q", res.Content[0])
	}
	if !strings.Contains(res.Content[0], "Old Analyst") {
		t.Errorf("archived role body not returned: %q", res.Content[0])
	}
}
