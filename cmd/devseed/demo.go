package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/mcp"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// Slugs are a contract with the e2e specs.
const (
	SlugCEO          = agent.CEOSlug
	SlugChiefOfStaff = "chief-of-staff"
	SlugEngLead      = "engineering-lead"
	SlugTestRunner   = "test-runner"
	SlugSupportLead  = "support-lead"
	SlugBookkeeper   = "bookkeeper"
)

// Fixed text the e2e specs assert on.
const (
	OrgName = "Plainsong"

	GoalMailSwitch = "Move shift reminders to the new mail provider"
	GoalHelpPages  = "Publish the new help pages"
	GoalFinance    = "Close the September books"

	PartSetUp       = "Set up the new mail provider"
	PartTestWeek    = "Send a week of test reminders"
	PartSwitch      = "Switch Riverside Food Bank over"
	PartSwapPage    = "Write the help page for shift swaps"
	PartScreenshots = "Take screenshots for the help pages"
	PartReconcile   = "Reconcile September card statements"
	CEOAssignment   = "Approve the mail provider contract"

	ItemDelivered = "Test reminders all delivered"
	ItemSwitched  = "Riverside Food Bank switched over"
	ItemBounces   = "Bounce handling checked"

	HireTitle        = "Approve hire: Docs writer"
	HireSlug         = "docs-writer"
	NotificationText = "Weekly report: test reminders all delivered"
	NoticeCall       = "Riverside Food Bank call moved to Thursday"
	NoticeTrial      = "Mail provider trial extended to Friday"

	CEOChatAsk   = "What's the latest on the mail switch? Give me the short version."
	AgentReplyMD = "**The mail switch is on track.** The test batch is in and every reminder was delivered; a week of test reminders is next, and the Riverside Food Bank switch waits on it.\n\n" +
		"| Measure | New provider | Target |\n|---|---|---|\n| Delivered | 500 of 500 | 99% |\n| Bounced | 0 | under 1% |\n| Median delay | 4 s | 60 s |\n\n" +
		"- Open risk: Riverside's volunteer list has old addresses that will bounce\n- Next: a week of test reminders, then switch Riverside Food Bank over"
	TurnErrorText = "The model call failed: overloaded_error (529). The turn stopped before it finished."

	SubagentID   = "a1b2c3d4e5f60718"
	SubagentDesc = "Summarise the test-reminder log"

	EgressHost      = "api.mail.example.net"
	CustomSkill     = "bounce-triage"
	BuiltinSkill    = "plan-and-fan-out"
	UnpricedModelID = "claude-lab-preview"
)

const handbook = `# Plainsong

Plainsong runs Plainsong Rota, a plain online service that lets food
banks, shelters and community groups schedule volunteer shifts and send
shift reminders by email. Small groups use it free; everyone else pays
one flat, modest price.

## How we work

- Ship small, ship often. A fix that reaches customers beats a perfect
  one that does not.
- Every piece of work lives in the assignment tracker, with a clear owner
  and a clear "done when".
- Spend under $500 needs no approval. Anything above goes to the CEO.
- Customer data stays inside Plainsong. Nothing leaves without the
  CEO's say-so.

## Tone

Plain words, short answers, numbers over adjectives.
`

const cosRole = `# Chief of Staff

You run operations and coordination for Plainsong. You keep the org
chart right, propose hires and reorgs to the CEO, and make sure every
goal has an owner and a plan.
`

// orgBasics writes what both the empty and demo scenarios share: the
// name, the handbook, the CEO and the Chief of Staff.
func (s *seeder) orgBasics(created time.Time) error {
	if err := s.st.WriteCompanyName(OrgName); err != nil {
		return err
	}
	if err := s.st.WriteHandbook(handbook); err != nil {
		return err
	}
	if err := s.createAgent(agentSpec{Agent: store.Agent{Slug: SlugCEO, Role: "CEO", CreatedAt: created}}); err != nil {
		return err
	}
	return s.createAgent(agentSpec{
		Agent: store.Agent{
			Slug: SlugChiefOfStaff, Role: "Chief of Staff", Icon: "compass", ReportsTo: SlugCEO,
			Model: "claude-opus-5-5", Effort: claudeagent.EffortHigh, CreatedAt: created,
		},
		RoleMD: cosRole,
		Memory: md(`
# Memory

- Plainsong runs one product, Plainsong Rota: volunteer shifts and email reminders.
- The CEO wants short answers with numbers.
- Riverside Food Bank is the first customer on the new mail provider.
`),
		Habits: md(`
- Put every ask in the tracker before chasing it in chat.
- Propose hires only with a role doc attached.
`),
	})
}

