package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestRotationMintsEpisodeIdAndArchivesUnderIt pins the citation
// contract between the reconcile turn and the episode writer: the
// generation name the agent is told to cite as [[ep:<ts>]] during the
// turn is the directory the transcript is archived under afterwards —
// so a citation made before the episode exists resolves once the
// writer lands it. It also pins that the principles file is
// snapshotted beside the memory snapshot, and that a principles write
// during the rotation turn reaches disk.
func TestRotationMintsEpisodeIdAndArchivesUnderIt(t *testing.T) {
	srv, _ := newTurnServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := srv.Store.WriteAgentMemory("alice", "# facts\n- prod runs v0.12 [[stated]]\n"); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	const priorHabits = "- Old rule. Why: it used to matter.\n"
	if err := srv.Store.WriteAgentHabits("alice", priorHabits); err != nil {
		t.Fatalf("write principles: %v", err)
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Content: "how's Q3?"},
		{Role: store.RoleSent, Content: "Q3 is on track"},
	} {
		if err := srv.Store.AppendChatMessage("alice", m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	principleIn, _ := json.Marshal(map[string]string{"text": "- Verify end-to-end before claiming a fix. Why: the Release fix that wasn't."})
	fake := installFakeAgentPod(t, srv, "alice")
	fake.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent {
		return memoryToolDispatchResponse(srv, "alice", agent.AgentHabitsAppendToolName, "tu-rotate-1", principleIn)
	})

	rr := postNewChat(t, srv, "alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && srv.ChatHubActive("alice") {
		time.Sleep(5 * time.Millisecond)
	}

	gens, err := srv.Store.ListArchivedChats("alice")
	if err != nil || len(gens) != 1 {
		t.Fatalf("generations = %+v, %v; want exactly one", gens, err)
	}
	ts := gens[0].Timestamp

	arch, err := srv.Store.ReadArchivedChat("alice", ts)
	if err != nil {
		t.Fatalf("read archived: %v", err)
	}
	var prompt string
	for _, m := range arch {
		if m.Kind == "rotation_prompt" {
			prompt = m.Content
		}
	}
	if prompt == "" {
		t.Fatal("archive has no rotation_prompt entry")
	}
	if !strings.Contains(prompt, "[[ep:"+ts+"]]") {
		t.Fatalf("the reconcile prompt cited a different generation than the one archived (%s):\n%s", ts, prompt)
	}
	if strings.Contains(prompt, "{{ts}}") {
		t.Fatal("reconcile prompt still carries the {{ts}} placeholder")
	}

	snap, err := srv.Store.ReadArchivedAgentHabits("alice", ts)
	if err != nil || snap != priorHabits {
		t.Fatalf("principles snapshot = (%q, %v), want the pre-rotation file", snap, err)
	}
	live, err := srv.Store.ReadAgentHabits("alice")
	if err != nil || !strings.Contains(live, "Verify end-to-end") || !strings.Contains(live, "Old rule") {
		t.Fatalf("live principles = (%q, %v), want old rule kept and new rule appended", live, err)
	}
	if _, ok, _ := srv.Store.ReadPendingRotation("alice"); ok {
		t.Fatal("rotation marker not cleared after finalize")
	}
}
