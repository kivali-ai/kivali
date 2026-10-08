package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

func TestDeriveSubagentTaskViewReadsMetaAndFinalText(t *testing.T) {
	root := t.TempDir()
	parent := "alice"
	id := "abc12345"
	dir := filepath.Join(root, "agents", parent, "subagents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"),
		[]byte(`{"id":"abc12345","status":"completed","description":"draft Q3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Chat with a tool round-trip + the verbatim final answer.
	transcript := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "kickoff prompt", TS: time.Now()},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u1", ToolName: "file_view", ToolInput: `{"path":"x"}`, TS: time.Now()},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "u1", Content: "x bytes", TS: time.Now()},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "Q3 plan complete. Focus: APAC.", TS: time.Now()},
	}
	f, err := os.Create(filepath.Join(dir, "chat.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, m := range transcript {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()

	v := deriveSubagentTaskView(root, parent, id, "draft Q3")
	if v.Status != "completed" {
		t.Errorf("Status = %q, want completed", v.Status)
	}
	if v.FinalText != "Q3 plan complete. Focus: APAC." {
		t.Errorf("FinalText = %q", v.FinalText)
	}
	if v.ID != id || v.Description != "draft Q3" {
		t.Errorf("ID/Description not threaded through: %+v", v)
	}
}

func TestDeriveSubagentTaskViewMissingMetaIsUnknown(t *testing.T) {
	root := t.TempDir()
	v := deriveSubagentTaskView(root, "ghost", "noidd", "x")
	if v.Status != "unknown" {
		t.Errorf("missing meta: Status = %q, want unknown", v.Status)
	}
}

func TestSubagentTaskViewsBuildsRowsFromInputAndResult(t *testing.T) {
	srv := newTestServer(t)
	parent := "alice"
	if err := srv.Store.CreateAgent(store.Agent{Slug: parent, Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	// Seed two subagent dirs with completed status + final text.
	for _, st := range []struct {
		id, desc, final string
	}{
		{"aaaa1111", "alpha", "alpha answer"},
		{"bbbb2222", "beta", "beta answer"},
	} {
		dir := filepath.Join(srv.Store.Root(), "agents", parent, "subagents", st.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"),
			[]byte(`{"status":"completed"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "chat.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(f).Encode(store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: st.final, TS: time.Now()})
		_ = f.Close()
	}

	toolInput := `{"tasks":[{"description":"alpha","prompt":"a"},{"description":"beta","prompt":"b"}]}`
	toolResult := `Subagent batch — 2 tasks:

=== task 1: alpha ===
id: aaaa1111 · transcript: /agents/alice/subagents/aaaa1111
alpha answer

=== task 2: beta ===
id: bbbb2222 · transcript: /agents/alice/subagents/bbbb2222
beta answer
`

	views := srv.subagentTaskViews(parent, toolInput, toolResult)
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	if views[0].Description != "alpha" || views[0].FinalText != "alpha answer" || views[0].Status != "completed" || views[0].ID != "aaaa1111" {
		t.Errorf("views[0] = %+v", views[0])
	}
	if views[1].Description != "beta" || views[1].FinalText != "beta answer" || views[1].Status != "completed" || views[1].ID != "bbbb2222" {
		t.Errorf("views[1] = %+v", views[1])
	}
}

func TestSubagentTaskViewsMidFlightHasNoIDs(t *testing.T) {
	srv := newTestServer(t)
	parent := "alice"
	if err := srv.Store.CreateAgent(store.Agent{Slug: parent, Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	toolInput := `{"tasks":[{"description":"alpha","prompt":"a"},{"description":"beta","prompt":"b"}]}`
	views := srv.subagentTaskViews(parent, toolInput, "")
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	for i, v := range views {
		if v.ID != "" {
			t.Errorf("views[%d].ID = %q, want empty (no tool_result yet)", i, v.ID)
		}
		if v.Status != "running" {
			t.Errorf("views[%d].Status = %q, want running", i, v.Status)
		}
	}
}