// empty is the scenario Home shows its empty states on: setup is done
// (the Chief of Staff exists) and nothing else is.
func (s *seeder) empty() error {
	return s.orgBasics(s.ago(24 * time.Hour))
}

func (s *seeder) demo() error {
	created := s.ago(30 * 24 * time.Hour)
	if err := s.orgBasics(created); err != nil {
		return err
	}
	assets, err := logoAssets()
	if err != nil {
		return err
	}
	if err := s.st.WriteFaviconAssets(assets); err != nil {
		return err
	}
	for _, a := range demoAgents(created) {
		if err := s.createAgent(a); err != nil {
			return err
		}
	}
	steps := []func() error{
		s.archivedEngineeringChat,
		s.timeline,
		s.settleChats,
		s.usage,
		s.settings,
		s.projectFiles,
		s.graph,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func demoAgents(created time.Time) []agentSpec {
	return []agentSpec{
		{
			Agent: store.Agent{Slug: SlugEngLead, Role: "Engineering lead", Icon: "code", ReportsTo: SlugChiefOfStaff,
				Model: "claude-opus-5-5", Effort: claudeagent.EffortXHigh, CreatedAt: created},
			RoleMD: md(`
# Engineering lead

You own Plainsong Rota's code and the services it runs on: the mail
switch, releases, and reminders going out on time. Test runner reports
to you.
`),
			Memory: md(`
# Memory

- The old mail provider throttles us at the 6 pm peak; some reminders land late.
- Riverside Food Bank moves first; they send about 1,200 reminders a week.
- The CEO wants a short version first, detail on request.
`),
			Habits: md(`
- Never switch a customer before a full week of test reminders.
- Count deliveries from the provider's log, not from our send queue.
`),
		},
		{
			Agent: store.Agent{Slug: SlugTestRunner, Role: "Test runner", Icon: "bug", ReportsTo: SlugEngLead,
				Model: "claude-sonnet-5", Effort: claudeagent.EffortMedium, CreatedAt: created},
			RoleMD: md(`
# Test runner

You run Plainsong Rota's tests: test reminders, shift sign-ups and the
CSV export. You report results to the Engineering lead with numbers.
`),
			Memory: "# Memory\n\n- The test list holds 500 addresses we own.\n",
			Habits: "- Log every run with its date and batch size.\n",
		},
		{
			Agent: store.Agent{Slug: SlugSupportLead, Role: "Support lead", Icon: "headset", ReportsTo: SlugChiefOfStaff,
				Model: "claude-sonnet-5", Effort: claudeagent.EffortHigh, CreatedAt: created},
			RoleMD: md(`
# Support lead

You answer customer questions about Plainsong Rota and own the help
pages.
`),
			Memory: "# Memory\n\n- The question asked most is how to swap a shift.\n",
			Habits: "- Every help page needs real screenshots, never mock-ups.\n",
		},
		{
			Agent: store.Agent{Slug: SlugBookkeeper, Role: "Bookkeeper", Icon: "calculator", ReportsTo: SlugChiefOfStaff,
				Model: "claude-haiku-4-5", Effort: claudeagent.EffortLow, CreatedAt: created},
			RoleMD: md(`
# Bookkeeper

You keep Plainsong's books: card reconciliations, the monthly close and
spend requests to the CEO.
`),
			Memory: "# Memory\n\n- The books close on the fifth working day of the month.\n",
			Habits: "- Anything over $500 goes to the CEO as an assignment.\n",
		},
	}
}

// archivedEngineeringChat gives the Engineering lead one past chat,
// rotated eight days ago, with its episode and the memory and habits it
// had then.
func (s *seeder) archivedEngineeringChat() error {
	t := s.ago(10 * 24 * time.Hour)
	sent := func(at time.Time, text string) store.ChatMessage {
		return store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: text, TS: at,
			Model: "claude-opus-5-5", Effort: claudeagent.EffortXHigh}
	}
	if err := s.chat(SlugEngLead,
		store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "Some reminders went out late last night. Walk me through it.", TS: t},
		sent(t.Add(20*time.Minute), "Two things went wrong:\n\n1. The old provider throttled us at the 6 pm peak.\n2. Our retry gave up after one attempt.\n\nThe retry is fixed; the throttling is why we are moving providers."),
		store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", Content: "Good. Write down what we learned before the switch.", TS: t.Add(24 * time.Hour)},
		sent(t.Add(24*time.Hour+15*time.Minute), "Done. The lessons are in my memory, and the switch checklist names both."),
	); err != nil {
		return err
	}
	ts := archiveTS(s.ago(8 * 24 * time.Hour))
	if err := s.st.ArchiveChatAs(SlugEngLead, ts); err != nil {
		return err
	}
	if err := s.st.WriteArchivedAgentMemory(SlugEngLead, ts, "# Memory\n\n- Riverside Food Bank moves first; they send about 1,200 reminders a week.\n"); err != nil {
		return err
	}
	if err := s.st.WriteArchivedAgentHabits(SlugEngLead, ts, "- Count deliveries from the provider's log, not from our send queue.\n"); err != nil {
		return err
	}
	return s.st.WriteEpisode(SlugEngLead, ts, fmt.Sprintf(
		"---\nts: %s\ntitle: Late reminders retro\ntouched: -\n---\nWalked the CEO through the late reminders: the old provider throttled us at the 6 pm peak and our retry gave up too soon. Both went into the switch checklist.\n", ts))
}

