// Package agent serialization contract tests.
//
// This file is the CENTRAL test suite for the "how agent context is
// serialized before we call the model" contract. It's the go-to
// place to answer:
//
//   - What goes in the system prompt, in what order, and what does
//     NOT belong there?
//   - How is chat.jsonl projected into req.Messages? (role mapping,
//     bucket-transition flushes, doc_published drop, attachment
//     inlining, file order = model order)
//   - How is a mid-flight delivery reordered so UI, file, and model
//     all agree?
//   - Where are the cache breakpoints, and why?
//
// See docs/developers/context-serialization.md for the full invariant catalog.
// Per-package tests (web/chat, release, claudeagent) cover BEHAVIORAL
// choreography (spawning, race recovery, transport dispatch) that
// builds on top of this contract; those tests live with their code.
// What lives here is pure-data: given a Context / a []ChatMessage,
// does the output match the invariants?
package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// ---- helpers ----------------------------------------------------------

func makeCtx(hist []store.ChatMessage) Context {
	return Context{
		Agent:                  store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"},
		Handbook:               "# HANDBOOK",
		Role:                   "# ROLE",
		AgentMemory:            "# MEMORY",
		ChatHistory:            hist,
		FilesystemAvailable:    true,
		IncludeFilesystemTools: true,
		Model:                  "claude-opus-4-7",
		MaxTokens:              32768,
	}
}

func msg(role, kind, content string, when time.Time) store.ChatMessage {
	return store.ChatMessage{Role: role, Kind: kind, Content: content, TS: when}
}

