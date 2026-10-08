package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/message"
	"github.com/kivali-ai/kivali/internal/owner"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

func TestBuildRequestSystemLayers(t *testing.T) {
	// The system prompt carries only invariants: handbook, agent
	// persona (role.md + agent_memory). Mutating state — org chart,
	// pending work, project files, skills — lives on on-demand MCP
	// tools (get_org_chart, assignment_list) or filesystem listings
	// (file_view /files/project/).
	// Expected layers: handbook, persona (2 when the file_* tools
	// are off). With them enabled, the filesystem help block also
	// appears for a total of 3.
	ctx := Context{
		Agent:          store.Agent{Slug: "cos", Role: "Chief of Staff", ReportsTo: "ceo"},
		IsChiefOfStaff: true,
		Handbook:       "# Handbook",
		Role:           "# Role",
		AgentMemory:    "notes",
		Model:          "claude-opus-4-7",
		MaxTokens:      4096,
		Purpose:        "turn",
	}
	req := ctx.BuildRequest()

	if req.Model != "claude-opus-4-7" || req.Agent != "cos" {
		t.Errorf("req meta: %+v", req)
	}
	if len(req.System) != 3 {
		t.Fatalf("system layers = %d, want 3 (handbook + owner + persona)", len(req.System))
	}
	// The zero Owner is a work team with no name chosen.
	if req.System[1].Text != owner.For("", "").Section() || req.System[1].Cache {
		t.Errorf("system[1] = %q, want the owner section with no cache marker", req.System[1].Text)
	}
	if !strings.Contains(req.System[0].Text, "Handbook") {
		t.Errorf("system[0] = %q", req.System[0].Text)
	}
	if req.System[0].Cache {
		t.Error("handbook should not carry a cache marker (subsumed by persona breakpoint)")
	}
	if !strings.Contains(req.System[2].Text, "You are cos") {
		t.Errorf("system[2] = %q", req.System[2].Text)
	}
	if !strings.Contains(req.System[2].Text, "Agent memory") {
		t.Errorf("agent memory not included: %q", req.System[2].Text)
	}
	if !req.System[2].Cache {
		t.Error("persona (role + agent_memory) should be the per-agent cache breakpoint")
	}
	// No more org-chart / project-files / pending-work blocks in the
	// system prompt. Those are fetched on demand via MCP tools.
	for i, b := range req.System {
		if strings.Contains(b.Text, "## Current org chart") {
			t.Errorf("system[%d] still contains org-chart block — should be moved to get_org_chart tool", i)
		}
		if strings.Contains(b.Text, "## Your pending work") {
			t.Errorf("system[%d] still contains pending-work block — open work belongs to the assignment_list tool", i)
		}
		if strings.Contains(b.Text, "## Project files") {
			t.Errorf("system[%d] still contains project-files listing — agents can file_view /files/project/ for this", i)
		}
	}
}

func TestBuildRequestCacheBreakpointBudget(t *testing.T) {
	// Anthropic caps cache_control markers at 4 per request. Worst
	// case: FilesystemAvailable + Skills + tools + messages. Must still
	// total ≤ 4.
	ctx := Context{
		Agent:                  store.Agent{Slug: "cos", Role: "Chief of Staff", ReportsTo: "ceo"},
		IsChiefOfStaff:         true,
		Handbook:               "# c",
		OrgChart:               "- ceo",
		ProjectFiles:           []store.ProjectFile{{OriginalName: "p.md", OriginalExt: ".md"}},
		Skills:                 []store.Skill{{Name: "s", Description: "d"}},
		Role:                   "# k",
		AgentMemory:            "m",
		FilesystemAvailable:    true,
		IncludeFilesystemTools: true,
		IncludeShellTool:       true,
		ChatHistory:            []store.ChatMessage{{Role: store.RoleReceived, Content: "hi"}},
		Model:                  "claude-opus-4-7",
	}
	req := ctx.BuildRequest()

	n := 0
	for _, sb := range req.System {
		if sb.Cache {
			n++
		}
	}
	for _, tl := range req.Tools {
		if tl.Cache {
			n++
		}
	}
	for _, msg := range req.Messages {
		for _, b := range msg.Content {
			if b.Cache {
				n++
			}
		}
	}
	if n > 4 {
		t.Fatalf("cache breakpoints = %d, Anthropic caps at 4", n)
	}
}

func TestBuildRequestToolSetByRole(t *testing.T) {
	base := Context{
		Agent:    store.Agent{Slug: "x", Role: "r"},
		Handbook: "c",
		Role:     "k",
		Model:    "m",
	}
	// Tool set, minus memory / shell (they're gated separately):
	//   publish_notice, publish_ceo_approval_request,
	//   publish_ceo_notification                                (=3 from message.AllTools)
	//   + get_org_chart, search_past_chats, list_project_files,
	//     list_skills, read_handbook                            (=5 from StateTools)
	//   + graph_query, graph_node                               (=2 from GraphTools)
	//   + assignment_create, assignment_update, assignment_close,
	//     assignment_reopen, assignment_list, assignment_view   (=6 from AssignmentTools)
	// → 16 tools for non-CoS. CoS additionally sees the five
	// org-mutating proposals (propose_hire, propose_offboard,
	// propose_reorg, propose_role_update, propose_handbook_update)
	// and read_agent_role → 22 tools.
	const wantNonCoS = 16
	const wantCoS = 22
	cosCtx := base
	cosCtx.IsChiefOfStaff = true
	cosTools := cosCtx.BuildRequest().Tools
	if len(cosTools) != wantCoS {
		t.Errorf("CoS should see %d tools, got %d", wantCoS, len(cosTools))
	}
	nonCoS := base
	nonCoS.IsChiefOfStaff = false
	tools := nonCoS.BuildRequest().Tools
	if len(tools) != wantNonCoS {
		t.Errorf("non-CoS should see %d tools, got %d", wantNonCoS, len(tools))
	}
	cosToolNames := map[string]bool{}
	for _, tt := range cosTools {
		cosToolNames[tt.Name] = true
	}
	nonCoSToolNames := map[string]bool{}
	for _, tt := range tools {
		nonCoSToolNames[tt.Name] = true
	}
	for _, want := range []string{message.ToolProposeRoleUpdate, message.ToolProposeReorg, message.ToolProposeHire, message.ToolProposeOffboard, message.ToolProposeHandbookUpdate, ReadAgentRoleToolName} {
		if !cosToolNames[want] {
			t.Errorf("CoS missing %q: have %v", want, cosToolNames)
		}
		if nonCoSToolNames[want] {
			t.Errorf("non-CoS should not see %q: have %v", want, nonCoSToolNames)
		}
	}
	// State-lookup and assignment tools are on every agent's call.
	for _, want := range []string{GetOrgChartToolName, ReadHandbookToolName, AssignmentListToolName, AssignmentCreateToolName} {
		var sawCoS, sawOther bool
		for _, t := range cosTools {
			if t.Name == want {
				sawCoS = true
			}
		}
		for _, t := range tools {
			if t.Name == want {
				sawOther = true
			}
		}
		if !sawCoS || !sawOther {
			t.Errorf("state tool %q missing: CoS=%v other=%v", want, sawCoS, sawOther)
		}
	}
}