// timeline plays the tracker, the inboxes and the chats in time order.
func (s *seeder) timeline() error {
	var err error
	must := func(e error) {
		if err == nil && e != nil {
			err = e
		}
	}
	id := func(n int, e error) int { must(e); return n }

	// The CEO files the mail switch; the Engineering lead splits it.
	t := s.ago(6 * 24 * time.Hour)
	goalMail := id(s.create(t, SlugCEO, released, assignments.CreateInput{
		Title: GoalMailSwitch, Assignee: SlugEngLead,
		Body:       "Riverside Food Bank moves first: about 1,200 reminders a week.",
		Acceptance: []string{ItemDelivered, ItemSwitched, ItemBounces},
	}))
	must(s.chat(SlugEngLead,
		store.ChatMessage{Role: store.RoleReceived, Kind: store.KindWakeUpdate, TS: t.Add(time.Second),
			Content: fmt.Sprintf("Your open assignments:\n- #%d %s (from you, the CEO): 0 of 3 done-when items met", goalMail, GoalMailSwitch)},
		store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", TS: t.Add(2 * time.Minute),
			Content: "assignment_create: " + PartSetUp, ToolUseID: "toolu_devseed_01", ToolName: "assignment_create",
			ToolInput: fmt.Sprintf(`{"title":%q,"assignee":%q,"parent":%d,"satisfies":[%q]}`, PartSetUp, SlugTestRunner, goalMail, ItemDelivered)},
	))
	tSetUp := t.Add(2*time.Minute + 5*time.Second)
	setUp := id(s.create(tSetUp, SlugEngLead, released, assignments.CreateInput{
		Title: PartSetUp, Assignee: SlugTestRunner, Parent: goalMail, Satisfies: []string{ItemDelivered},
		Body: "Connect the new provider on staging, send a test batch to the 500-address list, log delivered, bounced and delay.",
	}))
	must(s.chat(SlugEngLead,
		store.ChatMessage{Role: store.RoleReceived, Kind: "tool_result", TS: tSetUp, ToolUseID: "toolu_devseed_01",
			Content: fmt.Sprintf("Filed #%d %q, assigned to %s.", setUp, PartSetUp, SlugTestRunner)},
		store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", TS: t.Add(4 * time.Minute),
			Model: "claude-opus-5-5", Effort: claudeagent.EffortXHigh,
			Content: fmt.Sprintf("Filed #%d for Test runner to set up the new provider and send a test batch. The week of test reminders and the Riverside switch follow once it passes.", setUp)},
	))

	// The help pages.
	t = s.ago(5 * 24 * time.Hour)
	goalHelp := id(s.create(t, SlugCEO, released, assignments.CreateInput{
		Title: GoalHelpPages, Assignee: SlugSupportLead,
		Body: "The help pages need a page on shift swaps and fresh screenshots.",
	}))
	swapPage := id(s.create(t.Add(10*time.Minute), SlugSupportLead, released, assignments.CreateInput{
		Title: PartSwapPage, Assignee: SlugSupportLead, Parent: goalHelp,
		Body: "Two hundred words on swapping a shift, for volunteers and for coordinators.",
	}))
	screenshots := id(s.create(t.Add(12*time.Minute), SlugSupportLead, released, assignments.CreateInput{
		Title: PartScreenshots, Parent: goalHelp,
		Body: "One screenshot per step of each help page, taken on staging.",
	}))

	// Finance, and the spend question it puts to the CEO.
	t = s.ago(4 * 24 * time.Hour)
	goalFin := id(s.create(t, SlugCEO, released, assignments.CreateInput{
		Title: GoalFinance, Assignee: SlugBookkeeper,
		Body: "Close September by the fifth working day of October.",
	}))
	_ = id(s.create(t.Add(5*time.Minute), SlugBookkeeper, released, assignments.CreateInput{
		Title: PartReconcile, Parent: goalFin,
		Body: "Match every September card charge to a receipt.",
	}))
	_ = id(s.create(t.Add(7*time.Minute), SlugBookkeeper, released, assignments.CreateInput{
		Title: CEOAssignment, Assignee: SlugCEO, Parent: goalFin,
		Body: "The new mail provider's contract is $1,800 a year, over the $500 line. Approve it, or tell me what to cut.",
	}))

	// The Engineering lead: the CEO asks, a background task runs, a Stop.
	t = s.ago(3 * 24 * time.Hour)
	must(s.chiefOfStaffChat())
	must(s.engineeringConversation(t))

	// Test runner closes the setup; the Engineering lead hears it.
	t = s.ago(2 * 24 * time.Hour)
	_, e := s.assignment(t, SlugTestRunner, released, func(r assignments.Rules) (assignments.Change, error) {
		return r.Close(setUp, SlugTestRunner, assignments.ResolutionDone, "The new provider is set up on staging. 500 of 500 test reminders delivered, median delay 4 s. Log is in /files/artifacts/public/.")
	})
	must(e)
	must(s.chat(SlugTestRunner, store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", TS: t.Add(time.Minute),
		Model: "claude-sonnet-5", Effort: claudeagent.EffortMedium,
		Content: fmt.Sprintf("Every test reminder was delivered; closed #%d.", setUp)}))
	must(s.chat(SlugEngLead, store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", TS: t.Add(10 * time.Minute),
		Model: "claude-opus-5-5", Effort: claudeagent.EffortXHigh,
		Content: "Setup is closed. The week of test reminders is next; I'll file it once the trial account is sorted."}))

	// The Chief of Staff proposes a hire.
	_, e = s.toCEO(store.Message{
		Type: store.MsgCEOApprovalRequest, Title: HireTitle, From: SlugChiefOfStaff, To: store.Recipients{SlugCEO},
		Date: s.ago(26 * time.Hour),
		Body: "The Support lead writes every help page alone, and #" + fmt.Sprint(goalHelp) + " has eleven pages still to go.\n",
		Hire: &store.Hire{
			Slug: HireSlug, Role: "Docs writer", Icon: "file-text", ReportsTo: SlugSupportLead,
			Body: md(`
# Docs writer

You write Plainsong Rota's help pages: setting up a rota, swapping a
shift, exporting a list. You report to the Support lead and keep every
page in step with the product.
`),
			InitialAgentMemory: "# Memory\n\n- The shift-swap page is the model for every other help page.\n",
		},
	})
	must(e)

	// The shift-swap page is done; the screenshots wait on staging.
	t = s.ago(24 * time.Hour)
	_, e = s.assignment(t, SlugSupportLead, released, func(r assignments.Rules) (assignments.Change, error) {
		return r.Close(swapPage, SlugSupportLead, assignments.ResolutionDone, "The page is in /files/artifacts/public/shift-swaps.md.")
	})
	must(e)
	hold := true
	_, e = s.assignment(s.ago(20*time.Hour), SlugSupportLead, released, func(r assignments.Rules) (assignments.Change, error) {
		return r.Update(screenshots, SlugSupportLead, assignments.UpdateInput{Hold: &hold, Note: "Waiting for the new sign-up screen to reach staging."})
	})
	must(e)

	// A report for the CEO.
	_, e = s.toCEO(store.Message{
		Type: store.MsgCEONotification, Title: NotificationText, From: SlugChiefOfStaff, To: store.Recipients{SlugCEO},
		Date: s.ago(5 * time.Hour),
		Body: "- Every test reminder was delivered on staging.\n- The shift-swap help page is done; screenshots wait on staging.\n- September close is on track.\n",
	})
	must(e)

	// The test week and the switch it gates. Its wake to Test runner
	// waits in the queue.
	t = s.ago(3 * time.Hour)
	testWeek := id(s.create(t, SlugEngLead, queued, assignments.CreateInput{
		Title: PartTestWeek, Assignee: SlugTestRunner, Parent: goalMail,
		Body: "A reminder to the 500-address list every day for seven days; log delivered, bounced and delay each day.",
	}))
	_ = id(s.create(t.Add(time.Minute), SlugEngLead, released, assignments.CreateInput{
		Title: PartSwitch, Parent: goalMail, BlockedBy: []int{testWeek}, Satisfies: []string{ItemSwitched},
		Body: "Move Riverside Food Bank's reminders to the new provider once the test week passes.",
	}))

	// Two notices wait for release.
	must(s.queueNotice(store.Message{
		Type: store.MsgNotice, Title: NoticeCall, From: SlugChiefOfStaff,
		To: store.Recipients{SlugEngLead, SlugSupportLead}, Date: s.ago(90 * time.Minute),
		Body: "Riverside Food Bank moved their call to Thursday at 10:00. Bring the test-reminder numbers and the new help pages if they are up.",
	}))
	must(s.publishedNotice(SlugEngLead, "toolu_devseed_pub1", store.Message{
		Type: store.MsgNotice, Title: NoticeTrial, From: SlugEngLead,
		To: store.Recipients{SlugTestRunner}, Date: s.ago(40 * time.Minute),
		Body: "The mail provider extended our trial to Friday. Send every test reminder from the trial account until then.",
	}))

	// Test runner stops on an error.
	t = s.ago(30 * time.Minute)
	must(s.chat(SlugTestRunner,
		store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", TS: t, Content: "Can you get the test week ready?"},
		store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", TS: t.Add(time.Minute), Content: "Bash: ls /files/artifacts/public",
			ToolUseID: "toolu_devseed_tr1", ToolName: "Bash", ToolInput: `{"command":"ls /files/artifacts/public"}`},
		store.ChatMessage{Role: store.RoleReceived, Kind: "tool_result", TS: t.Add(time.Minute + 2*time.Second), ToolUseID: "toolu_devseed_tr1",
			Content: "one-day-test.md\ntest-week-plan.md"},
		store.ChatMessage{Role: store.RoleSent, Kind: store.KindTurnError, TS: t.Add(2 * time.Minute), Content: TurnErrorText},
	))
	return err
}