// A turn that dies mid-tool leaves a tool_use with no tool_result. The
// next text the runtime appends (a disruption notice, the knowledge
// graph trailer, a CEO redirect) must not follow the tool_use directly:
// the API requires every tool_use to be answered in the very next user
// message. The projection closes the gap with a synthetic error result.
func TestDanglingToolUseIsClosedBeforeTheNextText(t *testing.T) {
	base := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	hist := []store.ChatMessage{
		msg(store.RoleReceived, "direct_chat", "go", base),
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t1", ToolName: "file_view", ToolInput: `{}`, TS: base.Add(time.Second)},
		msg(store.RoleReceived, store.KindRuntimeDisruption, "died", base.Add(2*time.Second)),
		msg(store.RoleReceived, store.KindWakeUpdate, "Update from the runtime at this wake ...", base.Add(3*time.Second)),
	}
	msgs := Context{ChatHistory: hist}.chatHistoryToMessages()
	for i, m := range msgs {
		for _, c := range m.Content {
			if c.Type != provider.ContentToolUse {
				continue
			}
			if i+1 >= len(msgs) {
				t.Fatalf("tool_use %s is the last thing in the projection", c.ToolUseID)
			}
			answered := false
			for _, n := range msgs[i+1].Content {
				if n.Type == provider.ContentToolResult && n.ToolResultID == c.ToolUseID {
					answered = true
				}
			}
			if !answered {
				t.Errorf("tool_use %s is not answered by the next message: %+v", c.ToolUseID, msgs[i+1])
			}
		}
	}
	// The disruption notice and the wake note still reach the model, as
	// text, after the synthetic result.
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleUser || len(last.Content) != 1 || last.Content[0].Type != provider.ContentText ||
		!strings.Contains(last.Content[0].Text, "[died]") || !strings.Contains(last.Content[0].Text, "Update from the runtime at this wake") {
		t.Errorf("last message = %+v", last)
	}
	// A history whose tool_use IS answered gets no synthetic result.
	clean := []store.ChatMessage{
		msg(store.RoleReceived, "direct_chat", "go", base),
		{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t1", ToolName: "file_view", ToolInput: `{}`, TS: base.Add(time.Second)},
		{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "t1", Content: "ok", TS: base.Add(2 * time.Second)},
		msg(store.RoleReceived, "direct_chat", "next", base.Add(3*time.Second)),
	}
	cleanMsgs := Context{ChatHistory: clean}.chatHistoryToMessages()
	for _, m := range cleanMsgs {
		for _, c := range m.Content {
			if c.Type == provider.ContentToolResult && strings.Contains(c.ToolResultContent, "interrupted") {
				t.Errorf("synthetic result emitted for an answered tool_use: %+v", c)
			}
		}
	}
}

// ---- 1. System prompt composition ------------------------------------

// TestSystemPromptComposition locks in the rule: the system prompt
// carries ONLY invariants (handbook, filesystem help, agent
// persona). It must NOT contain mutable state — those moved to MCP
// tools (get_org_chart, assignment_list) so the model reads them
// as snapshots-at-lookup-time instead of invariant background facts.
func TestSystemPromptComposition(t *testing.T) {
	ctx := makeCtx(nil)
	req := ctx.BuildRequest()

	if len(req.System) < 4 {
		t.Fatalf("want 4+ system blocks, got %d", len(req.System))
	}

	// Block 0 = handbook.
	if !strings.Contains(req.System[0].Text, "HANDBOOK") {
		t.Errorf("block 0 should carry the handbook, got %q", req.System[0].Text)
	}
	// Block 1 = the owner section: what the person is called and their
	// markup marker, rendered from the stored name each turn. The one
	// live line in the system prompt, on purpose: a rename must reach
	// every agent's next turn (the runner respawns on prompt drift).
	if !strings.HasPrefix(req.System[1].Text, "## The owner\n") {
		t.Errorf("block 1 should carry the owner section, got %q", req.System[1].Text)
	}
	// Block 2 = filesystem (file_*) help (only when IncludeFilesystemTools set).
	if !strings.Contains(req.System[2].Text, "/files/") {
		t.Errorf("block 2 should carry filesystem help, got %q", req.System[2].Text)
	}
	// Final block = persona (role + agent_memory, cached).
	persona := req.System[len(req.System)-1]
	if !strings.Contains(persona.Text, "ROLE") {
		t.Errorf("persona block missing role; got %q", persona.Text)
	}
	if !strings.Contains(persona.Text, "MEMORY") {
		t.Errorf("persona block missing agent_memory; got %q", persona.Text)
	}
	if !persona.Cache {
		t.Errorf("persona block must have Cache=true — it's a stable identity block that pays the prompt-cache dividend on every call")
	}
}

// TestSystemPromptExcludesMutableState is the other half of the
// composition rule: anything the model should see as "as-of now"
// (org chart, pending work, project files, skills listing) lives on
// MCP tools, NOT in the system prompt, because the model reads system
// blocks as invariant background and misses within-chat state changes.
//
// Pending work is absent from the fixture because Context has no such
// field: open work reaches agents through assignment_list.
func TestSystemPromptExcludesMutableState(t *testing.T) {
	ctx := Context{
		Agent:                  store.Agent{Slug: "alice", Role: "Analyst"},
		Handbook:               "# C",
		Role:                   "# R",
		AgentMemory:            "# M",
		OrgChart:               "MUTABLE-ORG-CHART-STRING",
		ProjectFiles:           []store.ProjectFile{{OriginalName: "mutable-file.md", SHA: "deadbeef"}},
		Skills:                 []store.Skill{{Name: "mutable-skill"}},
		FilesystemAvailable:    true,
		IncludeFilesystemTools: true,
	}
	req := ctx.BuildRequest()

	full := ""
	for _, b := range req.System {
		full += b.Text + "\n"
	}
	forbidden := []string{
		"MUTABLE-ORG-CHART-STRING",
		"mutable-file.md",
		"mutable-skill",
	}
	for _, f := range forbidden {
		if strings.Contains(full, f) {
			t.Errorf("system prompt leaked mutable-state string %q — it must come from an MCP tool snapshot, not an invariant system block", f)
		}
	}
}

// ---- 2. Chat history projection ---------------------------------------

// TestChatProjectionRoleMapping locks in the role mapping:
// RoleReceived→RoleUser, RoleSent→RoleAssistant. Every downstream
// assumption (req.Messages shape, --resume selection, bucket flush)
// depends on this being exact.
func TestChatProjectionRoleMapping(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	hist := []store.ChatMessage{
		msg(store.RoleReceived, "direct_chat", "hi", base),
		msg(store.RoleSent, "direct_chat", "hello", base.Add(time.Second)),
	}
	ctx := makeCtx(hist)
	req := ctx.BuildRequest()
	if len(req.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != provider.RoleUser {
		t.Errorf("received → want RoleUser, got %q", req.Messages[0].Role)
	}
	if req.Messages[1].Role != provider.RoleAssistant {
		t.Errorf("sent → want RoleAssistant, got %q", req.Messages[1].Role)
	}
}

// TestChatProjectionBucketFlushToolThenText verifies the user-side
// bucket-transition flush: a user-role text (inbox_delivery,
// direct_chat, rotation_prompt) following a user-role tool_result
// must emit a fresh Message, not pack into the same user release.
// Without this split, the model reads the new directive as
// commentary on the tool result.
func TestChatProjectionBucketFlushToolThenText(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	hist := []store.ChatMessage{
		msg(store.RoleReceived, "direct_chat", "q1", base),
		msg(store.RoleSent, "tool_use", "", base.Add(1*time.Second)),
		msg(store.RoleReceived, "tool_result", "result-body", base.Add(2*time.Second)),
		msg(store.RoleReceived, "inbox_delivery", "new-directive", base.Add(3*time.Second)),
		msg(store.RoleSent, "direct_chat", "reply", base.Add(4*time.Second)),
	}
	hist[1].ToolUseID = "toolu_1"
	hist[1].ToolName = "publish_x"
	hist[1].ToolInput = `{"x":1}`
	hist[2].ToolUseID = "toolu_1"

	ctx := makeCtx(hist)
	req := ctx.BuildRequest()

	// Expect: user(text q1), assistant(tool_use), user(tool_result),
	// user(text new-directive), assistant(text reply) — FIVE
	// messages, with the fourth carrying the mid-flow directive as
	// its own user release. If the tool_result and new-directive had
	// been packed together, we'd only see four messages.
	if len(req.Messages) != 5 {
		t.Fatalf("bucket flush regression: want 5 messages, got %d: %+v", len(req.Messages), req.Messages)
	}
	// user-text → assistant-tool_use → user-tool_result →
	// user-text → assistant-text
	wantRoles := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleUser, provider.RoleUser, provider.RoleAssistant}
	for i, want := range wantRoles {
		if req.Messages[i].Role != want {
			t.Errorf("Messages[%d].Role = %q, want %q", i, req.Messages[i].Role, want)
		}
	}
	// Message 2 is the tool_result, Message 3 is the new user directive —
	// NOT packed together.
	if req.Messages[2].Content[0].Type != provider.ContentToolResult {
		t.Errorf("Messages[2] should carry the tool_result: %+v", req.Messages[2])
	}
	if req.Messages[3].Content[0].Type != provider.ContentText ||
		!strings.Contains(req.Messages[3].Content[0].Text, "new-directive") {
		t.Errorf("Messages[3] should carry the new user-text directive, got %+v", req.Messages[3])
	}
}

// TestChatProjectionDropsDocPublished locks in that doc_published
// chat entries (UI decorations that persist across refresh) do NOT
// project into req.Messages. They carry no info beyond the paired
// tool_result and including them would waste tokens on every call.
func TestChatProjectionDropsDocPublished(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	hist := []store.ChatMessage{
		msg(store.RoleReceived, "direct_chat", "ask", base),
		msg(store.RoleSent, "tool_use", "", base.Add(time.Second)),
		msg(store.RoleReceived, "tool_result", "ack", base.Add(2*time.Second)),
		// doc_published is a persistence record, not model context.
		msg(store.RoleSent, "doc_published", "📄 published something", base.Add(3*time.Second)),
		msg(store.RoleSent, "direct_chat", "done", base.Add(4*time.Second)),
	}
	hist[1].ToolUseID = "toolu_1"
	hist[1].ToolName = "publish_x"
	hist[1].ToolInput = `{}`
	hist[2].ToolUseID = "toolu_1"

	ctx := makeCtx(hist)
	req := ctx.BuildRequest()
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Text, "📄") {
				t.Errorf("doc_published leaked into req.Messages: %q — it is a UI decoration, not model context", c.Text)
			}
		}
	}
}

