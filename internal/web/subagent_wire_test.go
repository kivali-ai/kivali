package web

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/store"
)

// startSubagentWireTurn publishes spec through the real DriveSubagent
// and returns the event the pod would receive, the turn state core
// installed, and the channel DriveSubagent's result lands on.
func startSubagentWireTurn(t *testing.T, srv *Server, turnID string, spec agentpod.SubagentSpec) (agentpod.Event, *agentpodTurnState, <-chan subagentTurnResult) {
	t.Helper()
	srv.AgentpodHub = NewAgentpodHub()
	sub := srv.AgentpodHub.Subscribe("alice")
	done := make(chan subagentTurnResult, 1)
	go func() {
		text, err := srv.DriveSubagent(context.Background(), "alice", turnID, spec.SubagentID, spec)
		done <- subagentTurnResult{FinalText: text, Err: err}
	}()
	ev := <-sub.ch
	st := srv.lookupAgentpodTurnByID("alice", turnID)
	if st == nil {
		t.Fatal("DriveSubagent published without installing turn state")
	}
	return ev, st, done
}

// TestSubagentSpecWireIsWhatThePodDecodes is the both-sides check for
// the core-to-pod subagent spec: the spec SubagentService builds, as
// core's publisher serialises it, decodes on the pod's side into the
// same value — with the tool set in neutral terms under "tools" and no
// CLI flag strings.
func TestSubagentSpecWireIsWhatThePodDecodes(t *testing.T) {
	captured := make(chan agentpod.SubagentSpec, 1)
	driver := &fakeSubagentDriver{respond: func(req fakeDriveCall) (string, error) {
		captured <- req.Spec
		return "ok", nil
	}}
	svc, _, _, _, del := newSubagentTestServiceWithDeliveries(t, driver)
	args, _ := json.Marshal(map[string]any{"tasks": []map[string]any{{"description": "read the log", "prompt": "summarise it"}}})
	if _, err := svc.StartBatch(context.Background(), "alice", args); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	spec := <-captured
	del.waitFor(1)
	if !reflect.DeepEqual(spec.Tools, agentpod.SubagentTools(1)) {
		t.Errorf("core built Tools = %+v, want SubagentTools(1)", spec.Tools)
	}

	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatal(err)
	}
	ev, st, done := startSubagentWireTurn(t, srv, "turn-wire", spec)
	if ev.Type != agentpod.EventChatTurn {
		t.Fatalf("published %q, want a chat-turn", ev.Type)
	}

	// The raw wire: what a pod of this build reads.
	var raw struct {
		Subagent map[string]json.RawMessage `json:"subagent"`
	}
	if err := json.Unmarshal(ev.Data, &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	toolsRaw, ok := raw.Subagent["tools"]
	if !ok {
		t.Fatalf("spec on the wire has no \"tools\": %s", ev.Data)
	}
	// The exact snake_case keys the protocol uses, and nothing else.
	var tools map[string][]string
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		t.Fatalf("decode tools: %v (%s)", err, toolsRaw)
	}
	keys := make([]string, 0, len(tools))
	for k := range tools {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"builtins", "kivali"}) {
		t.Errorf("tools keys = %v, want exactly [builtins kivali]: %s", keys, toolsRaw)
	}
	if !slices.Equal(tools["kivali"], agentpod.SubagentTools(1).Kivali) ||
		!slices.Equal(tools["builtins"], []string{"web_fetch", "web_search"}) {
		t.Errorf("tools on the wire = %s", toolsRaw)
	}
	for _, gone := range []string{"allowed_tools", "builtin_tools"} {
		if _, ok := raw.Subagent[gone]; ok {
			t.Errorf("spec on the wire still carries %q: %s", gone, ev.Data)
		}
	}
	if strings.Contains(string(ev.Data), "mcp__") {
		t.Errorf("spec on the wire spells a tool the CLI's way: %s", ev.Data)
	}

	// The pod's decode (agentpod.Runtime.handleSubagentTurn reads the
	// event data into exactly this type).
	var ct agentpod.ChatTurnEvent
	if err := json.Unmarshal(ev.Data, &ct); err != nil {
		t.Fatalf("pod decode: %v", err)
	}
	if ct.Kind != agentpod.ChatTurnKindSubagent || ct.Subagent == nil {
		t.Fatalf("pod decoded kind %q with spec %v", ct.Kind, ct.Subagent)
	}
	if !reflect.DeepEqual(*ct.Subagent, spec) {
		t.Errorf("pod decoded spec\n %+v\nwant\n %+v", *ct.Subagent, spec)
	}

	srv.onAgentpodDone(st, agentpod.TurnEvent{Kind: agentpod.TurnEventDone, Text: "ok"})
	if res := <-done; res.Err != nil || res.FinalText != "ok" {
		t.Errorf("DriveSubagent = (%q, %v), want (ok, nil)", res.FinalText, res.Err)
	}
}

// TestSubagentTypedErrorFailsTheRun: a subagent run the driver closed
// on a failed model call reaches its caller as an error carrying the
// provider's text, with the partial answer alongside — never as a
// completed run whose answer is the error text.
func TestSubagentTypedErrorFailsTheRun(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatal(err)
	}
	spec := agentpod.SubagentSpec{SubagentID: "sub-err", UserPrompt: "go", Tools: agentpod.SubagentTools(1)}
	_, st, done := startSubagentWireTurn(t, srv, "turn-err", spec)

	srv.onAgentpodError(st, agentpod.TurnEvent{Kind: agentpod.TurnEventError, Text: "API Error: 529 overloaded"})
	srv.onAgentpodFailed(st, agentpod.TurnEvent{
		Kind:         agentpod.TurnEventFailed,
		FailedReason: agentpod.FailedReasonTurnError,
		FailedDetail: "API Error: 529 overloaded",
		Text:         "half an answer",
	})
	res := <-done
	if res.Err == nil || !strings.Contains(res.Err.Error(), "API Error: 529 overloaded") {
		t.Errorf("Err = %v, want the provider's error text", res.Err)
	}
	if res.FinalText != "half an answer" {
		t.Errorf("FinalText = %q, want the partial answer", res.FinalText)
	}
}

// TestDeriveCurrentActivityBareToolName: rows the driver writes now
// carry bare names, and the activity line reads them as they are.
func TestDeriveCurrentActivityBareToolName(t *testing.T) {
	path := t.TempDir() + "/chat.jsonl"
	body, _ := json.Marshal(store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "u1", ToolName: "run_shell"})
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := deriveCurrentActivity(path); got != "running run_shell" {
		t.Errorf("activity = %q, want running run_shell", got)
	}
}