// queueNotice writes a notice and queues it for each recipient.
func (s *seeder) queueNotice(m store.Message) error {
	rel, err := s.writeMessage(m)
	if err != nil {
		return err
	}
	return s.enqueue(rel, m.To...)
}

// publishedNotice is queueNotice as the sender's publish_notice call
// leaves it in the sender's chat: the call, its ack, and the sent row.
func (s *seeder) publishedNotice(from, toolUseID string, m store.Message) error {
	rel, err := s.writeMessage(m)
	if err != nil {
		return err
	}
	if err := s.enqueue(rel, m.To...); err != nil {
		return err
	}
	to := strings.Join(m.To, ",")
	return s.chat(from,
		store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", TS: m.Date, Content: "publish_notice: " + m.Title,
			ToolUseID: toolUseID, ToolName: "publish_notice",
			ToolInput: fmt.Sprintf(`{"title":%q,"to":%q}`, m.Title, to)},
		store.ChatMessage{Role: store.RoleReceived, Kind: "tool_result", TS: m.Date.Add(time.Second), ToolUseID: toolUseID,
			Content: fmt.Sprintf("publish_notice published: %q → %s (saved to %s)", m.Title, to, rel)},
		store.ChatMessage{Role: store.RoleSent, Kind: "doc_published", TS: m.Date.Add(time.Second), ToolUseID: toolUseID,
			ToolName: "publish_notice", MessageRef: rel,
			Content: fmt.Sprintf("📄 notice %q → %s", m.Title, to)},
	)
}

