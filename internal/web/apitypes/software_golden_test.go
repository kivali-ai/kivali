package apitypes

// Fixtures for the software lists (software.go).

func goldenAgentSoftware() AgentSoftware {
	return AgentSoftware{Org: []string{"git", "jq"}, Own: []string{"ffmpeg", "poppler-utils"}}
}
