package apitypes

// Fixtures for the software lists (software.go).

func goldenAgentSoftware() AgentSoftware {
	return AgentSoftware{Org: []string{"git", "jq"}, Own: []string{"ffmpeg", "poppler-utils", "x/y"}, Invalid: []string{"x/y"}}
}

// goldenOrgSoftware lists a name that is not a Debian package name, as
// a hand-edited software.yaml can.
func goldenOrgSoftware() OrgSoftware {
	return OrgSoftware{Packages: []string{"bad name", "git", "jq"}, Invalid: []string{"bad name"}}
}