// TestChatProjectionPreservesFileOrder is the UI = model invariant
// from the user side: file order of chat.jsonl is the order
// req.Messages reflects. The buffering architecture
// (web.deliverOrBuffer + tearDownAndFlush) keeps chat.jsonl strictly
// append-ordered, so a follow-up's view of "what happened" matches
// the user's view byte-for-byte. This test verifies no silent
// in-memory reorder is happening at projection time.
func TestChatProjectionPreservesFileOrder(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	// Intentionally out-of-TS order: the FILE order is what the
	// model must see, not a TS-sorted order. File order is the
	// canonical "what happened next" signal in chat.jsonl.
	hist := []store.ChatMessage{
		msg(store.RoleReceived, "direct_chat", "FIRST", base.Add(10*time.Second)), // later TS, but first in file
		msg(store.RoleSent, "direct_chat", "SECOND", base.Add(1*time.Second)),     // earlier TS, but second in file
	}
	ctx := makeCtx(hist)
	req := ctx.BuildRequest()
	if len(req.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[0].Content[0].Text != "FIRST" {
		t.Errorf("Messages[0] text = %q, want FIRST — projection must follow file order, not re-sort by TS", req.Messages[0].Content[0].Text)
	}
	if req.Messages[1].Content[0].Text != "SECOND" {
		t.Errorf("Messages[1] text = %q, want SECOND", req.Messages[1].Content[0].Text)
	}
}

// ---- 3a. Strictly-append-ordered chat.jsonl invariant ----------------

// TestFileOrderIsStrictlyAppendCanonical is the top-level ordering
// invariant: chat.jsonl is append-only — every write adds an entry
// to the tail, and file order IS conversation order. The web layer
// enforces this by holding externally-arriving deliveries in a
// per-agent buffer (pendingDeliveries) while a response loop is
// running, then flushing them to the tail at loop-end. Nothing ever
// gets inserted between existing entries; nothing ever gets moved.
//
// This test acts as the central documentation for that guarantee —
// the full behavior is exercised at the web layer, but the rule is
// named and tested here so anyone reading "how does context
// serialization work?" finds it next to the projection contract.
//
// If you need to verify the mechanism itself, see
// internal/web/chat_test.go TestChatLoopBuffersDeliveriesDuringLoop
// and TestChatLoopBuffersMultipleDeliveriesInArrivalOrder.
func TestFileOrderIsStrictlyAppendCanonical(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	// Simulate a chat that went through the buffering discipline:
	// loop responded to q1 (producing tool flow + reply), then two
	// deliveries that arrived mid-flight flushed in arrival order
	// AFTER the reply.
	hist := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "q1", TS: base.Add(0)},
		{Role: store.RoleSent, Kind: "tool_use", Content: "call", TS: base.Add(1 * time.Second), ToolUseID: "toolu_1", ToolName: "x", ToolInput: `{}`},
		{Role: store.RoleReceived, Kind: "tool_result", Content: "ack", TS: base.Add(1 * time.Second), ToolUseID: "toolu_1"},
		{Role: store.RoleSent, Kind: "direct_chat", Content: "reply-1", TS: base.Add(2 * time.Second)},
		// These two arrived mid-flight, were buffered, and flushed here.
		// TS may predate reply-1 (they arrived earlier, reply landed later)
		// but they appear AFTER reply-1 in file order — that's the
		// canonical invariant.
		{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "q2", TS: base.Add(500 * time.Millisecond)},
		{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "q3", TS: base.Add(1500 * time.Millisecond)},
	}
	ctx := makeCtx(hist)
	req := ctx.BuildRequest()

	// Every real received entry projects in its file position, not
	// re-sorted by TS.
	var order []string
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.Type == provider.ContentText {
				order = append(order, c.Text)
			}
		}
	}
	// Expected text order reflects file order: q1, reply-1, q2, q3.
	// (tool_use + tool_result don't have text blocks; they're
	// structured content and show up separately.)
	wantSubstrings := []string{"q1", "reply-1", "q2", "q3"}
	if len(order) < len(wantSubstrings) {
		t.Fatalf("got %d text blocks, want at least %d: %+v", len(order), len(wantSubstrings), order)
	}
	for i, want := range wantSubstrings {
		if !strings.Contains(order[i], want) {
			t.Errorf("text[%d] = %q, want substring %q (file order must be preserved)", i, order[i], want)
		}
	}
}

