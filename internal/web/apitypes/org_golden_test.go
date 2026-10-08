package apitypes

import "time"

// Golden fixtures for the Org page (registered in goldenFixtures):
// every optional field is set somewhere so the TypeScript side sees it.

func goldenUsage() Usage {
	daily := make([]UsageDay, 30)
	first := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	for i := range daily {
		daily[i] = UsageDay{Date: first.AddDate(0, 0, i).Format(time.DateOnly)}
	}
	daily[27].Spend, daily[28].Spend, daily[29].Spend = 12.5, 18.25, 4.25
	return Usage{
		Tiles: UsageTiles{
			Today: UsageTile{Spend: 4.25, Calls: 31, CacheHitPct: 82},
			D7:    UsageTile{Spend: 61.5, Calls: 540, CacheHitPct: 79},
			D30:   UsageTile{Spend: 210.75, Calls: 2210, CacheHitPct: 77},
		},
		Daily: daily,
		ByAgent: []UsageAgent{
			{Slug: "engineering-lead", Name: "Engineering lead", D7: 30, D30: 120.5},
			{Slug: "chief-of-staff", Name: "Chief of Staff", D7: 25, D30: 80},
			{Slug: "", Name: "Kivali", D7: 6.5, D30: 10.25},
		},
		Windows: UsageWindows{
			H24: UsageWindow{Calls: 40, TokensIn: 1_200_000, TokensOut: 30_000, CacheRead: 950_000, CacheCreate: 120_000, Spend: 5.5},
			D7:  UsageWindow{Calls: 540, TokensIn: 15_000_000, TokensOut: 400_000, CacheRead: 11_800_000, CacheCreate: 1_500_000, Spend: 61.5, Unpriced: 27_324_359, UnpricedCalls: 12},
			D30: UsageWindow{Calls: 2210, TokensIn: 60_000_000, TokensOut: 1_600_000, CacheRead: 46_000_000, CacheCreate: 6_000_000, Spend: 210.75, Unpriced: 27_324_359, UnpricedCalls: 12},
		},
	}
}

func goldenOrg() Org {
	logo := "/branding/icon-512.png"
	return Org{Name: "Plainsong", HasLogo: true, LogoURL: &logo}
}

func goldenHandbook() Handbook {
	at := t1
	return Handbook{
		Content: "We ship.\n## Money\n- Under budget\n",
		Sections: []DocSection{
			{Title: "Opening", Lines: []string{"We ship."}, LineCount: 1},
			{Title: "Money", Lines: []string{"- Under budget", ""}, LineCount: 1},
		},
		Draft:     false,
		UpdatedAt: &at,
	}
}

func goldenProjectFiles() ProjectFiles {
	const sha = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	return ProjectFiles{Files: []ProjectFile{{
		SHA: sha, Name: "plan.pdf", SizeBytes: 86016, UploadedAt: t0,
		Extracted: true, Kind: "reference", Summary: "The business plan.",
		URL: "/api/v1/org/files/" + sha + "/original",
	}}}
}

func goldenSkills() Skills {
	return Skills{Skills: []Skill{
		{Name: "pdf", Version: "1.2.0", Enabled: true, Builtin: true, Description: "Read and fill PDFs.", UpdatedAt: t0},
		{Name: "quote-check", Version: "0.3.1", Enabled: false, Builtin: false, Description: "Check a hosting quote.", UpdatedAt: t1},
	}}
}

func goldenSkillDowngrade() SkillDowngrade {
	return SkillDowngrade{
		Error: "quote-check 0.3.0 is not newer than the installed 0.3.1", Who: "you, by confirming the replacement",
		OldVersion: "0.3.1", NewVersion: "0.3.0",
	}
}
