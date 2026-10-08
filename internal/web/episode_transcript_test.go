package web

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// The rotation prompt is dropped from the digest input because it is
// the same boilerplate in every generation. Its ANSWER is the same
// boilerplate: agent_memory_view, then a run of appends and
// str_replaces, then a closing line about the memory being in shape.
// Keeping it is worse than keeping the prompt, because the shortener
// gives the tail 70% of the budget — so in a long chat the reconcile
// bookkeeping crowds out the work the episode is supposed to be about,
// and Haiku is asked to digest a chat whose visible ending is an agent
// editing its own memory. Every episode then risks reading "the agent
// reconciled its memory", which is exactly the failed,
// could-describe-a-hundred-chats title the prompt forbids.
//
// So the whole agent_memory_* family is dropped, not just the prompt.
// After the mid-chat append allowance was withdrawn (memory is
// rewritten only at rotation) every agent_memory_* call in a
// transcript IS reconcile bookkeeping, so the rule needs no window.
func TestRenderEpisodeTranscriptDropsMemoryReconcileTraffic(t *testing.T) {
	msgs := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "why is the nightly egress job failing?"},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t1", ToolName: "run_shell", ToolInput: `{"command":"kubectl logs egress-0"}`},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "t1", Content: "dial tcp 10.0.0.9:443: i/o timeout"},
		{Role: store.RoleSent, Content: "The egress pod cannot reach the proxy; the NetworkPolicy drops 443."},
		{Role: store.RoleReceived, Kind: "rotation_prompt", Content: "The CEO has requested a chat rotation. Reconcile your memory."},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t2", ToolName: "agent_memory_view", ToolInput: `{}`},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "t2", Content: "the egress proxy lives at 10.0.0.9 [[stated]]"},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t3", ToolName: "agent_memory_habits_append", ToolInput: `{"text":"Check NetworkPolicy before blaming DNS."}`},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "t3", Content: "habits updated"},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t4", ToolName: "agent_memory_str_replace", ToolInput: `{"old_str":"x","new_str":"y"}`},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "t4", Content: "agent memory updated"},
		{Role: store.RoleSent, Content: "Memory is in the shape I want future-me to see."},
	}
	got := renderEpisodeTranscript(msgs)

	for _, gone := range []string{
		"agent_memory_view",
		"agent_memory_habits_append",
		"agent_memory_str_replace",
		"habits updated",
		"agent memory updated",
		"[[stated]]",
		"requested a chat rotation",
	} {
		if strings.Contains(got, gone) {
			t.Errorf("reconcile bookkeeping %q survived into the digest input:\n%s", gone, got)
		}
	}
	// The actual work — and only it — must still be there.
	for _, want := range []string{
		"why is the nightly egress job failing?",
		"kubectl logs egress-0",
		"i/o timeout",
		"NetworkPolicy drops 443",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dropped real work %q from the digest input:\n%s", want, got)
		}
	}
}

// A tool_result is dropped by pairing with its tool_use, so a
// non-memory result is never collateral damage and a memory result is
// never orphaned into the transcript by an unpaired row.
func TestRenderEpisodeTranscriptKeepsNonMemoryToolResults(t *testing.T) {
	msgs := []store.ChatMessage{
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "m1", ToolName: "agent_memory_view", ToolInput: `{}`},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "m1", Content: "MEMORY BODY should not appear"},
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "f1", ToolName: "file_view", ToolInput: `{"path":"/files/project/plan.md"}`},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "f1", Content: "PLAN BODY should appear"},
	}
	got := renderEpisodeTranscript(msgs)
	if strings.Contains(got, "MEMORY BODY") {
		t.Errorf("agent_memory_view result leaked into the digest input:\n%s", got)
	}
	if !strings.Contains(got, "PLAN BODY") {
		t.Errorf("file_view result was dropped along with the memory traffic:\n%s", got)
	}
}

// fallbackEpisodeTitle labels a digest whose model reply carried no
// title. It must not reach for the rotation prompt or the reconcile
// chatter around it — both are identical in every generation, so a
// fallback that picked them would produce the same title for every
// episode an agent ever has.
func TestFallbackEpisodeTitleSkipsRotationBoilerplate(t *testing.T) {
	msgs := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "rotation_prompt", Content: "The CEO has requested a chat rotation."},
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "budget for the Q3 hosting bill\nsecond line"},
	}
	if got := fallbackEpisodeTitle(msgs, "20260921T000000.000000000Z"); got != "budget for the Q3 hosting bill" {
		t.Errorf("fallbackEpisodeTitle = %q, want the first real user line", got)
	}
	empty := fallbackEpisodeTitle(nil, "20260921T000000.000000000Z")
	if !strings.Contains(empty, "20260921T000000.000000000Z") {
		t.Errorf("fallbackEpisodeTitle with no messages = %q, want the generation stamp", empty)
	}
}