func (s *seeder) chiefOfStaffChat() error {
	t := s.ago(3*24*time.Hour + 2*time.Hour)
	return s.chat(SlugChiefOfStaff,
		store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", TS: t, Content: "Anything I should know this week?"},
		store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", TS: t.Add(3 * time.Minute),
			Model: "claude-opus-5-5", Effort: claudeagent.EffortHigh,
			Content: "Three things:\n\n1. The new mail provider is being set up, with Test runner sending the first test batch.\n2. The Support lead is writing every help page alone. I'll propose a hire.\n3. September books close on schedule."},
	)
}

// engineeringConversation is the CEO's exchange with the Engineering
// lead: a question, a background task, a Stop, a redirect, and the
// answer.
func (s *seeder) engineeringConversation(t time.Time) error {
	opus := func(at time.Time, text string) store.ChatMessage {
		return store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: text, TS: at,
			Model: "claude-opus-5-5", Effort: claudeagent.EffortXHigh}
	}
	const prompt = "Read the test-reminder log in /files/artifacts/public/ and summarise delivered, bounced and median delay against the 99% / 60 s targets. Five lines at most."
	final := "500 of 500 delivered (target 99%), none bounced, median delay 4 s (target 60 s). The slowest reminder took 41 s."
	dispatchAt := t.Add(time.Minute)
	receiptAt := dispatchAt.Add(2 * time.Second)
	resultAt := t.Add(20 * time.Minute)
	receipt := fmt.Sprintf("Dispatched 1 background task. They are RUNNING NOW — this is a receipt, not a result, and your turn is not blocked.\n\n"+
		"  %s — %s\n      transcript: /agents/%s/subagents/%s · artifacts: /files/subagents/%s/artifacts/private/\n",
		SubagentID, SubagentDesc, SlugEngLead, SubagentID, SubagentID)
	if err := s.chat(SlugEngLead,
		store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", TS: t, Content: CEOChatAsk},
		store.ChatMessage{Role: store.RoleSent, Kind: "tool_use", TS: dispatchAt, Content: "subagent: " + SubagentDesc,
			ToolUseID: "toolu_devseed_02", ToolName: mcp.SubagentToolName,
			ToolInput: fmt.Sprintf(`{"tasks":[{"description":%q,"prompt":%q,"model":"claude-sonnet-5","effort":"medium"}]}`, SubagentDesc, prompt)},
		store.ChatMessage{Role: store.RoleReceived, Kind: "tool_result", TS: receiptAt, ToolUseID: "toolu_devseed_02", Content: receipt},
		opus(t.Add(2*time.Minute), "I've asked a background task to pull the delivery numbers together. I'll report back when it lands."),
		store.ChatMessage{Role: store.RoleReceived, Kind: store.KindUserInterruption, TS: t.Add(3 * time.Minute), Content: "User pressed Stop."},
		store.ChatMessage{Role: store.RoleReceived, Kind: "direct_chat", TS: t.Add(4 * time.Minute), Content: "No need to chase the provider today. Just wait for the numbers."},
		opus(t.Add(5*time.Minute), "Understood. Nothing goes to the provider until you say so."),
		store.ChatMessage{Role: store.RoleReceived, Kind: store.KindSubagentResult, TS: resultAt,
			Content: fmt.Sprintf("Background task %s (%s) finished.\n\n%s\n\nAnything it wrote is at /files/subagents/%s/artifacts/private/ · full transcript: /agents/<you>/subagents/%s\n\nNothing else of yours is running — this was the last one.\n",
				SubagentID, SubagentDesc, final, SubagentID, SubagentID)},
		opus(t.Add(22*time.Minute), AgentReplyMD),
	); err != nil {
		return err
	}
	for _, m := range []store.ChatMessage{
		{Role: store.RoleReceived, Kind: "direct_chat", TS: receiptAt, Content: prompt},
		{Role: store.RoleSent, Kind: "tool_use", TS: receiptAt.Add(30 * time.Second), Content: "Bash: cat test-reminders.log",
			ToolUseID: "toolu_devseed_sub1", ToolName: "Bash", ToolInput: `{"command":"cat /files/artifacts/public/test-reminders.log"}`},
		{Role: store.RoleReceived, Kind: "tool_result", TS: receiptAt.Add(31 * time.Second), ToolUseID: "toolu_devseed_sub1",
			Content: "batch,sent,delivered,bounced,median_s\nT1,100,100,0,4\nT2,100,100,0,3\nT3,100,100,0,4\nT4,100,100,0,5\nT5,100,100,0,4"},
		{Role: store.RoleSent, Kind: "direct_chat", TS: resultAt.Add(-time.Minute), Content: final,
			Model: "claude-sonnet-5", Effort: claudeagent.EffortMedium},
	} {
		if err := s.st.AppendSubagentMessage(SlugEngLead, SubagentID, m); err != nil {
			return err
		}
	}
	return s.writeSubagentMeta(SlugEngLead, subagentMeta{
		ID: SubagentID, Description: SubagentDesc, Prompt: prompt, Model: "claude-sonnet-5", Effort: claudeagent.EffortMedium,
		Status: "completed", UpdatedAt: resultAt.Add(-time.Minute).Format(time.RFC3339),
	})
}