func TestBuildRequestMessages(t *testing.T) {
	ctx := Context{
		Agent:    store.Agent{Slug: "alice", Role: "r"},
		Handbook: "c", OrgChart: "o", Role: "k",
		ChatHistory: []store.ChatMessage{
			{Role: store.RoleReceived, Content: "hi"},
			{Role: store.RoleSent, Content: "hello"},
		},
		Inbox: []store.Message{
			{Type: store.MsgNotice, Title: "Draft", From: "cos", To: store.Recipients{"alice"}, Body: "please do"},
		},
	}
	req := ctx.BuildRequest()
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (2 history + 1 inbox)", len(req.Messages))
	}
	if req.Messages[0].Role != provider.RoleUser || req.Messages[0].Content[0].Text != "hi" {
		t.Errorf("history[0] = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != provider.RoleAssistant || req.Messages[1].Content[0].Text != "hello" {
		t.Errorf("history[1] = %+v", req.Messages[1])
	}
	inboxMsg := req.Messages[2]
	if inboxMsg.Role != provider.RoleUser {
		t.Errorf("inbox role = %q", inboxMsg.Role)
	}
	if !strings.Contains(inboxMsg.Content[0].Text, `cos sent you a notice titled "Draft"`) {
		t.Errorf("inbox message body missing natural-language lead: %q", inboxMsg.Content[0].Text)
	}
}

// The model-facing inbox render is a natural-language paraphrase
// generated from the structured Message fields — "X sent you a Y
// titled 'Z'. Their message follows." — rather than the structured
// machine-shaped `[INBOX …]` preamble. The UI bubble renders
// from the structured View, so the on-disk fields remain the
// authoritative record; only the model-facing projection is
// narrated. The lock-ins below pin the exact shape so a future
// refactor that drops path refs or decision verbs will fail loudly.

// TestRenderInboxBodyAssignmentEventNamesTheAssignmentAndForbidsReply: the
// tracker's wake reads as a report about an assignment, names the actor,
// and points at the assignment tools rather than a reply. No "(at <path>)"
// — nothing may point at an assignment event.
func TestRenderInboxBodyAssignmentEventNamesTheAssignmentAndForbidsReply(t *testing.T) {
	d := store.Message{
		Type: store.MsgAssignmentEvent, Title: "#42 assigned to you: Draft Q3 analysis", From: "chief-of-staff", To: store.Recipients{"analyst"},
		Assignment: &store.AssignmentRef{ID: 42, Seq: 7, Op: "created"},
		Body:       "chief-of-staff assigned you #42 \"Draft Q3 analysis\".\nIt is ready: nothing blocks it.\n\nplease do the thing\n",
	}
	got := RenderInboxBody(d, "messages/2026-04-18/x-assignment_event-chief-of-staff--to--analyst.md")
	for _, w := range []string{
		"The assignment tracker reports a change to assignment #42, made by chief-of-staff.",
		"Act on the assignment itself with the assignment_* tools; nothing is owed back on this message.",
		"chief-of-staff assigned you #42",
		"please do the thing",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("render missing %q in: %s", w, got)
		}
	}
	if strings.Contains(got, "(at messages/") {
		t.Errorf("an assignment event must not offer a path to reply to: %s", got)
	}
	if strings.Contains(got, "[INBOX") {
		t.Errorf("[INBOX ...] preamble in render: %s", got)
	}
}

func TestRenderInboxBodyCEOApprovedEmptyBodyDefaults(t *testing.T) {
	approved := true
	d := store.Message{
		Type:      store.MsgCEOApprovalResponse,
		Title:     "Approved: Ask X?",
		From:      "ceo",
		To:        store.Recipients{"chief-scientist"},
		Approved:  &approved,
		InReplyTo: "messages/2026-04-18/req.md",
	}
	got := RenderInboxBody(d, "messages/2026-04-18/resp.md")
	// Reported-speech frame: neutral "replied ... and said:" lead,
	// decision foregrounded in the quoted body as a first-person
	// statement. The original title (extracted from "Approved: X"
	// → "X") correlates back to the published ask.
	for _, w := range []string{
		`The owner replied to your approval request titled "Ask X?"`,
		"at messages/2026-04-18/req.md",
		"and said:",
		"I approved your request.", // decision in the quote, always present
	} {
		if !strings.Contains(got, w) {
			t.Errorf("render missing %q in: %s", w, got)
		}
	}
}

