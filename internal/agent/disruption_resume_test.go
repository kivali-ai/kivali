package agent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// Boot recovery appends the runtime-disruption marker as the LAST
// entry and spawns at once (RecoverInterruptedTurns → SpawnRecovered).
// The projection defers the marker into a notice that is only emitted
// as a prefix of a LATER text user message, so with nothing after it
// the notice is never emitted and the last user message the SDK path
// feeds the CLI is the one the agent already answered. The graph
// trailer now sometimes supplies that later text; delivery of "resume
// your prior work" should not depend on whether the graph changed.
func TestDisruptionNoticeReachesTheResumePromptWhenItIsTheLastEntry(t *testing.T) {
	ctx := Context{
		ChatHistory: []store.ChatMessage{
			{Role: store.RoleReceived, Kind: "direct_chat", Content: "draft the memo"},
			{Role: store.RoleSent, Kind: "direct_chat", Content: "On it…"},
			{Role: store.RoleReceived, Kind: store.KindRuntimeDisruption, Content: "The runtime failed mid-task at an unknown point; resume your prior work."},
		},
	}
	got := ctx.chatHistoryToMessages()
	var last *provider.Message
	for i := len(got) - 1; i >= 0; i-- {
		if got[i].Role == provider.RoleUser {
			last = &got[i]
			break
		}
	}
	if last == nil {
		t.Fatalf("no user message projected: %+v", got)
	}
	text := ""
	for _, b := range last.Content {
		if b.Type == provider.ContentText {
			text += b.Text
		}
	}
	if !strings.Contains(text, "runtime failed mid-task") {
		t.Fatalf("the resume prompt the SDK path would feed is %q, not the disruption notice", text)
	}
}
