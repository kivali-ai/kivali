package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinskills"
)

// Why this test exists: the build-an-integration skill's setup.md gives
// a setup bundle's author the runner's bounds and deadlines as a table,
// and the author writes the server to them: a call split to fit five
// minutes, requirements answered inside five seconds, words cut to fit.
// The numbers live in this package's constants. A constant changed
// without the table would teach a bound the runner does not hold, so
// each row is built here from the constant it states and must be in the
// file as agents read it.
func TestTheSkillsSetupBoundsAreTheRunners(t *testing.T) {
	var doc string
	for _, f := range builtinskills.Files("build-an-integration") {
		if f.Path == "setup.md" {
			doc = string(f.Data)
		}
	}
	if doc == "" {
		t.Fatal("the build-an-integration skill has no setup.md")
	}
	rows := []string{
		fmt.Sprintf("| One `kivali/setup` call | %d minutes, or the flow fails: %q |", int(setupCallTimeout.Minutes()), errSetupTimeout),
		fmt.Sprintf("| A flow | forgotten %d minutes after its last change or its last check of a Wait (made only while the owner's page is open), %d hours after its start at most; lost when Kivali restarts |", int(setupFlowTTL.Minutes()), int(setupFlowMaxAge.Hours())),
		fmt.Sprintf("| A Wait's checks | every %d seconds while the owner's page is open (a read of the flow in the last %d seconds counts), one at a time |", int(setupWaitEvery.Seconds()), int(setupWatchWindow.Seconds())),
		fmt.Sprintf("| The server's start, for a flow | %d minutes |", int(bundleStartWait.Minutes())),
		fmt.Sprintf("| `kivali/requirements` | %d seconds; %d rows; the answer kept %d seconds |", int(requirementsTimeout.Seconds()), requirementRowsMax, int(requirementsCacheTTL.Seconds())),
		fmt.Sprintf("| A requirement's title and words | %d and %d characters |", requirementTitleRunes, requirementWordsRunes),
		fmt.Sprintf("| `kivali/uninstall` | %d seconds |", int(integrationCallTimeout.Seconds())),
		fmt.Sprintf("| A step's words, progress message, error | %d characters; a Wait's code %d |", signInWordsRunes, setupCodeRunes),
		fmt.Sprintf("| A Go's or a Wait's address | %d characters |", setupURLMax),
		fmt.Sprintf("| `keep` and `state` together, and a Go's `post` | %d KiB each |", setupCarryMax>>10),
	}
	for _, row := range rows {
		if !strings.Contains(doc, row) {
			t.Errorf("setup.md's bounds table lacks the row the runner holds:\n%s", row)
		}
	}
}
