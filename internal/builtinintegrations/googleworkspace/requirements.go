package googleworkspace

import "strings"

// testingAdvice follows core's words of a sign-in Google no longer
// accepts. Google ends every sign-in of an External app left in
// Testing seven days after it was made, refusing the renewal with
// invalid_grant, which is also how it refuses a sign-in the owner
// revoked: so the words are conditional on the timing, and are added
// to every invalid_grant.
const testingAdvice = "If this came about a week after you signed in, the app is likely still in Testing on Google's Auth Platform, where Google ends every sign-in after seven days: publish it under Audience, then sign in again."

// SignInAdvice is Google's likely cause of a refused renewal, for the
// Sign-in requirement: the app left in Testing, for invalid_grant, and
// nothing for any other refusal.
func (g *GoogleWorkspace) SignInAdvice(refusal string) string {
	if strings.Contains(refusal, "invalid_grant") {
		return testingAdvice
	}
	return ""
}