// ---- 4. LastRealReceivedTS -------------------------------------------

// TestLastRealReceivedTSSkipsToolPlumbing exercises the shared helper
// that every race-detection path uses to answer "what's the latest
// real input we've seen?" Must skip tool_use / tool_result /
// doc_published since those are the agent's own in-flight thinking.
func TestLastRealReceivedTSSkipsToolPlumbing(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	ts := func(offset int) time.Time { return base.Add(time.Duration(offset) * time.Second) }
	hist := []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", Content: "q", TS: ts(0)},
		{Role: store.RoleSent, Kind: "tool_use", TS: ts(5)},
		{Role: store.RoleReceived, Kind: "tool_result", TS: ts(6)},     // newer but plumbing
		{Role: store.RoleSent, Kind: "doc_published", TS: ts(7)},       // not received
		{Role: store.RoleReceived, Kind: "inbox_delivery", TS: ts(10)}, // THIS is the answer
		{Role: store.RoleSent, Kind: "direct_chat", TS: ts(20)},        // sent, not received
	}
	got := store.LastRealReceivedTS(hist)
	want := ts(10)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v (latest real received TS — not tool_result, not doc_published, not sent)", got, want)
	}
}

// TestLastRealReceivedTSEmpty covers the "no received at all" case —
// e.g. a fresh chat after rotation has no entries. Zero time means
// "no input to respond to yet"; callers treat it as "do nothing."
func TestLastRealReceivedTSEmpty(t *testing.T) {
	if got := store.LastRealReceivedTS(nil); !got.IsZero() {
		t.Errorf("empty hist got %v, want zero", got)
	}
	sentOnly := []store.ChatMessage{
		{Role: store.RoleSent, Kind: "direct_chat", TS: time.Unix(1, 0)},
	}
	if got := store.LastRealReceivedTS(sentOnly); !got.IsZero() {
		t.Errorf("sent-only hist got %v, want zero", got)
	}
}

