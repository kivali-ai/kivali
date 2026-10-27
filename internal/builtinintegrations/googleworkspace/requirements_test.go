package googleworkspace

import (
	"strings"
	"testing"
)

// Why this test exists: an External app left in Testing has Google end
// its sign-in a week after it was made, refusing the renewal with
// invalid_grant, and core's words for that name only Google's refusal.
// Google Workspace must add the likely cause and what fixes it for
// invalid_grant, and add nothing for another refusal, whose cause it
// does not know.
func TestSignInAdviceNamesTesting(t *testing.T) {
	g := New()
	advice := g.SignInAdvice("the vendor refused the sign-in (invalid_grant): Token has been expired or revoked.")
	if advice != testingAdvice || !strings.Contains(advice, "Testing") || !strings.Contains(advice, "seven days") || !strings.Contains(advice, "publish") {
		t.Errorf("invalid_grant: %q", advice)
	}
	if got := g.SignInAdvice("the vendor refused the sign-in (invalid_client): The OAuth client was deleted."); got != "" {
		t.Errorf("invalid_client: %q", got)
	}
}