// When the CEO approves a hire, the quoted speech
// must include the provisioning facts — slug, role, reports_to —
// and that the agent is now live. With reported-speech framing
// ("The CEO replied and said: …") the decision and side-effect
// both live inside the CEO's "voice," so the recipient doesn't
// need handbook-level guardrails to interpret an approval as
// "the hire is done."
func TestRenderInboxBodyApprovedHireStatesAgentIsLive(t *testing.T) {
	approved := true
	d := store.Message{
		Type:     store.MsgCEOApprovalResponse,
		Title:    "Approved: Approve hire: Chief Scientist (test)",
		From:     "ceo",
		To:       store.Recipients{"chief-of-staff"},
		Approved: &approved,
		Hire: &store.Hire{
			Slug:      "chief-scientist",
			Role:      "Chief Scientist",
			ReportsTo: "ceo",
		},
		InReplyTo: "messages/2026-04-18/req.md",
	}
	got := RenderInboxBody(d, "messages/2026-04-18/resp.md")
	for _, w := range []string{
		`The owner replied to your approval request titled "Approve hire: Chief Scientist (test)"`,
		"at messages/2026-04-18/req.md",
		"and said:",
		"I approved your hire of Chief Scientist",
		"slug `chief-scientist`",
		"reports to `ceo`",
		"The agent has been provisioned and is now live.",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("render missing %q in: %s", w, got)
		}
	}
	// Should NOT contain the defensive language from the prior
	// iteration — the reported-speech frame is supposed to make
	// that unnecessary.
	for _, nope := range []string{
		"Do NOT draft another role",
		"not a slug collision",
		"Current org chart",
	} {
		if strings.Contains(got, nope) {
			t.Errorf("unexpected defensive language %q leaked into render: %s", nope, got)
		}
	}
}

// Denial with a custom message: the CEO's typed message is appended
// as a second paragraph after the "I denied your request." speech
// act, so the model sees both the decision and any elaborating
// text from the CEO without having to infer from tone.
func TestRenderInboxBodyCEODeniedWithCustomMessageAppended(t *testing.T) {
	approved := false
	d := store.Message{
		Type:      store.MsgCEOApprovalResponse,
		Title:     "Denied: Spend $X?",
		From:      "ceo",
		To:        store.Recipients{"cos"},
		Approved:  &approved,
		InReplyTo: "messages/2026-04-18/req.md",
		Body:      "not this quarter — revisit Q4",
	}
	got := RenderInboxBody(d, "messages/2026-04-18/resp.md")
	for _, w := range []string{
		"The owner replied to your approval request",
		`titled "Spend $X?"`,
		"and said:",
		"I denied your request.",
		"not this quarter — revisit Q4",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("render missing %q in: %s", w, got)
		}
	}
	// Decision must come BEFORE the custom note, so the model
	// sees the outcome first and then any elaboration.
	idxDecision := strings.Index(got, "I denied your request.")
	idxCustom := strings.Index(got, "not this quarter")
	if idxDecision < 0 || idxCustom < 0 || idxDecision > idxCustom {
		t.Errorf("decision speech must precede the custom message: %s", got)
	}
}

func TestRenderInboxBodyNotificationAckEmptyBody(t *testing.T) {
	d := store.Message{
		Type:      store.MsgCEONotificationAck,
		Title:     "Acknowledged: FYI X",
		From:      "ceo",
		To:        store.Recipients{"cos"},
		InReplyTo: "messages/2026-04-18/note.md",
	}
	got := RenderInboxBody(d, "messages/2026-04-18/ack.md")
	for _, w := range []string{
		"The owner replied to your notification",
		`titled "FYI X"`,
		"at messages/2026-04-18/note.md",
		"and said:",
		"I acknowledge your notification.",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("render missing %q in: %s", w, got)
		}
	}
}

func TestRenderInboxBodyAttachmentAdvertisement(t *testing.T) {
	d := store.Message{
		Type: store.MsgNotice, Title: "T", From: "a", To: store.Recipients{"b"}, Body: "hi",
		Attachments: []store.MessageAttachment{
			{Name: "brief.md", SHA: "x"},
			{Name: "data.csv", SHA: "y"},
		},
	}
	got := RenderInboxBody(d, "")
	if !strings.Contains(got, "2 attachments: brief.md, data.csv") {
		t.Errorf("attachment advertisement missing: %s", got)
	}

	d.Attachments = []store.MessageAttachment{{Name: "only.md", SHA: "z"}}
	got = RenderInboxBody(d, "")
	if !strings.Contains(got, "1 attachment: only.md") {
		t.Errorf("single-attachment advertisement missing: %s", got)
	}
}

func TestRenderInboxBodyHasNoMachinePreamble(t *testing.T) {
	// A machine-shaped "[INBOX …]" preamble is text models
	// confabulate against; the render is natural language across the
	// common types.
	cases := []store.Message{
		{Type: store.MsgNotice, Title: "T", From: "a", To: store.Recipients{"b"}, Body: "hi"},
		{Type: store.MsgCEOApprovalResponse, Title: "Approved: T", From: "ceo", To: store.Recipients{"b"}, Body: "ok"},
		{Type: store.MsgCEONotificationAck, Title: "Acknowledged: T", From: "ceo", To: store.Recipients{"b"}},
	}
	for _, c := range cases {
		got := RenderInboxBody(c, "messages/2026-04-18/x.md")
		if strings.Contains(got, "[INBOX") {
			t.Errorf("type=%s: [INBOX preamble present in %q", c.Type, got)
		}
	}
}

