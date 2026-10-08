package apitypes

// Golden fixtures for the proposal page (registered in goldenFixtures):
// between them every optional field is set.

// goldenProposalRoleUpdate is a denied role update: the role as it
// stands and the proposed one, with the answer's sentence.
func goldenProposalRoleUpdate() Proposal {
	before := "# Buyer\n\n## Scope\nBuys software and services.\n"
	return Proposal{
		Path:       "messages/2026-09-28/0003-chief-of-staff.md",
		Kind:       ProposalKindRoleUpdate,
		Title:      "Update role: Buyer",
		Proposer:   PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"},
		ProposedAt: t0,
		ReasonMD:   "The buyer should own vendor quotes too.",
		Attachments: []AttachmentRef{
			{Name: "quotes.csv", SizeBytes: 2048, URL: "/attachments/3f2a"},
		},
		Summary: ProposalSummary{
			Agent: &ProposalAgent{
				Slug: "buyer", Name: "Buyer", RoleTitle: "Buyer", Icon: "shopping-cart",
				ReportsTo: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"},
				Model:     "claude-opus-5-5", ModelLabel: "Opus 5.5", Effort: "high",
			},
			Moves: []ProposalMove{},
			Facts: []ProposalFact{
				{Label: "Reports to", Value: "Engineering lead"},
				{Label: "Model", Value: "Opus 5.5 · high"},
			},
		},
		Docs: []ProposalDoc{{
			Key: ProposalDocKeyRole, Title: "Role", Meta: "role.md · 5 lines",
			Before:          &before,
			After:           "# Buyer\n\n## Scope\nBuys software and services.\nOwns vendor quotes.\n",
			BeforeIsCurrent: true,
		}},
		Resolved: &ProposalResolution{Approved: false, At: t1, Result: "Buyer keeps its current role"},
	}
}

// goldenProposalReorg is an approved reorg where one move could not be
// made: the landed move has no from, the failed one keeps it.
func goldenProposalReorg() Proposal {
	from := PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"}
	reason := "tester reports to buyer, which would make a cycle"
	return Proposal{
		Path:        "messages/2026-09-28/0005-chief-of-staff.md",
		Kind:        ProposalKindReorg,
		Title:       "Reorg: review under the Engineering lead",
		Proposer:    PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"},
		ProposedAt:  t0,
		ReasonMD:    "Review work should sit with the Engineering lead.",
		Attachments: []AttachmentRef{},
		Summary: ProposalSummary{
			Moves: []ProposalMove{
				{Slug: "buyer", Name: "Buyer", To: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"}},
				{Slug: "tester", Name: "tester", From: &from, To: PersonRef{Slug: "buyer", Name: "Buyer"}, Failed: &reason},
			},
			Facts: []ProposalFact{{Label: "Changes", Value: "2 reporting lines"}},
		},
		Docs:     []ProposalDoc{},
		Resolved: &ProposalResolution{Approved: true, At: t1, Result: "1 reporting line changed and 1 could not be", Link: "/team"},
	}
}

// goldenProposalOffboard is an offboard waiting on you.
func goldenProposalOffboard() Proposal {
	sentence := "Its files move to the archive. Nothing is deleted, and you can restore it later."
	return Proposal{
		Path:        "messages/2026-09-28/0006-chief-of-staff.md",
		Kind:        ProposalKindOffboard,
		Title:       "Offboard: tester",
		Proposer:    PersonRef{Slug: "chief-of-staff", Name: "Chief of Staff"},
		ProposedAt:  t0,
		ReasonMD:    "Testing moved to CI.",
		Attachments: []AttachmentRef{},
		Summary: ProposalSummary{
			Agent: &ProposalAgent{
				Slug: "tester", Name: "tester", RoleTitle: "",
				ReportsTo: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"},
				Model:     "claude-sonnet-4-6", ModelLabel: "Sonnet 4.6", Effort: "medium",
			},
			Moves: []ProposalMove{},
			Facts: []ProposalFact{
				{Label: "Reports to", Value: "Engineering lead"},
				{Label: "Model", Value: "Sonnet 4.6 · medium"},
				{Label: "Its assignments move to", Value: "Engineering lead"},
			},
		},
		Docs:             []ProposalDoc{},
		OffboardSentence: &sentence,
	}
}
