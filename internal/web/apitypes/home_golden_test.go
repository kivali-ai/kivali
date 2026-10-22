package apitypes

import "time"

// Golden fixtures for Home (registered in goldenFixtures): every
// optional field is set somewhere so the TypeScript side sees it.

func goldenHome() Home {
	hire := ProposalKindHire
	review := "/proposals/messages/2026-09-28/0003-chief-of-staff.md"
	raw := "/messages/2026-09-28/0003-chief-of-staff.md"
	rawNote := "/messages/2026-09-28/0004-engineering-lead.md"
	software := ProposalKindSoftwareInstall
	rawSoftware := "/messages/2026-09-28/0005-engineering-lead.md"
	releases := t1.Add(30 * time.Second)
	integrationPage := "/org/integrations/github"
	return Home{
		Goals: []Goal{{
			ID: 40, Title: "Ship the release", Owner: "engineering-lead", Done: 7, Total: 12,
			Blocked: []GoalBlocker{{ID: 42, On: 45, OnTitle: "Run the tests", OnAssignee: &PersonRef{Slug: "test-runner", Name: "Test runner"}}},
			Workers: []string{"buyer"},
		}},
		Readouts: Readouts{Working: 2, Blocked: 1, ClosedWeek: 9, SpendToday: 4.25, Spend7d: 61.5},
		Needs: []NeedItem{
			{
				ID: "messages/2026-09-28/0003-chief-of-staff.md", Kind: NeedKindProposal, ProposalKind: &hire,
				Title: "Approve hire: Garden advisor", From: PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"},
				At: t1, BodyMD: "We need someone on the garden.",
				Attachments: []AttachmentRef{{Name: "hosting-quote.pdf", SizeBytes: 86016, URL: "/attachments/3f2a"}},
				Packages:    []SoftwarePackage{}, ReviewPath: &review, RawURL: &raw,
			},
			{
				ID: "messages/2026-09-28/0004-engineering-lead.md", Kind: NeedKindNotification,
				Title: "Weekly report", From: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"},
				At: t1, BodyMD: "All quiet.", Attachments: []AttachmentRef{}, Packages: []SoftwarePackage{}, RawURL: &rawNote,
			},
			{
				ID: "assignment:52", Kind: NeedKindAssignment, Title: "#52 assigned to you: Budget?",
				From: PersonRef{Slug: "buyer", Name: "Buyer"}, At: t1, BodyMD: "How much?",
				Attachments: []AttachmentRef{}, Packages: []SoftwarePackage{}, Assignment: &AssignmentRef{ID: 52, Title: "Budget?"},
			},
			{
				ID: "agent:tester", Kind: NeedKindNeedsHelp, Title: "tester keeps restarting",
				From: PersonRef{Slug: "tester", Name: "tester"}, At: t0,
				BodyMD:      "It restarted several times in a row without getting anything done.",
				Attachments: []AttachmentRef{}, Packages: []SoftwarePackage{},
				Agent: &AgentRef{Slug: "tester", Name: "tester", State: AgentStateQuarantined},
			},
			{
				ID: "web_sign_in:engineering-lead/bbbb2222", Kind: NeedKindWebSignIn, Title: "Engineering lead needs you to sign in to vendor.example",
				From: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"}, At: t1,
				BodyMD:      "The page asks for a sign-in. Open the browser, sign in, then choose Continue the task.",
				Attachments: []AttachmentRef{}, Packages: []SoftwarePackage{},
				Subagent: &SubagentRef{Parent: "engineering-lead", ID: "bbbb2222", Description: "Check the build logs", Site: "vendor.example", Reason: WebSignInReasonSignIn,
					PageURL: "https://vendor.example/login?next=%2Fbuilds", PageTitle: "Sign in · Vendor"},
			},
			{
				ID: "messages/2026-09-28/0005-engineering-lead.md", Kind: NeedKindApproval, ProposalKind: &software,
				Title: "Install software: PDF tools", From: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"},
				At: t1, BodyMD: "To read the PDF invoices.", Attachments: []AttachmentRef{},
				Packages: []SoftwarePackage{{Name: "poppler-utils", Version: new("25.03.0"), Synopsis: new("PDF utilities (based on Poppler)")}}, RawURL: &rawSoftware,
			},
			{
				ID: "web_upload:buyer/cccc3333", Kind: NeedKindWebUpload, Title: "Upload 2 files to vendor.example",
				From: PersonRef{Slug: "buyer", Name: "Buyer"}, At: t1,
				Attachments: []AttachmentRef{
					{Name: "quote-signed.pdf", SizeBytes: 48213, URL: "/api/v1/agents/buyer/subagents/cccc3333/files/quote-signed.pdf"},
					{Name: "terms.pdf", SizeBytes: 12077, URL: "/api/v1/agents/buyer/subagents/cccc3333/files/terms.pdf"},
				},
				Packages: []SoftwarePackage{},
				WebUpload: &WebUploadRef{
					Parent: "buyer", ID: "cccc3333", Description: "Send the signed quote", Site: "vendor.example",
					StartURL: "https://portal.vendor.example/quotes/new",
					Files:    []string{"/files/artifacts/private/quote-signed.pdf", "/files/project/terms.pdf"},
					Prompt:   "Upload the signed quote and our terms to the vendor's quote form, then submit it.",
				},
			},
			{
				ID: "web_budget:tester/dddd4444", Kind: NeedKindWebBudget, Title: "tester's web task tried to send 5.1 KB to paste.example",
				From: PersonRef{Slug: "tester", Name: "tester"}, At: t1,
				BodyMD:      "One action may send 2 KB. That is far more than a search or a form needs, so a page may have led the task to send out data it should not. Kivali refused the action and told the task, which carries on without it. Open the task to see what it was doing.",
				Attachments: []AttachmentRef{}, Packages: []SoftwarePackage{},
				WebBudget: &WebBudgetRef{Parent: "tester", ID: "dddd4444", Description: "Check the status page", Site: "paste.example", Bytes: 5222, Limit: WebBudgetLimitAction},
			},
			{
				ID: "integration_sign_in:github/a1b2c3d4", Kind: NeedKindIntegrationSignIn, Title: "GitHub needs you to sign in again",
				From: PersonRef{Slug: "kivali", Name: "Kivali"}, At: t1,
				BodyMD:      "The vendor refused the sign-in (bad_refresh_token): The refresh token passed is incorrect or expired. Agents cannot use the connection until you sign in again.",
				Attachments: []AttachmentRef{}, Packages: []SoftwarePackage{}, ReviewPath: &integrationPage,
			},
		},
		Queue: []QueueItem{
			{
				Path: "messages/2026-09-28/0007-engineering-lead.md", Kind: QueueKindNotice, Title: "Release update",
				From:   PersonRef{Slug: "engineering-lead", Name: "Engineering lead"},
				To:     []PersonRef{{Slug: "buyer", Name: "Buyer"}, {Slug: "tester", Name: "tester"}},
				BodyMD: "The build passed.", Attachments: []AttachmentRef{},
				QueuedAt: t1, ReleasesAt: &releases, Held: false,
				RawURL: "/messages/2026-09-28/0007-engineering-lead.md",
			},
			{
				Path: "messages/2026-09-28/0008-assignment.md", Kind: QueueKindAssignment, Title: "#42 assigned to you: Run the tests",
				From: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"}, To: []PersonRef{{Slug: "tester", Name: "tester"}},
				BodyMD: "Run the test suite.", Attachments: []AttachmentRef{}, Assignment: &AssignmentRef{ID: 42, Title: "Run the tests"},
				QueuedAt: t1, Held: true, RawURL: "/messages/2026-09-28/0008-assignment.md",
			},
		},
		AutoRelease:  AutoRelease30s,
		HistoryTotal: 41,
	}
}