func TestOrgChartMarkdown(t *testing.T) {
	agents := []store.Agent{
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
		{Slug: "market-analyst", Role: "Market Analyst", ReportsTo: "chief-of-staff"},
		{Slug: "engineer-1", Role: "Engineer", ReportsTo: "chief-of-staff"},
	}
	got := OrgChartMarkdown(agents, "CEO")
	for _, want := range []string{
		"- ceo (CEO, human)",
		"  - chief-of-staff (Chief of Staff)",
		"    - market-analyst (Market Analyst)",
		"    - engineer-1 (Engineer)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing line %q in:\n%s", want, got)
		}
	}
}

// TestOrgChartMarkdownHandlesCEOPseudoAgent guards against infinite
// recursion when the ceo pseudo-agent is included in ListActiveAgents()
// and has an empty reports_to. Without the guard, writeChildren recurses
// into children of ceo, which includes ceo itself, blowing the heap.
func TestOrgChartMarkdownHandlesCEOPseudoAgent(t *testing.T) {
	agents := []store.Agent{
		{Slug: "ceo", Role: "CEO", ReportsTo: ""},
		{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"},
	}
	// If recursion is unbounded, this call will hang / OOM. A normal run
	// completes in microseconds.
	done := make(chan string, 1)
	go func() { done <- OrgChartMarkdown(agents, "CEO") }()
	select {
	case got := <-done:
		if !strings.Contains(got, "chief-of-staff") {
			t.Errorf("missing CoS: %s", got)
		}
		// Should not contain "ceo" as a child of itself.
		if strings.Count(got, "- ceo (") > 1 {
			t.Errorf("ceo appears more than once: %s", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("OrgChartMarkdown hung — probably infinite recursion")
	}
}

func TestOrgChartMarkdownOrphans(t *testing.T) {
	agents := []store.Agent{
		{Slug: "chief-of-staff", Role: "CoS", ReportsTo: "ceo"},
		{Slug: "dangling", Role: "D", ReportsTo: "ghost"},
	}
	got := OrgChartMarkdown(agents, "CEO")
	if !strings.Contains(got, "Unattached") || !strings.Contains(got, "dangling") {
		t.Errorf("orphans section missing:\n%s", got)
	}
}

// fakeAttachmentReader is a minimal stub that returns canned text for a
// handful of SHAs; good enough to drive the inlining code paths without
// pulling in a real store. AttachmentHasTextSidecar always returns
// false — the tests using this fake exercise the no-filesystem
// inline-text branch where sidecars don't apply.
type fakeAttachmentReader map[string]string

func (f fakeAttachmentReader) ReadAttachmentText(sha string) (string, error) {
	return f[sha], nil
}

func (f fakeAttachmentReader) AttachmentHasTextSidecar(string) bool { return false }

// AttachmentLinkNames returns no mapping, so the render path falls
// back to the sender's filename. Tests that care about collision
// disambiguation use linkNameReader instead.
func (f fakeAttachmentReader) AttachmentLinkNames(string) (map[string]string, error) {
	return nil, nil
}

// linkNameReader is a fakeAttachmentReader that also knows what each
// SHA was actually written as under /files/attachments/ — the thing
// the delivery header has to quote when two blobs share a filename.
type linkNameReader struct {
	fakeAttachmentReader
	links    map[string]string
	sidecars map[string]bool
}

func (l linkNameReader) AttachmentHasTextSidecar(sha string) bool { return l.sidecars[sha] }

func (l linkNameReader) AttachmentLinkNames(string) (map[string]string, error) {
	return l.links, nil
}

func TestInboxDeliveryInlinesAttachmentText(t *testing.T) {
	ctx := Context{
		Agent:    store.Agent{Slug: "bob", Role: "r"},
		Handbook: "c", OrgChart: "o", Role: "k",
		Inbox: []store.Message{{
			Type: store.MsgNotice, Title: "T", From: "alice", To: store.Recipients{"bob"}, Body: "body",
			Attachments: []store.MessageAttachment{{SHA: "xxx", Name: "plan.md"}},
		}},
		Attachments: fakeAttachmentReader{"xxx": "# Plan body"},
	}
	req := ctx.BuildRequest()
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	body := req.Messages[0].Content[0].Text
	if !strings.Contains(body, "1 attachment: plan.md") {
		t.Errorf("attachment list not in inbox body: %q", body)
	}
	if !strings.Contains(body, "# Plan body") {
		t.Errorf("attachment text not inlined: %q", body)
	}
}

func TestBuildRequestMarksCacheBreakpoints(t *testing.T) {
	ctx := Context{
		Agent:    store.Agent{Slug: "x", Role: "r"},
		Handbook: "c", OrgChart: "o", Role: "k",
		ChatHistory: []store.ChatMessage{
			{Role: store.RoleReceived, Content: "hi"},
		},
	}
	req := ctx.BuildRequest()
	// Last tool should be marked for caching.
	if n := len(req.Tools); n == 0 || !req.Tools[n-1].Cache {
		t.Errorf("last tool should have Cache=true; tools=%d", n)
	}
	// Last message's last block should be marked for caching.
	if n := len(req.Messages); n > 0 {
		blocks := req.Messages[n-1].Content
		if len(blocks) == 0 || !blocks[len(blocks)-1].Cache {
			t.Error("last message's last block should have Cache=true")
		}
	}
	// Earlier tools / blocks should NOT be marked (avoids wasting
	// cache breakpoints, max 4 per request).
	if len(req.Tools) >= 2 && req.Tools[0].Cache {
		t.Error("only the last tool should be cached")
	}
}

func TestChatHistoryAttachmentsInlinedForReceivedOnly(t *testing.T) {
	ctx := Context{
		Agent:    store.Agent{Slug: "bob", Role: "r"},
		Handbook: "c", OrgChart: "o", Role: "k",
		ChatHistory: []store.ChatMessage{
			{Role: store.RoleReceived, Content: "see attached",
				Attachments: []store.MessageAttachment{{SHA: "rx", Name: "brief.md"}}},
			{Role: store.RoleSent, Content: "thanks",
				Attachments: []store.MessageAttachment{{SHA: "tx", Name: "reply.md"}}},
		},
		Attachments: fakeAttachmentReader{"rx": "RECEIVED_TEXT", "tx": "SENT_TEXT"},
	}
	req := ctx.BuildRequest()
	if got := req.Messages[0].Content[0].Text; !strings.Contains(got, "RECEIVED_TEXT") {
		t.Errorf("received attachment text should inline: %q", got)
	}
	if got := req.Messages[1].Content[0].Text; strings.Contains(got, "SENT_TEXT") {
		t.Errorf("sent attachment text should NOT inline (author already sent it): %q", got)
	}
}

// TestChatHistoryToMessagesStructure locks in the structured
// conversion from flat chat.jsonl entries to Anthropic-shaped
// messages: consecutive same-role entries collapse, tool_use /
// tool_result stay as typed content blocks (not text narration),
// and doc_published decorations are excluded.
//
// Regressions here cascade into every Claude call — the model
// would see narrated tool history instead of structured primitives.
func TestChatHistoryToMessagesStructure(t *testing.T) {
	t.Run("empty history yields no messages", func(t *testing.T) {
		ctx := Context{}
		if got := ctx.chatHistoryToMessages(); len(got) != 0 {
			t.Errorf("empty history produced %d messages: %+v", len(got), got)
		}
	})

	t.Run("single user message wraps as one text block", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "hi"},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 1 || got[0].Role != provider.RoleUser {
			t.Fatalf("got %+v", got)
		}
		if len(got[0].Content) != 1 || got[0].Content[0].Type != provider.ContentText || got[0].Content[0].Text != "hi" {
			t.Errorf("unexpected content: %+v", got[0].Content)
		}
	})

	t.Run("consecutive same-role entries collapse into one message", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleSent, Kind: "direct_chat", Content: "I'll check"},
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_1", ToolName: "file_view", ToolInput: `{"path":"/files/"}`},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 1 {
			t.Fatalf("expected 1 grouped message, got %d: %+v", len(got), got)
		}
		if got[0].Role != provider.RoleAssistant {
			t.Errorf("grouped role = %q, want assistant", got[0].Role)
		}
		if len(got[0].Content) != 2 {
			t.Fatalf("grouped content blocks = %d, want 2", len(got[0].Content))
		}
		if got[0].Content[0].Type != provider.ContentText || got[0].Content[0].Text != "I'll check" {
			t.Errorf("block[0] = %+v", got[0].Content[0])
		}
		if got[0].Content[1].Type != provider.ContentToolUse {
			t.Errorf("block[1] type = %q, want tool_use", got[0].Content[1].Type)
		}
		if got[0].Content[1].ToolUseID != "tu_1" || got[0].Content[1].ToolName != "file_view" {
			t.Errorf("block[1] id/name = %+v", got[0].Content[1])
		}
		if string(got[0].Content[1].ToolInput) != `{"path":"/files/"}` {
			t.Errorf("block[1] input = %s", got[0].Content[1].ToolInput)
		}
	})

	t.Run("role change flushes the group", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "hi"},
				{Role: store.RoleSent, Kind: "direct_chat", Content: "hello"},
				{Role: store.RoleSent, Kind: "direct_chat", Content: "more"},
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "follow-up"},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 3 {
			t.Fatalf("expected 3 messages (user, assistant x2 grouped, user), got %d: %+v", len(got), got)
		}
		if got[0].Role != provider.RoleUser || got[1].Role != provider.RoleAssistant || got[2].Role != provider.RoleUser {
			t.Errorf("role sequence = %q/%q/%q", got[0].Role, got[1].Role, got[2].Role)
		}
		if len(got[1].Content) != 2 {
			t.Errorf("assistant group didn't collapse: %+v", got[1].Content)
		}
	})

	t.Run("tool_use with empty ToolInput defaults to {}", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_x", ToolName: "something", ToolInput: ""},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 1 || len(got[0].Content) != 1 {
			t.Fatalf("unexpected shape: %+v", got)
		}
		if string(got[0].Content[0].ToolInput) != "{}" {
			t.Errorf("empty ToolInput should default to {}; got %s", got[0].Content[0].ToolInput)
		}
	})

	t.Run("tool_result becomes structured ContentToolResult with ID carried over", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "Directory: /files/"},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_2", Content: "boom", IsError: true},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 1 || got[0].Role != provider.RoleUser {
			t.Fatalf("expected one user message, got %+v", got)
		}
		if len(got[0].Content) != 2 {
			t.Fatalf("grouped tool_results = %d, want 2", len(got[0].Content))
		}
		if got[0].Content[0].Type != provider.ContentToolResult || got[0].Content[0].ToolResultID != "tu_1" || got[0].Content[0].ToolResultContent != "Directory: /files/" || got[0].Content[0].ToolResultIsError {
			t.Errorf("tool_result[0] = %+v", got[0].Content[0])
		}
		if !got[0].Content[1].ToolResultIsError {
			t.Errorf("tool_result[1] should have IsError=true; got %+v", got[0].Content[1])
		}
	})

	t.Run("doc_published entries are skipped entirely", func(t *testing.T) {
		// doc_published is a UI decoration persisted only so refresh
		// shows the 📄 notice. The model already sees the same info
		// in the paired tool_result, so we skip it to avoid bloating
		// every call with redundant tokens.
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "make one"},
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_1", ToolName: "publish_agent_role", ToolInput: `{"slug":"x"}`},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "ok"},
				{Role: store.RoleSent, Kind: "doc_published", Content: "📄 agent_role \"X\" → x"},
				{Role: store.RoleSent, Kind: "direct_chat", Content: "done"},
			},
		}
		got := ctx.chatHistoryToMessages()
		// Expected sequence (doc_published hidden):
		//   user{direct_chat}, assistant{tool_use}, user{tool_result}, assistant{direct_chat}
		if len(got) != 4 {
			t.Fatalf("want 4 messages (doc_published skipped), got %d: %+v", len(got), got)
		}
		for _, m := range got {
			for _, b := range m.Content {
				if b.Type == provider.ContentText && strings.Contains(b.Text, "📄") {
					t.Errorf("doc_published text leaked into context: %q", b.Text)
				}
			}
		}
		// Ensure the trailing assistant direct_chat is its own
		// message (wasn't accidentally merged with the skipped
		// doc_published above it into some degenerate group).
		if got[3].Role != provider.RoleAssistant || got[3].Content[0].Text != "done" {
			t.Errorf("final message shape: %+v", got[3])
		}
	})

	t.Run("received attachments inline", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "see attached",
					Attachments: []store.MessageAttachment{{SHA: "rx", Name: "brief.md"}}},
			},
			Attachments: fakeAttachmentReader{"rx": "BRIEF_TEXT"},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 1 {
			t.Fatalf("got %+v", got)
		}
		text := got[0].Content[0].Text
		if !strings.Contains(text, "see attached") || !strings.Contains(text, "BRIEF_TEXT") {
			t.Errorf("attachment text should still inline on received messages: %q", text)
		}
	})

	t.Run("user-side tool_result and direct_chat split into separate messages", func(t *testing.T) {
		// A new user directive must not be glommed onto a preceding
		// tool_result: a fresh direct_chat from
		// the user following a tool_result must land in its own
		// user message, not as a second content block after the
		// tool_result. Same for inbox_delivery following tool_result.
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_1", ToolName: "agent_memory_append", ToolInput: `{}`},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "appended 290 chars"},
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "Perfect thanks. now please issue them a test task"},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 3 {
			t.Fatalf("want 3 messages (assistant tool_use, user tool_result, user direct_chat); got %d: %+v", len(got), got)
		}
		if got[0].Role != provider.RoleAssistant || got[0].Content[0].Type != provider.ContentToolUse {
			t.Errorf("msg[0] should be assistant tool_use; got %+v", got[0])
		}
		if got[1].Role != provider.RoleUser || got[1].Content[0].Type != provider.ContentToolResult {
			t.Errorf("msg[1] should be user tool_result (alone); got %+v", got[1])
		}
		if got[2].Role != provider.RoleUser || got[2].Content[0].Type != provider.ContentText {
			t.Errorf("msg[2] should be user direct_chat (alone); got %+v", got[2])
		}
		if len(got[1].Content) != 1 || len(got[2].Content) != 1 {
			t.Errorf("tool_result and direct_chat should NOT be merged in the same user message")
		}
	})

	t.Run("tool_result and direct_chat never share one user message via BuildRequest", func(t *testing.T) {
		// A new direct_chat packed into the same user message as a
		// preceding tool_result reads to the model as commentary on
		// the tool result rather than fresh input. This exercises the
		// full BuildRequest path (not just chatHistoryToMessages).
		// The guarantee: a direct_chat following a
		// tool_result from the user lands in its OWN user message,
		// with no prior tool_result content block in the same
		// message.
		ctx := Context{
			Agent:    store.Agent{Slug: "cos", Role: "CoS"},
			Handbook: "c",
			OrgChart: "o",
			Role:     "k",
			Model:    "claude-opus-4-7",
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "hire X"},
				{Role: store.RoleSent, Kind: "direct_chat", Content: "on it"},
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "t1", ToolName: "agent_memory_append", ToolInput: `{}`},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "t1", Content: "appended 290 chars"},
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "Perfect thanks. now please issue them a test task"},
			},
		}
		req := ctx.BuildRequest()
		var sawToolResultOnly, sawFreshDirective bool
		for _, m := range req.Messages {
			if m.Role != provider.RoleUser {
				continue
			}
			hasToolResult := false
			hasText := false
			for _, b := range m.Content {
				if b.Type == provider.ContentToolResult {
					hasToolResult = true
				}
				if b.Type == provider.ContentText {
					hasText = true
					if strings.Contains(b.Text, "Perfect thanks") {
						sawFreshDirective = true
						if hasToolResult {
							t.Errorf("REGRESSION: fresh user directive packed into the same user message as a tool_result. Content: %+v", m.Content)
						}
					}
				}
			}
			if hasToolResult && !hasText {
				sawToolResultOnly = true
			}
		}
		if !sawToolResultOnly {
			t.Error("expected a user message that holds only a tool_result (no text blocks)")
		}
		if !sawFreshDirective {
			t.Error("expected to find the fresh user directive as a user text block")
		}
	})

	t.Run("user-side inbox_delivery and tool_result split apart", func(t *testing.T) {
		// Symmetric: tool_result following an inbox_delivery starts
		// a new user message. Inbox events are system-routed; a
		// tool_result is completion of an assistant call. They're
		// semantically different releases of input.
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "[INBOX task_request from a]..."},
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_1", ToolName: "file_view", ToolInput: `{}`},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "ok"},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 3 {
			t.Fatalf("want 3 messages; got %d: %+v", len(got), got)
		}
		if got[0].Content[0].Type != provider.ContentText || got[0].Role != provider.RoleUser {
			t.Errorf("msg[0] should be user text (inbox_delivery); got %+v", got[0])
		}
		if got[2].Content[0].Type != provider.ContentToolResult {
			t.Errorf("msg[2] should be user tool_result; got %+v", got[2])
		}
	})

	t.Run("full agent-creation sequence groups correctly", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "go"},                                            // user
				{Role: store.RoleSent, Kind: "direct_chat", Content: "ok"},                                                // assistant start
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "a", ToolName: "file_view", ToolInput: `{}`},          // ...still assistant
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "a", Content: "r1"},                            // user
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "b", ToolName: "publish_agent_role", ToolInput: `{}`}, // assistant
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "b", Content: "r2"},                            // user
				{Role: store.RoleSent, Kind: "direct_chat", Content: "done"},                                              // assistant
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 6 {
			t.Fatalf("want 6 messages, got %d:\n%+v", len(got), got)
		}
		wantRoles := []provider.Role{provider.RoleUser, provider.RoleAssistant, provider.RoleUser, provider.RoleAssistant, provider.RoleUser, provider.RoleAssistant}
		for i, m := range got {
			if m.Role != wantRoles[i] {
				t.Errorf("msg[%d].Role = %q, want %q", i, m.Role, wantRoles[i])
			}
		}
		// The first assistant message should have both the text and
		// the tool_use collapsed into one.
		if len(got[1].Content) != 2 {
			t.Errorf("first assistant msg should have 2 blocks (text + tool_use); got %+v", got[1].Content)
		}
	})
}

