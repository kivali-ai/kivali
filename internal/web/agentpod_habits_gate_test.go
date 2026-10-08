package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestHabitsWritesAreRotationOnly pins the one gate on habits, at the
// one chokepoint every habits write crosses:
// the pod's MCP tool → agentpod.Client.MemoryDispatch → core's
// memory/dispatch endpoint. Outside a rotation the write is refused
// as an ordinary tool error the model can act on; reads, and every
// semantic-memory verb, stay open; during the rotation turn the same
// call lands.
func TestHabitsWritesAreRotationOnly(t *testing.T) {
	path, srv, cleanup := startStateServer(t)
	defer cleanup()
	c := agentpod.NewClient(path, "alice")
	ctx := context.Background()

	appendIn, _ := json.Marshal(map[string]string{"text": "- Verify before claiming. Why: a wrong fact is worse than none."})
	replaceIn, _ := json.Marshal(map[string]string{"old_str": "Verify before claiming", "new_str": "Verify end-to-end before claiming"})

	// Mid-chat: the two writes are refused; the view and the memory
	// tools are not.
	for _, tool := range []string{agent.AgentHabitsAppendToolName, agent.AgentHabitsStrReplaceToolName} {
		in := appendIn
		if tool == agent.AgentHabitsStrReplaceToolName {
			in = replaceIn
		}
		resp, err := c.MemoryDispatch(ctx, tool, in)
		if err != nil {
			t.Fatalf("%s: transport error: %v", tool, err)
		}
		if !resp.IsError || !strings.Contains(resp.Body, "chat rotation") || !strings.Contains(resp.Body, "rotation prompt") {
			t.Fatalf("%s outside rotation = (%q, isError=%v), want a refusal that points at the rotation prompt", tool, resp.Body, resp.IsError)
		}
		// The refusal must not steer the agent into a mid-chat memory
		// write: that costs a runtime restart and a full-conversation
		// cache miss for a fact the rotation reads out of the chat.
		if strings.Contains(resp.Body, "agent_memory_append") {
			t.Fatalf("%s refusal suggests a mid-chat memory write: %q", tool, resp.Body)
		}
	}
	if _, err := srv.Store.ReadAgentHabits("alice"); err == nil {
		t.Fatal("a refused write still reached agent_memory_habits.md")
	}
	view, err := c.MemoryDispatch(ctx, agent.AgentHabitsViewToolName, json.RawMessage(`{}`))
	if err != nil || view.IsError || !strings.Contains(view.Body, "empty") {
		t.Fatalf("habits view outside rotation = (%+v, %v), want the empty-document text", view, err)
	}
	memIn, _ := json.Marshal(map[string]string{"text": "v3 ships on Friday [[stated]]"})
	mem, err := c.MemoryDispatch(ctx, agent.AgentMemoryAppendToolName, memIn)
	if err != nil || mem.IsError {
		t.Fatalf("agent_memory_append outside rotation = (%+v, %v), want success — semantic memory is not gated", mem, err)
	}

	// The rotation turn: the same calls land, and the archive snapshot
	// taken at request time would carry the pre-rotation file.
	if err := srv.Store.WritePendingRotation("alice", store.PendingRotation{RequestedBy: "ceo", Timestamp: store.NewArchiveTimestamp()}); err != nil {
		t.Fatalf("mark rotation: %v", err)
	}
	resp, err := c.MemoryDispatch(ctx, agent.AgentHabitsAppendToolName, appendIn)
	if err != nil || resp.IsError {
		t.Fatalf("habits append during rotation = (%+v, %v), want success", resp, err)
	}
	resp, err = c.MemoryDispatch(ctx, agent.AgentHabitsStrReplaceToolName, replaceIn)
	if err != nil || resp.IsError {
		t.Fatalf("habits str_replace during rotation = (%+v, %v), want success", resp, err)
	}
	got, err := srv.Store.ReadAgentHabits("alice")
	if err != nil || !strings.Contains(got, "Verify end-to-end before claiming. Why:") {
		t.Fatalf("agent_memory_habits.md = (%q, %v)", got, err)
	}
	view, err = c.MemoryDispatch(ctx, agent.AgentHabitsViewToolName, json.RawMessage(`{}`))
	if err != nil || view.IsError || view.Body != got {
		t.Fatalf("habits view = (%+v, %v), want the file body", view, err)
	}
}