// settleReplies answer whatever an agent was last handed, so no agent
// but Test runner (held on its error) owes a turn when the server boots.
var settleReplies = map[string]string{
	SlugChiefOfStaff: "Noted.",
	SlugEngLead:      "Noted; on it.",
	SlugSupportLead:  "Thanks. The shift-swap page is in; the screenshots wait on staging.",
	SlugBookkeeper:   "On it. I'll reconcile the cards first and send the mail provider contract to the CEO.",
}

func (s *seeder) settleChats() error {
	slugs := make([]string, 0, len(settleReplies))
	for slug := range settleReplies {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		hist, err := s.st.ReadChatHistory(slug)
		if err != nil {
			return err
		}
		if !store.HasUnansweredReceived(hist) {
			continue
		}
		a, err := s.st.GetAgent(slug)
		if err != nil {
			return err
		}
		if err := s.chat(slug, store.ChatMessage{Role: store.RoleSent, Kind: "direct_chat", Content: settleReplies[slug],
			TS: hist[len(hist)-1].TS.Add(2 * time.Minute), Model: a.Model, Effort: a.Effort}); err != nil {
			return err
		}
	}
	return nil
}

// usage writes thirty days of usage rows across the agents, in time
// order (usage.jsonl is read as a time-ordered tail), from a fixed
// formula, plus one row on a model Kivali has no price for.
func (s *seeder) usage() error {
	type pin struct{ slug, model string }
	pins := []pin{
		{SlugChiefOfStaff, "claude-opus-5-5"},
		{SlugEngLead, "claude-opus-5-5"},
		{SlugTestRunner, "claude-sonnet-5"},
		{SlugSupportLead, "claude-sonnet-5"},
		{SlugBookkeeper, "claude-haiku-4-5"},
	}
	purposes := []string{"chat", "chat", "subagent", "release"}
	var rows []store.UsageRecord
	for d := 0; d < 30; d++ {
		for i, p := range pins {
			if (d+i)%3 == 0 {
				continue
			}
			u := provider.TokenUsage{
				InputTokens:       1200 + 97*((d*7+i*13)%50),
				OutputTokens:      300 + 41*((d*5+i*3)%40),
				CacheReadTokens:   20000 + 1500*((d*3+i*11)%30),
				CacheCreateTokens: 800 + 60*((d+i*7)%20),
			}
			rows = append(rows, store.UsageRecord{
				TS:    s.ago(time.Duration(d)*24*time.Hour + time.Duration(i+1)*37*time.Minute),
				Agent: p.slug, Purpose: purposes[(d+i)%len(purposes)], Model: p.model,
				InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
				CacheReadTokens: u.CacheReadTokens, CacheCreateTokens: u.CacheCreateTokens,
				CostUSD: claudeagent.New(claudeagent.Options{}).Price(p.model, u),
			})
		}
	}
	rows = append(rows, store.UsageRecord{
		TS: s.ago(2*24*time.Hour + 5*time.Minute), Agent: SlugSupportLead, Purpose: "chat", Model: UnpricedModelID,
		InputTokens: 2400, OutputTokens: 610, CacheReadTokens: 18000,
	})
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TS.Before(rows[j].TS) })
	for _, r := range rows {
		if err := s.st.AppendUsage(r); err != nil {
			return err
		}
	}
	return nil
}

