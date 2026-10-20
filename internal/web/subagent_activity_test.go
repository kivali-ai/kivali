package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// The watcher keeps one subagentActivity for a task's whole run and
// parses only what each poll finds appended. If what it carries from
// poll to poll drifted from what the whole transcript says (a
// tool_result from an earlier poll forgotten, the newest row taken from
// an old batch), the activity line would stick on "running <tool>",
// the regression TestSubagentActivityFromTranscript records. So at
// every step a long-lived tracker must agree with a fresh one, which
// reads the transcript from the top.
func TestSubagentActivityFollowsAppends(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "alice", Role: "a", ReportsTo: "ceo"}, "role"); err != nil {
		t.Fatal(err)
	}
	path := subagentChatPath(st.Root(), "alice", "job-1")
	follow := newSubagentActivity(path)
	if got := follow.current(); got != "" {
		t.Fatalf("before the transcript exists: %q, want empty", got)
	}
	steps := []struct {
		rows []store.ChatMessage
		want string
	}{
		{[]store.ChatMessage{{Role: store.RoleReceived, Kind: "direct_chat", Content: "go"}}, ""},
		{[]store.ChatMessage{{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u1", ToolName: "run_shell"}}, "running run_shell"},
		{[]store.ChatMessage{{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "u1", Content: "ok"}}, "thinking"},
		// Two rows between polls: the tool ran and finished unseen.
		{[]store.ChatMessage{
			{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u2", ToolName: "file_view"},
			{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "u2", Content: "ok"},
		}, "thinking"},
		{[]store.ChatMessage{{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u3", ToolName: "web_fetch"}}, "running web_fetch"},
		// A result counts wherever it is in the transcript, so one read
		// in an earlier poll still finishes a tool_use that comes later.
		{[]store.ChatMessage{{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "u4", Content: "ok"}}, "thinking"},
		{[]store.ChatMessage{{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u4", ToolName: "run_shell"}}, "thinking"},
		{[]store.ChatMessage{{Role: store.RoleSent, Kind: "direct_chat", Content: "answer"}}, "responding"},
	}
	for i, s := range steps {
		for _, m := range s.rows {
			if err := st.AppendSubagentMessage("alice", "job-1", m); err != nil {
				t.Fatal(err)
			}
		}
		got, fresh := follow.current(), newSubagentActivity(path).current()
		if got != s.want || fresh != s.want {
			t.Errorf("step %d: following %q, read from the top %q; want %q", i, got, fresh, s.want)
		}
	}
}

// A running subagent has a watcher, up to six per agent, so a team of
// N agents can run 6N of them at once. At four reads a second each,
// with every read parsing the whole transcript, they kept the server
// busy and its heap churning in proportion to the team. A status line
// needs to move at most once a second, so the watcher's ticker must
// not be shorter.
func TestSubagentWatchReadsAtMostOnceASecond(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake()
	svc := &SubagentService{Provider: provider.MockProvider{}, Store: st, Clock: clk}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		svc.watchActivity("alice", "tu-1", 0, "job-1", stop)
		close(done)
	}()
	clk.BlockUntil(1)
	deadlines := clk.Deadlines()
	if len(deadlines) != 1 {
		t.Fatalf("waiters = %d, want the watcher's one ticker", len(deadlines))
	}
	if every := deadlines[0].Sub(clk.Now()); every < time.Second {
		t.Errorf("the watcher reads every %v, want at most once a second", every)
	}
	close(stop)
	<-done
}