func goldenHomeHistory() HistoryResponse {
	approved := "approved"
	next := "1790000000000000000:messages/2026-09-20/0001-chief-of-staff.md"
	return HistoryResponse{
		Threads: []HistoryThread{{
			Path:         "messages/2026-09-20/0001-chief-of-staff.md",
			LastActivity: t1,
			Request: HistoryMessage{
				Path: "messages/2026-09-20/0001-chief-of-staff.md", Type: "ceo_approval_request", Label: "Approval request",
				Title: "Accept the hosting quote", From: PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"},
				To: []PersonRef{{Slug: "ceo", Name: "You"}}, At: t0, BodyMD: "Quote attached.",
				Attachments: []AttachmentRef{{Name: "hosting-quote.pdf", SizeBytes: 86016, URL: "/attachments/3f2a"}},
				RawURL:      "/messages/2026-09-20/0001-chief-of-staff.md",
			},
			Replies: []HistoryMessage{{
				Path: "messages/2026-09-20/0002-ceo.md", Type: "ceo_approval_response", Label: "Approved",
				Title: "Approved: Accept the hosting quote", From: PersonRef{Slug: "ceo", Name: "You"},
				To: []PersonRef{{Slug: "chief-of-staff", Name: "Chief of Staff"}}, At: t1, BodyMD: "Keep it under budget.",
				Attachments: []AttachmentRef{}, Assignment: &AssignmentRef{ID: 40, Title: "Ship the release"}, Decision: &approved,
				RawURL: "/messages/2026-09-20/0002-ceo.md",
			}},
			Decision: &approved,
		}},
		Total:      41,
		NextBefore: &next,
	}
}