// settings: egress, skills.
// Auto-release stays off (the default); the e2e spec toggles it.
func (s *seeder) settings() error {
	al, err := s.st.ReadEgressAllowlist() // seeds the defaults
	if err != nil {
		return err
	}
	al.Patterns = append(al.Patterns, EgressHost)
	if err := s.st.WriteEgressAllowlist(al); err != nil {
		return err
	}
	if err := s.st.InstallBuiltinSkills(); err != nil {
		return err
	}
	if err := s.st.SetSkillEnabled(BuiltinSkill, true); err != nil {
		return err
	}
	return s.st.WriteSkillFromManifest(CustomSkill, md(`
---
name: bounce-triage
description: Sort bounced reminder emails by cause and say what to do about each.
when_to_use: A batch of shift reminders came back bounced and someone needs to know which addresses to fix.
version: 1.0.0
---

# Bounce triage

1. Read the bounce log as CSV: address, group, bounce code.
2. Sort each line into wrong address, full inbox or blocked.
3. List the wrong addresses per group, for its coordinator to fix.
`))
}

func (s *seeder) projectFiles() error {
	for _, f := range []struct{ name, body, summary string }{
		{"riverside-switch.md", "# Riverside Food Bank\n\nRiverside Food Bank moves its volunteer reminders to the new mail provider first: about 1,200 reminders a week, switched by the end of October.\n",
			"The Riverside Food Bank switch: about 1,200 reminders a week."},
		{"help-pages-plan.txt", "Help pages, in order\n- Set up a rota\n- Swap a shift\n- Export the volunteer list\n",
			"Which help pages to write, in order."},
	} {
		pf, err := s.st.AddProjectFile(f.name, strings.NewReader(f.body))
		if err != nil {
			return err
		}
		if err := s.st.SetProjectFileSummary(pf.SHA, f.summary); err != nil {
			return err
		}
	}
	return nil
}

