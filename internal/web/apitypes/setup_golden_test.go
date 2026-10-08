package apitypes

// goldenSetup is setup on its last step, with one read file and one
// that is named only, and no model connected.
func goldenSetup() Setup {
	return Setup{
		Needed: true,
		Step:   SetupStepCoS,
		Org:    MeOrg{Name: "Plainsong", HasLogo: true},
		Files: []SetupFile{
			{SHA: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", Name: "business-plan.pdf", SizeBytes: 482113, Extracted: true},
			{SHA: "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae", Name: "site-photo.jpg", SizeBytes: 1203344, Extracted: false},
		},
		CoS: SetupCoS{
			Exists:            false,
			DefaultRoleMD:     "# Chief of Staff\n\nYou run the org day to day.\n",
			DefaultHandbookMD: "# Handbook\n\n## The company\n\nThis section is a placeholder.\n",
		},
		Credential: SetupCredential{
			Ready:    false,
			Provider: "claude",
			Present:  false,
			Who:      "",
			Billing:  "",
			Guidance: "No model is connected yet; whoever runs this Kivali server can sign it in to Claude.",
		},
		RestoreAvailable: true,
	}
}

func goldenSeedCoSRequest() SeedCoSRequest {
	role := "# Chief of Staff\n\nEdited.\n"
	handbook := "# Handbook\n\nEdited.\n"
	return SeedCoSRequest{HandbookFromFiles: true, RoleMD: &role, HandbookMD: &handbook}
}

func goldenSetupProgress() SetupProgress {
	return SetupProgress{
		State: SetupProgressStateRunning,
		Stages: []SetupStage{
			{Label: "Reading your files", State: SetupStageStateDone},
			{Label: "Writing its first briefing", State: SetupStageStateRunning},
			{Label: "Hiring", State: SetupStageStateQueued},
			{Label: "Asking it to draft the handbook", State: SetupStageStateQueued},
		},
		ElapsedS: 34,
	}
}

func goldenSetupProgressFailed() SetupProgress {
	return SetupProgress{
		State: SetupProgressStateFailed,
		Stages: []SetupStage{
			{Label: "Reading your files", State: SetupStageStateDone},
			{Label: "Writing its first briefing", State: SetupStageStateFailed},
			{Label: "Hiring", State: SetupStageStateQueued},
		},
		ElapsedS: 61,
		Error:    "your Chief of Staff could not write its first briefing because the model call failed",
		Who:      "you, by trying again, or whoever runs this Kivali server if it keeps failing",
	}
}