// ---- 5. Cache breakpoints --------------------------------------------

// TestCacheBreakpointOnLastTool ensures we mark the final tool as
// Cache=true so every call after the first reads the full tool
// catalogue from prompt cache. If this regresses, every release pays
// full input-rate on the tool definitions.
func TestCacheBreakpointOnLastTool(t *testing.T) {
	ctx := makeCtx(nil)
	ctx.IncludeShellTool = true
	req := ctx.BuildRequest()
	if len(req.Tools) == 0 {
		t.Fatal("expected tools to be registered")
	}
	for i, tool := range req.Tools {
		if i == len(req.Tools)-1 {
			if !tool.Cache {
				t.Errorf("last tool %q must have Cache=true", tool.Name)
			}
		} else {
			if tool.Cache {
				t.Errorf("middle tool %q has Cache=true; only the last should", tool.Name)
			}
		}
	}
}

// TestCacheBreakpointOnLastMessage: within a tool loop, each iter's
// prior conversation is served from cache (5-min ephemeral TTL).
// The last block of the last message carries the breakpoint.
func TestCacheBreakpointOnLastMessage(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	hist := []store.ChatMessage{
		msg(store.RoleReceived, "direct_chat", "hi", base),
	}
	ctx := makeCtx(hist)
	req := ctx.BuildRequest()
	if len(req.Messages) == 0 {
		t.Fatal("expected at least one message")
	}
	last := req.Messages[len(req.Messages)-1]
	if len(last.Content) == 0 {
		t.Fatal("last message has no content blocks")
	}
	if !last.Content[len(last.Content)-1].Cache {
		t.Errorf("final block of last message must have Cache=true for tool-loop cache reuse")
	}
}