// graph writes a few knowledge-graph nodes into agents' public dirs,
// including a flagged one (it rests on a withdrawn procedure) and one
// with a problem (an unknown front-matter field), then indexes them.
func (s *seeder) graph() error {
	if err := s.st.SyncAllFilesystems(); err != nil {
		return err
	}
	nodes := []struct{ slug, name, body string }{
		{SlugEngLead, "mail-switch.md", md(`
---
id: mail-switch
status: current
summary: Mail provider switch design
---
# Mail provider switch

Reminders go out through the new provider; the old one stays as a
fallback for a week after each customer moves.
`)},
		{SlugEngLead, "reminder-delay.md", md(`
---
id: reminder-delay
kind: requirement
about: engineering-lead/mail-switch
check: Measure the median delay on the 500-address test list
summary: Reminders arrive within 60 s of their send time
---
The median delay stays under 60 seconds, the 6 pm peak included.
`)},
		{SlugTestRunner, "one-day-test.md", md(`
---
id: one-day-test
status: withdrawn
summary: One-day test reminder procedure
---
One day of test reminders to 100 addresses. Withdrawn: the switch needs a
full week on the whole list.
`)},
		{SlugTestRunner, "test-week-plan.md", md(`
---
id: test-week-plan
depends_on: test-runner/one-day-test
summary: Week of test reminders plan
---
Seven days, all 500 addresses, delivered and bounced logged after every send.
`)},
		{SlugSupportLead, "help-pages-brief.md", md(`
---
id: help-pages-brief
summary: New help pages brief
audience: volunteer coordinators
---
The new help pages go live in November, one page per task, each with
fresh screenshots.
`)},
	}
	for _, n := range nodes {
		if err := s.writePublic(n.slug, n.name, n.body); err != nil {
			return err
		}
	}
	// The files were written straight into the published trees, not
	// through artifact_publish, so the maintainer learns of them the
	// way it learns of any out-of-band edit: a scan.
	return s.st.Graph().Scan(context.Background())
}