// TestChatHistoryUserInterruptionPrependsToNextTextMessage: the
// user-interruption marker is not projected as a standalone user
// message (which would break the API tool-pair invariant when it
// lands mid-pair), but its content IS prepended to the next text-
// bucket user message. The model sees the interruption notice inline
// with the CEO's redirect, and on the SDK path latestUserMessage
// carries the prepended notice into stdin — overriding the CLI's
// own meta "Continue from where you left off" injection.
func TestChatHistoryUserInterruptionPrependsToNextTextMessage(t *testing.T) {
	t.Run("marker between tool_use and tool_result, then a CEO redirect", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "kick off the subtask"},
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_1", ToolName: "subtask", ToolInput: `{}`},
				{Role: store.RoleReceived, Kind: store.KindUserInterruption, Content: "User pressed Stop."},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "[error: subagent: cancelled]", IsError: true},
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "Cool I think it worked. What do you see?"},
			},
		}
		got := ctx.chatHistoryToMessages()

		// Expected projection:
		//   user[direct_chat "kick off"]
		//   assistant[tool_use]
		//   user[tool_result]                        ← unbroken pair
		//   user["[User pressed Stop.]" + redirect]  ← notice prepended here
		if len(got) != 4 {
			t.Fatalf("len(messages) = %d, want 4: %+v", len(got), got)
		}
		// Tool pair invariant: assistant tool_use immediately followed
		// by user tool_result with no text user message between.
		if got[1].Role != provider.RoleAssistant || got[1].Content[0].Type != provider.ContentToolUse {
			t.Fatalf("msg[1] = %+v, want assistant tool_use", got[1])
		}
		if got[2].Role != provider.RoleUser || got[2].Content[0].Type != provider.ContentToolResult {
			t.Fatalf("msg[2] = %+v, want user tool_result", got[2])
		}
		// Notice landed on the next text user message.
		if got[3].Role != provider.RoleUser || got[3].Content[0].Type != provider.ContentText {
			t.Fatalf("msg[3] = %+v, want user text", got[3])
		}
		text := got[3].Content[0].Text
		if !strings.Contains(text, "User pressed Stop.") {
			t.Errorf("msg[3].text = %q, want to contain the user-interruption notice", text)
		}
		if !strings.Contains(text, "Cool I think it worked") {
			t.Errorf("msg[3].text = %q, want the owner's actual redirect text", text)
		}
		// Notice precedes the redirect text.
		if strings.Index(text, "User pressed Stop.") > strings.Index(text, "Cool I think it worked") {
			t.Errorf("notice should precede the redirect; got %q", text)
		}
	})

	t.Run("runtime-disruption marker prepends similarly", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "draft the memo"},
				{Role: store.RoleSent, Kind: "direct_chat", Content: "On it…"},
				{Role: store.RoleReceived, Kind: store.KindRuntimeDisruption, Content: "The runtime failed mid-task: ..."},
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "any update?"},
			},
		}
		got := ctx.chatHistoryToMessages()
		// user[draft], assistant[On it], user["[runtime...]" + "any update"]
		if len(got) != 3 {
			t.Fatalf("len(messages) = %d, want 3: %+v", len(got), got)
		}
		text := got[2].Content[0].Text
		if !strings.Contains(text, "runtime failed mid-task") {
			t.Errorf("msg[2].text = %q, want runtime-disruption notice prepended", text)
		}
		if !strings.Contains(text, "any update") {
			t.Errorf("msg[2].text = %q, want CEO's actual message", text)
		}
	})

	t.Run("paused-to-deliver marker prepends to the delivered message", func(t *testing.T) {
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "draft the memo"},
				{Role: store.RoleSent, Kind: "direct_chat", Content: "Starting on the outline"},
				{Role: store.RoleReceived, Kind: store.KindPausedToDeliver, Content: "Paused to deliver your message"},
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "use the Q3 numbers"},
			},
		}
		got := ctx.chatHistoryToMessages()
		if len(got) != 3 {
			t.Fatalf("len(messages) = %d, want 3: %+v", len(got), got)
		}
		text := got[2].Content[0].Text
		if !strings.HasPrefix(text, "[Paused to deliver your message]") {
			t.Errorf("msg[2].text = %q, want the pause notice first", text)
		}
		if !strings.Contains(text, "use the Q3 numbers") {
			t.Errorf("msg[2].text = %q, want the delivered message", text)
		}
	})

	t.Run("marker with no following text user message trails as its own", func(t *testing.T) {
		// SpawnDecision returns SpawnHoldForCEO in this state so no
		// chat-turn fires, but exercise the projection regardless: the
		// notice must never contaminate a tool_result message, and it
		// must not vanish either (on the runtime-disruption path a
		// turn DOES fire from exactly this shape).
		ctx := Context{
			ChatHistory: []store.ChatMessage{
				{Role: store.RoleReceived, Kind: "direct_chat", Content: "hi"},
				{Role: store.RoleSent, Kind: "tool_use", ToolUseID: "tu_1", ToolName: "subtask", ToolInput: `{}`},
				{Role: store.RoleReceived, Kind: store.KindUserInterruption, Content: "User pressed Stop."},
				{Role: store.RoleReceived, Kind: "tool_result", ToolUseID: "tu_1", Content: "[cancelled]", IsError: true},
			},
		}
		got := ctx.chatHistoryToMessages()
		// user[hi], assistant[tool_use], user[tool_result], user[notice]
		if len(got) != 4 {
			t.Fatalf("len(messages) = %d, want 4: %+v", len(got), got)
		}
		// Verify no tool_result message is contaminated with notice text.
		if got[2].Content[0].Type != provider.ContentToolResult {
			t.Fatalf("msg[2] = %+v, want pure tool_result block", got[2])
		}
		if len(got[2].Content) != 1 {
			t.Errorf("msg[2] should have exactly 1 block (tool_result, no text); got %+v", got[2].Content)
		}
		if got[3].Role != provider.RoleUser || got[3].Content[0].Type != provider.ContentText || !strings.Contains(got[3].Content[0].Text, "User pressed Stop.") {
			t.Errorf("msg[3] = %+v, want the notice as a trailing user text", got[3])
		}
	})
}

