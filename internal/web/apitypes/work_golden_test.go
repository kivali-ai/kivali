package apitypes

import "time"

// Golden fixtures for Work and Graph (registered in goldenFixtures):
// every optional field is set somewhere so the TypeScript side sees it.

func goldenWorkBoard() WorkBoard {
	you := PersonRef{Slug: "ceo", Name: "You"}
	vp := PersonRef{Slug: "engineering-lead", Name: "Engineering lead"}
	buyer := PersonRef{Slug: "buyer", Name: "Buyer"}
	closedAt := t1.Add(-24 * time.Hour)
	done := AssignmentResolutionDone
	waiting := "Waiting on #45 Run the tests, assigned to Buyer"
	held := "On hold by you"
	return WorkBoard{
		Readouts: WorkReadouts{Open: 14, Ready: 5, Blocked: 2, OnHold: 1, ClosedWeek: 9},
		Goals: []WorkGoal{{
			ID: 40, Title: "Ship the release", Owner: vp, Done: 1, Total: 3, State: WorkItemStateMoving,
			LookBack: []WorkItem{{
				ID: 41, Title: "Draft the release notes", State: WorkItemStateClosed, Owner: buyer,
				WaitingOn: []AssignmentLink{}, ClosedAt: &closedAt, Resolution: &done,
			}},
			Current: []WorkItem{
				{
					ID: 42, Title: "Update the docs", State: WorkItemStateBlocked, Owner: buyer,
					WaitingOn: []AssignmentLink{{ID: 45, Title: "Run the tests", State: WorkItemStateReady, Owner: buyer}}, Why: &waiting,
					Acceptance: &ConditionProgress{Satisfied: 1, Claimed: 1, Unclaimed: 1},
				},
				{ID: 43, Title: "Fixture", State: WorkItemStateOnHold, Owner: vp, WaitingOn: []AssignmentLink{}, HeldBy: &you, Why: &held},
				{ID: 44, Title: "Migration", State: WorkItemStateMoving, Owner: vp, WaitingOn: []AssignmentLink{}},
			},
			LookForward: []WorkItem{{ID: 46, Title: "Label", State: WorkItemStateReady, Owner: buyer, WaitingOn: []AssignmentLink{}}},
			Unclaimed:   []UnclaimedCondition{{Name: "test plan"}},
		}},
		ClosedGoals: []ClosedGoal{{ID: 30, Title: "Pick a host", Owner: vp, ClosedAt: closedAt, Resolution: AssignmentResolutionDone}},
	}
}

func goldenAssignment() Assignment {
	you := PersonRef{Slug: "ceo", Name: "You"}
	vp := PersonRef{Slug: "engineering-lead", Name: "Engineering lead"}
	buyer := PersonRef{Slug: "buyer", Name: "Buyer"}
	why := "On hold under #40 by you"
	note := "Wait for the review."
	return Assignment{
		ID: 42, Title: "Update the docs", State: WorkItemStateOnHold, Why: &why, HeldBy: &you, HeldHere: false, Seq: 118,
		Facts: AssignmentFacts{
			Assignee: buyer, OpenedBy: vp, Opened: t0, Updated: t1,
			PartOf:       &AssignmentLink{ID: 40, Title: "Ship the release", State: WorkItemStateOnHold, Owner: vp},
			WaitsOn:      []AssignmentLink{{ID: 45, Title: "Run the tests", State: WorkItemStateReady, Owner: buyer}},
			HoldsUp:      []AssignmentLink{{ID: 40, Title: "Ship the release", State: WorkItemStateOnHold, Owner: vp}},
			CountsToward: []CountsToward{{ID: 40, Title: "Ship the release", Condition: "docs updated"}},
		},
		DescriptionMD: "Check the docs against the **v3** API.",
		Conditions: []AssignmentCondition{
			{Name: "page list", State: ConditionStateSatisfied, MetBy: &MetBy{ID: 47, Title: "List the pages", At: t1}, ClaimedBy: []AssignmentRef{}},
			{Name: "margins", State: ConditionStateClaimed, ClaimedBy: []AssignmentRef{{ID: 48, Title: "Margins"}}},
			{Name: "sign-off", State: ConditionStateUnclaimed, ClaimedBy: []AssignmentRef{}},
		},
		Progress: ConditionProgress{Satisfied: 1, Claimed: 1, Unclaimed: 1},
		Outcome:  &AssignmentOutcome{Resolution: AssignmentResolutionDone, Text: "Every page is up to date.", At: t1, Unmet: []string{"sign-off"}},
		Log: []AssignmentLogEntry{
			{At: t0, By: vp, Text: "opened it, assigned to Buyer"},
			{At: t0, By: vp, Text: "made it wait on #45", Ref: &AssignmentRef{ID: 45, Title: "Run the tests"}},
			{At: t1, By: you, Text: "edited the title", Note: &note, BeforeRef: &BeforeRef{Kind: "attachment", Ref: "3f2a", URL: "/attachments/3f2a"}},
		},
		Parts: []AssignmentLink{{ID: 47, Title: "List the pages", State: WorkItemStateClosed, Owner: buyer}},
	}
}

