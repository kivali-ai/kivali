package apitypes

// Golden fixtures for the agent chat. Between them the rows set every
// optional field of TranscriptRow and SubagentTask, so the TypeScript
// side sees each one.

func ref[T any](v T) *T { return &v }

func goldenTranscript() []TranscriptRow {
	ms := t0.UnixMilli()
	you := ChatParty{Kind: PartyKindPerson, Slug: "ceo", Name: "You"}
	vp := ChatParty{Kind: PartyKindAgent, Slug: "engineering-lead", Name: "Engineering lead"}
	buyer := PersonRef{Slug: "buyer", Name: "Buyer"}
	quote := []AttachmentRef{{Name: "hosting-quote.pdf", SizeBytes: 86016, URL: "/attachments/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}}
	done, errored, running := ToolStatusDone, ToolStatusError, ToolStatusRunning
	sent, received := MessageRoleSent, MessageRoleReceived
	stop, paused := MarkerKindUserInterruption, MarkerKindPausedToDeliver
	tasks := []SubagentTask{
		{Index: 0, ID: "aaaa1111", Title: "Compare hosting providers", Model: "Sonnet 4.6", Effort: "low", State: SubagentTaskStateDone,
			OutputMD: ref("Three providers quote under $4k."), URL: ref("/api/v1/agents/engineering-lead/subagents/aaaa1111")},
		{Index: 1, ID: "bbbb2222", Title: "Check the build logs", Model: "Haiku 4.5", Effort: "medium", State: SubagentTaskStateErrored,
			Error: ref("The agent pod went away"), URL: ref("/api/v1/agents/engineering-lead/subagents/bbbb2222")},
		{Index: 2, ID: "", Title: "Check lead times", Model: "Sonnet 4.6", Effort: "high", State: SubagentTaskStateRunning},
	}
	return []TranscriptRow{
		{Kind: TranscriptKindMessage, TS: ms, Role: &sent, From: &you, BodyMD: ref("Where are we on the quote?"),
			Attachments: &quote, Pending: ref(false)},
		{Kind: TranscriptKindMessage, TS: ms + 1000, Role: &received, From: &vp, BodyMD: ref("Checking now."),
			Attachments: &[]AttachmentRef{}, Model: ref("Opus 5.5"), Effort: ref("high"), Pending: ref(false)},
		{Kind: TranscriptKindMessage, TS: ms + 1500, Role: &received, From: &vp, BodyMD: ref("API Error: usage limit"),
			Attachments: &[]AttachmentRef{}, Pending: ref(false), IsError: true},
		{Kind: TranscriptKindMessage, TS: ms + 1800, Role: &sent, From: &you, BodyMD: ref("$ ls\nquote.pdf"),
			Attachments: &[]AttachmentRef{}, Pending: ref(false), Quiet: true, SourceKind: ref("shell_captured")},
		{Kind: TranscriptKindDelivery, TS: ms + 2000, From: &ChatParty{Kind: PartyKindAgent, Slug: "buyer", Name: "Buyer"},
			Title: ref("Quote is in"), DeliveredTo: &PersonRef{Slug: "engineering-lead", Name: "Engineering lead"},
			AlsoTo: &[]PersonRef{{Slug: "tester", Name: "tester"}}, BodyMD: ref("Attached."), Attachments: &quote,
			KindBadge: ref("assignment update"), MessageType: ref("assignment_event"), AssignmentRef: &AssignmentRef{ID: 42, Title: "Get a hosting quote"},
			RepliesTo: &buyer, InReplyTo: ref("/messages/2026-09-01/0001-engineering-lead.md"), RawURL: ref("/messages/2026-09-01/0002-buyer.md")},
		{Kind: TranscriptKindCEOQueue, TS: ms + 3000, From: &vp, Title: ref("Approve the hosting spend"), BodyMD: ref("$3.8k."),
			Attachments: &[]AttachmentRef{}, MessageType: ref("ceo_approval_request"),
			Path: ref("messages/2026-09-01/0003-engineering-lead.md"), RawURL: ref("/messages/2026-09-01/0003-engineering-lead.md"), Resolved: ref(false)},
		{Kind: TranscriptKindToolUse, TS: ms + 4000, ToolUseID: ref("toolu_01"), Name: ref("Bash"), Input: ref(`{"command":"ls"}`),
			Output: ref("quote.pdf"), Status: &done, StartedTS: ref(ms + 4000), EndedTS: ref(ms + 4200)},
		{Kind: TranscriptKindToolUse, TS: ms + 4300, ToolUseID: ref("toolu_02"), Name: ref("Read"), Input: ref(`{}`),
			Output: ref("no such file"), IsError: true, Status: &errored, StartedTS: ref(ms + 4300), EndedTS: ref(ms + 4400)},
		{Kind: TranscriptKindToolUse, TS: ms + 4500, ToolUseID: ref("toolu_03"), Name: ref("WebFetch"), Input: ref(`{}`),
			Status: &running, StartedTS: ref(ms + 4500)},
		// A long call the chat API cut: the rest is at .../tool-calls/toolu_06.
		{Kind: TranscriptKindToolUse, TS: ms + 4600, ToolUseID: ref("toolu_06"), Name: ref("file_create"),
			Input: ref(`{"path":"notes/plan.md","content":"# Plan…"}`), Output: ref("Wrote notes/plan.md (48213 bytes)…"),
			Status: &done, StartedTS: ref(ms + 4600), EndedTS: ref(ms + 4700), InputTruncated: true, OutputTruncated: true},
		{Kind: TranscriptKindDocPublished, TS: ms + 5000, ToolUseID: ref("toolu_04"), Title: ref("Quote accepted"),
			MessageType: ref("notice"), To: &[]PersonRef{buyer, {Slug: "tester", Name: "tester"}},
			BodyMD: ref("We take the **$3.8k** quote."), Attachments: &quote, AssignmentRef: &AssignmentRef{ID: 42, Title: "Get a hosting quote"},
			RawURL: ref("/messages/2026-09-01/0004-engineering-lead.md")},
		{Kind: TranscriptKindFileShared, TS: ms + 6000, From: &vp, BodyMD: ref("Shared file: hosting-quote.pdf"), Attachments: &quote},
		{Kind: TranscriptKindSubagents, TS: ms + 7000, ToolUseID: ref("toolu_05"), Tasks: &tasks},
		goldenTaskResult(),
		{Kind: TranscriptKindTaskResult, TS: ms + 7600, Tasks: &[]SubagentTask{}, BodyMD: ref("A report that names no task.")},
		{Kind: TranscriptKindMarker, TS: ms + 8000, MarkerKind: &stop, Text: ref("Chat interrupted by Stop"), BodyMD: ref("User pressed Stop.")},
		{Kind: TranscriptKindMarker, TS: ms + 8500, MarkerKind: &paused, Text: ref("Paused to deliver your message"), BodyMD: ref("")},
		{Kind: TranscriptKindWakeUpdate, TS: ms + 9000, BodyMD: ref("Since your last turn: #42 moved to review.")},
		{Kind: TranscriptKindRotationPrompt, TS: ms + 10000, BodyMD: ref("The CEO has requested a chat rotation.")},
	}
}

// goldenTaskResult is a background task's report as a row: the task
// that reported, as it ended, at index 0.
func goldenTaskResult() TranscriptRow {
	return TranscriptRow{Kind: TranscriptKindTaskResult, TS: t0.UnixMilli() + 7500, Tasks: &[]SubagentTask{
		{Index: 0, ID: "aaaa1111", Title: "Compare hosting providers", Model: "Sonnet 4.6", Effort: "low", State: SubagentTaskStateDone,
			OutputMD: ref("Three providers quote under $4k."), URL: ref("/api/v1/agents/engineering-lead/subagents/aaaa1111")},
	}}
}

// goldenChatMessages are the two forms of the chat_message stream
// event: a message you sent, and a background task's report with its
// row.
func goldenChatMessages() (typed, taskResult ChatMessageEvent) {
	typed = ChatMessageEvent{
		Role: MessageRoleReceived, Kind: "direct_chat", Content: "Where are we on the quote?", TS: t0.UnixMilli(),
		Attachments: []PendingAttachment{{SHA: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", Name: "hosting-quote.pdf"}},
	}
	row := goldenTaskResult()
	taskResult = ChatMessageEvent{
		Role: MessageRoleReceived, Kind: "subagent_result", TS: row.TS, Attachments: []PendingAttachment{}, Row: &row,
		Content: "Background task aaaa1111 (Compare hosting providers) finished.\n\nThree providers quote under $4k.\n",
	}
	return typed, taskResult
}

func goldenChat() Chat {
	return Chat{
		Rows:    goldenTranscript(),
		Pending: []PendingMessage{goldenPendingMessage()},
		Fill: ChatFill{Pct: 82, Tokens: 164000, Limit: 200000, Bucket: "high", LongThreshold: 150000,
			ResolvedModel: "claude-opus-5-5"},
		Models: []ModelOption{
			goldenModelOption("claude-opus-5-5", "Opus 5.5"),
			{ID: "claude-opus-4-1", Label: "Opus 4.1", Legacy: true, ContextWindow: 200000,
				Efforts: goldenEfforts(), Provider: "claude"},
		},
		CurrentModel:  "claude-opus-5-5",
		CurrentEffort: "high",
		Running:       true,
		WaitingTasks:  1,
		Archived:      false,
		Rotating:      false,
		Generation:    "20260901T120000.000000000Z",
	}
}

func goldenSubagentTranscript() SubagentTranscript {
	ms := t0.UnixMilli()
	vp := ChatParty{Kind: PartyKindAgent, Slug: "engineering-lead", Name: "Engineering lead"}
	task := ChatParty{Kind: PartyKindAgent, Slug: "bbbb2222", Name: "Check the build logs"}
	received := MessageRoleReceived
	return SubagentTranscript{
		Meta: SubagentMeta{
			ID: "bbbb2222", Parent: "engineering-lead", State: SubagentTaskStateErrored, Model: "Haiku 4.5", Effort: "medium",
			Description: "Check the build logs", Started: ms, Ended: ref(ms + 60000), Error: ref("The agent pod went away"),
			Step: ref("Read the logs"), Activity: ref("running Bash"),
		},
		Rows: []TranscriptRow{
			{Kind: TranscriptKindMessage, TS: ms, Role: &received, From: &vp, BodyMD: ref("Check the build logs."),
				Attachments: &[]AttachmentRef{}, Pending: ref(false)},
			{Kind: TranscriptKindMessage, TS: ms + 50000, Role: &received, From: &task, BodyMD: ref("Two flaky tests."),
				Attachments: &[]AttachmentRef{}, Model: ref("Haiku 4.5"), Effort: ref("medium"), Pending: ref(false)},
		},
	}
}

func goldenPastChat() PastChat {
	return PastChat{
		TS:       "20260901T093000.000000000Z",
		Title:    "Chose the hosting provider",
		Range:    ChatRange{From: t0.UnixMilli(), To: t0.UnixMilli() + 3600000},
		Messages: 24,
		Tokens:   51200,
	}
}

func goldenPastChatDetail() PastChatDetail {
	return PastChatDetail{
		Summary: PastChatSummary{
			PastChat:    goldenPastChat(),
			DigestMD:    "Compared three hosts and chose the cheapest.",
			MemoryAdded: []string{"The host gives a discount on a yearly plan [[ep:20260901T093000.000000000Z]]"},
			HabitsDiff:  HabitsDiff{Before: "- Ask for two quotes.", After: "- Ask for three quotes."},
		},
		Rows:   goldenTranscript()[:2],
		PrevTS: ref("20260820T120000.000000000Z"),
		NextTS: ref("20260915T080000.000000000Z"),
	}
}