// TestDeliveryHeaderQuotesOnDiskLinkName: two distinct blobs arrive
// under one filename. The writer gives the clean name to the first and
// writes the second beside it with a short-SHA prefix. The header must
// quote the name each file was actually written under; the sender's
// filename would open the FIRST blob for the second delivery.
func TestDeliveryHeaderQuotesOnDiskLinkName(t *testing.T) {
	const (
		oldSHA = "aaaaaaaaaaaa"
		newSHA = "bbbbbbbbbbbb"
	)
	reader := linkNameReader{
		fakeAttachmentReader: fakeAttachmentReader{},
		links: map[string]string{
			oldSHA: "handbook.md",
			newSHA: "bbbbbbbb-handbook.md",
		},
	}
	ctx := Context{
		Agent:               store.Agent{Slug: "alice", Role: "Eng"},
		FilesystemAvailable: true,
		Attachments:         reader,
		ChatHistory: []store.ChatMessage{
			{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "seed v1",
				Attachments: []store.MessageAttachment{{SHA: oldSHA, Name: "handbook.md"}}},
			{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "seed v2",
				Attachments: []store.MessageAttachment{{SHA: newSHA, Name: "handbook.md"}}},
		},
	}
	req := ctx.BuildRequest()

	var texts []string
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == provider.ContentText {
				texts = append(texts, b.Text)
			}
		}
	}
	joined := strings.Join(texts, "\n")

	if !strings.Contains(joined, "/files/attachments/bbbbbbbb-handbook.md") {
		t.Errorf("second delivery must quote its own disambiguated path; got:\n%s", joined)
	}
	// The first delivery legitimately owns the clean name, so the
	// clean path may appear — but exactly once, for that delivery.
	if n := strings.Count(joined, "/files/attachments/handbook.md\n"); n != 1 {
		t.Errorf("clean path appears %d times, want exactly 1 (the delivery that owns it); got:\n%s", n, joined)
	}
}