func goldenAssignmentUpdateRequest() AssignmentUpdateRequest {
	title, body, assignee := "Update the docs, v3", "Check the docs.", "buyer"
	parent, seq := 40, int64(118)
	waits := []int{45}
	conds := []string{"page list", "margins"}
	counts := []string{"docs updated"}
	return AssignmentUpdateRequest{
		Title: &title, DescriptionMD: &body, Assignee: &assignee, Parent: &parent,
		WaitsOn: &waits, Conditions: &conds, CountsToward: &counts,
		Note: "v3 changes the API.", Seq: &seq,
	}
}

func goldenGraph() Graph {
	problem := "front matter rejected: kind \"memo\" is not one of requirement, decision, certificate, reference"
	updated := t1
	return Graph{
		Readouts: GraphReadouts{Nodes: 1204, Flagged: 3, Problems: 2},
		Owners: []GraphOwner{
			{Slug: "engineering-lead", Name: "Engineering lead", Count: 12, HasMore: true, Nodes: []NodeRow{
				{ID: "engineering-lead/changelog", Kind: "requirement", Type: "artifact", Title: "Every release has a changelog", Status: "provisional", Flagged: true, Updated: &updated, Version: 3},
				{ID: "engineering-lead/bad", Kind: "artifact", Type: "artifact", Title: "bad.md", Status: "current", Problem: &problem, Version: 0},
			}},
			{Slug: "", Name: "Unowned", Count: 0, Nodes: []NodeRow{}},
		},
		Stale: true,
	}
}

func goldenGraphNode() GraphNode {
	url := "/attachments/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	// The text without its front matter, as the handler sends it.
	body := "Every release has a changelog."
	note := "The text of this version is no longer stored. The version stays on record."
	v := GraphVersion{N: 1, At: t0, SHA: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", PayloadSHA: "", URL: &url}
	return GraphNode{
		Fields: GraphNodeFields{
			ID: "engineering-lead/changelog", Type: "artifact", Kind: "requirement", Owner: PersonRef{Slug: "engineering-lead", Name: "Engineering lead"},
			Path: "reqs/changelog.md", Status: "superseded", DeclaredStatus: "provisional", Condition: "until the next release",
			Check: "changelog check passes", Source: "", Summary: "Every release has a changelog", Payload: "",
			Manifested: true, Rejected: "",
			About:        &GraphRef{ID: "engineering-lead/release-plan", Pin: "", Resolved: true},
			DependsOn:    []GraphRef{{ID: "chief-of-staff/release-policy", Pin: "2", Resolved: true}},
			Supersedes:   []GraphRef{},
			Evidence:     []string{"ci/run-12.csv"},
			Flagged:      true,
			Flags:        []string{"depends_on chief-of-staff/release-policy@2 is superseded"},
			Problems:     []string{},
			Versions:     []GraphVersion{v},
			AboutMe:      []string{},
			Dependents:   []string{"engineering-lead/signoff"},
			SupersededBy: []string{"engineering-lead/changelog-2"},
			ReportsTo:    "",
		},
		Version:  &v,
		BodyMD:   &body,
		BodyNote: &note,
	}
}
