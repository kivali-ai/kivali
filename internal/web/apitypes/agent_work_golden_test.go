package apitypes

// Fixtures for the agent's Background and About tabs (agent_work.go).

func goldenBackground() Background {
	two, three := 2, 3
	failure := "The pod ran out of memory"
	activity := "running file_view"
	caller := "a1b2c3d4"
	ended := t1
	return Background{
		Plan: &BackgroundPlan{
			Title: "Renewal recommendation: Acme",
			Steps: []PlanStep{
				{N: 1, Title: "Pull actual spend vs. contracted minimum", State: PlanStepStateDone},
				{N: 2, Title: "List every support incident", State: PlanStepStateRunning},
				{N: 3, Title: "Find two comparable vendors", State: PlanStepStateNeedsHelp},
				{N: 4, Title: "Write the recommendation", State: PlanStepStateIdle},
			},
			Done: 1, Total: 4, Running: 2, Finished: 2, NeedsHelp: 1,
		},
		Sessions: []BackgroundSession{
			{
				ID: "a1b2c3d4", Kind: SessionKindSubagent, State: SessionStateRunning, Title: "2. List every support incident",
				Model: "Opus 5.5", Effort: "high", ElapsedS: 312, Step: &two, Activity: &activity, Started: t0,
				URL: "/agents/engineering-lead/subagents/a1b2c3d4",
			},
			{
				ID: "e5f6a7b8", Kind: SessionKindSubagent, State: SessionStateQueued, Title: "Read the ticket export",
				Model: "Sonnet 4.6", Effort: "medium", ElapsedS: 4, Step: &two, CallerID: &caller, Started: t0,
				URL: "/agents/engineering-lead/subagents/e5f6a7b8",
			},
			{
				ID: "c9d0e1f2", Kind: SessionKindSubagent, State: SessionStateErrored, Title: "Find two comparable vendors",
				Model: "Opus 5.5", Effort: "high", ElapsedS: 95, Step: &three, Error: &failure, Started: t0, Ended: &ended,
				URL: "/agents/engineering-lead/subagents/c9d0e1f2",
			},
			{
				ID: "0a1b2c3d", Kind: SessionKindSubagent, State: SessionStateDone, Title: "Check the contract dates",
				Model: "Sonnet 4.6", Effort: "low", ElapsedS: 40, Started: t0, Ended: &ended,
				URL: "/agents/engineering-lead/subagents/0a1b2c3d",
			},
		},
	}
}

func goldenAgentDoc() AgentDoc {
	at := t1
	notes := 12
	return AgentDoc{
		Kind:      AgentDocKindMemory,
		Content:   "# Facts\n- Prod runs v0.14 [[ep:2026-09-17T01]]\n",
		UpdatedAt: &at,
		Stats:     AgentDocStats{Lines: 2, Chars: 43, Notes: &notes},
	}
}
