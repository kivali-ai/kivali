package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// watchActivity reads the transcript AppendSubagentMessage writes
// (agents/<parent>/subagents/<id>/chat.jsonl) and reports what the
// subagent is doing from it. A chat.jsonl the agent could write in its
// own tree, next to the subagent's /files/ root, is not read.
func TestWatchActivityReadsTheSubagentTranscript(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "alice", Role: "a", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	// The decoy, in the agent's own tree.
	decoy := filepath.Join(files.StorageRoot(filepath.Join(st.Root(), "agents", "alice")), "subagents", "chat.jsonl")
	if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoy, []byte(`{"kind":"tool_use","tool_name":"planted","tool_use_id":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendSubagentMessage("alice", "job-1", store.ChatMessage{
		Role: store.RoleSent, Kind: "tool_use", ToolName: "run_shell", ToolUseID: "t1",
	}); err != nil {
		t.Fatal(err)
	}

	clk := clock.NewFake()
	activity := make(chan string, 4)
	svc := &SubagentService{
		Provider: provider.MockProvider{},
		Store:    st,
		Clock:    clk,
		EmitToParent: func(_, kind string, payload any) {
			if kind == "subagent_activity" {
				activity <- payload.(map[string]any)["current_activity"].(string)
			}
		},
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		svc.watchActivity("alice", "tu-1", 0, "job-1", stop)
		close(done)
	}()
	clk.BlockUntil(1)
	clk.Advance(subagentWatchPoll)
	if got := <-activity; got != "running run_shell" {
		t.Errorf("current_activity = %q, want %q", got, "running run_shell")
	}
	close(stop)
	<-done
}
