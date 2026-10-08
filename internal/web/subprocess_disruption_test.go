package web

import (
	"errors"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestFinalizeSubprocessDisruptionWritesEntry: when the chat loop
// reports the underlying
// CLI subprocess died, finalizeSubprocessDisruption writes a
// kind:runtime-disruption row whose Content carries the cause and
// whose presence flips SpawnDecision to SpawnNow on the next inbound.
//
// This is the live-CLI-death counterpart to RecoverInterruptedTurns
// (which handles whole-pod death at boot). Same Kind, same downstream
// behavior; the row just lands inline rather than at boot.
func TestFinalizeSubprocessDisruptionWritesEntry(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	// Seed a tool_use without a tool_result — the orphan state we'd
	// see if the CLI died after emitting tool_use_end but before the
	// tool result came back. SpawnDecision would otherwise return
	// SpawnIdle here (the most-recent non-tool entry doesn't exist
	// yet, so the walk reaches no-meaningful-entries).
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleReceived, Kind: "direct_chat", Content: "hi",
	}); err != nil {
		t.Fatalf("seed inbound: %v", err)
	}
	if err := srv.Store.AppendChatMessage("alice", store.ChatMessage{
		Role: store.RoleSent, Kind: "tool_use",
		ToolUseID: "toolu_orphan", ToolName: "run_shell", ToolInput: `{"command":"true"}`,
		Content: "run_shell {...}",
	}); err != nil {
		t.Fatalf("seed orphan tool_use: %v", err)
	}

	cause := errors.New("claudeagent: runner[alice]: " + provider.ErrSubprocessExited.Error() + " (cause: signal: killed)")
	hub := &chatHub{hub: newHub(), slug: "alice"}
	srv.finalizeSubprocessDisruption("alice", hub, cause)

	hist, err := srv.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	last := hist[len(hist)-1]
	if last.Kind != store.KindRuntimeDisruption {
		t.Fatalf("last kind = %q, want %q", last.Kind, store.KindRuntimeDisruption)
	}
	if last.Role != store.RoleReceived {
		t.Errorf("last role = %q, want %q", last.Role, store.RoleReceived)
	}
	if !strings.Contains(last.Content, "subprocess died") {
		t.Errorf("disruption content does not name the cause: %q", last.Content)
	}
	// The whole point: SpawnDecision now flips to SpawnNow because of
	// the disruption marker, so the next inbound (CEO message,
	// release-all wake) re-engages the agent. Without our fix this
	// would be SpawnIdle and the chat would stay stuck.
	if got := store.SpawnDecision(hist); got != store.SpawnNow {
		t.Errorf("SpawnDecision after disruption write = %v, want SpawnNow", got)
	}

	// Live UI parity with the on-disk row: a chat_marker SSE event
	// must broadcast on the hub so a browser holding /stream open
	// paints the divider immediately. Without this emit, the agent's
	// chat shows nothing until reload — same class of bug as the Stop
	// click silently writing to disk.
	payload, ok := hubEventByKind(hub, "chat_marker")
	if !ok {
		t.Fatalf("no chat_marker emit on hub after subprocess disruption; replay = %v", hubEventKinds(hub))
	}
	if payload["kind"] != store.KindRuntimeDisruption {
		t.Errorf("chat_marker.kind = %v, want %q", payload["kind"], store.KindRuntimeDisruption)
	}
	if got, _ := payload["content"].(string); !strings.Contains(got, "subprocess died") {
		t.Errorf("chat_marker.content does not name the cause: %q", got)
	}
}

// TestFinalizeSubprocessDisruptionNilHubIsSafe: callers without a
// live hub (boot recovery, stale-failed path) pass nil so the disk
// write still happens but no SSE emit fires. This was the original
// contract; locking it in so a future refactor that tightens the
// signature still allows hub-less use.
func TestFinalizeSubprocessDisruptionNilHubIsSafe(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "k"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	srv.finalizeSubprocessDisruption("alice", nil, errors.New("boot recovery"))

	hist, _ := srv.Store.ReadChatHistory("alice")
	if len(hist) == 0 || hist[len(hist)-1].Kind != store.KindRuntimeDisruption {
		t.Errorf("disruption row not written with nil hub; hist = %+v", hist)
	}
}