// TestDeliveryHeaderSidecarInheritsLinkName checks the .txt sidecar
// line follows the disambiguated base name too. A sidecar built from
// the sender's filename would point at the older blob's extracted
// text — the same failure one level down, and the line an agent is
// most likely to read for a PDF or office doc.
func TestDeliveryHeaderSidecarInheritsLinkName(t *testing.T) {
	const newSHA = "cccccccccccc"
	reader := linkNameReader{
		fakeAttachmentReader: fakeAttachmentReader{},
		links:                map[string]string{newSHA: "cccccccc-layout.pdf"},
		sidecars:             map[string]bool{newSHA: true},
	}
	ctx := Context{
		Agent:               store.Agent{Slug: "alice", Role: "Eng"},
		FilesystemAvailable: true,
		Attachments:         reader,
		ChatHistory: []store.ChatMessage{
			{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "gating layout",
				Attachments: []store.MessageAttachment{{SHA: newSHA, Name: "layout.pdf"}}},
		},
	}
	req := ctx.BuildRequest()
	text := req.Messages[0].Content[0].Text

	if !strings.Contains(text, "/files/attachments/cccccccc-layout.pdf\n") {
		t.Errorf("binary line should quote the disambiguated name; got:\n%s", text)
	}
	if !strings.Contains(text, "/files/attachments/cccccccc-layout.pdf.txt") {
		t.Errorf("sidecar line should inherit the disambiguated name; got:\n%s", text)
	}
	if strings.Contains(text, "/files/attachments/layout.pdf") {
		t.Errorf("header must not quote the sender's filename here; got:\n%s", text)
	}
}

// TestDeliveryHeaderFallsBackToSenderName keeps the no-collision path
// (and stores that can't answer the lookup) rendering the clean name
// — the disambiguation must not leak short-SHA prefixes into the
// common case.
func TestDeliveryHeaderFallsBackToSenderName(t *testing.T) {
	ctx := Context{
		Agent:               store.Agent{Slug: "alice", Role: "Eng"},
		FilesystemAvailable: true,
		Attachments:         fakeAttachmentReader{},
		ChatHistory: []store.ChatMessage{
			{Role: store.RoleReceived, Kind: "inbox_delivery", Content: "notes",
				Attachments: []store.MessageAttachment{{SHA: "zzz", Name: "notes.md"}}},
		},
	}
	req := ctx.BuildRequest()
	text := req.Messages[0].Content[0].Text
	if !strings.Contains(text, "/files/attachments/notes.md") {
		t.Errorf("uncollided attachment should render its plain name; got:\n%s", text)
	}
}
